# Cluster add-ons, the ingress and DNS.
#
# Traefik behind a Network Load Balancer, not the ALB: every owner gets a
# *.<owner>.<base> certificate from cert-manager on an Ingress of its own
# (INSTALL.md, "Site addresses"), and the ALB takes certificates from ACM
# only. Traefik serves Secrets-based TLS for any number of Ingresses behind
# one load balancer, so owner certificates stay automatic.

resource "helm_release" "cert_manager" {
  count            = var.install_cert_manager ? 1 : 0
  name             = "simple-host-cert-manager"
  repository       = "https://charts.jetstack.io"
  chart            = "cert-manager"
  version          = "v1.21.2"
  namespace        = var.cert_manager_namespace
  create_namespace = true
  # A failed or timed-out install is removed again, so the next run starts clean.
  atomic  = true
  timeout = 600
  values = [yamlencode({
    fullnameOverride = "cert-manager"
    crds             = { enabled = true }
  })]
  # Its pods need the nodes, not just the control plane.
  depends_on = [module.eks]
}

resource "helm_release" "external_secrets" {
  count            = var.install_external_secrets ? 1 : 0
  name             = "simple-host-external-secrets"
  repository       = "https://charts.external-secrets.io"
  chart            = "external-secrets"
  version          = "2.11.0"
  namespace        = "external-secrets"
  create_namespace = true
  # A failed or timed-out install is removed again, so the next run starts clean.
  atomic  = true
  timeout = 600
  values = [yamlencode({
    fullnameOverride = "external-secrets"
    installCRDs      = true
  })]
  # Its pods need the nodes, not just the control plane.
  depends_on = [module.eks]
}

resource "helm_release" "traefik" {
  name             = "simple-host-traefik"
  repository       = "https://traefik.github.io/charts"
  chart            = "traefik"
  version          = "41.6.0"
  namespace        = "simple-host-ingress"
  create_namespace = true
  # A failed or timed-out install is removed again, so the next run starts clean.
  atomic  = true
  timeout = 600
  # Only plain Ingresses are used: no Traefik CRDs, so a Traefik the cluster
  # already runs is left alone.
  skip_crds = true
  values = [yamlencode({
    fullnameOverride    = "traefik"
    deployment          = { replicas = 2 }
    podDisruptionBudget = { enabled = true, maxUnavailable = 1 }
    ingressClass        = { enabled = true, isDefaultClass = false, name = "simple-host" }
    providers = {
      kubernetesCRD = { enabled = false }
      # Only Simple Host's Ingresses, in its namespace.
      kubernetesIngress = { ingressClass = "simple-host", namespaces = ["simple-host"] }
    }
    ports = {
      web = {
        http = { redirections = { entryPoint = { to = "websecure", scheme = "https", permanent = true } } }
      }
      # Uploads are up to 100 MiB (MAX_ARCHIVE_BYTES); Traefik has no body
      # cap, and five minutes covers a slow link.
      websecure = {
        transport = { respondingTimeouts = { readTimeout = "300s", writeTimeout = "300s", idleTimeout = "360s" } }
      }
    }
    service = {
      annotations = {
        "service.beta.kubernetes.io/aws-load-balancer-type"                     = "nlb"
        "service.beta.kubernetes.io/aws-load-balancer-additional-resource-tags" = join(",", [for k, v in merge({ "simple-host" = local.name }, var.tags) : "${k}=${v}"])
      }
      # The NLB keeps each person's address; Traefik passes it on in
      # X-Forwarded-For, which the server trusts from the VPC range only.
      spec = { externalTrafficPolicy = "Local" }
    }
  })]
  # Its pods need the nodes, not just the control plane.
  depends_on = [module.eks]
}

data "kubernetes_service_v1" "traefik" {
  metadata {
    name      = "traefik"
    namespace = "simple-host-ingress"
  }
  depends_on = [helm_release.traefik]
}

locals {
  lb_hostname = data.kubernetes_service_v1.traefik.status[0].load_balancer[0].ingress[0].hostname
}

# The load balancer the cluster made for Traefik (its name is the first part
# of its hostname), for the alias records' zone.
data "aws_lb" "traefik" {
  name = split("-", local.lb_hostname)[0]
}

# The zone for base_domain. You delegate base_domain to its name servers
# once, at whatever DNS provider holds the parent; everything under it
# (the address, *.base, the certificates' DNS-01 records) is managed here.
resource "aws_route53_zone" "base" {
  count         = var.dns_zone_id == "" ? 1 : 0
  name          = var.base_domain
  comment       = "Simple Host"
  force_destroy = true
}

locals {
  zone_id = var.dns_zone_id != "" ? var.dns_zone_id : aws_route53_zone.base[0].zone_id
}

# Only Let's Encrypt issues for the address. A CAA record here also stops
# the lookup at the address, so CAA records on a parent domain that leave
# out Let's Encrypt do not block the certificates.
resource "aws_route53_record" "caa" {
  count   = var.dns_zone_id == "" ? 1 : 0
  zone_id = local.zone_id
  name    = var.base_domain
  type    = "CAA"
  ttl     = 3600
  records = ["0 issue \"letsencrypt.org\"", "0 issuewild \"letsencrypt.org\""]
}

data "aws_route53_zone" "existing" {
  count   = var.dns_zone_id != "" ? 1 : 0
  zone_id = var.dns_zone_id
}

resource "aws_route53_record" "base" {
  for_each = toset([var.base_domain, "*.${var.base_domain}"])
  zone_id  = local.zone_id
  name     = each.key
  type     = "A"
  alias {
    name                   = local.lb_hostname
    zone_id                = data.aws_lb.traefik.zone_id
    evaluate_target_health = false
  }
}
