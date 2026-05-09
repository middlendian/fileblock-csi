//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// TestTwoStores provisions two independent SCs pointing at distinct local
// backing-store paths, creates one PVC+Pod from each, and asserts:
//  1. Both Pods reach Running.
//  2. The two PVs carry distinct storeID prefixes in their volumeHandle.
//  3. Each node DaemonSet pod that staged a PV contains exactly one .img file
//     under its matching store directory, and no .img file in the other store.
func TestTwoStores(t *testing.T) {
	ns := makeNamespace(t)

	applyYAML(t, scYAML("fileblock-store-a", "/srv/fileblock-source/a"))
	applyYAML(t, scYAML("fileblock-store-b", "/srv/fileblock-source/b"))
	t.Cleanup(func() {
		_, _ = kubectlRaw("delete", "sc", "fileblock-store-a", "--ignore-not-found")
		_, _ = kubectlRaw("delete", "sc", "fileblock-store-b", "--ignore-not-found")
	})

	applyYAML(t, pvcManifestSC(ns, "pvc-a", "128Mi", "fileblock-store-a"))
	applyYAML(t, pvcManifestSC(ns, "pvc-b", "128Mi", "fileblock-store-b"))
	applyYAML(t, podWithPVC(ns, "pod-a", "pvc-a"))
	applyYAML(t, podWithPVC(ns, "pod-b", "pvc-b"))

	waitPodReady(t, ns, "pod-a", defaultPodReady)
	waitPodReady(t, ns, "pod-b", defaultPodReady)

	// 1. Distinct storeID prefixes on the two volumeHandles.
	pvA := pvForPVC(t, ns, "pvc-a")
	pvB := pvForPVC(t, ns, "pvc-b")
	idA := mustParseStoreID(t, pvA)
	idB := mustParseStoreID(t, pvB)
	if idA == idB {
		t.Fatalf("expected distinct storeIDs, got %q for both", idA)
	}

	// 2. Each pod's node DaemonSet pod has exactly one .img file in the
	// matching store dir, and no .img file in the other store dir.
	for _, c := range []struct {
		pod          string
		id           string
		volumeHandle string
	}{
		{"pod-a", idA, pvA},
		{"pod-b", idB, pvB},
	} {
		nodeName := nodeOfPod(t, ns, c.pod)
		dsPod := dsPodOnNode(t, "fileblock-system", "fileblock-node", nodeName)
		out := execInPod(t, "fileblock-system", dsPod, "fileblock-node",
			fmt.Sprintf("ls /var/lib/fileblock/stores/%s/ 2>/dev/null || true", c.id))
		imgs := filterFiles(strings.Fields(out), ".img")
		if len(imgs) != 1 {
			t.Errorf("[%s] /var/lib/fileblock/stores/%s/ contains %d .img files: %v",
				c.pod, c.id, len(imgs), imgs)
		}
		expected := c.volumeHandle + ".img"
		if len(imgs) == 1 && imgs[0] != expected {
			t.Errorf("[%s] expected %s, got %s", c.pod, expected, imgs[0])
		}
	}
}

// scYAML renders a fileblock StorageClass with local backing at the given path.
func scYAML(name, path string) string {
	return fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
provisioner: fileblock.csi
parameters:
  backingStore.type: local
  backingStore.local.path: %s
  backingStore.local.shared: "true"
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
`, name, path)
}

// pvcManifestSC is like pvcManifest but uses an explicit storageClassName.
func pvcManifestSC(ns, name, size, sc string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: %s
  resources:
    requests:
      storage: %s
`, name, ns, sc, size)
}

// podWithPVC renders a minimal pod that mounts the given PVC at /data and
// sleeps indefinitely so it stays in Running state for inspection.
func podWithPVC(ns, name, pvc string) string {
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
      command: [sleep, "3600"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: %s
`, name, ns, pvc)
}

// pvForPVC returns the volumeHandle of the PV bound to the named PVC.
func pvForPVC(t *testing.T, ns, pvc string) string {
	t.Helper()
	handle := strings.TrimSpace(kubectl(t, "-n", ns, "get", "pvc", pvc,
		"-o", "jsonpath={.spec.volumeName}"))
	if handle == "" {
		t.Fatalf("pvc %s/%s has no bound volume", ns, pvc)
	}
	return strings.TrimSpace(kubectl(t, "get", "pv", handle,
		"-o", "jsonpath={.spec.csi.volumeHandle}"))
}

// mustParseStoreID extracts the 12-hex storeID from a volumeHandle of the
// form "fb-<12hexStoreID>-<name>".
func mustParseStoreID(t *testing.T, volumeHandle string) string {
	t.Helper()
	if !strings.HasPrefix(volumeHandle, "fb-") || len(volumeHandle) < 16 || volumeHandle[15] != '-' {
		t.Fatalf("malformed volumeHandle %q", volumeHandle)
	}
	return volumeHandle[3:15]
}

// nodeOfPod returns the node a Pod was scheduled on.
func nodeOfPod(t *testing.T, ns, pod string) string {
	t.Helper()
	node := strings.TrimSpace(kubectl(t, "-n", ns, "get", "pod", pod,
		"-o", "jsonpath={.spec.nodeName}"))
	if node == "" {
		t.Fatalf("pod %s/%s not yet scheduled", ns, pod)
	}
	return node
}

// dsPodOnNode returns the name of the DaemonSet pod matching dsName running on
// the given node.
func dsPodOnNode(t *testing.T, ns, dsName, nodeName string) string {
	t.Helper()
	out := kubectl(t, "-n", ns, "get", "pods",
		"-l", "app="+dsName,
		"--field-selector", "spec.nodeName="+nodeName,
		"-o", "jsonpath={.items[0].metadata.name}")
	name := strings.TrimSpace(out)
	if name == "" {
		t.Fatalf("no DaemonSet pod %s on node %s in namespace %s", dsName, nodeName, ns)
	}
	return name
}

// execInPod runs a shell command inside the named container of a pod and
// returns stdout+stderr combined.
func execInPod(t *testing.T, ns, pod, container, cmd string) string {
	t.Helper()
	return kubectl(t, "-n", ns, "exec", pod, "-c", container,
		"--", "sh", "-c", cmd)
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
