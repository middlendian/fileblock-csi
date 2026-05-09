#!/usr/bin/env bash
# Bring up a kind cluster, deploy fileblock-csi against a backing store, and
# run the Go-based end-to-end tests under test/e2e.
#
# This is the only test surface that exercises the kubelet stage->publish flow,
# the resizer sidecar, and a kubelet-mediated cross-node handoff. csi-sanity
# covers the spec contract; smoke.sh covers the binaries directly. e2e.sh
# covers what only kubelet can: pod-level chmod, flock, expand, and the
# node-to-node takeover that CSI's SINGLE_NODE_WRITER serializes.
#
# Backing-store modes (BACKING_KIND env var):
#   local  (default)  Plain host directory bind-mounted into both kind nodes.
#   nfs               Stand up nfs-kernel-server on the host, export a dir
#                     over NFSv3, mount it locally, and bind-mount the mount
#                     point into the kind nodes. The .img files then actually
#                     live on NFSv3, so the suite validates that fileblock
#                     fixes the NFSv3 exec-bit / chmod / in-pod flock
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
NFS_VERSION="${NFS_VERSION:-4.1}"
export NFS_VERSION
# NFS_SERVER: the address of the NFS server as seen from inside a kind node.
# Kind nodes run as Docker containers, so 127.0.0.1 (the host loopback) is not
# reachable from inside them. Default to the Docker bridge gateway, which is
# the host from the container's perspective. Falls back to host.docker.internal
# for environments (e.g. Docker Desktop on macOS) where the bridge may not exist.
if [ -z "${NFS_SERVER:-}" ]; then
  NFS_SERVER=$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null || echo "host.docker.internal")
fi
export NFS_SERVER
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
  # Create per-store subdirectories for TestTwoStores
  mkdir -p "$WORK/a" "$WORK/b"
}

prepare_backing_nfs() {
  require findmnt
  rm -rf "$WORK"
  mkdir -p "$NFS_EXPORT" "$BACKING_HOST"
  # Create per-store subdirectories for TestTwoStores
  mkdir -p "$WORK/a" "$WORK/b"

  if ! command -v exportfs >/dev/null; then
    log "installing nfs-kernel-server (one-shot)"
    $SUDO apt-get update -qq
    $SUDO DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
      nfs-kernel-server nfs-common
  fi

  # Kernel modules: nfsd serves NFSv3, lockd backs NLM. On stock GitHub
  # runners these aren't loaded by default, and nfs-kernel-server will
  # silently fail to start without them.
  log "loading nfsd / lockd kernel modules"
  $SUDO modprobe nfsd
  $SUDO modprobe lockd
  # /proc/fs/nfsd is the kernel server's control interface; nfs-kernel-server
  # mounts it on start, but on minimal init systems we may need to do it
  # ourselves before rpc.nfsd will accept exports.
  if ! mountpoint -q /proc/fs/nfsd; then
    $SUDO mount -t nfsd nfsd /proc/fs/nfsd
  fi

  log "exporting $NFS_EXPORT over NFSv3"
  # rw,sync: real write semantics. no_root_squash: kind nodes run as root, and
  # the .img files need to be created+owned by them. insecure: NFSv3 client
  # ports are non-privileged inside containers.
  EXPORT_LINE="$NFS_EXPORT *(rw,sync,no_subtree_check,no_root_squash,insecure)"
  $SUDO sh -c "grep -qF \"$EXPORT_LINE\" /etc/exports || echo \"$EXPORT_LINE\" >> /etc/exports"

  # Bring up rpcbind first (NFSv3 needs the portmapper) and then the kernel
  # NFS server. Try systemd then sysv-init then a direct daemon invocation
  # so this works on any of the init flavors GitHub runners ship.
  start_svc() {
    local svc="$1"
    $SUDO systemctl restart "$svc" 2>/dev/null && return 0
    $SUDO service "$svc" restart 2>/dev/null && return 0
    return 1
  }
  start_svc rpcbind || $SUDO rpcbind || true
  start_svc nfs-server || start_svc nfs-kernel-server || $SUDO rpc.nfsd 8 || true
  start_svc nfs-common 2>/dev/null || true
  $SUDO rpc.statd 2>/dev/null || true
  $SUDO exportfs -ra

  # Confirm rpcbind sees nfs+nlockmgr before mounting; fail fast with a clear
  # message if the server never came up. Better than waiting for mount(8) to
  # exit 32 with no context.
  for _ in $(seq 1 20); do
    if rpcinfo -p 127.0.0.1 2>/dev/null | grep -qE '\bnfs\b'; then
      break
    fi
    sleep 0.5
  done
  if ! rpcinfo -p 127.0.0.1 2>/dev/null | grep -qE '\bnfs\b'; then
    echo "nfs server never registered with rpcbind; rpcinfo:" >&2
    rpcinfo -p 127.0.0.1 2>&1 >&2 || true
    echo "---- /var/log/syslog (tail) ----" >&2
    $SUDO tail -n 50 /var/log/syslog 2>/dev/null >&2 || true
    exit 1
  fi

  log "mounting export at $BACKING_HOST as NFSv${NFS_VERSION}"
  $SUDO mount -t nfs -o "vers=${NFS_VERSION},lock,hard" \
    127.0.0.1:"$NFS_EXPORT" "$BACKING_HOST"
  $SUDO chmod 0777 "$BACKING_HOST"

  if ! findmnt -no FSTYPE "$BACKING_HOST" | grep -q '^nfs'; then
    echo "expected $BACKING_HOST to be mounted as nfs, got: $(findmnt -no FSTYPE "$BACKING_HOST")" >&2
    exit 1
  fi
  log "backing store is $(findmnt -no FSTYPE "$BACKING_HOST") (vers=${NFS_VERSION})"
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

# Loop subsystem must be available on the host; kind nodes share /dev with
# the runner via the kind config's extraMounts, so /dev/loopN here is what
# the node DaemonSet sees.
$SUDO modprobe loop 2>/dev/null || true

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
# For NFS backing-store mode, replace the local SC with a driver-NFS SC so the
# driver mounts the export itself (in addition to the host NFSv3 mount used by
# the backing-store path). This validates the driver's NFS mounter code path.
if [[ "$BACKING_KIND" == "nfs" ]]; then
  log "applying NFS StorageClass (nfsvers=${NFS_VERSION})"
  # The e2e overlay already created a local-type SC named "fileblock".
  # StorageClass parameters are immutable, so delete it before applying the
  # NFS variant (which has different parameters).
  kubectl delete sc fileblock --ignore-not-found --wait=false
  envsubst < "$ROOT/deploy/kustomize/overlays/e2e/storageclass-nfs.yaml.tpl" \
    | kubectl apply -f -
fi

# Inline diagnostics on rollout failure: when something doesn't come up,
# print the full pod and event state immediately so the CI log alone is
# enough to diagnose. Falling through to the workflow's failure-only dump
# step works too, but this surfaces the same info inline for local runs.
dump_state() {
  echo "==== nodes ===="; kubectl get nodes -o wide
  echo "==== fileblock-system pods ===="; kubectl -n fileblock-system get pods -o wide
  echo "==== fileblock-system describe ===="; kubectl -n fileblock-system describe pods
  echo "==== controller logs ===="; kubectl -n fileblock-system logs deploy/fileblock-controller --all-containers --tail=200 2>&1 || true
  echo "==== node logs ===="; kubectl -n fileblock-system logs ds/fileblock-node --all-containers --tail=200 2>&1 || true
  echo "==== events ===="; kubectl get events -A --sort-by=.lastTimestamp | tail -100
}

log "waiting for controller + node DaemonSet to be ready"
if ! kubectl -n fileblock-system rollout status deploy/fileblock-controller --timeout=180s; then
  dump_state; exit 1
fi
if ! kubectl -n fileblock-system rollout status ds/fileblock-node --timeout=180s; then
  dump_state; exit 1
fi

log "running go e2e tests (backing=$BACKING_KIND)"
export E2E_KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
export E2E_BACKING_HOST="$BACKING_HOST"
export E2E_BACKING_KIND="$BACKING_KIND"
if ! ( cd "$ROOT" && go test -tags=e2e -timeout 30m -v ./test/e2e/... ); then
  dump_state; exit 1
fi

log "e2e OK"
