# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- `Registry.AdoptExisting` now verifies each candidate directory is an
  actual mountpoint via `findmnt(8)` before caching it as "mounted".
  Previously, any storeID-shaped subdirectory under `--stores-root` was
  adopted unconditionally. Under the default `emptyDir`-backed stores
  volume, a container restart within the same pod (node reboot, OOM
  kill, liveness-probe failure) left the directory entry behind without
  its NFS mount; `AdoptExisting` poisoned the in-process cache; the
  next `NodeStageVolume` short-circuited on the false cache hit and
  returned `NotFound` for `.img` files that were still present on the
  NFS server. Confirmed in production on a 6-node k0s cluster after a
  multi-node reboot, 2026-05-17.
  `TestRegistryAdoptExistingSkipsNonMountedDirs` (unit) and
  `TestNodeContainerRestartRecovery` (e2e) added to guard against
  regression — the e2e variant SIGTERMs PID 1 of the fileblock-node
  container in-place (SIGKILL would be blocked by the kernel from
  within the same PID namespace) to reproduce the
  emptyDir-after-container-restart shape end-to-end against kubelet.

## [0.3.7] - 2026-05-12

### Fixed

- `CSIDriver` resource now declares `fsGroupPolicy: File`. Without an
  explicit policy, the API server defaults to `ReadWriteOnceWithFSType`,
  which tells kubelet to skip the fsGroup-based recursive chown unless
  the PV declares a `csi.fsType`. fileblock formats and mounts ext4
  internally but does not surface fsType on the PV, so the default
  policy was a no-op: freshly-provisioned volumes stayed owned by
  `root:root 0755` and non-root pods that set `securityContext.fsGroup`
  could not write to them (EACCES at the mount root). `File` tells
  kubelet to always apply fsGroup, which is the correct behavior for a
  driver that provisions one private ext4 filesystem per PV with
  `SINGLE_NODE_WRITER` access. `TestCSIDriverFsGroupPolicyIsFile` (unit)
  and `TestNonRootPodCanWriteWithFsGroup` (e2e) added to guard against
  regression.

## [0.3.6] - 2026-05-10

### Fixed

- Liveness-probe sidecar ports moved from `localhost:29652/29653`
  (matched csi-driver-nfs) to `localhost:29662/29663`. The
  csi-driver-nfs values collide for operators who run both drivers
  on the same cluster: under `hostNetwork: true`, the two node
  DaemonSets bind the same host port and one crash-loops. Real
  symptom in v0.3.5 on a cluster running both drivers — only one
  DaemonSet's node-on-host could become Ready at a time.

## [0.3.5] - 2026-05-10

### Fixed

- Controller and node `liveness-probe` sidecars now bind to distinct
  localhost ports via `--http-endpoint=localhost:29652` (controller)
  and `localhost:29653` (node). With `hostNetwork: true` (added in
  v0.3.3), the default `0.0.0.0:9808` collided on every host running
  both the controller pod and a node DaemonSet pod, leaving one
  liveness-probe in `Back-off restarting failed container`. Matches
  csi-driver-nfs's port choices. `TestLivenessProbePortsAreDistinct`
  added to `deploy/manifests_test.go` to guard against regression.

## [0.3.4] - 2026-05-10

### Fixed

- Dockerfile installs `netbase` alongside `nfs-common`. netbase
  provides `/etc/services`, `/etc/protocols`, and `/etc/rpc` — the
  name↔number lookup tables mount.nfs uses during NFSv3 mount(2).
  Without it, the kernel mount syscall returns `EPROTONOSUPPORT`,
  which mount.nfs surfaces as `Protocol not supported` even after
  successful portmapper discovery (verbose trace shows
  `trying vers=3, prot=6` followed by failure). debian:bookworm-slim
  strips netbase; nfs-common does not pull it in. csi-driver-nfs's
  Dockerfile installs netbase for the same reason. Real production
  failure on a UNAS Pro NAS in v0.3.3.

### Internal

- Dockerfile carries a build-time assertion that the netbase files
  exist (`test -s /etc/protocols && test -s /etc/services &&
  test -s /etc/rpc`). A future edit that drops netbase from the
  install line fails the build rather than shipping a regression.
- E2E workflow matrix now includes an explicit NFSv3 variant (was
  `[local, nfs]` with `nfs` defaulting to v4.1; now `[local,
  nfs (v4.1), nfs (v3)]` with `BACKING_KIND` and `NFS_VERSION` set
  per-variant). The netbase regression shipped past CI because v3
  was never exercised; v4 doesn't go through portmapper / mountd
  / `/etc/protocols` lookups, so v4-only coverage misses several
  whole code paths.

## [0.3.3] - 2026-05-10

### Fixed

- Runtime image base downgraded from `debian:trixie-slim` (nfs-utils
  2.8.3) to `debian:bookworm-slim` (nfs-utils 2.6.2). nfs-utils
  2.8.x has a NFSv3 mount regression that surfaces against some
  servers as `mount.nfs: Protocol not supported` even after
  successfully discovering both NFS (port 2049 TCP) and mountd
  ports. Real production failure on a UNAS Pro NAS in v0.3.2:
  csi-driver-nfs (running nfs-utils 2.6.2) mounted shares on the
  same NAS fine; fileblock-csi (2.8.3) failed identically on every
  share including the one csi-driver-nfs mounted successfully.
- Both controller and node pods now run with `hostNetwork: true`
  (and `dnsPolicy: ClusterFirstWithHostNet` so cluster DNS keeps
  working). Without this, the source IP of `mount.nfs` is the pod
  CIDR, and NFS server export ACLs that allow only the cluster's
  host network reject the mount with the generic "Protocol not
  supported". Matches csi-driver-nfs's controller and node pods.
  Real production failure in v0.3.2: a UNAS Pro NAS rejected
  pod-CIDR clients, leaving CreateVolume permanently failing on
  an export that worked fine when the kubelet's in-tree NFS
  plugin (host-network) accessed it.

### Internal

- `deploy/manifests_test.go` adds `TestHostNetwork` asserting both
  base manifests carry `hostNetwork: true` and the matching
  `dnsPolicy`, so a future edit can't regress it silently.

## [0.3.2] - 2026-05-09

### Fixed

- `csi-provisioner` and `csi-resizer` sidecars now run with
  `--timeout=1200s`. Default was 15s. NFSv3 `mount.nfs` first-connect
  takes longer than that (portmapper + mountd RPC roundtrips); the
  provisioner cancelled the gRPC context, the driver killed
  `mount.nfs`, and CreateVolume returned `exit -1: signal: killed`.
  Matches csi-driver-nfs.
- `pkg/store.NFSMounter` now invokes `mount -t nfs` (the generic
  mount(8) command, dispatching to mount.nfs internally) instead of
  calling `mount.nfs` directly. csi-driver-nfs goes through mount(8)
  via `k8s.io/mount-utils`, and the previous direct-`mount.nfs` path
  surfaced "Protocol not supported" failures on debian:trixie-slim
  that mount(8) does not reproduce. mount(8) does additional argument
  preprocessing before dispatching to the helper that, on this image,
  the helper requires.

### Documentation

- README now spells out the NFSv3 `nolock` requirement: fileblock
  doesn't depend on NFS-level file locks (CSI's `SINGLE_NODE_WRITER`
  + the loop device's exclusive open are what enforce cross-node
  mutual exclusion), so the client-side `rpc.statd` daemon — which
  the driver pod doesn't run — isn't needed. Without `nolock`,
  `mount.nfs` refuses with "rpc.statd is not running but is required
  for remote locking". README also recommends NFSv4 where the
  server speaks it: no NLM/statd/portmapper to negotiate.
- `deploy/kustomize/overlays/example-nfs-shared/storageclass.yaml`
  documents the v3-vs-v4 mountOptions choice inline.

### Internal

- `deploy/manifests_test.go` adds `TestSidecarTimeouts` asserting
  both sidecars carry `--timeout=1200s`, so a future edit can't
  regress it silently.

## [0.3.1] - 2026-05-09

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

[Unreleased]: https://github.com/middlendian/fileblock-csi/compare/v0.3.7...HEAD
[0.3.7]: https://github.com/middlendian/fileblock-csi/compare/v0.3.6...v0.3.7
[0.3.6]: https://github.com/middlendian/fileblock-csi/compare/v0.3.5...v0.3.6
[0.3.5]: https://github.com/middlendian/fileblock-csi/compare/v0.3.4...v0.3.5
[0.3.4]: https://github.com/middlendian/fileblock-csi/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/middlendian/fileblock-csi/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/middlendian/fileblock-csi/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/middlendian/fileblock-csi/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/middlendian/fileblock-csi/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/middlendian/fileblock-csi/releases/tag/v0.2.0
[0.1.1]: https://github.com/middlendian/fileblock-csi/releases/tag/v0.1.1
[0.1.0]: https://github.com/middlendian/fileblock-csi/releases/tag/v0.1.0
