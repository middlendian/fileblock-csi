package driver

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/flock"
	"github.com/middlendian/fileblock-csi/pkg/image"
	"github.com/middlendian/fileblock-csi/pkg/loop"
	"github.com/middlendian/fileblock-csi/pkg/mount"
)

// NodeServer implements the CSI NodeServer for fileblock. The node owns:
//   - the loop device for each staged volume
//   - the OS-level flock on the .img file
//   - the staging mount and the bind mount into the pod
//
// State is persisted to a JSON file so a plugin restart can reconcile.
type NodeServer struct {
	csi.UnimplementedNodeServer

	nodeID  string
	exec    fbexec.Runner
	mnt     *mount.Mounter
	losetup *loop.Losetup
	state   *loop.State
	log     *slog.Logger

	// topologyKey/topologyValue are the single segment NodeGetInfo reports.
	// When all nodes that mount the same backing store share these values,
	// any of them is a valid landing zone for a PV from that store.
	topologyKey   string
	topologyValue string

	// One mutex per volumeID protects Stage/Unstage from racing each other.
	mu       sync.Mutex
	volMutex map[string]*sync.Mutex
	// fdByVolume holds the open flock fd for each currently-staged volume.
	// We can't put a *flock.Handle in the on-disk state, so it's tracked
	// only in memory; on plugin restart the kernel-side flock is released
	// and Reconcile.go re-detaches the orphan loop.
	fdByVolume map[string]*flock.Handle
}

// NewNodeServer constructs a NodeServer. topologyKey/topologyValue may be
// empty, in which case they default to TopologyKeyNode and nodeID — that is,
// the legacy per-node pin.
func NewNodeServer(nodeID string, exec fbexec.Runner, mnt *mount.Mounter, ls *loop.Losetup, st *loop.State, log *slog.Logger, topologyKey, topologyValue string) *NodeServer {
	if topologyKey == "" {
		topologyKey = TopologyKeyNode
	}
	if topologyValue == "" {
		topologyValue = nodeID
	}
	return &NodeServer{
		nodeID:        nodeID,
		exec:          exec,
		mnt:           mnt,
		losetup:       ls,
		state:         st,
		log:           log,
		topologyKey:   topologyKey,
		topologyValue: topologyValue,
		volMutex:      map[string]*sync.Mutex{},
		fdByVolume:    map[string]*flock.Handle{},
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
			Segments: map[string]string{n.topologyKey: n.topologyValue},
		},
		// One loop device per volume; pick a generous cap. Operators can
		// raise this with modprobe loop max_loop=N.
		MaxVolumesPerNode: 64,
	}, nil
}

func (n *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	stagePath := req.GetStagingTargetPath()
	backing := req.GetVolumeContext()[ParamBackingStorePath]
	if volumeID == "" || stagePath == "" || backing == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id, staging_target_path, and volume_context.backingStorePath are required")
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

	// 1. Hold an OS-level lock on the .img for the lifetime of the stage.
	//    Defense-in-depth against accidental dual-node mount.
	fd, err := flock.TryLock(imgPath)
	if err != nil {
		if errors.Is(err, flock.ErrLocked) {
			return nil, status.Errorf(codes.FailedPrecondition,
				"image %s is locked by another node", imgPath)
		}
		return nil, status.Errorf(codes.Internal, "flock: %v", err)
	}
	released := false
	defer func() {
		if !released {
			_ = fd.Close()
		}
	}()

	// Holding the flock proves no other node is using this image. If the
	// sidecar still names a different node, the previous holder lost its
	// lock (process exit, kernel-side NFS lease expiry, etc.) and we are
	// taking over. Log it; SetAttachedNode below overwrites the field.
	if meta, err := images.Get(ctx, volumeID); err == nil &&
		meta.AttachedNode != "" && meta.AttachedNode != n.nodeID {
		n.log.Warn("taking over volume from previous node",
			"volumeID", volumeID, "from", meta.AttachedNode, "to", n.nodeID)
	}

	// 2. Attach a loop device.
	dev, err := n.losetup.Attach(ctx, imgPath)
	if err != nil {
		if errors.Is(err, loop.ErrPoolExhausted) {
			return nil, status.Errorf(codes.ResourceExhausted, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "losetup: %v", err)
	}
	detachOnFail := func() { _ = n.losetup.Detach(ctx, dev) }

	// 3. e2fsck always (-p is a no-op on clean fs).
	if err := image.Fsck(ctx, n.exec, dev); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "fsck: %v", err)
	}

	// 4. If the image was expanded since the last stage, grow the fs now.
	//    resize2fs is a no-op when the fs already fills the device.
	if err := n.losetup.SetCapacity(ctx, dev); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "set capacity: %v", err)
	}
	if err := image.Resize2fs(ctx, n.exec, dev); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "resize2fs: %v", err)
	}

	// 5. Mount.
	if err := os.MkdirAll(stagePath, 0o755); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "mkdir stage: %v", err)
	}
	mountOpts := mountOptionsFromCap(req.GetVolumeCapability())
	if err := n.mnt.Mount(ctx, dev, stagePath, image.DefaultFs, mountOpts); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "mount: %v", err)
	}

	// 6. Persist mapping + sidecar's AttachedNode.
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
	if err := images.SetAttachedNode(ctx, volumeID, n.nodeID); err != nil {
		n.log.Warn("could not record AttachedNode in sidecar", "volumeID", volumeID, "err", err)
	}

	released = true
	n.fdByVolume[volumeID] = fd
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
		// Best-effort sidecar update; don't fail unstage on a stale NFS link.
		if images, err := image.New(filepath.Dir(m.ImagePath), n.exec); err == nil {
			_ = images.SetAttachedNode(ctx, volumeID, "")
		}
		_ = n.state.Delete(volumeID)
	}
	if fd := n.fdByVolume[volumeID]; fd != nil {
		_ = fd.Close()
		delete(n.fdByVolume, volumeID)
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
	path := req.GetVolumePath()
	if path == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_path is required")
	}
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return nil, status.Errorf(codes.Internal, "statfs %s: %v", path, err)
	}
	bs := int64(st.Bsize)
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
