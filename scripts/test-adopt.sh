#!/usr/bin/env bash
# Runs the adoption block of deploy/terraform/aws/apply.sh (between
# "# <adopt>" and "# </adopt>") against a stand-in `aws` command, to check
# that a run only adopts what carries this install's tag:
#   1. nothing left over                         -> nothing imported
#   2. cluster, database, bucket, role with our tag -> all imported
#   3. an EKS cluster by that name, someone else's -> stops, nothing imported
#   4. an RDS database by that name, untagged      -> stops
#   5. a secret by that name, someone else's       -> stops
#   6. a KMS alias whose key is someone else's     -> stops
#   7. the cluster's IAM role, someone else's        -> stops
#   8. our client secret with a stored value         -> its version imported (never rewritten)
#   9. our app secret with keys, generator unknown   -> stops (no new envelope key)
#  10. same, generator in the state                  -> its version imported
#  11. (TEST_ADOPT_AWS=1) a real Terraform plan of case 8 with no client
#      secret given: import only, no value change
# Needs bash only. Usage: bash scripts/test-adopt.sh
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
block=$(sed -n '/# <adopt>/,/# <\/adopt>/p' "$here/deploy/terraform/aws/apply.sh")
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

# The stand-in: FAKE_<THING>=<tag> means the thing exists with that
# simple-host tag ("-" = exists, untagged); unset means it does not exist.
cat > "$tmp/aws" <<'EOF'
#!/usr/bin/env bash
a="$*"
tag() { [ "${1:--}" = - ] && echo None || echo "$1"; }
case "$a" in
  "eks describe-cluster --name c1 --query cluster.tags.\"simple-host\""*) [ -n "${FAKE_CLUSTER:-}" ] && tag "$FAKE_CLUSTER" || exit 254 ;;
  "eks describe-cluster --name c1 --query cluster.status"*) [ -n "${FAKE_CLUSTER:-}" ] && echo ACTIVE || exit 254 ;;
  "eks describe-cluster --name c1 --query cluster.roleArn"*) [ -n "${FAKE_CLUSTER:-}" ] && echo arn:aws:iam::111:role/c1-cluster-2026 || exit 254 ;;
  "eks describe-cluster --name c1"*) [ -n "${FAKE_CLUSTER:-}" ] || exit 254 ;;
  "eks list-nodegroups"*) echo None ;;
  "rds describe-db-instances --db-instance-identifier sh-test --query DBInstances[0].TagList"*) [ -n "${FAKE_DB:-}" ] && tag "$FAKE_DB" || exit 254 ;;
  "rds describe-db-instances --db-instance-identifier sh-test --query DBInstances[0].DBInstanceStatus"*) echo available ;;
  "rds describe-db-instances --db-instance-identifier sh-test"*) [ -n "${FAKE_DB:-}" ] || exit 254 ;;
  "kms describe-key --key-id alias/eks/c1"*) [ -n "${FAKE_KMS:-}" ] && echo key-1 || exit 254 ;;
  "kms list-resource-tags"*) tag "$FAKE_KMS" ;;
  "s3api head-bucket --bucket sh-test-sites-111-us-east-2"*) [ -n "${FAKE_BUCKET:-}" ] || exit 254 ;;
  "s3api get-bucket-tagging"*) tag "$FAKE_BUCKET" ;;
  "secretsmanager describe-secret --secret-id sh-test/app --query ARN"*) [ -n "${FAKE_SECRET:-}" ] && echo arn:secret:app || exit 254 ;;
  "secretsmanager describe-secret --secret-id sh-test/app --query Tags"*) tag "$FAKE_SECRET" ;;
  "secretsmanager list-secret-version-ids --secret-id sh-test/app"*) echo "${FAKE_APP_VERSION:-None}" ;;
  "secretsmanager describe-secret --secret-id sh-test/oidc --query ARN"*) [ -n "${FAKE_OIDC:-}" ] && echo arn:secret:oidc || exit 254 ;;
  "secretsmanager describe-secret --secret-id sh-test/oidc --query Tags"*) tag "$FAKE_OIDC" ;;
  "secretsmanager list-secret-version-ids --secret-id sh-test/oidc"*) echo "${FAKE_OIDC_VERSION:-None}" ;;
  "iam list-role-tags --role-name c1-cluster-2026"*) tag "${FAKE_CLUSTER_ROLE:-$FAKE_CLUSTER}" ;;
  "iam get-role --role-name sh-test-app-us-east-2"*) [ -n "${FAKE_ROLE:-}" ] || exit 254 ;;
  "iam list-role-tags --role-name sh-test-app-us-east-2"*) tag "$FAKE_ROLE" ;;
  "iam get-role-policy --role-name sh-test-app-us-east-2"*) [ -n "${FAKE_ROLE:-}" ] || exit 254 ;;
  *) exit 254 ;;
esac
EOF
chmod +x "$tmp/aws"

run() {
  ( set -euo pipefail
    export PATH="$tmp:$PATH"
    create=true cluster=c1 name=sh-test account=111 region=us-east-2 recover=''
    in_state() { grep -qxF "$1" <<<"${IN_STATE:-}"; }
    die() { printf 'Stopped: %s\n' "$*"; exit 1; }
    eks_addr='module.eks[0].aws_eks_cluster.this[0]'
    ng_addr='module.eks[0].module.eks_managed_node_group["default"].aws_eks_node_group.this[0]'
    db_addr='aws_db_instance.db'
    eval "$block"
    printf '%s' "$recover" | grep '  to = ' || true ) 2>&1 || true
}
fail=0
expect() {
  local name=$1 want=$2 not=$3 got
  got=$(run)
  if grep -qE "$want" <<<"$got" && { [ -z "$not" ] || ! grep -qE "$not" <<<"$got"; }; then echo "ok   $name"
  else echo "FAIL $name:"; sed 's/^/     /' <<<"$got"; fail=1; fi
}

expect "1 nothing left over: nothing imported" '^$|^[^t]*$' 'to = '
FAKE_CLUSTER=sh-test FAKE_DB=sh-test FAKE_BUCKET=sh-test FAKE_ROLE=sh-test FAKE_KMS=sh-test \
  expect "2 ours: cluster, database, key, alias, bucket, role and its policy imported" \
  'aws_eks_cluster.*' ''
FAKE_CLUSTER=sh-test FAKE_DB=sh-test FAKE_BUCKET=sh-test FAKE_ROLE=sh-test FAKE_KMS=sh-test run | grep -c '  to = ' | grep -qx 8 && echo "ok   2b eight imports (cluster, its role, database, key, alias, bucket, role, policy)" || { echo "FAIL 2b"; fail=1; }
FAKE_CLUSTER=prod-team expect "3 someone else's EKS cluster: stop" 'Stopped: the EKS cluster c1 already exists .*not made by this install' 'to = '
FAKE_DB=- expect "4 untagged RDS database: stop" 'Stopped: the RDS database sh-test already exists' 'to = '
FAKE_SECRET=other expect "5 someone else's secret: stop" 'Stopped: the secret sh-test/app already exists' 'to = '
FAKE_KMS=other expect "6 someone else's KMS key behind the alias: stop" 'Stopped: the KMS key behind alias/eks/c1 already exists' 'to = '
FAKE_CLUSTER=sh-test FAKE_CLUSTER_ROLE=other expect "7 the cluster's IAM role someone else's: stop" 'Stopped: the cluster.s IAM role c1-cluster-2026 already exists' ''
FAKE_OIDC=sh-test FAKE_OIDC_VERSION=v-1 expect "8 our client secret with a stored value: kept (its version imported, never rewritten)" 'aws_secretsmanager_secret_version.oidc' ''
FAKE_SECRET=sh-test FAKE_APP_VERSION=v-2 expect "9 our app secret with keys, its generator not in the state: stop, no new envelope key" 'Stopped: the secret sh-test/app already holds keys' 'to = '
FAKE_SECRET=sh-test FAKE_APP_VERSION=v-2 IN_STATE=random_bytes.backup_envelope_key expect "10 our app secret with keys, generator in the state: version imported" 'aws_secretsmanager_secret_version.app' ''

# With TEST_ADOPT_AWS=1 (and AWS credentials, Terraform, kubectl-free):
# the reviewer's case for real. A tagged <name>/oidc with a stored value is
# adopted by a run with no TF_VAR_oidc_client_secret; the plan must import
# the secret and its version and change neither.
if [ "${TEST_ADOPT_AWS:-}" = 1 ]; then
  n=sh-adopt$RANDOM; region=${AWS_REGION:-us-east-2}
  work=$(mktemp -d); cp -r "$here/deploy/terraform/aws/." "$work/"; cd "$work"
  rm -rf .terraform .work zz_*.tf terraform.tfvars
  printf 'terraform {\n  backend "local" {}\n}\n' > zz_override.tf
  arn=$(aws secretsmanager create-secret --region "$region" --name "$n/oidc" --description "Simple Host: the sign-in app's client secret" --secret-string '{"OIDC_CLIENT_SECRET":"kept-value"}' --tags "Key=simple-host,Value=$n" "Key=sh-cloudsetup,Value=test-adopt" --query ARN --output text)
  vid=$(aws secretsmanager list-secret-version-ids --region "$region" --secret-id "$n/oidc" --query 'Versions[0].VersionId' --output text)
  printf 'import {\n  to = aws_secretsmanager_secret.oidc\n  id = "%s"\n}\nimport {\n  to = aws_secretsmanager_secret_version.oidc\n  id = "%s|%s"\n}\n' "$arn" "$arn" "$vid" > zz_recover.tf
  printf 'create_cluster = false\ncluster_name = "none"\nregion = "%s"\nbase_domain = "x.example.com"\nadmin_emails = ["a@example.com"]\noidc_issuer = "https://example.okta.com"\noidc_client_id = "x"\nname = "%s"\n' "$region" "$n" > terraform.tfvars
  unset TF_VAR_oidc_client_secret
  export TF_VAR_tags='{"sh-cloudsetup"="test-adopt"}'
  terraform init -input=false >/dev/null
  terraform plan -input=false -out=p.bin -target=aws_secretsmanager_secret.oidc -target=aws_secretsmanager_secret_version.oidc >/dev/null 2>plan.err || { cat plan.err; }
  verdict=$(terraform show -json p.bin | python3 -c '
import json,sys
# Settings Terraform keeps only in its state (no AWS call) may be filled in.
state_only={"force_overwrite_replica_secret","recovery_window_in_days"}
d=json.load(sys.stdin); out=[]
for r in d.get("resource_changes",[]):
  if not r["address"].startswith("aws_secretsmanager_secret"): continue
  c=r["change"]; b=c.get("before") or {}; a=c.get("after") or {}
  changed=sorted(k for k in set(a)|set(b) if a.get(k)!=b.get(k) and k not in state_only)
  out.append("%s %s%s changed=%s" % (r["address"], ",".join(c["actions"]), " import" if c.get("importing") else "", ",".join(changed) or "nothing"))
print("\n".join(out))')
  aws secretsmanager delete-secret --region "$region" --secret-id "$n/oidc" --force-delete-without-recovery >/dev/null
  cd "$here"; rm -rf "$work"
  if grep -q 'aws_secretsmanager_secret_version.oidc no-op import changed=nothing' <<<"$verdict" && grep -q 'aws_secretsmanager_secret.oidc .* import changed=nothing' <<<"$verdict" && ! grep -qE 'create|delete' <<<"$verdict"; then
    echo "ok   11 (AWS) adopted client secret: imported, value untouched"; sed 's/^/     /' <<<"$verdict"
  else echo "FAIL 11 (AWS):"; sed 's/^/     /' <<<"$verdict"; fail=1; fi
fi
exit $fail
