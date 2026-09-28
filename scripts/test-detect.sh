#!/usr/bin/env bash
# Runs the existing-cluster detection block of deploy/terraform/aws/apply.sh
# (between "# <detect>" and "# </detect>") against a throwaway kind cluster,
# in the situations a customer's cluster can be in:
#   1. nothing installed                          -> install both
#   2. cert-manager from static manifests running -> use it (its namespace, SA)
#   3. only cert-manager's CRDs left, not ours    -> stop with the delete command
#   4. External Secrets under another release     -> use it
#   5. External Secrets CRDs left, no controller (the other release's deployments gone) -> stop
#   6. cert-manager older than 1.14 running        -> stop, asking for an upgrade
# Needs docker, kind, kubectl and helm. Usage: bash scripts/test-detect.sh
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
cluster=sh-detect-$$
kc=$(mktemp)
trap 'kind delete cluster --name "$cluster" >/dev/null 2>&1; rm -f "$kc"' EXIT
kind create cluster --name "$cluster" --kubeconfig "$kc" --wait 120s >/dev/null
block=$(sed -n '/# <detect>/,/# <\/detect>/p' "$here/deploy/terraform/aws/apply.sh")

detect() {
  # The block's context: k(), die(), $detected; it runs in a subshell.
  ( set -euo pipefail
    k() { kubectl --kubeconfig "$kc" "$@"; }
    die() { printf 'Stopped: %s\n' "$*"; exit 1; }
    detected=$(mktemp)
    eval "$block"
    cat "$detected"; rm -f "$detected" ) 2>&1 || true
}
fail=0
expect() {
  local name=$1 want=$2 got
  got=$(detect)
  if grep -qE "$want" <<<"$got"; then echo "ok   $name"; else echo "FAIL $name: wanted /$want/, got:"; sed 's/^/     /' <<<"$got"; fail=1; fi
}
k() { kubectl --kubeconfig "$kc" "$@"; }

expect "1 nothing installed: install both" '^$'

k apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.21.2/cert-manager.yaml >/dev/null
k -n cert-manager rollout status deploy/cert-manager --timeout=180s >/dev/null
expect "2 cert-manager from static manifests: use it" 'install_cert_manager = false'

k -n cert-manager scale deploy/cert-manager --replicas=0 >/dev/null
k -n cert-manager delete deploy cert-manager >/dev/null
expect "3 cert-manager CRDs left, not ours: stop with the delete command" 'Stopped: .*installed by hand.*kubectl delete customresourcedefinition'
k delete -f https://github.com/cert-manager/cert-manager/releases/download/v1.21.2/cert-manager.yaml --ignore-not-found >/dev/null 2>&1 || true

helm --kubeconfig "$kc" install eso external-secrets --repo https://charts.external-secrets.io --version 2.11.0 -n eso --create-namespace --wait >/dev/null
expect "4 External Secrets under another release: use it" 'install_external_secrets = false'

k -n eso delete deploy --all >/dev/null
expect "5 External Secrets CRDs left, no controller: stop with the delete command" 'Stopped: .*installed by eso.*kubectl delete customresourcedefinition'
k get crd -o name | grep external-secrets.io | xargs -r kubectl --kubeconfig "$kc" delete >/dev/null

k apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.13.6/cert-manager.yaml >/dev/null
k -n cert-manager rollout status deploy/cert-manager --timeout=180s >/dev/null
expect "6 cert-manager 1.13: stop, asking for an upgrade" 'Stopped: .*older than 1.14'

exit $fail
