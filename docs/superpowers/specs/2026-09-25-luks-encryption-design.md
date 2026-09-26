# Optional LUKS2 encryption with Secret-driven key rotation

**Status:** Draft
**Date:** 2026-09-25
**Target version:** v0.5.0

## Problem

A fileblock `.img` is a plain ext4 filesystem sitting on shared storage.
Anyone who can read the NFS export, the SMB share, or the local backing
directory — the NAS admin, a backup job, a stolen disk, a snapshot
copied off-site — can loop-mount it and read every file. The storage
tier has to be trusted with the plaintext.

## Solution

An opt-in, per-StorageClass `encrypted: "true"` parameter. Encrypted
volumes are LUKS2 containers: the node plugin layers dm-crypt between
the loop device and ext4, so the `.img` on the backing store is only
ever ciphertext. The key is a Kubernetes Secret named by the standard
CSI node-stage-secret parameters — the convention AWS EBS, ceph-csi,
Trident, and lukscryptwalker-csi use — which means the kubelet fetches
it and hands it to `NodeStageVolume`. The node plugin gets no new RBAC.

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: fileblock-encrypted
provisioner: fileblock.csi
parameters:
  backingStore.type: nfs
  backingStore.nfs.server: nfs.example.internal
  backingStore.nfs.path: /exports/fileblock
  encrypted: "true"
  csi.storage.k8s.io/node-stage-secret-name: fileblock-encryption-key
  csi.storage.k8s.io/node-stage-secret-namespace: fileblock-system
---
apiVersion: v1
kind: Secret
metadata:
  name: fileblock-encryption-key
  namespace: fileblock-system
stringData:
  key: "<openssl rand -base64 32>"
  # previousKey: "<the key being rotated out>"
```

Rotation is automatic: change the Secret and every volume moves its LUKS
key slot to the new key the next time it is staged. Existing
unencrypted StorageClasses and volumes are untouched.

```
before:  .img ── loop ── ext4 ── stage mount
after:   .img ── loop ── dm-crypt (LUKS2) ── ext4 ── stage mount
```

## Secret reference templating

The `csi.storage.k8s.io/node-stage-secret-*` parameters are consumed by
the external-provisioner, not by fileblock. It resolves their templates
itself (`getSecretReference` in csi-provisioner's
`pkg/controller/controller.go`), writes the result into the PV's
`spec.csi.nodeStageSecretRef`, and strips the keys before `CreateVolume`.
The driver never sees them.

The provisioner's token spelling is therefore fixed, and it differs
from the one `backingStore.nfs.subDir` shipped with in v0.4.0
(borrowed from csi-driver-nfs). Two spellings for the same three
values in one StorageClass would be a trap, so **`subDir` moves to the
provisioner's spelling** (design §7). After this change every template
in a fileblock StorageClass uses one vocabulary:

| token | `subDir` | node-stage-secret-name | node-stage-secret-namespace |
|---|---|---|---|
| `${pvc.namespace}` | yes | yes | yes |
| `${pvc.name}` | yes | yes | no |
| `${pv.name}` | yes | yes | yes |

An unresolvable token is fatal everywhere — `CreateVolume` fails for
`subDir`, provisioning fails (`invalid tokens`) for the secret
parameters — never a literal directory or Secret name.

`${pvc.name}` is refused in the namespace template by the provisioner,
deliberately: the PVC author picks the name, and must not be able to
steer the node to another namespace's Secret. `subDir` keeps allowing
it — a directory name carries no such authority, and volumeIDs are
unique per PV regardless. The provisioner's
`${pvc.annotations['…']}` token for secret names is not added to
`subDir`; nothing needs it.

A per-namespace key and a per-namespace directory now read the same:

```yaml
  backingStore.nfs.subDir: ${pvc.namespace}/fileblock
  csi.storage.k8s.io/node-stage-secret-name: fileblock-encryption-key
  csi.storage.k8s.io/node-stage-secret-namespace: ${pvc.namespace}
```

Keeping `subDir`'s spelling and adding our own secret parameters was
considered and rejected. They would be resolved by our controller, and
the node plugin would have to fetch the Secret from the API itself —
the kubelet only passes Secrets referenced from the PV. With a
templated namespace that is a ClusterRole granting `get secrets`
cluster-wide to a DaemonSet on every node, trading the kubelet's node
authorizer for a much wider grant.

## Secret contract

| key | required | meaning |
|---|---|---|
| `key` | yes | current passphrase; at least 32 bytes |
| `previousKey` | no | passphrase being rotated out |

- Values are used **byte-for-byte** as the LUKS passphrase — no
  trimming, no base64 decoding. A trailing newline from
  `kubectl create secret --from-file` is part of the key. The README's
  recovery recipe uses the same bytes, so this is consistent; it is
  documented rather than normalized, because normalizing would make the
  in-cluster key differ from what `kubectl get secret … | base64 -d`
  prints.
- `key` shorter than 32 bytes fails the stage with `InvalidArgument`.
  The minimum is what justifies the cheap PBKDF below; a human
  passphrase under a cheap PBKDF would be brute-forceable.
- `encrypted: "true"` with no `key` in the stage secrets fails with
  `FailedPrecondition`, naming the two StorageClass parameters.
- Secret values never appear in argv, logs, error strings, or on disk.

## Design

### 1. `pkg/exec` — feeding secrets to child processes

`Runner` has no stdin, and `cryptsetup luksAddKey` needs two keys at
once. A second method passes each secret over its own inherited pipe:

```go
type Runner interface {
    Run(ctx context.Context, name string, args ...string) (string, error)
    // RunWithSecrets exposes secrets[i] to the child as /dev/fd/(3+i)
    // via os/exec ExtraFiles; args reference them by that path.
    RunWithSecrets(ctx context.Context, secrets [][]byte, name string, args ...string) (string, error)
}
```

Each secret is written into a pipe by a goroutine and the write end
closed, so the child reads to EOF. `exectest.FakeRunner` records the
secrets on the `Call` so tests can assert which key reached which
command. `SecretFD(i int) string` returns the `/dev/fd/N` path so
callers don't hardcode fd numbers.

### 2. `pkg/crypt` — new package, the only caller of `cryptsetup`

```go
type Keys struct{ Current, Previous []byte }

// MapperName is deterministic and length-bounded: "fbcrypt-" + the
// first 24 hex chars of sha256(volumeID). dm names cap at 127 bytes;
// volumeIDs don't.
func MapperName(volumeID string) string

// Prepare formats (first stage only), rotates, and opens dev, returning
// /dev/mapper/<name>. Idempotent across crashes at any step.
func (c *Crypt) Prepare(ctx context.Context, dev, name string, k Keys) (string, error)

// IsOpen reports whether /dev/mapper/<name> exists.
func (c *Crypt) IsOpen(ctx context.Context, name string) (bool, error)

func (c *Crypt) Close(ctx context.Context, name string) error
func (c *Crypt) List(ctx context.Context) ([]Mapping, error) // name + backing device
```

Every `cryptsetup` invocation runs with `DM_DISABLE_UDEV=1` set by
`Crypt` (the container has no udevd; without it libdevmapper waits on a
udev cookie that never arrives). Smoke must confirm this.

**Format parameters:** `luksFormat --type luks2 --cipher aes-xts-plain64
--key-size 512 --pbkdf pbkdf2 --pbkdf-force-iterations 1000
--batch-mode`. PBKDF hardening protects low-entropy passphrases; a
≥32-byte random key needs none, and cryptsetup's argon2id default
benchmarks up to 1 GiB of memory per unlock — too much for a node
plugin on small nodes.

**`Prepare` flow:**

1. **First stage.** If `cryptsetup isLuks dev` fails, read the first
   16 MiB of `dev`. All zeros → a freshly truncated sparse image:
   `luksFormat --label fileblock-unformatted` with `Current`. Anything
   non-zero → fail `FailedPrecondition` ("not a LUKS volume and not
   blank; refusing to format"). A header fileblock can't open is never
   overwritten.
2. **Rotate.** `open --test-passphrase` with `Current`.
   - Fails, and `Previous` is set and opens → `luksAddKey` (authorized
     by `Previous`, adding `Current`), then `luksRemoveKey` `Previous`.
   - Succeeds, and `Previous` is set and also opens → `luksRemoveKey`
     `Previous`. This finishes a rotation that crashed between add and
     remove.
   - Neither opens → fail `PermissionDenied`. Header untouched.
   - If `Previous == Current`, skip removal: it would delete the only
     slot.
3. **Open** with `Current`.
4. **Make the filesystem, once.** If the LUKS2 label is
   `fileblock-unformatted`, `mkfs.ext4` the mapper (same flags
   `image.Create` uses today), then `cryptsetup config --label
   fileblock`. Return `/dev/mapper/<name>`.

The label is what makes first-stage crash-safe. A crash between
`luksFormat` and `mkfs` leaves a valid header over an empty payload;
the label says so explicitly, and the next stage finishes the job.
Inferring "needs mkfs" from `blkid` finding no signature was rejected:
a real ext4 with a damaged primary superblock also shows none, and
would be reformatted.

LUKS2 writes two header copies transactionally, so a crash mid
`luksAddKey`/`luksRemoveKey` leaves the header either before or after
the change — and step 2 converges from both.

### 3. `pkg/driver` — controller

`CreateVolume` parses `encrypted` (absent/`"false"` → off, `"true"` →
on, anything else → `InvalidArgument`). It is not part of
`store.Config`: encryption is a property of the volume, not the store,
so `StoreID` and every existing volumeID are unchanged. When on:

- `image.Manager.Create` gains an option to skip `mkfs.ext4` — the
  controller only truncates the sparse file. The node formats on first
  stage, which is also where the key is. The controller never holds a
  key and needs neither cryptsetup nor dm-crypt.
- The returned `VolumeContext` carries `encrypted: "true"`.

`pkg/image` stays the only package that *creates or deletes* `.img`
files; `pkg/crypt` writes the LUKS header and mkfs inside the mapping,
through the loop device, the same way `e2fsck`/`resize2fs` already
write through it today. The CLAUDE.md on-disk contract is amended to
say so.

### 4. `pkg/driver` — node

`NodeStageVolume`, with `encrypted=true` in volume context:

```
crypt.IsOpen? → losetup attach → set-capacity → crypt.Prepare → e2fsck → resize2fs → mount mapper
```

After the existing state-file idempotency check (already staged and
mounted at this path → OK), an existing `fbcrypt-<name>` mapping fails
the stage `FailedPrecondition` *before* a loop is attached.
`losetup --find --show` always takes a fresh loop device, so a mapping
that already exists belongs to another attachment of the same `.img`;
opening a second one is exactly the double-instance hazard of #42. The
deterministic mapper name makes this guard structural for encrypted
volumes. Leftovers from a crashed plugin are closed by the reconciler
at startup; in-process failures close their own mapping.

`losetup --set-capacity` moves ahead of the open, so a volume expanded
offline opens at its full new size and `resize2fs` on the mapper grows
ext4 into it. Unencrypted volumes keep today's order exactly. Failure
cleanup closes the mapping before detaching the loop.

`NodeUnstageVolume`: umount → `crypt.Close` → `losetup --detach`.

`NodeExpandVolume`: `resize2fs` targets `CryptDev` when set. Expansion
is `OFFLINE`, so a re-stage (and a full-size open) always precedes it;
no node-expand secret is needed.

`NodeGetVolumeStats` is unchanged (it `statfs`es the mount).

gRPC codes: missing key → `FailedPrecondition`; short key →
`InvalidArgument`; no key opens → `PermissionDenied`; mapping already
open, or non-blank non-LUKS image → `FailedPrecondition`;
missing `dm_crypt` / `cryptsetup` → `Internal` with a message naming
the module.

### 5. `pkg/loop` — state and reconcile

`Mapping` gains `CryptDev string \`json:"cryptDev,omitempty"\``. State
files written by v0.4.0 load unchanged.

The reconciler takes the crypt lister and, before touching loops:

1. Drops state entries whose `CryptDev` is set but no longer present.
2. Closes `fbcrypt-*` mappings with no state entry whose backing device
   is a loop fileblock would reclaim (backed by a `.img` under a store
   root). A live mapping holds its loop device open, so without this
   orphan loops could never detach.

Mappings not named `fbcrypt-*` are never touched.

### 6. Deployment

- `Dockerfile`: add `cryptsetup-bin` to the runtime image.
- No RBAC change. The kubelet reads the node-stage Secret under the node
  authorizer.
- `examples/`: an encrypted StorageClass + Secret pair.
- `deploy/kustomize/overlays/e2e`: an encrypted StorageClass and its
  Secret.
- Nodes need the `dm_crypt` module (loaded on demand by the kernel on
  every mainstream distro, including kind's host on GitHub runners).

### 7. `pkg/store` — `subDir` token spelling

`tmplPVCNamespace`, `tmplPVCName` and `tmplPVName` in
`pkg/store/parse.go` become `${pvc.namespace}`, `${pvc.name}` and
`${pv.name}`. Substitution, validation, and the fatal unresolved-token
check are unchanged; the error text already lists the supported tokens,
so an old-spelling StorageClass fails with the new spelling in the
message. The comment crediting csi-driver-nfs's spelling is replaced
with one pointing at the provisioner's.

No backward compatibility: the old spelling is not accepted as an
alias. Impact is limited to *new* provisioning — `subDir` is resolved
once in `CreateVolume` and baked into the volumeID's storeID and the
volume context, so volumes created under v0.4.0 keep working
unchanged. A StorageClass still using `${pvc.metadata.namespace}` gets
`InvalidArgument` on its next `CreateVolume` until it is edited
(StorageClass parameters are immutable, so: delete and recreate it
with the same name; bound PVs are unaffected).

Updated with it: `parse_test.go`, `controller_test.go`,
`test/e2e/subdir_test.go` (including its "no literal-token directory"
assertion), `deploy/manifests_test.go` and the controller Deployment
comment, README usage and parameter table. Historical specs, plans and
the released v0.4.0 CHANGELOG entry are left as written.

## Security properties and limits

- **Protects:** the `.img` at rest on the backing store, its snapshots,
  and its backups, against anyone without the key.
- **Does not protect:** data on the node while staged (plaintext in the
  page cache and via the mount), or from anyone who can read the Secret.
- **Rotation changes the key slot, not the master key.** Copies of the
  `.img` taken before rotation — NAS snapshots, backups — still open
  with the old key, and anyone who held the old key *and* a copy of the
  header could have derived the master key. Rotation is hygiene against
  a passphrase leaking in future; after a real compromise, copy the
  data into a fresh encrypted volume.
- **Losing the key loses the data.** The README tells operators to keep
  a copy of the key outside the cluster.
- **Offline recovery** needs only the file and the key:
  `kubectl -n <ns> get secret <name> -o jsonpath='{.data.key}' | base64 -d | cryptsetup open --key-file=- fb-….img recovered`.

## Testing

### Unit

- `pkg/exec`: `RunWithSecrets` delivers each secret on its fd and to
  EOF (real `cat /dev/fd/3` child); secrets absent from `Error`.
- `pkg/crypt` with `FakeRunner`, one test per branch of `Prepare`:
  blank → format + label, unformatted label → mkfs + relabel, non-blank
  non-LUKS → refuse, current opens, previous-only → add+remove,
  both open → remove previous, `Previous == Current`, neither opens.
  Assert which secret each command received.
- `pkg/driver`: `encrypted` parsing; volume context round trip;
  controller skips mkfs; existing mapping refused before attach; node stage ordering (set-capacity before
  open), missing/short key codes, unstage order, cleanup on failure;
  unencrypted path byte-for-byte unchanged.
- `pkg/store`: new spellings substitute; each old spelling fails as an
  unresolved token.
- `pkg/loop`: `CryptDev` round trip; old state file loads; reconciler
  closes orphan mappings before detaching loops, leaves foreign dm
  names alone.

### Integration (`hack/smoke.sh`, root)

Encrypted round trip: create, stage, write a marker, unstage, assert
the marker bytes are absent from the `.img`, restage, read it back.
Rotation: restage with `previousKey`=old, `key`=new; assert the old key
no longer opens (`cryptsetup open --test-passphrase`) and the marker is
intact. Orphan mapping reclaimed on plugin restart.

### `hack/csi-sanity.sh`

A second pass with `--csi.testvolumeparameters` setting `encrypted:
"true"` and `--csi.secrets` supplying `NodeStageVolumeSecret.key`.

### End-to-end (`test/e2e`, local and NFS variants)

An encrypted PVC: write a marker from a pod, grep the `.img` on the
backing store for it (must be absent), rotate the Secret, delete and
recreate the pod, read the marker back, and confirm the old key no
longer opens the header.

## Documentation

README: an *Encryption* section (StorageClass + Secret example, the
token table above, rotation procedure, recovery recipe, key backup,
the snapshot caveat, the 16 MiB header overhead). README's `subDir`
section moves to the new spelling. CLAUDE.md: CSI surface, on-disk
contract amendment, state-file `CryptDev`. CHANGELOG `[Unreleased]`:
the encryption feature under *Added*, and the `subDir` spelling change
under *Changed*, marked **breaking**, with the delete-and-recreate
StorageClass instruction.

## Out of scope

- Encrypting or migrating existing plaintext volumes.
- KMS / Vault integration; key sources other than a Secret.
- Live rotation of already-staged volumes (rotation lands on next
  stage). A node-side Secret watcher could add it later without
  changing the Secret contract.
- Master-key re-encryption (`cryptsetup reencrypt`).
- Padding the image by the LUKS header size. Capacity stays the `.img`
  apparent size; the 16 MiB header is documented overhead, like ext4's
  own metadata.
- Raw-block volumes (still unsupported for all volumes).

## Release

Minor bump (v0.5.0), which under 0.x semver carries the breaking
`subDir` spelling change. Adds a new optional StorageClass parameter
and a runtime dependency (`cryptsetup-bin`). No existing volume changes
behavior; only StorageClasses using the old `subDir` tokens need
editing.
