# CLAUDE.md — fileblock-csi contributor / agent guide

## Project goal

fileblock is a Kubernetes CSI driver that gives pods a real ext4 block device
backed by a sparse `.img` file on shared storage (NFS, SMB, FUSE, or a plain
directory). It exists to fix the NFSv3 execute-bit and POSIX-mode loss that
breaks `git`, `chmod`, and file locking inside pods.

## Repo map

```
cmd/controller/         CSI controller plugin entrypoint
cmd/node/               CSI node plugin entrypoint
pkg/driver/             Identity / Controller / Node CSI gRPC servers + bootstrap
pkg/image/              .img + sidecar JSON CRUD; ext4 fsck + resize2fs helpers
pkg/loop/               losetup wrappers, loop-mappings.json state file, reconciler
pkg/mount/              mount / bind / unmount / findmnt wrappers
pkg/flock/              advisory lock around an .img file
pkg/exec/               os/exec wrapper with timeout + structured Error
deploy/kustomize/       base manifests + example-localdir overlay
examples/               PVC + Pod manifests (no overlay-specific values)
hack/                   smoke.sh, csi-sanity.sh
Dockerfile              single multi-stage image used by both binaries
```

## Build / test commands

```sh
go build ./...
go vet ./...
go test ./...

sudo hack/smoke.sh         # local end-to-end, no cluster
sudo hack/csi-sanity.sh    # csi-test sanity suite, no cluster
```

The smoke and sanity scripts must run as root (loop devices and mount(8)).
They use plain temp directories — no kind, no kubelet.

## On-disk contract

`pkg/image` is the **only** package that writes to the backing store. Every
volume is a pair of files in `${backingStorePath}`:

```
fb-<uuid>.img      sparse ext4 image
fb-<uuid>.json     {volumeId, capacityBytes, fsType, createdAt, attachedNode}
```

The sidecar is written atomically (write to `.tmp`, fsync, rename). Reads are
lock-free.

`AttachedNode` is updated by the node plugin from inside the flock-guarded
section of `NodeStageVolume` and cleared in `NodeUnstageVolume`. The
controller refuses `ControllerExpandVolume` while it is non-empty.

## State file invariants (`pkg/loop`)

`/var/lib/kubelet/plugins/fileblock.csi/loop-mappings.json` is a JSON map
`volumeID -> {LoopDev, ImagePath, StagePath}`. Invariants:

1. Every entry must correspond to a `losetup --json --list` row whose
   `back-file` matches `ImagePath`. Otherwise the reconciler drops it.
2. Every loop device backed by a `.img` under our `backingStorePath` and
   *not* present in the state file gets detached on plugin start.
3. Concurrent in-process Stage/Unstage on the same volume is serialized by
   `NodeServer.lockVolume`.
4. The OS-level `flock` on the `.img` is held for the lifetime of the stage
   in `NodeServer.fdByVolume[volumeID]`. It is NOT persisted; on restart the
   kernel releases the flock and the reconciler detaches the orphan loop.

## Conventions

- Every shell-out goes through `pkg/exec.Runner` so it can be faked in tests.
- gRPC handlers map errors to canonical codes via
  `google.golang.org/grpc/status`. `image.CapacityMismatchError` →
  `AlreadyExists`; `image.VolumeInUseError` → `FailedPrecondition`;
  `loop.ErrPoolExhausted` → `ResourceExhausted`.
- Never `panic` in a handler. Return a `status.Error` and let the caller
  retry.
- One short comment per non-obvious block. No multi-line comment headers, no
  re-stating identifier names.
- Linux-only system calls (`unix.Flock`, `unix.Statfs`) are imported via
  `golang.org/x/sys/unix` and live in files that already require Linux
  semantically. We don't currently need build tags because nothing builds on
  non-Linux in CI.

## CSI surface (advertised)

- Identity: `CONTROLLER_SERVICE`, `VOLUME_ACCESSIBILITY_CONSTRAINTS`,
  `VolumeExpansion=OFFLINE`.
- Controller: `CREATE_DELETE_VOLUME`, `EXPAND_VOLUME`, `LIST_VOLUMES`.
- Node: `STAGE_UNSTAGE_VOLUME`, `GET_VOLUME_STATS`, `EXPAND_VOLUME`.
- Access modes: `SINGLE_NODE_WRITER` only.
- fsType: `ext4` only.

If you add a capability, also update `controller.go::ControllerGetCapabilities`
or `node.go::NodeGetCapabilities` AND the README's *Limitations* section.

## Pre-merge checklist

Mirrors the validation checklist in the project plan. Before merging:

- `go build ./...` and `go vet ./...` clean.
- `sudo hack/smoke.sh` passes locally.
- `sudo hack/csi-sanity.sh` passes locally.
- New behavior covered by a unit test in the relevant package.
- README + CLAUDE.md updated when surface changes.
