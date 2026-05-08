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
- `tag-and-release.yml` runs on every push to `main`. When the head
  commit's subject matches `release: vX.Y.Z` (the squash form of a
  `release/vX.Y.Z` PR), it creates the `vX.Y.Z` tag and calls the
  reusable `release.yml`. Other pushes no-op.
- `release.yml` is a reusable workflow (`workflow_call`) that takes a
  `version` input. It builds the multi-arch image, combines the
  manifest, extracts the `[X.Y.Z]` CHANGELOG section, and creates the
  GitHub release. Invoked only by `tag-and-release.yml`; not triggered
  by raw tag pushes (humans can't push `v*` tags — see the tag
  protection ruleset).

Lint config lives in `.golangci.yml`. When adding a shell-out, prefer to
unit-test the new package via `pkg/exec/exectest.FakeRunner` rather than
gating tests on root + losetup.

## Releases

Releases are PR-driven. The maintainer opens a `release/vX.Y.Z` PR via
`hack/cut-release.sh`; merging it to `main` (squash) triggers
`tag-and-release.yml`, which creates the tag and calls `release.yml` to
build and publish.

### How a release happens, end-to-end

1. **`hack/cut-release.sh vX.Y.Z`** (or `make release VERSION=vX.Y.Z`)
   creates a `release/vX.Y.Z` branch with one commit on it
   (CHANGELOG promotion + base kustomization `newTag` bump), subject
   `release: vX.Y.Z`, and opens a PR.
2. **Merge the PR.** Either squash-merge or "create a merge commit"
   works — both leave a `release: vX.Y.Z` line in the merge commit
   message (squash puts it in the subject, merge-commit puts it in the
   body), and `detect` scans the full message.
3. **`tag-and-release.yml`** fires on the push to `main`:
   - `detect` job scans the head commit message for a
     `release: vX.Y.Z` line. If it doesn't find one, the workflow skips
     the rest. The job also accepts a `workflow_dispatch` with an
     explicit `version` input — escape hatch for re-running a release
     manually if a CI flake aborted publish.
   - `tag` job creates the `vX.Y.Z` tag (using `GITHUB_TOKEN`, allowed by
     the `v*` tag-protection ruleset for `github-actions[bot]`) and pushes
     it.
   - `release` job calls `release.yml` as a reusable workflow with
     `version: vX.Y.Z`.
4. **`release.yml`** (reusable, `workflow_call`):
   - `image` matrix builds `linux/amd64` on `ubuntu-latest` and
     `linux/arm64` on `ubuntu-24.04-arm`, pushing per-arch tags
     `:vX.Y.Z-amd64` and `:vX.Y.Z-arm64` to GHCR. Native, no QEMU.
   - `manifest` combines the two via `docker buildx imagetools create`
     into the multi-arch tag `:vX.Y.Z`, and (for non-prerelease tags
     only) `:latest`.
   - `release` extracts the `[X.Y.Z]` section from `CHANGELOG.md` into
     `$RUNNER_TEMP/release-notes.md` and runs GoReleaser to publish the
     binary tarballs and the GitHub release with that body, marked
     latest.

Why the chained-workflow shape: the `v*` tag-protection ruleset blocks
human tag pushes, and `GITHUB_TOKEN` pushes don't trigger other workflows
(anti-recursion). Calling `release.yml` directly via `workflow_call` from
the same workflow that creates the tag sidesteps both constraints
without needing a PAT or GitHub App.

### Image-tag policy

`deploy/kustomize/base/kustomization.yaml` carries an `images:` override
whose `newTag` is the **most recent released version**.
`hack/cut-release.sh` bumps it from the previous release's tag to
`vX.Y.Z` as part of the release commit. So:

- `kubectl apply -k 'github.com/middlendian/fileblock-csi/deploy/kustomize/base?ref=v0.1.0'`
  resolves to `:v0.1.0` (immutable, the v0.1.0 source pinned that tag).
- `kubectl apply -k 'github.com/middlendian/fileblock-csi/deploy/kustomize/base?ref=main'`
  resolves to whatever the latest release was at `main`'s current tip.

People who want bleeding-edge use the `:dev` image (`make docker`), not
`main`.

### Cutting `vX.Y.Z`

1. Edit `CHANGELOG.md`: rename `## [Unreleased]` to
   `## [X.Y.Z] - YYYY-MM-DD`, add a fresh empty `## [Unreleased]` above
   it, and update the `[Unreleased]` / `[X.Y.Z]` link references at the
   bottom. Don't commit yet.
2. From a clean `main` synced with origin, run
   `make release VERSION=vX.Y.Z` (or `hack/cut-release.sh vX.Y.Z`). The
   script verifies pre-conditions, bumps `newTag`, commits, pushes the
   `release/vX.Y.Z` branch, and opens a PR.
3. Review the PR. Squash-merge with the default subject
   `release: vX.Y.Z`.
4. Watch the Actions tab — `tag-and-release.yml` fires, then
   `release.yml`. When green, the multi-arch image is at
   `ghcr.io/middlendian/fileblock-csi:vX.Y.Z` (and `:latest` for
   non-prereleases) and the GitHub release is published.

If the `release` job fails with `No CHANGELOG entry for vX.Y.Z`, the
CHANGELOG promotion in step 1 was wrong (typo, wrong date format).
Fix CHANGELOG on `main` via a follow-up PR, delete the broken tag
(`gh api -X DELETE repos/middlendian/fileblock-csi/git/refs/tags/vX.Y.Z`),
and re-trigger the release by re-opening and re-merging a release PR
(or via `gh workflow run tag-and-release.yml` with a fresh release
commit on `main`).

### Repo configuration this assumes

These rules live in the GitHub repo settings (not in this repo's tree),
and the release flow above relies on them:

- **`main` branch protection**: pull request required to push. Already
  in place — that's why `cut-release.sh` opens a PR.
- **`v*` tag protection ruleset**: tag creation/update/delete blocked
  for everyone except `github-actions[bot]`. This is what stops humans
  from pushing tags directly; the `tag` job in `tag-and-release.yml` is
  the only thing allowed to.

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
