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
pkg/exec/               os/exec wrapper with timeout + structured Error
deploy/kustomize/       base manifests + example-localdir, example-nfs-shared, e2e overlays
examples/               PVC + Pod manifests (no overlay-specific values)
test/e2e/               kubelet-driven end-to-end tests (build tag: e2e)
hack/                   smoke.sh, csi-sanity.sh, e2e.sh, e2e-kind.yaml
Dockerfile              single multi-stage image used by both binaries
```

## Build / test commands

The Makefile is the single source of truth. CI runs the same targets, so
running them locally is the fastest way to confirm a change will pass.

```sh
make build          # compile both binaries into ./bin
make test           # go test ./...
make test-race      # go test -race ./...
make cover          # coverage profile in cover.out
make vet            # go vet
make fmt-check      # fail if any file isn't gofmt-clean (CI gate)
make fmt            # gofmt -s -w in place
make lint           # golangci-lint run
make tidy           # go mod tidy
make tidy-check     # fail if go.mod/go.sum need tidying (CI gate)
make check          # full CI gate (fmt+vet+lint+tidy+cover+build+smoke+sanity)
make smoke          # sudo hack/smoke.sh
make sanity         # sudo hack/csi-sanity.sh
make e2e            # kind + go test ./test/e2e (local backing store)
make e2e-nfs        # kind + go test ./test/e2e (NFSv3 backing store)
make docker         # docker build the runtime image
make all            # fmt-check + vet + lint + test + build
```

**Run `make check` before every `git push`.** It is *literally* what
ci.yml runs — one job, one command — covering fmt, vet, lint, tidy,
race-enabled test + coverage, build, smoke, and csi-sanity. Skipping
it has burnt several CI runs already. Because smoke and sanity are
included, `make check` needs root, loop devices, `csc`, and
`csi-sanity` on the PATH (see the smoke/sanity prereqs); on machines
where those aren't available, run the lighter gates individually
(`make fmt-check vet lint tidy-check test build`) before pushing.

The smoke and sanity scripts must run as root (loop devices and mount(8)).
They use plain temp directories — no kind, no kubelet.

The e2e suite is the only layer that drives kubelet directly. It boots a
two-node kind cluster with a shared backing store, applies the `e2e` overlay,
and runs the Go tests under `test/e2e/` (build tag `e2e` so they don't run
under `make test`). `make e2e-nfs` further mounts the backing store over
NFSv3, so the same suite verifies that fileblock corrects the exec-bit,
chmod, and in-pod flock pathologies on a real NFSv3 export — the project's
stated reason to exist.

## CI

GitHub Actions workflows live in `.github/workflows/`:

- `ci.yml` runs on every push and PR: fmt-check, vet, golangci-lint
  (config in `.golangci.yml`), race-enabled `go test ./...` with
  coverage, `go mod tidy` verification, and a container build.
- `integration.yml` runs `hack/smoke.sh` and `hack/csi-sanity.sh` on
  push to `main` and via workflow_dispatch.
- `e2e.yml` runs `hack/e2e.sh` in a `local` and an `nfs` matrix variant on
  push to `main` and via workflow_dispatch. Each variant boots a kind
  cluster, deploys the e2e overlay, and runs `go test -tags=e2e
  ./test/e2e/...`.
- `release.yml` runs on `v*` tag pushes. It logs into `ghcr.io`, extracts
  the matching section from `CHANGELOG.md` as the GitHub release body,
  then runs GoReleaser (config in `.goreleaser.yaml`) to publish
  multi-arch images and create the GitHub release.

Lint config lives in `.golangci.yml`. When adding a shell-out, prefer to
unit-test the new package via `pkg/exec/exectest.FakeRunner` rather than
gating tests on root + losetup.

## Releases

A `v*` tag on any commit fires `.github/workflows/release.yml`, which has
three jobs running in parallel where possible:

1. **`image` (matrix)** — one job per arch on a native runner
   (`ubuntu-latest` for `linux/amd64`, `ubuntu-24.04-arm` for `linux/arm64`).
   Each runs `docker/build-push-action` against the existing top-level
   `Dockerfile`, builds the binaries inside the container natively (no
   QEMU), and pushes a per-arch tag like
   `ghcr.io/middlendian/fileblock-csi:vX.Y.Z-amd64`.
2. **`manifest`** — `needs: image`. `docker buildx imagetools create`
   combines the two per-arch tags into the multi-arch manifest at
   `ghcr.io/middlendian/fileblock-csi:vX.Y.Z`, and (for non-prerelease
   tags only — anything without `-` in the tag) tags `:latest` the same
   way.
3. **`release`** — runs in parallel with `image`. Extracts the
   `## [X.Y.Z]` section from `CHANGELOG.md` into `release-notes.md`, then
   runs [GoReleaser](https://goreleaser.com/) (`.goreleaser.yaml`) to
   build `cmd/controller` and `cmd/node` binaries for both linux arches
   (with `pkg/driver.Version` set via `-ldflags`), package them as
   `fileblock-csi_X.Y.Z_linux_{amd64,arm64}.tar.gz`, and create the
   GitHub release with those archives attached and the CHANGELOG section
   as the body.

GoReleaser owns binaries + GitHub release; the workflow owns container
images. Splitting it that way is what lets us keep the image build native
on each arch — GoReleaser's own `dockers:` integration assumes a single
runner with QEMU.

### Cutting a release

The `CHANGELOG.md` follows
[Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/). Every
user-visible change lands under `## [Unreleased]` as it's merged.

`deploy/kustomize/base/kustomization.yaml` carries an `images:` override
whose `newTag` tracks the ref:
- on `main` it is `latest`,
- on tag `vX.Y.Z` it is `vX.Y.Z`.

This is what makes `kubectl apply -k
github.com/middlendian/fileblock-csi/deploy/kustomize/base?ref=vX.Y.Z`
install the matching image. `hack/cut-release.sh` is responsible for
flipping the tag on the release commit and back to `latest` after.

To cut `vX.Y.Z`:

1. Edit `CHANGELOG.md`: rename `## [Unreleased]` to
   `## [X.Y.Z] - YYYY-MM-DD`, add a fresh empty `## [Unreleased]` above
   it, and update the `[Unreleased]` / `[X.Y.Z]` link references at the
   bottom. Don't commit yet.
2. From a clean `main` synced with origin, run
   `make release VERSION=vX.Y.Z`. The script:
   - Validates the CHANGELOG section exists and the kustomization is
     currently at `newTag: latest`.
   - Bumps base kustomization `newTag` to `vX.Y.Z`, commits the
     CHANGELOG promotion + bump as `release: vX.Y.Z`, and tags that
     commit.
   - Bumps `newTag` back to `latest` and commits
     `deploy: bump kustomization back to :latest after vX.Y.Z`.
3. Review `git log --oneline -3` and the new tag; push with
   `git push origin main vX.Y.Z`.
4. Watch `release.yml` in Actions. When it goes green, the multi-arch
   image is at `ghcr.io/middlendian/fileblock-csi:vX.Y.Z` (and
   `:latest` for non-prereleases), and the GitHub release is published
   with the CHANGELOG section as its body.

If the `release` job fails with `No CHANGELOG entry for vX.Y.Z`, you
forgot step 1 — the workflow refuses to publish a release whose notes
would be empty. Fix the CHANGELOG on `main` (a follow-up commit is
fine), delete and re-create the tag at the new commit, and push.

### Local dry run

`make release-snapshot` runs `goreleaser release --snapshot --clean
--skip=publish` against a fake `vX.Y.Z-SNAPSHOT-<sha>` tag — exercises
the binary-and-archive path without pushing or hitting GitHub. The image
build path is exercised by plain `make docker` against the top-level
`Dockerfile`.

## On-disk contract

`pkg/image` is the **only** package that writes to the backing store. Every
volume is a pair of files in `${backingStorePath}`:

```
fb-<uuid>.img      sparse ext4 image
fb-<uuid>.json     {volumeId, capacityBytes, fsType, createdAt}
```

The sidecar is written atomically (write to `.tmp`, fsync, rename). Reads are
lock-free.

Cross-node mutual exclusion is the kubelet's job. fileblock advertises only
`SINGLE_NODE_WRITER`, so the CSI attach/detach controller serializes
`NodeStageVolume` per volume — there is no fileblock-level lease on the
`.img`. `ControllerExpandVolume` is OFFLINE: the external-resizer holds it
back until the volume is unpublished, so the controller can safely truncate
without a separate "in use" check.

## State file invariants (`pkg/loop`)

`/var/lib/kubelet/plugins/fileblock.csi/loop-mappings.json` is a JSON map
`volumeID -> {LoopDev, ImagePath, StagePath}`. Invariants:

1. Every entry must correspond to a `losetup --json --list` row whose
   `back-file` matches `ImagePath`. Otherwise the reconciler drops it.
2. Every loop device backed by a `.img` under our `backingStorePath` and
   *not* present in the state file gets detached on plugin start.
3. Concurrent in-process Stage/Unstage on the same volume is serialized by
   `NodeServer.lockVolume`.

## Conventions

- Every shell-out goes through `pkg/exec.Runner` so it can be faked in tests.
- gRPC handlers map errors to canonical codes via
  `google.golang.org/grpc/status`. `image.CapacityMismatchError` →
  `AlreadyExists`; `loop.ErrPoolExhausted` → `ResourceExhausted`.
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
- `make e2e` passes locally if your change touches the kubelet path,
  StorageClass surface, sidecar arguments, or the .img/JSON contract.
- `make e2e-nfs` passes locally if your change could plausibly regress
  NFSv3 backing-store behavior (anything in `pkg/image` or `pkg/loop`'s
  reconciler).
- New behavior covered by a unit test in the relevant package.
- README + CLAUDE.md updated when surface changes.
