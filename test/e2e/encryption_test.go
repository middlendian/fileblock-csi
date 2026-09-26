//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestEncryptedVolumeRotation drives the whole encrypted lifecycle through
// the kubelet: the provisioner resolves ${pvc.namespace} in the secret
// namespace, the node formats on first stage, only ciphertext reaches the
// backing store, and a Secret change rotates the key slot on next stage.
func TestEncryptedVolumeRotation(t *testing.T) {
	ns := makeNamespace(t)
	const scName = "fileblock-encrypted"
	key1, key2 := randomKey(t), randomKey(t)

	applyYAML(t, luksSecretYAML(ns, key1, ""))
	applyYAML(t, encryptedSCYAML(t, scName))
	t.Cleanup(func() { _, _ = kubectlRaw("delete", "sc", scName, "--ignore-not-found") })

	applyYAML(t, pvcManifestSC(ns, "vol", "128Mi", scName))
	canary := "fileblock-e2e-canary-" + key1[:16]
	applyYAML(t, podWithScript(ns, "writer", "vol", fmt.Sprintf("set -eu\necho %s > /data/canary\nsync\nsleep 3600\n", canary)))
	waitPodReady(t, ns, "writer", defaultPodReady)
	img := imagePath(t, pvForPVC(t, ns, "vol"))
	kubectl(t, "-n", ns, "delete", "pod", "writer", "--wait=true")
	waitPodGone(t, ns, "writer", 60*time.Second)

	// 1. Only ciphertext on the backing store.
	if out, err := sudo(nil, "cryptsetup", "isLuks", img); err != nil {
		t.Fatalf("%s is not a LUKS volume: %v\n%s", img, err, out)
	}
	switch out, err := sudo(nil, "grep", "-qa", canary, img); {
	case err == nil:
		t.Fatalf("plaintext canary found in %s", img)
	case exitCode(err) == 1:
		// canary absent, as expected.
	default:
		t.Fatalf("grep -qa %s %s: %v\n%s", canary, img, err, out)
	}

	// 2. Rotate: the next stage moves the slot from key1 to key2.
	applyYAML(t, luksSecretYAML(ns, key2, key1))
	applyYAML(t, podWithScript(ns, "reader", "vol", fmt.Sprintf("set -eu\ngrep -qx %s /data/canary\nsleep 3600\n", canary)))
	waitPodReady(t, ns, "reader", defaultPodReady)
	kubectl(t, "-n", ns, "delete", "pod", "reader", "--wait=true")
	waitPodGone(t, ns, "reader", 60*time.Second)

	// 3. The old key no longer opens the header; the new one does.
	if opensWith(t, img, key1) {
		t.Fatal("old key still opens the volume after rotation")
	}
	if !opensWith(t, img, key2) {
		t.Fatal("new key does not open the volume after rotation")
	}
}

func randomKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func luksSecretYAML(ns, key, previous string) string {
	prev := ""
	if previous != "" {
		prev = fmt.Sprintf("  previousKey: %q\n", previous)
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: fileblock-luks
  namespace: %s
stringData:
  key: %q
%s`, ns, key, prev)
}

// encryptedSCYAML clones the default `fileblock` StorageClass's backing
// store, so the test runs against whichever store the variant set up.
func encryptedSCYAML(t *testing.T, name string) string {
	t.Helper()
	raw := kubectl(t, "get", "sc", "fileblock", "-o", "jsonpath={.parameters}")
	params := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &params); err != nil {
		t.Fatalf("parse fileblock SC parameters %q: %v", raw, err)
	}
	params["encrypted"] = "true"
	params["csi.storage.k8s.io/node-stage-secret-name"] = "fileblock-luks"
	params["csi.storage.k8s.io/node-stage-secret-namespace"] = "${pvc.namespace}"
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %q\n", k, params[k])
	}
	return fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
provisioner: fileblock.csi
parameters:
%sreclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
`, name, b.String())
}

// imagePath is where the runner sees a volume's .img: the NFS export
// itself in nfs mode, the shared host directory in local mode.
func imagePath(t *testing.T, handle string) string {
	t.Helper()
	dir := os.Getenv("E2E_BACKING_HOST")
	if os.Getenv("E2E_BACKING_KIND") == "nfs" {
		dir = os.Getenv("NFS_EXPORT")
	}
	if dir == "" {
		t.Skip("backing store path not exported by hack/e2e.sh")
	}
	return filepath.Join(dir, handle+".img")
}

// sudo runs a command as root on the runner: .img files are 0600 root. It
// returns combined stdout+stderr so callers can fold it into failure
// messages instead of guessing what went wrong from the exit code alone.
func sudo(stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.Command("sudo", append([]string{name}, args...)...)
	if stdin != nil {
		cmd.Stdin = strings.NewReader(string(stdin))
	}
	return cmd.CombinedOutput()
}

// exitCode returns the process exit code for an error from sudo, or -1 if
// err isn't an *exec.ExitError (e.g. sudo itself failed to start).
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func opensWith(t *testing.T, img, key string) bool {
	t.Helper()
	out, err := sudo([]byte(key), "cryptsetup", "open", "--test-passphrase", "--key-file=-", img)
	if err == nil {
		return true
	}
	if exitCode(err) == 2 {
		return false
	}
	t.Fatalf("cryptsetup --test-passphrase %s: %v\n%s", img, err, out)
	return false
}
