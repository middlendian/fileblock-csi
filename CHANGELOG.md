# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
- Test harness:
  - `hack/smoke.sh` — full lifecycle against a temp directory, no cluster.
  - `hack/csi-sanity.sh` — `csi-test` conformance suite, no cluster.
  - `hack/e2e.sh` — kubelet-driven end-to-end suite on a two-node kind
    cluster, with `local` and `nfs` backing-store variants.
- `make check` — single command that runs fmt, vet, lint, tidy-check, race
  test + coverage, build, smoke, and csi-sanity. CI runs the same target.

[Unreleased]: https://github.com/middlendian/fileblock-csi/compare/HEAD...HEAD
