# AdoptExisting must verify mountpoint before adopting

**Status:** Draft
**Date:** 2026-05-17
**Target version:** v0.3.8

## Problem

`Registry.AdoptExisting` at `pkg/store/registry.go:108-146` walks
`--stores-root` and inserts every immediate subdirectory whose name
matches the 12-hex-char storeID pattern into the in-process `mounted`
cache, without verifying that the directory is a live mount.
Subsequent `Get(cfg)` calls for the same storeID short-circuit on the
cached entry at `registry.go:58-63` and never trigger an actual `Mount`.

Under the `emptyDir`-backed `stores` volume (the shape the base
manifests use and the recommended cache shape per the existing doc),
this cache is poisoned after **any container restart within the same
pod**:

1. First container run mounts NFS into `<stores-root>/<storeID>`,
   creating the directory entry on the node's filesystem (the path that
   backs the emptyDir).
2. Container restarts (node reboot, OOM kill, liveness-probe failure).
   Pod UID is unchanged, so kubelet reuses the same emptyDir directory at
   `/var/lib/kubelet/pods/<pod-uid>/volumes/kubernetes.io~empty-dir/stores/`.
   The mount itself is gone (lived in the container's mount namespace)
   but the `<storeID>` directory entry persists on the node disk.
3. New container starts. `AdoptExisting` sees the `<storeID>` directory,
   adopts it. The in-process cache now claims the storeID is mounted
   when it is not.
4. `NodeStageVolume` calls `Registry.Get(cfg)`, hits the poisoned cache,
   returns the directory path without mounting NFS. `os.Stat(imgPath)`
   at `pkg/driver/node.go:127` returns `ENOENT` because NFS is not
   actually mounted at the cached path.

User-visible symptom: every `NodeStageVolume` returns
`code = NotFound desc = image …: no such file or directory` after a
fileblock-node container restart, even though the `.img` files are
present on the NFS server. The only operational recovery is forcing new
pod UIDs (e.g. `kubectl rollout restart ds`), because deleting pods is
the only way to get a fresh `emptyDir`.

The `AdoptExisting` doc block (`registry.go:108-121`) makes the
incorrect claim:

> Under emptyDir (the recommended cache shape) the directory is empty
> after restart, so this is always a no-op.

Kubernetes preserves `emptyDir` across container restarts within the
same pod — only pod-recreation wipes it. For a DaemonSet, pod-recreation
only happens on label/spec changes or explicit deletion, so any node
reboot or container crash leaves the emptyDir populated.

### Real-world incident

Confirmed on a homelab k0s cluster, 2026-05-17. All 6 worker nodes
rebooted within a ~6 hour window. Every fileblock volume across 2
namespaces (4 application pods, 1 fileblock-canary) failed to mount,
with backing `.img` files confirmed present on the NAS at the expected
paths. Direct verification inside a running fileblock-node pod showed:

```
$ mount | grep -E "fileblock|66db60478764"
/dev/sda2 on /var/lib/fileblock/stores type ext4 (rw,relatime)

$ ls -la /var/lib/fileblock/stores/66db60478764/
total 8
drwxr-xr-x 2 root root 4096 May 12 13:30 .
drwxrwxrwx 3 root root 4096 May 12 13:30 ..
```

`/var/lib/fileblock/stores` is the emptyDir on the node's root disk
(`/dev/sda2`); there is no NFS mount at the storeID subpath, and the
subpath's mtime predates the reboot by 5 days — exactly what the
hypothesis predicts. Recovery: `kubectl rollout restart ds -n
fileblock-system fileblock-node` to force new pod UIDs.

## Goals

1. **AdoptExisting only caches paths that are actually live mounts.** A
   directory that exists but is not a mount must not enter the cache.
2. **No behavior change for hostPath deployments** where the stores-root
   is a hostPath volume and mounts genuinely survive a container
   restart. Adoption still works there because the mount is real.
3. **Conservative on failure.** If we cannot prove a path is a mount, we
   do not adopt. The worst case is a redundant mount attempt on the next
   `Get`, which is safe (mount(8) is idempotent for the same
   source/target pair).
4. **Keep the fix scoped.** No changes to public CSI API, manifest
   defaults, or the broader caching/registry strategy.

## Non-goals

- Reworking the `Mounter` interface, the registry's caching strategy,
  or the controller's `ListVolumes` self-heal behavior.
- Switching manifest defaults from `emptyDir` to `hostPath`. The
  emptyDir shape is correct; the bug is in the recovery code.
- Renaming `AdoptExisting` or restructuring `pkg/store`.
- Fixing the controller-side analogue separately. Both binaries call
  the same `NewRegistry(...).AdoptExisting()`, so the fix applies
  uniformly. The controller's `Recreate` Deployment strategy already
  masks the issue in practice by giving each rollout a fresh pod UID;
  this design makes both binaries correct without special-casing
  either.

## Approach

Introduce a small interface in `pkg/store`:

```go
// MountChecker verifies whether a path is a live mountpoint. Implemented
// by pkg/mount.Mounter (which shells out to findmnt(8)); tests substitute
// a fake.
type MountChecker interface {
    IsMountPoint(ctx context.Context, target string) (bool, error)
}
```

`NewRegistry` takes the checker as a fourth parameter:

```go
func NewRegistry(root string, nfs Mounter, local Mounter, mp MountChecker) *Registry
```

`AdoptExisting` gains a `ctx context.Context` parameter so the findmnt
call can honor a startup deadline and tests can pass controlled
contexts:

```go
func (r *Registry) AdoptExisting(ctx context.Context) error
```

`*mount.Mounter` already implements `IsMountPoint(ctx context.Context,
target string) (bool, error)` with a matching signature; it satisfies
`MountChecker` without any new code. Both call sites in
`cmd/{controller,node}/main.go` already construct a `*mount.Mounter`
(`mnt := mount.New(exec)`) and pass it through as the new argument:

```go
registry := store.NewRegistry(
    *storesRoot,
    store.NewNFSMounter(exec),
    store.NewLocalMounter(mnt),
    mnt,
)
if err := registry.AdoptExisting(context.Background()); err != nil {
    log.Warn("adopt existing stores failed at startup", "err", err)
}
```

Inside `AdoptExisting`, after the existing `storeIDPattern` filter, each
candidate is verified before adoption:

```go
target := filepath.Join(r.root, id)
mounted, err := r.mp.IsMountPoint(ctx, target)
if err != nil || !mounted {
    continue
}
r.mounted[id] = target
```

Per-candidate `IsMountPoint` errors are absorbed silently inside
`AdoptExisting`. The existing call-site `log.Warn` already handles the
read-dir failure. Per-candidate failures are below that error budget:
their consequence is one redundant `mount(8)` invocation on the next
`Get`, which is safe.

The misleading doc block (`registry.go:108-121`) is rewritten to
document the new semantics: AdoptExisting only adopts directories
verified as live mounts, which makes it a true no-op under emptyDir
both when the dir is empty AND when it contains stale subdirs from a
prior container.

## Why this is safe

- The `mounted` map is read by `Get` (cache short-circuit),
  `ConfigByStoreID` (returns false if not seen via Get), and
  `MountedPaths` (used only by `ListVolumes`). With the new check,
  false-positive adoptions disappear without any compensating logic
  needed elsewhere.
- `Get` still self-heals: when a non-adopted storeID arrives, `Get`
  performs `MkdirAll` + `Mount` and populates the cache. The path is
  byte-identical to a cold-start first mount.
- `ListVolumes` semantic narrows correctly: post-restart, it returns
  volumes from actively-mounted stores only. Stores not yet seen by a
  `Get` since restart are not enumerated — same as today for stores
  that were never adopted (e.g. on a controller serving an SC for the
  first time).
- `mount(8)` idempotency: the existing `Get` already creates the target
  directory unconditionally and shells out to `mount`. Calling mount a
  second time for the same source/target pair either succeeds silently
  (the kernel treats it as a no-op for identical args) or returns a
  non-zero exit that surfaces as a normal `Get` error — exactly the
  same failure semantics as today.

## Failure modes

| Scenario | Old behavior | New behavior |
|---|---|---|
| Stale `<storeID>` dir in emptyDir, no live mount | Poisons cache; all subsequent Gets short-circuit; NodeStageVolume returns NotFound | Skipped; next Get does a real mount; NodeStageVolume succeeds |
| Live mount under hostPath that survived a container restart | Adopted (correct) | Adopted (still correct — IsMountPoint returns true) |
| `findmnt` unavailable on PATH | Adoption succeeds (wrong) | Skipped (conservative); next Get does a real mount and reports its own error if needed |
| `IsMountPoint` errors on one candidate | Adoption succeeds for adjacent entries | Per-entry skip; adjacent entries continue to be checked |
| stores-root unreadable | Returns `read stores root … : <err>` | Same — unchanged |

## Test plan

All additions in `pkg/store/registry_test.go`. Use the existing
`exectest.FakeRunner` to fake `findmnt` invocations — no kernel
interaction, no root needed. `Mounter.IsMountPoint` issues two findmnt
calls (`-n -o TARGET --target <path>` then `-n -o TARGET <path>`); the
fake rule set must account for both.

1. **`TestRegistryAdoptExistingSkipsNonMountedDirs`** — create a
   `<storeID>`-named dir under `t.TempDir()`; configure
   `fake.Set("findmnt", "", &fbexec.Error{ExitCode: 1})` (the
   `findmnt: <path>: not a mountpoint` case) and a separate
   `fake.Set("mount", "", nil)` so the follow-up `Get(cfg)` succeeds.
   Assert: `MountedPaths()` is empty after `AdoptExisting`. Then call
   `Get(cfg)` for the same cfg and assert exactly one `mount` call was
   issued (cache miss path).
2. **`TestRegistryAdoptExistingAdoptsMountedDirs`** — create a
   `<storeID>` dir; `fake.Set("findmnt", filepath.Join(root,
   "<storeID>"), nil)` covers both findmnt invocations in
   `Mounter.IsMountPoint` (same trick as note 4 below). Assert: the
   storeID appears in `MountedPaths()`. Then `Get(cfg)` issues zero
   `mount` calls (cache hit path).
3. **`TestRegistryAdoptExistingSkipsOnCheckError`** — `fake.Set(
   "findmnt", "", &fbexec.Error{ExitCode: 2})` (something other than
   the "not a mount" sentinel exit 1), plus `fake.Set("mount", "",
   nil)` for the follow-up `Get`. Assert: the storeID is not adopted;
   `AdoptExisting` itself still returns `nil`. Then `Get(cfg)` issues
   exactly one `mount` call.
4. **Update `TestRegistryAdoptExistingPreloadsKnownDirs`** to set fake
   findmnt rules such that `IsMountPoint` returns `true` for the
   `abc123def456` dir: a single `fake.Set("findmnt",
   filepath.Join(root, "abc123def456"), nil)` rule covers both findmnt
   invocations in `Mounter.IsMountPoint` (see test-plan notes below).
5. **Update `TestRegistryAdoptExistingNoOpOnEmptyRoot`** to construct
   the registry with a `MountChecker` (any return value; no entries
   reach the check).
6. **Update `TestRegistryAdoptExistingSkipsNonStoreIDDirs`** with fake
   findmnt rules returning the path of the one valid storeID dir
   (`0123456789ab`) — its positive-adoption assertion still has to hold.

### Test-plan notes for the implementer

- `exectest.FakeRunner.Set(name, out, err)` matches by command name
  only. `Mounter.IsMountPoint` issues two `findmnt` calls (one with
  `--target`, one without); a single `Set("findmnt", ...)` rule covers
  both, because the first call's output is discarded inside
  `IsMountPoint` and only the second is compared to `target`. Reach for
  `FakeRunner.Func` only if a test needs to differentiate between the
  two arg sets.
- Tests asserting non-exit-1 error handling will need to import
  `"github.com/middlendian/fileblock-csi/pkg/exec"` (alias `fbexec`)
  directly — `pkg/store/registry_test.go` currently imports `exectest`
  only.
- All existing `TestRegistry*` tests that call `NewRegistry` must be
  updated to pass the new `MountChecker` parameter — most can pass the
  same `mount.New(fake)` they already construct for the `LocalMounter`.

`AdoptExisting`'s new `ctx` parameter requires updating the two call
sites in `cmd/controller/main.go` and `cmd/node/main.go` to pass
`context.Background()`.

### Locking note

`AdoptExisting` currently holds `r.mu.Lock()` for the entire
read-dir loop (registry.go:130-144). With the new check this means the
global mutex is held across N `findmnt` shell-outs. This is acceptable
because `AdoptExisting` runs once at startup, before the gRPC server
starts serving CSI requests — there is no concurrent traffic. If that
contract ever changes, restructure to gather candidates under the
lock, release, run `IsMountPoint` for each, then re-acquire briefly to
commit the verified entries.

No e2e changes. Per the `CLAUDE.md` pre-merge checklist this change
does not touch the kubelet path, the .img/JSON contract, or
`pkg/loop/reconcile.go`. `make check` (fmt + vet + lint + tidy + test +
build + smoke + sanity) is sufficient.

## CHANGELOG

Add under `[Unreleased] → Fixed`:

> `Registry.AdoptExisting` now verifies each candidate directory is an
> actual mountpoint via `findmnt(8)` before caching it as "mounted".
> Previously, any storeID-shaped subdirectory under `--stores-root` was
> adopted unconditionally. Under the default `emptyDir`-backed stores
> volume, a container restart within the same pod (node reboot, OOM
> kill, liveness-probe failure) left the directory entry behind
> without its NFS mount; `AdoptExisting` poisoned the in-process cache;
> the next `NodeStageVolume` short-circuited on the false cache hit and
> returned `NotFound` for `.img` files that were still present on the
> NFS server. Confirmed in production on a 6-node k0s cluster after a
> multi-node reboot, 2026-05-17.
> `TestRegistryAdoptExistingSkipsNonMountedDirs` (unit) added to guard
> against regression.

## Release

Standard `cut-release` workflow: `Actions → Cut release → Run workflow`
with `version: v0.3.8`. CHANGELOG-promotion, tag, and image build are
automated by `tag-and-release.yml` + `release.yml`.

## Open questions

None.
