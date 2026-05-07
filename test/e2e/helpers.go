//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// kubectl runs `kubectl <args...>` against the cluster the harness brought up,
// returning stdout and failing the test on non-zero exit. Stderr is folded
// into the failure message so it surfaces in `go test -v` output without a
// second tool call.
func kubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := kubectlRaw(args...)
	if err != nil {
		t.Fatalf("kubectl %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func kubectlRaw(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	if kc := os.Getenv("E2E_KUBECONFIG"); kc != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kc)
	}
	var b strings.Builder
	cmd.Stdout = &b
	cmd.Stderr = &b
	err := cmd.Run()
	return b.String(), err
}

// applyYAML pipes a YAML document to `kubectl apply -f -`.
func applyYAML(t *testing.T, doc string) {
	t.Helper()
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	if kc := os.Getenv("E2E_KUBECONFIG"); kc != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kc)
	}
	cmd.Stdin = strings.NewReader(doc)
	var b strings.Builder
	cmd.Stdout = &b
	cmd.Stderr = &b
	if err := cmd.Run(); err != nil {
		t.Fatalf("kubectl apply failed: %v\n%s\n---doc---\n%s", err, b.String(), doc)
	}
}

func deleteYAML(_ *testing.T, doc string) {
	cmd := exec.Command("kubectl", "delete", "--ignore-not-found", "--wait=false", "-f", "-")
	if kc := os.Getenv("E2E_KUBECONFIG"); kc != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kc)
	}
	cmd.Stdin = strings.NewReader(doc)
	_ = cmd.Run()
}

// makeNamespace creates a per-test namespace and registers a cleanup hook.
// Using a fresh namespace per test means parallel resources can't collide and
// teardown is a single delete.
func makeNamespace(t *testing.T) string {
	t.Helper()
	ns := fmt.Sprintf("fb-e2e-%s", strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")))
	kubectl(t, "create", "namespace", ns)
	t.Cleanup(func() {
		_, _ = kubectlRaw("delete", "namespace", ns, "--wait=false", "--ignore-not-found")
	})
	return ns
}

// eventually polls fn every 2s until it returns nil or timeout fires. The last
// error from fn is included in the failure message so flakes are debuggable.
func eventually(t *testing.T, timeout time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := fn(); err == nil {
			return
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s: %v", timeout, lastErr)
		}
		time.Sleep(2 * time.Second)
	}
}

func waitPodReady(t *testing.T, ns, pod string, timeout time.Duration) {
	t.Helper()
	d := fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))
	kubectl(t, "-n", ns, "wait", "--for=condition=Ready", "pod/"+pod, d)
}

func waitPodGone(t *testing.T, ns, pod string, timeout time.Duration) {
	t.Helper()
	d := fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))
	_, _ = kubectlRaw("-n", ns, "wait", "--for=delete", "pod/"+pod, d)
}

// nodeNames returns the cluster's node names so cross-node tests can pick a
// pair without hardcoding kind's naming scheme.
func nodeNames(t *testing.T) []string {
	t.Helper()
	out := kubectl(t, "get", "nodes",
		"-o", "jsonpath={.items[*].metadata.name}")
	return strings.Fields(out)
}

// pvcManifest renders a PVC manifest with the given name and size. Size is a
// resource.Quantity string ("128Mi", "256Mi", ...).
func pvcManifest(ns, name, size string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: fileblock
  resources:
    requests:
      storage: %s
`, name, ns, size)
}
