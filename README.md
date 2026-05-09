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
| `backingStore.local.path`     | when type=local   | Absolute path on every node that can read & write the store   |

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

## Troubleshooting

| Symptom                                        | Fix                                                                                |
|------------------------------------------------|------------------------------------------------------------------------------------|
| `losetup: cannot find an unused loop device`   | `modprobe loop max_loop=64` (or higher) on the affected node                       |
| Stale `.img` on backing store after PVC delete | Reclaim policy may be `Retain`, or the controller failed mid-delete; remove by hand |
| Want to inspect state                          | Each node writes `/var/lib/kubelet/plugins/fileblock.csi/loop-mappings.json`        |

## Local development without a cluster

```sh
sudo hack/smoke.sh        # full lifecycle against a temp directory
sudo hack/csi-sanity.sh   # csi-test suite, also no cluster
```

Both run the binaries directly on unix sockets — no Docker, no kind, no
kubelet.

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
