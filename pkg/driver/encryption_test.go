package driver

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
