# StorageClass-driven backing-store config — Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move backing-store configuration from binary flags + manifest patches into StorageClass parameters so multiple backing stores can coexist behind a single driver install with zero kustomize patches in the common case. Cuts as v0.3.0 (breaking SC schema change).

**Architecture:** A new `pkg/store` package owns "given an SC config, produce a mounted directory" — `Config` (parsed SC params), `ID()` (deterministic 12-char hex hash), `Mounter` interface with `nfs` and `local` impls shelling out via `pkg/exec.Runner`, and a `Registry` that mounts each unique store once per process under `/var/lib/fileblock/stores/<id>/`. The controller and node both hold a `*store.Registry`; `CreateVolume` parses SC parameters and `NodeStageVolume` parses volume context, both routing through `Registry.Get` to obtain the per-store path before delegating to the existing `pkg/image` / `pkg/loop` / `pkg/mount` machinery. Topology becomes implicit (NFS → empty `AccessibleTopology`, local → preferred-node pin); binary `--backing-store`, `--topology-key`, `--topology-value` flags and the base `storageclass.yaml` are removed.

**Tech Stack:** Go 1.25, container-storage-interface/spec, debian:trixie-slim base + nfs-common, kustomize, kind+e2e harness already in `hack/`. Unit tests use the existing `pkg/exec/exectest.FakeRunner`. No new third-party dependencies.

**Spec:** `docs/superpowers/specs/2026-05-09-storageclass-driven-config-design.md`. Read it before starting any chunk.

**Branch:** Continue on `design/storageclass-driven-config` (the spec's branch). The implementation lands on top of the spec commits; one PR closes the loop. Optional rename to `feature/v0.3.0-storageclass-config` is fine but not required.

---

## File structure

**New (all under `pkg/store/`):**

| File | Responsibility |
|---|---|
| `store.go` | `Config` struct, `Type` string-enum, named constants for parameter keys, `Canonical()` byte serializer, `ID()` 12-char hex hash. |
| `parse.go` | `ConfigFromParams` (SC params → Config), `ConfigFromVolumeContext` (vol ctx → Config), `Config.ToVolumeContext()` (Config → map for `CreateVolume` response). |
| `mounter.go` | `Mounter` interface (`Mount(ctx, target, cfg) error`); `mountersByType` resolver. |
| `mounter_nfs.go` | `NFSMounter` — exec `mount.nfs <server>:<path> <target> -o <opts>`. |
| `mounter_local.go` | `LocalMounter` — `BindMount` of `cfg.LocalPath` → target via `pkg/mount.Mounter`. |
| `registry.go` | `Registry` — `Get(ctx, cfg) (mountedPath, error)`; per-store `sync.Mutex`; in-memory `mounted` set; root dir `/var/lib/fileblock/stores`. |
| `store_test.go`, `parse_test.go`, `mounter_nfs_test.go`, `mounter_local_test.go`, `registry_test.go` | Unit tests via `exectest.FakeRunner`. |

**Modified:**

| File | Change |
|---|---|
| `pkg/driver/controller.go` | `ControllerServer` gains `*store.Registry`; `backingStorePath` field removed. `CreateVolume` parses SC params via `store.ConfigFromParams`, calls `Registry.Get` for a path, calls `image.New(path, exec)`, and returns `cfg.ToVolumeContext()` as the volume context (not the old `ParamBackingStorePath` echo). `ListVolumes` also switches to per-store path resolution (or returns volumes only from the configured registry stores — see Task 3.4). Topology: NFS returns empty `AccessibleTopology`, local pins to provisioner's preferred segment. The exported `ParamBackingStorePath` constant is removed (callers updated). |
| `pkg/driver/controller_test.go` | Update to construct `ControllerServer` with a `*store.Registry`-shaped fake; cover both `nfs` and `local` types and the topology branching. |
| `pkg/driver/node.go` | `NodeServer` gains `*store.Registry`. `topologyKey` / `topologyValue` fields collapsed to a single segment `{fileblock.csi/node: <nodeID>}` reported by `NodeGetInfo`. `NodeStageVolume` parses volume context via `store.ConfigFromVolumeContext`, calls `Registry.Get`, then `image.New(path, exec)` exactly as today. The legacy `ParamBackingStorePath` read from volume context is removed. |
| `pkg/driver/node_test.go` | Update construction; add a stage test that exercises the new volume-context shape. |
| `pkg/driver/server.go` | No change. |
| `cmd/controller/main.go` | Remove `--backing-store`, `--topology-key` flags. Construct `*store.Registry` (root `/var/lib/fileblock/stores`, NFS + local mounters, real `exec`/`mnt`). Pass into `NewControllerServer`. Drop the eager `image.New` at startup (per-store images are constructed inside `CreateVolume` from the per-store path). |
| `cmd/node/main.go` | Remove `--backing-store`, `--topology-key`, `--topology-value` flags. Construct `*store.Registry`. Pass into `NewNodeServer`. Reconciler now takes `/var/lib/fileblock/stores` as its `backingStorePath` (parent of all per-store dirs); rename the constant locally (`storesRoot`) for clarity. |
| `Dockerfile` | Add `nfs-common` to the existing `apt-get install --no-install-recommends` line. |
| `deploy/kustomize/base/storageclass.yaml` | **Delete.** |
| `deploy/kustomize/base/kustomization.yaml` | Remove `storageclass.yaml` from `resources`. |
| `deploy/kustomize/base/controller-deployment.yaml` | Drop `volumes[backing-store]` + matching volumeMount. Drop `--backing-store=...` arg. Drop `--topology-key=...` arg. Add `securityContext.capabilities.add: [SYS_ADMIN]` to the `fileblock-controller` container. Add `volumes[stores]: emptyDir: {}` mounted at `/var/lib/fileblock/stores`. Remove `--strict-topology` from the `csi-provisioner` sidecar args. |
| `deploy/kustomize/base/node-daemonset.yaml` | Drop `volumes[backing-store]` + matching volumeMount. Drop `--backing-store`, `--topology-key`, `--topology-value` args. Add `volumes[stores]: emptyDir: {}` mounted at `/var/lib/fileblock/stores`. |
| `deploy/kustomize/overlays/example-localdir/` | Collapse to one `storageclass.yaml` (`backingStore.type: local`) + a `kustomization.yaml` that resources the base + the SC. Delete the existing patches. |
| `deploy/kustomize/overlays/example-nfs-shared/` | Same: one `storageclass.yaml` (`backingStore.type: nfs`) + a `kustomization.yaml`. Delete the existing patches. |
| `deploy/kustomize/overlays/e2e/` | Replace `patch-storageclass.yaml` with an SC yaml (`type: local` for `make e2e`, `type: nfs` for `make e2e-nfs`). Keep `patch-controller.yaml` / `patch-node.yaml` only for image-tag override; drop their `--backing-store` / `--topology-*` arg edits. |
| `hack/smoke.sh` | Add a `type: local` SC test that exercises the new path end-to-end on a developer host (no NFS dependency). |
| `hack/e2e.sh` (or `make e2e-nfs` config) | Run once with `mountOptions: "nfsvers=3,..."` and once with `"nfsvers=4.1,..."` so both versions are exercised in CI. |
| `test/e2e/...` | Adapt SC fixtures to new schema. Add new `TestTwoStores` covering two SCs with distinct paths on the same NFS server, asserting both PVs come up and `.img` files land in distinct `<storeID>/` dirs. |
| `CHANGELOG.md` | Promote `[Unreleased]` to `[0.3.0] - 2026-05-09` with the breaking-changes section. |
| `README.md` | Update Limitations / installation example to the new SC schema. |

---

## Chunk 1: `pkg/store` foundations

This chunk introduces the `pkg/store` package with no real mount logic — types, parsing, hashing, volume-context round-trip. All tests are pure-Go unit tests.

### Task 1.1: Create `pkg/store/store.go` skeleton

**Files:**
- Create: `pkg/store/store.go`
- Create: `pkg/store/store_test.go`

- [ ] **Step 1: Write the failing test**

Create `pkg/store/store_test.go`:

```go
package store

import "testing"

func TestTypeConstants(t *testing.T) {
	if TypeNFS != "nfs" {
		t.Errorf("TypeNFS = %q, want %q", TypeNFS, "nfs")
	}
	if TypeLocal != "local" {
		t.Errorf("TypeLocal = %q, want %q", TypeLocal, "local")
	}
}

func TestConfigZero(t *testing.T) {
	var c Config
	if c.Type != "" {
		t.Errorf("zero Config.Type = %q, want empty", c.Type)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/store/...`
Expected: FAIL — `pkg/store` doesn't exist yet.

- [ ] **Step 3: Write minimal implementation**

Create `pkg/store/store.go`:

```go
// Package store owns the lookup from a StorageClass parameters / volume
// context map to a mounted directory the rest of the driver can write
// .img files into. Each unique backing-store config is mounted once per
// process; multiple StorageClasses pointing at the same source share
// one mount, keyed by a deterministic ID.
package store

// Type is the discriminator value of the `backingStore.type` SC parameter.
type Type string

const (
	TypeNFS   Type = "nfs"
	TypeLocal Type = "local"
)

// Config is the parsed shape of an SC's backingStore.* parameters. It is
// the input to ID(), Canonical(), and Mounter.Mount.
type Config struct {
	Type Type

	// NFS-only.
	NFSServer       string
	NFSPath         string
	NFSMountOptions string

	// Local-only.
	LocalPath string
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/store/...`
Expected: PASS — both `TestTypeConstants` and `TestConfigZero`.

- [ ] **Step 5: Commit**

```bash
git add pkg/store/store.go pkg/store/store_test.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "store: introduce Config and Type enum"
```

### Task 1.2: Implement `Canonical()` and `ID()`

**Files:**
- Modify: `pkg/store/store.go`
- Modify: `pkg/store/store_test.go`

- [ ] **Step 1: Write the failing test**

Append to `pkg/store/store_test.go`:

```go
func TestCanonicalNFS(t *testing.T) {
	c := Config{
		Type:            TypeNFS,
		NFSServer:       "nfs.example.internal",
		NFSPath:         "/exports/fileblock",
		NFSMountOptions: "nfsvers=4.1,hard,timeo=600",
	}
	got := string(c.Canonical())
	want := "nfs|nfs.example.internal|/exports/fileblock|hard,nfsvers=4.1,timeo=600"
	if got != want {
		t.Errorf("Canonical = %q\n  want %q", got, want)
	}
}

func TestCanonicalNFSReordersOptions(t *testing.T) {
	a := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSMountOptions: "nfsvers=4.1,hard,timeo=600"}
	b := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSMountOptions: "hard,nfsvers=4.1,timeo=600"}
	if string(a.Canonical()) != string(b.Canonical()) {
		t.Error("differently-ordered mountOptions must canonicalize to the same bytes")
	}
}

func TestCanonicalNFSDropsEmptyOptions(t *testing.T) {
	c := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSMountOptions: ",hard,,nfsvers=3,"}
	got := string(c.Canonical())
	want := "nfs|s|/p|hard,nfsvers=3"
	if got != want {
		t.Errorf("Canonical = %q\n  want %q", got, want)
	}
}

func TestCanonicalLocal(t *testing.T) {
	c := Config{Type: TypeLocal, LocalPath: "/var/lib/fileblock-store"}
	got := string(c.Canonical())
	want := "local|/var/lib/fileblock-store"
	if got != want {
		t.Errorf("Canonical = %q\n  want %q", got, want)
	}
}

func TestIDIsDeterministicAndShort(t *testing.T) {
	c := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	id1 := c.ID()
	id2 := c.ID()
	if id1 != id2 {
		t.Errorf("ID not deterministic: %q vs %q", id1, id2)
	}
	if len(id1) != 12 {
		t.Errorf("ID length = %d, want 12", len(id1))
	}
}

func TestIDDiffersForDifferentConfigs(t *testing.T) {
	a := Config{Type: TypeNFS, NFSServer: "s1", NFSPath: "/p"}
	b := Config{Type: TypeNFS, NFSServer: "s2", NFSPath: "/p"}
	if a.ID() == b.ID() {
		t.Error("IDs collide for distinct configs")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/store/...`
Expected: FAIL — `Canonical` and `ID` methods undefined.

- [ ] **Step 3: Write minimal implementation**

Append to `pkg/store/store.go`:

```go
import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Canonical returns a stable byte representation of the config used as
// the input to ID(). Field order is fixed; mountOptions are split on
// commas, empties dropped, sorted lexicographically, then rejoined so
// that "a,b" and "b,a" hash identically.
func (c Config) Canonical() []byte {
	switch c.Type {
	case TypeNFS:
		return []byte(strings.Join([]string{
			"nfs",
			c.NFSServer,
			c.NFSPath,
			canonicalOptions(c.NFSMountOptions),
		}, "|"))
	case TypeLocal:
		return []byte(strings.Join([]string{
			"local",
			c.LocalPath,
		}, "|"))
	}
	// Unknown type — return a sentinel that won't collide with valid forms.
	return []byte("invalid|" + string(c.Type))
}

// ID is a deterministic 12-char hex truncation of sha256(Canonical()).
// Used to name the per-store mount directory and as a label in volume
// context for diagnostics.
func (c Config) ID() string {
	sum := sha256.Sum256(c.Canonical())
	return hex.EncodeToString(sum[:])[:12]
}

func canonicalOptions(opts string) string {
	parts := strings.Split(opts, ",")
	cleaned := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		cleaned = append(cleaned, p)
	}
	sort.Strings(cleaned)
	return strings.Join(cleaned, ",")
}
```

Note: combine the new imports with the existing import (Go file has only one import block).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/store/... -run 'TestCanonical|TestID' -v`
Expected: PASS — all six tests.

- [ ] **Step 5: Commit**

```bash
git add pkg/store/store.go pkg/store/store_test.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "store: Canonical() and deterministic 12-char ID()"
```

### Task 1.3: Implement `ConfigFromParams` (SC parameters → Config)

**Files:**
- Create: `pkg/store/parse.go`
- Create: `pkg/store/parse_test.go`

- [ ] **Step 1: Write the failing test**

Create `pkg/store/parse_test.go`:

```go
package store

import (
	"strings"
	"testing"
)

func TestConfigFromParamsNFS(t *testing.T) {
	in := map[string]string{
		"backingStore.type":                "nfs",
		"backingStore.nfs.server":          "nfs.example.internal",
		"backingStore.nfs.path":            "/exports/fileblock",
		"backingStore.nfs.mountOptions":    "nfsvers=4.1,hard,timeo=600",
	}
	c, err := ConfigFromParams(in)
	if err != nil {
		t.Fatalf("ConfigFromParams: %v", err)
	}
	if c.Type != TypeNFS {
		t.Errorf("Type = %q, want %q", c.Type, TypeNFS)
	}
	if c.NFSServer != "nfs.example.internal" || c.NFSPath != "/exports/fileblock" {
		t.Errorf("server/path mismatch: %+v", c)
	}
	if c.NFSMountOptions != "nfsvers=4.1,hard,timeo=600" {
		t.Errorf("mountOptions = %q", c.NFSMountOptions)
	}
}

func TestConfigFromParamsNFSMissingServer(t *testing.T) {
	in := map[string]string{
		"backingStore.type":     "nfs",
		"backingStore.nfs.path": "/exports/fileblock",
	}
	_, err := ConfigFromParams(in)
	if err == nil || !strings.Contains(err.Error(), "backingStore.nfs.server") {
		t.Fatalf("expected error mentioning backingStore.nfs.server, got %v", err)
	}
}

func TestConfigFromParamsNFSMissingPath(t *testing.T) {
	in := map[string]string{
		"backingStore.type":       "nfs",
		"backingStore.nfs.server": "x",
	}
	_, err := ConfigFromParams(in)
	if err == nil || !strings.Contains(err.Error(), "backingStore.nfs.path") {
		t.Fatalf("expected error mentioning backingStore.nfs.path, got %v", err)
	}
}

func TestConfigFromParamsLocal(t *testing.T) {
	in := map[string]string{
		"backingStore.type":       "local",
		"backingStore.local.path": "/var/lib/fileblock-store",
	}
	c, err := ConfigFromParams(in)
	if err != nil {
		t.Fatalf("ConfigFromParams: %v", err)
	}
	if c.Type != TypeLocal || c.LocalPath != "/var/lib/fileblock-store" {
		t.Errorf("got %+v", c)
	}
}

func TestConfigFromParamsLocalMissingPath(t *testing.T) {
	in := map[string]string{"backingStore.type": "local"}
	_, err := ConfigFromParams(in)
	if err == nil || !strings.Contains(err.Error(), "backingStore.local.path") {
		t.Fatalf("expected error mentioning backingStore.local.path, got %v", err)
	}
}

func TestConfigFromParamsMissingType(t *testing.T) {
	_, err := ConfigFromParams(map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "backingStore.type") {
		t.Fatalf("expected error mentioning backingStore.type, got %v", err)
	}
}

func TestConfigFromParamsUnknownType(t *testing.T) {
	in := map[string]string{"backingStore.type": "smb"}
	_, err := ConfigFromParams(in)
	if err == nil || !strings.Contains(err.Error(), "smb") {
		t.Fatalf("expected error mentioning unknown type smb, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/store/... -run TestConfigFromParams`
Expected: FAIL — `ConfigFromParams` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `pkg/store/parse.go`:

```go
package store

import "fmt"

// SC parameter key names. These are also the volume-context key names —
// the controller echoes the parsed config back so the node can re-parse.
const (
	ParamType            = "backingStore.type"
	ParamNFSServer       = "backingStore.nfs.server"
	ParamNFSPath         = "backingStore.nfs.path"
	ParamNFSMountOptions = "backingStore.nfs.mountOptions"
	ParamLocalPath       = "backingStore.local.path"

	// VolumeContextStoreID is added by the controller for diagnostics; the
	// node does not require it to re-parse.
	VolumeContextStoreID = "storeID"
)

// ConfigFromParams parses SC.parameters into a Config. Missing or
// malformed required keys produce a non-nil error suitable for surfacing
// as gRPC InvalidArgument by the caller.
func ConfigFromParams(params map[string]string) (Config, error) {
	t := Type(params[ParamType])
	switch t {
	case TypeNFS:
		c := Config{
			Type:            TypeNFS,
			NFSServer:       params[ParamNFSServer],
			NFSPath:         params[ParamNFSPath],
			NFSMountOptions: params[ParamNFSMountOptions],
		}
		if c.NFSServer == "" {
			return Config{}, fmt.Errorf("%s is required when %s=nfs", ParamNFSServer, ParamType)
		}
		if c.NFSPath == "" {
			return Config{}, fmt.Errorf("%s is required when %s=nfs", ParamNFSPath, ParamType)
		}
		return c, nil
	case TypeLocal:
		c := Config{
			Type:      TypeLocal,
			LocalPath: params[ParamLocalPath],
		}
		if c.LocalPath == "" {
			return Config{}, fmt.Errorf("%s is required when %s=local", ParamLocalPath, ParamType)
		}
		return c, nil
	case "":
		return Config{}, fmt.Errorf("%s is required (got empty)", ParamType)
	default:
		return Config{}, fmt.Errorf("%s=%q not supported (must be nfs or local)", ParamType, t)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/store/... -run TestConfigFromParams -v`
Expected: PASS — all seven tests.

- [ ] **Step 5: Commit**

```bash
git add pkg/store/parse.go pkg/store/parse_test.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "store: parse StorageClass parameters into Config"
```

### Task 1.4: Implement volume-context round-trip

**Files:**
- Modify: `pkg/store/parse.go`
- Modify: `pkg/store/parse_test.go`

- [ ] **Step 1: Write the failing test**

Append to `pkg/store/parse_test.go`:

```go
func TestVolumeContextRoundTripNFS(t *testing.T) {
	c := Config{
		Type:            TypeNFS,
		NFSServer:       "nfs.example.internal",
		NFSPath:         "/exports/fileblock",
		NFSMountOptions: "nfsvers=4.1,hard,timeo=600",
	}
	vc := c.ToVolumeContext()
	if vc[ParamType] != "nfs" {
		t.Errorf("vc[%s] = %q", ParamType, vc[ParamType])
	}
	if vc[VolumeContextStoreID] != c.ID() {
		t.Errorf("vc[storeID] = %q, want %q", vc[VolumeContextStoreID], c.ID())
	}
	got, err := ConfigFromVolumeContext(vc)
	if err != nil {
		t.Fatalf("ConfigFromVolumeContext: %v", err)
	}
	if got != c {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", got, c)
	}
}

func TestVolumeContextRoundTripLocal(t *testing.T) {
	c := Config{Type: TypeLocal, LocalPath: "/var/lib/fileblock-store"}
	vc := c.ToVolumeContext()
	got, err := ConfigFromVolumeContext(vc)
	if err != nil {
		t.Fatalf("ConfigFromVolumeContext: %v", err)
	}
	if got != c {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, c)
	}
}

func TestVolumeContextOmitsEmptyMountOptions(t *testing.T) {
	c := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	vc := c.ToVolumeContext()
	if _, present := vc[ParamNFSMountOptions]; present {
		t.Errorf("mountOptions key should be absent when empty, got %+v", vc)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/store/... -run TestVolumeContext`
Expected: FAIL — `ToVolumeContext` and `ConfigFromVolumeContext` undefined.

- [ ] **Step 3: Write minimal implementation**

Append to `pkg/store/parse.go`:

```go
// ToVolumeContext serializes a Config into the map the controller
// returns from CreateVolume and the node receives in NodeStageVolume.
// It also embeds the storeID for diagnostics.
func (c Config) ToVolumeContext() map[string]string {
	vc := map[string]string{
		ParamType:            string(c.Type),
		VolumeContextStoreID: c.ID(),
	}
	switch c.Type {
	case TypeNFS:
		vc[ParamNFSServer] = c.NFSServer
		vc[ParamNFSPath] = c.NFSPath
		if c.NFSMountOptions != "" {
			vc[ParamNFSMountOptions] = c.NFSMountOptions
		}
	case TypeLocal:
		vc[ParamLocalPath] = c.LocalPath
	}
	return vc
}

// ConfigFromVolumeContext is a thin wrapper that re-parses the same key
// set ConfigFromParams expects. The two APIs are kept distinct so callers
// signal intent clearly; the body is shared.
func ConfigFromVolumeContext(vc map[string]string) (Config, error) {
	return ConfigFromParams(vc)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/store/... -v`
Expected: PASS — all parse and round-trip tests.

- [ ] **Step 5: Commit**

```bash
git add pkg/store/parse.go pkg/store/parse_test.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "store: volume-context round-trip via ToVolumeContext/ConfigFromVolumeContext"
```

---

## Chunk 2: `pkg/store` mounters and Registry

This chunk implements the actual mount machinery — `Mounter` interface, NFS and local mounters, and the `Registry` that serializes per-store mounting and caches the mounted state.

### Task 2.1: Define `Mounter` interface and resolver

**Files:**
- Create: `pkg/store/mounter.go`

- [ ] **Step 1: Write the implementation directly (interface only — tested through impls)**

Create `pkg/store/mounter.go`:

```go
package store

import "context"

// Mounter knows how to mount one Type's source into a target directory.
// Implementations must be idempotent over a target that is already
// mounted with the same source — but the Registry guarantees Mount is
// not called twice for the same storeID, so the typical impl can assume
// target is empty.
type Mounter interface {
	Mount(ctx context.Context, target string, cfg Config) error
}
```

- [ ] **Step 2: Compile-check**

Run: `go build ./pkg/store/...`
Expected: succeeds (no test failures because nothing depends on it yet).

- [ ] **Step 3: Commit**

```bash
git add pkg/store/mounter.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "store: Mounter interface"
```

### Task 2.2: Implement `LocalMounter` (bind-mount)

**Files:**
- Create: `pkg/store/mounter_local.go`
- Create: `pkg/store/mounter_local_test.go`

`LocalMounter` delegates to `pkg/mount.Mounter.BindMount`, which is the same path `NodePublishVolume` already uses. That keeps the bind-mount mechanics in one place; this mounter is a thin adapter.

- [ ] **Step 1: Write the failing test**

Create `pkg/store/mounter_local_test.go`:

```go
package store

import (
	"context"
	"errors"
	"testing"

	"github.com/middlendian/fileblock-csi/pkg/exec/exectest"
	"github.com/middlendian/fileblock-csi/pkg/mount"
)

func TestLocalMounterBindMounts(t *testing.T) {
	fake := exectest.New()
	fake.SetDefault("", nil)
	m := NewLocalMounter(mount.New(fake))
	cfg := Config{Type: TypeLocal, LocalPath: "/srv/data"}
	if err := m.Mount(context.Background(), "/var/lib/fileblock/stores/abc", cfg); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	// pkg/mount.BindMount calls `mount --bind <src> <target>` with no
	// readOnly remount when readOnly=false. Expect exactly one call.
	if len(fake.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %+v", len(fake.Calls), fake.Calls)
	}
	c := fake.Calls[0]
	if c.Name != "mount" {
		t.Errorf("cmd = %q, want mount", c.Name)
	}
	if !equalArgs(c.Args, []string{"--bind", "/srv/data", "/var/lib/fileblock/stores/abc"}) {
		t.Errorf("args = %v", c.Args)
	}
}

func TestLocalMounterRejectsNonLocalConfig(t *testing.T) {
	m := NewLocalMounter(mount.New(exectest.New()))
	cfg := Config{Type: TypeNFS}
	err := m.Mount(context.Background(), "/x", cfg)
	if err == nil {
		t.Fatal("expected error for non-local config")
	}
}

func TestLocalMounterRejectsEmptyLocalPath(t *testing.T) {
	m := NewLocalMounter(mount.New(exectest.New()))
	cfg := Config{Type: TypeLocal}
	err := m.Mount(context.Background(), "/x", cfg)
	if err == nil {
		t.Fatal("expected error for empty LocalPath")
	}
}

func TestLocalMounterSurfacesExecError(t *testing.T) {
	fake := exectest.New()
	fake.SetDefault("permission denied", errors.New("exit 32"))
	m := NewLocalMounter(mount.New(fake))
	cfg := Config{Type: TypeLocal, LocalPath: "/srv/data"}
	err := m.Mount(context.Background(), "/x", cfg)
	if err == nil {
		t.Fatal("expected error")
	}
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/store/... -run TestLocalMounter`
Expected: FAIL — `NewLocalMounter` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `pkg/store/mounter_local.go`:

```go
package store

import (
	"context"
	"fmt"

	"github.com/middlendian/fileblock-csi/pkg/mount"
)

// LocalMounter bind-mounts a host directory into the per-store target
// via pkg/mount.Mounter.BindMount — the same code path NodePublishVolume
// uses. The host directory must be visible inside the driver pod's
// mount namespace; with the default emptyDir cache mount, operators
// adding type=local SCs need to add a hostPath patch on the controller
// Deployment and node DaemonSet to surface that source dir into the
// pod (see README "Local backing-store: required overlay").
type LocalMounter struct {
	mnt *mount.Mounter
}

func NewLocalMounter(m *mount.Mounter) *LocalMounter {
	return &LocalMounter{mnt: m}
}

func (m *LocalMounter) Mount(ctx context.Context, target string, cfg Config) error {
	if cfg.Type != TypeLocal {
		return fmt.Errorf("LocalMounter: cfg.Type = %q, want local", cfg.Type)
	}
	if cfg.LocalPath == "" {
		return fmt.Errorf("LocalMounter: cfg.LocalPath is empty")
	}
	return m.mnt.BindMount(ctx, cfg.LocalPath, target, false)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/store/... -run TestLocalMounter -v`
Expected: PASS — all four tests.

- [ ] **Step 5: Commit**

```bash
git add pkg/store/mounter_local.go pkg/store/mounter_local_test.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "store: LocalMounter via mount --bind"
```

### Task 2.3: Implement `NFSMounter` (mount.nfs)

**Files:**
- Create: `pkg/store/mounter_nfs.go`
- Create: `pkg/store/mounter_nfs_test.go`

- [ ] **Step 1: Write the failing test**

Create `pkg/store/mounter_nfs_test.go`:

```go
package store

import (
	"context"
	"errors"
	"testing"

	"github.com/middlendian/fileblock-csi/pkg/exec/exectest"
)

func TestNFSMounterCallsMountNFS(t *testing.T) {
	fake := exectest.New()
	fake.SetDefault("", nil)
	m := NewNFSMounter(fake)
	cfg := Config{
		Type:            TypeNFS,
		NFSServer:       "nfs.example.internal",
		NFSPath:         "/exports/fileblock",
		NFSMountOptions: "nfsvers=4.1,hard",
	}
	if err := m.Mount(context.Background(), "/var/lib/fileblock/stores/abc", cfg); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %+v", len(fake.Calls), fake.Calls)
	}
	c := fake.Calls[0]
	if c.Name != "mount.nfs" {
		t.Errorf("cmd = %q, want mount.nfs", c.Name)
	}
	wantArgs := []string{"-o", "nfsvers=4.1,hard", "nfs.example.internal:/exports/fileblock", "/var/lib/fileblock/stores/abc"}
	if !equalArgs(c.Args, wantArgs) {
		t.Errorf("args = %v\n want %v", c.Args, wantArgs)
	}
}

func TestNFSMounterOmitsOptsWhenEmpty(t *testing.T) {
	fake := exectest.New()
	fake.SetDefault("", nil)
	m := NewNFSMounter(fake)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	if err := m.Mount(context.Background(), "/t", cfg); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	c := fake.Calls[0]
	wantArgs := []string{"s:/p", "/t"}
	if !equalArgs(c.Args, wantArgs) {
		t.Errorf("args = %v\n want %v", c.Args, wantArgs)
	}
}

func TestNFSMounterV3Variant(t *testing.T) {
	fake := exectest.New()
	fake.SetDefault("", nil)
	m := NewNFSMounter(fake)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSMountOptions: "nfsvers=3"}
	if err := m.Mount(context.Background(), "/t", cfg); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	c := fake.Calls[0]
	wantArgs := []string{"-o", "nfsvers=3", "s:/p", "/t"}
	if !equalArgs(c.Args, wantArgs) {
		t.Errorf("args = %v\n want %v", c.Args, wantArgs)
	}
}

func TestNFSMounterRejectsNonNFS(t *testing.T) {
	m := NewNFSMounter(exectest.New())
	if err := m.Mount(context.Background(), "/t", Config{Type: TypeLocal}); err == nil {
		t.Fatal("expected error for non-nfs config")
	}
}

func TestNFSMounterRequiresServerAndPath(t *testing.T) {
	m := NewNFSMounter(exectest.New())
	if err := m.Mount(context.Background(), "/t", Config{Type: TypeNFS, NFSPath: "/p"}); err == nil {
		t.Fatal("expected error for missing server")
	}
	if err := m.Mount(context.Background(), "/t", Config{Type: TypeNFS, NFSServer: "s"}); err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestNFSMounterSurfacesExecError(t *testing.T) {
	fake := exectest.New()
	fake.SetDefault("server unreachable", errors.New("exit 32"))
	m := NewNFSMounter(fake)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	if err := m.Mount(context.Background(), "/t", cfg); err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/store/... -run TestNFSMounter`
Expected: FAIL — `NewNFSMounter` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `pkg/store/mounter_nfs.go`:

```go
package store

import (
	"context"
	"fmt"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
)

// NFSMounter shells out to mount.nfs (the generic helper from
// nfs-common). The same binary handles both NFSv3 and NFSv4 — the
// version is selected via the nfsvers= option, or auto-negotiated when
// omitted.
type NFSMounter struct {
	exec fbexec.Runner
}

func NewNFSMounter(r fbexec.Runner) *NFSMounter {
	return &NFSMounter{exec: r}
}

func (m *NFSMounter) Mount(ctx context.Context, target string, cfg Config) error {
	if cfg.Type != TypeNFS {
		return fmt.Errorf("NFSMounter: cfg.Type = %q, want nfs", cfg.Type)
	}
	if cfg.NFSServer == "" {
		return fmt.Errorf("NFSMounter: cfg.NFSServer is empty")
	}
	if cfg.NFSPath == "" {
		return fmt.Errorf("NFSMounter: cfg.NFSPath is empty")
	}
	source := cfg.NFSServer + ":" + cfg.NFSPath
	args := make([]string, 0, 4)
	if cfg.NFSMountOptions != "" {
		args = append(args, "-o", cfg.NFSMountOptions)
	}
	args = append(args, source, target)
	if _, err := m.exec.Run(ctx, "mount.nfs", args...); err != nil {
		return fmt.Errorf("mount.nfs %s -> %s: %w", source, target, err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/store/... -run TestNFSMounter -v`
Expected: PASS — all six tests.

- [ ] **Step 5: Commit**

```bash
git add pkg/store/mounter_nfs.go pkg/store/mounter_nfs_test.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "store: NFSMounter via mount.nfs (v3 + v4 generic helper)"
```

### Task 2.4: Implement `Registry` (per-store mutex + mounted cache)

**Files:**
- Create: `pkg/store/registry.go`
- Create: `pkg/store/registry_test.go`

- [ ] **Step 1: Write the failing test**

Create `pkg/store/registry_test.go`:

```go
package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/middlendian/fileblock-csi/pkg/exec/exectest"
)

func TestRegistryGetMountsOnce(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	p1, err := reg.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Get #1: %v", err)
	}
	p2, err := reg.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Get #2: %v", err)
	}
	if p1 != p2 {
		t.Errorf("path mismatch: %q vs %q", p1, p2)
	}
	if p1 != filepath.Join(root, cfg.ID()) {
		t.Errorf("path = %q, want %q", p1, filepath.Join(root, cfg.ID()))
	}
	mountCalls := 0
	for _, c := range fake.Calls {
		if c.Name == "mount.nfs" {
			mountCalls++
		}
	}
	if mountCalls != 1 {
		t.Errorf("mount.nfs called %d times, want 1", mountCalls)
	}
}

func TestRegistryDistinctConfigsMountSeparately(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
	a := Config{Type: TypeNFS, NFSServer: "s1", NFSPath: "/p"}
	b := Config{Type: TypeNFS, NFSServer: "s2", NFSPath: "/p"}
	pa, _ := reg.Get(context.Background(), a)
	pb, _ := reg.Get(context.Background(), b)
	if pa == pb {
		t.Errorf("distinct configs returned same path: %q", pa)
	}
}

func TestRegistryConcurrentGetSerializes(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reg.Get(context.Background(), cfg); err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}
	wg.Wait()
	mountCalls := 0
	for _, c := range fake.Calls {
		if c.Name == "mount.nfs" {
			mountCalls++
		}
	}
	if mountCalls != 1 {
		t.Errorf("under concurrent Get, mount.nfs called %d times, want 1", mountCalls)
	}
}

func TestRegistryRejectsUnknownType(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
	_, err := reg.Get(context.Background(), Config{Type: "smb"})
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestRegistryConfigByStoreID(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	if _, ok := reg.ConfigByStoreID(cfg.ID()); ok {
		t.Fatal("ConfigByStoreID returned true before any Get")
	}
	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	got, ok := reg.ConfigByStoreID(cfg.ID())
	if !ok {
		t.Fatal("ConfigByStoreID returned false after Get")
	}
	if got != cfg {
		t.Errorf("config mismatch: got %+v, want %+v", got, cfg)
	}
}

func TestRegistryDoesNotCacheOnMountFailure(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	// FakeRunner.Func runs in place of the rules table when set. It must
	// not touch fake.Calls — FakeRunner.Run already records the call
	// under its own mutex before invoking Func.
	var calls atomic.Int32
	fake.Func = func(ctx context.Context, name string, args ...string) (string, error) {
		n := calls.Add(1)
		if n == 1 {
			return "boom", errors.New("mount failed")
		}
		return "", nil
	}
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	if _, err := reg.Get(context.Background(), cfg); err == nil {
		t.Fatal("expected first Get to fail")
	}
	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatalf("expected second Get to succeed, got %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("mount called %d times, want 2 (first failed, second retried)", calls.Load())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/store/... -run TestRegistry`
Expected: FAIL — `NewRegistry` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `pkg/store/registry.go`:

```go
package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Registry mounts each unique store config once per process and hands
// out the resulting paths. Concurrency: per-storeID Mutex prevents two
// callers from racing into Mount; the global mu only guards the mounted
// map and the per-store mutex/config maps.
type Registry struct {
	root      string
	nfsM      Mounter
	localM    Mounter

	mu       sync.Mutex
	mounted  map[string]string         // storeID -> mounted path
	configs  map[string]Config         // storeID -> Config that produced this storeID
	storeMu  map[string]*sync.Mutex    // storeID -> per-store lock
}

// NewRegistry returns a Registry that mounts under root. mounters may be
// nil if the corresponding type is not supported in this binary.
func NewRegistry(root string, nfs Mounter, local Mounter) *Registry {
	return &Registry{
		root:    root,
		nfsM:    nfs,
		localM:  local,
		mounted: map[string]string{},
		configs: map[string]Config{},
		storeMu: map[string]*sync.Mutex{},
	}
}

// Get ensures cfg's source is mounted under <root>/<storeID>/ and
// returns that path. Idempotent: subsequent calls with the same cfg fast
// path on the cached mount. Also caches cfg keyed by storeID so callers
// that hold only a storeID (controller's DeleteVolume / Expand path) can
// resolve back to a Config via ConfigByStoreID.
func (r *Registry) Get(ctx context.Context, cfg Config) (string, error) {
	id := cfg.ID()
	storeMu := r.lockStore(id)
	storeMu.Lock()
	defer storeMu.Unlock()

	r.mu.Lock()
	if path, ok := r.mounted[id]; ok {
		r.mu.Unlock()
		return path, nil
	}
	r.mu.Unlock()

	target := filepath.Join(r.root, id)
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
	r.mounted[id] = target
	r.configs[id] = cfg
	r.mu.Unlock()
	return target, nil
}

// ConfigByStoreID returns the Config that produced the given storeID,
// if this Registry has seen it (i.e. Get(cfg) where cfg.ID() == id has
// previously succeeded in this process). Used by the controller to
// resolve DeleteVolume and ControllerExpandVolume — both of which carry
// only a volumeID, with the storeID encoded in the volumeID prefix.
func (r *Registry) ConfigByStoreID(id string) (Config, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg, ok := r.configs[id]
	return cfg, ok
}

func (r *Registry) lockStore(id string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.storeMu[id]; ok {
		return m
	}
	m := &sync.Mutex{}
	r.storeMu[id] = m
	return m
}

func (r *Registry) mounterFor(t Type) (Mounter, error) {
	switch t {
	case TypeNFS:
		if r.nfsM == nil {
			return nil, fmt.Errorf("nfs mounter not available")
		}
		return r.nfsM, nil
	case TypeLocal:
		if r.localM == nil {
			return nil, fmt.Errorf("local mounter not available")
		}
		return r.localM, nil
	default:
		return nil, fmt.Errorf("unknown backing-store type %q", t)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/store/... -v`
Expected: PASS — all chunk-1 + chunk-2 tests. Run with `-race`:

```bash
go test -race ./pkg/store/...
```
Expected: PASS, no race warnings.

- [ ] **Step 5: Commit**

```bash
git add pkg/store/registry.go pkg/store/registry_test.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "store: Registry with per-store mutex and mounted cache"
```

---

## Chunk 3: Wire Registry into the controller

This chunk threads the new `pkg/store` package through `pkg/driver/controller.go`. The legacy `backingStorePath` field and `ParamBackingStorePath` constant are removed; SC parameters drive the per-volume backing path; topology is selected per-cfg.

### Task 3.1: Add Registry to `ControllerServer` and remove `backingStorePath`

**Files:**
- Modify: `pkg/driver/controller.go`
- Modify: `pkg/driver/controller_test.go`

- [ ] **Step 1: Update the test setup to construct with a Registry**

The fake-images path stays; add a fake Registry-like helper. In `controller_test.go`, replace the existing `newControllerServer` helper (or inline construction) with one that takes a `*store.Registry`. Concretely, add to `controller_test.go`:

```go
import "github.com/middlendian/fileblock-csi/pkg/store"
import "github.com/middlendian/fileblock-csi/pkg/exec/exectest"

func newTestRegistry(t *testing.T) *store.Registry {
	t.Helper()
	fake := exectest.New()
	fake.SetDefault("", nil)
	return store.NewRegistry(t.TempDir(), store.NewNFSMounter(fake), store.NewLocalMounter(mount.New(fake)))
}
```

And update existing tests that construct a `ControllerServer` to use the new constructor signature (next step). The image manager API stays — `image.New` is still called per-volume, but inside `CreateVolume` instead of in `cmd/controller`. The fake-images path needs adjustment: the controller now calls `image.New(path, exec)` itself, so the test must inject the image factory. Introduce an `imageFactory func(path string, exec fbexec.Runner) (image.Manager, error)` field on `ControllerServer` defaulting to `image.New`, and override it in tests with one that returns the existing `fakeImages`.

(See the controller refactor in Step 3 below — this is the supporting test-side change.)

- [ ] **Step 2: Update `ControllerServer` struct, constructor, and image-factory hook**

In `pkg/driver/controller.go`:

Replace:
```go
type ControllerServer struct {
	csi.UnimplementedControllerServer
	images           image.Manager
	backingStorePath string
	topologyKey string
}

// NewControllerServer constructs a ControllerServer. topologyKey may be empty;
// it defaults to TopologyKeyNode (per-node pin).
func NewControllerServer(images image.Manager, backingStorePath, topologyKey string) *ControllerServer {
	if topologyKey == "" {
		topologyKey = TopologyKeyNode
	}
	return &ControllerServer{
		images:           images,
		backingStorePath: backingStorePath,
		topologyKey:      topologyKey,
	}
}
```

With:
```go
type imageFactory func(backingStorePath string, exec fbexec.Runner) (image.Manager, error)

type ControllerServer struct {
	csi.UnimplementedControllerServer
	registry *store.Registry
	exec     fbexec.Runner
	newImages imageFactory
}

// NewControllerServer constructs a ControllerServer. The registry routes
// per-StorageClass backing-store configs to mounted directories; the
// image manager for each volume is built lazily inside CreateVolume from
// the per-store path.
func NewControllerServer(reg *store.Registry, r fbexec.Runner) *ControllerServer {
	return &ControllerServer{
		registry:  reg,
		exec:      r,
		newImages: image.New,
	}
}
```

Add imports for `fbexec "github.com/middlendian/fileblock-csi/pkg/exec"` and `"github.com/middlendian/fileblock-csi/pkg/store"`.

Remove the `ParamBackingStorePath` and `TopologyKeyNode` constants and the comment block above them — they move to `pkg/store` (already there as `ParamType` etc.) and to a small driver-internal const respectively. Re-add `TopologyKeyNode` as an unexported constant in `controller.go` since it's still used for the local-pin topology segment:

```go
// topologyKeyNode is the segment key reported by the node plugin. The
// controller echoes it back when pinning local-type volumes to the
// provisioner's preferred node.
const topologyKeyNode = "fileblock.csi/node"
```

- [ ] **Step 3: Update `CreateVolume` to parse SC params, route via Registry, build per-store image manager**

Replace the body of `CreateVolume` from the line after `validateCapabilities(...)` through the end. Updated body (in full):

```go
func (c *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := validateCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, err
	}

	cfg, err := store.ConfigFromParams(req.GetParameters())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	mountedPath, err := c.registry.Get(ctx, cfg)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "mount backing store: %v", err)
	}
	images, err := c.newImages(mountedPath, c.exec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "open backing store: %v", err)
	}

	capacity := defaultCapacityBytes
	if r := req.GetCapacityRange(); r != nil {
		if r.RequiredBytes > 0 {
			capacity = int(r.RequiredBytes)
		} else if r.LimitBytes > 0 {
			capacity = int(r.LimitBytes)
		}
	}

	volumeID, err := volumeIDFromName(cfg, req.GetName())
	if err != nil {
		return nil, err
	}
	meta, err := images.Create(ctx, volumeID, int64(capacity))
	if err != nil {
		var mismatch *image.CapacityMismatchError
		if errors.As(err, &mismatch) {
			return nil, status.Error(codes.AlreadyExists, mismatch.Error())
		}
		return nil, status.Errorf(codes.Internal, "create volume: %v", err)
	}

	vol := &csi.Volume{
		VolumeId:      meta.VolumeID,
		CapacityBytes: meta.CapacityBytes,
		VolumeContext: cfg.ToVolumeContext(),
	}
	vol.AccessibleTopology = topologyForCfg(cfg, req.GetAccessibilityRequirements())
	return &csi.CreateVolumeResponse{Volume: vol}, nil
}
```

Add the topology helper inside the same file:

```go
// topologyForCfg returns AccessibleTopology that the external-provisioner
// will honor: empty for nfs (any node), pinned to the provisioner's
// preferred segment for local (matches today's per-node behavior).
func topologyForCfg(cfg store.Config, req *csi.TopologyRequirement) []*csi.Topology {
	if cfg.Type == store.TypeNFS {
		return nil
	}
	if req == nil {
		return nil
	}
	if pref := req.GetPreferred(); len(pref) > 0 {
		return []*csi.Topology{pref[0]}
	}
	if reqs := req.GetRequisite(); len(reqs) > 0 {
		return []*csi.Topology{reqs[0]}
	}
	return nil
}
```

Update `DeleteVolume`, `ValidateVolumeCapabilities`, `ControllerExpandVolume`, `ListVolumes` — they previously used `c.images`, which is gone. The CSI spec doesn't give them `parameters`, only `volume_id`. To resolve the right store, **encode `storeID` in the volumeID** so the volumeID is self-locating.

**volumeID layout:** `fb-<storeID>-<name>` where `<storeID>` is the 12-char hex hash. The controller parses the storeID back out, looks up the Config in `Registry.ConfigByStoreID`, calls `Registry.Get(cfg)` to ensure the store is mounted, and constructs `image.Manager` against that path. Two SCs against distinct backing stores cannot collide on volumeID; SCs against the same backing store (same storeID) coexist correctly because `image.Create` is idempotent on `(volumeID, capacity)`.

**Limitation, documented in CHANGELOG:** if the controller pod restarts between `CreateVolume` and a later `DeleteVolume` / `ControllerExpandVolume` for the same volume, `Registry.ConfigByStoreID` returns `false` until something re-registers the Config (e.g. a `CreateVolume` against the same SC). external-provisioner retries `DeleteVolume` indefinitely, so the operation eventually succeeds; document the symptom (PVC stuck in `Released` until the next reconcile) and the recovery (restart provisioner sidecar, or create a no-op PVC against the same SC).

Update `volumeIDFromName` to take a Config:

```go
// volumeIDFromName produces the on-disk volume ID for a given CSI request
// name. The mapping is deterministic on (cfg, name): the same name +
// same backing store always yields the same ID, so a retried CreateVolume
// call lands on the existing image instead of minting a fresh one. The
// "fb-<storeID>-" prefix lets DeleteVolume / Expand resolve the
// volumeID's home store without parameters in the request.
func volumeIDFromName(cfg store.Config, name string) (string, error) {
	if strings.ContainsAny(name, "/\\\x00-") {
		return "", status.Errorf(codes.InvalidArgument, "name %q contains invalid characters", name)
	}
	return "fb-" + cfg.ID() + "-" + name, nil
}

// parseStoreIDFromVolumeID extracts the 12-char storeID from a volumeID
// produced by volumeIDFromName.
func parseStoreIDFromVolumeID(volumeID string) (string, error) {
	const prefix = "fb-"
	if !strings.HasPrefix(volumeID, prefix) {
		return "", status.Errorf(codes.InvalidArgument, "volumeID %q is not in fb-<storeID>-<name> form", volumeID)
	}
	rest := volumeID[len(prefix):]
	if len(rest) < 13 || rest[12] != '-' {
		return "", status.Errorf(codes.InvalidArgument, "volumeID %q has malformed storeID segment", volumeID)
	}
	return rest[:12], nil
}
```

Note the addition of `'-'` to the disallowed-name characters: the volumeID format uses `-` as the storeID separator, so allowing `-` in `name` would create ambiguity. v0.2.0 allowed `-` in names; the tightening is acknowledged in the CHANGELOG breaking section.

Update `CreateVolume` to pass `cfg` to `volumeIDFromName(cfg, req.GetName())`.

Add a `imageManagerForVolumeID` helper:

```go
// imageManagerForVolumeID resolves a volumeID's home store and returns
// an image.Manager over its mounted path. Returns NotFound if the
// store has not been seen by this controller process (typical after a
// controller-pod restart before CreateVolume re-populates the Registry
// for that storeID — see CHANGELOG migration note).
func (c *ControllerServer) imageManagerForVolumeID(ctx context.Context, volumeID string) (image.Manager, error) {
	storeID, err := parseStoreIDFromVolumeID(volumeID)
	if err != nil {
		return nil, err
	}
	cfg, ok := c.registry.ConfigByStoreID(storeID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "store %s for volume %s is not mounted on this controller; retry after the SC is in use", storeID, volumeID)
	}
	path, err := c.registry.Get(ctx, cfg)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "remount backing store %s: %v", storeID, err)
	}
	return c.newImages(path, c.exec)
}
```

Use it in:

- **`DeleteVolume`:** `m, err := c.imageManagerForVolumeID(ctx, req.GetVolumeId()); ... m.Delete(ctx, volumeID)`.
- **`ValidateVolumeCapabilities`:** `m, err := c.imageManagerForVolumeID(...); ... m.Get(ctx, volumeID)` (the existing logic, now per-store).
- **`ControllerExpandVolume`:** same — `m, err := c.imageManagerForVolumeID(...); m.Resize(ctx, volumeID, r.RequiredBytes)`.

For `ListVolumes`, iterate `MountedPaths()`:

```go
func (c *ControllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	if req.GetStartingToken() != "" {
		return nil, status.Error(codes.Aborted, "starting_token is not supported")
	}
	out := []*csi.ListVolumesResponse_Entry{}
	for _, path := range c.registry.MountedPaths() {
		m, err := c.newImages(path, c.exec)
		if err != nil {
			continue
		}
		metas, err := m.List(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list: %v", err)
		}
		for _, meta := range metas {
			out = append(out, &csi.ListVolumesResponse_Entry{
				Volume: &csi.Volume{
					VolumeId:      meta.VolumeID,
					CapacityBytes: meta.CapacityBytes,
				},
			})
		}
	}
	return &csi.ListVolumesResponse{Entries: out}, nil
}
```

`MountedPaths()` is added on the Registry alongside `ConfigByStoreID`:

```go
// MountedPaths returns the absolute paths of every store currently
// mounted by this Registry. Order is unspecified.
func (r *Registry) MountedPaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.mounted))
	for _, p := range r.mounted {
		out = append(out, p)
	}
	return out
}
```

(Add a small test in `registry_test.go` mirroring `TestRegistryConfigByStoreID`.)

`VolumeContext` is omitted on `ListVolumes` entries because the controller no longer holds a per-volume cfg; v0.2.0 behavior change documented in CHANGELOG.

- [ ] **Step 4: Run tests to verify the controller still compiles and existing tests pass**

Run: `go build ./... && go test ./pkg/driver/... -v`
Expected: existing tests have been updated to construct with a Registry and image-factory override; all pass. If old tests reference `c.images`, they fail to compile and must be migrated to the new shape (next task).

- [ ] **Step 5: Commit**

```bash
git add pkg/driver/controller.go pkg/driver/controller_test.go pkg/store/registry.go pkg/store/registry_test.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "controller: route via store.Registry; drop backingStorePath field"
```

### Task 3.2: Update `controller_test.go` to cover NFS + local + topology branches

**Files:**
- Modify: `pkg/driver/controller_test.go`

- [ ] **Step 1: Replace existing CreateVolume tests with cases that exercise the new SC schema**

Update or replace the existing CreateVolume tests so each case constructs SC parameters (not a backing path) and verifies:

1. `CreateVolume` with `nfs` SC → `vol.AccessibleTopology` is empty (or nil).
2. `CreateVolume` with `local` SC + `req.AccessibilityRequirements.Preferred = [{node: N}]` → `vol.AccessibleTopology = [{node: N}]`.
3. `CreateVolume` with missing `backingStore.type` → returns `InvalidArgument`.
4. Volume context returned matches `cfg.ToVolumeContext()`.

A representative test (others follow the same pattern):

```go
func TestCreateVolumeNFSReturnsEmptyTopology(t *testing.T) {
	reg := newTestRegistry(t)
	imgFac := func(path string, _ fbexec.Runner) (image.Manager, error) {
		return newFakeImages(), nil
	}
	c := NewControllerServer(reg, nil); c.newImages = imgFac

	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "v1",
		Parameters: map[string]string{
			store.ParamType:      "nfs",
			store.ParamNFSServer: "s",
			store.ParamNFSPath:   "/p",
		},
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if len(resp.Volume.AccessibleTopology) != 0 {
		t.Errorf("AccessibleTopology = %v, want empty", resp.Volume.AccessibleTopology)
	}
	if resp.Volume.VolumeContext[store.ParamType] != "nfs" {
		t.Errorf("VolumeContext = %v", resp.Volume.VolumeContext)
	}
}

func TestCreateVolumeLocalPinsToPreferredNode(t *testing.T) {
	reg := newTestRegistry(t)
	imgFac := func(path string, _ fbexec.Runner) (image.Manager, error) {
		return newFakeImages(), nil
	}
	c := NewControllerServer(reg, nil); c.newImages = imgFac

	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "v1",
		Parameters: map[string]string{
			store.ParamType:      "local",
			store.ParamLocalPath: "/srv/data",
		},
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Preferred: []*csi.Topology{{Segments: map[string]string{"fileblock.csi/node": "node-a"}}},
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if len(resp.Volume.AccessibleTopology) != 1 {
		t.Fatalf("AccessibleTopology = %v", resp.Volume.AccessibleTopology)
	}
	if resp.Volume.AccessibleTopology[0].Segments["fileblock.csi/node"] != "node-a" {
		t.Errorf("pinned segment = %v", resp.Volume.AccessibleTopology[0].Segments)
	}
}

func TestCreateVolumeMissingTypeIsInvalidArgument(t *testing.T) {
	reg := newTestRegistry(t)
	c := NewControllerServer(reg, nil); c.newImages = func(string, fbexec.Runner) (image.Manager, error) { return newFakeImages(), nil }
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "v1",
		Parameters:         map[string]string{},
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}
```

`singleNodeWriterMount()` is a small helper for assembling a valid `VolumeCapability`:

```go
func singleNodeWriterMount() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}
```

Migrate or delete pre-existing tests that referenced `c.images` directly — those access patterns are gone.

- [ ] **Step 2: Run the test suite**

Run: `go test ./pkg/driver/... -v`
Expected: PASS — all rewritten controller tests.

- [ ] **Step 3: Commit**

```bash
git add pkg/driver/controller_test.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "controller: tests cover nfs/local CreateVolume topology branches"
```

### Task 3.3: Patch `cmd/controller/main.go` so the project still builds

A *placeholder* update keeping the codebase buildable between Chunk 3 and Chunk 5. The full flag cleanup happens in Chunk 5 Task 5.1; this patch only swaps the constructor to the new signature so `go build ./...` succeeds at every commit boundary.

**Files:**
- Modify: `cmd/controller/main.go`

- [ ] **Step 1: Replace the controller construction block**

Find:
```go
exec := fbexec.New(0)
images, err := image.New(*backingStore, exec)
if err != nil {
	log.Error("open backing store", "err", err)
	os.Exit(2)
}

identity := driver.NewIdentityServer(true)
controller := driver.NewControllerServer(images, *backingStore, *topologyKey)
```

Replace with:
```go
exec := fbexec.New(0)
mnt := mount.New(exec)
storesRoot := "/var/lib/fileblock/stores"
if err := os.MkdirAll(storesRoot, 0o755); err != nil {
	log.Error("create stores root", "err", err)
	os.Exit(2)
}
registry := store.NewRegistry(storesRoot, store.NewNFSMounter(exec), store.NewLocalMounter(mnt))
_ = backingStore // unused until Chunk 5 removes the flag
_ = topologyKey  // unused until Chunk 5 removes the flag

identity := driver.NewIdentityServer(true)
controller := driver.NewControllerServer(registry, exec)
```

Add imports `"github.com/middlendian/fileblock-csi/pkg/store"` and `"github.com/middlendian/fileblock-csi/pkg/mount"`. Drop the `pkg/image` import.

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: succeeds.

- [ ] **Step 3: Commit**

```bash
git add cmd/controller/main.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "cmd/controller: swap to store.Registry constructor (flags cleanup deferred)"
```

---

## Chunk 4: Wire Registry into the node

This chunk threads `pkg/store` through `pkg/driver/node.go`. The `topologyKey`/`topologyValue` fields collapse to a single hard-coded segment key; the volume-context shape changes; the staging path now comes from `Registry.Get`.

### Task 4.1: Update `NodeServer` struct and constructor

**Files:**
- Modify: `pkg/driver/node.go`

- [ ] **Step 1: Replace constructor and struct**

Replace:

```go
type NodeServer struct {
	csi.UnimplementedNodeServer

	nodeID  string
	exec    fbexec.Runner
	mnt     *mount.Mounter
	losetup *loop.Losetup
	state   *loop.State
	log     *slog.Logger

	topologyKey   string
	topologyValue string

	mu       sync.Mutex
	volMutex map[string]*sync.Mutex
}

func NewNodeServer(nodeID string, exec fbexec.Runner, mnt *mount.Mounter, ls *loop.Losetup, st *loop.State, log *slog.Logger, topologyKey, topologyValue string) *NodeServer {
	if topologyKey == "" {
		topologyKey = TopologyKeyNode
	}
	if topologyValue == "" {
		topologyValue = nodeID
	}
	return &NodeServer{ ... }
}
```

With:

```go
type NodeServer struct {
	csi.UnimplementedNodeServer

	nodeID   string
	exec     fbexec.Runner
	mnt      *mount.Mounter
	losetup  *loop.Losetup
	state    *loop.State
	log      *slog.Logger
	registry *store.Registry

	mu       sync.Mutex
	volMutex map[string]*sync.Mutex
}

// NewNodeServer constructs a NodeServer wired to a Registry that the
// node uses to mount each PV's backing store on demand inside this pod.
// Topology is fixed: every node reports {fileblock.csi/node: nodeID}.
// The controller decides whether a volume is shared (any node OK) or
// pinned (single node) at CreateVolume time.
func NewNodeServer(nodeID string, exec fbexec.Runner, mnt *mount.Mounter, ls *loop.Losetup, st *loop.State, log *slog.Logger, reg *store.Registry) *NodeServer {
	return &NodeServer{
		nodeID:   nodeID,
		exec:     exec,
		mnt:      mnt,
		losetup:  ls,
		state:    st,
		log:      log,
		registry: reg,
		volMutex: map[string]*sync.Mutex{},
	}
}
```

Add the import for `pkg/store`.

- [ ] **Step 2: Update `NodeGetInfo` to report a single hard-coded segment**

Replace:
```go
AccessibleTopology: &csi.Topology{
	Segments: map[string]string{n.topologyKey: n.topologyValue},
},
```

With:
```go
AccessibleTopology: &csi.Topology{
	Segments: map[string]string{topologyKeyNode: n.nodeID},
},
```

Where `topologyKeyNode` is the unexported constant added in `controller.go`. Ensure it is shared (move to a small `topology.go` in the same package, or duplicate in node.go — duplicate is fine; both files mention it explicitly).

- [ ] **Step 3: Update `NodeStageVolume` to parse the new volume context and call Registry**

Replace the `backing := req.GetVolumeContext()[ParamBackingStorePath]` line and the `backing == ""` guard with:

```go
cfg, err := store.ConfigFromVolumeContext(req.GetVolumeContext())
if err != nil {
	return nil, status.Errorf(codes.InvalidArgument, "%v", err)
}
backing, err := n.registry.Get(ctx, cfg)
if err != nil {
	return nil, status.Errorf(codes.Internal, "mount backing store: %v", err)
}
```

The rest of `NodeStageVolume` (image manager construction, losetup, fsck, resize2fs, mount, state.Put) is unchanged — `backing` flows in the same way.

- [ ] **Step 4: Run tests to verify the package still compiles**

Run: `go build ./pkg/driver/...`
Expected: succeeds. Tests will fail until updated in Task 4.2.

- [ ] **Step 5: Commit**

```bash
git add pkg/driver/node.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "node: route NodeStageVolume via store.Registry; collapse topology to single segment"
```

### Task 4.2: Update `node_test.go`

**Files:**
- Modify: `pkg/driver/node_test.go`

- [ ] **Step 1: Update test setup for the new constructor**

Helper:

```go
func newTestNodeRegistry(t *testing.T) *store.Registry {
	t.Helper()
	fake := exectest.New()
	fake.SetDefault("", nil)
	return store.NewRegistry(t.TempDir(), store.NewNFSMounter(fake), store.NewLocalMounter(mount.New(fake)))
}
```

Update existing tests that constructed `NodeServer` with the old signature.

- [ ] **Step 2: Add a test for NodeGetInfo's topology**

```go
func TestNodeGetInfoReportsNodeSegment(t *testing.T) {
	n := NewNodeServer("nodeA", nil, nil, nil, nil, nil, nil)
	resp, err := n.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AccessibleTopology.Segments["fileblock.csi/node"] != "nodeA" {
		t.Errorf("segments = %v", resp.AccessibleTopology.Segments)
	}
	if len(resp.AccessibleTopology.Segments) != 1 {
		t.Errorf("expected exactly one segment, got %v", resp.AccessibleTopology.Segments)
	}
}
```

- [ ] **Step 3: Add a NodeStageVolume test that exercises the new volume-context parsing**

```go
func TestNodeStageVolumeRejectsMissingBackingStoreType(t *testing.T) {
	n := NewNodeServer("nodeA", nil, nil, nil, nil, nil, newTestNodeRegistry(t))
	_, err := n.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "v1",
		StagingTargetPath: t.TempDir(),
		VolumeContext:     map[string]string{},
		VolumeCapability:  singleNodeWriterMount(),
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v", st.Code())
	}
}
```

A full happy-path NodeStageVolume test requires mocking losetup, e2fsck, resize2fs, mount — substantial. Defer the happy path to smoke + e2e, where it's exercised against real code. The Stage tests stay focused on input-validation and idempotency.

- [ ] **Step 4: Run the suite**

Run: `go test ./pkg/driver/... -v`
Expected: PASS — all node tests with the new shape.

- [ ] **Step 5: Commit**

```bash
git add pkg/driver/node_test.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "node: tests for new constructor, single-segment topology, Stage validation"
```

### Task 4.3: Patch `cmd/node/main.go` so the project still builds

Mirror of Task 3.3 for the node binary. Placeholder; full cleanup is in Task 5.2.

**Files:**
- Modify: `cmd/node/main.go`

- [ ] **Step 1: Replace the node-construction block**

Find:
```go
identity := driver.NewIdentityServer(false)
node := driver.NewNodeServer(*nodeID, exec, mnt, losetup, state, log, *topologyKey, *topologyValue)
```

Replace with:
```go
storesRoot := "/var/lib/fileblock/stores"
if err := os.MkdirAll(storesRoot, 0o755); err != nil {
	log.Error("create stores root", "err", err)
	os.Exit(2)
}
registry := store.NewRegistry(storesRoot, store.NewNFSMounter(exec), store.NewLocalMounter(mnt))
_ = topologyKey
_ = topologyValue

identity := driver.NewIdentityServer(false)
node := driver.NewNodeServer(*nodeID, exec, mnt, losetup, state, log, registry)
```

Add `"github.com/middlendian/fileblock-csi/pkg/store"` to imports.

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: succeeds.

- [ ] **Step 3: Commit**

```bash
git add cmd/node/main.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "cmd/node: swap to store.Registry constructor (flags cleanup deferred)"
```

---

## Chunk 5: Binary entrypoints and reconciler

This chunk updates `cmd/controller/main.go` and `cmd/node/main.go` to construct a `*store.Registry`, drops the removed flags, and points `pkg/loop`'s reconciler at the parent stores directory.

### Task 5.1: Update `cmd/controller/main.go`

**Files:**
- Modify: `cmd/controller/main.go`

- [ ] **Step 1: Rewrite main()**

Replace `main()` body:

```go
func main() {
	endpoint := flag.String("endpoint", "unix:///csi/csi.sock", "CSI endpoint (unix:// or tcp://)")
	storesRoot := flag.String("stores-root", "/var/lib/fileblock/stores", "directory under which each backing store is mounted at <id>/")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	flag.Parse()

	log := newLogger(*logLevel)

	if err := os.MkdirAll(*storesRoot, 0o755); err != nil {
		log.Error("create stores root", "err", err)
		os.Exit(2)
	}

	exec := fbexec.New(0)
	mnt := mount.New(exec)
	registry := store.NewRegistry(*storesRoot, store.NewNFSMounter(exec), store.NewLocalMounter(mnt))

	identity := driver.NewIdentityServer(true)
	controller := driver.NewControllerServer(registry, exec)
	srv := driver.NewServer(*endpoint, log, identity, controller, nil)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := srv.Serve(ctx); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
```

Update imports: drop `pkg/image` (no longer used here), add `pkg/store`. The `--backing-store` and `--topology-key` flags are gone.

- [ ] **Step 2: Build**

Run: `go build ./cmd/controller`
Expected: succeeds.

- [ ] **Step 3: Commit**

```bash
git add cmd/controller/main.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "cmd/controller: drop --backing-store/--topology-key; wire store.Registry"
```

### Task 5.2: Update `cmd/node/main.go`

**Files:**
- Modify: `cmd/node/main.go`

- [ ] **Step 1: Rewrite main()**

Replace `main()` body:

```go
func main() {
	endpoint := flag.String("endpoint", "unix:///csi/csi.sock", "CSI endpoint (unix:// or tcp://)")
	nodeID := flag.String("node-id", os.Getenv("NODE_NAME"), "node identifier; defaults to $NODE_NAME")
	stateDir := flag.String("state-dir", "/var/lib/kubelet/plugins/fileblock.csi", "directory for the loop-mappings state file")
	storesRoot := flag.String("stores-root", "/var/lib/fileblock/stores", "directory under which each backing store is mounted at <id>/")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	flag.Parse()

	log := newLogger(*logLevel)

	if *nodeID == "" {
		log.Error("--node-id (or $NODE_NAME) is required")
		os.Exit(2)
	}
	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		log.Error("create state dir", "err", err)
		os.Exit(2)
	}
	if err := os.MkdirAll(*storesRoot, 0o755); err != nil {
		log.Error("create stores root", "err", err)
		os.Exit(2)
	}

	exec := fbexec.New(0)
	mnt := mount.New(exec)
	losetup := loop.NewLosetup(exec)
	state, err := loop.LoadState(filepath.Join(*stateDir, "loop-mappings.json"))
	if err != nil {
		log.Error("load state", "err", err)
		os.Exit(2)
	}

	// Reconcile any orphan loop devices anywhere under storesRoot. The
	// reconciler's prefix check handles all per-store subdirs uniformly.
	rec := loop.NewReconciler(state, losetup, *storesRoot)
	if err := rec.Reconcile(context.Background()); err != nil {
		log.Warn("reconcile failed at startup", "err", err)
	}

	registry := store.NewRegistry(*storesRoot, store.NewNFSMounter(exec), store.NewLocalMounter(mnt))

	identity := driver.NewIdentityServer(false)
	node := driver.NewNodeServer(*nodeID, exec, mnt, losetup, state, log, registry)
	srv := driver.NewServer(*endpoint, log, identity, nil, node)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := srv.Serve(ctx); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
```

Update imports: drop nothing, add `pkg/store`. Removed flags: `--backing-store`, `--topology-key`, `--topology-value`.

- [ ] **Step 2: Build**

Run: `go build ./cmd/node`
Expected: succeeds.

- [ ] **Step 3: Commit**

```bash
git add cmd/node/main.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "cmd/node: drop --backing-store/--topology-* flags; wire store.Registry; reconcile via storesRoot"
```

### Task 5.3: Add `Registry.AdoptExisting` for the hostPath cache variant

The spec at "Reconcile on plugin restart" requires the Registry to scan its root on init and adopt any pre-existing per-store mounts. Under the recommended `emptyDir` cache (Chunk 6) this is always a no-op, but the spec keeps it for the supported `hostPath: DirectoryOrCreate` cache variant where mount state must survive driver-pod restarts.

**Files:**
- Modify: `pkg/store/registry.go`
- Modify: `pkg/store/registry_test.go`
- Modify: `cmd/node/main.go`
- Modify: `cmd/controller/main.go`

- [ ] **Step 1: Write the failing test**

Add to `pkg/store/registry_test.go`:

```go
func TestRegistryAdoptExistingNoOpOnEmptyRoot(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
	if err := reg.AdoptExisting(); err != nil {
		t.Fatalf("AdoptExisting: %v", err)
	}
	if len(reg.MountedPaths()) != 0 {
		t.Errorf("expected empty mounted set, got %v", reg.MountedPaths())
	}
}

func TestRegistryAdoptExistingPreloadsKnownDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "abc123def456"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := exectest.New()
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
	if err := reg.AdoptExisting(); err != nil {
		t.Fatalf("AdoptExisting: %v", err)
	}
	got := reg.MountedPaths()
	if len(got) != 1 || got[0] != filepath.Join(root, "abc123def456") {
		t.Errorf("MountedPaths = %v", got)
	}
}
```

Add `"os"` and `"path/filepath"` to the existing import block (the test file already has `path/filepath`; just add `os`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/store/... -run TestRegistryAdoptExisting`
Expected: FAIL — `AdoptExisting` undefined.

- [ ] **Step 3: Implement**

Add to `pkg/store/registry.go`:

```go
// AdoptExisting walks r.root and treats every immediate subdirectory as
// a previously-mounted store, populating the mounted-paths cache so
// MountedPaths() / DeleteVolume / etc. can find them. It does NOT
// re-mount anything — the directory is assumed to still hold a live
// mount (true under hostPath caches that survive pod restarts).
//
// Caveats:
//   - Cannot reconstruct the full Config for adopted stores; only the
//     storeID is recovered (from the directory name). DeleteVolume
//     against an adopted-but-not-Get-ed store will return NotFound from
//     ConfigByStoreID. After the next CreateVolume against the SC,
//     the Config is registered and Delete works.
//   - Under emptyDir (the recommended cache shape) the directory is
//     empty after restart, so this is always a no-op.
func (r *Registry) AdoptExisting() error {
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
		r.mounted[id] = filepath.Join(r.root, id)
	}
	return nil
}
```

Add `"os"` to `pkg/store/registry.go`'s import block.

- [ ] **Step 4: Run tests**

Run: `go test ./pkg/store/... -v`
Expected: PASS — both new tests.

- [ ] **Step 5: Wire into the binaries**

In `cmd/node/main.go`, immediately after `registry := store.NewRegistry(...)`, add:

```go
if err := registry.AdoptExisting(); err != nil {
	log.Warn("adopt existing stores failed at startup", "err", err)
}
```

Same change in `cmd/controller/main.go` after the registry is created.

- [ ] **Step 6: Build + commit**

```bash
go build ./...
git add pkg/store/registry.go pkg/store/registry_test.go cmd/node/main.go cmd/controller/main.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "store: AdoptExisting for hostPath cache variant; wire into both binaries"
```

### Task 5.4: Run the full unit-test gate

- [ ] **Step 1: Confirm everything still builds and tests pass under -race**

Run:
```bash
make fmt-check vet lint tidy-check
go test -race ./...
```
Expected: PASS for all four. Address any lint or vet findings before proceeding (typically: unused imports from the controller/node refactor, or doc-comment lints on new exported symbols).

- [ ] **Step 2: Commit any cleanup**

If any cleanup commits are needed (formatting, doc comments), commit them with `chore: golangci-lint cleanup` or similar.

---

## Chunk 6: Manifests + Dockerfile

This chunk updates the deployable artifacts. After this chunk lands, `kustomize build deploy/kustomize/base` produces a working driver install (Deployment + DaemonSet + RBAC + CSIDriver) with no StorageClass; operators ship one or more SCs of their own.

### Task 6.1: `Dockerfile` — add `nfs-common`

**Files:**
- Modify: `Dockerfile`

- [ ] **Step 1: Replace the apt install line**

In `Dockerfile`, change:
```dockerfile
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        e2fsprogs util-linux ca-certificates \
 && rm -rf /var/lib/apt/lists/*
```

To:
```dockerfile
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        e2fsprogs util-linux ca-certificates nfs-common \
 && rm -rf /var/lib/apt/lists/*
```

- [ ] **Step 2: Build the image and confirm `mount.nfs` is on PATH**

Run:
```bash
make docker
docker run --rm --entrypoint=which ghcr.io/middlendian/fileblock-csi:dev mount.nfs
```
Expected: `/sbin/mount.nfs` (or similar) printed; exit 0.

Optional: also confirm `mount.nfs4`:
```bash
docker run --rm --entrypoint=which ghcr.io/middlendian/fileblock-csi:dev mount.nfs4
```
Expected: a path printed; exit 0.

- [ ] **Step 3: Commit**

```bash
git add Dockerfile
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "image: add nfs-common for in-driver NFS mounts (v3 + v4)"
```

### Task 6.2: `controller-deployment.yaml`

**Files:**
- Modify: `deploy/kustomize/base/controller-deployment.yaml`

- [ ] **Step 1: Apply the changes**

Edits:

1. In `csi-provisioner` container args, **delete** the line `- --strict-topology`.
2. In `fileblock-controller` container `args`, replace the existing block with:
   ```yaml
   args:
     - --endpoint=unix:///csi/csi.sock
     - --stores-root=/var/lib/fileblock/stores
     - --log-level=info
   ```
3. In `fileblock-controller` container, **add** a `securityContext`:
   ```yaml
   securityContext:
     capabilities:
       add: [SYS_ADMIN]
   ```
4. In `fileblock-controller` container `volumeMounts`, **delete** the `backing-store` entry. **Add** a `stores` entry:
   ```yaml
   - name: stores
     mountPath: /var/lib/fileblock/stores
   ```
5. In `volumes`, **delete** the `backing-store` volume. **Add**:
   ```yaml
   - name: stores
     emptyDir: {}
   ```

- [ ] **Step 2: Render the manifest and confirm it's valid**

Run:
```bash
kubectl kustomize deploy/kustomize/base | kubectl --dry-run=client apply -f -
```
Expected: the controller deployment validates; no errors about unknown fields or missing required values.

- [ ] **Step 3: Commit**

```bash
git add deploy/kustomize/base/controller-deployment.yaml
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "deploy: controller now mounts backing stores itself; SYS_ADMIN cap; drop --strict-topology"
```

- [ ] **Step 4 (REQUIRED): Verify `SYS_ADMIN`-only is sufficient on a real cluster**

Spec promotes this from "open question" to a required implementation step. On a kind cluster (or whichever cluster you have), apply the new manifests and exercise an NFS mount end to end:

```bash
# 1. Bring up the test stack (kind-based works; the real homelab works too).
kubectl apply -k deploy/kustomize/overlays/example-nfs-shared/
kubectl wait -n fileblock-system --for=condition=Ready pod -l app=fileblock-controller --timeout=120s

# 2. Create a PVC and Pod that reference the new SC.
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: pvc-syscheck, namespace: default}
spec:
  storageClassName: fileblock-nfs-shared
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 64Mi}}
---
apiVersion: v1
kind: Pod
metadata: {name: syscheck, namespace: default}
spec:
  containers:
    - name: c
      image: alpine:3.21
      command: [sh, -c, 'echo ok > /data/x && cat /data/x && sleep 3600']
      volumeMounts: [{name: d, mountPath: /data}]
  volumes: [{name: d, persistentVolumeClaim: {claimName: pvc-syscheck}}]
EOF
kubectl wait pod/syscheck --for=condition=Ready --timeout=120s
```

If the controller pod logs `mount.nfs ...: Operation not permitted` (EPERM), the host's LSM denies mount syscalls from a `SYS_ADMIN`-but-not-`privileged` container. **Escalate the controller container to `privileged: true`** (replace the `securityContext` block in `controller-deployment.yaml` with `securityContext: {privileged: true}`) and re-run. The spec accepts this fallback explicitly.

If everything works, no further change. Document in CHANGELOG which form was needed (e.g., "controller runs with `securityContext: {capabilities: {add: [SYS_ADMIN]}}`; if this is rejected by your host's LSM, set `privileged: true` instead").

- [ ] **Step 5: Commit any escalation, if needed**

```bash
git add deploy/kustomize/base/controller-deployment.yaml
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "deploy: controller requires privileged: true on this host (LSM rejected SYS_ADMIN-only)"
```

(If `SYS_ADMIN` was sufficient, this commit is omitted.)

### Task 6.3: `node-daemonset.yaml`

**Files:**
- Modify: `deploy/kustomize/base/node-daemonset.yaml`

- [ ] **Step 1: Apply the changes**

Edits:

1. In `fileblock-node` container `args`, replace with:
   ```yaml
   args:
     - --endpoint=unix:///csi/csi.sock
     - --node-id=$(NODE_NAME)
     - --state-dir=/var/lib/kubelet/plugins/fileblock.csi
     - --stores-root=/var/lib/fileblock/stores
     - --log-level=info
   ```
2. In `fileblock-node` container `volumeMounts`, **replace** the `backing-store` entry with:
   ```yaml
   - name: stores
     mountPath: /var/lib/fileblock/stores
   ```
   No `mountPropagation` — `emptyDir` is pod-private; the kubelet doesn't need to see per-store NFS mounts. The kubelet's view of the *staging* mount (under `/var/lib/kubelet/...`) is already covered by the existing `kubelet-dir` Bidirectional volumeMount, which is unchanged.
3. In `volumes`, replace `backing-store` with:
   ```yaml
   - name: stores
     emptyDir: {}
   ```

- [ ] **Step 2: Validate**

Run:
```bash
kubectl kustomize deploy/kustomize/base | kubectl --dry-run=client apply -f -
```
Expected: validates.

- [ ] **Step 3: Commit**

```bash
git add deploy/kustomize/base/node-daemonset.yaml
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "deploy: node now mounts backing stores itself; drop --backing-store/--topology flags"
```

### Task 6.4: Drop the base StorageClass

**Files:**
- Delete: `deploy/kustomize/base/storageclass.yaml`
- Modify: `deploy/kustomize/base/kustomization.yaml`

- [ ] **Step 1: Delete the file and remove the reference**

```bash
git rm deploy/kustomize/base/storageclass.yaml
```

In `deploy/kustomize/base/kustomization.yaml`, remove the line referencing `storageclass.yaml` from `resources:`.

- [ ] **Step 2: Validate**

```bash
kubectl kustomize deploy/kustomize/base | grep -c 'kind: StorageClass'
```
Expected: `0`. The base no longer ships an SC; operators ship their own.

- [ ] **Step 3: Commit**

```bash
git add deploy/kustomize/base/kustomization.yaml
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "deploy: drop base StorageClass; operators ship their own SC"
```

### Task 6.5: Simplify `example-localdir` overlay

**Files:**
- Delete: `deploy/kustomize/overlays/example-localdir/patch-controller.yaml` (if exists)
- Delete: `deploy/kustomize/overlays/example-localdir/patch-node.yaml` (if exists)
- Create: `deploy/kustomize/overlays/example-localdir/storageclass.yaml`
- Modify: `deploy/kustomize/overlays/example-localdir/kustomization.yaml`

- [ ] **Step 1: Inspect the current overlay**

```bash
ls deploy/kustomize/overlays/example-localdir/
cat deploy/kustomize/overlays/example-localdir/kustomization.yaml
```

- [ ] **Step 2: Replace with a minimal overlay**

Create `deploy/kustomize/overlays/example-localdir/storageclass.yaml`:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: fileblock-localdir
provisioner: fileblock.csi
parameters:
  backingStore.type: local
  backingStore.local.path: /var/lib/fileblock-localdir
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
```

Replace `deploy/kustomize/overlays/example-localdir/kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

# Single-node, host-directory backing store. Useful for kind-based tests
# and trivial dev setups. Replace `backingStore.local.path` with the host
# directory the driver should manage; it must exist on every node where
# the DaemonSet runs.
#
# Local-type also requires that the host directory be visible inside the
# driver pods' mount namespaces — the LocalMounter bind-mounts it into
# /var/lib/fileblock/stores/<id>/. The base manifests do not mount any
# host paths beyond /var/lib/kubelet and /dev. This overlay ships a
# minimal patch (host-source-patch.yaml) that adds a hostPath volume
# matching `backingStore.local.path`, mounted at the same path inside
# the controller and node pods. NFS-type SCs do NOT need such a patch —
# they're zero-overlay by design.

namespace: fileblock-system

resources:
  - ../../base
  - storageclass.yaml

patches:
  - path: host-source-patch-controller.yaml
    target:
      kind: Deployment
      name: fileblock-controller
  - path: host-source-patch-node.yaml
    target:
      kind: DaemonSet
      name: fileblock-node
```

Create `deploy/kustomize/overlays/example-localdir/host-source-patch-controller.yaml`:

```yaml
# Surfaces /var/lib/fileblock-localdir from the host into the controller
# pod so the LocalMounter's bind-mount target has a real source.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: fileblock-controller
spec:
  template:
    spec:
      containers:
        - name: fileblock-controller
          volumeMounts:
            - name: host-source
              mountPath: /var/lib/fileblock-localdir
      volumes:
        - name: host-source
          hostPath:
            path: /var/lib/fileblock-localdir
            type: DirectoryOrCreate
```

Create `deploy/kustomize/overlays/example-localdir/host-source-patch-node.yaml`:

```yaml
# Same hostPath surface for the node DaemonSet's container.
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: fileblock-node
spec:
  template:
    spec:
      containers:
        - name: fileblock-node
          volumeMounts:
            - name: host-source
              mountPath: /var/lib/fileblock-localdir
      volumes:
        - name: host-source
          hostPath:
            path: /var/lib/fileblock-localdir
            type: DirectoryOrCreate
```

Delete any existing patches:

```bash
git rm -f deploy/kustomize/overlays/example-localdir/patch-*.yaml 2>/dev/null || true
```

- [ ] **Step 3: Validate**

```bash
kubectl kustomize deploy/kustomize/overlays/example-localdir | grep -A2 'kind: StorageClass'
```
Expected: shows the local-type SC.

- [ ] **Step 4: Commit**

```bash
git add deploy/kustomize/overlays/example-localdir/
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "overlays: example-localdir is now an SC + base; no patches needed"
```

### Task 6.6: Simplify `example-nfs-shared` overlay

**Files:**
- Delete: `deploy/kustomize/overlays/example-nfs-shared/patch-controller.yaml`
- Delete: `deploy/kustomize/overlays/example-nfs-shared/patch-node.yaml`
- Create: `deploy/kustomize/overlays/example-nfs-shared/storageclass.yaml`
- Modify: `deploy/kustomize/overlays/example-nfs-shared/kustomization.yaml`

- [ ] **Step 1: Replace with a minimal overlay**

Create `deploy/kustomize/overlays/example-nfs-shared/storageclass.yaml`:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: fileblock-nfs-shared
provisioner: fileblock.csi
parameters:
  backingStore.type: nfs
  backingStore.nfs.server: nfs.example.internal
  backingStore.nfs.path: /exports/fileblock
  backingStore.nfs.mountOptions: "nfsvers=4.1,hard,timeo=600"
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
```

Replace `deploy/kustomize/overlays/example-nfs-shared/kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

# Shared NFS export as backing store. Every node mounts the same export
# inside its DaemonSet pod on demand; cross-node mutual exclusion is
# CSI's SINGLE_NODE_WRITER serialization, so neither NLM (NFSv3) nor
# server-side locks are on the critical path.
#
# Replace `backingStore.nfs.server` and `backingStore.nfs.path` with
# your NFS export. Set `nfsvers=3` if the server only supports v3.

namespace: fileblock-system

resources:
  - ../../base
  - storageclass.yaml
```

Delete patches:

```bash
git rm -f deploy/kustomize/overlays/example-nfs-shared/patch-*.yaml
```

- [ ] **Step 2: Validate**

```bash
kubectl kustomize deploy/kustomize/overlays/example-nfs-shared | grep 'kind: StorageClass'
```
Expected: SC present.

- [ ] **Step 3: Commit**

```bash
git add deploy/kustomize/overlays/example-nfs-shared/
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "overlays: example-nfs-shared is now an SC + base; no patches needed"
```

### Task 6.7: Update `e2e` overlay

**Files:**
- Modify: `deploy/kustomize/overlays/e2e/kustomization.yaml`
- Modify: `deploy/kustomize/overlays/e2e/patch-controller.yaml`
- Modify: `deploy/kustomize/overlays/e2e/patch-node.yaml`
- Replace: `deploy/kustomize/overlays/e2e/patch-storageclass.yaml` → `deploy/kustomize/overlays/e2e/storageclass.yaml`

- [ ] **Step 1: Inspect current overlay**

```bash
cat deploy/kustomize/overlays/e2e/kustomization.yaml
cat deploy/kustomize/overlays/e2e/patch-storageclass.yaml
cat deploy/kustomize/overlays/e2e/patch-controller.yaml
cat deploy/kustomize/overlays/e2e/patch-node.yaml
```

- [ ] **Step 2: Replace SC patch with a fresh SC yaml**

Create `deploy/kustomize/overlays/e2e/storageclass.yaml`:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: fileblock-e2e
provisioner: fileblock.csi
parameters:
  backingStore.type: local
  backingStore.local.path: /tmp/fileblock-e2e
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
```

Delete the old `patch-storageclass.yaml`:

```bash
git rm deploy/kustomize/overlays/e2e/patch-storageclass.yaml
```

- [ ] **Step 3: Reduce `patch-controller.yaml` and `patch-node.yaml` to image-tag overrides**

Each file should be reduced to a strategic-merge patch that only sets the image tag; remove any `args:` modifications. Concretely, for `patch-controller.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: fileblock-controller
spec:
  template:
    spec:
      containers:
        - name: fileblock-controller
          image: ghcr.io/middlendian/fileblock-csi:e2e
          imagePullPolicy: IfNotPresent
```

Same shape for `patch-node.yaml` (DaemonSet, container `fileblock-node`).

- [ ] **Step 4: Update `kustomization.yaml`**

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

namespace: fileblock-system

resources:
  - ../../base
  - storageclass.yaml

patches:
  - path: patch-controller.yaml
    target:
      kind: Deployment
      name: fileblock-controller
  - path: patch-node.yaml
    target:
      kind: DaemonSet
      name: fileblock-node
```

- [ ] **Step 5: Validate**

```bash
kubectl kustomize deploy/kustomize/overlays/e2e
```
Expected: builds without errors; output includes the e2e SC, the controller Deployment with the e2e image tag, and the node DaemonSet with the e2e image tag.

- [ ] **Step 6: Commit**

```bash
git add deploy/kustomize/overlays/e2e/
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "overlays: e2e ships an SC; controller/node patches now image-tag-only"
```

---

## Chunk 7: Tests, smoke, e2e, docs

This chunk validates the implementation end-to-end and updates user-facing docs.

### Task 7.1: Update `hack/smoke.sh` for `type: local`

**Files:**
- Modify: `hack/smoke.sh`

- [ ] **Step 1: Inspect current smoke**

```bash
cat hack/smoke.sh
```

- [ ] **Step 2: Update the SC and CreateVolume invocations**

Whatever the current smoke produces (raw `csc` calls, embedded YAML, or a temporary kind cluster), the change is:

- The CreateVolume parameters map now contains `backingStore.type=local` and `backingStore.local.path=$BACKING_DIR` instead of `backingStorePath=$BACKING_DIR`.
- The startup invocation of `fileblock-controller` and `fileblock-node` no longer passes `--backing-store=...` or `--topology-*`. They take `--stores-root=$WORK/stores` (a temp dir).
- The script's pre-test setup creates the local-source dir (`mkdir -p $BACKING_DIR`) before invoking CreateVolume.

Concretely, the patches to apply are inline edits to whatever sections of `hack/smoke.sh` currently set up the binary args and the CSC call. After the change, the test still asserts: a PV is created, an `.img` file appears under `<BACKING_DIR>` (the local source — the bind-mount means it shows up there), and `csc ns stage` works.

- [ ] **Step 3: Run smoke**

```bash
sudo hack/smoke.sh
```
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add hack/smoke.sh
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "smoke: drive via backingStore.type=local SC parameters"
```

### Task 7.2: Update `test/e2e/...` SC fixtures

**Files:**
- Modify: any `*.yaml` fixtures under `test/e2e/` that declare a fileblock StorageClass
- Modify: any Go test helpers that build SC params

- [ ] **Step 1: Survey the fixtures**

```bash
grep -rln 'fileblock' test/e2e/
grep -rln 'backingStorePath' test/e2e/
```

- [ ] **Step 2: Migrate every reference**

Each `backingStorePath: /...` parameter becomes:
- `backingStore.type: local` + `backingStore.local.path: /...` for kind-local cases.
- `backingStore.type: nfs` + `backingStore.nfs.server: ...` + `backingStore.nfs.path: ...` for the `make e2e-nfs` variant.

If the e2e harness templates the SC at runtime, update the templating. If it uses `kubectl kustomize deploy/kustomize/overlays/e2e`, the SC change in Task 6.7 already covers it.

- [ ] **Step 3: Run e2e local**

```bash
make e2e
```
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add test/e2e/
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "e2e: migrate SC fixtures to backingStore.type schema"
```

### Task 7.3: Add NFSv3 + NFSv4 e2e variants

**Files:**
- Modify: `hack/e2e.sh` (or whatever harness `make e2e-nfs` runs)

- [ ] **Step 1: Inspect current e2e-nfs harness**

```bash
cat hack/e2e.sh
grep -n 'e2e-nfs' Makefile
```

- [ ] **Step 2: Parameterize the SC `mountOptions` via `envsubst`**

In `hack/e2e.sh`, replace any direct `kubectl apply -f .../storageclass.yaml` against the e2e-nfs overlay's SC with a templated apply:

```sh
NFS_VERSION="${NFS_VERSION:-4.1}"
export NFS_VERSION
envsubst < deploy/kustomize/overlays/e2e/storageclass.yaml.tpl \
    | kubectl apply -f -
```

Rename the e2e overlay's `storageclass.yaml` (NFS variant only) to `storageclass.yaml.tpl` and replace the literal `nfsvers=4.1` with `nfsvers=${NFS_VERSION}`:

```yaml
parameters:
  backingStore.type: nfs
  backingStore.nfs.server: ...
  backingStore.nfs.path: ...
  backingStore.nfs.mountOptions: "nfsvers=${NFS_VERSION},hard,timeo=600"
```

In `Makefile`, add an `e2e-nfs3` target:

```makefile
e2e-nfs3:
	NFS_VERSION=3 $(MAKE) e2e-nfs
```

- [ ] **Step 3: Run both**

```bash
make e2e-nfs           # default 4.1
make e2e-nfs3          # uses NFS_VERSION=3
```
Expected: PASS for both. The exec-bit + chmod + flock assertions in `test/e2e/...` must run against the driver-mounted store in both cases.

- [ ] **Step 4: Commit**

```bash
git add hack/e2e.sh Makefile deploy/kustomize/overlays/e2e/storageclass.yaml.tpl
git rm deploy/kustomize/overlays/e2e/storageclass.yaml || true   # if it was the NFS variant
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "e2e: matrix NFSv3 and NFSv4.1 against driver-mounted store"
```

### Task 7.4: Add the "two stores" e2e

**Files:**
- Create: `test/e2e/two_stores_test.go` (filename to match existing convention)

- [ ] **Step 1: Write the test**

The test creates two SCs pointing at distinct paths on the same NFS server (or two distinct local paths for the local variant), creates one PVC + Pod from each, and asserts:

1. Both Pods reach `Running`.
2. The two PVs report distinct `volumeHandle` prefixes (`fb-<storeIDA>-...` vs `fb-<storeIDB>-...`).
3. Inside the **node DaemonSet pod that staged each PV**, the matching store dir exists and contains exactly one `fb-*.img` file.

Skeleton (helpers `applySC`, `applyPVCAndPod`, `waitRunning`, `scYAML`, `mustClientset` reuse existing harness conventions in `test/e2e/`; `execInDaemonSetOn` is a thin helper to be added):

```go
//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTwoStores(t *testing.T) {
	ctx := context.Background()
	clientset, _ := mustClientset(t)

	applySC(t, ctx, clientset, scYAML("fileblock-store-a", "local", "/tmp/fileblock-e2e/a"))
	applySC(t, ctx, clientset, scYAML("fileblock-store-b", "local", "/tmp/fileblock-e2e/b"))

	applyPVCAndPod(t, ctx, clientset, "pod-a", "pvc-a", "fileblock-store-a")
	applyPVCAndPod(t, ctx, clientset, "pod-b", "pvc-b", "fileblock-store-b")
	waitRunning(t, ctx, clientset, "pod-a")
	waitRunning(t, ctx, clientset, "pod-b")

	// 1. Distinct storeID prefixes on the two volumeHandles.
	pvA := pvForPVC(t, ctx, clientset, "pvc-a")
	pvB := pvForPVC(t, ctx, clientset, "pvc-b")
	idA := mustParseStoreID(t, pvA.Spec.CSI.VolumeHandle)
	idB := mustParseStoreID(t, pvB.Spec.CSI.VolumeHandle)
	if idA == idB {
		t.Fatalf("expected distinct storeIDs, got %q for both", idA)
	}

	// 2. Each pod's node DaemonSet pod has exactly one .img file in
	// the matching store dir, and no .img file in the other store dir.
	for _, c := range []struct {
		pod string
		id  string
		volumeHandle string
	}{
		{"pod-a", idA, pvA.Spec.CSI.VolumeHandle},
		{"pod-b", idB, pvB.Spec.CSI.VolumeHandle},
	} {
		nodeName := nodeOfPod(t, ctx, clientset, c.pod)
		dsPod := dsPodOnNode(t, ctx, clientset, "fileblock-system", "fileblock-node", nodeName)
		out := execInPod(t, ctx, clientset, dsPod, "fileblock-node",
			fmt.Sprintf("ls /var/lib/fileblock/stores/%s/", c.id))
		imgs := filterFiles(strings.Fields(out), ".img")
		if len(imgs) != 1 {
			t.Errorf("[%s] /var/lib/fileblock/stores/%s/ contains %d .img files: %v", c.pod, c.id, len(imgs), imgs)
		}
		expected := c.volumeHandle + ".img"
		if len(imgs) == 1 && imgs[0] != expected {
			t.Errorf("[%s] expected %s, got %s", c.pod, expected, imgs[0])
		}
	}
}

func mustParseStoreID(t *testing.T, volumeHandle string) string {
	// volumeHandle = "fb-<12-hex-storeID>-<name>"; extract the storeID.
	if !strings.HasPrefix(volumeHandle, "fb-") || len(volumeHandle) < 16 || volumeHandle[15] != '-' {
		t.Fatalf("malformed volumeHandle %q", volumeHandle)
	}
	return volumeHandle[3:15]
}
```

Helpers to add to `test/e2e/helpers_test.go` (or wherever existing helpers live):

```go
// pvForPVC returns the bound PV for a PVC.
func pvForPVC(t *testing.T, ctx context.Context, cs *kubernetes.Clientset, pvc string) *corev1.PersistentVolume {
	c, _ := cs.CoreV1().PersistentVolumeClaims("default").Get(ctx, pvc, metav1.GetOptions{})
	if c.Spec.VolumeName == "" {
		t.Fatalf("pvc %s has no bound volume", pvc)
	}
	pv, _ := cs.CoreV1().PersistentVolumes().Get(ctx, c.Spec.VolumeName, metav1.GetOptions{})
	return pv
}

// nodeOfPod returns the node a Pod was scheduled on.
func nodeOfPod(t *testing.T, ctx context.Context, cs *kubernetes.Clientset, pod string) string {
	p, _ := cs.CoreV1().Pods("default").Get(ctx, pod, metav1.GetOptions{})
	if p.Spec.NodeName == "" {
		t.Fatalf("pod %s not yet scheduled", pod)
	}
	return p.Spec.NodeName
}

// dsPodOnNode returns the DaemonSet pod that runs on a given node.
func dsPodOnNode(t *testing.T, ctx context.Context, cs *kubernetes.Clientset, ns, dsName, nodeName string) string {
	pods, _ := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + dsName,
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if len(pods.Items) != 1 {
		t.Fatalf("expected 1 ds pod %s on %s, got %d", dsName, nodeName, len(pods.Items))
	}
	return pods.Items[0].Name
}

// filterFiles returns only entries that end with the given suffix.
func filterFiles(entries []string, suffix string) []string {
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e, suffix) {
			out = append(out, e)
		}
	}
	return out
}
```

`execInPod` (kubelet exec helper) most likely already exists in the harness; if not, port from `client-go`'s `remotecommand.NewSPDYExecutor` — about 25 lines, one-time effort.

Use `local` SCs (not NFS) so the test runs under `make e2e` without an NFS server. The local source paths `/tmp/fileblock-e2e/{a,b}` need to exist on each node — the e2e overlay's hostPath patch can pre-create them, or the test setup `mkdir -p`s them via `kubectl debug node/.../-- mkdir -p ...` (or, easier, the e2e overlay's `host-source-patch.yaml` uses `type: DirectoryOrCreate` and the kubelet creates them when the DaemonSet pod first comes up).

- [ ] **Step 2: Run the test**

```bash
go test -tags=e2e ./test/e2e/... -run TestTwoStores -v
```
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/two_stores_test.go
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "e2e: two SCs / two backing stores in the same install"
```

### Task 7.5: Update `CHANGELOG.md`

**Files:**
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Promote `[Unreleased]` to `[0.3.0] - 2026-05-09`**

Under `## [0.3.0] - 2026-05-09`, add sections:

```markdown
### Breaking

- StorageClass schema changed. The decorative `backingStorePath`
  parameter is removed. New required parameters:
  `backingStore.type` (`nfs` | `local`),
  `backingStore.nfs.server`, `backingStore.nfs.path`,
  `backingStore.nfs.mountOptions` (optional),
  `backingStore.local.path`. See README and
  `deploy/kustomize/overlays/example-{nfs-shared,localdir}` for
  examples.
- Binary flags removed: `--backing-store`, `--topology-key`,
  `--topology-value`. Both binaries now accept `--stores-root`
  (default `/var/lib/fileblock/stores`).
- Controller pod requires `SYS_ADMIN` capability (added by base
  manifests). `privileged: true` is a tested fallback if `SYS_ADMIN`
  alone is rejected by the host's LSM.
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
```

Add the link reference at the bottom of the CHANGELOG (existing convention).

- [ ] **Step 2: Commit**

```bash
git add CHANGELOG.md
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "changelog: 0.3.0 — StorageClass-driven backing-store config"
```

### Task 7.6: Update `README.md`

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Update the installation example**

Find the existing "Quick start" / installation section and replace any patch-based examples with:

```yaml
# Apply the base manifests (driver, RBAC, CSIDriver):
kubectl apply -k 'github.com/middlendian/fileblock-csi/deploy/kustomize/base?ref=v0.3.0'

# Then ship a StorageClass of your own:
cat <<'EOF' | kubectl apply -f -
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: fileblock
provisioner: fileblock.csi
parameters:
  backingStore.type: nfs
  backingStore.nfs.server: nfs.example.internal
  backingStore.nfs.path: /exports/fileblock
  backingStore.nfs.mountOptions: "nfsvers=4.1,hard,timeo=600"
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
EOF
```

Update the **Limitations** section to reflect: still ext4 only, still SINGLE_NODE_WRITER only, OFFLINE expansion only. Remove any text claiming the operator must patch volumes.

Add a brief note: "Both NFSv3 and NFSv4 are supported. Use `nfsvers=3` in `mountOptions` for v3 servers; `nfsvers=4.1` (or omit) for v4."

- [ ] **Step 2: Commit**

```bash
git add README.md
git -c user.email=hello@middlendian.dev -c user.name=middlendian commit -m "readme: SC-driven installation; both NFSv3 and v4 supported"
```

### Task 7.7: Final `make check` gate

- [ ] **Step 1: Run the full CI gate**

```bash
make fmt-check vet lint tidy-check
go test -race ./...
make build
```

If a Linux host with loop devices is available:

```bash
sudo make smoke
sudo make sanity
make e2e
make e2e-nfs
```

All must pass before opening the PR.

- [ ] **Step 2: Open the PR**

```bash
git push origin design/storageclass-driven-config
gh pr create --title "feat: StorageClass-driven backing-store config (v0.3.0 breaking)" \
  --body "$(cat <<'EOF'
## Summary

- Implements the design at `docs/superpowers/specs/2026-05-09-storageclass-driven-config-design.md`.
- Backing-store config moves from binary flags + manifest patches into StorageClass parameters.
- `pkg/store` package owns the SC-config → mounted-directory lookup; controller and node both route through `Registry.Get` before delegating to the existing `pkg/image` / `pkg/loop` machinery.
- Two SCs against distinct backing stores now coexist in a single driver install.
- v0.3.0 hard cutover; CHANGELOG enumerates the breaking changes; migration runbook in the spec.

## Test plan

- [x] `go test -race ./...` (unit, including new `pkg/store/...`)
- [x] `sudo hack/smoke.sh`
- [x] `sudo hack/csi-sanity.sh`
- [x] `make e2e` (local-type SC, exec-bit/chmod/flock survive)
- [x] `make e2e-nfs` (NFSv4.1)
- [x] `NFS_VERSION=3 make e2e-nfs` (NFSv3)
- [x] New `TestTwoStores` e2e
- [x] `kubectl kustomize deploy/kustomize/overlays/{example-localdir,example-nfs-shared,e2e}` validate

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

---

## Done

When PR merges and the release flow cuts `v0.3.0`, the design closes out. Downstream consumer overlays (operator-side) update separately to drop their volume-replacement patches in favor of an SC YAML; that work is not part of this plan.
