# The cluster: a new EKS cluster in its own VPC, or the one you already have.

data "aws_caller_identity" "current" {}

data "aws_availability_zones" "available" {
  state = "available"
  filter {
    name   = "opt-in-status"
    values = ["opt-in-not-required"]
  }
}

locals {
  name             = var.name != "" ? var.name : "sh-${substr(sha1(var.base_domain), 0, 8)}"
  new_cluster_name = var.cluster_name != "" ? var.cluster_name : local.name
}

module "vpc" {
  count   = var.create_cluster ? 1 : 0
  source  = "terraform-aws-modules/vpc/aws"
  version = "6.7.3"

  name = local.name
  cidr = "10.42.0.0/16"
  azs  = slice(data.aws_availability_zones.available.names, 0, 2)

  # Nodes and the database in private subnets; the load balancer in public
  # ones. One NAT gateway for the nodes' way out (images, the sign-in provider).
  private_subnets    = ["10.42.0.0/19", "10.42.32.0/19"]
  public_subnets     = ["10.42.64.0/24", "10.42.65.0/24"]
  enable_nat_gateway = true
  single_nat_gateway = true

  public_subnet_tags  = { "kubernetes.io/role/elb" = "1" }
  private_subnet_tags = { "kubernetes.io/role/internal-elb" = "1" }
}

module "eks" {
  count   = var.create_cluster ? 1 : 0
  source  = "terraform-aws-modules/eks/aws"
  version = "21.26.0"

  name               = local.new_cluster_name
  kubernetes_version = var.kubernetes_version

  # CloudShell (or your pipeline) reaches the API over the internet; whoever
  # runs this becomes the cluster's admin.
  endpoint_public_access                   = true
  endpoint_public_access_cidrs             = var.api_access_cidrs
  enable_cluster_creator_admin_permissions = true

  vpc_id = module.vpc[0].vpc_id
  # The nodes reach the internet through the NAT gateway and join the
  # cluster only through it: naming it here makes the cluster wait for it.
  # (A module-level depends_on would defer the module's data sources and
  # show changes on every run.)
  subnet_ids = length(module.vpc[0].natgw_ids) > 0 ? module.vpc[0].private_subnets : []

  addons = {
    coredns    = {}
    kube-proxy = {}
    vpc-cni    = { before_compute = true }
  }

  eks_managed_node_groups = {
    default = {
      instance_types = [var.node_size]
      min_size       = var.node_count
      max_size       = var.node_count + 1
      desired_size   = var.node_count
    }
  }
}

# An existing cluster: read what the rest needs from it.
data "aws_eks_cluster" "existing" {
  count = var.create_cluster ? 0 : 1
  name  = var.cluster_name

  lifecycle {
    postcondition {
      condition     = var.cluster_name != ""
      error_message = "cluster_name is required with create_cluster = false."
    }
  }
}

data "aws_vpc" "existing" {
  count = var.create_cluster ? 0 : 1
  id    = data.aws_eks_cluster.existing[0].vpc_config[0].vpc_id
}

# IAM roles for service accounts need the cluster's OIDC provider in IAM.
# On a cluster you already have, apply.sh creates it when it is missing,
# outside Terraform, so removing Simple Host never removes it from under
# roles of your own.
data "aws_iam_openid_connect_provider" "existing" {
  count = var.create_cluster ? 0 : 1
  url   = data.aws_eks_cluster.existing[0].identity[0].oidc[0].issuer
}

locals {
  cluster_name     = var.create_cluster ? module.eks[0].cluster_name : var.cluster_name
  cluster_endpoint = var.create_cluster ? module.eks[0].cluster_endpoint : data.aws_eks_cluster.existing[0].endpoint
  cluster_ca       = var.create_cluster ? module.eks[0].cluster_certificate_authority_data : data.aws_eks_cluster.existing[0].certificate_authority[0].data

  oidc_provider_arn = var.create_cluster ? module.eks[0].oidc_provider_arn : data.aws_iam_openid_connect_provider.existing[0].arn
  oidc_issuer_host  = replace(var.create_cluster ? module.eks[0].cluster_oidc_issuer_url : data.aws_eks_cluster.existing[0].identity[0].oidc[0].issuer, "https://", "")

  vpc_id = var.create_cluster ? module.vpc[0].vpc_id : data.aws_vpc.existing[0].id
  # Every range of the VPC: pods can have addresses in a secondary one
  # (VPC CNI custom networking).
  vpc_cidrs = var.create_cluster ? [module.vpc[0].vpc_cidr_block] : [for a in data.aws_vpc.existing[0].cidr_block_associations : a.cidr_block]
  # The database goes in the cluster's own subnets (private ones for a new cluster).
  db_subnet_ids = var.create_cluster ? module.vpc[0].private_subnets : tolist(data.aws_eks_cluster.existing[0].vpc_config[0].subnet_ids)
}
