package driver

import "fmt"

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
