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
NFSv3 with NLM, so the same suite verifies that fileblock corrects the
exec-bit, chmod, and flock pathologies on a real NFSv3 export — the
project's stated reason to exist. Caveat: kind nodes share the host's NFS
client, so cross-node flock is serialized in-kernel before reaching NLM.
The full cross-client NLM path needs a custom kind node image with
`nfs-common`; we have not paid that cost yet.

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

Lint config lives in `.golangci.yml`. When adding a shell-out, prefer to
unit-test the new package via `pkg/exec/exectest.FakeRunner` rather than
gating tests on root + losetup.

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
controller refuses `ControllerExpandVolume` while it is non-empty. With a
shared backing store the field may legitimately change as a pod moves
between nodes; the cross-node lease is the kernel-level `flock(2)` on the
`.img` (NFS-mediated via NLM on v3, native on v4), not this field. When
NodeStage acquires the flock and finds a stale `AttachedNode != nodeID`,
it logs a takeover warning and overwrites.

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
- `make e2e` passes locally if your change touches the kubelet path,
  StorageClass surface, sidecar arguments, or the .img/JSON contract.
- `make e2e-nfs` passes locally if your change could plausibly regress
  NFSv3 backing-store behavior (anything in `pkg/image`, `pkg/flock`, or
  `pkg/loop`'s reconciler).
- New behavior covered by a unit test in the relevant package.
- README + CLAUDE.md updated when surface changes.
