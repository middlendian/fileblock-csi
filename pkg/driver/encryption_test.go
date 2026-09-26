package driver

import (
	"bytes"
	"errors"
	osexec "os/exec"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
)

var testKey = strings.Repeat("k", 44)

func TestKeysFromSecretsMissing(t *testing.T) {
	for _, s := range []map[string]string{nil, {}, {"key": ""}, {"previousKey": testKey}} {
		_, err := keysFromSecrets(s)
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("%v: got %v, want FailedPrecondition", s, err)
		}
		if !strings.Contains(err.Error(), "csi.storage.k8s.io/node-stage-secret-name") {
			t.Fatalf("error does not name the SC parameter: %v", err)
		}
	}
}

func TestKeysFromSecretsTooShort(t *testing.T) {
	_, err := keysFromSecrets(map[string]string{"key": "short"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
	if strings.Contains(err.Error(), "short") {
		t.Fatalf("error leaks the key: %v", err)
	}
}

// Review Focus 1: kubectl create secret --from-file keeps the trailing
// newline; the key is used exactly as stored.
func TestKeysFromSecretsPreservesBytes(t *testing.T) {
	k, err := keysFromSecrets(map[string]string{"key": testKey + "\n", "previousKey": " old \n"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k.Current, []byte(testKey+"\n")) || !bytes.Equal(k.Previous, []byte(" old \n")) {
		t.Fatalf("keys altered: %q %q", k.Current, k.Previous)
	}
}

// Review finding 6: a missing cryptsetup binary is the one Internal
// failure worth naming the kernel module for; every other Internal
// failure gets a neutral message instead of blaming a healthy install.
func TestCryptStatusMissingBinaryKeepsHint(t *testing.T) {
	err := &fbexec.Error{Cmd: "cryptsetup", ExitCode: -1, Err: &osexec.Error{Name: "cryptsetup", Err: osexec.ErrNotFound}}
	got := cryptStatus(err)
	if status.Code(got) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(got))
	}
	if !strings.Contains(got.Error(), "dm_crypt kernel module") {
		t.Fatalf("expected the missing-binary hint, got: %v", got)
	}
}

func TestCryptStatusOtherInternalIsNeutral(t *testing.T) {
	err := &fbexec.Error{Cmd: "cryptsetup", ExitCode: 1, Err: errors.New("boom")}
	got := cryptStatus(err)
	if status.Code(got) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(got))
	}
	if strings.Contains(got.Error(), "dm_crypt kernel module") {
		t.Fatalf("expected a neutral message for a non-missing-binary failure, got: %v", got)
	}
}
