# Where the data lives: Postgres (RDS) and the bucket (S3), with the settings
# docs/cloud/aws.md sections 3 and 4 require.

resource "random_password" "db_owner" {
  length  = 40
  special = false
}

resource "random_password" "db_app" {
  length  = 40
  special = false
}

resource "aws_db_subnet_group" "db" {
  name       = local.name
  subnet_ids = local.db_subnet_ids
}

resource "aws_security_group" "db" {
  name_prefix = "${local.name}-db-"
  description = "Postgres for Simple Host, from inside the cluster VPC"
  vpc_id      = local.vpc_id

  ingress {
    description = "Postgres from the VPC ranges"
    from_port   = 5432
    to_port     = 5432
    protocol    = "tcp"
    cidr_blocks = local.vpc_cidrs
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_db_instance" "db" {
  identifier     = local.name
  engine         = "postgres"
  engine_version = "16"
  instance_class = var.db_size

  allocated_storage     = 20
  max_allocated_storage = 200
  storage_type          = "gp3"
  # Encryption at rest cannot be turned on after creation.
  storage_encrypted = true

  db_name  = "simplehost"
  username = "simplehost"
  password = random_password.db_owner.result

  db_subnet_group_name   = aws_db_subnet_group.db.name
  vpc_security_group_ids = [aws_security_group.db.id]
  publicly_accessible    = false

  # Point-in-time recovery is the database's only backup (docs/install.md
  # section 9). rds.force_ssl is on by default for PostgreSQL 15 and later.
  backup_retention_period    = var.db_backup_days
  copy_tags_to_snapshot      = true
  auto_minor_version_upgrade = true
  apply_immediately          = true

  deletion_protection       = var.protect_data
  skip_final_snapshot       = !var.protect_data
  final_snapshot_identifier = var.protect_data ? "${local.name}-final" : null
}

# The CA bundle for sslmode=verify-full.
data "http" "rds_ca" {
  url = "https://truststore.pki.rds.amazonaws.com/${var.region}/${var.region}-bundle.pem"
  lifecycle {
    postcondition {
      condition     = self.status_code == 200 && strcontains(self.response_body, "BEGIN CERTIFICATE")
      error_message = "Could not download the RDS CA bundle for ${var.region}."
    }
  }
}

resource "aws_s3_bucket" "sites" {
  bucket        = "${local.name}-sites-${data.aws_caller_identity.current.account_id}-${var.region}"
  force_destroy = !var.protect_data
}

resource "aws_s3_bucket_public_access_block" "sites" {
  bucket                  = aws_s3_bucket.sites.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_versioning" "sites" {
  bucket = aws_s3_bucket.sites.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "sites" {
  bucket = aws_s3_bucket.sites.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# Versioning is the file backup: overwritten and deleted objects stay for 30
# days (docs/storage.md, bucket requirements).
resource "aws_s3_bucket_lifecycle_configuration" "sites" {
  bucket = aws_s3_bucket.sites.id
  rule {
    id     = "simple-host"
    status = "Enabled"
    filter {}
    noncurrent_version_expiration {
      noncurrent_days = 30
    }
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
    expiration {
      expired_object_delete_marker = true
    }
  }
  depends_on = [aws_s3_bucket_versioning.sites]
}

data "aws_iam_policy_document" "sites_tls_only" {
  statement {
    sid       = "TLSOnly"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.sites.arn, "${aws_s3_bucket.sites.arn}/*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "sites" {
  bucket     = aws_s3_bucket.sites.id
  policy     = data.aws_iam_policy_document.sites_tls_only.json
  depends_on = [aws_s3_bucket_public_access_block.sites]
}
