#!/usr/bin/env bash
# Simple Host Enterprise on AWS, from AWS CloudShell (or any shell signed in
# to AWS). https://simple-host.app/setup writes the line that runs this:
#
#   bash apply.sh --ref <commit> --tfvars <base64 of terraform.tfvars>
#
# It fetches this repository at <commit>, writes terraform.tfvars, keeps the
# Terraform state in an S3 bucket of your own, asks once for the sign-in
# app's client secret (hidden), and runs terraform apply. Running it again,
# from any shell, continues where it stopped.
#
# Other flags: --plan (show what it would do, change nothing), --yes (no
# confirmation), --destroy (remove what it made;
# the database and bucket stay unless protect_data = false), --src <dir>
# (use a local checkout instead of fetching).
set -euo pipefail

TF_VERSION=1.16.4
TF_SHA256_amd64=dc94af0eef1147718ad7c8daea792ed199e3e0492eec180d0adafa2a65a879df
TF_SHA256_arm64=8263f301cb1a24489a4adeed147bf28504053f77237b3ea97a0ef2972659de30
KUBECTL_VERSION=v1.37.1
KUBECTL_SHA256_amd64=65691ff77eb6fa44c908b77a1082c9f092c3b9733b5cefabec0d1104890e21a8
KUBECTL_SHA256_arm64=ff749f4b78d9c4f1ec87307df9b50119ed819e2094aa9810cb9acffc3286c8c7
REPO=https://github.com/vineetu/simple-host-enterprise

ref='' tfvars_b64='' yes='' destroy='' src='' plan=''
while [ $# -gt 0 ]; do
  case "$1" in
    --ref) ref=$2; shift 2 ;;
    --tfvars) tfvars_b64=$2; shift 2 ;;
    --yes) yes=1; shift ;;
    --plan) plan=1; shift ;;
    --destroy) destroy=1; shift ;;
    --src) src=$2; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done
say() { printf '\n== %s\n' "$*"; }
die() { printf 'Stopped: %s\n' "$*" >&2; exit 1; }
[ -n "$tfvars_b64" ] || die "--tfvars is required (copy the whole line from https://simple-host.app/setup)"
[ -n "$ref$src" ] || die "--ref is required"
case "$ref" in ''|[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]*) ;; *) die "--ref is a commit id" ;; esac

tfvars=$(printf '%s' "$tfvars_b64" | base64 -d) || die "--tfvars is not base64"
# One value from terraform.tfvars (a string, a bool or a number).
var() { printf '%s\n' "$tfvars" | sed -n "s/^$1 *= *\"\{0,1\}\([^\"]*\)\"\{0,1\} *$/\1/p" | head -n1; }
region=$(var region); base=$(var base_domain); create=$(var create_cluster); cluster=$(var cluster_name); name=$(var name)
name=${name:-simple-host}
[ -n "$region" ] && [ -n "$base" ] && [ -n "$create" ] || die "terraform.tfvars needs region, base_domain and create_cluster"
export AWS_REGION=$region AWS_DEFAULT_REGION=$region

say "AWS account"
command -v aws >/dev/null || die "the AWS CLI is missing (AWS CloudShell has it)"
account=$(aws sts get-caller-identity --query Account --output text) || die "this shell is not signed in to AWS"
echo "Account $account, region $region, address $base"

# An account on AWS's free plan launches only free-tier sizes, and a node
# group that cannot launch its nodes waits most of an hour before it fails:
# say so now instead.
if [ "$(aws freetier get-account-plan-state --region us-east-1 --query accountPlanType --output text 2>/dev/null || true)" = FREE ]; then
  node_size=$(var node_size); node_size=${node_size:-t3.medium}; db_size=$(var db_size); db_size=${db_size:-db.t4g.small}
  small=yes
  [ "$create" = true ] && [ "$(aws ec2 describe-instance-types --instance-types "$node_size" --query 'InstanceTypes[0].FreeTierEligible' --output text 2>/dev/null)" != True ] && small=no
  case "$db_size" in db.t4g.micro|db.t3.micro) ;; *) small=no ;; esac
  [ "$(var db_backup_days)" = 1 ] || small=no
  [ "$small" = yes ] || die "this AWS account is on the free plan, which cannot launch the cluster's nodes or the database at their sizes, or keep 7 days of database backups. Upgrade it to a paid plan (Billing and Cost Management, then Plans), then run the same line again."
fi

# Tools: Terraform and kubectl, each checked against its pinned checksum.
arch=$(uname -m); case "$arch" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) die "unsupported machine: $arch" ;; esac
mkdir -p "$HOME/.local/bin"; export PATH="$HOME/.local/bin:$PATH"
tf_ok() { command -v terraform >/dev/null && terraform version -json 2>/dev/null | grep -Eq '"terraform_version": *"1\.(1[0-9]|[2-9][0-9])\.'; }
if ! tf_ok; then
  say "Installing Terraform $TF_VERSION"
  t=$(mktemp -d); sum_var=TF_SHA256_$arch
  curl -fsSL "https://releases.hashicorp.com/terraform/$TF_VERSION/terraform_${TF_VERSION}_linux_$arch.zip" -o "$t/tf.zip"
  printf '%s  %s\n' "${!sum_var}" "$t/tf.zip" | sha256sum -c --quiet -
  unzip -o -q "$t/tf.zip" terraform -d "$HOME/.local/bin"; rm -rf "$t"
fi
if ! command -v kubectl >/dev/null; then
  say "Installing kubectl $KUBECTL_VERSION"
  sum_var=KUBECTL_SHA256_$arch
  curl -fsSL "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/$arch/kubectl" -o "$HOME/.local/bin/kubectl"
  printf '%s  %s\n' "${!sum_var}" "$HOME/.local/bin/kubectl" | sha256sum -c --quiet -
  chmod +x "$HOME/.local/bin/kubectl"
fi

# The module, at the commit the setup page names.
work="$HOME/simple-host-$base"
mkdir -p "$work"; chmod 700 "$work"
if [ -n "$src" ]; then
  mod="$(cd "$src" && pwd)/deploy/terraform/aws"
else
  say "Fetching simple-host-enterprise at $ref"
  [ -d "$work/src/.git" ] || git init -q "$work/src"
  git -C "$work/src" fetch -q --depth 1 "$REPO" "$ref"
  git -C "$work/src" -c advice.detachedHead=false checkout -q --force FETCH_HEAD
  [ "$(git -C "$work/src" rev-parse HEAD)" = "$(git -C "$work/src" rev-parse "$ref^{commit}" 2>/dev/null || echo x)" ] || die "fetched commit is not $ref"
  mod="$work/src/deploy/terraform/aws"
fi
printf '%s\n' "$tfvars" > "$mod/terraform.tfvars"
cp "$mod/terraform.tfvars" "$work/terraform.tfvars"

# Terraform state: a bucket in this account, created once.
say "Terraform state"
state_bucket="$name-tfstate-$account-$region"
if ! aws s3api head-bucket --bucket "$state_bucket" 2>/dev/null; then
  if [ "$region" = us-east-1 ]; then aws s3api create-bucket --bucket "$state_bucket" >/dev/null
  else aws s3api create-bucket --bucket "$state_bucket" --create-bucket-configuration "LocationConstraint=$region" >/dev/null; fi
  aws s3api put-public-access-block --bucket "$state_bucket" --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
  aws s3api put-bucket-tagging --bucket "$state_bucket" --tagging "TagSet=[{Key=simple-host,Value=$name}]"
  aws s3api put-bucket-versioning --bucket "$state_bucket" --versioning-configuration Status=Enabled
  aws s3api put-bucket-encryption --bucket "$state_bucket" --server-side-encryption-configuration '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
  aws s3api put-bucket-policy --bucket "$state_bucket" --policy "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Sid\":\"TLSOnly\",\"Effect\":\"Deny\",\"Principal\":\"*\",\"Action\":\"s3:*\",\"Resource\":[\"arn:aws:s3:::$state_bucket\",\"arn:aws:s3:::$state_bucket/*\"],\"Condition\":{\"Bool\":{\"aws:SecureTransport\":\"false\"}}}]}"
  echo "Created s3://$state_bucket"
else
  echo "Using s3://$state_bucket"
fi
cd "$mod"
terraform init -input=false -reconfigure -backend-config="bucket=$state_bucket" -backend-config="key=$base/terraform.tfstate" -backend-config="region=$region" -backend-config="use_lockfile=true" >/dev/null

# What an existing cluster already runs, so nothing is installed twice.
detected="$mod/detected.auto.tfvars"; : > "$detected"
if [ "$create" = false ]; then
  say "Looking at cluster $cluster"
  [ -n "$cluster" ] || die "cluster_name is empty"
  aws eks describe-cluster --name "$cluster" --query cluster.status --output text >/dev/null || die "no EKS cluster named $cluster in $region"
  kc="$work/kubeconfig.detect"
  aws eks update-kubeconfig --name "$cluster" --kubeconfig "$kc" >/dev/null
  k() { kubectl --kubeconfig "$kc" "$@"; }
  k auth can-i create namespace >/dev/null 2>&1 || die "this AWS identity cannot create namespaces in $cluster: ask for cluster-admin access (an EKS access entry)"
  # ours: the Helm release that installed it is one of ours.
  release_of() { k get "$@" -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null || true; }
  if k get crd certificates.cert-manager.io >/dev/null 2>&1; then
    ns=$(k get deploy -A -l app.kubernetes.io/name=cert-manager,app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.namespace}' 2>/dev/null || true)
    dep=$(k get deploy -A -l app.kubernetes.io/name=cert-manager,app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    # CRDs left behind with no controller (an earlier removal keeps them): install ours.
    if [ -n "$ns" ] && [ "$(release_of -n "$ns" deploy "$dep")" != simple-host-cert-manager ]; then
      sa=$(k -n "$ns" get deploy "$dep" -o jsonpath='{.spec.template.spec.serviceAccountName}')
      printf 'install_cert_manager = false\ncert_manager_namespace = "%s"\ncert_manager_service_account = "%s"\n' "$ns" "${sa:-cert-manager}" >> "$detected"
      echo "Using the cert-manager already in namespace $ns"
    fi
  fi
  if k get crd externalsecrets.external-secrets.io >/dev/null 2>&1; then
    ns=$(k get deploy -A -l app.kubernetes.io/name=external-secrets -o jsonpath='{.items[0].metadata.namespace}' 2>/dev/null || true)
    dep=$(k get deploy -A -l app.kubernetes.io/name=external-secrets -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    if [ -z "$ns" ] || [ "$(release_of -n "$ns" deploy "$dep")" != simple-host-external-secrets ]; then
      printf 'install_external_secrets = false\n' >> "$detected"
      echo "Using the External Secrets Operator already installed"
    fi
  fi
  issuer=$(aws eks describe-cluster --name "$cluster" --query cluster.identity.oidc.issuer --output text)
  if aws iam list-open-id-connect-providers --query 'OpenIDConnectProviderList[].Arn' --output text | tr '\t' '\n' | grep -q "/${issuer#https://}\$"; then
    # Ours when this state already manages it; otherwise it was there before.
    if ! grep -q 'aws_iam_openid_connect_provider.existing' <(cd "$mod" && terraform state list 2>/dev/null || true); then
      printf 'create_oidc_provider = false\n' >> "$detected"
    fi
  fi
  rm -f "$kc"
fi
# A Route 53 zone for the address that is already in the account is used as it is.
zone=$(aws route53 list-hosted-zones-by-name --dns-name "$base." --max-items 1 --query "HostedZones[?Name=='$base.' && !Config.PrivateZone].Id | [0]" --output text 2>/dev/null || true)
if [ -n "$zone" ] && [ "$zone" != None ]; then
  zone=${zone#/hostedzone/}
  if ! (cd "$mod" && terraform state list 2>/dev/null | grep -q '^aws_route53_zone.base'); then
    printf 'dns_zone_id = "%s"\n' "$zone" >> "$detected"
  fi
fi

if [ -n "$destroy" ]; then
  say "Removing Simple Host from $base"
  terraform destroy ${yes:+-input=false -auto-approve} -var-file=terraform.tfvars
  exit 0
fi

# An install stopped halfway (the shell closed during a Helm install) leaves
# its Helm release pending, and the next install of that name would refuse:
# forget such a release when Terraform does not know it. Its objects stay and
# the next install adopts them.
if [ -z "$destroy$plan" ] && tf_state=$(terraform state list 2>/dev/null) && grep -q '^module.eks\|^data.aws_eks_cluster.existing' <<<"$tf_state$([ "$create" = false ] && echo data.aws_eks_cluster.existing)"; then
  kc="$work/kubeconfig.check"
  if aws eks update-kubeconfig --name "${cluster:-$name}" --kubeconfig "$kc" >/dev/null 2>&1; then
    for rel in cert_manager:simple-host-cert-manager external_secrets:simple-host-external-secrets traefik:simple-host-traefik; do
      grep -q "^helm_release.${rel%%:*}" <<<"$tf_state" && continue
      kubectl --kubeconfig "$kc" get secret -A -l "owner=helm,name=${rel#*:}" -o jsonpath='{range .items[*]}{.metadata.namespace} {.metadata.name} {.metadata.labels.status}{"\n"}{end}' 2>/dev/null |
        while read -r ns sec st; do
          case "$st" in pending-*|failed) kubectl --kubeconfig "$kc" -n "$ns" delete secret "$sec" >/dev/null && echo "Cleared the unfinished Helm release ${rel#*:}" ;; esac
        done
    done
  fi
  rm -f "$kc"
fi

# The sign-in app's client secret: asked for once, never shown or stored on
# disk. Later runs keep the stored one.
if [ -z "$plan" ] && [ -z "${TF_VAR_oidc_client_secret:-}" ] && ! aws secretsmanager describe-secret --secret-id "$name/oidc" --query 'VersionIdsToStages' --output text 2>/dev/null | grep -q AWSCURRENT; then
  say "Your sign-in app's client secret"
  printf 'Client secret (not shown): '
  trap 'stty echo 2>/dev/null' INT; read -rs TF_VAR_oidc_client_secret; trap - INT; echo
  [ -n "$TF_VAR_oidc_client_secret" ] || die "the client secret is empty"
  export TF_VAR_oidc_client_secret
fi

if [ -n "$plan" ]; then
  say "terraform plan (nothing is changed)"
  terraform plan -input=false -var-file=terraform.tfvars
  exit 0
fi
say "terraform apply (a new cluster takes about 20 minutes)"
terraform apply ${yes:+-input=false -auto-approve} -var-file=terraform.tfvars
unset TF_VAR_oidc_client_secret
say "Next steps"
terraform output -raw next_steps
