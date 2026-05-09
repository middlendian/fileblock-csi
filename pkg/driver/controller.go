package driver

import (
	"context"
	"errors"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/image"
	"github.com/middlendian/fileblock-csi/pkg/store"
)

// topologyKeyNode is the segment key reported by the node plugin. The
// controller echoes it back when pinning local-type volumes to the
// provisioner's preferred node.
const topologyKeyNode = "fileblock.csi/node"

const (
	defaultCapacityBytes = 1 << 30 // 1 GiB
)

type imageFactory func(backingStorePath string, exec fbexec.Runner) (image.Manager, error)

type ControllerServer struct {
	csi.UnimplementedControllerServer
	registry  *store.Registry
	exec      fbexec.Runner
	newImages imageFactory
}

// NewControllerServer constructs a ControllerServer. The registry routes
// per-StorageClass backing-store configs to mounted directories; the
// image manager for each volume is built lazily inside CreateVolume from
// the per-store path.
func NewControllerServer(reg *store.Registry, r fbexec.Runner) *ControllerServer {
	return &ControllerServer{
		registry:  reg,
		exec:      r,
		newImages: image.New,
	}
}

func (c *ControllerServer) ControllerGetCapabilities(ctx context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	rpc := func(t csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
		return &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: t},
			},
		}
	}
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			rpc(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
			rpc(csi.ControllerServiceCapability_RPC_EXPAND_VOLUME),
			rpc(csi.ControllerServiceCapability_RPC_LIST_VOLUMES),
		},
	}, nil
}

func (c *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := validateCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, err
	}

	cfg, err := store.ConfigFromParams(req.GetParameters())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	mountedPath, err := c.registry.Get(ctx, cfg)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "mount backing store: %v", err)
	}
	images, err := c.newImages(mountedPath, c.exec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "open backing store: %v", err)
	}

	capacity := defaultCapacityBytes
	if r := req.GetCapacityRange(); r != nil {
		if r.RequiredBytes > 0 {
			capacity = int(r.RequiredBytes)
		} else if r.LimitBytes > 0 {
			capacity = int(r.LimitBytes)
		}
	}

	volumeID, err := volumeIDFromName(cfg, req.GetName())
	if err != nil {
		return nil, err
	}
	meta, err := images.Create(ctx, volumeID, int64(capacity))
	if err != nil {
		var mismatch *image.CapacityMismatchError
		if errors.As(err, &mismatch) {
			return nil, status.Error(codes.AlreadyExists, mismatch.Error())
		}
		return nil, status.Errorf(codes.Internal, "create volume: %v", err)
	}

	vol := &csi.Volume{
		VolumeId:      meta.VolumeID,
		CapacityBytes: meta.CapacityBytes,
		VolumeContext: cfg.ToVolumeContext(),
	}
	vol.AccessibleTopology = topologyForCfg(cfg, req.GetAccessibilityRequirements())
	return &csi.CreateVolumeResponse{Volume: vol}, nil
}

// topologyForCfg returns AccessibleTopology that the external-provisioner
// will honor: empty for nfs (any node), pinned to the provisioner's
// preferred segment for local (matches today's per-node behavior).
func topologyForCfg(cfg store.Config, req *csi.TopologyRequirement) []*csi.Topology {
	if cfg.Type == store.TypeNFS {
		return nil
	}
	if req == nil {
		return nil
	}
	if pref := req.GetPreferred(); len(pref) > 0 {
		return []*csi.Topology{pref[0]}
	}
	if reqs := req.GetRequisite(); len(reqs) > 0 {
		return []*csi.Topology{reqs[0]}
	}
	return nil
}

func (c *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volumeId is required")
	}
	m, err := c.imageManagerForVolumeID(ctx, req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	if err := m.Delete(ctx, req.GetVolumeId()); err != nil {
		return nil, status.Errorf(codes.Internal, "delete volume: %v", err)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

func (c *ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volumeId is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume_capabilities is required")
	}
	m, err := c.imageManagerForVolumeID(ctx, req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	if _, err := m.Get(ctx, req.GetVolumeId()); err != nil {
		return nil, status.Errorf(codes.NotFound, "volume %s: %v", req.GetVolumeId(), err)
	}
	if err := validateCapabilities(req.GetVolumeCapabilities()); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{Message: err.Error()}, nil
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
	}, nil
}

func (c *ControllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	// Pagination is not implemented; reject any starting_token rather than
	// silently returning the full list and confusing the caller.
	if req.GetStartingToken() != "" {
		return nil, status.Error(codes.Aborted, "starting_token is not supported")
	}
	out := []*csi.ListVolumesResponse_Entry{}
	for _, path := range c.registry.MountedPaths() {
		m, err := c.newImages(path, c.exec)
		if err != nil {
			continue
		}
		metas, err := m.List(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list: %v", err)
		}
		for _, meta := range metas {
			out = append(out, &csi.ListVolumesResponse_Entry{
				Volume: &csi.Volume{
					VolumeId:      meta.VolumeID,
					CapacityBytes: meta.CapacityBytes,
				},
			})
		}
	}
	return &csi.ListVolumesResponse{Entries: out}, nil
}

func (c *ControllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volumeId is required")
	}
	r := req.GetCapacityRange()
	if r == nil || r.RequiredBytes <= 0 {
		return nil, status.Error(codes.InvalidArgument, "capacity_range.required_bytes is required")
	}
	m, err := c.imageManagerForVolumeID(ctx, req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	meta, err := m.Resize(ctx, req.GetVolumeId(), r.RequiredBytes)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resize: %v", err)
	}
	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         meta.CapacityBytes,
		NodeExpansionRequired: true, // node runs resize2fs after re-stage
	}, nil
}

// imageManagerForVolumeID resolves a volumeID's home store and returns
// an image.Manager over its mounted path. Returns NotFound if the
// store has not been seen by this controller process (typical after a
// controller-pod restart before CreateVolume re-populates the Registry
// for that storeID — see CHANGELOG migration note).
func (c *ControllerServer) imageManagerForVolumeID(ctx context.Context, volumeID string) (image.Manager, error) {
	storeID, err := parseStoreIDFromVolumeID(volumeID)
	if err != nil {
		return nil, err
	}
	cfg, ok := c.registry.ConfigByStoreID(storeID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "store %s for volume %s is not mounted on this controller; retry after the SC is in use", storeID, volumeID)
	}
	path, err := c.registry.Get(ctx, cfg)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "remount backing store %s: %v", storeID, err)
	}
	return c.newImages(path, c.exec)
}

// volumeIDFromName produces the on-disk volume ID for a given CSI request
// name. The mapping is deterministic on (cfg, name): the same name +
// same backing store always yields the same ID, so a retried CreateVolume
// call lands on the existing image instead of minting a fresh one. The
// "fb-<storeID>-" prefix lets DeleteVolume / Expand resolve the
// volumeID's home store without parameters in the request.
func volumeIDFromName(cfg store.Config, name string) (string, error) {
	if strings.ContainsAny(name, "/\\\x00-") {
		return "", status.Errorf(codes.InvalidArgument, "name %q contains invalid characters", name)
	}
	return "fb-" + cfg.ID() + "-" + name, nil
}

// parseStoreIDFromVolumeID extracts the 12-char storeID from a volumeID
// produced by volumeIDFromName.
func parseStoreIDFromVolumeID(volumeID string) (string, error) {
	const prefix = "fb-"
	if !strings.HasPrefix(volumeID, prefix) {
		return "", status.Errorf(codes.InvalidArgument, "volumeID %q is not in fb-<storeID>-<name> form", volumeID)
	}
	rest := volumeID[len(prefix):]
	if len(rest) < 13 || rest[12] != '-' {
		return "", status.Errorf(codes.InvalidArgument, "volumeID %q has malformed storeID segment", volumeID)
	}
	return rest[:12], nil
}

func validateCapabilities(caps []*csi.VolumeCapability) error {
	if len(caps) == 0 {
		return status.Error(codes.InvalidArgument, "volume_capabilities is required")
	}
	for _, c := range caps {
		if c.GetBlock() != nil {
			return status.Error(codes.InvalidArgument, "block volumes are not supported; mount only")
		}
		mode := c.GetAccessMode().GetMode()
		if mode != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
			return status.Errorf(codes.InvalidArgument,
				"access mode %s not supported; fileblock is RWO only", mode)
		}
		if m := c.GetMount(); m != nil && m.FsType != "" && m.FsType != image.DefaultFs {
			return status.Errorf(codes.InvalidArgument, "fsType %q not supported; only ext4", m.FsType)
		}
	}
	return nil
}
