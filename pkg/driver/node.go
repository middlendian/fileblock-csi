package driver

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/image"
	"github.com/middlendian/fileblock-csi/pkg/loop"
	"github.com/middlendian/fileblock-csi/pkg/mount"
	"github.com/middlendian/fileblock-csi/pkg/store"
)

// NodeServer implements the CSI NodeServer for fileblock. The node owns:
//   - the loop device for each staged volume
//   - the staging mount and the bind mount into the pod
//
// Cross-node mutual exclusion is the kubelet's job: fileblock advertises only
// SINGLE_NODE_WRITER, so the CSI attach/detach path serializes Stage/Unstage
// per volume across the cluster.
//
// State is persisted to a JSON file so a plugin restart can reconcile.
type NodeServer struct {
	csi.UnimplementedNodeServer

	nodeID   string
	exec     fbexec.Runner
	mnt      *mount.Mounter
	losetup  *loop.Losetup
	state    *loop.State
	log      *slog.Logger
	registry *store.Registry

	// One mutex per volumeID protects Stage/Unstage from racing each other
	// inside this process.
	mu       sync.Mutex
	volMutex map[string]*sync.Mutex
}

// NewNodeServer constructs a NodeServer. The registry is used to resolve and
// mount backing stores on demand when a volume is staged.
func NewNodeServer(nodeID string, exec fbexec.Runner, mnt *mount.Mounter, ls *loop.Losetup, st *loop.State, log *slog.Logger, reg *store.Registry) *NodeServer {
	return &NodeServer{
		nodeID:   nodeID,
		exec:     exec,
		mnt:      mnt,
		losetup:  ls,
		state:    st,
		log:      log,
		registry: reg,
		volMutex: map[string]*sync.Mutex{},
	}
}

func (n *NodeServer) NodeGetCapabilities(ctx context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	rpc := func(t csi.NodeServiceCapability_RPC_Type) *csi.NodeServiceCapability {
		return &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{Type: t},
			},
		}
	}
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			rpc(csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME),
			rpc(csi.NodeServiceCapability_RPC_GET_VOLUME_STATS),
			rpc(csi.NodeServiceCapability_RPC_EXPAND_VOLUME),
		},
	}, nil
}

func (n *NodeServer) NodeGetInfo(ctx context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: n.nodeID,
		AccessibleTopology: &csi.Topology{
			Segments: map[string]string{topologyKeyNode: n.nodeID},
		},
		// One loop device per volume; pick a generous cap. Operators can
		// raise this with modprobe loop max_loop=N.
		MaxVolumesPerNode: 64,
	}, nil
}

func (n *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	stagePath := req.GetStagingTargetPath()
	if volumeID == "" || stagePath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id and staging_target_path are required")
	}
	cfg, err := store.ConfigFromVolumeContext(req.GetVolumeContext())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	backing, err := n.registry.Get(ctx, cfg)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "mount backing store: %v", err)
	}
	if err := validateCapability(req.GetVolumeCapability()); err != nil {
		return nil, err
	}

	unlock := n.lockVolume(volumeID)
	defer unlock()

	// Idempotency: if we already staged this volume to this path, return ok.
	if existing, ok := n.state.Get(volumeID); ok && existing.StagePath == stagePath {
		mounted, err := n.mnt.IsMountPoint(ctx, stagePath)
		if err == nil && mounted {
			return &csi.NodeStageVolumeResponse{}, nil
		}
	}

	images, err := image.New(backing, n.exec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "open backing store: %v", err)
	}
	imgPath := images.ImagePath(volumeID)
	if _, err := os.Stat(imgPath); err != nil {
		return nil, status.Errorf(codes.NotFound, "image %s: %v", imgPath, err)
	}

	// 1. Attach a loop device.
	dev, err := n.losetup.Attach(ctx, imgPath)
	if err != nil {
		if errors.Is(err, loop.ErrPoolExhausted) {
			return nil, status.Errorf(codes.ResourceExhausted, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "losetup: %v", err)
	}
	detachOnFail := func() { _ = n.losetup.Detach(ctx, dev) }

	// 2. e2fsck always (-p is a no-op on clean fs).
	if err := image.Fsck(ctx, n.exec, dev); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "fsck: %v", err)
	}

	// 3. If the image was expanded since the last stage, grow the fs now.
	//    resize2fs is a no-op when the fs already fills the device.
	if err := n.losetup.SetCapacity(ctx, dev); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "set capacity: %v", err)
	}
	if err := image.Resize2fs(ctx, n.exec, dev); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "resize2fs: %v", err)
	}

	// 4. Mount.
	if err := os.MkdirAll(stagePath, 0o755); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "mkdir stage: %v", err)
	}
	mountOpts := mountOptionsFromCap(req.GetVolumeCapability())
	if err := n.mnt.Mount(ctx, dev, stagePath, image.DefaultFs, mountOpts); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "mount: %v", err)
	}

	// 5. Persist mapping.
	if err := n.state.Put(loop.Mapping{
		VolumeID:  volumeID,
		LoopDev:   dev,
		ImagePath: imgPath,
		StagePath: stagePath,
	}); err != nil {
		_ = n.mnt.Unmount(ctx, stagePath)
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "persist state: %v", err)
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

func (n *NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	stagePath := req.GetStagingTargetPath()
	if volumeID == "" || stagePath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id and staging_target_path are required")
	}
	unlock := n.lockVolume(volumeID)
	defer unlock()

	if err := n.mnt.Unmount(ctx, stagePath); err != nil {
		return nil, status.Errorf(codes.Internal, "umount stage: %v", err)
	}
	_ = os.Remove(stagePath)

	if m, ok := n.state.Get(volumeID); ok {
		if err := n.losetup.Detach(ctx, m.LoopDev); err != nil {
			return nil, status.Errorf(codes.Internal, "losetup --detach: %v", err)
		}
		_ = n.state.Delete(volumeID)
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (n *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	stagePath := req.GetStagingTargetPath()
	target := req.GetTargetPath()
	if req.GetVolumeId() == "" || stagePath == "" || target == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id, staging_target_path, target_path are required")
	}
	if err := validateCapability(req.GetVolumeCapability()); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, status.Errorf(codes.Internal, "mkdir target: %v", err)
	}
	if mounted, err := n.mnt.IsMountPoint(ctx, target); err == nil && mounted {
		return &csi.NodePublishVolumeResponse{}, nil
	}
	if err := n.mnt.BindMount(ctx, stagePath, target, req.GetReadonly()); err != nil {
		return nil, status.Errorf(codes.Internal, "bind mount: %v", err)
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func (n *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	target := req.GetTargetPath()
	if req.GetVolumeId() == "" || target == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id and target_path are required")
	}
	if err := n.mnt.Unmount(ctx, target); err != nil {
		return nil, status.Errorf(codes.Internal, "umount target: %v", err)
	}
	_ = os.Remove(target)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (n *NodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	volumeID := req.GetVolumeId()
	path := req.GetVolumePath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if path == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_path is required")
	}
	if _, ok := n.state.Get(volumeID); !ok {
		return nil, status.Errorf(codes.NotFound, "volume %s not staged on this node", volumeID)
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, status.Errorf(codes.NotFound, "volume_path %s does not exist", path)
		}
		return nil, status.Errorf(codes.Internal, "stat %s: %v", path, err)
	}
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return nil, status.Errorf(codes.Internal, "statfs %s: %v", path, err)
	}
	bs := st.Bsize
	total := int64(st.Blocks) * bs
	free := int64(st.Bavail) * bs
	used := total - free
	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{Unit: csi.VolumeUsage_BYTES, Total: total, Available: free, Used: used},
			{Unit: csi.VolumeUsage_INODES, Total: int64(st.Files), Available: int64(st.Ffree), Used: int64(st.Files - st.Ffree)},
		},
	}, nil
}

func (n *NodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if req.GetVolumePath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_path is required")
	}
	m, ok := n.state.Get(volumeID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "volume %s not staged on this node", volumeID)
	}
	if err := n.losetup.SetCapacity(ctx, m.LoopDev); err != nil {
		return nil, status.Errorf(codes.Internal, "set capacity: %v", err)
	}
	if err := image.Resize2fs(ctx, n.exec, m.LoopDev); err != nil {
		return nil, status.Errorf(codes.Internal, "resize2fs: %v", err)
	}
	return &csi.NodeExpandVolumeResponse{
		CapacityBytes: req.GetCapacityRange().GetRequiredBytes(),
	}, nil
}

func (n *NodeServer) lockVolume(volumeID string) func() {
	n.mu.Lock()
	m, ok := n.volMutex[volumeID]
	if !ok {
		m = &sync.Mutex{}
		n.volMutex[volumeID] = m
	}
	n.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func mountOptionsFromCap(cap *csi.VolumeCapability) []string {
	if cap == nil {
		return nil
	}
	if m := cap.GetMount(); m != nil {
		return append([]string(nil), m.MountFlags...)
	}
	return nil
}

func validateCapability(cap *csi.VolumeCapability) error {
	if cap == nil {
		return status.Error(codes.InvalidArgument, "volume_capability is required")
	}
	if cap.GetBlock() != nil {
		return status.Error(codes.InvalidArgument, "block volumes are not supported")
	}
	mode := cap.GetAccessMode().GetMode()
	if mode != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
		return status.Errorf(codes.InvalidArgument, "access mode %s not supported", mode)
	}
	if m := cap.GetMount(); m != nil && m.FsType != "" && m.FsType != image.DefaultFs {
		return status.Errorf(codes.InvalidArgument, "fsType %q not supported", m.FsType)
	}
	return nil
}
