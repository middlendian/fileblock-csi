# backingStore.nfs.subDir Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an optional `backingStore.nfs.subDir` StorageClass parameter so one NFS export can hold a separate backing directory per namespace, while still being mounted exactly once per node.

**Architecture:** `store.Config` carries two identities instead of one — `MountID()` for the mount source (shared across subDirs) and `StoreID()` for the store (mount source plus subDir, distinct per namespace). `Registry` keys its mount bookkeeping by `MountID` and returns `<mount>/<subDir>`, creating that directory after the mount is live. The `${pvc.metadata.namespace}` token is substituted by the driver from metadata keys that external-provisioner injects under `--extra-create-metadata=true`.

**Tech Stack:** Go 1.25, `crypto/sha256`, `path`/`path/filepath`, `pkg/exec.Runner` for shell-outs, `pkg/exec/exectest.FakeRunner` for test doubles, kustomize manifests, kind-based e2e under build tag `e2e`.

**Spec:** `docs/superpowers/specs/2026-09-21-nfs-subdir-design.md`

## Global Constraints

Copied from the spec and `CLAUDE.md`; every task's requirements implicitly include these.

- **Token spelling is `${pvc.metadata.namespace}`, `${pvc.metadata.name}`, `${pv.metadata.name}`** — with `.metadata.`, matching csi-driver-nfs. Not `${pvc.namespace}`.
- **Injected metadata keys are `csi.storage.k8s.io/pvc/name`, `csi.storage.k8s.io/pvc/namespace`, `csi.storage.k8s.io/pv/name`.**
- **The empty-subDir hash must stay byte-identical to 0.3.x.** `StoreID() == MountID()` exactly when no `subDir` is set. Breaking this orphans every existing `.img`.
- **The pinned legacy storeID is `2a355b61d5f7`** for `Config{Type: TypeNFS, NFSServer: "nfs.example.internal", NFSPath: "/exports/k8s_ns"}`. Computed from the v0.3.8 tag, verifiable with `printf 'nfs|nfs.example.internal|/exports/k8s_ns|' | sha256sum | cut -c1-12`. Never regenerate this from the working tree.
- **`subDir` is NFS-only.** Setting it with `backingStore.type: local` is an error.
- **Unresolved `${` after substitution is fatal** — `InvalidArgument`, never a literal directory name.
- **Every shell-out goes through `pkg/exec.Runner`** so it can be faked in tests.
- **Never `panic` in a gRPC handler.** Return a `status.Error`.
- **One short comment per non-obvious block.** No multi-line comment headers, no re-stating identifier names.
- **Gates before every push** (this container has no root or loop devices, so the full `make check` cannot run here): `make fmt-check vet lint tidy-check test build`.

---

### Task 1: Identity API — `MountID` / `StoreID`

Replaces `Canonical() []byte` and `ID() string` with two sibling methods over unexported canonical strings, and adds the `NFSSubDir` field the store identity needs.

**Files:**
- Modify: `pkg/store/store.go` (whole file)
- Modify: `pkg/store/registry.go:110` (`cfg.ID()` → `cfg.MountID()` is Task 3; here only make it compile as `cfg.StoreID()`)
- Modify: `pkg/store/parse.go:59` (`c.ID()` → `c.StoreID()`)
- Modify: `pkg/driver/controller.go:273` (`cfg.ID()` → `cfg.StoreID()`)
- Test: `pkg/store/store_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces:
  - `store.Config` gains field `NFSSubDir string`
  - `func (c Config) MountID() string` — 12-char lowercase hex
  - `func (c Config) StoreID() string` — 12-char lowercase hex
  - unexported `func (c Config) mountKey() string`, `func (c Config) storeKey() string`, `func hashID(key string) string`
  - `Canonical()` and `ID()` no longer exist.

- [ ] **Step 1: Write the failing tests**

Replace the `Canonical`/`ID` tests in `pkg/store/store_test.go` (lines 21-79) with the following. Keep `TestTypeConstants` and `TestConfigZero` as they are.

```go
func TestMountKeyNFS(t *testing.T) {
	c := Config{
		Type:            TypeNFS,
		NFSServer:       "nfs.example.internal",
		NFSPath:         "/exports/fileblock",
		NFSMountOptions: "nfsvers=4.1,hard,timeo=600",
	}
	got := c.mountKey()
	want := "nfs|nfs.example.internal|/exports/fileblock|hard,nfsvers=4.1,timeo=600"
	if got != want {
		t.Errorf("mountKey = %q\n  want %q", got, want)
	}
}

func TestMountKeyNFSReordersOptions(t *testing.T) {
	a := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSMountOptions: "nfsvers=4.1,hard,timeo=600"}
	b := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSMountOptions: "hard,nfsvers=4.1,timeo=600"}
	if a.mountKey() != b.mountKey() {
		t.Error("differently-ordered mountOptions must canonicalize identically")
	}
}

func TestMountKeyNFSDropsEmptyOptions(t *testing.T) {
	c := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSMountOptions: ",hard,,nfsvers=3,"}
	got := c.mountKey()
	want := "nfs|s|/p|hard,nfsvers=3"
	if got != want {
		t.Errorf("mountKey = %q\n  want %q", got, want)
	}
}

func TestMountKeyLocal(t *testing.T) {
	c := Config{Type: TypeLocal, LocalPath: "/var/lib/fileblock-store"}
	got := c.mountKey()
	want := "local|/var/lib/fileblock-store"
	if got != want {
		t.Errorf("mountKey = %q\n  want %q", got, want)
	}
}

func TestStoreIDIsDeterministicAndShort(t *testing.T) {
	c := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	id1 := c.StoreID()
	id2 := c.StoreID()
	if id1 != id2 {
		t.Errorf("StoreID not deterministic: %q vs %q", id1, id2)
	}
	if len(id1) != 12 {
		t.Errorf("StoreID length = %d, want 12", len(id1))
	}
	if len(c.MountID()) != 12 {
		t.Errorf("MountID length = %d, want 12", len(c.MountID()))
	}
}

func TestStoreIDDiffersForDifferentConfigs(t *testing.T) {
	a := Config{Type: TypeNFS, NFSServer: "s1", NFSPath: "/p"}
	b := Config{Type: TypeNFS, NFSServer: "s2", NFSPath: "/p"}
	if a.StoreID() == b.StoreID() {
		t.Error("StoreIDs collide for distinct configs")
	}
}

// TestStoreIDPinnedLegacyValue pins a storeID computed from the v0.3.8
// tag, before subDir existed:
//
//	printf 'nfs|nfs.example.internal|/exports/k8s_ns|' | sha256sum | cut -c1-12
//
// If this fails, the canonical form of a no-subDir config has changed and
// every volumeID issued by an earlier release now resolves to a store that
// does not exist. DeleteVolume would return OK per the CSI idempotency
// rule and leave the .img orphaned on the export forever. Do not
// regenerate this value from the working tree — that pins the bug.
func TestStoreIDPinnedLegacyValue(t *testing.T) {
	c := Config{
		Type:      TypeNFS,
		NFSServer: "nfs.example.internal",
		NFSPath:   "/exports/k8s_ns",
	}
	const want = "2a355b61d5f7"
	if got := c.StoreID(); got != want {
		t.Errorf("StoreID = %q, want %q (v0.3.8 value)", got, want)
	}
	if got := c.MountID(); got != want {
		t.Errorf("MountID = %q, want %q (v0.3.8 value)", got, want)
	}
}

// TestStoreIDEqualsMountIDWithoutSubDir is the compatibility invariant
// that the NFSSubDir == "" branch of storeKey exists to hold. Without
// this test the branch looks like dead code.
func TestStoreIDEqualsMountIDWithoutSubDir(t *testing.T) {
	for _, c := range []Config{
		{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"},
		{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSMountOptions: "hard,nfsvers=3"},
		{Type: TypeLocal, LocalPath: "/var/lib/fileblock"},
	} {
		if c.StoreID() != c.MountID() {
			t.Errorf("%+v: StoreID %q != MountID %q with no subDir", c, c.StoreID(), c.MountID())
		}
	}
}

func TestSubDirChangesStoreIDNotMountID(t *testing.T) {
	base := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	a := base
	a.NFSSubDir = "ns-a/fileblock"
	b := base
	b.NFSSubDir = "ns-b/fileblock"

	if a.StoreID() == b.StoreID() {
		t.Error("distinct subDirs must produce distinct StoreIDs")
	}
	if a.StoreID() == base.StoreID() {
		t.Error("a subDir must change the StoreID")
	}
	if a.MountID() != b.MountID() || a.MountID() != base.MountID() {
		t.Errorf("subDir must not change MountID: %q %q %q",
			base.MountID(), a.MountID(), b.MountID())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./pkg/store/ -run 'MountKey|StoreID|SubDir' -v`
Expected: FAIL to compile — `c.mountKey undefined`, `c.StoreID undefined`, `unknown field NFSSubDir`.

- [ ] **Step 3: Rewrite `pkg/store/store.go`**

Replace the file body from the `Config` struct through `ID()` with this. Keep the package doc comment, the imports, `Type`, the `TypeNFS`/`TypeLocal` constants and `canonicalOptions` exactly as they are.

```go
// Config is the parsed shape of an SC's backingStore.* parameters. It is
// the input to MountID(), StoreID(), and Mounter.Mount.
type Config struct {
	Type Type

	// NFS-only.
	NFSServer       string
	NFSPath         string
	NFSMountOptions string
	// NFSSubDir places this store's .img files in a subdirectory of the
	// export rather than at its root. Empty means the export root, which
	// is the 0.3.x behaviour. Already substituted and cleaned by
	// ConfigFromParams.
	NFSSubDir string

	// Local-only.
	LocalPath   string
	LocalShared bool // true if local.path is shared across nodes (e.g. via OS-level
	// shared FS or kind extraMount); the controller then advertises
	// this PV as schedulable on any node, like NFS-type.
}

// mountKey is a stable string identifying the mount *source*: type,
// server, path and mount options. Field order is fixed; mountOptions are
// split on commas, empties dropped, sorted lexicographically, then
// rejoined so that "a,b" and "b,a" hash identically. Deliberately
// excludes NFSSubDir — every subDir under one export shares a single
// mount.
func (c Config) mountKey() string {
	switch c.Type {
	case TypeNFS:
		return strings.Join([]string{
			"nfs",
			c.NFSServer,
			c.NFSPath,
			canonicalOptions(c.NFSMountOptions),
		}, "|")
	case TypeLocal:
		return strings.Join([]string{
			"local",
			c.LocalPath,
		}, "|")
	}
	return "invalid|" + string(c.Type)
}

// storeKey is a stable string identifying the *store*: the mount source
// plus the subDir that .img files actually live in.
//
// The empty-subDir branch is load-bearing, not an optimization. It keeps
// the string byte-identical to the 0.3.x canonical form, so every
// storeID issued by an earlier release still resolves. Appending a
// separator unconditionally would re-hash every existing config and
// leave DeleteVolume unable to find images it then reports as deleted.
func (c Config) storeKey() string {
	if c.NFSSubDir == "" {
		return c.mountKey()
	}
	return c.mountKey() + "|" + c.NFSSubDir
}

func hashID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// MountID identifies the mount source. Two configs differing only in
// subDir share one MountID, and therefore one mount: mounting the same
// export once per namespace would be wasteful and would multiply the
// blast radius of a hung mount. Names the mount directory under
// --stores-root.
func (c Config) MountID() string { return hashID(c.mountKey()) }

// StoreID identifies the directory .img files live in — the mount source
// plus subDir. This is the storeID in a volumeID's "fb-<storeID>-"
// prefix and in volume context, which is how DeleteVolume and
// ControllerExpandVolume find a volume's home store.
//
// Invariant: StoreID() == MountID() exactly when no subDir is set, and
// both equal the value 0.3.x produced. See storeKey.
func (c Config) StoreID() string { return hashID(c.storeKey()) }
```

- [ ] **Step 4: Fix the three non-test call sites so the tree compiles**

`pkg/store/parse.go:59`, inside `ToVolumeContext`:

```go
		VolumeContextStoreID: c.StoreID(),
```

`pkg/store/registry.go:110`, inside `Get` (Task 3 replaces this line properly; for now it only has to compile):

```go
	id := cfg.StoreID()
```

`pkg/driver/controller.go:273`, inside `volumeIDFromName`:

```go
	return "fb-" + cfg.StoreID() + "-" + name, nil
```

- [ ] **Step 5: Fix remaining `.ID()` references in other packages' tests**

Run: `grep -rn '\.ID()' --include='*_test.go' .`
Replace each `cfg.ID()` / `c.ID()` with `.StoreID()`. There are roughly 12, in `pkg/store/registry_test.go` and `pkg/driver/*_test.go`. They are mechanical — the value is unchanged for every config in those tests, because none sets a subDir.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./... && go vet ./...`
Expected: PASS, no vet findings. In particular `TestStoreIDPinnedLegacyValue` must pass — if it does not, `mountKey` diverged from the 0.3.x canonical form, which is a stop-and-reassess, not a test to adjust.

- [ ] **Step 7: Verify the pinned value independently**

Run: `printf 'nfs|nfs.example.internal|/exports/k8s_ns|' | sha256sum | cut -c1-12`
Expected: `2a355b61d5f7` — the same literal the test pins.

- [ ] **Step 8: Run the gates and commit**

```bash
make fmt-check vet lint tidy-check test build
git add pkg/store/store.go pkg/store/store_test.go pkg/store/parse.go pkg/store/registry.go pkg/driver/controller.go pkg/store/registry_test.go pkg/driver/controller_test.go pkg/driver/node_test.go
git commit -m "store: split Config identity into MountID and StoreID

Replaces Canonical()/ID() with two sibling methods over unexported
mountKey()/storeKey(). MountID identifies the mount source and is shared
across subDirs; StoreID identifies the directory .img files live in.

storeKey's empty-subDir branch keeps the string byte-identical to 0.3.x,
pinned by TestStoreIDPinnedLegacyValue against a literal computed from
the v0.3.8 tag. Without it every pre-0.4.0 storeID stops resolving and
DeleteVolume silently orphans images on the export.

Refs #39

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01L9utG96jJpXvfiX7d2HSjk"
```

---

### Task 2: Parsing, token substitution, validation

**Files:**
- Modify: `pkg/store/parse.go`
- Test: `pkg/store/parse_test.go`

**Interfaces:**
- Consumes: `Config.NFSSubDir` from Task 1.
- Produces:
  - exported `ParamNFSSubDir = "backingStore.nfs.subDir"`
  - unexported `func resolveSubDir(raw string, params map[string]string) (string, error)`
  - `ConfigFromParams` sets `Config.NFSSubDir` to the substituted, cleaned value
  - `ToVolumeContext` emits `ParamNFSSubDir` when non-empty

- [ ] **Step 1: Write the failing tests**

Append to `pkg/store/parse_test.go`:

```go
func nfsParams(extra map[string]string) map[string]string {
	p := map[string]string{
		"backingStore.type":       "nfs",
		"backingStore.nfs.server": "nfs.example.internal",
		"backingStore.nfs.path":   "/exports/k8s_ns",
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func TestConfigFromParamsSubDirAbsentIsEmpty(t *testing.T) {
	c, err := ConfigFromParams(nfsParams(nil))
	if err != nil {
		t.Fatalf("ConfigFromParams: %v", err)
	}
	if c.NFSSubDir != "" {
		t.Errorf("NFSSubDir = %q, want empty", c.NFSSubDir)
	}
}

func TestConfigFromParamsSubDirLiteral(t *testing.T) {
	c, err := ConfigFromParams(nfsParams(map[string]string{
		"backingStore.nfs.subDir": "team-a/fileblock",
	}))
	if err != nil {
		t.Fatalf("ConfigFromParams: %v", err)
	}
	if c.NFSSubDir != "team-a/fileblock" {
		t.Errorf("NFSSubDir = %q, want %q", c.NFSSubDir, "team-a/fileblock")
	}
}

func TestConfigFromParamsSubDirSubstitutesNamespace(t *testing.T) {
	c, err := ConfigFromParams(nfsParams(map[string]string{
		"backingStore.nfs.subDir":            "${pvc.metadata.namespace}/fileblock",
		"csi.storage.k8s.io/pvc/namespace":   "team-a",
	}))
	if err != nil {
		t.Fatalf("ConfigFromParams: %v", err)
	}
	if c.NFSSubDir != "team-a/fileblock" {
		t.Errorf("NFSSubDir = %q, want %q", c.NFSSubDir, "team-a/fileblock")
	}
}

func TestConfigFromParamsSubDirSubstitutesPVCAndPVName(t *testing.T) {
	c, err := ConfigFromParams(nfsParams(map[string]string{
		"backingStore.nfs.subDir":       "${pvc.metadata.name}/${pv.metadata.name}",
		"csi.storage.k8s.io/pvc/name":   "my-claim",
		"csi.storage.k8s.io/pv/name":    "pv-123",
	}))
	if err != nil {
		t.Fatalf("ConfigFromParams: %v", err)
	}
	if c.NFSSubDir != "my-claim/pv-123" {
		t.Errorf("NFSSubDir = %q, want %q", c.NFSSubDir, "my-claim/pv-123")
	}
}

// Without --extra-create-metadata=true the metadata keys are absent, the
// token survives, and every namespace would land in one shared directory
// whose name looks like a template. That is the exact isolation failure
// the parameter exists to prevent, so it is fatal.
func TestConfigFromParamsSubDirUnresolvedTokenIsFatal(t *testing.T) {
	_, err := ConfigFromParams(nfsParams(map[string]string{
		"backingStore.nfs.subDir": "${pvc.metadata.namespace}/fileblock",
	}))
	if err == nil {
		t.Fatal("expected an error for an unresolved token")
	}
	for _, want := range []string{
		"backingStore.nfs.subDir",
		"${pvc.metadata.namespace}",
		"--extra-create-metadata=true",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestConfigFromParamsSubDirMistypedTokenIsFatal(t *testing.T) {
	_, err := ConfigFromParams(nfsParams(map[string]string{
		"backingStore.nfs.subDir":          "${pvc.namespace}/fileblock",
		"csi.storage.k8s.io/pvc/namespace": "team-a",
	}))
	if err == nil {
		t.Fatal("expected an error for the mistyped ${pvc.namespace} token")
	}
	if !strings.Contains(err.Error(), "${pvc.namespace}") {
		t.Errorf("error %q should quote the unresolved token", err)
	}
}

func TestConfigFromParamsSubDirRejectsTraversal(t *testing.T) {
	for _, bad := range []string{
		"../escape",
		"team-a/../../escape",
		"a/../b",
	} {
		_, err := ConfigFromParams(nfsParams(map[string]string{
			"backingStore.nfs.subDir": bad,
		}))
		if err == nil {
			t.Errorf("subDir %q was accepted; traversal defeats the isolation the feature provides", bad)
		}
	}
}

func TestConfigFromParamsSubDirRejectsAbsolute(t *testing.T) {
	_, err := ConfigFromParams(nfsParams(map[string]string{
		"backingStore.nfs.subDir": "/exports/elsewhere",
	}))
	if err == nil || !strings.Contains(err.Error(), "relative") {
		t.Fatalf("expected a 'relative' error, got %v", err)
	}
}

func TestConfigFromParamsSubDirRejectsDot(t *testing.T) {
	for _, bad := range []string{".", "./"} {
		_, err := ConfigFromParams(nfsParams(map[string]string{
			"backingStore.nfs.subDir": bad,
		}))
		if err == nil {
			t.Errorf("subDir %q was accepted; it names the mount root under a distinct StoreID", bad)
		}
	}
}

func TestConfigFromParamsSubDirRejectsNUL(t *testing.T) {
	_, err := ConfigFromParams(nfsParams(map[string]string{
		"backingStore.nfs.subDir": "team-a\x00/fileblock",
	}))
	if err == nil {
		t.Fatal("expected an error for a NUL byte in subDir")
	}
}

// Trailing slashes and a leading ./ must not mint separate StoreIDs for
// one directory.
func TestConfigFromParamsSubDirNormalizes(t *testing.T) {
	var ids []string
	for _, in := range []string{"team-a/fileblock", "team-a/fileblock/", "./team-a/fileblock"} {
		c, err := ConfigFromParams(nfsParams(map[string]string{
			"backingStore.nfs.subDir": in,
		}))
		if err != nil {
			t.Fatalf("ConfigFromParams(%q): %v", in, err)
		}
		if c.NFSSubDir != "team-a/fileblock" {
			t.Errorf("subDir %q normalized to %q, want %q", in, c.NFSSubDir, "team-a/fileblock")
		}
		ids = append(ids, c.StoreID())
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Errorf("normalization produced distinct StoreIDs: %v", ids)
			break
		}
	}
}

func TestConfigFromParamsSubDirRejectedForLocal(t *testing.T) {
	_, err := ConfigFromParams(map[string]string{
		"backingStore.type":       "local",
		"backingStore.local.path": "/var/lib/fileblock",
		"backingStore.nfs.subDir": "team-a",
	})
	if err == nil || !strings.Contains(err.Error(), "backingStore.nfs.subDir") {
		t.Fatalf("expected a subDir-not-supported error, got %v", err)
	}
}

// The node receives an already-resolved subDir in volume context and must
// re-parse it without metadata keys present.
func TestVolumeContextRoundTripsSubDir(t *testing.T) {
	c := Config{
		Type:      TypeNFS,
		NFSServer: "nfs.example.internal",
		NFSPath:   "/exports/k8s_ns",
		NFSSubDir: "team-a/fileblock",
	}
	vc := c.ToVolumeContext()
	if vc["backingStore.nfs.subDir"] != "team-a/fileblock" {
		t.Fatalf("volume context subDir = %q", vc["backingStore.nfs.subDir"])
	}
	back, err := ConfigFromVolumeContext(vc)
	if err != nil {
		t.Fatalf("ConfigFromVolumeContext: %v", err)
	}
	if back.NFSSubDir != c.NFSSubDir {
		t.Errorf("subDir round-trip: %q -> %q", c.NFSSubDir, back.NFSSubDir)
	}
	if back.StoreID() != c.StoreID() {
		t.Errorf("StoreID round-trip: %q -> %q", c.StoreID(), back.StoreID())
	}
}

func TestVolumeContextOmitsEmptySubDir(t *testing.T) {
	c := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	if _, ok := c.ToVolumeContext()["backingStore.nfs.subDir"]; ok {
		t.Error("volume context must omit subDir when it is empty")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./pkg/store/ -run SubDir -v`
Expected: FAIL — `ConfigFromParams` ignores the key, so `NFSSubDir` is empty and the rejection tests get `nil` errors.

- [ ] **Step 3: Add the constants to `pkg/store/parse.go`**

Extend the existing `const` block at the top of the file:

```go
const (
	ParamType            = "backingStore.type"
	ParamNFSServer       = "backingStore.nfs.server"
	ParamNFSPath         = "backingStore.nfs.path"
	ParamNFSMountOptions = "backingStore.nfs.mountOptions"
	ParamNFSSubDir       = "backingStore.nfs.subDir"
	ParamLocalPath       = "backingStore.local.path"
	ParamLocalShared     = "backingStore.local.shared"

	VolumeContextStoreID = "storeID"

	// Injected into CreateVolumeRequest.parameters by external-provisioner,
	// but only when it runs with --extra-create-metadata=true. The sidecar
	// does no templating of its own; substitution below is ours.
	paramPVCName      = "csi.storage.k8s.io/pvc/name"
	paramPVCNamespace = "csi.storage.k8s.io/pvc/namespace"
	paramPVName       = "csi.storage.k8s.io/pv/name"

	// Spelled as csi-driver-nfs spells them, so a working subDir value
	// moves across unchanged.
	tokenPVCName      = "${pvc.metadata.name}"
	tokenPVCNamespace = "${pvc.metadata.namespace}"
	tokenPVName       = "${pv.metadata.name}"
)
```

Add `"path"` and `"strings"` to the file's imports (it currently imports only `"fmt"`).

- [ ] **Step 4: Add `resolveSubDir` to `pkg/store/parse.go`**

```go
// resolveSubDir substitutes the pv/pvc metadata tokens in raw and
// validates the result, returning the cleaned relative path.
//
// An unresolved token is fatal rather than a literal directory name.
// csi-driver-nfs passes the literal through; we do not, because the
// operator asked for per-namespace isolation and a directory called
// "${pvc.metadata.namespace}" silently gives every namespace the same
// one — invisible short of listing the export by hand.
func resolveSubDir(raw string, params map[string]string) (string, error) {
	if raw == "" {
		return "", nil
	}
	sub := raw
	if v := params[paramPVCNamespace]; v != "" {
		sub = strings.ReplaceAll(sub, tokenPVCNamespace, v)
	}
	if v := params[paramPVCName]; v != "" {
		sub = strings.ReplaceAll(sub, tokenPVCName, v)
	}
	if v := params[paramPVName]; v != "" {
		sub = strings.ReplaceAll(sub, tokenPVName, v)
	}

	if i := strings.Index(sub, "${"); i >= 0 {
		tok := sub[i:]
		if j := strings.Index(tok, "}"); j >= 0 {
			tok = tok[:j+1]
		}
		return "", fmt.Errorf("%s contains unresolved template %s: supported tokens are %s, %s and %s, "+
			"and the csi-provisioner sidecar must run with --extra-create-metadata=true for them to resolve",
			ParamNFSSubDir, tok, tokenPVCNamespace, tokenPVCName, tokenPVName)
	}
	if strings.ContainsRune(sub, 0) {
		return "", fmt.Errorf("%s must not contain NUL bytes", ParamNFSSubDir)
	}
	if path.IsAbs(sub) {
		return "", fmt.Errorf("%s must be relative to the export, got %q", ParamNFSSubDir, sub)
	}
	// Checked on the operator's literal input rather than after Clean so
	// the message quotes what they wrote. Clean would fold "a/../b" to
	// "b" and lose the fact that they asked to traverse.
	for _, seg := range strings.Split(sub, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%s must not contain %q path elements, got %q", ParamNFSSubDir, "..", sub)
		}
	}
	clean := path.Clean(sub)
	if clean == "." {
		return "", fmt.Errorf("%s must name a subdirectory of the export, got %q", ParamNFSSubDir, sub)
	}
	return clean, nil
}
```

- [ ] **Step 5: Wire it into `ConfigFromParams`**

In the `case TypeNFS:` branch, after the existing `NFSPath` check and before `return c, nil`:

```go
		sub, err := resolveSubDir(params[ParamNFSSubDir], params)
		if err != nil {
			return Config{}, err
		}
		c.NFSSubDir = sub
		return c, nil
```

In the `case TypeLocal:` branch, after the existing `LocalPath` check and before `return c, nil`:

```go
		// Accepting and ignoring it would look like it worked.
		if params[ParamNFSSubDir] != "" {
			return Config{}, fmt.Errorf("%s is not supported when %s=local", ParamNFSSubDir, ParamType)
		}
		return c, nil
```

- [ ] **Step 6: Emit it from `ToVolumeContext`**

In the `case TypeNFS:` branch of `ToVolumeContext`, after the `NFSMountOptions` block:

```go
		if c.NFSSubDir != "" {
			vc[ParamNFSSubDir] = c.NFSSubDir
		}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./pkg/store/ -v`
Expected: PASS, including every test from Task 1.

- [ ] **Step 8: Run the gates and commit**

```bash
make fmt-check vet lint tidy-check test build
git add pkg/store/parse.go pkg/store/parse_test.go
git commit -m "store: parse, substitute and validate backingStore.nfs.subDir

The driver does the token substitution itself. external-provisioner does
no templating of StorageClass parameter values -- under
--extra-create-metadata=true it only injects csi.storage.k8s.io/pvc/name,
.../pvc/namespace and .../pv/name, which is what we replace against.
Token spelling follows csi-driver-nfs (\${pvc.metadata.namespace}).

An unresolved token is InvalidArgument rather than a literal directory
name: the operator asked for per-namespace isolation, and a directory
called \${pvc.metadata.namespace} silently gives every namespace the
same one.

subDir is rejected as absolute, containing .., containing NUL, resolving
to \".\", or set alongside type=local. It is Cleaned before it reaches
storeKey so trailing slashes do not mint separate StoreIDs.

Refs #39

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01L9utG96jJpXvfiX7d2HSjk"
```

---

### Task 3: Registry — one mount, many stores

**Files:**
- Modify: `pkg/store/registry.go`
- Test: `pkg/store/registry_test.go`

**Interfaces:**
- Consumes: `Config.MountID()`, `Config.StoreID()`, `Config.NFSSubDir` from Tasks 1-2.
- Produces:
  - `Registry.Get` returns `<root>/<mountID>/<subDir>` and creates it
  - `Registry` gains unexported field `stores map[string]string` (storeID → resolved store path)
  - `Registry.MountedPaths()` returns the deduplicated union of store paths and mount roots
  - `Registry.ConfigByStoreID` unchanged in signature, still keyed by `StoreID()`

- [ ] **Step 1: Write the failing tests**

Append to `pkg/store/registry_test.go`:

```go
func TestRegistryGetReturnsSubDirPath(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	findmntReportsMounted(fake)
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSSubDir: "team-a/fileblock"}

	got, err := reg.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := filepath.Join(root, cfg.MountID(), "team-a", "fileblock")
	if got != want {
		t.Errorf("Get = %q, want %q", got, want)
	}
	// The namespace directory does not exist until the first volume lands
	// in it, and nothing else is positioned to create it.
	st, err := os.Stat(got)
	if err != nil || !st.IsDir() {
		t.Errorf("subDir was not created: %v", err)
	}
}

// Mounting one export once per namespace would be wasteful and would
// multiply the blast radius of a hung mount.
func TestRegistrySubDirsShareOneMount(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	findmntReportsMounted(fake)
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)

	a := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSSubDir: "ns-a/fileblock"}
	b := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSSubDir: "ns-b/fileblock"}

	pa, err := reg.Get(context.Background(), a)
	if err != nil {
		t.Fatalf("Get(a): %v", err)
	}
	pb, err := reg.Get(context.Background(), b)
	if err != nil {
		t.Fatalf("Get(b): %v", err)
	}
	if pa == pb {
		t.Fatalf("distinct subDirs returned the same path %q", pa)
	}
	if filepath.Dir(filepath.Dir(pa)) != filepath.Dir(filepath.Dir(pb)) {
		t.Errorf("subDirs must live under one mount: %q vs %q", pa, pb)
	}

	mountCalls := 0
	for _, c := range fake.Calls {
		if c.Name == "mount" {
			mountCalls++
		}
	}
	if mountCalls != 1 {
		t.Errorf("mount called %d times, want 1", mountCalls)
	}
}

func TestRegistryConfigByStoreIDResolvesSubDir(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	findmntReportsMounted(fake)
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSSubDir: "team-a/fileblock"}

	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, ok := reg.ConfigByStoreID(cfg.StoreID())
	if !ok {
		t.Fatal("ConfigByStoreID did not find the store")
	}
	if got.NFSSubDir != "team-a/fileblock" {
		t.Errorf("recovered subDir = %q, want %q", got.NFSSubDir, "team-a/fileblock")
	}
}

// ListVolumes iterates MountedPaths. A subDir store whose path is missing
// from it has its volumes silently disappear from ListVolumes.
func TestMountedPathsIncludesSubDirStores(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	findmntReportsMounted(fake)
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSSubDir: "team-a/fileblock"}

	p, err := reg.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var found bool
	for _, got := range reg.MountedPaths() {
		if got == p {
			found = true
		}
	}
	if !found {
		t.Errorf("MountedPaths %v does not contain store path %q", reg.MountedPaths(), p)
	}
}

// With no subDir the store path and the mount root are the same string;
// reporting it twice would list every volume twice.
func TestMountedPathsDeduplicates(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	findmntReportsMounted(fake)
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	p, err := reg.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	paths := reg.MountedPaths()
	if len(paths) != 1 || paths[0] != p {
		t.Errorf("MountedPaths = %v, want exactly [%q]", paths, p)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./pkg/store/ -run 'SubDir|MountedPaths' -v`
Expected: FAIL — `Get` still returns `<root>/<storeID>` with no subdirectory, and `MountedPaths` returns only mount roots.

- [ ] **Step 3: Add the `stores` map**

In the `Registry` struct, alongside the other maps (replace the existing map comments so they say what each is keyed by now):

```go
	mu       sync.Mutex
	mounted  map[string]string        // mountID -> mounted path
	stores   map[string]string        // storeID -> mounted path + subDir
	configs  map[string]Config        // storeID -> Config that produced this storeID
	storeMu  map[string]*sync.Mutex   // mountID -> per-mount lock
	checking map[string]chan checkRes // mountID -> in-flight staleness check
```

In `NewRegistry`'s returned literal, add:

```go
		stores:       map[string]string{},
```

- [ ] **Step 4: Rewrite `Registry.Get`**

Replace the whole of `Get` (its doc comment included) with:

```go
// Get ensures cfg's source is mounted under <root>/<mountID>/ and returns
// the store path inside it — <root>/<mountID>/<subDir>, or the mount
// itself when cfg sets no subDir. Idempotent: subsequent calls with the
// same cfg fast path on the cached mount, but only after re-verifying
// that the cached path is still a live mountpoint — a backing-store mount
// can drop out from under a running process, and the mountpoint directory
// it leaves behind is readable and empty, so an unverified cache turns
// every later lookup into a spurious "not found".
//
// Mount bookkeeping is keyed by MountID, so every subDir on one export
// shares a single mount and a single staleness check. The store path and
// cfg are cached by StoreID so callers holding only a storeID (the
// controller's DeleteVolume / Expand path) can resolve back to a Config
// via ConfigByStoreID.
func (r *Registry) Get(ctx context.Context, cfg Config) (string, error) {
	mountID := cfg.MountID()
	mountMu := r.lockStore(mountID)
	mountMu.Lock()
	defer mountMu.Unlock()

	r.mu.Lock()
	path, cached := r.mounted[mountID]
	r.mu.Unlock()
	if cached && !r.stillMounted(ctx, mountID, path) {
		r.log.Warn("backing store is no longer mounted; remounting",
			"mountID", mountID, "path", path)
		r.mu.Lock()
		delete(r.mounted, mountID)
		r.mu.Unlock()
		cached = false
	}

	if !cached {
		target := filepath.Join(r.root, mountID)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", target, err)
		}
		mnt, err := r.mounterFor(cfg.Type)
		if err != nil {
			return "", err
		}
		if err := mnt.Mount(ctx, target, cfg); err != nil {
			return "", err
		}
		r.mu.Lock()
		r.mounted[mountID] = target
		r.mu.Unlock()
		path = target
	}

	// After the mount, never before: creating the subDir first would
	// populate the directory the mount then hides. Repeated on every Get
	// rather than only on first mount, so a namespace directory removed
	// out-of-band comes back.
	sub := filepath.Join(path, cfg.NFSSubDir)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", sub, err)
	}

	r.mu.Lock()
	r.stores[cfg.StoreID()] = sub
	r.configs[cfg.StoreID()] = cfg
	r.mu.Unlock()
	return sub, nil
}
```

- [ ] **Step 5: Rewrite `Registry.MountedPaths`**

```go
// MountedPaths returns the absolute paths this Registry can hand to
// image.New: every store path it has resolved, plus every mount root it
// has mounted or adopted. Order is unspecified.
//
// Store paths are what ListVolumes actually needs — with a subDir the
// .img files live below the mount root, so returning roots alone would
// hide those volumes. Roots stay in the union because AdoptExisting
// recovers a mount directory without a Config, and its volumes must
// still be listable. For a store with no subDir the two coincide, hence
// the dedupe: a duplicate path would report every volume in it twice.
func (r *Registry) MountedPaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[string]struct{}, len(r.stores)+len(r.mounted))
	out := make([]string, 0, len(r.stores)+len(r.mounted))
	for _, m := range []map[string]string{r.stores, r.mounted} {
		for _, p := range m {
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}
```

- [ ] **Step 6: Update the stale doc comments that still say storeID**

`storeIDPattern` (around line 21) — it now matches mount directory names:

```go
// storeIDPattern matches a 12-char lowercase hex sha256 prefix — the
// shape Config.MountID() and Config.StoreID() both produce. AdoptExisting
// uses this to skip directories that happen to live under r.root but were
// not created by the Registry (e.g. an operator's local-backing source
// dir if they chose to put it under stores-root).
var storeIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)
```

`ConfigByStoreID` doc — change `Get(cfg) where cfg.ID() == id` to `Get(cfg) where cfg.StoreID() == id`.

`AdoptExisting` doc — append to the existing caveats list:

```go
//   - Adopts mount directories, which are named by MountID. A subDir
//     store's path is one level below and is only recorded by Get, so an
//     adopted mount contributes its root to MountedPaths but no subDir
//     stores until the next CreateVolume against that SC.
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./pkg/store/ ./pkg/driver/ -v`
Expected: PASS. `TestRegistryGetMountsOnce` still passes because with no subDir `MountID() == StoreID()`, so the returned path is unchanged.

- [ ] **Step 8: Run the gates and commit**

```bash
make fmt-check vet lint tidy-check test build
git add pkg/store/registry.go pkg/store/registry_test.go
git commit -m "store: share one mount across subDirs, return the store path

Registry keys mount bookkeeping by MountID and returns
<root>/<mountID>/<subDir>, creating the subDir after the mount is
confirmed live -- before it, the mkdir would populate the directory the
mount then hides. Two namespaces on one export share a mount, a
per-mount lock and a staleness check.

MountedPaths now returns the deduplicated union of store paths and mount
roots. ListVolumes iterates it, and with a subDir the .img files live
below the mount root, so returning roots alone hid those volumes
entirely. Roots stay in the union for stores recovered by AdoptExisting,
which has a directory name but no Config.

Refs #39

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01L9utG96jJpXvfiX7d2HSjk"
```

---

### Task 4: Provisioner flag, docs, CHANGELOG

**Files:**
- Modify: `deploy/kustomize/base/controller-deployment.yaml:53-62` (csi-provisioner args)
- Modify: `README.md:163-167` (parameter table) and after the NFSv3 section around line 126
- Modify: `CHANGELOG.md:8` (`## [Unreleased]`)
- Modify: `CLAUDE.md` (on-disk contract section)

**Interfaces:**
- Consumes: `ParamNFSSubDir` and the token spelling from Task 2.
- Produces: no Go symbols. The deployed provisioner injects the metadata keys `resolveSubDir` reads.

- [ ] **Step 1: Add `--extra-create-metadata=true` to the provisioner**

In `deploy/kustomize/base/controller-deployment.yaml`, in the `csi-provisioner` container's `args`, after the `--timeout=1200s` line:

```yaml
            # Injects csi.storage.k8s.io/pvc/{name,namespace} and
            # .../pv/name into CreateVolume parameters. Without it the
            # ${pvc.metadata.namespace} token in backingStore.nfs.subDir
            # never resolves and CreateVolume fails by design.
            - --extra-create-metadata=true
```

- [ ] **Step 2: Verify the rendered manifest carries it**

Run: `kubectl kustomize deploy/kustomize/base | grep -c 'extra-create-metadata=true'`
Expected: `1`

- [ ] **Step 3: Add the README parameter-table row**

In the table at `README.md:163-167`, after the `backingStore.nfs.mountOptions` row:

```
| `backingStore.nfs.subDir`     | no (type=nfs)     | Subdirectory of the export to hold this store's `.img` files; supports `${pvc.metadata.namespace}` |
```

- [ ] **Step 4: Add the worked example to the README**

After the NFSv3 mountOptions block (around `README.md:126`), add a new subsection:

````markdown
### Per-namespace backing directories

One export can hold a separate backing directory per namespace.
`backingStore.nfs.subDir` accepts the same pv/pvc metadata tokens
csi-driver-nfs uses:

```yaml
parameters:
  backingStore.type: nfs
  backingStore.nfs.server: nfs.example.internal
  backingStore.nfs.path: /exports/k8s_ns
  backingStore.nfs.subDir: ${pvc.metadata.namespace}/fileblock
```

A PVC in namespace `team-a` then lands at
`/exports/k8s_ns/team-a/fileblock/fb-<uuid>.img`. The export is still
mounted exactly once per node — every `subDir` under one export shares a
single mount.

Supported tokens are `${pvc.metadata.namespace}`, `${pvc.metadata.name}`
and `${pv.metadata.name}`. They are substituted by the driver from
metadata that external-provisioner injects only when it runs with
`--extra-create-metadata=true`; the shipped manifests set that flag. If a
token cannot be resolved, `CreateVolume` fails with `InvalidArgument`
rather than creating a directory named after the literal token — every
namespace would otherwise share it, and nothing would reveal that short
of listing the export by hand.

`subDir` must be relative and must not contain `..`. It is NFS-only;
setting it with `backingStore.type: local` is rejected. Omitting it is
the existing behaviour: the export root. Existing volumes are unaffected
— a store with no `subDir` keeps the storeID it has always had.
````

- [ ] **Step 5: Add the CHANGELOG entry**

Under `## [Unreleased]` in `CHANGELOG.md`:

```markdown
### Added

- `backingStore.nfs.subDir` StorageClass parameter places a store's
  `.img` files in a subdirectory of the NFS export instead of at its
  root, so one export and one StorageClass can give every namespace its
  own backing directory. Supports the `${pvc.metadata.namespace}`,
  `${pvc.metadata.name}` and `${pv.metadata.name}` tokens, spelled as
  csi-driver-nfs spells them. The export is still mounted exactly once
  per node: `Config` now carries a mount identity (`MountID`, shared
  across subDirs) separate from its store identity (`StoreID`).
- The `csi-provisioner` sidecar now runs with
  `--extra-create-metadata=true`, which is what injects the pvc/pv
  metadata the tokens above are substituted from. A `subDir` token that
  cannot be resolved fails `CreateVolume` with `InvalidArgument` rather
  than creating a directory named after the literal token.

### Changed

- `store.Config`'s `Canonical()` and `ID()` are replaced by `MountID()`
  and `StoreID()`. Internal API only; no on-disk or wire change.
  `StoreID()` equals the old `ID()` byte-for-byte for any config without
  a `subDir`, so existing volumeIDs, volume contexts and mount
  directories are untouched and no migration is required.
```

- [ ] **Step 6: Amend the CLAUDE.md convention and on-disk contract**

In the "On-disk contract" section, change the opening sentence from
"`pkg/image` is the **only** package that writes to the backing store." to:

```markdown
`pkg/image` is the **only** package that writes `.img` files. `pkg/store`
creates store root directories (`Registry.Get` creates a `subDir` after
its mount is live, since the namespace directory does not exist until the
first volume lands in it) and writes nothing else.
```

In the same section, after the `fb-<uuid>.img` block, add:

```markdown
With `backingStore.nfs.subDir` set, that file lives one level down:

```
<export>/<subDir>/fb-<uuid>.img
```

`subDir` participates in `Config.StoreID()` but not `Config.MountID()`,
so each namespace is a distinct store while the export is mounted once.
```

- [ ] **Step 7: Verify nothing else still claims subDir is unsupported**

Run: `grep -rn 'subDir\|subdir' README.md CLAUDE.md CHANGELOG.md deploy/`
Expected: only the additions above. Confirm the README's *Limitations* section does not contradict them.

- [ ] **Step 8: Run the gates and commit**

```bash
make fmt-check vet lint tidy-check test build
git add deploy/kustomize/base/controller-deployment.yaml README.md CHANGELOG.md CLAUDE.md
git commit -m "deploy,docs: enable --extra-create-metadata, document subDir

The provisioner sidecar needs --extra-create-metadata=true to inject the
pvc/pv metadata that backingStore.nfs.subDir tokens are substituted
from; without it every templated subDir fails validation. It ships with
the manifests rather than being left for operators to discover.

Also narrows the CLAUDE.md convention: pkg/image is the only package
that writes .img files, and pkg/store creates store roots.

Refs #39

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01L9utG96jJpXvfiX7d2HSjk"
```

---

### Task 5: End-to-end coverage on a real NFS export

This is the only layer that proves `--extra-create-metadata=true` is actually wired up: the unit tests supply the metadata keys directly, so they cannot catch a missing sidecar flag.

**Files:**
- Create: `test/e2e/subdir_test.go`
- Test: the file is the test.

**Interfaces:**
- Consumes: helpers already defined in `test/e2e/` (same package, build tag `e2e`) — `makeNamespace`, `applyYAML`, `kubectlRaw`, `waitPodReady`, `defaultPodReady` from `helpers.go`/`e2e_test.go`, and `pvcManifestSC`, `podWithPVC`, `pvForPVC` from `two_stores_test.go`.
- Consumes: environment exported by `hack/e2e.sh` before `go test` — `E2E_BACKING_KIND`, `NFS_SERVER`, `NFS_EXPORT`, `NFS_VERSION`.
- Produces: nothing other packages use.

- [ ] **Step 1: Write the test**

Create `test/e2e/subdir_test.go`:

```go
//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNFSSubDirPerNamespace provisions a StorageClass whose subDir carries
// the ${pvc.metadata.namespace} token and asserts the .img lands under the
// namespace directory on the export rather than at its root.
//
// This is the only layer that exercises the real external-provisioner, so
// it is the only thing that catches a missing --extra-create-metadata=true
// on the sidecar. The unit tests feed the metadata keys in directly.
func TestNFSSubDirPerNamespace(t *testing.T) {
	if os.Getenv("E2E_BACKING_KIND") != "nfs" {
		t.Skip("E2E_BACKING_KIND != nfs; subDir is NFS-only")
	}
	export := os.Getenv("NFS_EXPORT")
	if export == "" {
		t.Skip("NFS_EXPORT not set; cannot inspect the export directly")
	}

	ns := makeNamespace(t)
	const scName = "fileblock-subdir"
	applyYAML(t, subDirSCYAML(scName))
	t.Cleanup(func() {
		_, _ = kubectlRaw("delete", "sc", scName, "--ignore-not-found")
	})

	applyYAML(t, pvcManifestSC(ns, "vol", "128Mi", scName))
	applyYAML(t, podWithPVC(ns, "subdir", "vol"))
	waitPodReady(t, ns, "subdir", defaultPodReady)

	handle := pvForPVC(t, ns, "vol")

	// 1. The .img is under <export>/<namespace>/fileblock/.
	wantDir := filepath.Join(export, ns, "fileblock")
	wantImg := filepath.Join(wantDir, handle+".img")
	if _, err := os.Stat(wantImg); err != nil {
		t.Fatalf("expected %s on the export: %v (%s contains %v)",
			wantImg, err, wantDir, dirNames(wantDir))
	}

	// 2. Nothing at the export root — that separation is the whole point.
	for _, name := range dirNames(export) {
		if strings.HasSuffix(name, ".img") {
			t.Errorf("found %s at the export root; subDir did not take effect", name)
		}
	}

	// 3. The directory is named for the namespace, not for the token. A
	// literal directory here means the provisioner is missing
	// --extra-create-metadata=true and every namespace would share it.
	if _, err := os.Stat(filepath.Join(export, "${pvc.metadata.namespace}")); err == nil {
		t.Error("export has a directory named after the literal token; substitution did not happen")
	}
}

// subDirSCYAML renders an NFS StorageClass with a templated subDir,
// against the same export hack/e2e.sh stood up.
func subDirSCYAML(name string) string {
	version := os.Getenv("NFS_VERSION")
	if version == "" {
		version = "4.1"
	}
	return fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
provisioner: fileblock.csi
parameters:
  backingStore.type: nfs
  backingStore.nfs.server: %s
  backingStore.nfs.path: %s
  backingStore.nfs.mountOptions: "nfsvers=%s,hard,timeo=600,nolock"
  backingStore.nfs.subDir: ${pvc.metadata.namespace}/fileblock
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
`, name, os.Getenv("NFS_SERVER"), os.Getenv("NFS_EXPORT"), version)
}

// dirNames lists a directory's entries, returning nil rather than failing
// so it can be used inside failure messages.
func dirNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
```

- [ ] **Step 2: Verify it compiles under the build tag**

Run: `go vet -tags=e2e ./test/e2e/...`
Expected: no output. A `declared and not used` or `undefined` error here means a helper name drifted — check `two_stores_test.go` for the current spelling rather than inventing one.

- [ ] **Step 3: Confirm it is skipped, not run, in the local variant**

Run: `go test -tags=e2e -run TestNFSSubDirPerNamespace -list '.*' ./test/e2e/...`
Expected: the test name is listed. It only executes under `make e2e-nfs`; a plain `make e2e` skips it because `E2E_BACKING_KIND` is `local`.

- [ ] **Step 4: Run the NFS e2e suite if the machine can**

Run: `make e2e-nfs`
Expected: PASS, `TestNFSSubDirPerNamespace` included.

This needs docker, kind, sudo and `nfs-kernel-server`. **If the environment cannot provide those, do not fake the result** — say so explicitly in the task report, note that CI's `e2e.yml` `nfs` matrix variant covers it on push to `main`, and leave the verification to CI. Report honestly which of steps 2-4 actually ran.

- [ ] **Step 5: Run the gates and commit**

```bash
make fmt-check vet lint tidy-check test build
git add test/e2e/subdir_test.go
git commit -m "test/e2e: cover per-namespace subDir on a real NFS export

Asserts the .img lands under <export>/<namespace>/fileblock/, that
nothing lands at the export root, and that no directory named after the
literal token exists.

The unit tests feed the csi.storage.k8s.io metadata keys in directly, so
they cannot catch a provisioner missing --extra-create-metadata=true.
This runs the real sidecar, which makes it the only check that does.
Skipped unless E2E_BACKING_KIND=nfs.

Refs #39

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01L9utG96jJpXvfiX7d2HSjk"
```

---

## After all tasks

- [ ] Update the PR description on #40 to describe the shipped change rather than the spec, then mark it ready for review.
- [ ] Wait for explicit approval before merging.
- [ ] After merge, cut v0.4.0: **Actions → Cut release → Run workflow**, input `version: v0.4.0`. The workflow promotes the CHANGELOG `## [Unreleased]` section written in Task 4 and bumps the base kustomization `newTag`.
