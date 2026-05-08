#!/usr/bin/env bash
# Local end-to-end smoke test. No kubernetes, no kind, no NFS.
# Runs the controller and node binaries against a plain temp directory and
# drives them with `csc` (the kubernetes-csi CSI client CLI).
#
# Prereqs (Linux): go, losetup, mkfs.ext4, e2fsck, resize2fs, mount, umount,
# findmnt, and `csc` (https://github.com/rexray/gocsi/tree/master/csc).
# Run as root (loop devices and mount(8) require it).
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "must run as root (loop devices + mount(8))" >&2
  exit 1
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="${WORK:-/tmp/fileblock-smoke}"
BACKING="$WORK/backing"
STATE="$WORK/state"
BIN="$WORK/bin"
CTL_SOCK="$WORK/ctl.sock"
NODE_SOCK="$WORK/node.sock"
LOG="$WORK/log"

cleanup() {
  set +e
  if [[ -d "$STATE/staging" ]]; then
    for d in "$STATE"/staging/*; do
      [[ -d "$d" ]] && umount "$d" 2>/dev/null
    done
  fi
  losetup --json --list 2>/dev/null \
    | grep -oE '"/dev/loop[0-9]+"' \
    | tr -d '"' \
    | while read -r dev; do
        back=$(losetup --noheadings --output BACK-FILE "$dev" 2>/dev/null || true)
        case "$back" in "$BACKING"/*) losetup --detach "$dev" ;; esac
      done
  if [[ -n "${CTL_PID-}" ]]; then kill "$CTL_PID" 2>/dev/null || true; fi
  if [[ -n "${NODE_PID-}" ]]; then kill "$NODE_PID" 2>/dev/null || true; fi
  wait 2>/dev/null || true
}
trap cleanup EXIT

rm -rf "$WORK"
mkdir -p "$BACKING" "$STATE" "$BIN" "$LOG"

echo "::: building binaries"
( cd "$ROOT" && go build -o "$BIN/fileblock-controller" ./cmd/controller )
( cd "$ROOT" && go build -o "$BIN/fileblock-node" ./cmd/node )

echo "::: starting controller"
"$BIN/fileblock-controller" \
  --endpoint="unix://$CTL_SOCK" \
  --backing-store="$BACKING" \
  --log-level=debug >"$LOG/controller.log" 2>&1 &
CTL_PID=$!

echo "::: starting node"
"$BIN/fileblock-node" \
  --endpoint="unix://$NODE_SOCK" \
  --node-id=local \
  --state-dir="$STATE" \
  --backing-store="$BACKING" \
  --log-level=debug >"$LOG/node.log" 2>&1 &
NODE_PID=$!

# Wait for sockets.
for _ in $(seq 1 20); do
  [[ -S "$CTL_SOCK" && -S "$NODE_SOCK" ]] && break
  sleep 0.1
done
[[ -S "$CTL_SOCK" && -S "$NODE_SOCK" ]] || { echo "sockets never appeared"; exit 1; }

export CSI_ENDPOINT="unix://$CTL_SOCK"

echo "::: identity probe (controller)"
csc identity probe

echo "::: create volume"
CREATE_OUT=$(csc controller new \
  --cap "SINGLE_NODE_WRITER,mount,ext4" \
  --req-bytes $((128*1024*1024)) \
  --params "backingStorePath=$BACKING" \
  smoke-vol)
VOL_ID=$(printf '%s\n' "$CREATE_OUT" | head -n1 | awk '{print $1}' | tr -d '"')
echo "  created volumeID=$VOL_ID"
[[ -f "$BACKING/$VOL_ID.img"  ]] || { echo "missing .img"; exit 1; }

echo "::: stage on node"
STAGE="$STATE/staging/$VOL_ID"
mkdir -p "$STAGE"
CSI_ENDPOINT="unix://$NODE_SOCK" csc node stage \
  --cap "SINGLE_NODE_WRITER,mount,ext4" \
  --staging-target-path "$STAGE" \
  --vol-context "backingStorePath=$BACKING" \
  "$VOL_ID"

mount | grep -q " on $STAGE " || { echo "stage path not mounted"; exit 1; }
findmnt -no FSTYPE "$STAGE" | grep -q '^ext4$' || { echo "fs not ext4"; exit 1; }

echo "::: chmod +x survives unstage/stage"
echo '#!/bin/sh' >"$STAGE/x.sh"; chmod +x "$STAGE/x.sh"
[[ -x "$STAGE/x.sh" ]] || { echo "+x didn't take"; exit 1; }
CSI_ENDPOINT="unix://$NODE_SOCK" csc node unstage --staging-target-path "$STAGE" "$VOL_ID"
CSI_ENDPOINT="unix://$NODE_SOCK" csc node stage \
  --cap "SINGLE_NODE_WRITER,mount,ext4" \
  --staging-target-path "$STAGE" \
  --vol-context "backingStorePath=$BACKING" \
  "$VOL_ID"
[[ -x "$STAGE/x.sh" ]] || { echo "+x lost across remount"; exit 1; }

echo "::: git fileMode survives"
( cd "$STAGE" && git init -q && touch a.sh && chmod +x a.sh \
  && git add . && git -c user.email=t@t -c user.name=t commit -q -m a \
  && git status --porcelain | grep -q . && exit 1 || true )

echo "::: unstage + delete"
CSI_ENDPOINT="unix://$NODE_SOCK" csc node unstage --staging-target-path "$STAGE" "$VOL_ID"
csc controller del "$VOL_ID"
[[ ! -f "$BACKING/$VOL_ID.img" ]]  || { echo ".img still present"; exit 1; }

echo "::: orphan loop is reclaimed on plugin restart"
CREATE_OUT=$(csc controller new \
  --cap "SINGLE_NODE_WRITER,mount,ext4" \
  --req-bytes $((128*1024*1024)) \
  --params "backingStorePath=$BACKING" \
  orphan-vol)
VOL2=$(printf '%s\n' "$CREATE_OUT" | head -n1 | awk '{print $1}' | tr -d '"')
ORPHAN=$(losetup --find --show "$BACKING/$VOL2.img")
kill "$NODE_PID"; wait "$NODE_PID" 2>/dev/null || true
"$BIN/fileblock-node" \
  --endpoint="unix://$NODE_SOCK" \
  --node-id=local \
  --state-dir="$STATE" \
  --backing-store="$BACKING" \
  --log-level=debug >>"$LOG/node.log" 2>&1 &
NODE_PID=$!
sleep 1
losetup "$ORPHAN" 2>/dev/null && { echo "orphan loop not reclaimed"; exit 1; } || true
csc controller del "$VOL2"

echo "::: cross-node takeover (shared backing store)"
# Simulates a pod rescheduling onto a different node when the .img lives on
# a filesystem both nodes mount. Cross-node mutual exclusion is the kubelet's
# job (SINGLE_NODE_WRITER); fileblock just has to make sure that once node-a
# has unstaged, node-b can stage and read the data node-a wrote.
CREATE_OUT=$(csc controller new \
  --cap "SINGLE_NODE_WRITER,mount,ext4" \
  --req-bytes $((128*1024*1024)) \
  --params "backingStorePath=$BACKING" \
  takeover-vol)
VOL3=$(printf '%s\n' "$CREATE_OUT" | head -n1 | awk '{print $1}' | tr -d '"')
STAGE_A="$STATE/staging/${VOL3}-a"
STAGE_B="$STATE/staging/${VOL3}-b"
mkdir -p "$STAGE_A" "$STAGE_B"

kill "$NODE_PID"; wait "$NODE_PID" 2>/dev/null || true
"$BIN/fileblock-node" \
  --endpoint="unix://$NODE_SOCK" \
  --node-id=node-a \
  --state-dir="$STATE" \
  --backing-store="$BACKING" \
  --log-level=debug >>"$LOG/node.log" 2>&1 &
NODE_PID=$!
for _ in $(seq 1 20); do [[ -S "$NODE_SOCK" ]] && break; sleep 0.1; done
CSI_ENDPOINT="unix://$NODE_SOCK" csc node stage \
  --cap "SINGLE_NODE_WRITER,mount,ext4" \
  --staging-target-path "$STAGE_A" \
  --vol-context "backingStorePath=$BACKING" \
  "$VOL3"
echo node-a-was-here > "$STAGE_A/who"
CSI_ENDPOINT="unix://$NODE_SOCK" csc node unstage --staging-target-path "$STAGE_A" "$VOL3"

# node-a "crashes" before unstage in production; here we already unstaged
# cleanly because that's what kubelet would do before letting another node
# take the volume. Restart as node-b and stage the same image.
kill "$NODE_PID"; wait "$NODE_PID" 2>/dev/null || true
"$BIN/fileblock-node" \
  --endpoint="unix://$NODE_SOCK" \
  --node-id=node-b \
  --state-dir="$STATE" \
  --backing-store="$BACKING" \
  --log-level=debug >>"$LOG/node.log" 2>&1 &
NODE_PID=$!
for _ in $(seq 1 20); do [[ -S "$NODE_SOCK" ]] && break; sleep 0.1; done
CSI_ENDPOINT="unix://$NODE_SOCK" csc node stage \
  --cap "SINGLE_NODE_WRITER,mount,ext4" \
  --staging-target-path "$STAGE_B" \
  --vol-context "backingStorePath=$BACKING" \
  "$VOL3"
grep -q '^node-a-was-here$' "$STAGE_B/who" || {
  echo "data written by node-a not visible to node-b"; exit 1; }

CSI_ENDPOINT="unix://$NODE_SOCK" csc node unstage --staging-target-path "$STAGE_B" "$VOL3"
csc controller del "$VOL3"

echo
echo "smoke OK"
