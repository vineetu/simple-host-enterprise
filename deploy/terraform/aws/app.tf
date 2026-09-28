# Simple Host itself: the byo overlay's shape (deploy/overlays/byo), written
# into .work/ next to this file and applied with kubectl. config.env comes
# from the variables and what was just created; secrets.env is replaced by
# an ExternalSecret that reads Secrets Manager.

locals {
  work = "${abspath(path.module)}/.work"

  config = merge(
    {
      PUBLIC_BASE_URL   = "https://${var.base_domain}"
      SECURE_MODE       = "true"
      OWNER_CERT_ISSUER = "simple-host-letsencrypt"
      # Traefik's pods have addresses in the VPC (the VPC CNI).
      TRUSTED_PROXY_CIDRS     = local.vpc_cidr
      OIDC_ISSUER             = var.oidc_issuer
      OIDC_CLIENT_ID          = var.oidc_client_id
      ADMIN_EMAILS            = join(",", [for e in var.admin_emails : lower(trimspace(e))])
      DB_HOST                 = aws_db_instance.db.address
      DB_PORT                 = tostring(aws_db_instance.db.port)
      DB_NAME                 = aws_db_instance.db.db_name
      DB_USER                 = aws_db_instance.db.username
      DB_SSLMODE              = "verify-full"
      DB_SSL_ROOT_CERT        = "/etc/simple-host/db-ca/ca.crt"
      BACKUP_STORAGE_ENDPOINT = "https://s3.${var.region}.amazonaws.com"
      BACKUP_STORAGE_REGION   = var.region
      BACKUP_STORAGE_BUCKET   = aws_s3_bucket.sites.id
      BACKUP_SSE              = "AES256"
    },
    length(var.allowed_email_domains) > 0 ? { ALLOWED_EMAIL_DOMAINS = join(",", [for d in var.allowed_email_domains : lower(trimspace(d))]) } : {},
    var.extra_config,
  )
  config_env = join("", [for k in sort(keys(local.config)) : "${k}=${local.config[k]}\n"])

  ingress_patch = yamlencode({
    apiVersion = "networking.k8s.io/v1"
    kind       = "Ingress"
    metadata = {
      name        = "simple-host"
      annotations = { "cert-manager.io/cluster-issuer" = "simple-host-letsencrypt" }
    }
    spec = {
      ingressClassName = "simple-host"
      tls              = [{ hosts = [var.base_domain, "*.${var.base_domain}"], secretName = "simple-host-tls" }]
      rules = [for h in [var.base_domain, "*.${var.base_domain}"] : {
        host = h
        http = { paths = [{ path = "/", pathType = "Prefix", backend = { service = { name = "simple-host", port = { name = "http" } } } }] }
      }]
    }
  })

  secrets_yaml = join("---\n", [for d in [
    {
      apiVersion = "v1"
      kind       = "ServiceAccount"
      metadata = {
        name        = "simple-host-secrets-reader"
        annotations = { "eks.amazonaws.com/role-arn" = aws_iam_role.sa["secrets"].arn }
      }
    },
    {
      apiVersion = "external-secrets.io/v1"
      kind       = "SecretStore"
      metadata   = { name = "simple-host" }
      spec = { provider = { aws = {
        service = "SecretsManager"
        region  = var.region
        auth    = { jwt = { serviceAccountRef = { name = "simple-host-secrets-reader" } } }
      } } }
    },
    {
      apiVersion = "external-secrets.io/v1"
      kind       = "ExternalSecret"
      metadata   = { name = "simple-host-secrets" }
      spec = {
        refreshInterval = "1h"
        secretStoreRef  = { kind = "SecretStore", name = "simple-host" }
        target          = { name = "simple-host-secrets", creationPolicy = "Owner" }
        dataFrom        = [{ extract = { key = aws_secretsmanager_secret.app.name } }, { extract = { key = aws_secretsmanager_secret.oidc.name } }]
      }
    },
  ] : yamlencode(d)])

  # Pods restart when their configuration changes (a ConfigMap edit alone
  # does not restart them).
  config_hash = sha256(join("", [local.config_env, aws_secretsmanager_secret_version.app.version_id]))

  kustomization = yamlencode({
    apiVersion         = "kustomize.config.k8s.io/v1beta1"
    kind               = "Kustomization"
    namespace          = "simple-host"
    resources          = ["../../../../base", "secrets.yaml"]
    components         = ["../../../../components/owner-hosts"]
    generatorOptions   = { disableNameSuffixHash = true }
    configMapGenerator = [{ name = "simple-host-config", envs = ["config.env"] }]
    secretGenerator    = [{ name = "simple-host-db-ca", files = ["ca.crt=db-ca.crt"] }]
    images             = [{ name = "ghcr.io/vineetu/simple-host-enterprise", newName = var.image, digest = var.image_digest }]
    patches = concat(
      [{ path = "ingress-patch.yaml" }],
      [{
        target = { kind = "ServiceAccount", name = "simple-host" }
        patch  = yamlencode([{ op = "add", path = "/metadata/annotations", value = { "eks.amazonaws.com/role-arn" = aws_iam_role.sa["app"].arn } }])
      }],
      [for d in ["simple-host", "simple-host-owner-hosts"] : {
        target = { kind = "Deployment", name = d }
        patch  = yamlencode([{ op = "add", path = "/spec/template/metadata/annotations", value = { "simple-host.app/config" = local.config_hash } }])
      }],
    )
  })

  # cert-manager's side, in its own namespace: the ServiceAccount whose token
  # it trades for the DNS role, the right to ask for that token, and the
  # Let's Encrypt issuer for the base and every owner certificate.
  issuer_yaml = join("---\n", [for d in [
    {
      apiVersion = "v1"
      kind       = "ServiceAccount"
      metadata = {
        name        = "simple-host-dns"
        namespace   = var.cert_manager_namespace
        annotations = { "eks.amazonaws.com/role-arn" = aws_iam_role.sa["dns"].arn }
      }
    },
    {
      apiVersion = "rbac.authorization.k8s.io/v1"
      kind       = "Role"
      metadata   = { name = "simple-host-dns-token", namespace = var.cert_manager_namespace }
      rules      = [{ apiGroups = [""], resources = ["serviceaccounts/token"], resourceNames = ["simple-host-dns"], verbs = ["create"] }]
    },
    {
      apiVersion = "rbac.authorization.k8s.io/v1"
      kind       = "RoleBinding"
      metadata   = { name = "simple-host-dns-token", namespace = var.cert_manager_namespace }
      roleRef    = { apiGroup = "rbac.authorization.k8s.io", kind = "Role", name = "simple-host-dns-token" }
      subjects   = [{ kind = "ServiceAccount", name = var.cert_manager_service_account, namespace = var.cert_manager_namespace }]
    },
    {
      apiVersion = "cert-manager.io/v1"
      kind       = "ClusterIssuer"
      metadata   = { name = "simple-host-letsencrypt" }
      spec = { acme = {
        server              = "https://acme-v02.api.letsencrypt.org/directory"
        email               = var.letsencrypt_email != "" ? var.letsencrypt_email : lower(trimspace(var.admin_emails[0]))
        privateKeySecretRef = { name = "simple-host-letsencrypt-account" }
        solvers = [{
          selector = { dnsZones = [var.base_domain] }
          dns01 = { route53 = {
            region       = var.region
            hostedZoneID = local.zone_id
            role         = aws_iam_role.sa["dns"].arn
            auth         = { kubernetes = { serviceAccountRef = { name = "simple-host-dns" } } }
          } }
        }]
      } }
    },
  ] : yamlencode(d)])

  kubeconfig = yamlencode({
    apiVersion        = "v1"
    kind              = "Config"
    clusters          = [{ name = "eks", cluster = { server = local.cluster_endpoint, "certificate-authority-data" = local.cluster_ca } }]
    users             = [{ name = "eks", user = { exec = { apiVersion = local.kube_exec.api_version, command = local.kube_exec.command, args = local.kube_exec.args } } }]
    contexts          = [{ name = "simple-host", context = { cluster = "eks", user = "eks" } }]
    "current-context" = "simple-host"
  })

  files = {
    "kubeconfig"                 = local.kubeconfig
    "overlay/kustomization.yaml" = local.kustomization
    "overlay/config.env"         = local.config_env
    "overlay/ingress-patch.yaml" = local.ingress_patch
    "overlay/secrets.yaml"       = local.secrets_yaml
    "overlay/db-ca.crt"          = data.http.rds_ca.response_body
    "issuer.yaml"                = local.issuer_yaml
  }
}

resource "local_file" "work" {
  for_each        = local.files
  filename        = "${local.work}/${each.key}"
  content         = each.value
  file_permission = "0600"
}

# kubectl apply is idempotent: this runs again whenever any file changes,
# and a failed run is picked up by the next apply.
resource "terraform_data" "install" {
  triggers_replace = [for k in sort(keys(local.files)) : sha256(local.files[k])]
  # What the removal below needs, since it may run from another shell.
  input = { cluster = local.cluster_name, region = var.region, cert_manager_namespace = var.cert_manager_namespace }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = <<-EOT
      set -euo pipefail
      k() { kubectl --kubeconfig '${local.work}/kubeconfig' "$@"; }
      k apply -f '${local.work}/issuer.yaml'
      k apply -k '${local.work}/overlay'
      k -n simple-host wait --for=condition=Ready externalsecret/simple-host-secrets --timeout=180s
      k -n simple-host rollout status deploy/simple-host --timeout=600s
      k -n simple-host rollout status deploy/simple-host-owner-hosts --timeout=300s
    EOT
  }

  # terraform destroy: take Simple Host off the cluster (it may be yours and
  # stay). The namespace takes the Ingresses, certificates and secrets with it.
  provisioner "local-exec" {
    when        = destroy
    interpreter = ["/bin/bash", "-c"]
    command     = <<-EOT
      kc=$(mktemp)
      aws eks update-kubeconfig --name '${self.input.cluster}' --region '${self.input.region}' --kubeconfig "$kc" >/dev/null || exit 0
      k() { kubectl --kubeconfig "$kc" "$@"; }
      k delete namespace simple-host --ignore-not-found --wait=true --timeout=300s || true
      k delete clusterissuer simple-host-letsencrypt --ignore-not-found || true
      k -n '${self.input.cert_manager_namespace}' delete serviceaccount/simple-host-dns role/simple-host-dns-token rolebinding/simple-host-dns-token --ignore-not-found || true
      rm -f "$kc"
    EOT
  }

  depends_on = [
    local_file.work,
    helm_release.cert_manager,
    helm_release.external_secrets,
    helm_release.traefik,
    aws_secretsmanager_secret_version.app,
    aws_secretsmanager_secret_version.oidc,
    aws_iam_role_policy.sa,
    aws_db_instance.db,
    aws_s3_bucket_versioning.sites,
    aws_s3_bucket_lifecycle_configuration.sites,
    aws_route53_record.base,
  ]
}
