package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// DriverName is the CSI driver name. Must match StorageClass.provisioner and
// the CSIDriver object name.
const DriverName = "fileblock.csi"

// Version is reported by GetPluginInfo. Bump on protocol-affecting changes.
const Version = "0.1.0"

type IdentityServer struct {
	csi.UnimplementedIdentityServer
	hasController bool
}

func NewIdentityServer(hasController bool) *IdentityServer {
	return &IdentityServer{hasController: hasController}
}

func (s *IdentityServer) GetPluginInfo(ctx context.Context, _ *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          DriverName,
		VendorVersion: Version,
	}, nil
}

func (s *IdentityServer) GetPluginCapabilities(ctx context.Context, _ *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	caps := []*csi.PluginCapability{
		{
			Type: &csi.PluginCapability_Service_{
				Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_CONTROLLER_SERVICE},
			},
		},
		{
			Type: &csi.PluginCapability_Service_{
				Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS},
			},
		},
		{
			Type: &csi.PluginCapability_VolumeExpansion_{
				VolumeExpansion: &csi.PluginCapability_VolumeExpansion{Type: csi.PluginCapability_VolumeExpansion_OFFLINE},
			},
		},
	}
	return &csi.GetPluginCapabilitiesResponse{Capabilities: caps}, nil
}

func (s *IdentityServer) Probe(ctx context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}
