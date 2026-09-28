locals {
  name_servers = var.dns_zone_id == "" ? aws_route53_zone.base[0].name_servers : data.aws_route53_zone.existing[0].name_servers
  # A zone you already had is already delegated (or you know how it is);
  # a new one needs the NS records at the parent.
  dns_records = var.dns_zone_id == "" ? [{ name = var.base_domain, type = "NS", values = sort(local.name_servers) }] : []
}

output "url" {
  value = "https://${var.base_domain}"
}

output "readyz_url" {
  value = "https://${var.base_domain}/readyz"
}

output "dns_records" {
  description = "The records to add at your DNS provider: base_domain delegated to the Route 53 zone."
  value       = local.dns_records
}

output "cluster_name" {
  value = local.cluster_name
}

output "kubeconfig_command" {
  value = "aws eks update-kubeconfig --region ${var.region} --name ${local.cluster_name}"
}

output "secrets_location" {
  description = "Where the generated secrets are kept. BACKUP_ENVELOPE_KEY is in the first: every site is readable only with it."
  value       = "AWS Secrets Manager, ${var.region}: ${aws_secretsmanager_secret.app.name} and ${aws_secretsmanager_secret.oidc.name}"
}

output "next_steps" {
  value = join("\n", concat(
    ["", "Simple Host is installed on ${local.cluster_name}.", ""],
    length(local.dns_records) > 0 ? concat(
      ["1. DNS: add these ${length(local.dns_records[0].values)} NS records for ${var.base_domain}:", ""],
      [for ns in local.dns_records[0].values : "   ${var.base_domain}  NS  ${ns}"],
      ["",
        "   If ${var.base_domain} is under a domain you already manage (sites.example.com under example.com),",
        "   add them in that domain's DNS zone. If it is a domain of its own, set them as its name servers",
      "   at your registrar instead.", ""],
    ) : ["1. DNS: ${var.base_domain} is already a Route 53 zone in this account (${var.dns_zone_id}); nothing to add.", ""],
    [
      "2. Certificates are issued by themselves once the names resolve (a few minutes). Then check:",
      "",
      "   curl -fsS https://${var.base_domain}/readyz",
      "",
      "   It prints {\"status\":\"ok\"}. Then an admin signs in at https://${var.base_domain}/auth/login.",
      "",
      "Secrets (keep them backed up): ${aws_secretsmanager_secret.app.name} and ${aws_secretsmanager_secret.oidc.name} in AWS Secrets Manager.",
      "kubectl: aws eks update-kubeconfig --region ${var.region} --name ${local.cluster_name}",
      "",
    ],
  ))
}
