#!/usr/bin/env bash
# Bring up a kind cluster, deploy fileblock-csi against a backing store, and
# run the Go-based end-to-end tests under test/e2e.
#
# This is the only test surface that exercises the kubelet stage->publish flow,
# the resizer sidecar, and a real cross-node flock handoff. csi-sanity covers
# the spec contract; smoke.sh covers the binaries directly. e2e.sh covers what
# only kubelet can: pod-level chmod, flock, expand, and node-to-node takeover.
#
# Backing-store modes (BACKING_KIND env var):
#   local  (default)  Plain host directory bind-mounted into both kind nodes.
#   nfs               Stand up nfs-kernel-server on the host, export a dir
#                     over NFSv3 (with NLM), mount it locally, and bind-mount
#                     the mount point into the kind nodes. The .img files
#                     then actually live on NFSv3, so the suite validates
#                     that fileblock fixes the NFSv3 exec-bit / chmod / flock
#                     pathologies the README calls out.
#
# Requires: docker, kind, kubectl, go. The 'nfs' mode additionally requires
# sudo and nfs-kernel-server (apt-installed on demand on Debian/Ubuntu).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CLUSTER="${CLUSTER:-fileblock-e2e}"
WORK="${WORK:-/tmp/fileblock-e2e}"
IMAGE="${IMAGE:-fileblock-csi:e2e}"
KEEP="${KEEP:-0}"
BACKING_KIND="${BACKING_KIND:-local}"
SUDO="${SUDO:-sudo}"

BACKING_HOST="$WORK/backing"
NFS_EXPORT="$WORK/export"

log() { printf '::: %s\n' "$*"; }

require() {
  command -v "$1" >/dev/null || { echo "missing required binary: $1" >&2; exit 1; }
}
require docker
require kind
require kubectl
require go

prepare_backing_local() {
  rm -rf "$WORK"
  mkdir -p "$BACKING_HOST"
}

prepare_backing_nfs() {
  require findmnt
  rm -rf "$WORK"
  mkdir -p "$NFS_EXPORT" "$BACKING_HOST"

  if ! command -v exportfs >/dev/null; then
    log "installing nfs-kernel-server (one-shot)"
    $SUDO apt-get update -qq
    $SUDO DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
      nfs-kernel-server nfs-common
  fi

  log "exporting $NFS_EXPORT over NFSv3"
  # rw,sync: real write semantics. no_root_squash: kind nodes run as root, and
  # the .img files need to be created+owned by them. insecure: NFSv3 client
  # ports are non-privileged inside containers.
  EXPORT_LINE="$NFS_EXPORT *(rw,sync,no_subtree_check,no_root_squash,insecure)"
  $SUDO sh -c "grep -qF \"$EXPORT_LINE\" /etc/exports || echo \"$EXPORT_LINE\" >> /etc/exports"

  # Make sure rpcbind, statd and the kernel server are up. systemd vs sysv
  # vs github-runner-style init varies; try the well-known service names in
  # order and accept the first one that starts cleanly.
  for svc in rpcbind nfs-server nfs-kernel-server; do
    $SUDO systemctl restart "$svc" >/dev/null 2>&1 \
      || $SUDO service "$svc" restart >/dev/null 2>&1 \
      || true
  done
  $SUDO exportfs -ra

  log "mounting export at $BACKING_HOST as NFSv3"
  $SUDO mount -t nfs -o vers=3,lock,hard,nolock=0 \
    127.0.0.1:"$NFS_EXPORT" "$BACKING_HOST"
  $SUDO chmod 0777 "$BACKING_HOST"

  if ! findmnt -no FSTYPE "$BACKING_HOST" | grep -q '^nfs'; then
    echo "expected $BACKING_HOST to be mounted as nfs, got: $(findmnt -no FSTYPE "$BACKING_HOST")" >&2
    exit 1
  fi
  log "backing store is $(findmnt -no FSTYPE "$BACKING_HOST") (vers=3)"
}

cleanup() {
  set +e
  if [[ "$KEEP" != "1" ]]; then
    log "deleting kind cluster $CLUSTER"
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
    if [[ "$BACKING_KIND" == "nfs" ]]; then
      $SUDO umount "$BACKING_HOST" 2>/dev/null
      $SUDO sed -i "\|^$NFS_EXPORT |d" /etc/exports 2>/dev/null
      $SUDO exportfs -ra 2>/dev/null
    fi
    rm -rf "$WORK" 2>/dev/null
  else
    log "KEEP=1: leaving cluster $CLUSTER and $WORK in place"
  fi
}
trap cleanup EXIT

case "$BACKING_KIND" in
  local) log "backing-store kind: local directory"; prepare_backing_local ;;
  nfs)   log "backing-store kind: NFSv3";            prepare_backing_nfs   ;;
  *)     echo "unknown BACKING_KIND=$BACKING_KIND (want: local|nfs)" >&2; exit 1 ;;
esac

KIND_CFG="$WORK/kind.yaml"
sed "s|__E2E_BACKING_HOST__|$BACKING_HOST|g" "$ROOT/hack/e2e-kind.yaml" >"$KIND_CFG"

log "creating kind cluster $CLUSTER"
kind create cluster --name "$CLUSTER" --config "$KIND_CFG" --wait 120s

log "building image $IMAGE"
( cd "$ROOT" && docker build -t "$IMAGE" . )

log "loading $IMAGE into kind"
kind load docker-image "$IMAGE" --name "$CLUSTER"

log "applying e2e overlay"
kubectl apply -k "$ROOT/deploy/kustomize/overlays/e2e"

log "waiting for controller + node DaemonSet to be ready"
kubectl -n fileblock-system rollout status deploy/fileblock-controller --timeout=180s
kubectl -n fileblock-system rollout status ds/fileblock-node --timeout=180s

log "running go e2e tests (backing=$BACKING_KIND)"
export E2E_KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
export E2E_BACKING_HOST="$BACKING_HOST"
export E2E_BACKING_KIND="$BACKING_KIND"
( cd "$ROOT" && go test -tags=e2e -timeout 30m -v ./test/e2e/... )

log "e2e OK"
