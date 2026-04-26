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

Each PV is one `${volumeId}.img` (sparse — actual disk usage grows with
real writes) plus a `${volumeId}.json` sidecar holding capacity and
metadata. Nothing else lives on the backing store.

## Requirements

- Linux nodes with `losetup`, `mount`, `umount`, `findmnt`,
  `e2fsprogs` (`mkfs.ext4`, `e2fsck`, `resize2fs`).
- The `loop` kernel module loaded. On Raspberry Pi / small ARM nodes you
  may want `modprobe loop max_loop=64` to expand the default 8-loop pool.
- A directory readable + writable from every node at the same path
  (`backingStorePath`). Common choices:
  - an NFS share mounted at `/mnt/nfs` on every node,
  - an SMB share,
  - a FUSE-backed mount,
  - or a plain local directory if you only have one node.

## Quickstart

1. **Install the driver**

   ```sh
   kubectl apply -k deploy/kustomize/overlays/example-localdir
   ```

   The example overlay uses `/var/lib/fileblock` on every node. To point at
   your own NFS / SMB / FUSE mount, copy the overlay and patch
   `backingStorePath` in the StorageClass plus the `hostPath` on the
   controller Deployment and node DaemonSet.

2. **Create a volume**

   ```sh
   kubectl apply -f examples/pvc.yaml -f examples/pod.yaml
   ```

3. **Verify**

   ```sh
   kubectl exec fileblock-demo -- sh -c 'stat -f -c %T /data; ls -l /data/hello.sh'
   # → ext2/ext3   (yes, that's what stat reports for ext4)
   # → -rwxr-xr-x  (execute bit survived)
   ```

   Drop into a shell and run `git`-style checks; the execute bit and `chmod`
   round-trip without `core.fileMode=false`.

## Configuration

`StorageClass` parameters:

| Key                | Required | Notes                                                        |
|--------------------|----------|--------------------------------------------------------------|
| `backingStorePath` | yes      | Directory the controller and every node can read & write.    |

Other knobs:

- `volumeBindingMode: WaitForFirstConsumer` is **required** — fileblock pins
  each PV to a topology segment at provisioning time, so the scheduler must
  place the pod before the volume is provisioned. By default the segment is
  the node ID, so the PV is pinned to that one node. See
  [Sharing a backing store across nodes](#sharing-a-backing-store-across-nodes)
  to make a PV usable from any node that mounts the same shared store.
- `reclaimPolicy: Delete` removes the `.img` and sidecar when the PVC is
  deleted. `Retain` leaves them in place.
- `allowVolumeExpansion: false` in v1; offline expand is supported via
  `ControllerExpandVolume` but the operator must restart the consuming pod
  for the resize to land.
- `fsType` is pinned to `ext4` in v1.

## Sharing a backing store across nodes

When the backing store is genuinely shared (one NFS export mounted at the
same path on every node, an SMB share, a FUSE-backed cluster filesystem),
you can let any node stage any volume. The driver's defense-in-depth still
applies — the OS-level `flock` held on each `.img` is what keeps two nodes
from staging the same volume at once.

Two flags control this:

| Flag (node and controller)        | Default               | Notes                                                       |
|-----------------------------------|-----------------------|-------------------------------------------------------------|
| `--topology-key`                  | `fileblock.csi/node`  | Set the same on the controller and every node DaemonSet pod |
| `--topology-value` (node only)    | `$NODE_NAME`          | Set the **same value** on every node sharing the store      |

When every node advertises an identical `(key, value)` segment, the
external-provisioner's `--strict-topology` no longer pins the PV to one
node — it pins it to that segment, and any node that advertises it is a
valid landing zone.

A worked example overlay lives at
`deploy/kustomize/overlays/example-nfs-shared/`. Edit the `nfs.server` /
`nfs.path` placeholders in `patch-controller.yaml` and `patch-node.yaml`
and apply with `kubectl apply -k`.

NFS lock-manager note: the cross-node mutual exclusion that prevents two
nodes from loop-mounting the same `.img` simultaneously relies on
`flock(2)` being honored across hosts. NFSv4 has byte-range locks built in;
NFSv3 needs the kernel's NLM (`rpc.statd` and `rpc.lockd`) running on
every node and on the server. If lock recovery after a node crash is too
slow for your workloads, prefer NFSv4.

## Limitations

- **RWO only.** `ext4` has no distributed locking, so two nodes cannot
  safely mount the same image **at the same time**. fileblock advertises
  only `SINGLE_NODE_WRITER` and additionally holds an OS-level `flock` on
  the `.img` file for the lifetime of the mount as defense-in-depth — this
  is also what serializes a pod moving between nodes when the backing
  store is shared.
- **Offline expand only.** Expanding the PVC truncates the image; the
  filesystem grows on the next stage (i.e. after the pod is recreated).
- **One pod per volume at a time.** As above.
- **Sparse, not thin.** Capacity is enforced inside the image's ext4, not
  by the backing store. You can overcommit, but a full backing store
  produces I/O errors inside pods.

## Troubleshooting

| Symptom                                        | Fix                                                                                |
|------------------------------------------------|------------------------------------------------------------------------------------|
| `losetup: cannot find an unused loop device`   | `modprobe loop max_loop=64` (or higher) on the affected node                       |
| Pod stuck `ContainerCreating`, "image locked"  | The volume is still flock'd by another node; check the previous attachment cleared |
| Stale `.img` on backing store after PVC delete | Reclaim policy may be `Retain`, or the controller failed mid-delete; remove by hand |
| Want to inspect state                          | Each node writes `/var/lib/kubelet/plugins/fileblock.csi/loop-mappings.json`        |

## Local development without a cluster

```sh
sudo hack/smoke.sh        # full lifecycle against a temp directory
sudo hack/csi-sanity.sh   # csi-test suite, also no cluster
```

Both run the binaries directly on unix sockets — no Docker, no kind, no
kubelet. See [CLAUDE.md](./CLAUDE.md) for contributor notes.
