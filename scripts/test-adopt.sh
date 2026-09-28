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
    in_state() { return 1; }
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
FAKE_CLUSTER=sh-test FAKE_DB=sh-test FAKE_BUCKET=sh-test FAKE_ROLE=sh-test FAKE_KMS=sh-test run | grep -c '  to = ' | grep -qx 7 && echo "ok   2b seven imports (cluster, database, key, alias, bucket, role, policy)" || { echo "FAIL 2b"; fail=1; }
FAKE_CLUSTER=prod-team expect "3 someone else's EKS cluster: stop" 'Stopped: the EKS cluster c1 already exists .*not made by this install' 'to = '
FAKE_DB=- expect "4 untagged RDS database: stop" 'Stopped: the RDS database sh-test already exists' 'to = '
FAKE_SECRET=other expect "5 someone else's secret: stop" 'Stopped: the secret sh-test/app already exists' 'to = '
FAKE_KMS=other expect "6 someone else's KMS key behind the alias: stop" 'Stopped: the KMS key behind alias/eks/c1 already exists' 'to = '
exit $fail
