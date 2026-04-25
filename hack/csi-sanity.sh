#!/usr/bin/env bash
# Drive csi-sanity (https://github.com/kubernetes-csi/csi-test) against the
# controller and node binaries running on local unix sockets. No cluster.
#
# Requires: go, csi-sanity (`go install github.com/kubernetes-csi/csi-test/v5/cmd/csi-sanity@latest`),
#           and the same OS tools as smoke.sh. Run as root.
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "must run as root (loop devices + mount(8))" >&2
  exit 1
fi
if ! command -v csi-sanity >/dev/null; then
  echo "csi-sanity not on PATH; go install github.com/kubernetes-csi/csi-test/v5/cmd/csi-sanity@latest" >&2
  exit 1
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="${WORK:-/tmp/fileblock-sanity}"
BACKING="$WORK/backing"
STATE="$WORK/state"
BIN="$WORK/bin"
CTL_SOCK="$WORK/ctl.sock"
NODE_SOCK="$WORK/node.sock"

cleanup() {
  set +e
  losetup --json --list 2>/dev/null \
    | grep -oE '"/dev/loop[0-9]+"' \
    | tr -d '"' \
    | while read -r dev; do
        back=$(losetup --noheadings --output BACK-FILE "$dev" 2>/dev/null || true)
        case "$back" in "$BACKING"/*) losetup --detach "$dev" ;; esac
      done
  [[ -n "${CTL_PID-}" ]]  && kill "$CTL_PID"  2>/dev/null
  [[ -n "${NODE_PID-}" ]] && kill "$NODE_PID" 2>/dev/null
  wait 2>/dev/null || true
}
trap cleanup EXIT

rm -rf "$WORK"
mkdir -p "$BACKING" "$STATE" "$BIN"

( cd "$ROOT" && go build -o "$BIN/fileblock-controller" ./cmd/controller )
( cd "$ROOT" && go build -o "$BIN/fileblock-node" ./cmd/node )

"$BIN/fileblock-controller" \
  --endpoint="unix://$CTL_SOCK" --backing-store="$BACKING" --log-level=debug &
CTL_PID=$!
"$BIN/fileblock-node" \
  --endpoint="unix://$NODE_SOCK" --node-id=local --state-dir="$STATE" \
  --backing-store="$BACKING" --log-level=debug &
NODE_PID=$!

for _ in $(seq 1 20); do
  [[ -S "$CTL_SOCK" && -S "$NODE_SOCK" ]] && break
  sleep 0.1
done

csi-sanity \
  --csi.controllerendpoint="unix://$CTL_SOCK" \
  --csi.endpoint="unix://$NODE_SOCK" \
  --csi.testvolumeparameters=<(printf "backingStorePath: %s\n" "$BACKING") \
  --csi.testvolumesize=$((128*1024*1024))
