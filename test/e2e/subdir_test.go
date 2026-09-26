//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestNFSSubDirPerNamespace provisions a StorageClass whose subDir carries
// the ${pvc.namespace} token and asserts the .img lands under the
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

	// 2. This volume's .img is not at the export root — that separation
	// is the whole point. Scoped to this volume's handle rather than
	// asserting the export root holds no .img at all: every other test
	// in this suite provisions through the `fileblock` StorageClass
	// whose backingStore.nfs.path IS the export root, namespace teardown
	// uses --wait=false, and Go runs test files in alphabetical order —
	// so a prior test's PV reclamation can still be in flight here,
	// leaving a stray .img at the root that has nothing to do with this
	// test. A per-handle check still fails if Registry.Get ignored the
	// subDir and wrote this volume at the root, but is immune to that
	// unrelated residue.
	if _, err := os.Stat(filepath.Join(export, handle+".img")); err == nil {
		t.Errorf("found %s.img at the export root; subDir did not take effect", handle)
	}

	// 3. Belt-and-braces: a literal-token directory would mean the
	// provisioner is missing --extra-create-metadata=true. In practice
	// this can never trigger — an unresolved token is fatal at
	// CreateVolume (see store.resolveSubDir), so the PVC never binds and
	// waitPodReady above fails long before this Stat runs. The real
	// detector for a missing --extra-create-metadata=true is that
	// waitPodReady timeout, not this assertion; this stays only in case
	// the design ever changes to tolerate unresolved tokens.
	if _, err := os.Stat(filepath.Join(export, "${pvc.namespace}")); err == nil {
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
  backingStore.nfs.subDir: ${pvc.namespace}/fileblock
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
