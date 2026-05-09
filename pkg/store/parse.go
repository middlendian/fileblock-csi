package store

import "fmt"

const (
	ParamType            = "backingStore.type"
	ParamNFSServer       = "backingStore.nfs.server"
	ParamNFSPath         = "backingStore.nfs.path"
	ParamNFSMountOptions = "backingStore.nfs.mountOptions"
	ParamLocalPath       = "backingStore.local.path"

	VolumeContextStoreID = "storeID"
)

// ConfigFromParams parses SC.parameters into a Config. Missing or
// malformed required keys produce a non-nil error suitable for surfacing
// as gRPC InvalidArgument by the caller.
func ConfigFromParams(params map[string]string) (Config, error) {
	t := Type(params[ParamType])
	switch t {
	case TypeNFS:
		c := Config{
			Type:            TypeNFS,
			NFSServer:       params[ParamNFSServer],
			NFSPath:         params[ParamNFSPath],
			NFSMountOptions: params[ParamNFSMountOptions],
		}
		if c.NFSServer == "" {
			return Config{}, fmt.Errorf("%s is required when %s=nfs", ParamNFSServer, ParamType)
		}
		if c.NFSPath == "" {
			return Config{}, fmt.Errorf("%s is required when %s=nfs", ParamNFSPath, ParamType)
		}
		return c, nil
	case TypeLocal:
		c := Config{Type: TypeLocal, LocalPath: params[ParamLocalPath]}
		if c.LocalPath == "" {
			return Config{}, fmt.Errorf("%s is required when %s=local", ParamLocalPath, ParamType)
		}
		return c, nil
	case "":
		return Config{}, fmt.Errorf("%s is required (got empty)", ParamType)
	default:
		return Config{}, fmt.Errorf("%s=%q not supported (must be nfs or local)", ParamType, t)
	}
}
