# StorageClass-driven backing-store configuration

**Status:** Draft
**Date:** 2026-05-09
**Target version:** v0.3.0 (breaking change)

## Problem

Today, configuring fileblock-csi for a real backing store requires patching
both the controller `Deployment` and the node `DaemonSet`:

- The base `storageclass.yaml` declares `parameters.backingStorePath:
  /var/lib/fileblock`, but the value is **decorative**. The controller
  echoes its own `--backing-store` flag into volume context regardless of
  what the StorageClass said (`pkg/driver/controller.go:109`).
- The actual backing path comes from the binary `--backing-store` flag and
  the `volumes[backing-store]` definition on each pod, which an overlay
  must patch (e.g. `deploy/kustomize/overlays/example-nfs-shared/`).
- Topology selection (`--topology-key`, `--topology-value`) is also
  flag-driven and lives in overlay patches.
- Two StorageClasses backed by different paths (different NFS exports, or
  one NFS + one local) require two complete copies of the controller +
  node manifests with distinct flag sets — not feasible in practice.

The result: every operator copies `example-nfs-shared/`, edits the IP and
path, and ships custom kustomize patches for what should be SC-level
config. The operator who consumed this driver in
`greghaskins/homelab/apps/csi-driver-fileblock/` ended up wiring a
`replacements:` block from a `nas-config` ConfigMap into the volume
definitions on both pods to keep IPs/paths in one place — a workaround for
a missing first-class interface.

## Goals

1. **Zero kustomize patches for the common case.** A user installs the
   base manifests once and ships StorageClasses to configure backing
   stores.
2. **Multiple StorageClasses, multiple backing stores, single driver
   install.** Two SCs pointing at different NFS exports (or one NFS + one
   local hostPath) work simultaneously.
3. **The StorageClass is the single source of truth** for everything
   per-pool: backing-store type, location, mount options. `reclaimPolicy`
   and `allowVolumeExpansion` are already SC-native and unchanged.
4. **No new CRDs** in v1. SC parameters are sufficient.

## Non-goals

- SMB / CIFS backing-store types (deferred).
- FUSE-mount backing types (deferred).
- Per-SC default capacity, fsType, or access mode. fileblock remains
  ext4-only, `SINGLE_NODE_WRITER`-only, with the existing 1 GiB default.
- Backward compatibility with v0.2.0 SC schema. v0.3.0 is a hard cutover;
  see Migration.
- Online (live) volume expansion. Still OFFLINE.
- Reference-counted unmounting of backing stores. Mount lifecycle is
  sticky-per-process — see Mount lifecycle below.

## Approach

The driver pods mount the backing source themselves, on demand, driven by
SC parameters. This mirrors `csi-driver-nfs` in shape: the operator
installs a single Deployment + DaemonSet, and StorageClasses declare what
to mount.

Approaches considered and rejected:

- **`BackingStore` CRD with reconciler-managed mounts.** Adds a CRD,
  reconciler, and RBAC for what is currently a small set of static config.
  Defer to a future version if SC parameter sprawl warrants it.
- **Hybrid (driver inline + CRD escape hatch).** Most flexible, largest
  surface area. Premature; ship the simpler shape first.

## SC parameter schema

Discriminated form, with a `backingStore.type` discriminator and
type-prefixed sub-keys. Validation happens in `CreateVolume`; missing or
malformed required keys → `InvalidArgument`.

### NFS

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: fileblock-shared
provisioner: fileblock.csi
parameters:
  backingStore.type: nfs
  backingStore.nfs.server: 10.0.0.235
  backingStore.nfs.path: /var/nfs/shared/fileblock
  backingStore.nfs.mountOptions: "nfsvers=4.1,hard,timeo=600"   # optional
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
```

`backingStore.nfs.mountOptions` is a comma-separated string passed to
`mount.nfs4` as `-o`. If omitted, the kernel default applies.

### Local hostPath

```yaml
parameters:
  backingStore.type: local
  backingStore.local.path: /var/lib/fileblock-store
```

The path must already exist on every node where the DaemonSet runs (and
on the controller host if the controller pod is scheduled there).
Intended for kind-based e2e and trivial single-node test setups.

### Two SCs sharing a physical pool

```yaml
# SC #1 — same server, same path
parameters:
  backingStore.type: nfs
  backingStore.nfs.server: 10.0.0.235
  backingStore.nfs.path: /var/nfs/shared/fileblock
---
# SC #2 — same server, same path: shares the on-disk pool with SC #1
parameters:
  backingStore.type: nfs
  backingStore.nfs.server: 10.0.0.235
  backingStore.nfs.path: /var/nfs/shared/fileblock
reclaimPolicy: Retain   # different reclaim from SC #1
```

The driver derives `storeID = sha256(canonicalize(Config))[:12]`. Two SCs
with identical `Config` → same `storeID` → same on-disk pool of `.img`
files, mounted once per process. The `reclaimPolicy` lives on the SC
itself (Kubernetes-native), so the same pool can serve PVs with different
delete/retain semantics.

`canonicalize(Config)` normalizes mount options before hashing so that
two SCs that differ only in cosmetic option-string formatting still
share a `storeID`. Specifically: split `mountOptions` on commas, drop
empties, sort lexicographically, join. So
`"nfsvers=4.1,hard,timeo=600"` and `"hard,nfsvers=4.1,timeo=600"`
canonicalize to the same byte string. Type and other primitive fields
serialize as-is.

## Architecture

### Per-store mount cache

Inside both controller and node containers:

```
/var/lib/fileblock/stores/<storeID>/                ← mount target
/var/lib/fileblock/stores/<storeID>/fb-<uuid>.img   ← .img files in that pool
```

`<storeID>` is the deterministic hash of the canonical Config. Multiple
PVs from the same SC (or from a peer SC with identical Config) share one
mount.

### Mount lifecycle: sticky-per-process

First time the driver sees `storeID=X` (in `CreateVolume` on the
controller, or `NodeStageVolume` on the node), it mounts the source at
`/var/lib/fileblock/stores/X/`. The mount stays until the pod exits.
Pod restarts (image bumps, manifest changes) clean up. We do not
reference-count or unmount on PV deletion.

Rationale: pod restarts happen often enough that an idle mount has no
real cost; the per-PV/per-Stage code path stays simple; reconcile-on-
restart logic stays simple.

### Concurrency

A per-store `sync.Mutex` guards "is it mounted yet?" + the `mount(2)`
call. Subsequent calls fast-path on a `mounted bool` cached in the
Registry.

### Privilege

- **Node container** already runs `privileged: true` for losetup. No
  change.
- **Controller container** today is unprivileged. To allow it to mount
  NFS itself, it gains `securityContext.capabilities.add: [SYS_ADMIN]`
  and an `emptyDir` writable at `/var/lib/fileblock/stores/`. It does not
  need full `privileged: true`.

### Image

Today's `Dockerfile` is `debian:trixie-slim` + `e2fsprogs util-linux
ca-certificates`. Add `nfs-common` to the same `apt-get install
--no-install-recommends` line. This brings `mount.nfs` and `mount.nfs4`
plus minimal RPC machinery (~5 MB compressed). Base remains
`debian:trixie-slim`. No multi-stage runtime change.

We do not pull in a Go-level NFS client. POSIX semantics on the in-pod
ext4 mount come from the loop device + ext4, not from the NFS client; so
NFS only needs to ship bytes faithfully, which the kernel client does.

### Volume context shape

What `CreateVolume` returns and `NodeStageVolume` reads:

```json
{
  "backingStore.type": "nfs",
  "backingStore.nfs.server": "10.0.0.235",
  "backingStore.nfs.path": "/var/nfs/shared/fileblock",
  "backingStore.nfs.mountOptions": "nfsvers=4.1,hard,timeo=600",
  "storeID": "a3f1c290bb04"
}
```

The node parses these keys back into a `store.Config` and asks the
Registry to ensure it's mounted, then proceeds with the existing
losetup + ext4 path. The old `backingStorePath` key is removed from the
volume context entirely.

### Topology

Implicit, derived from backing-store type. The `--topology-key` and
`--topology-value` flags are removed from both binaries.

| Backing type | Controller's `AccessibleTopology` | Effect |
|---|---|---|
| `nfs` | empty (`[]`) | any node may stage; provisioner schedules freely |
| `local` | `[{fileblock.csi/node: <preferredNode>}]` | per-node pin (existing default behavior) |

Node always reports a single segment `{fileblock.csi/node: <nodeID>}`
at registration time. For `nfs`-backed PVs, the controller leaves
`AccessibleTopology` empty so the provisioner treats the volume as
universally schedulable. For `local`-backed PVs, the controller echoes
the provisioner's preferred segment exactly as today.

**`--strict-topology` removed from external-provisioner.** The
controller sidecar today sets `--strict-topology`
(`controller-deployment.yaml:38`). Combined with empty
`AccessibleTopology` for NFS, this would cause the provisioner to
reject the volume — the strict mode requires the volume's topology to
match the selected node's segments exactly, and "no segments" doesn't
match. Drop `--strict-topology` from the provisioner args. Local-pin
behavior still works: the provisioner falls back to its default
non-strict topology mode, which honors the preferred segment we echo
without requiring an exact match. This matches the configuration
shipped by `csi-driver-nfs`.

## Components and package layout

A new package owns the "given an SC config, produce a mounted directory"
boundary, independent of `pkg/image`, `pkg/loop`, `pkg/mount`.

### New: `pkg/store/`

```
pkg/store/
  store.go         types: Config, Type, ID
  parse.go         StorageClass params <-> Config; volume context <-> Config;
                   Config.ID() = sha256(canonical(Config))[:12]
  registry.go      per-process Registry: Get(ctx, Config) (mountedPath, error)
                   serializes per-storeID; caches mounted state in memory;
                   hosts mounts under /var/lib/fileblock/stores/<storeID>/
  mounter_nfs.go   exec("mount.nfs4", server:path, target, "-o", opts)
                   via pkg/exec.Runner
  mounter_local.go validates Config.LocalPath exists; bind-mounts it into
                   /var/lib/fileblock/stores/<storeID>/
  store_test.go    unit tests with FakeRunner — no real mounts
```

Public surface:

- `Config` — parsed SC config; comparable; `Canonical() []byte` for hashing.
- `ConfigFromParams(map[string]string) (Config, error)` — for the
  controller's `CreateVolume`.
- `ConfigFromVolumeContext(map[string]string) (Config, error)` — for the
  node's `NodeStageVolume`.
- `Config.ToVolumeContext() map[string]string` — for `CreateVolume` to
  echo back.
- `Config.ID() string` — deterministic 12-char hex.
- `Registry.Get(ctx, Config) (path string, err error)` — idempotent;
  ensures mounted; returns absolute path.

### Existing: `pkg/driver/`

- **`controller.go`**: `ControllerServer` gains `*store.Registry`;
  `CreateVolume` parses SC params → `store.Config`, calls `registry.Get`,
  passes the resulting path to `image.New(...)`, and echoes
  `cfg.ToVolumeContext()` (plus `storeID`) into the response. The
  `backingStorePath string` field is removed. `--backing-store` flag is
  removed. `ParamBackingStorePath` constant relocates to `pkg/store` and
  splits into a set of volume-context-key constants.
  - **Bundled cleanup:** `CreateVolume` is currently 50+ lines and grows
    further with config parsing — extract a `parseAndPrepareStore(req)`
    helper alongside this work. (Targeted; serves the change.)
- **`node.go`**: `NodeServer` gains `*store.Registry`; `NodeStageVolume`
  swaps the `backing := req.GetVolumeContext()[ParamBackingStorePath]`
  read for `cfg, _ := store.ConfigFromVolumeContext(req.GetVolumeContext())
  + path, _ := registry.Get(ctx, cfg)`. `--backing-store`,
  `--topology-key`, `--topology-value` flags removed. The
  `topologyKey/topologyValue` fields collapse to a single computed segment
  `{fileblock.csi/node: <nodeID>}`.
- **`server.go`** and `cmd/{controller,node}/`: drop the removed flags,
  construct the `*store.Registry` at startup.

### Existing: `pkg/image`, `pkg/loop`, `pkg/mount`

`pkg/image` and `pkg/mount` are unchanged.

- `image.New(path, exec)` is called per-store with the per-store mounted
  path. Same code, different argument value per call.
- `pkg/mount` is unchanged.

`pkg/loop` requires one constructor-argument change:

- `Reconciler.backingStorePath` (today a single per-binary store path,
  used by `Reconcile` to decide which orphan loops are fileblock's to
  detach via a `strings.HasPrefix(back, cleanRoot+"/")` check) is now
  set to the **parent stores directory**
  `/var/lib/fileblock/stores`. Every store mounts at
  `/var/lib/fileblock/stores/<storeID>/`, so any loop whose back-file
  is under that tree is ours regardless of which `storeID` it belongs
  to. The reconcile pass then DTRT across all live stores: drop stale
  state entries, detach orphan loops anywhere under
  `/var/lib/fileblock/stores/`. No code change inside `pkg/loop` —
  only the value the node binary passes at startup.
- `loop`'s state file (`Mapping{VolumeID, LoopDev, ImagePath,
  StagePath}`) carries the absolute `.img` path, which already spans
  stores correctly. v0.2.0 entries deserialize into v0.3.0 `Mapping`s
  unchanged; the reconciler will simply find them stale (no live loop
  matches) on first start under v0.3.0 and drop them, which is the
  desired migration behavior.

### Reconcile on plugin restart

`pkg/loop`'s reconciler (drop entries whose `back-file` doesn't match)
stays as-is. New addition: on `Registry` init, scan
`/var/lib/fileblock/stores/*/` for any pre-existing mounts and adopt
them as already-mounted. With sticky-per-process this is mostly belt-
and-braces — `emptyDir` is wiped on restart in normal cases — but
correct under unexpected restarts.

## Data flow

```
1. User applies PVC referencing SC "fileblock-shared".
2. external-provisioner sees PVC, calls
   CreateVolume(name, capabilities, parameters)
   where parameters = SC.parameters verbatim.
3. Controller:
   a. cfg, err := store.ConfigFromParams(req.parameters)
        -> InvalidArgument if missing/malformed
   b. mountedPath, err := registry.Get(ctx, cfg)
        - already mounted? return /var/lib/fileblock/stores/<id>/
        - else: mkdir target, exec mount.nfs4 (or bind-mount for local),
          mark mounted in cache; surface stderr in error
   c. images := image.New(mountedPath, exec)
   d. images.Create(ctx, volumeID, capacity)
        - idempotent; existing fb-<uuid>.img is adopted at same size
   e. return Volume{
        VolumeID,
        VolumeContext: cfg.ToVolumeContext() ∪ {storeID: cfg.ID()},
        AccessibleTopology: typeBasedTopology(cfg, req)
      }
4. external-provisioner creates PV with that volumeContext.
5. Pod schedules; kubelet calls
   NodeStageVolume(volumeID, stagingTargetPath, volumeContext).
6. Node:
   a. cfg, err := store.ConfigFromVolumeContext(req.volumeContext)
   b. mountedPath, err := registry.Get(ctx, cfg)   // mounts in node pod if new
   c. images := image.New(mountedPath, exec)
   d. losetup attach + e2fsck + resize2fs + mount  // existing logic, unchanged
```

## Error handling

| Scenario | gRPC code |
|---|---|
| SC params missing required key (`backingStore.type`) | `InvalidArgument` (controller) |
| SC params unknown type (`type: smb`) | `InvalidArgument` (controller) |
| Volume context missing on Stage (e.g. PV from older release) | `InvalidArgument` (node) |
| `mount.nfs4` exit non-zero | `Internal`; stderr surfaced in message |
| `local`-type, `path` doesn't exist | `Internal` |
| Mount target already exists with wrong source | `Internal`; logged; manual recovery |
| Two simultaneous CreateVolumes for same `storeID` | serialized by per-store mutex; second sees `mounted=true` |

### Operational concerns

**Asymmetric mount failure (controller succeeds, node fails).** The
controller and node mount the same `server:path` independently inside
their own pods. There is no shared "is this store reachable
cluster-wide?" probe. If `CreateVolume` succeeds (controller mounted)
but `NodeStageVolume` fails (node can't reach the server, e.g.
NetworkPolicy blocks the path or DNS resolves differently in the
DaemonSet pod's network namespace), the PV exists with an `.img` but
the Pod stays in `ContainerCreating` indefinitely while the kubelet
retries Stage with exponential backoff. This failure mode is new in
v0.3.0 — today the kubelet pre-mounts the hostPath, so connectivity
problems surface at pod-scheduling time. Operator runbook:

```
$ kubectl describe pod <stuck-pod>           # see Stage error
$ kubectl logs -n fileblock-system fileblock-node-<x>
                                             # find the mount.nfs4 stderr
$ # fix DNS / firewall / NFS export ACL on the failing node
```

No driver-side back-pressure on `CreateVolume` is added. PV cleanup
on permanent failure is the operator's call: `kubectl delete pvc`
honors the SC's `reclaimPolicy`.

**NFS server flap.** Sticky-per-process mounts mean that if the NFS
server goes away while the driver pod is up, the existing mount may
hang on I/O depending on `hard` vs `soft` mount options. Recommend
operators set `nfs.mountOptions: "hard,timeo=600"` (or similar) so
recovery on server-return is automatic; the spec's example SC reflects
this. A truly dead server requires a driver-pod restart to drop the
hung mount; this is the same trade-off `csi-driver-nfs` accepts.

**Privilege verification.** The spec advertises `SYS_ADMIN`-only for
the controller. Verify on a kind cluster *and* on the production k0s
cluster during step 6 (deploy/kustomize/base) of the implementation
order that `SYS_ADMIN` without `privileged: true` is sufficient for
`mount.nfs4` and the bind-mount used by `type: local`. Some kernels,
AppArmor profiles, or SELinux contexts deny mount syscalls from a
non-`privileged` container regardless of caps. If verification fails,
escalate the controller to `privileged: true` — same blast radius as
the node container today, and the spec accepts this fallback. This
verification is a required implementation step, not an open question.

## Deploy / kustomize impact

### `deploy/kustomize/base/`

- **`storageclass.yaml`**: deleted. The base ships the driver; operators
  ship StorageClasses. (An example SC is included in each overlay.)
- **`controller-deployment.yaml`**:
  - Drop `volumes[backing-store]` and the corresponding `volumeMounts`
    entry.
  - Drop `--backing-store=...` arg.
  - Drop `--topology-key=...` arg if present in any inherited overlay
    (none in base today, but flagged for overlays).
  - Add `securityContext.capabilities.add: [SYS_ADMIN]` to the
    `fileblock-controller` container.
  - Add an `emptyDir` volume mounted at `/var/lib/fileblock/stores/`.
- **`node-daemonset.yaml`**:
  - Drop `volumes[backing-store]` and the corresponding `volumeMounts`
    entry.
  - Drop `--backing-store=...`, `--topology-key=...`, `--topology-value=...`
    args.
  - Add an `emptyDir` (or `hostPath: type: DirectoryOrCreate`) volume
    mounted at `/var/lib/fileblock/stores/`. `emptyDir` is fine because
    the cache is rebuilt on pod restart by sticky-per-process design.

### `deploy/kustomize/overlays/`

- **`example-localdir/`**: collapses to a single `storageclass.yaml`
  with `backingStore.type: local`. No patches. Optionally a
  `kustomization.yaml` that includes the base and the SC.
- **`example-nfs-shared/`**: collapses to a single `storageclass.yaml`
  with `backingStore.type: nfs`. No patches. README updated to point
  here as the canonical "how to deploy".
- **`e2e/`**: keeps `patch-controller.yaml` / `patch-node.yaml` only for
  image-tag override (the `:e2e` test image). The
  `patch-storageclass.yaml` is replaced with an SC yaml that points at
  the kind-shared dir as `type: local` (or NFS for `make e2e-nfs`).

### `Dockerfile`

Add `nfs-common` to the existing apt install line:

```dockerfile
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        e2fsprogs util-linux ca-certificates nfs-common \
 && rm -rf /var/lib/apt/lists/*
```

### Downstream consumer (`greghaskins/homelab/apps/csi-driver-fileblock/`)

The current overlay's `replacements:` block (substituting `nas-ip` and
`nas-fileblock-path` from `nas-config` into the controller and node pod
volumes) is removed. The same replacement block now lives on a
`storageclass.yaml` shipped from this overlay, substituting
`nas-config.data.nas-ip` → `parameters.backingStore.nfs.server` and
`nas-config.data.nas-fileblock-path` → `parameters.backingStore.nfs.path`.
The `patches/controller.yaml` and `patches/node.yaml` lose their volume
overrides; what remains is k0s kubelet-root and StorageClass
`reclaimPolicy: Retain`, both still relevant.

## Testing strategy

- **Unit, `pkg/store/`**: `parse_test.go` covers SC param round-trip,
  volume context round-trip, hash determinism, and every error case.
  `registry_test.go` uses `pkg/exec/exectest.FakeRunner` to assert
  `mount.nfs4` invocations and idempotent re-`Get`. No real mounts.
- **Smoke, `hack/smoke.sh`**: extend to include a `local`-type SC and
  assert end-to-end PVC → PV → loop attach → mount on a developer host
  (no NFS server required).
- **csi-sanity, `hack/csi-sanity.sh`**: unchanged at the CSI layer; SC
  params and volume context flow through the same way csi-sanity
  already exercises them.
- **e2e local, `make e2e`**: SC switches to `type: local`. Verifies
  the per-node topology pin still works.
- **e2e nfs, `make e2e-nfs`**: SC switches to `type: nfs`. Verifies
  driver-mounted NFS, exec-bit, chmod, and flock through the loop +
  ext4 path. This is the headline test for zero-patch operation. The
  three load-bearing assertions — exec bit survives, chmod is honored,
  flock works — must run against the **driver-mounted** store
  specifically, not against any NFS-backed PV. Easy to regress if
  `mount.nfs4 -o ...` ends up with different default options than the
  kubelet's in-tree NFS mount; the test should fail loudly if so.
- **New e2e: "two stores"**: same NFS server, two paths, two SCs. PVC
  from each. Verify both Pods come up and `.img` files land in the
  correct pools (`/var/lib/fileblock/stores/<idA>/` vs `<idB>/`).

## Migration

v0.3.0 is a hard cutover. v0.2.0 PVs cannot be staged by v0.3.0 (volume
context shape differs) and v0.3.0 PVs cannot be staged by v0.2.0 (SC
schema differs).

Runbook:

1. Stop and drain workloads using fileblock PVCs. (They'd fail on next
   stage anyway.)
2. Delete PVCs. PVs with `reclaimPolicy: Retain` keep their `.img` on
   disk; with `Delete` they are removed.
3. Apply v0.3.0 manifests.
4. Apply new StorageClasses with `backingStore.type` etc.
   **`backingStore.nfs.path` is the NFS server's export path**, not
   the in-pod mount path that v0.2.0 used (`--backing-store`,
   `volumes[backing-store].mountPath`). Read it off your old
   `volumes[backing-store].nfs.path` (the source side of the volume),
   not off the `volumeMounts` entry. Easy to get wrong on first
   migration; concretely, the homelab consumer's value is
   `/var/nfs/shared/fileblock`, not `/var/lib/fileblock`.
5. Re-create PVCs. New PVs adopt existing `.img` files at the same
   backing path (`image.Create` is idempotent: same `volumeID` + same
   on-disk size = adopt).

Rollback: redeploy v0.2.0 manifests + old SC. Existing PVs created
under v0.3.0 cannot be staged by v0.2.0; same drain + recreate.
Documented honestly in CHANGELOG.

The CHANGELOG entry under "Breaking" calls out:

- `backingStorePath` parameter removed; `backingStore.type` etc. now
  required.
- Binaries no longer accept `--backing-store`, `--topology-key`,
  `--topology-value`.
- Controller container now requires `SYS_ADMIN` capability.
- Image now includes `nfs-common`.

## Implementation order (sketch — writing-plans turns this into the plan)

1. `pkg/store` — `Config`, parser, ID, `Registry` skeleton with no real
   mounting yet, full unit tests on the parse and serialize paths.
2. `pkg/store/mounter_local.go` + `mounter_nfs.go` — real mounts behind
   `pkg/exec.Runner`. Smoke covers local; manual NFS validation.
3. `pkg/driver/controller.go` — wire `Registry`, drop
   `backingStorePath` field, switch volume context shape, extract
   `parseAndPrepareStore` helper.
4. `pkg/driver/node.go` — wire `Registry`, switch volume context
   parsing, collapse topology fields.
5. `cmd/{controller,node}/` and `pkg/driver/server.go` — drop removed
   flags; construct `Registry` at startup.
6. `deploy/kustomize/base/` — drop backing-store volume and SC,
   `SYS_ADMIN` cap, drop topology flags.
7. `Dockerfile` — add `nfs-common`.
8. Overlays: simplify `example-localdir`, `example-nfs-shared`, `e2e`.
9. Tests: smoke updates, e2e updates, new "two stores" e2e.
10. `greghaskins/homelab/apps/csi-driver-fileblock/` — drop the volume
    `replacements:`, add a `storageclass.yaml`, simplify the overlay.

## Open questions for implementation

None blocking. One item the implementer should verify in code:

- Confirm `mount.nfs4 -o ...` is the right invocation under
  `nfs-common` on `debian:trixie-slim`. Alternative: call `mount(2)`
  directly with `type="nfs4"` for v4-only (no userland helper needed),
  fall back to `mount.nfs` for v3. Stay with `mount.nfs4` as the
  baseline; revisit if it pulls in unexpected RPC daemons.

(The controller-`SYS_ADMIN`-sufficiency question that was here
previously has been promoted to a required verification step under
"Operational concerns".)
