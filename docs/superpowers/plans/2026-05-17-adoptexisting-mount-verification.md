# AdoptExisting Mountpoint Verification Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix `Registry.AdoptExisting` cache poisoning under `emptyDir`-after-container-restart by verifying each candidate directory is a live mountpoint (via `findmnt(8)`) before adopting it.

**Architecture:** Define a small `MountChecker` interface in `pkg/store`. Add a fourth parameter to `NewRegistry` that takes a `MountChecker`. Add a `ctx context.Context` parameter to `AdoptExisting`. Use the existing `*mount.Mounter` (already constructed in both `cmd/*/main.go` entrypoints) as the implementation — it already exposes `IsMountPoint(ctx, target) (bool, error)` with the exact signature.

**Tech Stack:** Go 1.22+, Kubernetes CSI spec, `findmnt(8)` from util-linux, `pkg/exec/exectest.FakeRunner` for unit tests.

**Spec:** [`docs/superpowers/specs/2026-05-17-adoptexisting-mount-verification-design.md`](../specs/2026-05-17-adoptexisting-mount-verification-design.md)

**PR:** https://github.com/middlendian/fileblock-csi/pull/34

**Branch:** `fix/adoptexisting-mountpoint-verify`

---

## Pre-flight

- [ ] Confirm you are in `/Users/greg/code/greghaskins/claude-code-workspace/middlendian/fileblock-csi` (the submodule, not the workspace root).
- [ ] Confirm you are on branch `fix/adoptexisting-mountpoint-verify` with the spec commit (`7864a62`) as `HEAD`. Run: `git status && git log --oneline -2`. Expected: clean working tree, HEAD contains the spec doc.
- [ ] Confirm `make test` passes against the current `HEAD` (baseline). Run: `make test`. Expected: all packages PASS.

---

## Chunk 1: API surface change (no behavior change)

These two tasks change the `Registry`/`AdoptExisting` signatures and thread the new dependencies through every call site. No test logic changes; no behavior changes. Each task is fully reversible and the codebase compiles + tests pass at the end of every task.

### Task 1: Add `MountChecker` interface and update `NewRegistry` signature

**Files:**
- Modify: `pkg/store/registry.go` (add interface, add `mp` field, update `NewRegistry`)
- Modify: `pkg/store/registry_test.go` (every `NewRegistry(...)` call site — 10 in this file)
- Modify: `pkg/driver/controller_test.go:105` (one `store.NewRegistry` call inside a helper)
- Modify: `pkg/driver/node_test.go:29` (one `store.NewRegistry` call, passes `nil` mounters)
- Modify: `cmd/controller/main.go:34` (the `NewRegistry` call)
- Modify: `cmd/node/main.go:60` (the `NewRegistry` call)

Sanity check before editing: run `grep -rn "NewRegistry(" --include='*.go' .` from the repo root. Expected callers (all of which the new 4-arg signature breaks): 10 in `pkg/store/registry_test.go`, 1 in `pkg/driver/controller_test.go`, 1 in `pkg/driver/node_test.go`, 1 each in `cmd/controller/main.go` and `cmd/node/main.go`, plus the definition itself in `pkg/store/registry.go`.

- [ ] **Step 1.1: Add the `MountChecker` interface in `pkg/store/registry.go`**

Insert after the package import block (around line 10), before the `storeIDPattern` declaration:

```go
// MountChecker verifies whether a path is a live mountpoint. Implemented
// by pkg/mount.Mounter (which shells out to findmnt(8)); tests substitute
// a fake.
type MountChecker interface {
	IsMountPoint(ctx context.Context, target string) (bool, error)
}
```

`pkg/store/registry.go` already imports `"context"` (line 4), so no new import is required.

- [ ] **Step 1.2: Add `mp MountChecker` field to `Registry` struct**

Edit the struct definition in `pkg/store/registry.go:23-32`:

```go
type Registry struct {
	root   string
	nfsM   Mounter
	localM Mounter
	mp     MountChecker

	mu      sync.Mutex
	mounted map[string]string      // storeID -> mounted path
	configs map[string]Config      // storeID -> Config that produced this storeID
	storeMu map[string]*sync.Mutex // storeID -> per-store lock
}
```

- [ ] **Step 1.3: Update `NewRegistry` to accept and store the `MountChecker`**

Edit `pkg/store/registry.go:36-45`:

```go
// NewRegistry returns a Registry that mounts under root. mounters may be
// nil if the corresponding type is not supported in this binary. mp is
// used by AdoptExisting to verify candidate directories are live mounts
// before adopting them — without it, a stale <storeID> directory in an
// emptyDir cache would poison the mounted-paths map after a container
// restart.
func NewRegistry(root string, nfs Mounter, local Mounter, mp MountChecker) *Registry {
	return &Registry{
		root:    root,
		nfsM:    nfs,
		localM:  local,
		mp:      mp,
		mounted: map[string]string{},
		configs: map[string]Config{},
		storeMu: map[string]*sync.Mutex{},
	}
}
```

- [ ] **Step 1.4: Update every `NewRegistry` call in `pkg/store/registry_test.go`**

There are 10 callers. Each currently looks like:

```go
reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
```

Change each to extract the `*mount.Mounter` and pass it as the new fourth argument:

```go
mnt := mount.New(fake)
reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)
```

Apply this transformation to every test function in `pkg/store/registry_test.go`:
- `TestRegistryGetMountsOnce`
- `TestRegistryDistinctConfigsMountSeparately`
- `TestRegistryConcurrentGetSerializes`
- `TestRegistryRejectsUnknownType`
- `TestRegistryConfigByStoreID`
- `TestRegistryMountedPaths`
- `TestRegistryAdoptExistingNoOpOnEmptyRoot`
- `TestRegistryAdoptExistingPreloadsKnownDirs`
- `TestRegistryAdoptExistingSkipsNonStoreIDDirs`
- `TestRegistryDoesNotCacheOnMountFailure`

- [ ] **Step 1.5: Update `NewRegistry` calls in `pkg/driver/{controller,node}_test.go`**

`pkg/driver/controller_test.go:105` (inside a `newTestRegistry`/`newRegistry` helper). It currently passes `mount.New(fake)` for the LocalMounter; reuse it for the new arg:

```go
mnt := mount.New(fake)
return store.NewRegistry(t.TempDir(), store.NewNFSMounter(fake), store.NewLocalMounter(mnt), mnt)
```

`pkg/driver/node_test.go:29` passes `nil, nil` for the mounters. The node-driver tests don't exercise `AdoptExisting`, so the new `MountChecker` argument can be `nil` here as well — no new imports needed:

```go
return store.NewRegistry(t.TempDir(), nil, nil, nil)
```

Verify both files compile after these edits. `go build` skips `_test.go` files, so use `go vet` (which does parse tests) for the check:

```
go vet ./pkg/driver/...
```

Expected: no output (vet-clean). The full `make test` in Step 1.8 will also exercise these.

- [ ] **Step 1.6: Update `cmd/controller/main.go` call site**

Edit `cmd/controller/main.go:34`:

```go
registry := store.NewRegistry(*storesRoot, store.NewNFSMounter(exec), store.NewLocalMounter(mnt), mnt)
```

(`mnt` is already declared at `cmd/controller/main.go:33` as `mnt := mount.New(exec)`.)

- [ ] **Step 1.7: Update `cmd/node/main.go` call site**

Edit `cmd/node/main.go:60`:

```go
registry := store.NewRegistry(*storesRoot, store.NewNFSMounter(exec), store.NewLocalMounter(mnt), mnt)
```

(`mnt` is already declared at `cmd/node/main.go:45` as `mnt := mount.New(exec)`.)

- [ ] **Step 1.8: Run build and tests**

```
make build
make test
```

Expected: both succeed. No behavior changes yet, so the existing test suite should pass without further modification.

- [ ] **Step 1.9: Commit**

```
git add pkg/store/registry.go pkg/store/registry_test.go pkg/driver/controller_test.go pkg/driver/node_test.go cmd/controller/main.go cmd/node/main.go
git commit -m "$(cat <<'EOF'
store: add MountChecker interface, thread into Registry

Adds a MountChecker interface (single method IsMountPoint) and a fourth
parameter to NewRegistry. *mount.Mounter satisfies it structurally — no
adapter needed. Both cmd/*/main.go entrypoints already construct a
*mount.Mounter for the LocalMounter; they now pass it as the new
argument. Driver tests in pkg/driver get the same plumbing
(controller_test.go reuses its mount.New(fake); node_test.go passes
nil because it does not exercise AdoptExisting).

No behavior change. Test signature updates only.

Prep for AdoptExisting verification fix; see
docs/superpowers/specs/2026-05-17-adoptexisting-mount-verification-design.md.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

### Task 2: Add `ctx context.Context` parameter to `AdoptExisting`

**Files:**
- Modify: `pkg/store/registry.go` (signature of `AdoptExisting`)
- Modify: `pkg/store/registry_test.go` (every `AdoptExisting()` call — there are 3)
- Modify: `cmd/controller/main.go:35` (`AdoptExisting()` call)
- Modify: `cmd/node/main.go:61` (`AdoptExisting()` call)

- [ ] **Step 2.1: Add `ctx context.Context` to `AdoptExisting` signature**

Edit `pkg/store/registry.go:122`:

```go
func (r *Registry) AdoptExisting(ctx context.Context) error {
```

Body unchanged in this task. `ctx` is declared but not yet used.

- [ ] **Step 2.2: Update `cmd/controller/main.go` call**

Edit `cmd/controller/main.go:35`:

```go
if err := registry.AdoptExisting(context.Background()); err != nil {
	log.Warn("adopt existing stores failed at startup", "err", err)
}
```

`"context"` is already in the import block (the file uses `context.Background()` for signal-notify-context).

- [ ] **Step 2.3: Update `cmd/node/main.go` call**

Edit `cmd/node/main.go:61`:

```go
if err := registry.AdoptExisting(context.Background()); err != nil {
	log.Warn("adopt existing stores failed at startup", "err", err)
}
```

`"context"` is already imported.

- [ ] **Step 2.4: Update every `AdoptExisting()` call in `pkg/store/registry_test.go`**

The three tests calling `AdoptExisting`:
- `TestRegistryAdoptExistingNoOpOnEmptyRoot` (around line 152)
- `TestRegistryAdoptExistingPreloadsKnownDirs` (around line 167)
- `TestRegistryAdoptExistingSkipsNonStoreIDDirs` (around line 202)

Change each to:

```go
if err := reg.AdoptExisting(context.Background()); err != nil {
	t.Fatalf("AdoptExisting: %v", err)
}
```

Add `"context"` to the test file's imports if not present.

- [ ] **Step 2.5: Build and test**

```
make build
make test
```

Expected: both succeed. No behavior changes yet.

- [ ] **Step 2.6: Commit**

```
git add pkg/store/registry.go pkg/store/registry_test.go cmd/controller/main.go cmd/node/main.go
git commit -m "$(cat <<'EOF'
store: add ctx parameter to Registry.AdoptExisting

Allows the upcoming IsMountPoint check inside AdoptExisting to honor a
startup deadline and lets tests pass controlled contexts. Both
cmd/*/main.go call sites pass context.Background() (no deadline at
startup).

No behavior change.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Chunk 2: Behavior change (TDD)

### Task 3: Add the mountpoint verification — TDD

**Files:**
- Modify: `pkg/store/registry_test.go` (add `TestRegistryAdoptExistingSkipsNonMountedDirs`)
- Modify: `pkg/store/registry.go` (`AdoptExisting` body)

- [ ] **Step 3.1: Add the failing test**

Append to `pkg/store/registry_test.go`. This test creates a stale `<storeID>` directory in `t.TempDir()` (simulating an emptyDir after container restart), configures `findmnt` to report exit code 1 ("not a mountpoint"), and asserts that AdoptExisting does NOT adopt the stale dir. It then exercises the recovery path: a subsequent `Get(cfg)` for the same store ID issues a real `mount` call.

Add this import if not yet present at the top of the file:

```go
import fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
```

Then add the test:

```go
// TestRegistryAdoptExistingSkipsNonMountedDirs verifies the fix for the
// emptyDir cache-poisoning bug: a storeID-shaped directory that exists
// but is not currently a mountpoint must NOT be adopted. Otherwise a
// stale leftover from a prior container run causes Get to short-circuit
// and NodeStageVolume to fail with NotFound on the .img file. The
// fixture wires findmnt to return exit 1 (the "not a mountpoint"
// sentinel that pkg/mount.Mounter.IsMountPoint recognizes) and a stub
// for the real mount(8) call that the recovery path needs.
func TestRegistryAdoptExistingSkipsNonMountedDirs(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	if err := os.MkdirAll(filepath.Join(root, cfg.ID()), 0o755); err != nil {
		t.Fatal(err)
	}

	fake := exectest.New()
	fake.Set("findmnt", "", &fbexec.Error{ExitCode: 1})
	fake.Set("mount", "", nil)

	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)

	if err := reg.AdoptExisting(context.Background()); err != nil {
		t.Fatalf("AdoptExisting: %v", err)
	}
	if paths := reg.MountedPaths(); len(paths) != 0 {
		t.Errorf("MountedPaths after AdoptExisting on non-mounted stale dir = %v, want empty", paths)
	}

	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatalf("Get: %v", err)
	}
	mountCalls := 0
	for _, c := range fake.Calls {
		if c.Name == "mount" {
			mountCalls++
		}
	}
	if mountCalls != 1 {
		t.Errorf("after non-adopted Get, mount called %d times, want 1", mountCalls)
	}
}
```

- [ ] **Step 3.2: Run the test and verify it FAILS**

```
go test ./pkg/store/ -run TestRegistryAdoptExistingSkipsNonMountedDirs -v
```

Expected: FAIL. The current `AdoptExisting` adopts the stale dir unconditionally, so `MountedPaths()` returns one entry and the subsequent `Get` short-circuits — zero `mount` calls instead of the expected one.

If the test PASSES at this point, stop and investigate — the implementation change in 3.3 may already be present.

- [ ] **Step 3.3: Implement the mountpoint check in `AdoptExisting`**

Edit the body of `AdoptExisting` in `pkg/store/registry.go`. Replace the loop interior so each candidate is verified via `r.mp.IsMountPoint` before adoption. Final function body:

```go
func (r *Registry) AdoptExisting(ctx context.Context) error {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read stores root %s: %w", r.root, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if !storeIDPattern.MatchString(id) {
			// Not a Registry-managed dir; skip. This guards against
			// accidental adoption of bind-mount source dirs the
			// operator may have placed under stores-root.
			continue
		}
		target := filepath.Join(r.root, id)
		mounted, err := r.mp.IsMountPoint(ctx, target)
		if err != nil || !mounted {
			// Conservative: skip on any failure or non-mount. Worst
			// case is a redundant mount(8) call on the next Get,
			// which is safe.
			continue
		}
		r.mounted[id] = target
	}
	return nil
}
```

- [ ] **Step 3.4: Run the new test and verify it PASSES**

```
go test ./pkg/store/ -run TestRegistryAdoptExistingSkipsNonMountedDirs -v
```

Expected: PASS.

- [ ] **Step 3.5: Run the full `pkg/store` test suite — expect existing AdoptExisting tests to FAIL**

```
go test ./pkg/store/ -v
```

Expected: `TestRegistryAdoptExistingPreloadsKnownDirs` and `TestRegistryAdoptExistingSkipsNonStoreIDDirs` now FAIL because they no longer set up `findmnt` rules. Task 4 fixes them. The other tests should still PASS.

- [ ] **Step 3.6: Commit (still red — existing tests broken)**

```
git add pkg/store/registry.go pkg/store/registry_test.go
git commit -m "$(cat <<'EOF'
store: AdoptExisting verifies mountpoint before adopting

Calls MountChecker.IsMountPoint on each storeID-shaped candidate
directory; only adopts when it reports a live mount. Stale leftovers
under an emptyDir-backed stores-root (after a container restart within
the same pod — node reboot, OOM, livenessprobe kill) no longer poison
the in-process mounted-paths cache. The next Get for that storeID does
a real mount, restoring NodeStageVolume.

New test (TestRegistryAdoptExistingSkipsNonMountedDirs) covers the
emptyDir-with-stale-dir case end-to-end: AdoptExisting skips, then
Get(cfg) issues exactly one mount call.

This commit breaks two existing tests
(TestRegistryAdoptExistingPreloadsKnownDirs and
TestRegistryAdoptExistingSkipsNonStoreIDDirs) because they don't set up
findmnt rules. Fixed in the next commit.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

### Task 4: Fix existing AdoptExisting tests broken by the behavior change

**Files:**
- Modify: `pkg/store/registry_test.go` — `TestRegistryAdoptExistingPreloadsKnownDirs` and `TestRegistryAdoptExistingSkipsNonStoreIDDirs`

- [ ] **Step 4.1: Update `TestRegistryAdoptExistingPreloadsKnownDirs`**

Find the existing test (around line 160). The current body constructs a `FakeRunner` with no rules. Update it to set a `findmnt` rule that makes `IsMountPoint` return `true` for the storeID-shaped dir. A single `fake.Set("findmnt", filepath.Join(root, "abc123def456"), nil)` covers both findmnt invocations in `Mounter.IsMountPoint` because the first call's output is discarded inside `IsMountPoint` (only the second is compared to `target`).

Final test body:

```go
func TestRegistryAdoptExistingPreloadsKnownDirs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "abc123def456")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := exectest.New()
	fake.Set("findmnt", dir, nil)
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)
	if err := reg.AdoptExisting(context.Background()); err != nil {
		t.Fatalf("AdoptExisting: %v", err)
	}
	got := reg.MountedPaths()
	if len(got) != 1 || got[0] != dir {
		t.Errorf("MountedPaths = %v", got)
	}
}
```

- [ ] **Step 4.2: Update `TestRegistryAdoptExistingSkipsNonStoreIDDirs`**

Find the existing test (around line 181). It creates several misnamed dirs (which the storeID-pattern filter rejects before any findmnt check) plus one valid storeID-shaped dir `0123456789ab` and asserts the valid one is adopted. Add a `findmnt` rule that makes `IsMountPoint` return `true` for the `0123456789ab` dir:

```go
func TestRegistryAdoptExistingSkipsNonStoreIDDirs(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{
		"local",               // bare word
		"FOO",                 // uppercase
		"abc123",              // too short
		"abc123def4567",       // too long
		"abc123def45z",        // not hex
		"abcdefabcdef.bak",    // extra chars
		"abcdefabcdef-suffix", // hyphen
	} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	validDir := filepath.Join(root, "0123456789ab")
	if err := os.MkdirAll(validDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := exectest.New()
	fake.Set("findmnt", validDir, nil)
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)
	if err := reg.AdoptExisting(context.Background()); err != nil {
		t.Fatalf("AdoptExisting: %v", err)
	}
	got := reg.MountedPaths()
	if len(got) != 1 || got[0] != validDir {
		t.Errorf("MountedPaths = %v; want exactly the one storeID-shaped dir", got)
	}
}
```

- [ ] **Step 4.3: Run the full `pkg/store` test suite, expect PASS**

```
go test ./pkg/store/ -v
```

Expected: all tests PASS.

- [ ] **Step 4.4: Commit**

```
git add pkg/store/registry_test.go
git commit -m "$(cat <<'EOF'
store: update existing AdoptExisting tests for the mountpoint check

PreloadsKnownDirs and SkipsNonStoreIDDirs now set a findmnt rule so
Mounter.IsMountPoint returns true for the storeID-shaped dir(s) they
create. A single Set("findmnt", <dir>, nil) rule covers both findmnt
invocations inside IsMountPoint (the first call's output is discarded;
only the second is compared to target).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

### Task 5: Add `TestRegistryAdoptExistingAdoptsMountedDirs`

This is the positive-path companion to Task 3's test: assert that when `IsMountPoint` returns `true`, the storeID is adopted AND subsequent `Get` short-circuits without issuing a `mount` call.

**Files:**
- Modify: `pkg/store/registry_test.go` (append new test)

- [ ] **Step 5.1: Add the test**

Append:

```go
// TestRegistryAdoptExistingAdoptsMountedDirs asserts the positive path:
// when IsMountPoint reports a live mount, AdoptExisting caches the
// storeID and subsequent Get(cfg) short-circuits without issuing a real
// mount(8) call.
func TestRegistryAdoptExistingAdoptsMountedDirs(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	dir := filepath.Join(root, cfg.ID())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := exectest.New()
	fake.Set("findmnt", dir, nil)

	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)

	if err := reg.AdoptExisting(context.Background()); err != nil {
		t.Fatalf("AdoptExisting: %v", err)
	}
	got := reg.MountedPaths()
	if len(got) != 1 || got[0] != dir {
		t.Fatalf("MountedPaths after adopting verified mount = %v, want [%q]", got, dir)
	}

	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, c := range fake.Calls {
		if c.Name == "mount" {
			t.Errorf("Get after adoption should hit cache, but called mount: %v", c.Args)
		}
	}
}
```

- [ ] **Step 5.2: Run the test, expect PASS**

```
go test ./pkg/store/ -run TestRegistryAdoptExistingAdoptsMountedDirs -v
```

Expected: PASS.

- [ ] **Step 5.3: Commit**

```
git add pkg/store/registry_test.go
git commit -m "$(cat <<'EOF'
store: positive-path test for AdoptExisting cache hit

Covers the symmetric case to TestRegistryAdoptExistingSkipsNonMountedDirs:
when IsMountPoint reports a live mount, AdoptExisting populates the
cache and subsequent Get short-circuits (zero mount calls). Guards
against accidentally regressing the cache short-circuit path.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

### Task 6: Add `TestRegistryAdoptExistingSkipsOnCheckError`

Cover the third branch in `AdoptExisting`'s new check: when `IsMountPoint` returns a non-exit-1 error, the candidate is skipped (conservative), `AdoptExisting` itself still returns `nil`, and the next `Get(cfg)` triggers a real mount.

**Files:**
- Modify: `pkg/store/registry_test.go` (append new test)

- [ ] **Step 6.1: Add the test**

Append:

```go
// TestRegistryAdoptExistingSkipsOnCheckError verifies that if
// IsMountPoint returns an error that's NOT the exit-1 "not a mount"
// sentinel (e.g. findmnt missing, or an unexpected exit code),
// AdoptExisting skips that candidate but still returns nil. The next
// Get(cfg) triggers a real mount(8) call — the recovery is identical
// to the "not a mount" case.
func TestRegistryAdoptExistingSkipsOnCheckError(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	if err := os.MkdirAll(filepath.Join(root, cfg.ID()), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := exectest.New()
	fake.Set("findmnt", "", &fbexec.Error{ExitCode: 2})
	fake.Set("mount", "", nil)

	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)

	if err := reg.AdoptExisting(context.Background()); err != nil {
		t.Fatalf("AdoptExisting should swallow per-candidate check errors, got %v", err)
	}
	if paths := reg.MountedPaths(); len(paths) != 0 {
		t.Errorf("MountedPaths after IsMountPoint error = %v, want empty", paths)
	}

	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatalf("Get: %v", err)
	}
	mountCalls := 0
	for _, c := range fake.Calls {
		if c.Name == "mount" {
			mountCalls++
		}
	}
	if mountCalls != 1 {
		t.Errorf("after non-adopted Get, mount called %d times, want 1", mountCalls)
	}
}
```

- [ ] **Step 6.2: Run the test, expect PASS**

```
go test ./pkg/store/ -run TestRegistryAdoptExistingSkipsOnCheckError -v
```

Expected: PASS.

- [ ] **Step 6.3: Commit**

```
git add pkg/store/registry_test.go
git commit -m "$(cat <<'EOF'
store: error-path test for AdoptExisting mountpoint check

Covers the third branch in AdoptExisting's IsMountPoint use: a non-
exit-1 error from findmnt (e.g. findmnt missing, unexpected exit code)
is treated like "not a mount" — skip the candidate, return nil from
AdoptExisting, let the next Get do a real mount.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Chunk 3: Documentation and release prep

### Task 7: Rewrite the `AdoptExisting` doc block

The current doc comment at `pkg/store/registry.go:108-121` claims emptyDir is always a no-op. After the fix, that's still true but for a different reason (the dir may exist but won't pass `IsMountPoint`). Rewrite to reflect the new semantics.

**Files:**
- Modify: `pkg/store/registry.go:108-121`

- [ ] **Step 7.1: Rewrite the doc block**

Replace the comment block immediately above `func (r *Registry) AdoptExisting` with:

```go
// AdoptExisting walks r.root and adopts each immediate subdirectory whose
// name matches the storeID pattern AND whose path is currently a live
// mountpoint (verified via r.mp.IsMountPoint). Adopted entries populate
// the mounted-paths cache so MountedPaths() / DeleteVolume / etc. can
// find them. It does NOT re-mount anything — adoption requires an
// existing live mount.
//
// The mountpoint check is load-bearing under the default emptyDir cache
// shape: emptyDir survives container restarts within the same pod (only
// pod recreation wipes it), so after a container restart the stores-root
// may still contain a <storeID> directory from a prior run with no NFS
// mount underneath. Treating that as "mounted" would short-circuit the
// next Get and break NodeStageVolume. Under hostPath, the mount itself
// survives, IsMountPoint returns true, and adoption proceeds.
//
// Caveats:
//   - Cannot reconstruct the full Config for adopted stores; only the
//     storeID is recovered (from the directory name). DeleteVolume
//     against an adopted-but-not-Get-ed store will return NotFound from
//     ConfigByStoreID. After the next CreateVolume against the SC,
//     the Config is registered and Delete works.
//   - Per-candidate IsMountPoint errors are absorbed silently and treated
//     as "not adopted". The worst case is a redundant mount(8) on the
//     next Get, which is safe.
```

- [ ] **Step 7.2: Run tests to confirm no regression from the comment change**

```
go test ./pkg/store/ -v
```

Expected: all PASS (no code change in this step, but a sanity check).

- [ ] **Step 7.3: Commit**

```
git add pkg/store/registry.go
git commit -m "$(cat <<'EOF'
store: rewrite AdoptExisting doc to match verified-adoption semantics

The prior comment claimed "Under emptyDir (the recommended cache shape)
the directory is empty after restart, so this is always a no-op." That
assumption was wrong: emptyDir is preserved across container restarts
within the same pod; only pod recreation wipes it. The new doc explains
why the IsMountPoint check is load-bearing for the emptyDir case and
why hostPath still adopts cleanly.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

### Task 8: Add CHANGELOG entry

**Files:**
- Modify: `CHANGELOG.md` (`[Unreleased] → Fixed` section)

- [ ] **Step 8.1: Add the entry**

Open `CHANGELOG.md`. Find the `## [Unreleased]` heading near the top. Under its `### Fixed` subheading (create the subheading if it doesn't yet exist), prepend:

```markdown
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
  `TestRegistryAdoptExistingSkipsNonMountedDirs` (unit) added to guard
  against regression.
```

- [ ] **Step 8.2: Commit**

```
git add CHANGELOG.md
git commit -m "$(cat <<'EOF'
CHANGELOG: AdoptExisting mountpoint verification

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

### Task 9: Final verification

- [ ] **Step 9.1: Run `make check`**

```
make check
```

Expected: all gates PASS (fmt-check + vet + lint + tidy-check + race-enabled test + coverage + build + smoke + sanity).

If `make check` cannot be run locally because the host lacks root / loop devices / csc / csi-sanity (see CLAUDE.md), run the lighter gate set:

```
make fmt-check vet lint tidy-check test build
```

Expected: all PASS. CI will run the full `make check` once pushed.

- [ ] **Step 9.2: Push commits**

```
git push
```

Expected: pushes 8 new commits — one per task (Tasks 1 through 8). The spec commit is the pre-existing branch HEAD per Pre-flight and is not produced by this plan. The PR (#34) updates automatically.

- [ ] **Step 9.3: Confirm CI is green on the PR**

```
gh pr checks 34
```

Expected: all checks PASS. If any fail, investigate via `gh pr view 34 --json statusCheckRollup` and re-push fixes.

---

## Done state

After Task 9:
- `Registry.AdoptExisting` verifies each candidate via `findmnt(8)` before adopting.
- Three new unit tests (`SkipsNonMountedDirs`, `AdoptsMountedDirs`, `SkipsOnCheckError`) cover the three branches of the new check.
- Two existing tests (`PreloadsKnownDirs`, `SkipsNonStoreIDDirs`) updated to set the required findmnt rules.
- The misleading doc block is rewritten to reflect the verified-adoption semantics.
- CHANGELOG `[Unreleased] → Fixed` carries the incident-driven entry.
- PR #34 is ready for review; CI green.

Release path (separate, after PR merge): standard `cut-release` workflow → `version: v0.3.8` → automated tag + image + GitHub release.
