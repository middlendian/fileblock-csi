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

// TestHostNetwork asserts both pods run with hostNetwork: true and
// dnsPolicy: ClusterFirstWithHostNet. Source IP of NFS mounts must be
// the host's so NFS server export ACLs (which typically allow the
// cluster's host network, not the pod CIDR) accept it. Matches
// csi-driver-nfs's controller and node pods. Real production failure
// in v0.3.2: NAS-side ACL rejected pod-CIDR clients with the generic
// "Protocol not supported" error, leaving CreateVolume permanently
// failing.
func TestHostNetwork(t *testing.T) {
	for _, p := range []string{
		"kustomize/base/controller-deployment.yaml",
		"kustomize/base/node-daemonset.yaml",
	} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if !strings.Contains(text, "hostNetwork: true") {
			t.Errorf("%s: missing hostNetwork: true", p)
		}
		if !strings.Contains(text, "dnsPolicy: ClusterFirstWithHostNet") {
			t.Errorf("%s: missing dnsPolicy: ClusterFirstWithHostNet (required for DNS resolution under hostNetwork)", p)
		}
	}
}

// TestLivenessProbePortsAreDistinct asserts the controller and node
// liveness-probe sidecars bind to distinct localhost ports. With
// hostNetwork: true, the default 0.0.0.0:9808 collides between the
// controller pod and the node-DaemonSet pod scheduled on the same
// host (kubelet keeps one running, the other crash-loops). Distinct
// localhost: ports per pod-role avoid the collision and don't expose
// the probe outside the pod. Matches csi-driver-nfs (29652/29653).
func TestLivenessProbePortsAreDistinct(t *testing.T) {
	ctlPort := requireLivenessHTTPEndpointPort(t, "kustomize/base/controller-deployment.yaml")
	nodePort := requireLivenessHTTPEndpointPort(t, "kustomize/base/node-daemonset.yaml")
	if ctlPort == "" || nodePort == "" {
		// requireLivenessHTTPEndpointPort already failed the test.
		return
	}
	if ctlPort == nodePort {
		t.Errorf("controller and node liveness-probe ports must differ; both set to %s", ctlPort)
	}
}

// requireLivenessHTTPEndpointPort returns the port from a
// `--http-endpoint=localhost:PORT` arg in the named manifest's
// liveness-probe sidecar. Fails the test if absent.
func requireLivenessHTTPEndpointPort(t *testing.T, relPath string) string {
	t.Helper()
	data, err := os.ReadFile(relPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	idx := strings.Index(text, "name: liveness-probe")
	if idx == -1 {
		t.Fatalf("%s: missing liveness-probe sidecar", relPath)
	}
	end := strings.Index(text[idx+1:], "- name:")
	if end == -1 {
		end = len(text) - idx - 1
	}
	block := text[idx : idx+1+end]
	const prefix = "--http-endpoint=localhost:"
	pos := strings.Index(block, prefix)
	if pos == -1 {
		t.Errorf("%s: liveness-probe must include --http-endpoint=localhost:PORT to avoid hostNetwork port collision", relPath)
		return ""
	}
	rest := block[pos+len(prefix):]
	end2 := strings.IndexAny(rest, " \n")
	if end2 == -1 {
		end2 = len(rest)
	}
	return rest[:end2]
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
