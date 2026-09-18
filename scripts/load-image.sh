#!/usr/bin/env bash
# Puts a locally built image onto the cluster node so imagePullPolicy: Never
# finds it. Docker Desktop's Kubernetes runs its node inside the VM with its
# own containerd, so `docker build` alone is not enough; minikube has
# `minikube image load` for the same reason. The Docker Desktop path uses a
# node debug pod, which can see the node's filesystem and its ctr binary.
#
#   scripts/load-image.sh <image:tag> [kubectl-context]
set -euo pipefail
IMAGE="${1:?image:tag}"
CONTEXT="${2:-docker-desktop}"

if [ "$CONTEXT" = "minikube" ]; then
  minikube image load "$IMAGE"
  exit 0
fi

NODE="$(kubectl --context "$CONTEXT" get nodes -o jsonpath='{.items[0].metadata.name}')"
TAR="$(mktemp -t simple-host-image.XXXXXX).tar"
trap 'rm -f "$TAR"' EXIT
docker save "$IMAGE" -o "$TAR"

kubectl --context "$CONTEXT" -n default debug "node/$NODE" --image=alpine:3.20 --profile=sysadmin -- sleep 900 >/dev/null
POD=""
for _ in $(seq 1 30); do
  POD="$(kubectl --context "$CONTEXT" -n default get pods -o name | grep "node-debugger-$NODE" | head -1 | cut -d/ -f2)"
  [ -n "$POD" ] && break
  sleep 1
done
[ -n "$POD" ] || { echo "debug pod did not appear" >&2; exit 1; }
kubectl --context "$CONTEXT" -n default wait --for=condition=Ready "pod/$POD" --timeout=120s >/dev/null
kubectl --context "$CONTEXT" -n default cp "$TAR" "default/$POD:/host/tmp/image.tar"
kubectl --context "$CONTEXT" -n default exec "$POD" -- chroot /host ctr -n k8s.io images import /tmp/image.tar
kubectl --context "$CONTEXT" -n default exec "$POD" -- rm -f /host/tmp/image.tar
kubectl --context "$CONTEXT" -n default delete pod "$POD" --wait=false >/dev/null
echo "loaded $IMAGE onto $NODE"
