package store

import (
	"fmt"
	"path"
	"strings"
)

const (
	ParamType            = "backingStore.type"
	ParamNFSServer       = "backingStore.nfs.server"
	ParamNFSPath         = "backingStore.nfs.path"
	ParamNFSMountOptions = "backingStore.nfs.mountOptions"
	ParamNFSSubDir       = "backingStore.nfs.subDir"
	ParamLocalPath       = "backingStore.local.path"
	ParamLocalShared     = "backingStore.local.shared"

	VolumeContextStoreID = "storeID"

	// Injected into CreateVolumeRequest.parameters by external-provisioner,
	// but only when it runs with --extra-create-metadata=true. The sidecar
	// does no templating of its own; substitution below is ours.
	paramPVCName      = "csi.storage.k8s.io/pvc/name"
	paramPVCNamespace = "csi.storage.k8s.io/pvc/namespace"
	paramPVName       = "csi.storage.k8s.io/pv/name"

	// Spelled as csi-driver-nfs spells them, so a working subDir value
	// moves across unchanged.
	tmplPVCName      = "${pvc.metadata.name}"
	tmplPVCNamespace = "${pvc.metadata.namespace}"
	tmplPVName       = "${pv.metadata.name}"
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
		sub, err := resolveSubDir(params[ParamNFSSubDir], params)
		if err != nil {
			return Config{}, err
		}
		c.NFSSubDir = sub
		return c, nil
	case TypeLocal:
		c := Config{
			Type:        TypeLocal,
			LocalPath:   params[ParamLocalPath],
			LocalShared: params[ParamLocalShared] == "true",
		}
		if c.LocalPath == "" {
			return Config{}, fmt.Errorf("%s is required when %s=local", ParamLocalPath, ParamType)
		}
		// Accepting and ignoring it would look like it worked.
		if params[ParamNFSSubDir] != "" {
			return Config{}, fmt.Errorf("%s is not supported when %s=local", ParamNFSSubDir, ParamType)
		}
		return c, nil
	case "":
		return Config{}, fmt.Errorf("%s is required (got empty)", ParamType)
	default:
		return Config{}, fmt.Errorf("%s=%q not supported (must be nfs or local)", ParamType, t)
	}
}

// resolveSubDir substitutes the pv/pvc metadata tokens in raw and
// validates the result, returning the cleaned relative path.
//
// An unresolved token is fatal rather than a literal directory name.
// csi-driver-nfs passes the literal through; we do not, because the
// operator asked for per-namespace isolation and a directory called
// "${pvc.metadata.namespace}" silently gives every namespace the same
// one — invisible short of listing the export by hand.
func resolveSubDir(raw string, params map[string]string) (string, error) {
	if raw == "" {
		return "", nil
	}
	sub := raw
	if v := params[paramPVCNamespace]; v != "" {
		sub = strings.ReplaceAll(sub, tmplPVCNamespace, v)
	}
	if v := params[paramPVCName]; v != "" {
		sub = strings.ReplaceAll(sub, tmplPVCName, v)
	}
	if v := params[paramPVName]; v != "" {
		sub = strings.ReplaceAll(sub, tmplPVName, v)
	}

	if i := strings.Index(sub, "${"); i >= 0 {
		tok := sub[i:]
		if j := strings.Index(tok, "}"); j >= 0 {
			tok = tok[:j+1]
		}
		return "", fmt.Errorf("%s contains unresolved template %s: supported tokens are %s, %s and %s, "+
			"and the csi-provisioner sidecar must run with --extra-create-metadata=true for them to resolve",
			ParamNFSSubDir, tok, tmplPVCNamespace, tmplPVCName, tmplPVName)
	}
	if strings.ContainsRune(sub, 0) {
		return "", fmt.Errorf("%s must not contain NUL bytes", ParamNFSSubDir)
	}
	if path.IsAbs(sub) {
		return "", fmt.Errorf("%s must be relative to the export, got %q", ParamNFSSubDir, sub)
	}
	// Checked on the operator's literal input rather than after Clean so
	// the message quotes what they wrote. Clean would fold "a/../b" to
	// "b" and lose the fact that they asked to traverse.
	for _, seg := range strings.Split(sub, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%s must not contain %q path elements, got %q", ParamNFSSubDir, "..", sub)
		}
	}
	clean := path.Clean(sub)
	if clean == "." {
		return "", fmt.Errorf("%s must name a subdirectory of the export, got %q", ParamNFSSubDir, sub)
	}
	return clean, nil
}

// ToVolumeContext serializes a Config into the map the controller
// returns from CreateVolume and the node receives in NodeStageVolume.
// It also embeds the storeID for diagnostics.
func (c Config) ToVolumeContext() map[string]string {
	vc := map[string]string{
		ParamType:            string(c.Type),
		VolumeContextStoreID: c.StoreID(),
	}
	switch c.Type {
	case TypeNFS:
		vc[ParamNFSServer] = c.NFSServer
		vc[ParamNFSPath] = c.NFSPath
		if c.NFSMountOptions != "" {
			vc[ParamNFSMountOptions] = c.NFSMountOptions
		}
		if c.NFSSubDir != "" {
			vc[ParamNFSSubDir] = c.NFSSubDir
		}
	case TypeLocal:
		vc[ParamLocalPath] = c.LocalPath
		if c.LocalShared {
			vc[ParamLocalShared] = "true"
		}
	}
	return vc
}

// ConfigFromVolumeContext is a thin wrapper that re-parses the same key
// set ConfigFromParams expects. The two APIs are kept distinct so callers
// signal intent clearly; the body is shared.
func ConfigFromVolumeContext(vc map[string]string) (Config, error) {
	return ConfigFromParams(vc)
}
