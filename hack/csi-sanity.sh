#!/usr/bin/env bash
# Drive csi-sanity (https://github.com/kubernetes-csi/csi-test) against the
# controller and node binaries running on local unix sockets. No cluster.
#
# Requires: go, csi-sanity, and the same OS tools as smoke.sh. `mise install`
#           from the repo root provides go and csi-sanity. Run as root —
#           `make sanity` forwards PATH through sudo so mise-provided tools
#           stay reachable.
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "must run as root (loop devices + mount(8))" >&2
  exit 1
fi
if ! command -v csi-sanity >/dev/null; then
  echo "csi-sanity not on PATH; run \`mise install\` from the repo root" >&2
  exit 1
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="${WORK:-/tmp/fileblock-sanity}"
STORES="$WORK/stores"
BACKING="$STORES/local"
STATE="$WORK/state"
BIN="$WORK/bin"
CTL_SOCK="$WORK/ctl.sock"
NODE_SOCK="$WORK/node.sock"

cleanup() {
  set +e
  for m in /dev/mapper/fbcrypt-*; do
    [[ -e "$m" ]] || continue
    dev=$(cryptsetup status "$(basename "$m")" 2>/dev/null | awk '/device:/ {print $2}')
    back=$(losetup --noheadings --output BACK-FILE "$dev" 2>/dev/null || true)
    case "$back" in "$STORES"/*) DM_DISABLE_UDEV=1 cryptsetup close "$(basename "$m")" ;; esac
  done
  losetup --json --list 2>/dev/null \
    | grep -oE '"/dev/loop[0-9]+"' \
    | tr -d '"' \
    | while read -r dev; do
        back=$(losetup --noheadings --output BACK-FILE "$dev" 2>/dev/null || true)
        case "$back" in "$STORES"/*) losetup --detach "$dev" ;; esac
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
  --endpoint="unix://$CTL_SOCK" --stores-root="$STORES" --log-level=debug &
CTL_PID=$!
"$BIN/fileblock-node" \
  --endpoint="unix://$NODE_SOCK" --node-id=local --state-dir="$STATE" \
  --stores-root="$STORES" --log-level=debug &
NODE_PID=$!

for _ in $(seq 1 20); do
  [[ -S "$CTL_SOCK" && -S "$NODE_SOCK" ]] && break
  sleep 0.1
done

csi-sanity \
  --csi.controllerendpoint="unix://$CTL_SOCK" \
  --csi.endpoint="unix://$NODE_SOCK" \
  --csi.testvolumeparameters=<(printf "backingStore.type: local\nbackingStore.local.path: %s\n" "$BACKING") \
  --csi.testvolumesize=$((128*1024*1024))

echo "::: csi-sanity (encrypted)"
csi-sanity \
  --csi.controllerendpoint="unix://$CTL_SOCK" \
  --csi.endpoint="unix://$NODE_SOCK" \
  --csi.testvolumeparameters=<(printf "backingStore.type: local\nbackingStore.local.path: %s\nencrypted: \"true\"\n" "$BACKING") \
  --csi.secrets=<(printf "NodeStageVolumeSecret:\n  key: %s\n" "$(openssl rand -hex 32)") \
  --csi.testvolumesize=$((128*1024*1024))
