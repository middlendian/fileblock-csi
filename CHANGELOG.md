# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- Controller pod now runs `privileged: true` (in addition to its
  pre-existing `SYS_ADMIN` capability). Without this, `mount.nfs`
  inside the controller hangs and is killed by the driver's exec
  timeout when the SC uses `nfsvers=3`: NFSv3's lock manager binds a
  privileged source port, which the LSM rejects under
  SYS_ADMIN-without-privileged on most production hosts. Real
  symptom in v0.3.0 was a stuck `Pending` PVC with controller logs
  showing `mount.nfs ...: exit -1: signal: killed`. The change
  matches csi-driver-nfs's controller pod and our own node
  DaemonSet's pre-existing posture.

### Security

- Both controller and node containers now also `drop: [ALL]` caps,
  so `SYS_ADMIN` is the only capability either retains. Mirrors
  csi-driver-nfs.

### Internal

- `deploy/manifests_test.go` asserts both base manifests carry the
  canonical `privileged: true` + `add: [SYS_ADMIN]` + `drop: [ALL]`
  block in order, so a future edit can't regress it silently.

## [0.3.0] - 2026-05-09

### Breaking

- StorageClass schema changed. The `backingStorePath` parameter is removed.
  New required parameters: `backingStore.type` (`nfs` | `local`),
  `backingStore.nfs.server`, `backingStore.nfs.path`,
  `backingStore.nfs.mountOptions` (optional), `backingStore.local.path`.
  See README and `deploy/kustomize/overlays/example-{nfs-shared,localdir}`
  for examples.
- Binary flags removed: `--backing-store`, `--topology-key`,
  `--topology-value`. Both binaries now accept `--stores-root`
  (default `/var/lib/fileblock/stores`).
- Controller pod requires `SYS_ADMIN` capability (added by base
  manifests). `privileged: true` is a tested fallback if `SYS_ADMIN`
  alone is rejected by the host's LSM. (Operators using NFSv3 need
  this fallback; see v0.3.1 Fixed where `privileged: true` becomes
  the default.)
- Container image now includes `nfs-common` for in-driver NFS
  mounting (NFSv3 and NFSv4 both supported via the generic
  `mount.nfs` helper).
- `external-provisioner` sidecar no longer runs with
  `--strict-topology`. NFS-backed PVs are universally schedulable;
  local-backed PVs pin to the provisioner's preferred node.
- `ListVolumes` no longer returns per-volume `VolumeContext`
  (operationally low-impact; consumers that depended on it must
  read PV state via the Kubernetes API).
- v0.2.0 PVs cannot be staged by v0.3.0 (volume context shape
  changed). Migration runbook in the design spec
  (`docs/superpowers/specs/2026-05-09-storageclass-driven-config-design.md`).

### Added

- `pkg/store` package: `Config`, `ID`, `Mounter` interface with
  NFS and local impls, `Registry` for per-process mount caching.
- Two SCs against distinct backing stores work in a single driver
  install — no manifest forking required.
- e2e matrix covers NFSv3 and NFSv4.1 against the driver-mounted
  store; new `TestTwoStores` exercises multi-store scheduling.

### Internal

- `pkg/loop`'s reconciler now takes `/var/lib/fileblock/stores` as
  its `backingStorePath` (parent of all per-store dirs); orphan-loop
  cleanup spans every store. No code change inside `pkg/loop`.

## [0.2.0] - 2026-05-08

### Changed

- `CreateVolume` is now strictly idempotent against the on-disk `.img`:
  if the file exists at the requested size it is adopted as-is, never
  re-`mkfs`'d. Mismatched on-disk size returns `AlreadyExists`. A corrupt
  or otherwise unusable `.img` surfaces at `NodeStageVolume`'s `e2fsck`
  step as a mount error rather than being silently overwritten.
- Dropped the `fb-<uuid>.json` sidecar. The `.img` is the single source of
  truth — capacity is `os.Stat().Size()`. Existing sidecars on upgraded
  deployments are left in place; they are inert and removed on next
  `DeleteVolume`.

## [0.1.1] - 2026-05-08

### Changed

- Release pipeline is now PR-driven. Cutting a release opens a
  `release/vX.Y.Z` PR; squash-merging it on `main` automatically
  creates the `vX.Y.Z` tag and runs the publish pipeline through a
  chained reusable workflow. No user-visible changes to the published
  binaries or images.

## [0.1.0] - 2026-05-08

### Added

- Initial CSI driver implementation: controller and node plugins backed by sparse
  `ext4` `.img` files on a shared directory (NFS, SMB, FUSE, or a plain local
  directory). Each volume is a single `fb-<uuid>.img` plus a `fb-<uuid>.json`
  sidecar holding capacity and metadata.
- `StorageClass` parameter `backingStorePath` to point the controller and every
  node at the shared directory.
- Topology-aware provisioning. By default, every node advertises a unique
  `fileblock.csi/node` segment so each PV is pinned to one node. Setting
  `--topology-key` and `--topology-value` to a shared value across nodes lets
  any of them stage volumes from a genuinely shared backing store.
- CSI surface advertised:
  - Identity: `CONTROLLER_SERVICE`, `VOLUME_ACCESSIBILITY_CONSTRAINTS`,
    `VolumeExpansion=OFFLINE`.
  - Controller: `CREATE_DELETE_VOLUME`, `EXPAND_VOLUME`, `LIST_VOLUMES`.
  - Node: `STAGE_UNSTAGE_VOLUME`, `GET_VOLUME_STATS`, `EXPAND_VOLUME`.
  - Access mode: `SINGLE_NODE_WRITER` only.
  - `fsType`: `ext4` only.
- Loop-device reconciler that cleans up orphaned loop attachments backed by
  files under `backingStorePath` on plugin start, and serializes per-volume
  Stage/Unstage in-process.
- `kubectl apply -k`-able overlays under `deploy/kustomize/`:
  - `base/` — controller `Deployment` + node `DaemonSet` with the standard
    sidecar set (external-provisioner, external-resizer, livenessprobe,
    node-driver-registrar).
  - `overlays/example-localdir/` — single-node example using
    `/var/lib/fileblock`.
  - `overlays/example-nfs-shared/` — shared NFSv3 backing store with worked
    placeholders for `nfs.server` / `nfs.path`.
- Container image build (`Dockerfile`) carrying both binaries on a
  `debian:bookworm-slim` base with `e2fsprogs` and `util-linux` for
  `mkfs.ext4`, `e2fsck`, `resize2fs`, `losetup`, `mount`, `umount`, and
  `findmnt`.
- Tag-driven release pipeline. Pushing a `v*` tag publishes a multi-arch
  (`linux/amd64`, `linux/arm64`) image to
  `ghcr.io/middlendian/fileblock-csi`, built natively on each arch, and
  creates a GitHub Release with notes drawn from this file.
- Install-by-ref: `deploy/kustomize/base/kustomization.yaml` carries an
  image tag that tracks the git ref — `latest` on `main`, `vX.Y.Z` on
  release tags. So
  `kubectl apply -k 'github.com/middlendian/fileblock-csi/deploy/kustomize/overlays/example-localdir?ref=v0.1.0'`
  installs `:v0.1.0`, and the same URL with `?ref=main` installs `:latest`.
- Test harness:
  - `hack/smoke.sh` — full lifecycle against a temp directory, no cluster.
  - `hack/csi-sanity.sh` — `csi-test` conformance suite, no cluster.
  - `hack/e2e.sh` — kubelet-driven end-to-end suite on a two-node kind
    cluster, with `local` and `nfs` backing-store variants.
- `make check` — single command that runs fmt, vet, lint, tidy-check, race
  test + coverage, build, smoke, and csi-sanity. CI runs the same target.

[Unreleased]: https://github.com/middlendian/fileblock-csi/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/middlendian/fileblock-csi/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/middlendian/fileblock-csi/releases/tag/v0.2.0
[0.1.1]: https://github.com/middlendian/fileblock-csi/releases/tag/v0.1.1
[0.1.0]: https://github.com/middlendian/fileblock-csi/releases/tag/v0.1.0
