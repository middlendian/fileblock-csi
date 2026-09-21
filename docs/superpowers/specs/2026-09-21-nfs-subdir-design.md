# backingStore.nfs.subDir: namespace-scoped backing directories

**Status:** Draft
**Date:** 2026-09-21
**Target version:** v0.4.0
**Issue:** #39

## Problem

Every volume provisioned from a StorageClass puts its `.img` at the root
of that store's backing directory. An operator who wants the backing
files of each namespace kept apart — so that an external admission
policy can bind a path prefix to a namespace — has no way to express
that. The only workaround is one StorageClass per namespace, which
repeats the server/path pair in every one of them and does not scale.

## Solution

A new optional StorageClass parameter, `backingStore.nfs.subDir`,
places a store's `.img` files in a subdirectory of the export rather
than at its root. With the `${pvc.metadata.namespace}` token, one
StorageClass gives every namespace its own directory:

```yaml
parameters:
  backingStore.type: nfs
  backingStore.nfs.server: nfs.example.internal
  backingStore.nfs.path: /exports/k8s_ns
  backingStore.nfs.subDir: ${pvc.metadata.namespace}/fileblock
```

The parameter is NFS-only and optional. Absent, behaviour is exactly as
today: the export root. Existing volumes keep working untouched.

## Two identities, deliberately split

`subDir` must be recoverable from the volumeID, or `DeleteVolume`
cannot find the `.img` — so it has to participate in the store
identity. But every `subDir` under one export shares a single NFS mount:
mounting the same export N times for N namespaces would be wasteful and
would multiply the blast radius of a hung mount.

`Config` therefore grows two keys rather than one.

| method | value | identifies |
|---|---|---|
| `MountKey() string` | the 0.3.x canonical form, verbatim | the **mount source**: type, server, path, mount options |
| `MountID() string` | `sha256(MountKey())[:12]` | the mount directory under `--stores-root` |
| `Canonical() []byte` | `MountKey()`, plus `"\|" + NFSSubDir` when non-empty | the **store** |
| `ID() string` | `sha256(Canonical())[:12]` | the storeID in the volumeID prefix and volume context |

Two configs differing only in `subDir` share one `MountID` and one
mount, and have distinct `ID`s.

### The backwards-compatibility hazard

Appending `"|" + NFSSubDir` unconditionally would change the hash of
**every existing config**, including those with no `subDir`. The failure
is quiet and bad: `DeleteVolume` computes a storeID that resolves to
nothing, returns OK per the idempotency rule, and the `.img` is orphaned
on the export forever.

The empty case must stay byte-identical to 0.3.x:

```go
func (c Config) Canonical() []byte {
    if c.NFSSubDir == "" {
        return []byte(c.MountKey())
    }
    return []byte(c.MountKey() + "|" + c.NFSSubDir)
}
```

So for any 0.3.x config, `ID() == MountID() ==` the value 0.3.x
produced. Existing mount directories, volumeIDs and volume contexts are
untouched.

This needs a regression test pinning a literal storeID, and **the pinned
value must be computed from the v0.3.8 tag, not from the working tree** —
otherwise it pins the new behaviour and proves nothing.
`pkg/store/store.go` and `pkg/store/parse.go` are unchanged between
v0.3.8 and v0.3.9, and the canonical form is plain text, so the value is
independently verifiable:

```
$ printf 'nfs|nfs.example.internal|/exports/k8s_ns|' | sha256sum | cut -c1-12
2a355b61d5f7
```

The test pins that literal for a `Config{Type: TypeNFS, NFSServer:
"nfs.example.internal", NFSPath: "/exports/k8s_ns"}` with `NFSSubDir`
empty, and asserts the same config with a `NFSSubDir` set produces a
different `ID()` but the same `MountID()`.

## Template substitution is ours, not the sidecar's

Issue #39 states that `${pvc.namespace}` substitution "is done by
external-provisioner". It is not. external-provisioner performs no
generic templating of StorageClass parameter values. With
`--extra-create-metadata=true` it injects three extra keys into
`CreateVolumeRequest.parameters`:

```
csi.storage.k8s.io/pvc/name
csi.storage.k8s.io/pvc/namespace
csi.storage.k8s.io/pv/name
```

and the driver does its own replacement against them. csi-driver-nfs
works exactly this way, and its token spelling carries `.metadata.`:
`${pvc.metadata.name}`, `${pvc.metadata.namespace}`,
`${pv.metadata.name}`. We match that spelling so an operator can move a
working csi-driver-nfs `subDir` value across unchanged, and so there is
one spelling in the ecosystem rather than two.

The issue's `${pvc.namespace}` example would not be substituted by
anything, and would land every namespace in a shared directory named
literally `${pvc.namespace}` — precisely the failure the issue warns
about. The README and the example StorageClass use the corrected
spelling.

### Unresolved tokens are fatal

If `subDir` still contains `${` after substitution — because
`--extra-create-metadata` is off, or because the token was mistyped —
`CreateVolume` fails with `InvalidArgument`, naming the unresolved token
and the `--extra-create-metadata=true` requirement.

This diverges from csi-driver-nfs, which passes the literal through. The
PVC staying `Pending` with a clear event is much better than every
namespace quietly sharing one directory whose name looks like a
template: the isolation the feature exists to provide would be silently
absent, and nothing would reveal it short of listing the export by hand.

## Design

### 1. `pkg/store` — config

`Config` gains `NFSSubDir string`, documented NFS-only alongside the
other `NFS*` fields. `MountKey()`, `MountID()`, `Canonical()` and `ID()`
as tabulated above.

### 2. `pkg/store/parse.go` — parsing, substitution, validation

New parameter constant `ParamNFSSubDir = "backingStore.nfs.subDir"` and
constants for the three injected metadata keys and the three tokens.

`ConfigFromParams` gains, on the `TypeNFS` branch:

1. Read the raw `subDir`. Empty means the export root; skip the rest.
2. Substitute each supported token from the corresponding injected key,
   where that key is present.
3. If `${` remains, return an error naming the unresolved token and the
   sidecar flag.
4. Validate: reject an absolute path, any `..` path element, and NUL.
5. Normalize with `path.Clean`. Reject the result if it is `"."` —
   `subDir: ./` would otherwise name the mount root while producing a
   storeID distinct from the no-subDir config for that same directory.
   Store the cleaned value.

Normalization happens **before** the value reaches `Canonical()`, so
`a/b`, `a/b/` and `./a/b` do not mint three storeIDs for one directory.
The `..` rejection precedes `Clean` deliberately: `Clean` would collapse
`a/../../b` to `../b`, which the check still catches, but rejecting on
the operator's literal input produces the clearer error message.

On the `TypeLocal` branch, a present `backingStore.nfs.subDir` is an
error — it would otherwise be accepted and silently ignored.

`ToVolumeContext` emits `backingStore.nfs.subDir` with the already
substituted, already cleaned value when non-empty.
`ConfigFromVolumeContext` needs no special case: the context carries no
metadata keys and no surviving tokens, so substitution is a no-op and
validation passes on a value that already satisfies it.

### 3. `pkg/store/registry.go` — one mount, many stores

`mounted`, `storeMu` and `checking` re-key from storeID to **MountID**.
`configs` stays keyed by storeID, because that is what the controller
holds when resolving `DeleteVolume` and `ControllerExpandVolume` from a
volumeID prefix.

A new `stores map[string]string` maps storeID to the resolved store path.

`Get(ctx, cfg)`:

1. Lock on `cfg.MountID()`.
2. Establish or re-verify the mount at `<root>/<mountID>` — unchanged
   logic, including the staleness check and its timeout semantics.
3. `sub := filepath.Join(mountPath, cfg.NFSSubDir)`; `os.MkdirAll(sub,
   0o755)`.
4. Record `stores[cfg.ID()] = sub` and `configs[cfg.ID()] = cfg`.
5. Return `sub`.

Step 3 must run **after** the mount is confirmed live, never before:
creating the directory first would populate the underlying directory
that the mount then hides. Running it on every `Get` rather than only on
first mount is deliberate — it costs one `mkdir` syscall against an
existing directory and it repairs a namespace directory removed
out-of-band. For `NFSSubDir == ""` it is a no-op on the mount root.

Two namespaces on one export therefore share one mount, one per-store
lock and one staleness check, while holding distinct storeIDs.

### 4. `Registry.MountedPaths` — the gap issue #39 misses

`ListVolumes` iterates `MountedPaths()` and lists `.img` files at each
path. With `subDir`, images live one level below the mount root, so
every subDir volume would silently disappear from `ListVolumes`.

`MountedPaths()` returns the deduplicated union of the values of
`stores` and of `mounted`. Deduplication is required because for
`NFSSubDir == ""` the store path and the mount root are the same string,
and a duplicate would report every such volume twice. Listing a mount
root that only has subDir stores under it is harmless: it finds no
`.img` files there, which is correct.

Mount roots are kept in the union so that stores adopted by
`AdoptExisting` — which recovers a directory name but no `Config` — stay
visible to `ListVolumes`, exactly as today.

### 5. `AdoptExisting`

Unchanged. `MountID` has the same 12-hex shape as the storeID the
pattern already matches, and adopted entries populate `mounted`, which
is now the MountID-keyed map — the correct one. The documented caveat
(an adopted store has no recoverable `Config`, so `DeleteVolume` against
it returns `NotFound` until the next `CreateVolume` re-registers it)
applies to subDir stores identically, and no more severely.

### 6. `pkg/image`

No code change. `image.New` keeps its must-exist precondition, which is
load-bearing: `ListVolumes` relies on `New` failing to skip a store
whose mount has dropped out, and `NodeStageVolume` relies on it to avoid
treating a vanished mount as an empty store.

Issue #39 proposed that `image.New` create its root. It does not, because
that would remove exactly that precondition everywhere it is relied on.
The directory is created by `Registry.Get` instead, which is the only
code positioned to do it at the one moment it is safe.

### 7. `pkg/driver`

No change. `CreateVolume` already passes `Registry.Get`'s return value
straight to `image.New`, and `volumeIDFromName` already uses `cfg.ID()`,
which now varies per `subDir`.

### 8. Deployment

`--extra-create-metadata=true` is added to the `csi-provisioner`
container args in
`deploy/kustomize/base/controller-deployment.yaml`. Without it the
metadata keys are never injected and every templated `subDir` fails
validation, so it ships as part of this change rather than being left
for operators to discover.

## Convention amendment

`CLAUDE.md` states that `pkg/image` is "the **only** package that writes
to the backing store". `Registry.Get` creating a store root makes that
false as written. The line narrows to: `pkg/image` is the only package
that writes `.img` files; `pkg/store` creates store root directories.
The on-disk contract section gains the subDir layout:

```
<export>/<subDir>/fb-<uuid>.img
```

## Testing

### Unit

`pkg/store`:

- The pinned legacy storeID, `2a355b61d5f7`, computed from v0.3.8 and
  asserted for an empty `NFSSubDir`.
- `ID()` differs and `MountID()` agrees between two configs differing
  only in `subDir`.
- Substitution of each of the three tokens from its injected key.
- An unresolved token is rejected, with the sidecar flag named in the
  message.
- Absolute path, `..` element, NUL and `.` are rejected.
- `a/b`, `a/b/` and `./a/b` normalize to one `ID()`.
- `backingStore.nfs.subDir` with `type: local` is rejected.
- `ToVolumeContext` → `ConfigFromVolumeContext` round-trips a resolved
  `subDir`.
- `Registry.Get` returns `<mount>/<subDir>` and creates it.
- Two configs differing only in `subDir` produce exactly one `Mount`
  call against a counting fake mounter.
- `MountedPaths()` deduplicates when `subDir` is empty and includes both
  store paths and adopted mount roots.

### End-to-end

`subDir` is NFS-only, so coverage belongs in the `make e2e-nfs` variant,
which is the only layer that exercises kubelet, the real sidecars and a
real NFSv3 export together — and therefore the only layer that proves
the `--extra-create-metadata` flag actually does its job. A StorageClass
with `subDir: ${pvc.metadata.namespace}/fileblock`, a PVC and pod in a
test namespace, asserting that the `.img` appears under the namespace
directory on the export and **not** at the export root.

## Documentation

- README: `backingStore.nfs.subDir` row in the parameter table, and a
  worked per-namespace example using the corrected token spelling.
- CHANGELOG: entry under `## [Unreleased]`, calling out that the
  provisioner now runs with `--extra-create-metadata=true` and that
  existing volumes and storeIDs are unaffected.
- CLAUDE.md: the convention amendment above.

## Out of scope

- `subDir` for the local backing store. The parameter stays
  `backingStore.nfs.*`, matching the existing per-type grouping.
- Migrating existing volumes into a `subDir`. A store with a `subDir`
  set is a different store; moving `.img` files between them is an
  operator task, and one this driver deliberately does not automate.
- Deleting an empty namespace directory when its last volume goes away.
  Directories are cheap and an empty one is the correct steady state for
  a namespace between volumes.

## Release

v0.4.0, cut via the `cut-release` workflow after this PR merges.
