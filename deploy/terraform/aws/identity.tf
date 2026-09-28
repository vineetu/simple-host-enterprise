# IAM roles for service accounts (IRSA): no access keys anywhere. Three
# ServiceAccounts, each with only what it needs:
#   simple-host/simple-host               the server: the bucket
#   simple-host/simple-host-secrets-reader External Secrets: the two secrets
#   <cert-manager ns>/simple-host-dns      cert-manager: DNS-01 in the zone

locals {
  service_accounts = {
    app     = "system:serviceaccount:simple-host:simple-host"
    secrets = "system:serviceaccount:simple-host:simple-host-secrets-reader"
    dns     = "system:serviceaccount:${var.cert_manager_namespace}:simple-host-dns"
  }
}

data "aws_iam_policy_document" "trust" {
  for_each = local.service_accounts
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [local.oidc_provider_arn]
    }
    condition {
      test     = "StringEquals"
      variable = "${local.oidc_issuer_host}:sub"
      values   = [each.value]
    }
    condition {
      test     = "StringEquals"
      variable = "${local.oidc_issuer_host}:aud"
      values   = ["sts.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "sa" {
  for_each           = local.service_accounts
  name               = "${local.name}-${each.key}-${var.region}"
  assume_role_policy = data.aws_iam_policy_document.trust[each.key].json
}

data "aws_iam_policy_document" "app" {
  statement {
    actions   = ["s3:ListBucket", "s3:GetBucketVersioning"]
    resources = [aws_s3_bucket.sites.arn]
  }
  statement {
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
    resources = ["${aws_s3_bucket.sites.arn}/*"]
  }
}

data "aws_iam_policy_document" "secrets" {
  statement {
    actions   = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"]
    resources = [aws_secretsmanager_secret.app.arn, aws_secretsmanager_secret.oidc.arn]
  }
}

data "aws_iam_policy_document" "dns" {
  statement {
    actions   = ["route53:GetChange"]
    resources = ["arn:aws:route53:::change/*"]
  }
  statement {
    actions   = ["route53:ChangeResourceRecordSets", "route53:ListResourceRecordSets"]
    resources = ["arn:aws:route53:::hostedzone/${local.zone_id}"]
  }
}

resource "aws_iam_role_policy" "sa" {
  for_each = {
    app     = data.aws_iam_policy_document.app.json
    secrets = data.aws_iam_policy_document.secrets.json
    dns     = data.aws_iam_policy_document.dns.json
  }
  name   = "simple-host"
  role   = aws_iam_role.sa[each.key].id
  policy = each.value
}
