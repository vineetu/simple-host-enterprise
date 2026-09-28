# Simple Host Enterprise on AWS, in one Terraform root module: the cluster
# (or yours), Postgres, the bucket, the secrets, the ingress and certificates,
# and the application itself. apply.sh next to this file runs it from AWS
# CloudShell; README.md says how to run it from your own pipeline.

terraform {
  required_version = ">= 1.10"

  required_providers {
    aws        = { source = "hashicorp/aws", version = "~> 6.0" }
    helm       = { source = "hashicorp/helm", version = "~> 3.0" }
    kubernetes = { source = "hashicorp/kubernetes", version = "~> 3.0" }
    random     = { source = "hashicorp/random", version = "~> 3.6" }
    http       = { source = "hashicorp/http", version = "~> 3.4" }
    local      = { source = "hashicorp/local", version = "~> 2.5" }
  }

  # State lives in your own account. apply.sh fills this in (bucket, key,
  # region, use_lockfile); a pipeline passes its own -backend-config.
  backend "s3" {}
}

provider "aws" {
  region = var.region
  default_tags {
    tags = merge({ "simple-host" = var.name }, var.tags)
  }
}

# Both Kubernetes providers sign in the way kubectl does on EKS: a short-lived
# token from the AWS CLI for whoever runs Terraform.
locals {
  kube_exec = {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args        = ["eks", "get-token", "--cluster-name", local.cluster_name, "--region", var.region, "--output", "json"]
  }
}

provider "kubernetes" {
  host                   = local.cluster_endpoint
  cluster_ca_certificate = base64decode(local.cluster_ca)
  exec {
    api_version = local.kube_exec.api_version
    command     = local.kube_exec.command
    args        = local.kube_exec.args
  }
}

provider "helm" {
  kubernetes = {
    host                   = local.cluster_endpoint
    cluster_ca_certificate = base64decode(local.cluster_ca)
    exec                   = local.kube_exec
  }
}
