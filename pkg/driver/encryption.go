package driver

import (
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/middlendian/fileblock-csi/pkg/crypt"
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
	default:
		return status.Errorf(codes.Internal, "luks (needs cryptsetup and the dm_crypt kernel module): %v", err)
	}
}
