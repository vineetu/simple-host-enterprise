#!/usr/bin/env bash
# Simple Host Enterprise on AWS, from AWS CloudShell (or any Linux shell
# signed in to AWS). https://simple-host.app/setup writes the line that runs
# this:
#
#   bash apply.sh --ref <commit> --tfvars <base64 of terraform.tfvars>
#
# It fetches this repository at <commit>, writes terraform.tfvars, keeps the
# Terraform state in an S3 bucket of your own, asks once for the sign-in
# app's client secret (hidden), and runs Terraform. Running the same line
# again, from any shell, picks up where it stopped: after an error, after
# Ctrl-C, or after the shell closed.
#
# Other flags: --plan (show what it would do, change nothing), --yes (no
# confirmation), --destroy (remove what it made; needs protect_data = false),
# --src <dir> (use a local checkout instead of fetching).
#
# Everything big (Terraform, its providers, kubectl, the checkout) goes under
# /tmp: AWS CloudShell keeps only 1 GB in $HOME. The state is in S3, so
# nothing in /tmp needs to survive.
set -euo pipefail

TF_VERSION=1.16.4
TF_SHA256_amd64=dc94af0eef1147718ad7c8daea792ed199e3e0492eec180d0adafa2a65a879df
TF_SHA256_arm64=8263f301cb1a24489a4adeed147bf28504053f77237b3ea97a0ef2972659de30
KUBECTL_VERSION=v1.37.1
KUBECTL_SHA256_amd64=65691ff77eb6fa44c908b77a1082c9f092c3b9733b5cefabec0d1104890e21a8
KUBECTL_SHA256_arm64=ff749f4b78d9c4f1ec87307df9b50119ed819e2094aa9810cb9acffc3286c8c7
# A mirror of this repository can be named here (git URL or path).
REPO=${SH_ENTERPRISE_REPO_URL:-https://github.com/vineetu/simple-host-enterprise}

ref='' tfvars_b64='' yes='' destroy='' src='' plan=''
while [ $# -gt 0 ]; do
  case "$1" in
    --ref) ref=${2:-}; shift 2 ;;
    --tfvars) tfvars_b64=${2:-}; shift 2 ;;
    --yes) yes=1; shift ;;
    --plan) plan=1; shift ;;
    --destroy) destroy=1; shift ;;
    --src) src=${2:-}; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done
say() { printf '\n== %s\n' "$*"; }
# Keys pressed while it worked (Enter to keep AWS CloudShell awake) must not
# answer the next question: drop them first.
drain() { while read -r -t 0.3 -n 1 _ </dev/tty 2>/dev/null; do :; done; }
die() { printf '\nStopped: %s\n' "$*" >&2; exit 1; }
[ -n "$tfvars_b64" ] || die "--tfvars is required (copy the whole line from https://simple-host.app/setup)"
[ -n "$src" ] || [[ "$ref" =~ ^[0-9a-f]{40}$ ]] || die "--ref is the full 40-character commit id"

tfvars=$(printf '%s' "$tfvars_b64" | base64 -d 2>/dev/null) || die "--tfvars is not base64"
# One value, as Terraform sees it: terraform.tfvars wins, then
# TF_VAR_<name> from the environment (a string, a bool or a number).
var() {
  local env="TF_VAR_$1" v
  v=$(printf '%s\n' "$tfvars" | sed -n "s/^$1 *= *\"\{0,1\}\([^\"]*\)\"\{0,1\} *$/\1/p" | head -n1)
  if [ -n "$v" ]; then printf '%s' "$v"; else printf '%s' "${!env:-}"; fi
}
region=$(var region); base=$(var base_domain); create=$(var create_cluster); cluster=$(var cluster_name); name=$(var name)
[ -n "$region" ] && [ -n "$base" ] && [ -n "$create" ] || die "terraform.tfvars needs region, base_domain and create_cluster"
[[ "$region" =~ ^[a-z]{2}(-[a-z]+)+-[0-9]$ ]] || die "region \"$region\" is not an AWS region"
[[ "$base" =~ ^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$ ]] || die "base_domain \"$base\" is not a hostname"
# The same default as variables.tf: sh- and 8 hex of the address's sha1.
[ -n "$name" ] || name="sh-$(printf '%s' "$base" | sha1sum | cut -c1-8)"
[ "$create" = true ] && cluster=${cluster:-$name}
export AWS_REGION=$region AWS_DEFAULT_REGION=$region

say "Simple Host Enterprise for $base"
if [ -z "$plan$destroy" ]; then
  echo "This takes about $([ "$create" = true ] && echo 25 || echo 15) minutes. Keep this tab open and press Enter every 10 minutes or so: AWS CloudShell closes after about 20 minutes without a key press, and closing stops the work. If it does close, open it again and paste the same line: it picks up where it stopped."
fi
command -v aws >/dev/null || die "the AWS CLI is missing (AWS CloudShell has it)"
account=$(aws sts get-caller-identity --query Account --output text 2>/dev/null) || die "this shell is not signed in to AWS (aws sts get-caller-identity failed)"
echo "AWS account $account, region $region"

# An account on AWS's free plan launches only free-tier sizes, and a node
# group that cannot launch its nodes waits most of an hour before it fails:
# say so now instead.
if [ -z "$destroy" ] && [ "$(aws freetier get-account-plan-state --region us-east-1 --query accountPlanType --output text 2>/dev/null || true)" = FREE ]; then
  node_size=$(var node_size); node_size=${node_size:-t3.medium}; db_size=$(var db_size); db_size=${db_size:-db.t4g.small}
  small=yes
  [ "$create" = true ] && [ "$(aws ec2 describe-instance-types --instance-types "$node_size" --query 'InstanceTypes[0].FreeTierEligible' --output text 2>/dev/null)" != True ] && small=no
  case "$db_size" in db.t4g.micro|db.t3.micro) ;; *) small=no ;; esac
  [ "$(var db_backup_days)" = 1 ] || small=no
  [ "$small" = yes ] || die "this AWS account is on the free plan, which cannot launch the cluster's nodes or the database at their sizes, or keep 7 days of database backups. Upgrade it to a paid plan (Billing and Cost Management, then Plans), then run the same line again."
fi

# Tools, under /tmp, each checked against its pinned checksum.
arch=$(uname -m); case "$arch" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) die "unsupported machine: $arch" ;; esac
top=/tmp/simple-host; bin=$top/bin
mkdir -p "$bin" "$top/plugin-cache"; chmod 700 "$top"
export PATH="$bin:$PATH" TF_PLUGIN_CACHE_DIR="$top/plugin-cache" TF_IN_AUTOMATION=1
if ! [ -x "$bin/terraform" ] || ! "$bin/terraform" version | grep -q "v$TF_VERSION"; then
  say "Installing Terraform $TF_VERSION (in $bin)"
  t=$(mktemp -d); sum_var=TF_SHA256_$arch
  curl -fsSL "https://releases.hashicorp.com/terraform/$TF_VERSION/terraform_${TF_VERSION}_linux_$arch.zip" -o "$t/tf.zip"
  printf '%s  %s\n' "${!sum_var}" "$t/tf.zip" | sha256sum -c --quiet -
  unzip -o -q "$t/tf.zip" terraform -d "$bin"; rm -rf "$t"
fi
if ! command -v kubectl >/dev/null; then
  say "Installing kubectl $KUBECTL_VERSION (in $bin)"
  sum_var=KUBECTL_SHA256_$arch
  curl -fsSL "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/$arch/kubectl" -o "$bin/kubectl"
  printf '%s  %s\n' "${!sum_var}" "$bin/kubectl" | sha256sum -c --quiet -
  chmod +x "$bin/kubectl"
fi

# The module, at the commit the setup page names.
work="$top/$base"
mkdir -p "$work"
if [ -n "$src" ]; then
  mod="$(cd "$src" && pwd)/deploy/terraform/aws"
else
  say "Fetching simple-host-enterprise at $ref"
  [ -d "$work/src/.git" ] || git -c init.defaultBranch=main init -q "$work/src"
  git -C "$work/src" fetch -q --depth 1 "$REPO" "$ref"
  git -C "$work/src" -c advice.detachedHead=false checkout -q --force FETCH_HEAD
  [ "$(git -C "$work/src" rev-parse HEAD)" = "$ref" ] || die "the fetched commit is not $ref"
  mod="$work/src/deploy/terraform/aws"
fi
export TF_DATA_DIR="$work/tfdata"
printf '%s\n' "$tfvars" > "$mod/terraform.tfvars"
rm -f "$mod/detected.auto.tfvars" "$mod/zz_recover.tf" "$mod/zz_plan_override.tf"
# A copy of your answers where you will find it again (no secrets in it).
mkdir -p "$HOME/simple-host"; cp "$mod/terraform.tfvars" "$HOME/simple-host/$base.tfvars"
cd "$mod"

# Terraform state: a bucket in this account. Its settings are applied on
# every run, so a run that stopped halfway through creating it is finished.
state_bucket="$name-tfstate-$account-$region"
state_key="$base/terraform.tfstate"
bucket_exists() { aws s3api head-bucket --bucket "$state_bucket" >/dev/null 2>&1; }
if [ -n "$plan" ] && ! bucket_exists; then
  # Nothing exists yet: plan against an empty local state, create nothing.
  printf 'terraform {\n  backend "local" {}\n}\n' > zz_plan_override.tf
  terraform init -input=false -reconfigure >/dev/null
else
  say "Terraform state in s3://$state_bucket"
  if ! bucket_exists; then
    if [ "$region" = us-east-1 ]; then aws s3api create-bucket --bucket "$state_bucket" >/dev/null
    else aws s3api create-bucket --bucket "$state_bucket" --create-bucket-configuration "LocationConstraint=$region" >/dev/null; fi
  fi
  aws s3api put-public-access-block --bucket "$state_bucket" --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
  aws s3api put-bucket-tagging --bucket "$state_bucket" --tagging "TagSet=[{Key=simple-host,Value=$name}]"
  aws s3api put-bucket-versioning --bucket "$state_bucket" --versioning-configuration Status=Enabled
  aws s3api put-bucket-encryption --bucket "$state_bucket" --server-side-encryption-configuration '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
  aws s3api put-bucket-policy --bucket "$state_bucket" --policy "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Sid\":\"TLSOnly\",\"Effect\":\"Deny\",\"Principal\":\"*\",\"Action\":\"s3:*\",\"Resource\":[\"arn:aws:s3:::$state_bucket\",\"arn:aws:s3:::$state_bucket/*\"],\"Condition\":{\"Bool\":{\"aws:SecureTransport\":\"false\"}}}]}"
  terraform init -input=false -reconfigure -backend-config="bucket=$state_bucket" -backend-config="key=$state_key" -backend-config="region=$region" -backend-config="use_lockfile=true" >/dev/null

  # A run that was cut off without a chance to stop (the shell was killed)
  # leaves its lock behind. While apply.sh runs Terraform it keeps a
  # heartbeat next to the state. A lock is cleared only when that heartbeat
  # was written after the lock was taken (so the lock is apply.sh's) and has
  # been silent for 3 minutes. Any other lock (a pipeline, a laptop, a run
  # still going) is left alone.
  if lock=$(aws s3 cp "s3://$state_bucket/$state_key.tflock" - 2>/dev/null); then
    lock_id=$(printf '%s' "$lock" | sed -n 's/.*"ID": *"\([^"]*\)".*/\1/p')
    lock_who=$(printf '%s' "$lock" | sed -n 's/.*"Who": *"\([^"]*\)".*/\1/p')
    lock_at=$(printf '%s' "$lock" | sed -n 's/.*"Created": *"\([^"]*\)".*/\1/p')
    beat=$(aws s3api head-object --bucket "$state_bucket" --key "$state_key.alive" --query LastModified --output text 2>/dev/null || true)
    held() { die "the install is locked by $lock_who since $lock_at ($1). If you are sure nothing is running on it, clear the lock from $mod with: TF_DATA_DIR=$TF_DATA_DIR terraform force-unlock $lock_id"; }
    [ -n "$beat" ] && [ "$beat" != None ] || held "not by a run of this line"
    beat_s=$(date -d "$beat" +%s 2>/dev/null) || held "its heartbeat's time ($beat) cannot be read"
    [ -n "$lock_at" ] || held "the lock has no time"
    lock_s=$(date -d "$lock_at" +%s 2>/dev/null) || held "the lock's time cannot be read"
    [ "$beat_s" -ge "$lock_s" ] || held "not by a run of this line"
    age=$(( $(date +%s) - beat_s ))
    [ "$age" -ge 180 ] || die "another run is working on this install right now (its heartbeat is ${age}s old). Wait for it to finish, or close that shell, and run the same line again."
    say "Clearing the lock a stopped run left"
    terraform force-unlock -force "$lock_id" >/dev/null
  fi
fi
tfstate=$(terraform state list 2>/dev/null || true)
db_addr='aws_db_instance.db'
in_state() { grep -qxF "$1" <<<"$tfstate"; }

# Runs Terraform so that closing the shell stops it cleanly: Terraform runs
# in its own session (the shell's hang-up does not reach it) and writes to a
# log that is shown here; a hang-up, Ctrl-C or kill reaching this script is
# passed to Terraform as an interrupt, which saves the state and releases
# the lock. A heartbeat next to the state tells a later run whether this one
# is still alive.
tf_pid='' tail_pid='' beat_pid=''
# heartbeat <pid>: next to the state every 60 s while <pid> lives, in a
# session of its own so a hang-up does not stop it while Terraform winds down.
heartbeat() {
  : > "$work/heartbeat"
  setsid bash -c 'while kill -0 "$1" 2>/dev/null; do aws s3api put-object --bucket "$2" --key "$3" --body "$4" >/dev/null 2>&1 || true; sleep 60; done' _ "$1" "$state_bucket" "$state_key.alive" "$work/heartbeat" </dev/null >/dev/null 2>&1 &
  beat_pid=$!
}
stop_tf() {
  trap '' HUP INT TERM
  printf '\nStopping cleanly, keeping what is done (this can take a minute)...\n' 2>/dev/null || true
  if [ -n "$tf_pid" ]; then
    kill -INT "$tf_pid" 2>/dev/null || true
    while kill -0 "$tf_pid" 2>/dev/null; do sleep 1; done
  fi
  [ -z "$beat_pid" ] || kill "$beat_pid" 2>/dev/null || true
  [ -z "$tail_pid" ] || kill "$tail_pid" 2>/dev/null || true
  printf 'Stopped. Paste the same line again to pick up where it stopped.\n' 2>/dev/null || true
  exit 130
}
run_tf() {
  local log="$work/terraform.log" from
  printf '\n### %s terraform %s\n' "$(date -u +%FT%TZ)" "$1" >> "$log"
  from=$(( $(wc -l < "$log") + 1 ))
  setsid terraform "$@" -no-color >> "$log" 2>&1 < /dev/null &
  tf_pid=$!
  heartbeat "$tf_pid"
  tail -n +"$from" -f "$log" --pid="$tf_pid" 2>/dev/null &
  tail_pid=$!
  trap stop_tf HUP INT TERM
  local rc=0
  wait "$tf_pid" || rc=$?
  wait "$tail_pid" 2>/dev/null || true
  kill "$beat_pid" 2>/dev/null || true
  trap - HUP INT TERM
  tf_pid='' tail_pid='' beat_pid=''
  return "$rc"
}

# What an existing cluster already runs, so nothing is installed twice and
# nothing of yours is changed. Ours = installed by a Helm release of ours.
detected="$mod/detected.auto.tfvars"; : > "$detected"
if [ "$create" = false ]; then
  say "Looking at cluster $cluster"
  aws eks describe-cluster --name "$cluster" --query cluster.status --output text >/dev/null 2>&1 || die "no EKS cluster named $cluster in $region"
  kc="$work/kubeconfig.detect"
  aws eks update-kubeconfig --name "$cluster" --kubeconfig "$kc" >/dev/null
  k() { kubectl --kubeconfig "$kc" "$@"; }
  [ "$(k auth can-i create namespace 2>/dev/null)" = yes ] || die "this AWS identity cannot create namespaces in $cluster: it needs cluster-admin (an EKS access entry with AmazonEKSClusterAdminPolicy)"
  # <detect> (scripts/test-detect.sh runs this block against a test cluster)
  release_of() { k get "$@" -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null || true; }
  first() { k get deploy -A -l "$1" -o jsonpath='{range .items[*]}{.metadata.namespace} {.metadata.name}{"\n"}{end}' 2>/dev/null | head -n1; }
  crds_of() { k get crd -o name 2>/dev/null | grep "$1" | tr '\n' ' '; }

  if k get crd certificates.cert-manager.io >/dev/null 2>&1; then
    read -r ns dep <<<"$(first app.kubernetes.io/name=cert-manager,app.kubernetes.io/component=controller)" || true
    if [ -n "${ns:-}" ]; then
      if [ "$(release_of -n "$ns" deploy "$dep")" != simple-host-cert-manager ]; then
        img=$(k -n "$ns" get deploy "$dep" -o jsonpath='{.spec.template.spec.containers[0].image}')
        v=$(sed -n 's/.*:v\([0-9]*\)\.\([0-9]*\).*/\1 \2/p' <<<"$img")
        read -r maj min <<<"${v:-0 0}"
        if [ "$maj" -lt 1 ] || { [ "$maj" -eq 1 ] && [ "$min" -lt 15 ]; }; then
          die "the cert-manager in namespace $ns ($img) is older than 1.15, which Simple Host needs (Route 53 DNS-01 with a ServiceAccount token). Upgrade it, then run the same line again."
        fi
        sa=$(k -n "$ns" get deploy "$dep" -o jsonpath='{.spec.template.spec.serviceAccountName}')
        printf 'install_cert_manager = false\ncert_manager_namespace = "%s"\ncert_manager_service_account = "%s"\n' "$ns" "${sa:-cert-manager}" >> "$detected"
        echo "Using the cert-manager already in namespace $ns ($img)"
      fi
    else
      owner=$(release_of crd certificates.cert-manager.io)
      [ "$owner" = simple-host-cert-manager ] || die "cert-manager's CRDs are here (installed by ${owner:-hand, not by Helm}) but no cert-manager runs. Start that cert-manager again, or, if nothing uses them, remove them: kubectl delete $(crds_of cert-manager.io)"
    fi
  fi

  if k get crd externalsecrets.external-secrets.io >/dev/null 2>&1; then
    read -r ns dep <<<"$(first app.kubernetes.io/name=external-secrets)" || true
    if [ -n "${ns:-}" ]; then
      if [ "$(release_of -n "$ns" deploy "$dep")" != simple-host-external-secrets ]; then
        served=$(k get crd externalsecrets.external-secrets.io -o jsonpath='{.spec.versions[?(@.served==true)].name}')
        grep -qw v1 <<<"$served" || die "the External Secrets Operator in namespace $ns serves only $served; Simple Host needs external-secrets.io/v1 (External Secrets 0.16 or later). Upgrade it, then run the same line again."
        printf 'install_external_secrets = false\n' >> "$detected"
        echo "Using the External Secrets Operator already in namespace $ns"
      fi
    else
      owner=$(release_of crd externalsecrets.external-secrets.io)
      [ "$owner" = simple-host-external-secrets ] || die "External Secrets' CRDs are here (installed by ${owner:-hand, not by Helm}) but no External Secrets Operator runs. Start it again, or, if nothing uses them, remove them: kubectl delete $(crds_of external-secrets.io)"
    fi
  fi

  # A namespace simple-host is this install's when it carries its label
  # (a run cut off during the install left it); otherwise it is another one.
  if k get namespace simple-host >/dev/null 2>&1; then
    ns_owner=$(k get namespace simple-host -o jsonpath='{.metadata.labels.simple-host\.app/install}' 2>/dev/null || true)
    [ "$ns_owner" = "$name" ] || die "namespace simple-host already exists in $cluster and was not made by this install (its label simple-host.app/install is \"${ns_owner:-missing}\", not \"$name\"). Is another Simple Host installed there? Remove it first (kubectl delete namespace simple-host) if it is a leftover."
  fi
  # </detect>

  # IAM roles for service accounts need the cluster's OIDC provider. Made
  # here, outside Terraform, when missing, so a later removal never takes it
  # away from roles of your own.
  if [ -z "$destroy" ]; then
    issuer=$(aws eks describe-cluster --name "$cluster" --query cluster.identity.oidc.issuer --output text)
    if ! aws iam list-open-id-connect-providers --query 'OpenIDConnectProviderList[].Arn' --output text | tr '\t' '\n' | grep -q "/${issuer#https://}\$"; then
      [ -n "$plan" ] && echo "(it would add the cluster's IAM OIDC provider for $issuer)" || {
        aws iam create-open-id-connect-provider --url "$issuer" --client-id-list sts.amazonaws.com --tags "Key=simple-host,Value=$name" >/dev/null
        echo "Added the cluster's IAM OIDC provider (it stays when Simple Host is removed)"
      }
    fi
  fi
  rm -f "$kc"
fi
# A public Route 53 zone for the address: one this install made (its tag)
# but lost track of is adopted; any other is yours and used as it is.
zone=$(aws route53 list-hosted-zones-by-name --dns-name "$base." --max-items 1 --query "HostedZones[?Name=='$base.' && !Config.PrivateZone].Id | [0]" --output text 2>/dev/null || true)
if [ -n "$zone" ] && [ "$zone" != None ] && ! in_state 'aws_route53_zone.base[0]'; then
  zone=${zone#/hostedzone/}
  if [ "$(aws route53 list-tags-for-resource --resource-type hostedzone --resource-id "$zone" --query 'ResourceTagSet.Tags[?Key==`simple-host`].Value | [0]' --output text 2>/dev/null)" = "$name" ]; then
    echo "Adopting the DNS zone $zone, which an earlier run made"
    printf 'import {\n  to = aws_route53_zone.base[0]\n  id = "%s"\n}\n' "$zone" > zz_recover.tf
  else
    printf 'dns_zone_id = "%s"\n' "$zone" >> "$detected"
  fi
fi

if [ -n "$plan" ] && [ -n "$destroy" ]; then
  say "terraform plan -destroy (nothing is changed)"
  terraform plan -destroy -input=false -var-file=terraform.tfvars
  rm -f zz_plan_override.tf
  exit 0
fi

if [ -n "$destroy" ]; then
  [ "$(var protect_data)" = false ] || die "the database and the bucket are protected (protect_data). To remove everything, take what you need first (docs/uninstall.md), then run the same line again with TF_VAR_protect_data=false in front of bash and --destroy at the end. See deploy/terraform/aws/README.md, Remove."
  say "Removing Simple Host from $base"
  if [ -z "$yes" ]; then
    answer=''
    for try in 1 2; do
      drain; printf 'This deletes the database and every site. Type the address (%s) to go ahead: ' "$base"
      read -r answer </dev/tty || die "nothing typed"
      [ -n "$answer" ] && break
    done
    [ "$answer" = "$base" ] || die "not removed"
  fi
  # Deletion protection is lifted first (it is a setting of the database
  # and the bucket, and destroy cannot change settings).
  # The secrets' recovery window goes to 0 the same way, so nothing named
  # after this install is left scheduled for deletion.
  # Only what the state holds is targeted (a target it lacks would be created).
  targets=''
  for t in "$db_addr" aws_s3_bucket.sites aws_secretsmanager_secret.app aws_secretsmanager_secret.oidc; do
    in_state "$t" && targets+=" -target=$t"
  done
  if [ -n "$targets" ]; then
    # shellcheck disable=SC2086
    run_tf apply -input=false -auto-approve -var-file=terraform.tfvars $targets || die "lifting the deletion protection stopped; run the same command again"
  fi
  run_tf destroy -input=false -auto-approve -var-file=terraform.tfvars || die "the removal stopped; run the same command again"
  exit 0
fi

if [ -n "$plan" ]; then
  say "terraform plan (nothing is changed)"
  terraform plan -input=false -var-file=terraform.tfvars
  rm -f zz_plan_override.tf
  exit 0
fi

# Picking up after a run that stopped halfway.
# Secrets scheduled for deletion by an earlier removal come back.
for s in app oidc; do
  if aws secretsmanager describe-secret --secret-id "$name/$s" --query DeletedDate --output text 2>/dev/null | grep -q '[0-9]' &&
    [ "$(aws secretsmanager describe-secret --secret-id "$name/$s" --query 'Tags[?Key==`simple-host`].Value | [0]' --output text 2>/dev/null)" = "$name" ]; then
    aws secretsmanager restore-secret --secret-id "$name/$s" >/dev/null && echo "Restored the secret $name/$s"
  fi
done
# Long creations an interrupt cut short are kept in the state marked for
# replacement; waiting for them to finish and keeping them saves 10 minutes.
# One that is not in the state at all (the shell was killed outright) is
# adopted.
recover=''
if [ -n "$tfstate" ]; then
  tainted=$(terraform show -json 2>/dev/null | python3 -c '
import json,sys
d=json.load(sys.stdin).get("values",{}).get("root_module",{})
def walk(m):
  for r in m.get("resources",[]):
    if r.get("tainted"): print(r["address"])
  for c in m.get("child_modules",[]): walk(c)
walk(d)' 2>/dev/null || true)
else
  tainted=''
fi
eks_addr='module.eks[0].aws_eks_cluster.this[0]'
ng_addr='module.eks[0].module.eks_managed_node_group["default"].aws_eks_node_group.this[0]'
db_addr='aws_db_instance.db'
# <adopt> (scripts/test-adopt.sh runs this block against a stand-in aws)
# Things a run cut off outright made but never recorded in the state. They
# are adopted only when they carry this install's tag, simple-host=<name>
# (every resource this module makes has it from the moment it is created);
# anything else by that name is someone else's, and the run stops.
until_status() { # <command printing a status> <wanted> <"failed states"> <seconds>
  local end=$((SECONDS + $4)) st
  while [ $SECONDS -lt $end ]; do
    st=$(eval "$1" 2>/dev/null || true)
    [ "$st" = "$2" ] && return 0
    case " $3 " in *" $st "*) return 1 ;; esac
    sleep 15
  done
  return 1
}
import_block() { recover+=$'import {\n  to = '"$1"$'\n  id = "'"$2"$'"\n}\n'; }
q() { local v; v=$(aws "$@" --output text 2>/dev/null) || return 0; [ "$v" = None ] || printf '%s' "$v"; }
not_ours() { die "$1 already exists in this AWS account and was not made by this install (it has no simple-host=$name tag). Nothing was changed. Choose another name (name = \"...\" in terraform.tfvars), or remove $1 if it is left from something else."; }
# adopt <address> <import id> <what> <tag of the existing thing, or "absent">
adopt() {
  in_state "$1" && return 0
  [ "$4" = absent ] && return 0
  [ "$4" = "$name" ] || not_ours "$3"
  echo "Adopting $3, which an earlier run made"
  import_block "$1" "$2"
}
if [ "$create" = true ] && ! in_state "$eks_addr" && aws eks describe-cluster --name "$cluster" >/dev/null 2>&1; then
  t=$(q eks describe-cluster --name "$cluster" --query 'cluster.tags."simple-host"')
  [ "$t" = "$name" ] || not_ours "the EKS cluster $cluster"
  echo "Waiting for the cluster an earlier run started, to adopt it"
  until_status "aws eks describe-cluster --name $cluster --query cluster.status --output text" ACTIVE FAILED 1200 || die "the cluster $cluster an earlier run started did not become active; delete it in the EKS console and run the same line again"
  import_block "$eks_addr" "$cluster"
fi
# The cluster's own role: a new one would force a new cluster.
if [ "$create" = true ] && ! in_state 'module.eks[0].aws_iam_role.this[0]'; then
  crole=$(q eks describe-cluster --name "$cluster" --query cluster.roleArn); crole=${crole##*/}
  [ -n "$crole" ] && adopt 'module.eks[0].aws_iam_role.this[0]' "$crole" "the cluster's IAM role $crole" "$(q iam list-role-tags --role-name "$crole" --query 'Tags[?Key==`simple-host`].Value | [0]')"
fi
if [ "$create" = true ] && ! in_state "$ng_addr" && aws eks describe-cluster --name "$cluster" >/dev/null 2>&1; then
  ng=$(q eks list-nodegroups --cluster-name "$cluster" --query 'nodegroups[?starts_with(@, `default-`)] | [0]')
  if [ -n "$ng" ] && [ "$(q eks describe-nodegroup --cluster-name "$cluster" --nodegroup-name "$ng" --query 'nodegroup.tags."simple-host"')" = "$name" ]; then
    echo "Waiting for the nodes an earlier run started, to adopt them"
    if until_status "aws eks describe-nodegroup --cluster-name $cluster --nodegroup-name $ng --query nodegroup.status --output text" ACTIVE 'CREATE_FAILED DEGRADED DELETING' 600; then
      import_block "$ng_addr" "$cluster:$ng"
    else
      # Nodes that never joined: remove them, and new ones are made.
      echo "Those nodes did not start; removing them to make new ones"
      aws eks delete-nodegroup --cluster-name "$cluster" --nodegroup-name "$ng" >/dev/null
      aws eks wait nodegroup-deleted --cluster-name "$cluster" --nodegroup-name "$ng"
    fi
  fi
fi
if ! in_state "$db_addr" && aws rds describe-db-instances --db-instance-identifier "$name" >/dev/null 2>&1; then
  t=$(q rds describe-db-instances --db-instance-identifier "$name" --query 'DBInstances[0].TagList[?Key==`simple-host`].Value | [0]')
  [ "$t" = "$name" ] || not_ours "the RDS database $name"
  echo "Waiting for the database an earlier run started, to adopt it"
  until_status "aws rds describe-db-instances --db-instance-identifier $name --query DBInstances[0].DBInstanceStatus --output text" available 'failed incompatible-parameters incompatible-network storage-full' 1800 || die "the database $name an earlier run started did not become available; see the RDS console"
  import_block "$db_addr" "$name"
fi
if [ "$create" = true ]; then
  kms_key=$(q kms describe-key --key-id "alias/eks/$cluster" --query KeyMetadata.KeyId)
  if [ -n "$kms_key" ]; then
    t=$(q kms list-resource-tags --key-id "$kms_key" --query 'Tags[?TagKey==`simple-host`].TagValue | [0]')
    adopt 'module.eks[0].module.kms.aws_kms_key.this[0]' "$kms_key" "the KMS key behind alias/eks/$cluster" "$t"
    # An alias carries no tags: it is ours when the key it names is.
    adopt 'module.eks[0].module.kms.aws_kms_alias.this["cluster"]' "alias/eks/$cluster" "the KMS alias alias/eks/$cluster" "$t"
  fi
  if [ -n "$(q logs describe-log-groups --log-group-name-prefix "/aws/eks/$cluster/cluster" --query 'logGroups[?logGroupName==`'"/aws/eks/$cluster/cluster"'`].logGroupName | [0]')" ]; then
    adopt 'module.eks[0].aws_cloudwatch_log_group.this[0]' "/aws/eks/$cluster/cluster" "the log group /aws/eks/$cluster/cluster" "$(q logs list-tags-log-group --log-group-name "/aws/eks/$cluster/cluster" --query 'tags."simple-host"')"
  fi
fi
sgarn=$(q rds describe-db-subnet-groups --db-subnet-group-name "$name" --query 'DBSubnetGroups[0].DBSubnetGroupArn')
[ -n "$sgarn" ] && adopt 'aws_db_subnet_group.db' "$name" "the DB subnet group $name" "$(q rds list-tags-for-resource --resource-name "$sgarn" --query 'TagList[?Key==`simple-host`].Value | [0]')"
dbsg=$(q ec2 describe-security-groups --filters "Name=tag:simple-host,Values=$name" "Name=group-name,Values=$name-db-*" --query 'SecurityGroups[0].GroupId')
[ -n "$dbsg" ] && adopt 'aws_security_group.db' "$dbsg" "the database security group $dbsg" "$name"
bucket="$name-sites-$account-$region"
if aws s3api head-bucket --bucket "$bucket" >/dev/null 2>&1; then
  adopt 'aws_s3_bucket.sites' "$bucket" "the S3 bucket $bucket" "$(q s3api get-bucket-tagging --bucket "$bucket" --query 'TagSet[?Key==`simple-host`].Value | [0]')"
fi
for sn in app oidc; do
  arn=$(q secretsmanager describe-secret --secret-id "$name/$sn" --query ARN)
  [ -n "$arn" ] || continue
  adopt "aws_secretsmanager_secret.$sn" "$arn" "the secret $name/$sn" "$(q secretsmanager describe-secret --secret-id "$name/$sn" --query 'Tags[?Key==`simple-host`].Value | [0]')"
  # A value already stored is never replaced by one this run makes up.
  in_state "aws_secretsmanager_secret_version.$sn" && continue
  vid=$(q secretsmanager list-secret-version-ids --secret-id "$name/$sn" --query 'Versions[?contains(VersionStages, `AWSCURRENT`)].VersionId | [0]')
  [ -n "$vid" ] || continue
  if [ "$sn" = app ] && ! in_state random_bytes.backup_envelope_key; then
    die "the secret $name/app already holds keys this run does not know, among them the envelope key every site is encrypted with. Nothing was changed. If the Terraform state was lost, restore it (the state bucket keeps old versions); if nothing was ever published, delete the secret: aws secretsmanager delete-secret --secret-id $name/app --force-delete-without-recovery"
  fi
  echo "Keeping the value stored in $name/$sn"
  import_block "aws_secretsmanager_secret_version.$sn" "$arn|$vid"
done
for r in app secrets dns; do
  role="$name-$r-$region"
  if aws iam get-role --role-name "$role" >/dev/null 2>&1; then
    adopt "aws_iam_role.sa[\"$r\"]" "$role" "the IAM role $role" "$(q iam list-role-tags --role-name "$role" --query 'Tags[?Key==`simple-host`].Value | [0]')"
    # Its inline policy is ours when the role is (a policy has no tags).
    aws iam get-role-policy --role-name "$role" --policy-name simple-host >/dev/null 2>&1 && adopt "aws_iam_role_policy.sa[\"$r\"]" "$role:simple-host" "the policy of $role" "$name"
  fi
done
# </adopt>
keep() {
  echo "Waiting for $2 that an earlier run started, to keep it"
  if eval "$3"; then terraform untaint "$1" >/dev/null && echo "Kept $2"; else echo "$2 did not finish; it will be made again"; fi
}
# A tainted entry in this install's own state is this install's: keep it.
if [ "$create" = true ]; then
  if grep -qxF "$eks_addr" <<<"$tainted"; then keep "$eks_addr" "the cluster" "until_status 'aws eks describe-cluster --name $cluster --query cluster.status --output text' ACTIVE FAILED 1200"; fi
  if grep -qxF "$ng_addr" <<<"$tainted"; then
    ng=$(terraform state show -no-color "$ng_addr" 2>/dev/null | sed -n 's/^ *node_group_name *= *"\(.*\)"/\1/p' | head -n1)
    # Nodes that cannot join never finish: give them 10 minutes, then make them again.
    keep "$ng_addr" "the nodes" "until_status 'aws eks describe-nodegroup --cluster-name $cluster --nodegroup-name $ng --query nodegroup.status --output text' ACTIVE 'CREATE_FAILED DEGRADED DELETING' 600"
  fi
fi
# The zone is usable as soon as it exists (only waiting for Route 53 to
# spread it was cut short); making it again would change its name servers.
if grep -qxF 'aws_route53_zone.base[0]' <<<"$tainted"; then terraform untaint 'aws_route53_zone.base[0]' >/dev/null && echo "Kept the DNS zone"; fi
if grep -qxF "$db_addr" <<<"$tainted"; then keep "$db_addr" "the database" "until_status 'aws rds describe-db-instances --db-instance-identifier $name --query DBInstances[0].DBInstanceStatus --output text' available 'failed incompatible-parameters incompatible-network storage-full' 1800"; fi

[ -n "$recover" ] && printf '%s' "$recover" >> zz_recover.tf

# A Helm install cut short leaves its release pending, and the next install
# of that name would refuse: forget such a release. Its objects stay and the
# next install adopts them.
if aws eks describe-cluster --name "$cluster" >/dev/null 2>&1; then
  kc="$work/kubeconfig.check"
  if aws eks update-kubeconfig --name "$cluster" --kubeconfig "$kc" >/dev/null 2>&1 && kubectl --kubeconfig "$kc" get ns >/dev/null 2>&1; then
    for rel in simple-host-cert-manager simple-host-external-secrets simple-host-traefik; do
      kubectl --kubeconfig "$kc" get secret -A -l "owner=helm,name=$rel" -o jsonpath='{range .items[*]}{.metadata.namespace} {.metadata.name} {.metadata.labels.status}{"\n"}{end}' 2>/dev/null |
        while read -r ns sec st; do
          case "$st" in pending-*) kubectl --kubeconfig "$kc" -n "$ns" delete secret "$sec" >/dev/null && echo "Cleared the unfinished Helm release $rel" ;; esac
        done
    done
  fi
  rm -f "$kc"
fi

# The sign-in app's client secret: asked for once, never shown or stored on
# disk. Later runs keep the stored one.
if [ -z "${TF_VAR_oidc_client_secret:-}" ] && ! aws secretsmanager describe-secret --secret-id "$name/oidc" --query 'VersionIdsToStages' --output text 2>/dev/null | grep -q AWSCURRENT; then
  say "Your sign-in app's client secret"
  drain; printf 'Client secret (not shown as you type): '
  trap 'stty echo 2>/dev/null' INT
  if ! read -rs TF_VAR_oidc_client_secret </dev/tty; then trap - INT; echo; die "no client secret was typed"; fi
  trap - INT; echo
  [ -n "$TF_VAR_oidc_client_secret" ] || die "the client secret is empty"
  export TF_VAR_oidc_client_secret
fi

# One confirmation, on the whole plan, then the work in two steps: the
# cluster and the database first (the long part), then the rest. The plan
# is made with --yes too: what it would remove or replace is always shown,
# and replacing the cluster or the database always needs the address typed.
say "What it will do"
planlog="$work/plan.log"
terraform plan -input=false -no-color -var-file=terraform.tfvars -compact-warnings > "$planlog" 2>&1 & plan_pid=$!; heartbeat "$plan_pid"; wait "$plan_pid" || { cat "$planlog"; die "the plan failed (above). Nothing was changed."; }
# "No changes" has no such section: grep then finds nothing, which must not stop the run.
[ -n "$yes" ] || { sed -n '/Terraform will perform/,$p' "$planlog" | grep -v '^$' | tail -n 60 || true; }
grep -E '^Plan:' "$planlog" || true
# Replacing the generated files and the install step is routine; anything
# else removed or replaced is worth a second look.
gone=$(grep -E '^  # .* (must be replaced|will be destroyed)' "$planlog" | grep -vE '# (local_file|terraform_data)\.' || true)
[ -z "$gone" ] || printf '\nNote: it will REMOVE or REPLACE these:\n%s\n' "$gone"
if grep -qE '# (module\.eks\[0\]\.aws_eks_cluster\.this\[0\]|aws_db_instance\.db) (must be replaced|will be destroyed)' <<<"$gone"; then
  printf '\nThis would REPLACE the cluster or the database (a new database is an empty one).\n'
  answer=''
  for try in 1 2; do
    drain; printf 'Type the address (%s) to go ahead anyway: ' "$base"
    read -r answer </dev/tty || die "nothing typed; nothing was changed"
    [ -n "$answer" ] && break
  done
  [ "$answer" = "$base" ] || die "nothing was changed"
elif [ -z "$yes" ]; then
  answer=''
  for try in 1 2; do
    drain; printf '\nType yes to go ahead: '
    read -r answer </dev/tty || die "nothing typed"
    [ -n "$answer" ] && break
  done
  [ "$answer" = yes ] || die "nothing was changed"
fi
stage1=''
if [ "$create" = true ]; then
  if [ -n "$recover" ] || ! in_state "$eks_addr" || ! in_state "$ng_addr" || ! in_state "$db_addr"; then stage1=yes; fi
fi
if [ -n "$stage1" ]; then
  say "Step 1 of 2: the cluster and the database (about 15 minutes)"
  run_tf apply -input=false -auto-approve -compact-warnings -var-file=terraform.tfvars -target=module.vpc -target=module.eks -target=aws_db_instance.db || die "step 1 stopped (above). Run the same line again to pick up where it stopped."
  say "Step 2 of 2: secrets, certificates and Simple Host (about 10 minutes)"
else
  say "Setting it up (about 10 minutes)"
fi
run_tf apply -input=false -auto-approve -compact-warnings -var-file=terraform.tfvars || die "it stopped (above). Run the same line again to pick up where it stopped."
rm -f zz_recover.tf
unset TF_VAR_oidc_client_secret

# Anything tagged for this install that the state does not know is left over
# from a run cut off outright (a NAT gateway keeps costing money): say so.
known=$(terraform show -json 2>/dev/null | python3 -c '
import json,sys
def walk(m):
  for r in m.get("resources",[]):
    v=r.get("values") or {}
    for k in ("arn","id"):
      if isinstance(v.get(k),str): print(v[k])
  for c in m.get("child_modules",[]): walk(c)
walk(json.load(sys.stdin).get("values",{}).get("root_module",{}))' 2>/dev/null || true)
if tagged=$(aws resourcegroupstaggingapi get-resources --tag-filters "Key=simple-host,Values=$name" --resource-type-filters ec2:vpc ec2:natgateway ec2:elastic-ip ec2:internet-gateway ec2:security-group rds:db secretsmanager:secret iam:role iam:policy kms:key logs:log-group --query 'ResourceTagMappingList[].ResourceARN' --output text 2>&1); then
  left=$(tr '\t' '\n' <<<"$tagged" | while read -r a; do
    [ -z "$a" ] && continue
    grep -qxF "$a" <<<"$known" || grep -qxF "${a##*/}" <<<"$known" || grep -qxF "${a##*:}" <<<"$known" && continue
    # A key waiting out its deletion window is already on its way.
    case "$a" in *:kms:*) [ "$(aws kms describe-key --key-id "$a" --query KeyMetadata.KeyState --output text 2>/dev/null)" = PendingDeletion ] && continue ;; esac
    echo "$a"
  done || true)
else
  left=''
  printf '\n(Could not look for leftovers of a cut-off run: %s)\n' "$tagged"
fi
if [ -n "$left" ]; then
  say "Tagged for this install but not part of it: left over from a run that was cut off. Check them in the AWS console and delete them there"
  printf '%s\n' "$left"
fi
say "Next steps"
terraform output -raw next_steps
