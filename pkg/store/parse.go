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

// ToVolumeContext serializes a Config into the map the controller
// returns from CreateVolume and the node receives in NodeStageVolume.
// It also embeds the storeID for diagnostics.
func (c Config) ToVolumeContext() map[string]string {
	vc := map[string]string{
		ParamType:            string(c.Type),
		VolumeContextStoreID: c.ID(),
	}
	switch c.Type {
	case TypeNFS:
		vc[ParamNFSServer] = c.NFSServer
		vc[ParamNFSPath] = c.NFSPath
		if c.NFSMountOptions != "" {
			vc[ParamNFSMountOptions] = c.NFSMountOptions
		}
	case TypeLocal:
		vc[ParamLocalPath] = c.LocalPath
	}
	return vc
}

// ConfigFromVolumeContext is a thin wrapper that re-parses the same key
// set ConfigFromParams expects.
func ConfigFromVolumeContext(vc map[string]string) (Config, error) {
	return ConfigFromParams(vc)
}

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
