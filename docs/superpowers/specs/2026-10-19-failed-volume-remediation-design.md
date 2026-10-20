# Failed-volume remediation: recover volumes that ext4 forced read-only

**Status:** Draft
**Date:** 2026-10-19
**Target version:** v0.6.0

## Problem

A short burst of `ESTALE` from the NFS server breaks every fileblock volume
that has I/O in flight, and none of them recover on their own. NFS servers
commonly send `ESTALE` for a minute or two while they restart: storage or
exports go away before nfsd stops, or come back after it starts.

The chain, on Linux 7.0 (`fs/nfs`, `drivers/block/loop.c`):

1. **The server answers a READ, WRITE or GETATTR on the `.img` with
   `NFS3ERR_STALE`.** `hard` does not retry it, because it is a definitive
   reply and not a timeout.
2. **The client marks the inode stale, permanently.**
   - `nfs_readpage_done()` and `__nfs_revalidate_inode()` call
     `nfs_set_inode_stale()`, which sets `NFS_INO_STALE`.
   - Nothing ever clears it. After that, `nfs_read_folio()`,
     `nfs_readahead()` and `nfs_revalidate_inode()` return `-ESTALE`
     without sending an RPC.
   - `nfs_find_actor()` will not match a stale inode, so a new `open()` of
     the same path builds a fresh inode and works.
3. **The loop device keeps its `struct file`, and so that inode, for as long
   as it is attached.** Every later request to the loop fails with
   `BLK_STS_IOERR`. This continues long after the server is healthy again.
4. **ext4 hits the I/O error,** aborts its journal and forces the
   superblock read-only.
5. **None of this can be undone in place.**
   - `LOOP_CHANGE_FD` returns `-EINVAL` unless the loop is read-only.
   - ext4 will not return to read-write after a journal abort.
   - The volume has to be unmounted, detached, attached again (a fresh
     `open()`) and mounted again. That replays the journal at the
     `e2fsck` step.

Kubernetes never does that on its own. NodeUnstageVolume runs only once no
pod on the node uses the volume. Restarting a container, including one
restarted by a liveness probe, re-binds the same read-only mount. A
workload crash-loops, or serves errors, until a person deletes its pod.

Volumes that were idle during the `ESTALE` window are not affected: their
inode never received a stale reply.

The CSI spec can *report* this: `VolumeCondition` on `NodeGetVolumeStats`
(CSI 1.3+). Kubernetes, though, only turns it into an event and an alpha
kubelet metric. KEP-1432 leaves remediation to something outside the
driver.

## Solution

`fileblock-node` gets a **failed-volume monitor**. It notices when a staged
volume's ext4 superblock has been forced read-only and reports it. By
default it also deletes the pods using that volume on its own node, so
their controller recreates them. The usual unstage then runs, or
fileblock's internal re-stage (below) when the replacement lands on the
same node. Either way the volume is attached again from a fresh `open()`.

The node plugin is the only component that can see the whole chain: volume
→ loop → dm-crypt → staging mount → publish targets. It is already
privileged on the host, so a delete permission limited to its own node adds
little. That limit is enforced in the cluster by a
ValidatingAdmissionPolicy shipped in `deploy/kustomize/base`.

The interface copies Longhorn's `auto-delete-pod-when-volume-detached-unexpectedly`:
- one boolean, on by default;
- only controller-owned pods are deleted;
- a pod is deleted only if it started before the volume failed.

Longhorn also has a denylist of controller kinds. That is left out until a
workload needs one. An empty denylist behaves exactly like having none, so
adding it later breaks nothing.

## Detection

Every 10s (a constant, not a flag), the monitor reads
`/proc/self/mountinfo` for each volume in the state file.

**Trigger:** the staging mount's **per-mount** options say `rw`, but its
**super options** say `ro`.
- ext4 changes only the superblock flag when it forces itself read-only,
  so a mismatch means "this was mounted read-write and the filesystem gave
  up". A volume mounted `ro` on purpose (`ro` in its mount flags) has both
  fields `ro` and never triggers.
- `statfs(2)` is not enough on its own, because its `ST_RDONLY` merges
  the two.

**Why not active probes.** Reading the loop device, or `stat`ing the
`.img`, sends a READ or GETATTR to the server. Inside an `ESTALE` window,
that one RPC would poison an inode that would otherwise have survived.
Detection has to be passive. Reading mountinfo touches no storage and
cannot hang on a dead hard mount.

**Not detected in v1:** a volume that only reads data blocks. A poisoned
inode returns EIO to the application on each data read. ext4 does not
force itself read-only for data-block read errors, so the trigger never
fires. Watching `/dev/kmsg` for `I/O error, dev loopN` would catch this,
and it is passive. It is left out of v1 because the kernel rate-limits
those lines, and because a read error on its own does not show the
volume is permanently broken.

A volume that triggers is **failed**, and the monitor records when it
failed (`failedAt`). That state is held in memory only, and a re-stage
clears it. When a volume enters it, the monitor:
- logs once at `warn`, with the volumeID, the loop and crypt devices, and
  the staging path;
- emits a `Warning` event `FileblockVolumeFailed` on every pod that
  publishes the volume;
- reports `VolumeCondition{abnormal: true, message: …}` from
  `NodeGetVolumeStats`. The node gains the `VOLUME_CONDITION` capability.

## Remediation

`--auto-delete-pod-when-volume-failed`, a boolean, **default `true`**. With
`false`, everything under Detection still happens, and the monitor logs
that it is not deleting.

For a failed volume, with the flag on:

1. **Find the publishing pods.** Look for mountinfo entries on the same
   device (`major:minor`) whose mount point ends in
   `/pods/<podUID>/volumes/kubernetes.io~csi/<pv>/mount`. This needs no
   state-file change, and it covers volumes published before the upgrade.
2. **Resolve the pods.** List pods with `fieldSelector=spec.nodeName=<node>`
   and match on UID.
3. **Skip pods that must not be deleted**, logging and emitting an event
   for each. These mirror the admission policy, so a denial there means a
   bug here:
   - no controlling owner reference (`metav1.GetControllerOf(pod) == nil`),
     because nothing would recreate it;
   - already terminating (`deletionTimestamp` set);
   - `status.startTime` unset or not before `failedAt`. This is Longhorn's
     loop guard: a replacement pod is never deleted for the failure that
     replaced it.
4. **Delete each remaining pod** with `preconditions.uid`, so a StatefulSet
   pod recreated under the same name is never hit, and with the pod's own
   grace period. Longhorn uses a fixed 30s; the pod's own lets workloads
   that need longer shut down cleanly.
5. **Hold off for 5 minutes** (a constant). The volume is not considered
   again during that time, even if a re-stage cleared its state and it
   failed again. Longhorn has no equivalent. This one guards against a
   filesystem that goes read-only again on every fresh mount, which would
   otherwise loop through delete and recreate. The hold-off is per volume,
   in memory, and does not double.

There is no per-node rate limit. The incident this addresses breaks many
volumes at once, and every one of them needs its pod deleted.

### Re-stage on the same node

If the replacement pod is scheduled to the same node while the old one is
still terminating, the kubelet may never unstage. It keeps the staging
mount for the new pod and only calls NodePublishVolume, which would bind
the same broken filesystem into the new pod.

To prevent that, **NodePublishVolume refuses to publish a failed volume.**
When it is called for a volume in the failed state:
- If any other publish target of that staging mount still exists (from
  mountinfo, as above), it returns `Unavailable`. The kubelet retries.
- If there are none, it **re-stages internally**, under the volume lock:
  1. unmount the staging path;
  2. close the crypt mapping, if any;
  3. detach the loop;
  4. run the normal NodeStageVolume attach → (open crypt) → `e2fsck` →
     mount sequence, with the stage parameters saved in the state file;
  5. clear the failed state and publish.

  If the re-stage fails, it returns `Internal` and the kubelet retries.

That needs one state-file change: `Mapping` gains the stage request's
volume context and mount flags (`StageContext`, `MountFlags`, both
optional).

**Encrypted volumes need their key to re-stage.** NodePublishVolume does
not receive the stage secret. And while the replacement pod wants the
volume, the kubelet will never unstage it, so an encrypted volume that
can't be re-staged internally stays stuck. To avoid that:
- NodeStageVolume keeps each encrypted volume's key **in process memory
  only**, from stage until unstage. It is never written to disk or the
  state file, and is zeroed on unstage.
- The kernel already holds the dm-crypt master key for as long as the
  mapping is open, so this widens exposure only to the plugin process,
  which is privileged on the host already.
- If the plugin restarted after staging, the key is gone. NodePublishVolume
  then returns `Unavailable` and emits an event saying the pod must be
  deleted by hand. That is the one remaining manual case.

This internal re-stage is also the first thing to check in an e2e test.
"The kubelet unstages before the same-node replacement publishes" is
assumed nowhere else in the design.

## Admission policy (shipped in `deploy/kustomize/base`)

`fileblock-node` gets RBAC `get, list, delete` on `pods` and
`create, patch` on `events`, through its existing ClusterRole. That grant
is cluster-wide. The new `remediation-policy.yaml`
(`ValidatingAdmissionPolicy` + `ValidatingAdmissionPolicyBinding`,
`validationActions: [Deny]`, `failurePolicy: Fail`) narrows it:

- **Match:** `DELETE` on `pods`, with a `matchCondition` that the caller's
  username matches `system:serviceaccount:*:fileblock-node`. The
  namespace is a wildcard on purpose. Kustomize's `namespace:` rewrites
  the ServiceAccount but not a string inside CEL. A broader match only
  restricts more callers, and grants nothing.
- **Validation 1:** `'authentication.kubernetes.io/node-name' in
  request.userInfo.extra && oldObject.spec.nodeName ==
  request.userInfo.extra['authentication.kubernetes.io/node-name'][0]`.
  This uses `oldObject`: on DELETE, `object` is null.
- **Validation 2:** `oldObject.metadata.?ownerReferences.orValue([]).exists(r,
  r.?controller.orValue(false))`.
- **Validation 3:** `oldObject.spec.?volumes.orValue([]).exists(v,
  has(v.persistentVolumeClaim))`. The pod must mount at least one PVC.
  - This is a sanity check, not a fileblock check. Proving that a PVC is
    fileblock means following PVC → PV → driver, and admission can't look
    up other objects. The driver's own check (the pod publishes the failed
    volume on this node) is what makes it fileblock-specific.
  - What it catches is the likeliest bug: resolving the wrong pod. A pod
    with no persistent storage can never be the right target.
  - fileblock supports only `Persistent` volumes, so a fileblock pod always
    has a `persistentVolumeClaim` volume. A generic ephemeral volume is not
    one and is denied; fileblock does not support those.

The policy guards against bugs in the remediation logic, not a compromised
plugin. A privileged node plugin with host PID can already stop any
container on its node through the container runtime, so a node-scoped
delete grants nothing new.

This needs **Kubernetes 1.30+**:
- that is where ValidatingAdmissionPolicy reached GA;
- and where pod-bound service-account tokens carry the node name in
  `userInfo.extra` by default (`ServiceAccountTokenPodNodeInfo`).

The README states this as the minimum for automatic deletion. Detection
and reporting need nothing new.

With the flag on, the plugin also checks at startup that the policy and
the binding exist by name, with `Deny` in the binding's actions. If they
don't, it logs an error and only detects and reports, because the RBAC
grant alone is not node-scoped. On a cluster older than 1.30, or where
only part of the base was applied, the default is therefore safe. This needs `get` on
`validatingadmissionpolicies` and `validatingadmissionpolicybindings`.

## Surface changes

| | |
|---|---|
| Flags (node) | `--auto-delete-pod-when-volume-failed` (`true`) |
| Constants | check interval 10s; per-volume hold-off 5m |
| CSI | Node: `VOLUME_CONDITION` |
| RBAC (`fileblock-node`) | pods `get, list, delete`; events `create, patch`; VAP and binding `get` |
| Manifests | `deploy/kustomize/base/remediation-policy.yaml` |
| State file | `Mapping.StageContext`, `Mapping.MountFlags` (optional; old files load unchanged) |
| Docs | README: a *Recovering from backing-store errors* section, the version requirement, and the flag; CLAUDE.md: CSI surface and state-file invariants |
| CHANGELOG | Leads with the behaviour change: after upgrading and applying the base, failed volumes' pods are deleted automatically; `--auto-delete-pod-when-volume-failed=false` turns that off |

## Testing

- **Unit (`pkg/mount`):** parse mountinfo per-mount vs super options. The
  cases: `rw`/`ro` triggers; `ro`/`ro` and `rw`/`rw` do not; a publish
  target is found by device and pod UID; the kubelet dir is prefix-agnostic.
- **Unit (monitor):** with fake pods and a fake clock, check:
  - owner and terminating skips;
  - a pod that started at or after `failedAt`, or has no start time, is
    skipped;
  - the 5-minute hold-off blocks a second delete even after the volume
    re-fails;
  - `preconditions.uid` is set;
  - with the flag off, nothing is deleted;
  - a missing policy or binding means detection only.
- **Unit (NodePublishVolume):** a failed volume with another live target
  gets `Unavailable`. With none, it re-stages a plaintext volume, and an
  encrypted one when its in-memory key is present; without the key it
  returns `Unavailable` plus an event. Unstage zeroes the cached key.
- **Policy:** a CEL test against the manifest, covering allow on the same
  node, deny on another node, deny unowned, deny a pod with no PVC volume,
  deny when `node-name` is missing, and no effect on other callers. Plus
  an e2e check that a cross-node delete by the plugin's service account is `Forbidden`.
- **e2e (kind, NFS variant):**
  1. Force a staged volume's ext4 read-only with
     `echo 1 > /sys/fs/ext4/<dev>/trigger_fs_error`, or use `dmsetup` to
     load an `error` target under the loop.
  2. Check the event and the `VolumeCondition`, then that the pod is
     deleted and its replacement mounts read-write.
  3. Run it once with the replacement pinned to the same node, to cover
     the internal re-stage.
- **Reproducing real `ESTALE`:** in `hack/`, a manual script that restarts
  the e2e NFS server with its export temporarily removed. It's documented
  and not in CI, because it depends on timing.

## Non-goals

- **Avoiding the downtime entirely.** This would mean a single-path
  dm-multipath with `queue_if_no_path` under ext4, swapping in a freshly
  opened loop when the old one errors. It is promising, but it needs a
  spike first, plus loop direct I/O so that writes the loop acknowledged
  aren't lost in a stale page cache.
- **Preventing the server's `ESTALE`.** That is the NFS server's
  behaviour, outside the driver.
- **Data written into the loop's page cache that never reached the
  server.** It is lost when the inode goes stale. ext4's journal replay
  brings the filesystem back to its last commit, not to that data.
- **Prometheus metrics.** fileblock exports none today. Events, logs and
  `VolumeCondition` cover alerting until it does.
