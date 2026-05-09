// Package deploy holds tests against the static kustomize manifests
// shipped under deploy/kustomize/. They guard against silent regressions
// in fields that are easy to drop in a refactor but load-bearing at
// deploy time.
package deploy

import (
	"os"
	"strings"
	"testing"
)

// TestControllerSecurityContext asserts the controller container runs
// with privileged: true, SYS_ADMIN, and drop ALL — the posture
// csi-driver-nfs uses. This was added after a real regression: the
// initial v0.3.0 base manifest set SYS_ADMIN-only on the controller,
// and NFSv3 mounts hung in production because the LSM rejected the
// privileged-port bind that NLM needs. e2e CI didn't catch it because
// the matrix uses nfsvers=4.1, which doesn't need NLM.
func TestControllerSecurityContext(t *testing.T) {
	requireFileblockSecurityContext(t, "kustomize/base/controller-deployment.yaml")
}

// TestNodeSecurityContext asserts the same for the node DaemonSet.
// The node has always been privileged for losetup; the drop ALL is the
// addition.
func TestNodeSecurityContext(t *testing.T) {
	requireFileblockSecurityContext(t, "kustomize/base/node-daemonset.yaml")
}

// TestSidecarTimeouts asserts the csi-provisioner and csi-resizer
// sidecars on the controller pod set --timeout=1200s. This was added
// after a real regression: the default 15s gRPC timeout on the
// provisioner sidecar killed mount.nfs mid-call on slow first-connect
// NFSv3 mounts, surfacing as 'exit -1: signal: killed' in CreateVolume
// errors. csi-driver-nfs uses the same 1200s timeout for the same
// reason.
func TestSidecarTimeouts(t *testing.T) {
	data, err := os.ReadFile("kustomize/base/controller-deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, sidecar := range []string{"csi-provisioner", "csi-resizer"} {
		idx := strings.Index(text, "name: "+sidecar)
		if idx == -1 {
			t.Fatalf("controller-deployment.yaml: missing sidecar %q", sidecar)
		}
		// Search forward from the sidecar's name to its args block;
		// the next container's `name:` (or end of file) bounds it.
		end := strings.Index(text[idx+1:], "- name:")
		if end == -1 {
			end = len(text) - idx - 1
		}
		block := text[idx : idx+1+end]
		if !strings.Contains(block, "--timeout=1200s") {
			t.Errorf("controller-deployment.yaml: %s sidecar must include --timeout=1200s arg", sidecar)
		}
	}
}

// requireFileblockSecurityContext reads a base manifest and asserts the
// canonical security-context block — privileged, SYS_ADMIN, drop ALL —
// appears in order. The match is substring-based rather than
// YAML-structural so the test has no extra deps; the manifests are
// hand-maintained in this repo so the format is stable.
func requireFileblockSecurityContext(t *testing.T, relPath string) {
	t.Helper()
	data, err := os.ReadFile(relPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	pos := 0
	for _, want := range []string{
		"privileged: true",
		"capabilities:",
		"add: [SYS_ADMIN]",
		"drop: [ALL]",
	} {
		idx := strings.Index(text[pos:], want)
		if idx == -1 {
			t.Fatalf("%s: missing %q after byte %d (looking for the privileged + SYS_ADMIN + drop ALL block)", relPath, want, pos)
		}
		pos += idx + len(want)
	}
}
