# The secrets, generated here and kept in AWS Secrets Manager. External
# Secrets Operator copies them into the simple-host-secrets Secret; nobody
# types or sees them. The keys are the ones secrets.env has.

resource "random_bytes" "session_signing_key" {
  length = 32
}

# Encrypts every site object before it reaches the bucket. Secrets Manager is
# its escrow: lose it and every site is lost (INSTALL.md section 4).
resource "random_bytes" "backup_envelope_key" {
  length = 32
}

resource "aws_secretsmanager_secret" "app" {
  name                    = "${var.name}/app"
  description             = "Simple Host: signing and envelope keys, database passwords"
  recovery_window_in_days = var.protect_data ? 30 : 0
}

resource "aws_secretsmanager_secret_version" "app" {
  secret_id = aws_secretsmanager_secret.app.id
  secret_string = jsonencode({
    SESSION_SIGNING_KEY = "k1:${random_bytes.session_signing_key.base64}"
    BACKUP_ENVELOPE_KEY = "k1:${random_bytes.backup_envelope_key.base64}"
    DB_PASSWORD         = random_password.db_owner.result
    DB_APP_PASSWORD     = random_password.db_app.result
  })
}

# The sign-in app's client secret comes from you, once. To change it later:
# aws secretsmanager put-secret-value --secret-id <name>/oidc --secret-string file:///dev/stdin
# (paste {"OIDC_CLIENT_SECRET":"..."} and press Ctrl-D), then restart the pods.
resource "aws_secretsmanager_secret" "oidc" {
  name                    = "${var.name}/oidc"
  description             = "Simple Host: the sign-in app's client secret"
  recovery_window_in_days = var.protect_data ? 30 : 0
}

resource "aws_secretsmanager_secret_version" "oidc" {
  secret_id     = aws_secretsmanager_secret.oidc.id
  secret_string = jsonencode({ OIDC_CLIENT_SECRET = var.oidc_client_secret })

  # Written on the first run only (apply.sh asks for it then); an empty value
  # on later runs keeps what is stored.
  lifecycle {
    ignore_changes = [secret_string]
  }
}
