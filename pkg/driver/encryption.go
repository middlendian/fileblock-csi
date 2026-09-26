package driver

import (
	"errors"
	"fmt"
	osexec "os/exec"
	"regexp"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/middlendian/fileblock-csi/pkg/crypt"
	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
)

// ParamEncrypted opts a StorageClass into LUKS2 encryption. The controller
// copies it into volume context, which is how the node learns of it.
const ParamEncrypted = "encrypted"

// minEncryptedCapacity leaves room for the 16 MiB LUKS2 header plus a
// usable ext4.
const minEncryptedCapacity = 32 << 20

func encryptedFromParams(params map[string]string) (bool, error) {
	switch v := params[ParamEncrypted]; v {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, fmt.Errorf("%s=%q: must be \"true\" or \"false\"", ParamEncrypted, v)
	}
}

// ParamCipher and ParamKeySize choose what luksFormat is given on first
// stage; both are optional and copied into volume context only when set.
const (
	ParamCipher  = "encryption.cipher"
	ParamKeySize = "encryption.keySize"
)

// cipherPattern admits every cryptsetup cipher spec, including kernel
// crypto API ones like capi:xts(aes)-plain64, while keeping the value a
// single argument that can't be mistaken for a flag.
var cipherPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9:,()_-]*$`)

// formatFromParams reads the cipher parameters from StorageClass
// parameters or volume context. Absent parameters mean DefaultFormat; a
// cipher without a key size leaves the key size to cryptsetup.
func formatFromParams(params map[string]string, encrypted bool) (crypt.Format, error) {
	cipher, keySize := params[ParamCipher], params[ParamKeySize]
	if cipher == "" && keySize == "" {
		return crypt.DefaultFormat, nil
	}
	if !encrypted {
		return crypt.Format{}, fmt.Errorf("%s and %s need %s=\"true\"", ParamCipher, ParamKeySize, ParamEncrypted)
	}
	f := crypt.DefaultFormat
	if cipher != "" {
		if !cipherPattern.MatchString(cipher) {
			return crypt.Format{}, fmt.Errorf("%s=%q is not a cryptsetup cipher spec", ParamCipher, cipher)
		}
		f = crypt.Format{Cipher: cipher}
	}
	if keySize != "" {
		n, err := strconv.Atoi(keySize)
		if err != nil || n <= 0 || n%8 != 0 {
			return crypt.Format{}, fmt.Errorf("%s=%q: must be a positive number of bits divisible by 8", ParamKeySize, keySize)
		}
		f.KeySize = n
	}
	return f, nil
}

// formatToVolumeContext copies only the cipher parameters that are set, so
// volumes on the default format carry nothing new.
func formatToVolumeContext(vc, params map[string]string) {
	for _, k := range []string{ParamCipher, ParamKeySize} {
		if v := params[k]; v != "" {
			vc[k] = v
		}
	}
}

const (
	secretKey         = "key"
	secretPreviousKey = "previousKey"
)

// keysFromSecrets reads the node-stage secret. Values are used exactly as
// stored: the README's recovery recipe feeds the same bytes to cryptsetup.
func keysFromSecrets(s map[string]string) (crypt.Keys, error) {
	cur := s[secretKey]
	if cur == "" {
		return crypt.Keys{}, status.Errorf(codes.FailedPrecondition,
			"encrypted volume needs a %q entry in its node-stage secret; set "+
				"csi.storage.k8s.io/node-stage-secret-name and "+
				"csi.storage.k8s.io/node-stage-secret-namespace on the StorageClass", secretKey)
	}
	if len(cur) < crypt.MinKeyLen {
		return crypt.Keys{}, status.Errorf(codes.InvalidArgument,
			"node-stage secret %q is %d bytes; at least %d are required", secretKey, len(cur), crypt.MinKeyLen)
	}
	return crypt.Keys{Current: []byte(cur), Previous: []byte(s[secretPreviousKey])}, nil
}

func cryptStatus(err error) error {
	switch {
	case errors.Is(err, crypt.ErrWrongKey):
		return status.Errorf(codes.PermissionDenied, "%v", err)
	case errors.Is(err, crypt.ErrNotBlank):
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	case missingCryptsetupBinary(err):
		return status.Errorf(codes.Internal, "luks (needs cryptsetup and the dm_crypt kernel module): %v", err)
	default:
		return status.Errorf(codes.Internal, "luks: %v", err)
	}
}

// missingCryptsetupBinary reports whether err is exec's own "no such
// file" failure to start cryptsetup at all, as opposed to cryptsetup
// running and failing. Only that case justifies naming the kernel
// module; every other Internal failure gets a neutral message.
func missingCryptsetupBinary(err error) bool {
	var e *fbexec.Error
	if !errors.As(err, &e) || e.ExitCode != -1 || e.Err == nil {
		return false
	}
	return errors.Is(e.Err, osexec.ErrNotFound)
}
