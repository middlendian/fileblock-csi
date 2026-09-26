# fileblock

A Kubernetes CSI driver that gives pods a real local-disk filesystem
(`ext4`) backed by a sparse image file on **any** directory the node can
read — NFS, SMB, local disk, or a FUSE mount.

## Why

NFSv3 silently strips the POSIX execute bit and round-trips other mode bits
inconsistently. The fallout:

- `git status` shows phantom diffs unless you set `core.fileMode=false`.
- `chmod +x` doesn't stick.
- File locking is unreliable.
- Anything that depends on real local-disk semantics (`flock`, `O_DIRECT`,
  proper `chown`) breaks.

Existing NFS CSI drivers paper over this by bind-mounting NFS into pods,
which preserves all of the same problems. **fileblock** doesn't do that.
It stores each PV as a single sparse `ext4` image file on the backing store,
loop-mounts it on the node where the pod is scheduled, and presents the pod
with a genuine local filesystem.

## How it works

```
  PVC ──┐
        │   controller (one Deployment per cluster)
        │     truncate ─► /backing/${vol}.img    (sparse)
        │     mkfs.ext4 ─► same
        │
   pod ─┤   node plugin  (one DaemonSet pod per node)
        │     losetup --find --show /backing/${vol}.img  ─► /dev/loopN
        │     e2fsck -p /dev/loopN                       (always)
        │     mount -t ext4 /dev/loopN <staging>
        │     mount --bind <staging> <pod target>
        ▼
     /data inside the pod === ext4 on a loop device
```

Each PV is a single sparse `fb-<uuid>.img` file on the backing store —
actual disk usage grows only with real writes. There is no separate metadata
sidecar; capacity is read from the file's apparent size (`stat().Size()`).

## Requirements

- Linux nodes with `losetup`, `mount`, `umount`, `findmnt`,
  `e2fsprogs` (`mkfs.ext4`, `e2fsck`, `resize2fs`).
- The `loop` kernel module loaded. On Raspberry Pi / small ARM nodes you
  may want `modprobe loop max_loop=64` to expand the default 8-loop pool.
- For NFS backing stores: `nfs-common` on each node (the driver image
  includes it). For local backing stores: no extra dependencies.

## Quickstart

1. **Apply the base manifests** (driver, RBAC, CSIDriver):

   ```sh
   kubectl apply -k 'github.com/middlendian/fileblock-csi/deploy/kustomize/base?ref=v0.3.0'
   ```

   Or follow `main`:

   ```sh
   kubectl apply -k 'github.com/middlendian/fileblock-csi/deploy/kustomize/base?ref=main'
   ```

   The base kustomization sets the image tag from the ref — `vX.Y.Z` on a
   release tag, `latest` on `main`.

2. **Create a StorageClass** pointing at your backing store:

   ```yaml
   # NFS-backed example:
   apiVersion: storage.k8s.io/v1
   kind: StorageClass
   metadata:
     name: fileblock
   provisioner: fileblock.csi
   parameters:
     backingStore.type: nfs
     backingStore.nfs.server: nfs.example.internal
     backingStore.nfs.path: /exports/fileblock
     backingStore.nfs.mountOptions: "nfsvers=4.1,hard,timeo=600"
   reclaimPolicy: Delete
   allowVolumeExpansion: true
   volumeBindingMode: WaitForFirstConsumer
   ```

   For a local directory (single-node or testing):

   ```yaml
   apiVersion: storage.k8s.io/v1
   kind: StorageClass
   metadata:
     name: fileblock
   provisioner: fileblock.csi
   parameters:
     backingStore.type: local
     backingStore.local.path: /var/lib/fileblock
   reclaimPolicy: Delete
   allowVolumeExpansion: true
   volumeBindingMode: WaitForFirstConsumer
   ```

   **Local SCs require a hostPath overlay patch.** The base manifests do not
   mount any host paths beyond `/var/lib/kubelet` and `/dev`. When you use a
   `local`-type StorageClass, `LocalMounter` will bind-mount
   `backingStore.local.path` from the host — but that path must first be
   visible inside the controller and node pods' mount namespaces. Add a
   hostPath patch for both the Deployment and the DaemonSet that mounts the
   same path through. See
   `deploy/kustomize/overlays/example-localdir/host-source-patch-controller.yaml`
   and `host-source-patch-node.yaml` for the canonical reference; the
   `example-localdir` kustomization wires them in. NFS-type SCs do **not**
   need such a patch — they mount the export themselves at runtime.

   Both NFSv3 and NFSv4 are supported, but **prefer NFSv4 where the
   server speaks it**. NFSv4 has no NLM/statd/portmapper to negotiate
   and no privileged-port binds — `mount.nfs` succeeds immediately with
   just `nfsvers=4.1` in `mountOptions`.

   For NFSv3 servers, you **must** add `nolock` to `mountOptions`:

   ```yaml
   backingStore.nfs.mountOptions: "nfsvers=3,hard,timeo=600,nolock"
   ```

   fileblock's cross-node mutual exclusion is CSI's `SINGLE_NODE_WRITER`
   serialization, not NFS-level file locks (the `.img` is opened by
   exactly one node's loop device at a time). The client-side lock
   manager (`rpc.statd`) is therefore not needed and isn't running in
   the driver pod — without `nolock`, `mount.nfs` refuses with
   "rpc.statd is not running but is required for remote locking".

   ### Per-namespace backing directories

   One export can hold a separate backing directory per namespace.
   `backingStore.nfs.subDir` accepts pv/pvc metadata tokens spelled as
   external-provisioner spells them:

   ```yaml
   parameters:
     backingStore.type: nfs
     backingStore.nfs.server: nfs.example.internal
     backingStore.nfs.path: /exports/k8s_ns
     backingStore.nfs.subDir: ${pvc.namespace}/fileblock
   ```

   A PVC in namespace `team-a` then lands at
   `/exports/k8s_ns/team-a/fileblock/fb-<uuid>.img`. The export is still
   mounted exactly once per node — every `subDir` under one export shares a
   single mount.

   Supported tokens are `${pvc.namespace}`, `${pvc.name}` and `${pv.name}`
   — the same spelling external-provisioner uses for
   `csi.storage.k8s.io/node-stage-secret-*`, so every template in a
   StorageClass reads alike. They are substituted by the driver from
   metadata that external-provisioner injects only when it runs with
   `--extra-create-metadata=true`; the shipped manifests set that flag. If
   a token cannot be resolved, `CreateVolume` fails with `InvalidArgument`
   rather than creating a directory named after the literal token — every
   namespace would otherwise share it, and nothing would reveal that
   short of listing the export by hand.

   `subDir` must be relative, must not contain `..`, must not contain NUL
   bytes, and must not clean to `.`. It is NFS-only;
   setting it with `backingStore.type: local` is rejected. Omitting it is
   the existing behaviour: the export root. Existing volumes are unaffected
   — a store with no `subDir` keeps the storeID it has always had.

   A namespace directory removed out-of-band is not recreated until that
   export's mount is re-established or the driver process restarts, so
   `CreateVolume` returns `Internal` for that store in the meantime.

   Pre-built example overlays live at:
   - `deploy/kustomize/overlays/example-localdir/`
   - `deploy/kustomize/overlays/example-nfs-shared/`

3. **Create a volume**

   ```sh
   kubectl apply -f examples/pvc.yaml -f examples/pod.yaml
   ```

4. **Verify**

   ```sh
   kubectl exec fileblock-demo -- sh -c 'stat -f -c %T /data; ls -l /data/hello.sh'
   # → ext2/ext3   (yes, that's what stat reports for ext4)
   # → -rwxr-xr-x  (execute bit survived)
   ```

   Drop into a shell and run `git`-style checks; the execute bit and `chmod`
   round-trip without `core.fileMode=false`.

## Configuration

`StorageClass` parameters:

| Key                           | Required          | Notes                                                         |
|-------------------------------|-------------------|---------------------------------------------------------------|
| `backingStore.type`           | yes               | `nfs` or `local`                                              |
| `backingStore.nfs.server`     | when type=nfs     | NFS server hostname or IP                                     |
| `backingStore.nfs.path`       | when type=nfs     | Exported path on the server                                   |
| `backingStore.nfs.mountOptions` | no (type=nfs)  | Mount options string, e.g. `"nfsvers=4.1,hard,timeo=600"`    |
| `backingStore.nfs.subDir`     | no (type=nfs)     | Subdirectory of the export to hold this store's `.img` files; supports `${pvc.namespace}`, `${pvc.name}`, `${pv.name}` |
| `backingStore.local.path`     | when type=local   | Absolute path on every node that can read & write the store   |
| `encrypted`                   | no                | `"true"` enables LUKS2 encryption; see [Encryption](#encryption) |
| `encryption.cipher`           | no (encrypted)    | cryptsetup `--cipher` spec; default `aes-xts-plain64`; see [Choosing a cipher](#choosing-a-cipher) |
| `encryption.keySize`          | with `encryption.cipher` | Key size in bits (cryptsetup `--key-size`); default `512` with the default cipher |
| `csi.storage.k8s.io/node-stage-secret-name` | when `encrypted=true` | Name of the Secret holding `key` (and optional `previousKey`) |
| `csi.storage.k8s.io/node-stage-secret-namespace` | when `encrypted=true` | Namespace of that Secret |

Multiple StorageClasses with distinct backing stores can coexist in a
single driver install — no manifest forking is required.

Other knobs:

- `volumeBindingMode: WaitForFirstConsumer` is **required** — fileblock
  must see the scheduler's node selection before provisioning.
- `reclaimPolicy: Delete` removes the `.img` when the PVC is deleted.
  `Retain` leaves it in place.
- `allowVolumeExpansion: true` enables offline expansion via
  `ControllerExpandVolume`. The consuming pod must be restarted for the
  resize to land (OFFLINE expansion contract).
- `fsType` is pinned to `ext4`.

## Encryption

An opt-in, per-StorageClass `encrypted: "true"` parameter wraps a
volume's `.img` in LUKS2: the node plugin layers dm-crypt between the
loop device and ext4, so the file on the backing store is only ever
ciphertext.

**What it protects:** the `.img` at rest on the backing store, and any
snapshot or backup taken of it. **What it doesn't protect:** data on
the node while the volume is staged (it's plaintext in the page cache
and via the mount), or anyone who can read the Secret holding the key.

The key comes from a Kubernetes Secret named by the standard CSI
node-stage-secret parameters, so the kubelet fetches it and hands it to
`NodeStageVolume` — the node plugin needs no new RBAC to read it. See
`examples/storageclass-encrypted.yaml` for a complete StorageClass +
Secret pair:

```yaml
parameters:
  encrypted: "true"
  csi.storage.k8s.io/node-stage-secret-name: fileblock-encryption-key
  csi.storage.k8s.io/node-stage-secret-namespace: ${pvc.namespace}
```

The node-stage secret parameters accept the same tokens as `subDir`,
with one difference: `${pvc.name}` is refused in the *namespace*
template (a PVC author must not be able to steer the node to another
namespace's Secret), while `subDir` still allows it.

| token | `subDir` | node-stage-secret-name | node-stage-secret-namespace |
|---|---|---|---|
| `${pvc.namespace}` | yes | yes | yes |
| `${pvc.name}` | yes | yes | no |
| `${pv.name}` | yes | yes | yes |

**Key rules:** the Secret's `key` must be at least 32 bytes and is used
byte-for-byte as the LUKS passphrase — no trimming, no base64 decoding.
A trailing newline from `kubectl create secret --from-file` is part of
the key.

**Rotation:** set the Secret's `previousKey` to the current key and
`key` to a new one. Each volume moves its LUKS key slot to the new key
the next time it is staged (i.e. on the consuming pod's next restart or
reschedule) — rotation does not reach an already-staged volume. Remove
`previousKey` once every volume using that Secret has restaged.

To tell when every volume has finished: the node plugin logs an Info
line with the volumeID and outcome (`formatted`, `rotated`, or
`finished-interrupted-rotation`) each time `NodeStageVolume` changes a
volume's header, so a `rotated` (or `finished-interrupted-rotation`)
line for every volume using the Secret means the rotation is done.
Without log access, `cryptsetup open --test-passphrase` against each
`.img` with the old key tells the same story: once it fails everywhere,
`previousKey` can be removed.

Removing `previousKey` before every volume has restaged, or rotating
`key` a second time while a volume is still lagging, leaves that volume
unable to open with either key in the current Secret —
`NodeStageVolume` fails `PermissionDenied`. Recovery is putting the key
that volume's header still has back into `previousKey` for one more
restage.

If a restage is interrupted between adding the new key slot and
removing the old one, and `previousKey` is then removed from the
Secret before the volume restages again, the old slot is left in the
header (harmless, but not cleaned up). Restoring `previousKey` for one
more restage removes it.

Rotation changes the key slot, not the master key: snapshots or backups
of the `.img` taken before rotation still open with the old key. After
a real compromise, don't just rotate — copy the data into a fresh
encrypted volume.

**Key backup:** keep a copy of the key outside the cluster. A lost key
is lost data.

**Recovery**, given the `.img` file and the key:

```sh
kubectl -n <ns> get secret fileblock-encryption-key \
    -o jsonpath='{.data.key}' | base64 -d \
  | sudo cryptsetup open --key-file=- /path/to/fb-….img recovered
sudo mount /dev/mapper/recovered /mnt
```

**Overhead:** the LUKS2 header takes 16 MiB inside the requested
capacity, so encrypted volumes must be at least 32 MiB — `CreateVolume`
rejects smaller requests with `OutOfRange`. Nodes need the `dm_crypt`
kernel module loaded. Existing plaintext volumes are not converted;
encryption applies only to volumes created with `encrypted: "true"`.

### Choosing a cipher

The default, `aes-xts-plain64` with a 512-bit key (AES-256), is right
wherever the CPU has AES instructions (x86 AES-NI, ARMv8 Crypto
Extensions). On CPUs without them — many ARM SoCs, older Raspberry Pi
models — AES is slow in software, and Adiantum is the kernel's answer:

```yaml
parameters:
  encrypted: "true"
  encryption.cipher: xchacha12,aes-adiantum-plain64
  encryption.keySize: "256"
```

`encryption.cipher` takes any cipher spec `cryptsetup luksFormat
--cipher` accepts (`serpent-xts-plain64`, `twofish-xts-plain64`,
`aes-cbc-essiv:sha256`, `capi:…` kernel crypto API specs, …), and
`encryption.keySize` — required whenever `encryption.cipher` is set —
is `--key-size` in bits: `256` for Adiantum, `512` for any `*-xts`
cipher (XTS splits the key in two, so 512 bits is AES-256). The key
size is never left to cryptsetup's compiled-in default, which could
change with a driver upgrade; the StorageClass alone determines how its
volumes are formatted.

**Portability.** The cipher and key size are recorded in each volume's
LUKS header when it is first formatted, and every later open reads them
from there — no node default is ever consulted, and changing the
StorageClass never affects existing volumes. What a node does need is
kernel support for the cipher: an Adiantum volume can only be staged on
nodes whose kernel has the `adiantum`, `chacha` and `nhpoly1305`
modules. Any node that may stage a volume must support its cipher, so
in a mixed cluster pick a cipher every node has. `cryptsetup benchmark
-c <cipher> -s <keySize>` on a node shows whether it is supported and
how fast it is. The controller only checks the spec's syntax; a node
without the cipher fails `NodeStageVolume` with `Internal` and
cryptsetup's error — on first stage the image is left blank, so fix the
StorageClass and recreate the PVC.

## Limitations

- **RWO only.** `ext4` has no distributed locking, so two nodes cannot
  safely mount the same image **at the same time**. fileblock advertises
  only `SINGLE_NODE_WRITER` and trusts the kubelet to enforce that — there
  is no fileblock-level cross-node lease on the `.img`.
- **Offline expand only.** Expanding the PVC truncates the image; the
  filesystem grows on the next stage (i.e. after the pod is recreated).
  The pod must be deleted and recreated to pick up the new size.
- **One pod per volume at a time.** As above.
- **Sparse, not thin.** Capacity is enforced inside the image's ext4, not
  by the backing store. You can overcommit, but a full backing store
  produces I/O errors inside pods.
- **ext4 only.** No other filesystem types are supported.
- **Privileged pods.** Both the controller and the node DaemonSet run
  `privileged: true` with `SYS_ADMIN`, matching csi-driver-nfs. The
  privilege is required for `mount.nfs` (NFSv3 in particular needs a
  privileged source port for the lock manager) and for `losetup`.
- **Encryption applies to new volumes only.** Key rotation lands on the
  next stage, not live; master-key re-encryption is not supported.

## Troubleshooting

| Symptom                                        | Fix                                                                                |
|------------------------------------------------|------------------------------------------------------------------------------------|
| `losetup: cannot find an unused loop device`   | `modprobe loop max_loop=64` (or higher) on the affected node                       |
| Stale `.img` on backing store after PVC delete | Reclaim policy may be `Retain`, or the controller failed mid-delete; remove by hand |
| Want to inspect state                          | Each node writes `/var/lib/kubelet/plugins/fileblock.csi/loop-mappings.json`        |
| One node fails every new volume `Unavailable`, peers are fine | That node's backing-store mount went away; the plugin logs `backing store is no longer mounted` and remounts on the next stage. Persisting means the remount itself is failing — check the node's connectivity to the backing store |
| `<volume> is already open on this node as /dev/mapper/fbcrypt-…` (`FailedPrecondition`) | A crashed plugin left the dm-crypt mapping open without detaching its loop device — the reconciler normally closes these on startup. Manual recovery: `umount` the stage path if still mounted, `cryptsetup close fbcrypt-<name>`, then restart the node plugin pod so the reconciler finishes cleanup |
| `not a LUKS volume and not blank; refusing to format` (`FailedPrecondition`) | The `.img`'s first 16 MiB are neither a LUKS header nor all zero, so fileblock won't overwrite it. This means the volume was created (or corrupted) outside the LUKS format-on-first-stage flow; inspect the `.img` by hand before deciding whether to discard it |
| `PermissionDenied` opening an encrypted volume | Neither `key` nor `previousKey` in the node-stage secret opens the volume's LUKS header. Check the Secret against the key this volume was last staged with; if `previousKey` was removed too early or `key` was rotated twice while this volume lagged, put its old key back in `previousKey` and restage |

## Local development without a cluster

```sh
mise install              # go, linters, and the csc / csi-sanity drivers
sudo hack/smoke.sh        # full lifecycle against a temp directory
sudo hack/csi-sanity.sh   # csi-test suite, also no cluster
```

Both run the binaries directly on unix sockets — no Docker, no kind, no
kubelet. They do need root and loop devices, so neither runs inside an
unprivileged container. Prefer `make smoke` / `make sanity` over calling
the scripts directly: those forward `PATH` through `sudo` so the
mise-provided `csc` and `csi-sanity` stay reachable as root.

## End-to-end tests against kind

`hack/e2e.sh` brings up a two-node kind cluster, builds and loads the image,
applies `deploy/kustomize/overlays/e2e`, and runs the Go suite under
`test/e2e/` (build tag `e2e`). It exercises the parts of the driver only a
real kubelet can reach: pod-level chmod / fs-type, flock semantics on the
loop-mounted ext4, offline expansion through the resizer sidecar, and
node-to-node takeover on a shared backing store.

```sh
make e2e        # plain host directory shared into both kind nodes
make e2e-nfs    # same suite, backing store over NFS (default NFSv4.1)
make e2e-nfs3   # same as e2e-nfs with NFS_VERSION=3
```

`make e2e-nfs` stands up `nfs-kernel-server` on the host, mounts the export,
and points the kind cluster at that mount — so the suite validates that
fileblock corrects the NFSv3/v4 exec-bit, chmod, and in-pod flock
pathologies described above. `make e2e-nfs3` runs the same suite with
NFSv3. See [CLAUDE.md](./CLAUDE.md) for contributor notes and harness
limitations.
