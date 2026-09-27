#!/usr/bin/env bash
# Run one simple-host subcommand as a one-off Job, from the prune CronJob's
# pod template: the same image, config and the database's owning
# credential, which the server's own container does not hold (so
# `kubectl exec deploy/simple-host -- /simple-host audit-verify` cannot
# work there, by design). Follows the Job's output, deletes the Job, and
# exits with the command's status.
#
#   make job ARGS="audit-verify"
#   make job ARGS="migrate -status" OVERLAY=deploy/overlays/drill
#   CONTEXT=<ctx> NAMESPACE=simple-host ./scripts/run-job.sh prune -dry-run
set -eu

CONTEXT="${CONTEXT:-}"
NAMESPACE="${NAMESPACE:-simple-host}"
[ -n "$CONTEXT" ] || { echo "run-job: set CONTEXT to the cluster's kubectl context" >&2; exit 2; }
[ "$#" -gt 0 ] || { echo "run-job: name the subcommand, e.g. audit-verify or 'migrate -status'" >&2; exit 2; }
command -v python3 >/dev/null || { echo "run-job: missing: python3" >&2; exit 2; }

k=(kubectl --context "$CONTEXT" -n "$NAMESPACE")
job="simple-host-$(printf '%s' "$1" | tr -c 'a-z0-9-' '-')-$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')"

# The pod template is immutable once a Job exists, so the args are set in
# the rendered JSON before it is created.
"${k[@]}" create job "$job" --from=cronjob/simple-host-prune --dry-run=client -o json \
  | python3 -c 'import json, sys
j = json.load(sys.stdin)
j["spec"]["backoffLimit"] = 0
j["spec"]["ttlSecondsAfterFinished"] = 3600
t = j["spec"]["template"]["spec"]
t["restartPolicy"] = "Never"
t["containers"][0]["args"] = sys.argv[1:]
print(json.dumps(j))' "$@" \
  | "${k[@]}" create -f - >/dev/null
trap '"${k[@]}" delete job "$job" --wait=false >/dev/null 2>&1 || true' EXIT

"${k[@]}" logs -f --pod-running-timeout=3m "job/$job" || true
for _ in $(seq 1 60); do
  s="$("${k[@]}" get job "$job" -o jsonpath='{.status.succeeded}/{.status.failed}' 2>/dev/null || true)"
  case "$s" in
    1/*) exit 0 ;;
    */1) echo "run-job: $* failed" >&2; exit 1 ;;
  esac
  sleep 2
done
trap - EXIT
echo "run-job: $* did not finish; see: kubectl --context $CONTEXT -n $NAMESPACE describe job $job" >&2
exit 1
