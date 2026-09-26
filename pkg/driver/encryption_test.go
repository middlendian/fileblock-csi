package driver

import (
	"bytes"
	"errors"
	osexec "os/exec"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/middlendian/fileblock-csi/pkg/crypt"
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

func TestFormatFromParams(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params map[string]string
		want   crypt.Format
	}{
		{"default", map[string]string{}, crypt.DefaultFormat},
		{"adiantum", map[string]string{ParamCipher: "xchacha12,aes-adiantum-plain64", ParamKeySize: "256"},
			crypt.Format{Cipher: "xchacha12,aes-adiantum-plain64", KeySize: 256}},
		{"cipher and key size", map[string]string{ParamCipher: "serpent-xts-plain64", ParamKeySize: "512"},
			crypt.Format{Cipher: "serpent-xts-plain64", KeySize: 512}},
		{"key size only", map[string]string{ParamKeySize: "256"},
			crypt.Format{Cipher: crypt.DefaultFormat.Cipher, KeySize: 256}},
		{"kernel crypto API spec", map[string]string{ParamCipher: "capi:xts(aes)-plain64", ParamKeySize: "512"},
			crypt.Format{Cipher: "capi:xts(aes)-plain64", KeySize: 512}},
	} {
		got, err := formatFromParams(tc.params, true)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
	}
}

func TestFormatFromParamsRejects(t *testing.T) {
	for _, tc := range []struct {
		name      string
		params    map[string]string
		encrypted bool
	}{
		{"cipher without encrypted", map[string]string{ParamCipher: "aes-xts-plain64"}, false},
		{"key size without encrypted", map[string]string{ParamKeySize: "512"}, false},
		// The key size is never left to cryptsetup's compiled-in default,
		// which could change with the image and make a StorageClass
		// describe different volumes over time.
		{"cipher without key size", map[string]string{ParamCipher: "xchacha12,aes-adiantum-plain64"}, true},
		{"cipher with a space", map[string]string{ParamCipher: "aes-xts-plain64 --foo", ParamKeySize: "512"}, true},
		{"cipher starting with a dash", map[string]string{ParamCipher: "-aes", ParamKeySize: "512"}, true},
		{"uppercase cipher", map[string]string{ParamCipher: "AES-XTS-PLAIN64", ParamKeySize: "512"}, true},
		{"key size not a number", map[string]string{ParamKeySize: "big"}, true},
		{"key size zero", map[string]string{ParamKeySize: "0"}, true},
		{"key size negative", map[string]string{ParamKeySize: "-8"}, true},
		{"key size not bytes", map[string]string{ParamKeySize: "12"}, true},
	} {
		if _, err := formatFromParams(tc.params, tc.encrypted); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}

// Volumes created before the parameters existed have neither key in their
// volume context and must keep formatting exactly as before.
func TestFormatToVolumeContextOmitsDefaults(t *testing.T) {
	vc := map[string]string{}
	formatToVolumeContext(vc, map[string]string{})
	if len(vc) != 0 {
		t.Fatalf("defaults leaked into volume context: %v", vc)
	}
	formatToVolumeContext(vc, map[string]string{ParamCipher: "serpent-xts-plain64", ParamKeySize: "512"})
	if vc[ParamCipher] != "serpent-xts-plain64" || vc[ParamKeySize] != "512" {
		t.Fatalf("volume context = %v", vc)
	}
}
