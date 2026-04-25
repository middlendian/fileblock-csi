package driver

import (
	"context"
	"errors"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/middlendian/fileblock-csi/pkg/image"
)

// ParamBackingStorePath is the StorageClass parameter that names the
// host-readable directory where .img files live. The same key is also
// returned in the volume context so the node plugin knows where to look.
const ParamBackingStorePath = "backingStorePath"

// TopologyKeyNode is the default topology key. With this key each node
// reports its own NodeID as the segment value, so a PV is pinned to the
// node it was first scheduled on. Operators sharing a backing store across
// multiple nodes (NFS, SMB, FUSE) override this with a per-store key — for
// example fileblock.csi/backing-store — so all nodes that mount the same
// store advertise the same segment and the PV can land on any of them.
const TopologyKeyNode = "fileblock.csi/node"

const (
	defaultCapacityBytes = 1 << 30 // 1 GiB
)

type ControllerServer struct {
	csi.UnimplementedControllerServer
	images           image.Manager
	backingStorePath string
	// topologyKey is accepted for symmetry with the node plugin so the two
	// can be configured together; the controller itself only echoes whatever
	// key the provisioner passes in AccessibilityRequirements.
	topologyKey string
}

// NewControllerServer constructs a ControllerServer. topologyKey may be empty;
// it defaults to TopologyKeyNode (per-node pin).
func NewControllerServer(images image.Manager, backingStorePath, topologyKey string) *ControllerServer {
	if topologyKey == "" {
		topologyKey = TopologyKeyNode
	}
	return &ControllerServer{
		images:           images,
		backingStorePath: backingStorePath,
		topologyKey:      topologyKey,
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

	capacity := defaultCapacityBytes
	if r := req.GetCapacityRange(); r != nil {
		if r.RequiredBytes > 0 {
			capacity = int(r.RequiredBytes)
		} else if r.LimitBytes > 0 {
			capacity = int(r.LimitBytes)
		}
	}

	volumeID := "fb-" + uuid.NewString()
	meta, err := c.images.Create(ctx, volumeID, int64(capacity))
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
		VolumeContext: map[string]string{
			ParamBackingStorePath: c.backingStorePath,
		},
	}
	// Honor the provisioner's topology preference. The pin is to a single
	// segment (key=value), not necessarily a single node: when every node
	// that mounts the same backing store advertises the same segment, the
	// PV is schedulable on any of them. ext4 still has no distributed
	// locking, so the OS-level flock on the .img file remains the
	// per-stage cross-node mutual-exclusion primitive (see pkg/flock).
	if reqTop := req.GetAccessibilityRequirements(); reqTop != nil {
		if pref := reqTop.GetPreferred(); len(pref) > 0 {
			vol.AccessibleTopology = []*csi.Topology{pref[0]}
		} else if reqs := reqTop.GetRequisite(); len(reqs) > 0 {
			vol.AccessibleTopology = []*csi.Topology{reqs[0]}
		}
	}
	return &csi.CreateVolumeResponse{Volume: vol}, nil
}

func (c *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volumeId is required")
	}
	if err := c.images.Delete(ctx, req.GetVolumeId()); err != nil {
		return nil, status.Errorf(codes.Internal, "delete volume: %v", err)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

func (c *ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volumeId is required")
	}
	if _, err := c.images.Get(ctx, req.GetVolumeId()); err != nil {
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

func (c *ControllerServer) ListVolumes(ctx context.Context, _ *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	metas, err := c.images.List(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list: %v", err)
	}
	out := make([]*csi.ListVolumesResponse_Entry, 0, len(metas))
	for _, m := range metas {
		out = append(out, &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{
				VolumeId:      m.VolumeID,
				CapacityBytes: m.CapacityBytes,
				VolumeContext: map[string]string{ParamBackingStorePath: c.backingStorePath},
			},
		})
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
	meta, err := c.images.Resize(ctx, req.GetVolumeId(), r.RequiredBytes)
	if err != nil {
		var inUse *image.VolumeInUseError
		if errors.As(err, &inUse) {
			return nil, status.Errorf(codes.FailedPrecondition, "volume in use: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "resize: %v", err)
	}
	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         meta.CapacityBytes,
		NodeExpansionRequired: true, // node runs resize2fs after re-stage
	}, nil
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
