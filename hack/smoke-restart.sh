#!/usr/bin/env bash
# Regression test: replacing the node plugin (a DaemonSet rollout, or any
# container restart) must not leak loop devices or attach a second one.
#
# Mirrors the node DaemonSet's mount layout without kubernetes: the
# staging area sits under a shared mount (the Bidirectional kubelet dir),
# and the stores root is private to the plugin's mount namespace (the
# emptyDir). The plugin runs under `unshare --mount`, so killing it
# destroys that namespace the way a pod replacement does, and the loops it
# attached then report their back-file relative to the store mount.
#
# Prereqs: the same as hack/smoke.sh, plus unshare(1). Run as root —
# `make smoke-restart` forwards PATH through sudo.
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "must run as root (loop devices, mount(8), unshare --mount)" >&2
  exit 1
fi
command -v unshare >/dev/null || { echo "unshare(1) not found" >&2; exit 1; }

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="${WORK:-/tmp/fileblock-smoke-restart}"
BACKING="$WORK/backing"
CTL_STORES="$WORK/ctl-stores"
STORES="$WORK/stores"
KUBELET="$WORK/kubelet"
STATE="$KUBELET/plugins/fileblock.csi"
BIN="$WORK/bin"
LOG="$WORK/log"
CTL_SOCK="$WORK/ctl.sock"
NODE_SOCK="$WORK/node.sock"
CAP="SINGLE_NODE_WRITER,mount,ext4"

cleanup() {
  set +e
  if [[ -n "${NODE_PID-}" ]]; then kill "$NODE_PID" 2>/dev/null; fi
  if [[ -n "${CTL_PID-}" ]]; then kill "$CTL_PID" 2>/dev/null; fi
  wait 2>/dev/null
  if [[ -d "$KUBELET/staging" ]]; then
    for d in "$KUBELET"/staging/*; do
      for _ in 1 2 3 4 5 6 7 8; do mountpoint -q "$d" && umount "$d"; done
    done
  fi
  for img in "$BACKING"/*.img; do
    [[ -e "$img" ]] || continue
    losetup -j "$img" | cut -d: -f1 | while read -r dev; do losetup --detach "$dev"; done
  done
  for d in "$CTL_STORES"/*; do mountpoint -q "$d" && umount "$d"; done
  mountpoint -q "$STORES" && umount -l "$STORES"
  mountpoint -q "$KUBELET" && umount -l "$KUBELET"
}
trap cleanup EXIT

# What the mount and loop tables looked like, for diagnosing a failure.
dump() {
  set +e
  echo "--- / and kubelet-dir propagation (outer namespace)"
  findmnt -n -o TARGET,PROPAGATION,ID,PARENT /
  findmnt -n -o TARGET,PROPAGATION,ID,PARENT "$KUBELET"
  if [[ -n "${STAGE-}" ]]; then
    echo "--- findmnt $STAGE (outer namespace)"
    findmnt -n -o TARGET,SOURCE,FSTYPE,PROPAGATION,ID,PARENT "$STAGE"
  fi
  echo "--- outer mountinfo under $WORK"
  grep -F "$WORK" /proc/self/mountinfo
  if [[ -n "${NODE_PID-}" && -r "/proc/$NODE_PID/mountinfo" ]]; then
    echo "--- node plugin (pid $NODE_PID) mountinfo under $WORK"
    grep -F "$WORK" "/proc/$NODE_PID/mountinfo"
  fi
  echo "--- losetup"
  losetup -l -O NAME,BACK-FILE
  echo "--- node plugin log (tail)"
  tail -n 40 "$LOG"/node*.log
  set -e
}

fail() { echo "FAIL: $*" >&2; dump >&2; exit 1; }
trap 'echo "FAIL: command failed at line $LINENO" >&2; dump >&2' ERR

rm -rf "$WORK"
mkdir -p "$BACKING" "$CTL_STORES" "$STORES" "$KUBELET/staging" "$STATE" "$BIN" "$LOG"

echo "::: building binaries"
( cd "$ROOT" && go build -o "$BIN/fileblock-controller" ./cmd/controller )
( cd "$ROOT" && go build -o "$BIN/fileblock-node" ./cmd/node )

# Shared staging area, private stores root: the DaemonSet's layout.
mount --bind "$KUBELET" "$KUBELET"
mount --make-shared "$KUBELET"
mount --bind "$STORES" "$STORES"
mount --make-private "$STORES"

start_node() {
  rm -f "$NODE_SOCK"
  unshare --mount --propagation unchanged "$BIN/fileblock-node" \
    --endpoint="unix://$NODE_SOCK" \
    --node-id=local \
    --state-dir="$STATE" \
    --stores-root="$STORES" \
    --log-level=debug >>"$LOG/node.log" 2>&1 &
  NODE_PID=$!
  for _ in $(seq 1 50); do [[ -S "$NODE_SOCK" ]] && return; sleep 0.1; done
  fail "node socket never appeared"
}

# Kill the plugin and start a new one in a fresh mount namespace.
replace_node() {
  kill "$NODE_PID"; wait "$NODE_PID" 2>/dev/null || true
  start_node
}

node() { CSI_ENDPOINT="unix://$NODE_SOCK" csc node "$@"; }

stage() {
  node stage --cap "$CAP" --staging-target-path "$1" \
    --vol-context "backingStore.type=local" \
    --vol-context "backingStore.local.path=$BACKING" \
    "$2"
}

loops_on() { losetup -j "$1" | wc -l; }

"$BIN/fileblock-controller" \
  --endpoint="unix://$CTL_SOCK" \
  --stores-root="$CTL_STORES" \
  --log-level=debug >"$LOG/controller.log" 2>&1 &
CTL_PID=$!
for _ in $(seq 1 50); do [[ -S "$CTL_SOCK" ]] && break; sleep 0.1; done
export CSI_ENDPOINT="unix://$CTL_SOCK"

new_volume() {
  csc controller new --cap "$CAP" --req-bytes $((64*1024*1024)) \
    --params "backingStore.type=local" \
    --params "backingStore.local.path=$BACKING" \
    "$1" | head -n1 | awk '{print $1}' | tr -d '"'
}

start_node

echo "::: staged volume survives a plugin replacement, then unstages cleanly"
VOL=$(new_volume restart-vol)
IMG="$BACKING/$VOL.img"
STAGE="$KUBELET/staging/$VOL"
mkdir -p "$STAGE"
stage "$STAGE" "$VOL"
findmnt -n "$STAGE" >/dev/null || fail "stage mount not visible outside the plugin namespace"
[[ $(loops_on "$IMG") -eq 1 ]] || fail "expected one loop after stage"
echo "--- mount table after the first stage"
dump
replace_node
grep -q "\"$VOL\"" "$STATE/loop-mappings.json" || fail "reconciler dropped the staged volume's state entry"
[[ $(loops_on "$IMG") -eq 1 ]] || fail "expected one loop after plugin replacement"

echo "::: re-stage of a still-mounted volume adopts it (no second loop, no stacked mount)"
stage "$STAGE" "$VOL"
[[ $(loops_on "$IMG") -eq 1 ]] || fail "re-stage attached a second loop"
[[ $(findmnt -n "$STAGE" | wc -l) -eq 1 ]] || fail "re-stage stacked a second mount"

echo "::: stateless re-stage of a still-mounted volume adopts it"
# As v0.5.0 left it after a replacement: mounted, attached, no state entry.
kill "$NODE_PID"; wait "$NODE_PID" 2>/dev/null || true
rm -f "$STATE/loop-mappings.json"
start_node
stage "$STAGE" "$VOL"
[[ $(loops_on "$IMG") -eq 1 ]] || fail "stateless re-stage attached a second loop"
[[ $(findmnt -n "$STAGE" | wc -l) -eq 1 ]] || fail "stateless re-stage stacked a second mount"
grep -q "\"$VOL\"" "$STATE/loop-mappings.json" || fail "stateless re-stage did not record the adopted mount"

replace_node
node unstage --staging-target-path "$STAGE" "$VOL"
mountpoint -q "$STAGE" && fail "stage path still mounted after unstage"
[[ $(loops_on "$IMG") -eq 0 ]] || fail "loop leaked after unstage: $(losetup -j "$IMG")"

# The new plugin's reconciler also marks the untracked loop autoclear, so
# this checks the outcome (no leak), not which of the two paths freed it.
echo "::: unstage with the state entry gone (as v0.5.0 left it) still detaches"
stage "$STAGE" "$VOL"
kill "$NODE_PID"; wait "$NODE_PID" 2>/dev/null || true
rm -f "$STATE/loop-mappings.json"
start_node
node unstage --staging-target-path "$STAGE" "$VOL"
[[ $(loops_on "$IMG") -eq 0 ]] || fail "loop leaked after stateless unstage: $(losetup -j "$IMG")"

csc controller del "$VOL"
echo "::: smoke-restart passed"
