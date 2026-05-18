//go:build e2e

// Package e2e drives kubelet-mediated tests that csi-sanity and the bare
// binary smoke test cannot: pod-level chmod / fs-type, flock semantics on
// the loop-mounted ext4, offline expand through the resizer sidecar, and a
// real cross-node takeover when the backing store is shared. The harness
// (hack/e2e.sh) is responsible for cluster bring-up and teardown — these
// tests assume a kind cluster with the e2e overlay already applied.
package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	defaultPodReady = 120 * time.Second
	defaultExpand   = 180 * time.Second
)

// TestExt4AndExecuteBit is the headline assertion: a pod sees a real ext4
// filesystem under /data and chmod +x lands. This is the entire point of
// fileblock — if it ever regresses, every other test in this suite is
// downstream of the bug.
func TestExt4AndExecuteBit(t *testing.T) {
	ns := makeNamespace(t)
	applyYAML(t, pvcManifest(ns, "vol", "128Mi"))
	applyYAML(t, podWithScript(ns, "demo", "vol", `
set -eu
cd /data
echo '#!/bin/sh' > hello.sh
echo 'echo executed' >> hello.sh
chmod +x hello.sh
test -x hello.sh
./hello.sh > /tmp/out
grep -q '^executed$' /tmp/out
# ext4 reports "ext2/ext3" as the human-readable fs type via stat -f.
stat -f -c '%T' /data | grep -q '^ext2/ext3$'
sleep 3600
`))
	waitPodReady(t, ns, "demo", defaultPodReady)
	out := kubectl(t, "-n", ns, "exec", "demo", "--",
		"sh", "-c", "ls -l /data/hello.sh && stat -f -c %T /data")
	if !strings.Contains(out, "ext2/ext3") {
		t.Fatalf("expected ext4 filesystem, got: %s", out)
	}
	if !strings.Contains(out, "-rwxr-xr-x") && !strings.Contains(out, "rwx") {
		t.Fatalf("execute bit not set on hello.sh:\n%s", out)
	}
}

// TestExecuteBitSurvivesPodRestart exercises the unstage->stage cycle: a
// pod chmods +x, gets deleted, a fresh pod re-mounts the same PVC, and the
// bit is still there. NFSv3 fails this test in the absence of fileblock —
// it's the regression we're guarding against.
func TestExecuteBitSurvivesPodRestart(t *testing.T) {
	ns := makeNamespace(t)
	applyYAML(t, pvcManifest(ns, "vol", "128Mi"))

	// Phase 1: drop a +x script.
	applyYAML(t, podWithScript(ns, "writer", "vol", `
set -eu
cd /data
echo '#!/bin/sh' > marker.sh
echo 'echo persisted' >> marker.sh
chmod +x marker.sh
sleep 3600
`))
	waitPodReady(t, ns, "writer", defaultPodReady)
	kubectl(t, "-n", ns, "delete", "pod", "writer", "--wait=true")
	waitPodGone(t, ns, "writer", 60*time.Second)

	// Phase 2: re-mount and check.
	applyYAML(t, podWithScript(ns, "reader", "vol", `
set -eu
test -x /data/marker.sh
/data/marker.sh > /tmp/out
grep -q '^persisted$' /tmp/out
sleep 3600
`))
	waitPodReady(t, ns, "reader", defaultPodReady)
}

// TestNodeContainerRestartRecovery exercises the AdoptExisting cache-
// poisoning regression: a fileblock-node container restart within the
// same pod preserves the emptyDir backing the stores volume — including
// the <storeID> directory the prior container created when it mounted
// the backing source. The post-restart container must NOT adopt that
// directory as "mounted"; the next NodeStageVolume must do a real mount.
//
// Pre-fix, the next stage returned `code = NotFound desc = image …: no
// such file or directory` for the still-present .img and consumer pods
// hung in ContainerCreating until a new pod UID forced a fresh emptyDir.
//
// Reproduction shape:
//  1. Apply PVC + consumer pod, wait for Ready (NodeStage → backing
//     source mounted, <storeID> dir populated, .img created).
//  2. Delete the consumer pod (NodeUnstage releases the staging mount;
//     the per-process registry cache is in-memory only).
//  3. SIGTERM fileblock-node PID 1 on the consumer's node — same pod
//     sandbox, fresh container, emptyDir contents preserved on disk.
//     SIGKILL would be ideal but the kernel blocks it from within the
//     same PID namespace unless init has a handler installed (which
//     SIGKILL can't have); SIGTERM is delivered because fileblock-node
//     calls signal.NotifyContext on SIGTERM, which cancels Serve's ctx
//     and exits the binary cleanly. The container restart that follows
//     is what we're after.
//  4. Wait for fileblock-node to come back Ready (AdoptExisting runs).
//  5. Re-apply the consumer pod and wait for Ready. Assert the marker
//     file from run 1 is still there — proves the new stage actually
//     re-mounted the same backing source rather than handing out an
//     empty mountpoint.
func TestNodeContainerRestartRecovery(t *testing.T) {
	ns := makeNamespace(t)
	applyYAML(t, pvcManifest(ns, "vol", "64Mi"))

	applyYAML(t, podWithScript(ns, "consumer", "vol", `
set -eu
echo first-run > /data/marker
sleep 3600
`))
	waitPodReady(t, ns, "consumer", defaultPodReady)
	node := nodeOfPod(t, ns, "consumer")
	nodePod := dsPodOnNode(t, "fileblock-system", "fileblock-node", node)

	kubectl(t, "-n", ns, "delete", "pod", "consumer", "--wait=true")
	waitPodGone(t, ns, "consumer", 60*time.Second)

	rcBefore := strings.TrimSpace(kubectl(t, "-n", "fileblock-system", "get", "pod", nodePod,
		"-o", "jsonpath={.status.containerStatuses[?(@.name=='fileblock-node')].restartCount}"))

	// SIGTERM PID 1 via the shell builtin (the runtime image doesn't
	// ship the standalone /bin/kill — only sh's builtin is reliable
	// here). fileblock-node's signal.NotifyContext handler cancels
	// Serve's ctx, the binary exits cleanly, and kubelet restarts the
	// container in the same pod sandbox. The exec connection drops as
	// the container dies, so we ignore its return.
	_, _ = kubectlRaw("-n", "fileblock-system", "exec", nodePod,
		"-c", "fileblock-node", "--", "sh", "-c", "kill -TERM 1")

	// Wait for the restart to register and the new container to become
	// Ready. The restart-count bump is the cleanest signal that the
	// fresh container has actually started (AdoptExisting has run).
	eventually(t, defaultPodReady, func() error {
		rcAfter := strings.TrimSpace(kubectl(t, "-n", "fileblock-system", "get", "pod", nodePod,
			"-o", "jsonpath={.status.containerStatuses[?(@.name=='fileblock-node')].restartCount}"))
		ready := strings.TrimSpace(kubectl(t, "-n", "fileblock-system", "get", "pod", nodePod,
			"-o", "jsonpath={.status.containerStatuses[?(@.name=='fileblock-node')].ready}"))
		if rcAfter == rcBefore {
			return fmt.Errorf("restartCount still %s, want > %s", rcAfter, rcBefore)
		}
		if ready != "true" {
			return fmt.Errorf("fileblock-node container not Ready yet: ready=%q", ready)
		}
		return nil
	})

	// Force the consumer back onto the same node so the regression
	// path — the post-restart fileblock-node serving NodeStageVolume —
	// is the one under test. Without pinning, the scheduler is free to
	// land the pod on a different node whose node-plugin never saw the
	// stale dir.
	applyYAML(t, podOnNode(ns, "consumer", "vol", node, `
set -eu
test -f /data/marker
grep -q first-run /data/marker
sleep 3600
`))
	waitPodReady(t, ns, "consumer", defaultPodReady)
}

// TestFlockSerializes asserts that flock(2) on the loop-mounted ext4 has
// real local-disk semantics — two processes contending on the same lock
// file see a non-blocking flock fail when the lock is held. Bind-mounted
// NFS would happily let both succeed.
func TestFlockSerializes(t *testing.T) {
	ns := makeNamespace(t)
	applyYAML(t, pvcManifest(ns, "vol", "128Mi"))
	applyYAML(t, podWithScript(ns, "lockpod", "vol", `
set -eu
apt-get update >/dev/null 2>&1 || true
which flock >/dev/null
cd /data
# Hold an exclusive lock for 10s in the background, then try to grab it
# non-blocking — must fail with exit code 1.
( flock -x lockfile -c 'sleep 10' ) &
sleep 1
if flock -n lockfile -c true; then
  echo "FAIL: non-blocking flock should have been rejected" >&2
  exit 1
fi
wait
echo flock-ok > /tmp/done
sleep 3600
`))
	waitPodReady(t, ns, "lockpod", defaultPodReady)
	eventually(t, 30*time.Second, func() error {
		out, err := kubectlRaw("-n", ns, "exec", "lockpod", "--", "cat", "/tmp/done")
		if err != nil {
			return fmt.Errorf("waiting for flock test: %s", out)
		}
		if !strings.Contains(out, "flock-ok") {
			return fmt.Errorf("flock test not finished: %s", out)
		}
		return nil
	})
}

// TestOfflineExpand exercises the resizer sidecar end to end: a PVC's
// requested capacity is bumped, the consuming pod is recreated, and the
// new mount reflects the larger size. The driver advertises OFFLINE
// expansion so the pod restart is part of the contract.
func TestOfflineExpand(t *testing.T) {
	ns := makeNamespace(t)
	applyYAML(t, pvcManifest(ns, "vol", "128Mi"))
	applyYAML(t, podWithScript(ns, "user", "vol", `
set -eu
df -B1 /data | tail -n1
sleep 3600
`))
	waitPodReady(t, ns, "user", defaultPodReady)

	// Capture the original byte capacity reported by df, then ask for 3x.
	beforeBytes := dfBytes(t, ns, "user", "/data")
	kubectl(t, "-n", ns, "delete", "pod", "user", "--wait=true")
	waitPodGone(t, ns, "user", 60*time.Second)

	kubectl(t, "-n", ns, "patch", "pvc", "vol",
		"--type=merge",
		"-p", `{"spec":{"resources":{"requests":{"storage":"384Mi"}}}}`)

	// Wait for the controller-side expand to complete before recreating the
	// pod. fileblock advertises OFFLINE expansion, so the external-resizer
	// holds back ControllerExpandVolume until the volume is unpublished; if
	// we recreate the pod first the resizer will never make progress.
	//
	// We watch the *PV*'s spec.capacity rather than the PVC's status.capacity:
	// after ControllerExpandVolume succeeds, the resizer updates PV.spec.capacity
	// and marks the PVC FileSystemResizePending. PVC.status.capacity itself only
	// flips to the new size after NodeExpandVolume runs — which only happens on
	// the next NodeStageVolume — so waiting on it here would deadlock.
	pv := strings.TrimSpace(kubectl(t, "-n", ns, "get", "pvc", "vol",
		"-o", "jsonpath={.spec.volumeName}"))
	eventually(t, defaultExpand, func() error {
		out := strings.TrimSpace(kubectl(t, "get", "pv", pv,
			"-o", "jsonpath={.spec.capacity.storage}"))
		if out == "" || out == "128Mi" {
			return fmt.Errorf("PV spec.capacity not yet expanded: %q", out)
		}
		return nil
	})

	// Re-create the pod and wait for the mount to reflect the new size. The
	// node plugin runs e2fsck + resize2fs on stage, so the new capacity lands
	// on the first NodeStageVolume after the controller-side expand.
	applyYAML(t, podWithScript(ns, "user", "vol", `
set -eu
df -B1 /data | tail -n1
sleep 3600
`))
	waitPodReady(t, ns, "user", defaultPodReady)

	eventually(t, defaultExpand, func() error {
		got := dfBytes(t, ns, "user", "/data")
		if got <= beforeBytes {
			return fmt.Errorf("expected capacity > %d after expand, got %d", beforeBytes, got)
		}
		return nil
	})
}

// TestCrossNodeTakeover validates the fileblock-specific cross-node handoff
// the project's design notes call out: a pod on node A is removed, a pod on
// node B takes the same volume, and reads the data the first pod wrote.
// Cross-node mutual exclusion is the kubelet's job — fileblock advertises
// SINGLE_NODE_WRITER and trusts CSI to serialize Stage/Unstage. This test
// just exercises the data-persistence half of the handoff. It requires the
// e2e overlay's shared-topology setting; without it the PV would be pinned
// to one node and the second pod would never schedule.
func TestCrossNodeTakeover(t *testing.T) {
	nodes := nodeNames(t)
	if len(nodes) < 2 {
		t.Skipf("need >=2 nodes for cross-node takeover, have %d", len(nodes))
	}
	nodeA, nodeB := nodes[0], nodes[1]
	ns := makeNamespace(t)
	applyYAML(t, pvcManifest(ns, "vol", "128Mi"))

	applyYAML(t, podOnNode(ns, "pod-a", "vol", nodeA, `
set -eu
echo node-a > /data/who
sleep 3600
`))
	waitPodReady(t, ns, "pod-a", defaultPodReady)

	kubectl(t, "-n", ns, "delete", "pod", "pod-a", "--wait=true")
	waitPodGone(t, ns, "pod-a", 60*time.Second)

	applyYAML(t, podOnNode(ns, "pod-b", "vol", nodeB, `
set -eu
grep -q '^node-a$' /data/who
echo node-b >> /data/who
sleep 3600
`))
	waitPodReady(t, ns, "pod-b", defaultPodReady)
}

// TestNonRootPodCanWriteWithFsGroup mounts a fileblock PVC into a pod
// that runs as a non-root user and declares an fsGroup, then asserts the
// container can create a file at the root of the mount. This regresses
// without `fsGroupPolicy: File` on the CSIDriver: the API server default
// (ReadWriteOnceWithFSType) tells kubelet to skip fsGroup chown unless
// the PV declares a csi.fsType, which fileblock does not. The mount
// would then be owned root:root 0755 and the non-root container would
// fail to write with EACCES.
func TestNonRootPodCanWriteWithFsGroup(t *testing.T) {
	ns := makeNamespace(t)
	applyYAML(t, pvcManifest(ns, "vol", "128Mi"))
	applyYAML(t, podNonRoot(ns, "writer", "vol", `
set -eu
id
touch /data/owned-by-fsgroup
ls -ln /data/owned-by-fsgroup
test -O /data/owned-by-fsgroup
sleep 3600
`))
	waitPodReady(t, ns, "writer", defaultPodReady)
	out := kubectl(t, "-n", ns, "exec", "writer", "--",
		"sh", "-c", "stat -c '%u %g %a' /data && stat -c '%u %g' /data/owned-by-fsgroup")
	if !strings.Contains(out, "1000") {
		t.Fatalf("expected uid/gid 1000 ownership on mount or written file, got: %s", out)
	}
}

// TestNFSv3BackingProperties only runs when E2E_BACKING_KIND=nfs. It first
// confirms the controller pod really sees an NFS-mounted backing store
// (otherwise the rest of the assertion is meaningless), then asserts that a
// fileblock-mediated PVC fixes the three NFSv3 pathologies the README
// promises to fix: chmod +x is preserved, the in-pod fs reports as ext4,
// and flock(2) has real local-disk semantics. Without fileblock these would
// silently misbehave on NFSv3.
func TestNFSv3BackingProperties(t *testing.T) {
	if os.Getenv("E2E_BACKING_KIND") != "nfs" {
		t.Skip("E2E_BACKING_KIND != nfs; skipping NFSv3-specific properties test")
	}

	ns := makeNamespace(t)
	applyYAML(t, pvcManifest(ns, "vol", "128Mi"))
	applyYAML(t, podWithScript(ns, "nfs-props", "vol", `
set -eu
cd /data
# Property 1: in-pod fs is ext4, not NFS. This is the structural fix —
# everything else follows from the loop-mounted ext4 image.
stat -f -c '%T' . | grep -q '^ext2/ext3$'

# Property 2: chmod +x lands and survives a re-stat. NFSv3 strips this in
# the absence of the experimental "acl" extension.
echo '#!/bin/sh' > exec.sh
chmod +x exec.sh
test -x exec.sh

# Property 3: a non-blocking flock fails when the lock is held. NFSv3 NLM
# would let both succeed against the raw export; the loop-mounted ext4
# inside the .img is local and serializes correctly.
( flock -x lockfile -c 'sleep 5' ) &
sleep 1
if flock -n lockfile -c true; then
  echo "FAIL: non-blocking flock should have been rejected" >&2
  exit 1
fi
wait

# Property 4: git core.fileMode round-trip — the historical NFSv3 footgun
# that was the original motivation for fileblock. After a chmod +x and
# commit, working tree must be clean.
apt-get update -qq >/dev/null 2>&1 || true
which git >/dev/null 2>&1 || apt-get install -y -qq git >/dev/null 2>&1
mkdir repo && cd repo
git init -q
touch a.sh && chmod +x a.sh
git -c user.email=t@t -c user.name=t add . >/dev/null
git -c user.email=t@t -c user.name=t commit -q -m a
test -z "$(git status --porcelain)"
echo nfsv3-ok > /tmp/done
sleep 3600
`))
	waitPodReady(t, ns, "nfs-props", defaultPodReady)

	// 1. The backing store the controller writes to is actually NFS — checked
	//    after the pod is ready so that CreateVolume has already triggered a
	//    store mount under /var/lib/fileblock/stores/<storeID>/. Checking the
	//    parent /var/lib/fileblock/stores itself would see the emptyDir cache
	//    volume, not the per-store NFS mount that lives one level deeper.
	storeCheck := kubectl(t, "-n", "fileblock-system", "exec",
		"deploy/fileblock-controller", "-c", "fileblock-controller",
		"--", "sh", "-c",
		`stat -f -c %T /var/lib/fileblock/stores/$(ls /var/lib/fileblock/stores | head -1)`)
	if !strings.Contains(storeCheck, "nfs") {
		t.Fatalf("expected backing store to be on NFS, stat reported: %q", storeCheck)
	}

	eventually(t, 60*time.Second, func() error {
		out, err := kubectlRaw("-n", ns, "exec", "nfs-props", "--", "cat", "/tmp/done")
		if err != nil {
			return fmt.Errorf("waiting for nfs-properties script: %s", out)
		}
		if !strings.Contains(out, "nfsv3-ok") {
			return fmt.Errorf("nfs-properties script not finished: %s", out)
		}
		return nil
	})
}

// dfBytes parses the second column of `df -B1 /path | tail -n1` (the 1-KiB
// scaled total) so we can assert capacity changes without parsing units.
func dfBytes(t *testing.T, ns, pod, path string) int64 {
	t.Helper()
	out := kubectl(t, "-n", ns, "exec", pod, "--",
		"sh", "-c", "df -B1 "+path+" | tail -n1")
	fields := strings.Fields(out)
	if len(fields) < 2 {
		t.Fatalf("unparseable df output: %q", out)
	}
	var n int64
	if _, err := fmt.Sscanf(fields[1], "%d", &n); err != nil {
		t.Fatalf("could not parse df bytes %q: %v", fields[1], err)
	}
	return n
}

// podWithScript builds a debian-based pod that runs the given shell script
// against /data backed by `pvc`. We use bookworm-slim because it carries
// flock + util-linux out of the box.
func podWithScript(ns, name, pvc, script string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  restartPolicy: Never
  containers:
    - name: shell
      image: debian:bookworm-slim
      command: [/bin/sh, -c]
      args:
        - |
%s
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: %s
`, name, ns, indent(script, 10), pvc)
}

// podNonRoot is podWithScript with a pod-level securityContext that runs
// the container as uid:gid 1000:1000 with fsGroup 1000. The fsGroup is
// what triggers the kubelet's volume-ownership pass that this test is
// exercising — without it, no chown would be expected regardless of
// driver configuration.
func podNonRoot(ns, name, pvc, script string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  restartPolicy: Never
  securityContext:
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
  containers:
    - name: shell
      image: debian:bookworm-slim
      command: [/bin/sh, -c]
      args:
        - |
%s
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: %s
`, name, ns, indent(script, 10), pvc)
}

// podOnNode pins the pod to a specific node via nodeSelector rather than
// nodeName. nodeName bypasses the scheduler, which means the PVC never gets
// the volume.kubernetes.io/selected-node annotation that
// WaitForFirstConsumer provisioning waits for — the pod would sit in
// ContainerCreating forever. nodeSelector still lets the scheduler decide
// (and set the annotation) while constraining the node choice.
func podOnNode(ns, name, pvc, node, script string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: %s
  tolerations:
    - operator: Exists
  containers:
    - name: shell
      image: debian:bookworm-slim
      command: [/bin/sh, -c]
      args:
        - |
%s
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: %s
`, name, ns, node, indent(script, 10), pvc)
}

func indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}
