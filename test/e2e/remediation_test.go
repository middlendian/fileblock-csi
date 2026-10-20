//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	remountROSC = "fileblock-remount-ro"
	// The monitor checks every 10s, so a failure is acted on well inside this.
	remediationTimeout = 60 * time.Second
)

// TestFailedVolumeIsRemediated forces a staged ext4 read-only and expects
// fileblock-node to report it on the pod and delete the pod, so that its
// Deployment brings up a replacement with a writable volume.
func TestFailedVolumeIsRemediated(t *testing.T) {
	ns := makeNamespace(t)
	applyRemountROSC(t)
	applyYAML(t, pvcManifestSC(ns, "vol", "128Mi", remountROSC))
	applyYAML(t, deploymentWithPVC(ns, "app", "vol", ""))

	old := waitDeploymentPod(t, ns, "app", "")
	failVolume(t, ns, old)

	waitFailedEvent(t, ns, old.uid)
	waitPodUIDGone(t, ns, "app", old.uid)
	repl := waitDeploymentPod(t, ns, "app", old.uid)
	assertWritable(t, ns, repl.name)
}

// TestFailedVolumeRestagesOnSameNode pins the replacement to the failed
// volume's node. The kubelet keeps the volume staged there for the
// replacement, so only the driver's re-stage on publish can hand it a
// working filesystem.
func TestFailedVolumeRestagesOnSameNode(t *testing.T) {
	node := nodeNames(t)[0]
	ns := makeNamespace(t)
	applyRemountROSC(t)
	applyYAML(t, pvcManifestSC(ns, "vol", "128Mi", remountROSC))
	applyYAML(t, deploymentWithPVC(ns, "app", "vol", node))

	old := waitDeploymentPod(t, ns, "app", "")
	if old.node != node {
		t.Fatalf("pod %s landed on %s, want %s", old.name, old.node, node)
	}
	volumeID := pvForPVC(t, ns, "vol")
	failVolume(t, ns, old)

	waitFailedEvent(t, ns, old.uid)
	waitPodUIDGone(t, ns, "app", old.uid)
	repl := waitDeploymentPod(t, ns, "app", old.uid)
	if repl.node != node {
		t.Fatalf("replacement %s landed on %s, want %s", repl.name, repl.node, node)
	}
	assertWritable(t, ns, repl.name)

	// A fresh superblock (checked by assertWritable) is the proof; the log
	// says which path produced it. The kubelet unstaging and re-staging the
	// volume itself would be just as good, but logs nothing at info level.
	nodePod := dsPodOnNode(t, "fileblock-system", "fileblock-node", node)
	logs := kubectl(t, "-n", "fileblock-system", "logs", nodePod, "-c", "fileblock-node")
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "re-staged failed volume") && strings.Contains(line, volumeID) {
			t.Logf("node plugin re-staged in place: %s", line)
			return
		}
	}
	t.Logf("no in-place re-stage logged for %s; the kubelet must have unstaged and re-staged it", volumeID)
}

// TestPodDeletePolicyDeniesOtherNode checks the admission policy that limits
// fileblock-node's cluster-wide pod delete: a token bound to the node
// plugin's pod on node A cannot delete a pod on node B, but can delete a
// controller-owned PVC pod on node A.
func TestPodDeletePolicyDeniesOtherNode(t *testing.T) {
	nodes := nodeNames(t)
	if len(nodes) < 2 {
		t.Skipf("need >=2 nodes, have %d", len(nodes))
	}
	nodeA, nodeB := nodes[0], nodes[1]
	ns := makeNamespace(t)

	applyYAML(t, pvcManifest(ns, "vol-b", "64Mi"))
	applyYAML(t, podOnNode(ns, "pod-b", "vol-b", nodeB, "sleep 3600\n"))
	applyYAML(t, pvcManifest(ns, "vol-a", "64Mi"))
	applyYAML(t, deploymentWithPVC(ns, "app-a", "vol-a", nodeA))
	waitPodReady(t, ns, "pod-b", defaultPodReady)
	podA := waitDeploymentPod(t, ns, "app-a", "")

	nodePod := dsPodOnNode(t, "fileblock-system", "fileblock-node", nodeA)
	token := strings.TrimSpace(kubectl(t, "-n", "fileblock-system", "create", "token", "fileblock-node",
		"--bound-object-kind", "Pod", "--bound-object-name", nodePod, "--duration", "10m"))
	kc := tokenKubeconfig(t, token)

	// --wait=false: the role grants no watch, which a waiting delete needs.
	out, err := kubectlRaw("--kubeconfig", kc, "-n", ns, "delete", "pod", "pod-b", "--wait=false")
	if err == nil {
		t.Fatalf("delete of a pod on another node succeeded:\n%s", out)
	}
	if !strings.Contains(out, "may only delete pods on its own node") {
		t.Fatalf("delete of a pod on another node failed for the wrong reason: %v\n%s", err, out)
	}

	if out, err := kubectlRaw("--kubeconfig", kc, "-n", ns, "delete", "pod", podA.name, "--wait=false"); err != nil {
		t.Fatalf("delete of a controller-owned PVC pod on the token's node failed: %v\n%s", err, out)
	}
	waitPodUIDGone(t, ns, "app-a", podA.uid)
}

// applyRemountROSC applies a clone of the default `fileblock` StorageClass
// that mounts with errors=remount-ro, so an ext4 error makes the superblock
// read-only: the condition the monitor looks for.
func applyRemountROSC(t *testing.T) {
	t.Helper()
	raw := kubectl(t, "get", "sc", "fileblock", "-o", "jsonpath={.parameters}")
	params := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &params); err != nil {
		t.Fatalf("parse fileblock SC parameters %q: %v", raw, err)
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %q\n", k, params[k])
	}
	applyYAML(t, fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
provisioner: fileblock.csi
parameters:
%smountOptions: [errors=remount-ro]
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
`, remountROSC, b.String()))
	t.Cleanup(func() { _, _ = kubectlRaw("delete", "sc", remountROSC, "--ignore-not-found") })
}

// deploymentWithPVC renders a one-replica Deployment (so its pods are
// controller-owned) mounting pvc at /data. A non-empty node pins it there
// through nodeSelector, which unlike nodeName still lets WaitForFirstConsumer
// provisioning see the scheduler's choice. The short grace period stops
// `sleep` as PID 1, which ignores SIGTERM, holding the volume for 30s.
func deploymentWithPVC(ns, name, pvc, node string) string {
	selector := ""
	if node != "" {
		selector = fmt.Sprintf("      nodeSelector:\n        kubernetes.io/hostname: %s\n", node)
	}
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %[1]s
  template:
    metadata:
      labels:
        app: %[1]s
    spec:
      terminationGracePeriodSeconds: 1
%[3]s      tolerations:
        - operator: Exists
      containers:
        - name: shell
          image: debian:bookworm-slim
          command: [sleep, "3600"]
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: %[4]s
`, name, ns, selector, pvc)
}

type podRef struct{ name, uid, node string }

// waitDeploymentPod waits for a Ready, non-terminating pod of the
// Deployment whose UID is not notUID.
func waitDeploymentPod(t *testing.T, ns, app, notUID string) podRef {
	t.Helper()
	var p podRef
	eventually(t, defaultPodReady, func() error {
		out, err := kubectlRaw("-n", ns, "get", "pods", "-l", "app="+app, "-o",
			`jsonpath={range .items[*]}{.metadata.name}{"\t"}{.metadata.uid}{"\t"}{.spec.nodeName}{"\t"}{.metadata.deletionTimestamp}{"\t"}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}`)
		if err != nil {
			return fmt.Errorf("list pods: %v: %s", err, out)
		}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			f := strings.Split(line, "\t")
			if len(f) == 5 && f[1] != notUID && f[3] == "" && f[4] == "True" {
				p = podRef{name: f[0], uid: f[1], node: f[2]}
				return nil
			}
		}
		return fmt.Errorf("no Ready pod of %s other than uid %q yet:\n%s", app, notUID, out)
	})
	return p
}

// dataMount returns the device number and superblock options of the pod's
// /data mount, from its mountinfo.
func dataMount(t *testing.T, ns, pod string) (majMin string, superOpts []string) {
	t.Helper()
	out := kubectl(t, "-n", ns, "exec", pod, "--", "cat", "/proc/self/mountinfo")
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[4] != "/data" {
			continue
		}
		// Optional fields end at "-", then fstype, source, super options.
		for i := 6; i+3 < len(f); i++ {
			if f[i] == "-" {
				return f[2], strings.Split(f[i+3], ",")
			}
		}
	}
	t.Fatalf("no /data mount in %s/%s mountinfo:\n%s", ns, pod, out)
	return "", nil
}

func hasOpt(opts []string, want string) bool {
	for _, o := range opts {
		if o == want {
			return true
		}
	}
	return false
}

// failVolume raises an ext4 error on the pod's volume through sysfs on its
// kind node. With errors=remount-ro, ext4 makes the superblock read-only
// while each mount stays rw.
func failVolume(t *testing.T, ns string, p podRef) {
	t.Helper()
	majMin, _ := dataMount(t, ns, p.name)
	dev := strings.TrimSpace(nodeExec(t, p.node, fmt.Sprintf("basename \"$(readlink /sys/dev/block/%s)\"", majMin)))
	if dev == "" || dev == "." {
		t.Fatalf("no block device for %s on %s", majMin, p.node)
	}
	nodeExec(t, p.node, fmt.Sprintf("echo 1 > /sys/fs/ext4/%s/trigger_fs_error", dev))
	if _, opts := dataMount(t, ns, p.name); !hasOpt(opts, "ro") {
		t.Fatalf("superblock of %s still %v after trigger_fs_error; is errors=remount-ro in effect?", dev, opts)
	}
	t.Logf("forced %s (%s) read-only on %s", dev, majMin, p.node)
}

// nodeExec runs a shell script on a kind node. Kind names each node's
// container after the node.
func nodeExec(t *testing.T, node, script string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", node, "sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec %s sh -c %q: %v\n%s", node, script, err, out)
	}
	return string(out)
}

func waitFailedEvent(t *testing.T, ns, uid string) {
	t.Helper()
	eventually(t, remediationTimeout, func() error {
		out, err := kubectlRaw("-n", ns, "get", "events",
			"--field-selector", "reason=FileblockVolumeFailed,involvedObject.uid="+uid,
			"-o", "jsonpath={.items[*].message}")
		if err != nil {
			return fmt.Errorf("get events: %v: %s", err, out)
		}
		if strings.TrimSpace(out) == "" {
			return fmt.Errorf("no FileblockVolumeFailed event on pod uid %s", uid)
		}
		return nil
	})
}

func waitPodUIDGone(t *testing.T, ns, app, uid string) {
	t.Helper()
	eventually(t, remediationTimeout, func() error {
		out, err := kubectlRaw("-n", ns, "get", "pods", "-l", "app="+app,
			"-o", "jsonpath={.items[*].metadata.uid}")
		if err != nil {
			return fmt.Errorf("list pods: %v: %s", err, out)
		}
		if hasOpt(strings.Fields(out), uid) {
			return fmt.Errorf("pod uid %s still present", uid)
		}
		return nil
	})
}

// assertWritable checks that the pod got a fresh superblock, not the one
// that was forced read-only, and that a write lands.
func assertWritable(t *testing.T, ns, pod string) {
	t.Helper()
	if _, opts := dataMount(t, ns, pod); hasOpt(opts, "ro") {
		t.Fatalf("replacement %s was handed the read-only superblock: %v", pod, opts)
	}
	kubectl(t, "-n", ns, "exec", pod, "--", "sh", "-c", "touch /data/ok && sync")
}

// tokenKubeconfig writes a kubeconfig that authenticates with token alone.
// Passing --token next to the harness kubeconfig would also present its
// admin client certificate, which the API server checks first.
func tokenKubeconfig(t *testing.T, token string) string {
	t.Helper()
	server := strings.TrimSpace(kubectl(t, "config", "view", "--minify",
		"-o", "jsonpath={.clusters[0].cluster.server}"))
	ca := strings.TrimSpace(kubectl(t, "config", "view", "--minify", "--raw",
		"-o", "jsonpath={.clusters[0].cluster.certificate-authority-data}"))
	doc := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
  - name: e2e
    cluster:
      server: %s
      certificate-authority-data: %s
users:
  - name: fileblock-node
    user:
      token: %s
contexts:
  - name: e2e
    context: {cluster: e2e, user: fileblock-node}
current-context: e2e
`, server, ca, token)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
