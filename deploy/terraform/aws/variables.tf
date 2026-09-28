# What https://simple-host.app/setup writes into terraform.tfvars: the
# cluster question, the region, the address, the admins and the sign-in app.

variable "create_cluster" {
  description = "true creates a new EKS cluster; false installs into the existing cluster named in cluster_name."
  type        = bool
}

variable "cluster_name" {
  description = "The existing EKS cluster to install into (create_cluster = false). With create_cluster = true, the new cluster's name; empty means the value of name."
  type        = string
  default     = ""
  validation {
    condition     = var.cluster_name == "" || can(regex("^[A-Za-z0-9][A-Za-z0-9_-]{0,99}$", var.cluster_name))
    error_message = "cluster_name is an EKS cluster name: letters, digits, - and _."
  }
}

variable "region" {
  description = "The AWS region, for example us-east-2."
  type        = string
}

variable "base_domain" {
  description = "The address Simple Host lives at, for example sites.example.com. People get <name>.<base_domain> and sites <site>.<name>.<base_domain>."
  type        = string
  validation {
    condition     = can(regex("^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$", var.base_domain))
    error_message = "base_domain is a hostname like sites.example.com, lowercase, without https://."
  }
}

variable "admin_emails" {
  description = "The people who get admin rights."
  type        = list(string)
  validation {
    condition     = length(var.admin_emails) > 0 && alltrue([for e in var.admin_emails : can(regex("^[^@\\s,]+@[^@\\s,]+\\.[^@\\s,]+$", e))])
    error_message = "admin_emails needs at least one email address."
  }
}

variable "allowed_email_domains" {
  description = "Who may sign in at all, for example [\"example.com\"]. Required with Google; empty admits whoever the provider issues a token for."
  type        = list(string)
  default     = []
  validation {
    condition     = !can(regex("^https://accounts\\.google\\.com\\.?([:/].*)?$", lower(var.oidc_issuer))) || length(var.allowed_email_domains) > 0
    error_message = "With Google sign-in, allowed_email_domains is required: otherwise any Google account could sign in."
  }
}

variable "oidc_issuer" {
  description = "The sign-in provider's issuer URL."
  type        = string
  validation {
    condition     = can(regex("^https://[^\\s/\"\\\\?#@]+(/[^\\s\"\\\\?#]*)?$", var.oidc_issuer)) && !endswith(var.oidc_issuer, "/") && length(var.oidc_issuer) <= 2048
    error_message = "oidc_issuer is an https:// URL without a trailing slash, spaces, quotes, a query or user:password@."
  }
  validation {
    condition     = !can(regex("^https://accounts\\.google\\.com\\.?([:/].*)?$", lower(var.oidc_issuer))) || var.oidc_issuer == "https://accounts.google.com"
    error_message = "Google's issuer is exactly https://accounts.google.com."
  }
}

variable "oidc_client_id" {
  description = "The client ID of the web application registered at the sign-in provider."
  type        = string
}

variable "oidc_client_secret" {
  description = "The client secret. Set it through the environment (TF_VAR_oidc_client_secret), never in a file. Needed on the first run only: later runs keep the stored value when this is empty."
  type        = string
  default     = ""
  sensitive   = true
}

variable "extra_config" {
  description = "More settings for config.env (docs/configuration.md), as NAME = value. The setup page's Advanced step writes these."
  type        = map(string)
  default     = {}
}

# Everything below has a default the setup page keeps.

variable "name" {
  description = "Prefix for the AWS resources this creates, and a new cluster's name. Empty means sh- and 8 characters of the address's hash, so two installs in one account and region never share a name."
  type        = string
  default     = ""
  validation {
    condition     = var.name == "" || can(regex("^[a-z][a-z0-9-]{1,22}[a-z0-9]$", var.name))
    error_message = "name is 3 to 24 lowercase letters, digits and -."
  }
}

variable "api_access_cidrs" {
  description = "For a new cluster: the addresses allowed to reach the Kubernetes API over the internet (it always needs a signed-in AWS identity as well). AWS CloudShell has no fixed address, so the default is everywhere; narrow it to your network once the install is done."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "tags" {
  description = "Extra tags for every AWS resource this creates."
  type        = map(string)
  default     = {}
}

variable "image" {
  description = "The Simple Host Enterprise image, without tag."
  type        = string
  default     = "ghcr.io/vineetu/simple-host-enterprise"
}

variable "image_digest" {
  description = "The release to run, pinned by digest (INSTALL.md section 3). The default is v1.9.0."
  type        = string
  default     = "sha256:b4da00fd29f897687a0866e09bb1569a56fb027053bad1dc5fe912547b514c5d"
  validation {
    condition     = can(regex("^sha256:[0-9a-f]{64}$", var.image_digest))
    error_message = "image_digest is sha256:<64 hex>."
  }
}

variable "protect_data" {
  description = "Keeps the database and the bucket from being deleted by terraform destroy (deletion protection, final snapshot, no forced bucket delete)."
  type        = bool
  default     = true
}

variable "kubernetes_version" {
  description = "For a new cluster: the Kubernetes version. Pinned like every other version and raised on purpose: upgrading the control plane replaces every node, so it is never done by a routine re-run."
  type        = string
  default     = "1.36"
}

variable "node_size" {
  description = "For a new cluster: the node instance type."
  type        = string
  default     = "t3.medium"
}

variable "node_count" {
  description = "For a new cluster: how many nodes."
  type        = number
  default     = 2
}

variable "db_size" {
  description = "The RDS instance class."
  type        = string
  default     = "db.t4g.small"
}

variable "db_backup_days" {
  description = "Point-in-time recovery window for the database, in days. It is the database's only backup: keep it at 7 or more."
  type        = number
  default     = 7
  validation {
    condition     = var.db_backup_days >= 1 && var.db_backup_days <= 35
    error_message = "db_backup_days is 1 to 35."
  }
}

variable "dns_zone_id" {
  description = "An existing public Route 53 zone for base_domain. Empty creates one; you then delegate base_domain to its name servers."
  type        = string
  default     = ""
}

variable "letsencrypt_email" {
  description = "An email address for Let's Encrypt's expiry notices (optional; it must be on a public domain). Empty registers without one."
  type        = string
  default     = ""
}

# Found by apply.sh on an existing cluster, so nothing already there is
# installed twice.

variable "install_cert_manager" {
  description = "Install cert-manager. false when the cluster already runs one (it must be v1.15 or later)."
  type        = bool
  default     = true
}

variable "cert_manager_namespace" {
  description = "The namespace cert-manager runs in."
  type        = string
  default     = "cert-manager"
}

variable "cert_manager_service_account" {
  description = "cert-manager controller's ServiceAccount."
  type        = string
  default     = "cert-manager"
}

variable "install_external_secrets" {
  description = "Install External Secrets Operator. false when the cluster already runs one (it must serve external-secrets.io/v1)."
  type        = bool
  default     = true
}

