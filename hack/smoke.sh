#!/usr/bin/env bash
# Local end-to-end smoke test. No kubernetes, no kind, no NFS.
# Runs the controller and node binaries against a plain temp directory and
# drives them with `csc` (the kubernetes-csi CSI client CLI).
#
# Prereqs (Linux): go, losetup, mkfs.ext4, e2fsck, resize2fs, mount, umount,
# findmnt, cryptsetup, openssl, and `csc`
# (https://github.com/rexray/gocsi/tree/master/csc).
# `mise install` from the repo root provides go and csc; the rest come from
# the OS. Run as root (loop devices and mount(8) require it) — `make smoke`
# forwards PATH through sudo so mise-provided tools stay reachable.
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "must run as root (loop devices + mount(8))" >&2
  exit 1
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="${WORK:-/tmp/fileblock-smoke}"
STORES="$WORK/stores"
BACKING="$STORES/local"
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
( cd "$ROOT" && go build -o "$BIN/csi-call" ./hack/csi-call )

echo "::: starting controller"
"$BIN/fileblock-controller" \
  --endpoint="unix://$CTL_SOCK" \
  --stores-root="$STORES" \
  --log-level=debug >"$LOG/controller.log" 2>&1 &
CTL_PID=$!

echo "::: starting node"
"$BIN/fileblock-node" \
  --endpoint="unix://$NODE_SOCK" \
  --node-id=local \
  --state-dir="$STATE" \
  --stores-root="$STORES" \
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
  --params "backingStore.type=local" \
  --params "backingStore.local.path=$BACKING" \
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
  --vol-context "backingStore.type=local" \
  --vol-context "backingStore.local.path=$BACKING" \
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
  --vol-context "backingStore.type=local" \
  --vol-context "backingStore.local.path=$BACKING" \
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
  --params "backingStore.type=local" \
  --params "backingStore.local.path=$BACKING" \
  orphan-vol)
VOL2=$(printf '%s\n' "$CREATE_OUT" | head -n1 | awk '{print $1}' | tr -d '"')
ORPHAN=$(losetup --find --show "$BACKING/$VOL2.img")
kill "$NODE_PID"; wait "$NODE_PID" 2>/dev/null || true
"$BIN/fileblock-node" \
  --endpoint="unix://$NODE_SOCK" \
  --node-id=local \
  --state-dir="$STATE" \
  --stores-root="$STORES" \
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
  --params "backingStore.type=local" \
  --params "backingStore.local.path=$BACKING" \
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
  --stores-root="$STORES" \
  --log-level=debug >>"$LOG/node.log" 2>&1 &
NODE_PID=$!
for _ in $(seq 1 20); do [[ -S "$NODE_SOCK" ]] && break; sleep 0.1; done
CSI_ENDPOINT="unix://$NODE_SOCK" csc node stage \
  --cap "SINGLE_NODE_WRITER,mount,ext4" \
  --staging-target-path "$STAGE_A" \
  --vol-context "backingStore.type=local" \
  --vol-context "backingStore.local.path=$BACKING" \
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
  --stores-root="$STORES" \
  --log-level=debug >>"$LOG/node.log" 2>&1 &
NODE_PID=$!
for _ in $(seq 1 20); do [[ -S "$NODE_SOCK" ]] && break; sleep 0.1; done
CSI_ENDPOINT="unix://$NODE_SOCK" csc node stage \
  --cap "SINGLE_NODE_WRITER,mount,ext4" \
  --staging-target-path "$STAGE_B" \
  --vol-context "backingStore.type=local" \
  --vol-context "backingStore.local.path=$BACKING" \
  "$VOL3"
grep -q '^node-a-was-here$' "$STAGE_B/who" || {
  echo "data written by node-a not visible to node-b"; exit 1; }

CSI_ENDPOINT="unix://$NODE_SOCK" csc node unstage --staging-target-path "$STAGE_B" "$VOL3"
csc controller del "$VOL3"

# loop_ss prints the logical sector size of the loop device backing $1.
loop_ss() {
  local dev
  dev=$(losetup --noheadings --output NAME --associated "$1" | head -n1)
  [[ -n "$dev" ]] || { echo "no loop device for $1" >&2; return 1; }
  blockdev --getss "$dev"
}

echo "::: loop sector size follows the on-disk format"
# restart_node brings the node plugin back as "local"; the takeover test
# above left it running as node-b.
restart_node() {
  kill "$NODE_PID"; wait "$NODE_PID" 2>/dev/null || true
  "$BIN/fileblock-node" \
    --endpoint="unix://$NODE_SOCK" \
    --node-id=local \
    --state-dir="$STATE" \
    --stores-root="$STORES" \
    --log-level=debug >>"$LOG/node.log" 2>&1 &
  NODE_PID=$!
  for _ in $(seq 1 20); do [[ -S "$NODE_SOCK" ]] && break; sleep 0.1; done
}
restart_node
CREATE_OUT=$(csc controller new \
  --cap "SINGLE_NODE_WRITER,mount,ext4" \
  --req-bytes $((128*1024*1024)) \
  --params "backingStore.type=local" \
  --params "backingStore.local.path=$BACKING" \
  sector-vol)
VOL6=$(printf '%s\n' "$CREATE_OUT" | head -n1 | awk '{print $1}' | tr -d '"')
IMG6="$BACKING/$VOL6.img"
STAGE6="$STATE/staging/$VOL6"
mkdir -p "$STAGE6"
stage6() {
  CSI_ENDPOINT="unix://$NODE_SOCK" csc node stage \
    --cap "SINGLE_NODE_WRITER,mount,ext4" \
    --staging-target-path "$STAGE6" \
    --vol-context "backingStore.type=local" \
    --vol-context "backingStore.local.path=$BACKING" \
    "$VOL6"
}
unstage6() {
  CSI_ENDPOINT="unix://$NODE_SOCK" csc node unstage --staging-target-path "$STAGE6" "$VOL6"
}
# expect_ss <case> <want>: stage, check the loop sector size and the mount,
# unstage.
expect_ss() {
  stage6
  local got
  got=$(loop_ss "$IMG6")
  [[ "$got" == "$2" ]] || { echo "$1: loop sector size $got, want $2"; exit 1; }
  findmnt -no FSTYPE "$STAGE6" | grep -q '^ext4$' || { echo "$1: not mounted as ext4"; exit 1; }
  unstage6
}
dumpe2fs -h "$IMG6" 2>/dev/null | grep -Eq '^Block size:[[:space:]]+4096$' \
  || { echo "new volume's ext4 does not use 4096-byte blocks"; exit 1; }
expect_ss "new plaintext volume" 4096
# A volume made by an older mke2fs config with 1 KiB blocks must keep
# mounting: its loop may not use sectors larger than its blocks.
mkfs.ext4 -q -F -b 1024 -m 0 "$IMG6"
expect_ss "legacy 1 KiB-block volume" 1024
# An image that predates 4 KiB size rounding: the sector size must divide
# the image size.
mkfs.ext4 -q -F -b 4096 -m 0 "$IMG6"
truncate -s +512 "$IMG6"
expect_ss "unaligned legacy image" 512
csc controller del "$VOL6"

echo "::: encrypted volume: first-stage format, ciphertext at rest, rotation"
restart_node
# Hex keys: csc parses X_CSI_SECRETS as k=v pairs and base64 padding is '='.
KEY1=$(openssl rand -hex 32)
KEY2=$(openssl rand -hex 32)
CREATE_OUT=$(csc controller new \
  --cap "SINGLE_NODE_WRITER,mount,ext4" \
  --req-bytes $((128*1024*1024)) \
  --params "backingStore.type=local" \
  --params "backingStore.local.path=$BACKING" \
  --params "encrypted=true" \
  enc-vol)
VOL4=$(printf '%s\n' "$CREATE_OUT" | head -n1 | awk '{print $1}' | tr -d '"')
IMG4="$BACKING/$VOL4.img"
STAGE4="$STATE/staging/$VOL4"
mkdir -p "$STAGE4"
stage_enc() {
  X_CSI_SECRETS="$1" CSI_ENDPOINT="unix://$NODE_SOCK" csc node stage \
    --cap "SINGLE_NODE_WRITER,mount,ext4" \
    --staging-target-path "$STAGE4" \
    --vol-context "backingStore.type=local" \
    --vol-context "backingStore.local.path=$BACKING" \
    --vol-context "encrypted=true" \
    "$VOL4"
}
unstage_enc() {
  CSI_ENDPOINT="unix://$NODE_SOCK" csc node unstage --staging-target-path "$STAGE4" "$VOL4"
}
cmp -s <(head -c 16777216 "$IMG4") <(head -c 16777216 /dev/zero) \
  || { echo "controller wrote to an encrypted image"; exit 1; }
stage_enc "key=$KEY1"
cryptsetup isLuks "$IMG4" || { echo "image is not LUKS after first stage"; exit 1; }
[[ $(loop_ss "$IMG4") == 4096 ]] || { echo "encrypted volume's loop is not 4096-byte sectors"; exit 1; }
cryptsetup luksDump "$IMG4" | grep -Eq 'sector:[[:space:]]+4096' \
  || { echo "LUKS data segment does not use 4096-byte sectors"; exit 1; }
findmnt -no FSTYPE "$STAGE4" | grep -q '^ext4$' || { echo "encrypted fs not ext4"; exit 1; }
CANARY="fileblock-smoke-canary-$(openssl rand -hex 8)"
echo "$CANARY" >"$STAGE4/canary"
unstage_enc
grep -qa "$CANARY" "$IMG4" && { echo "plaintext canary found in the .img"; exit 1; }

echo "::: rotation: key=KEY2, previousKey=KEY1"
stage_enc "key=$KEY2,previousKey=$KEY1"
grep -qx "$CANARY" "$STAGE4/canary" || { echo "data lost across rotation"; exit 1; }
unstage_enc
printf %s "$KEY1" | cryptsetup open --test-passphrase --key-file=- "$IMG4" \
  && { echo "old key still opens after rotation"; exit 1; }
printf %s "$KEY2" | cryptsetup open --test-passphrase --key-file=- "$IMG4" \
  || { echo "new key does not open after rotation"; exit 1; }

echo "::: wrong key is refused and leaves nothing attached"
stage_enc "key=$(openssl rand -hex 32)" && { echo "stage with a wrong key succeeded"; exit 1; }
losetup --noheadings --output BACK-FILE | grep -qF "$IMG4" \
  && { echo "loop left attached after wrong-key stage"; exit 1; }

# name cipher keySize requestBytes imageBytes. Adiantum is the reason the
# parameter exists; AES-CBC is a second, structurally different spec and
# also requests an unaligned size (like a "1G" PVC), which must round up
# to whole 4 KiB LUKS2 sectors. Driven by hack/csi-call, not csc: csc
# splits key=val lists on commas, and Adiantum's spec has one.
for spec in "cbc aes-cbc-essiv:sha256 256 100000001 100003840" \
            "adiantum xchacha12,aes-adiantum-plain64 256 134217728 134217728"; do
  read -r CNAME CIPHER CKEYSIZE CREQ CIMG <<<"$spec"
  echo "::: configurable cipher: $CIPHER ($CKEYSIZE-bit key, $CREQ bytes requested)"
  CREATE_OUT=$("$BIN/csi-call" -endpoint "unix://$CTL_SOCK" create \
    -name "cipher-$CNAME" \
    -bytes "$CREQ" \
    -p "backingStore.type=local" \
    -p "backingStore.local.path=$BACKING" \
    -p "encrypted=true" \
    -p "encryption.cipher=$CIPHER" \
    -p "encryption.keySize=$CKEYSIZE")
  printf '%s\n' "$CREATE_OUT" | grep -qxF "encryption.cipher=$CIPHER" \
    || { echo "controller did not carry the cipher into volume context: $CREATE_OUT"; exit 1; }
  VOL5=$(printf '%s\n' "$CREATE_OUT" | head -n1)
  STAGE5="$STATE/staging/$VOL5"
  mkdir -p "$STAGE5"
  # Stage with exactly the volume context the controller returned.
  CTX_ARGS=()
  while IFS= read -r line; do CTX_ARGS+=(-ctx "$line"); done < <(printf '%s\n' "$CREATE_OUT" | tail -n +2)
  "$BIN/csi-call" -endpoint "unix://$NODE_SOCK" stage \
    -volume "$VOL5" -staging "$STAGE5" "${CTX_ARGS[@]}" -secret "key=$KEY2"
  findmnt -no FSTYPE "$STAGE5" | grep -q '^ext4$' || { echo "$CIPHER volume fs not ext4"; exit 1; }
  CSI_ENDPOINT="unix://$NODE_SOCK" csc node unstage --staging-target-path "$STAGE5" "$VOL5"
  [[ $(stat -c %s "$BACKING/$VOL5.img") == "$CIMG" ]] \
    || { echo "image is $(stat -c %s "$BACKING/$VOL5.img") bytes, want $CIMG"; exit 1; }
  DUMP=$(cryptsetup luksDump "$BACKING/$VOL5.img")
  grep -Eq 'sector:[[:space:]]+4096' <<<"$DUMP" \
    || { echo "$CIPHER data segment does not use 4096-byte sectors"; exit 1; }
  grep -Eq "cipher:[[:space:]]+$CIPHER\$" <<<"$DUMP" \
    || { echo "LUKS header does not record $CIPHER"; exit 1; }
  # A keyslot's "Key:" line is the volume key; "Cipher key:" is the slot's own.
  grep -Eq "^[[:space:]]+Key:[[:space:]]+$CKEYSIZE bits" <<<"$DUMP" \
    || { echo "LUKS header does not record a $CKEYSIZE-bit key"; exit 1; }
  csc controller del "$VOL5"
done

echo "::: orphan crypt mapping is reclaimed on plugin restart"
ORPHAN4=$(losetup --find --show "$IMG4")
printf %s "$KEY2" | DM_DISABLE_UDEV=1 cryptsetup open --key-file=- "$ORPHAN4" fbcrypt-smokeorphan
restart_node
sleep 1
[[ -e /dev/mapper/fbcrypt-smokeorphan ]] && { echo "orphan crypt mapping not closed"; exit 1; }
losetup "$ORPHAN4" 2>/dev/null && { echo "orphan loop under crypt not detached"; exit 1; } || true
csc controller del "$VOL4"

echo
echo "smoke OK"
