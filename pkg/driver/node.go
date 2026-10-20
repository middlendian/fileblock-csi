package driver

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/middlendian/fileblock-csi/pkg/crypt"
	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/image"
	"github.com/middlendian/fileblock-csi/pkg/kube"
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
	luks     *crypt.Crypt
	sysRoot  string
	failures *failures
	// mountInfo reads this process's mountinfo; tests replace it.
	mountInfo func() ([]mount.MountInfo, error)
	// kube, when set, receives events for pods whose publish cannot
	// recover a failed volume.
	kube kube.Client

	// One mutex per volumeID protects Stage/Unstage from racing each other
	// inside this process.
	mu       sync.Mutex
	volMutex map[string]*sync.Mutex
	// keys holds each staged encrypted volume's stage secret, in memory
	// only, so a failed volume can be re-staged on publish. Guarded by mu.
	keys map[string]crypt.Keys
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
		luks:     crypt.New(exec),
		sysRoot:  "/sys",
		failures: newFailures(),
		mountInfo: func() ([]mount.MountInfo, error) {
			return mount.ReadMountInfo("/proc/self/mountinfo")
		},
		volMutex: map[string]*sync.Mutex{},
		keys:     map[string]crypt.Keys{},
	}
}

// SetKube gives the server a Kubernetes client for pod events.
func (n *NodeServer) SetKube(k kube.Client) { n.kube = k }

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
			rpc(csi.NodeServiceCapability_RPC_VOLUME_CONDITION),
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
	if volumeID == "" || req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id and staging_target_path are required")
	}
	unlock := n.lockVolume(volumeID)
	defer unlock()
	return n.stageLocked(ctx, req)
}

// stageLocked is NodeStageVolume for a caller that holds the volume lock.
func (n *NodeServer) stageLocked(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	stagePath := req.GetStagingTargetPath()
	if volumeID == "" || stagePath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id and staging_target_path are required")
	}
	cfg, err := store.ConfigFromVolumeContext(req.GetVolumeContext())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	encrypted := req.GetVolumeContext()[ParamEncrypted] == "true"
	var keys crypt.Keys
	var format crypt.Format
	if encrypted {
		if keys, err = keysFromSecrets(req.GetSecrets()); err != nil {
			return nil, err
		}
		if format, err = formatFromParams(req.GetVolumeContext(), true); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "%v", err)
		}
	} else if _, ok := req.GetSecrets()[secretKey]; ok {
		// A key in the stage secret does nothing for a plaintext volume;
		// most likely the operator forgot encrypted: "true".
		n.log.Warn("node-stage secret has a key but the volume is not encrypted; "+
			"check the StorageClass `encrypted` parameter", "volumeID", volumeID)
	}
	backing, err := n.registry.Get(ctx, cfg)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "mount backing store: %v", err)
	}
	if err := validateCapability(req.GetVolumeCapability()); err != nil {
		return nil, err
	}

	// Idempotency: if we already staged this volume to this path, return ok.
	stageMounted, err := n.mnt.IsMountPoint(ctx, stagePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check staging path: %v", err)
	}
	if existing, ok := n.state.Get(volumeID); ok && existing.StagePath == stagePath && stageMounted {
		return &csi.NodeStageVolumeResponse{}, nil
	}

	images, err := image.New(backing, n.exec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "open backing store: %v", err)
	}
	imgPath := images.ImagePath(volumeID)
	imgInfo, err := os.Stat(imgPath)
	if err != nil {
		// A backing store whose mount has gone away is an empty readable
		// directory, so every image under it stats as absent. Reporting
		// NotFound there points the operator at the controller and the
		// backing store, which are both fine. Only a definitive "not a
		// mountpoint" reclassifies — an errored check leaves the honest
		// NotFound in place.
		if mounted, mErr := n.mnt.IsMountPoint(ctx, backing); mErr == nil && !mounted {
			n.log.Error("backing store is not mounted; cannot resolve image",
				"volumeID", volumeID, "backingStore", backing)
			return nil, status.Errorf(codes.Unavailable,
				"backing store %s is not mounted; cannot resolve image %s", backing, imgPath)
		}
		return nil, status.Errorf(codes.NotFound, "image %s: %v", imgPath, err)
	}

	mapper := crypt.MapperName(volumeID)
	loops, err := n.losetup.FindBacking(ctx, imgPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "find loop devices: %v", err)
	}
	if stageMounted {
		return n.adoptStage(ctx, volumeID, imgPath, stagePath, loops, encrypted)
	}
	// The staging path is not mounted, so a loop already on the image is
	// either a leftover or in use elsewhere. A live one must not be
	// fscked, mounted again, or joined by a second loop.
	for _, dev := range loops {
		busy, err := n.loopBusy(ctx, dev)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "check %s: %v", dev, err)
		}
		if busy {
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %s is already in use on this node through %s; refusing a second attachment of one image",
				volumeID, dev)
		}
	}
	if encrypted {
		// The staging path is not mounted, so an open mapping is held by
		// something else: a second one would be two ext4 instances over
		// one file (#42).
		open, err := n.luks.IsOpen(ctx, mapper)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list dm-crypt mappings: %v", err)
		}
		if open {
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %s is already open on this node as %s; refusing a second mapping of one image",
				volumeID, crypt.MapperPath(mapper))
		}
	}

	// Idle leftovers (a stage whose state entry was lost) are detached, so
	// this stage is a normal first stage: fresh sector size, a freshly
	// opened backing file, set-capacity before the first read.
	for _, dev := range loops {
		if err := n.losetup.Detach(ctx, dev); err != nil {
			return nil, status.Errorf(codes.Internal, "losetup --detach: %v", err)
		}
	}
	if len(loops) > 0 {
		n.log.Info("detached idle loop devices left on image", "volumeID", volumeID, "loopDevs", loops)
	}

	// 1. Attach a loop device, sized to what the image already records.
	sector, err := n.loopSectorSize(ctx, imgPath, imgInfo.Size(), encrypted)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read sector size of %s: %v", imgPath, err)
	}
	if sector != image.DefaultBlockSize {
		n.log.Info("loop sector size follows the image's on-disk format",
			"volumeID", volumeID, "sectorSize", sector)
	}
	dev, err := n.losetup.Attach(ctx, imgPath, sector)
	if err != nil {
		if errors.Is(err, loop.ErrPoolExhausted) {
			return nil, status.Errorf(codes.ResourceExhausted, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "losetup: %v", err)
	}
	detachOnFail := func() { _ = n.losetup.Detach(ctx, dev) }

	target, cryptDev := dev, ""
	if encrypted {
		// Grow the loop first so the mapping opens at the expanded size.
		if err := n.losetup.SetCapacity(ctx, dev); err != nil {
			detachOnFail()
			return nil, status.Errorf(codes.Internal, "set capacity: %v", err)
		}
		var outcome crypt.Outcome
		cryptDev, outcome, err = n.luks.Prepare(ctx, dev, mapper, keys, format)
		if err != nil {
			detachOnFail()
			return nil, cryptStatus(err)
		}
		if outcome != crypt.OutcomeNone {
			n.log.Info("luks", "volumeID", volumeID, "outcome", string(outcome))
		}
		target = cryptDev
		detachOnFail = func() {
			_ = n.luks.Close(ctx, mapper)
			_ = n.losetup.Detach(ctx, dev)
		}
	}

	// 2. e2fsck always (-p is a no-op on clean fs).
	if err := image.Fsck(ctx, n.exec, target); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "fsck: %v", err)
	}

	// 3. If the image was expanded since the last stage, grow the fs now.
	//    resize2fs is a no-op when the fs already fills the device.
	if !encrypted {
		if err := n.losetup.SetCapacity(ctx, dev); err != nil {
			detachOnFail()
			return nil, status.Errorf(codes.Internal, "set capacity: %v", err)
		}
	}
	if err := image.Resize2fs(ctx, n.exec, target); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "resize2fs: %v", err)
	}

	// 4. Mount.
	if err := os.MkdirAll(stagePath, 0o755); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "mkdir stage: %v", err)
	}
	mountOpts := mountOptionsFromCap(req.GetVolumeCapability())
	if err := n.mnt.Mount(ctx, target, stagePath, image.DefaultFs, mountOpts); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "mount: %v", err)
	}

	// 5. Persist mapping.
	if err := n.state.Put(loop.Mapping{
		VolumeID:     volumeID,
		LoopDev:      dev,
		ImagePath:    imgPath,
		StagePath:    stagePath,
		CryptDev:     cryptDev,
		StageContext: req.GetVolumeContext(),
		MountFlags:   mountOptionsFromCap(req.GetVolumeCapability()),
	}); err != nil {
		_ = n.mnt.Unmount(ctx, stagePath)
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "persist state: %v", err)
	}
	if encrypted {
		// Kept for an internal re-stage: publish never sees the secret.
		n.mu.Lock()
		if old, ok := n.keys[volumeID]; ok {
			clear(old.Current)
			clear(old.Previous)
		}
		n.keys[volumeID] = crypt.Keys{Current: bytes.Clone(keys.Current), Previous: bytes.Clone(keys.Previous)}
		n.mu.Unlock()
	}
	n.failures.Clear(volumeID)
	return &csi.NodeStageVolumeResponse{}, nil
}

// adoptStage handles a stage request for a path that is already mounted
// but that the state file does not record — the kubelet re-staging after
// the plugin lost its state. If the mount is this volume's, record it and
// succeed; never stack a second loop and mount over it. Nothing else a
// stage does runs here: key-slot rotation and checking the requested
// capability against the existing mount wait for the next real stage.
func (n *NodeServer) adoptStage(ctx context.Context, volumeID, imgPath, stagePath string, loops []string, encrypted bool) (*csi.NodeStageVolumeResponse, error) {
	src, err := n.mnt.Source(ctx, stagePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read stage mount: %v", err)
	}
	m := loop.Mapping{VolumeID: volumeID, ImagePath: imgPath, StagePath: stagePath}
	if encrypted && src == crypt.MapperPath(crypt.MapperName(volumeID)) {
		ms, err := n.luks.List(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list dm-crypt mappings: %v", err)
		}
		for _, cm := range ms {
			if cm.Name == crypt.MapperName(volumeID) && slices.Contains(loops, cm.Backing) {
				m.LoopDev, m.CryptDev = cm.Backing, src
			}
		}
	} else if !encrypted && slices.Contains(loops, src) {
		m.LoopDev = src
	}
	if m.LoopDev == "" {
		return nil, status.Errorf(codes.FailedPrecondition,
			"staging path %s is already mounted from %s, which is not volume %s", stagePath, src, volumeID)
	}
	n.log.Info("adopting existing stage mount", "volumeID", volumeID, "loopDev", m.LoopDev)
	if err := n.state.Put(m); err != nil {
		return nil, status.Errorf(codes.Internal, "persist state: %v", err)
	}
	n.failures.Clear(volumeID)
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

	if err := n.popStageMounts(ctx, stagePath); err != nil {
		return nil, err
	}
	_ = os.Remove(stagePath)

	m, ok := n.state.Get(volumeID)
	if ok {
		if m.CryptDev != "" {
			if err := n.luks.Close(ctx, filepath.Base(m.CryptDev)); err != nil {
				return nil, status.Errorf(codes.Internal, "cryptsetup close: %v", err)
			}
		}
	} else {
		// No state entry to say whether this volume was ever opened
		// encrypted; try the deterministic mapper name anyway. A mapping
		// still mounted elsewhere fails "busy" and must be left alone, so
		// the error is discarded rather than failing the unstage.
		_ = n.luks.Close(ctx, crypt.MapperName(volumeID))
	}
	// Detach whatever is attached to the image, not just the state entry's
	// loop: the entry can be missing, or name one of several, after a
	// plugin restart, and its loop number may since have been reused by
	// another volume. A loop still held open elsewhere is only marked
	// autoclear. An image deleted while staged on NFS is silly-renamed to
	// .nfsXXXX, so then only the state entry still names its loop.
	name := volumeID + image.ImageExt
	loops, err := n.losetup.Find(ctx, func(a loop.Attachment) bool {
		return filepath.Base(a.BackFile) == name ||
			ok && a.Device == m.LoopDev && loop.SillyRenamed(a.BackFile)
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "find loop devices: %v", err)
	}
	for _, dev := range loops {
		if err := n.losetup.Detach(ctx, dev); err != nil {
			return nil, status.Errorf(codes.Internal, "losetup --detach: %v", err)
		}
	}
	n.mu.Lock()
	if k, ok := n.keys[volumeID]; ok {
		clear(k.Current)
		clear(k.Previous)
		delete(n.keys, volumeID)
	}
	n.mu.Unlock()
	if ok {
		_ = n.state.Delete(volumeID)
	}
	n.failures.Clear(volumeID)
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
	if _, failed := n.failures.Get(req.GetVolumeId()); failed {
		if err := n.restageFailed(ctx, req); err != nil {
			return nil, err
		}
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

// restageFailed rebuilds a failed volume's staging mount from a fresh open of
// its image when nothing publishes it. The kubelet never unstages a volume a
// pod on this node still wants, so a same-node replacement would otherwise be
// handed the broken filesystem.
func (n *NodeServer) restageFailed(ctx context.Context, req *csi.NodePublishVolumeRequest) error {
	volumeID := req.GetVolumeId()
	unlock := n.lockVolume(volumeID)
	defer unlock()
	if _, failed := n.failures.Get(volumeID); !failed {
		return nil // another publish already re-staged it
	}
	m, ok := n.state.Get(volumeID)
	if !ok {
		return status.Errorf(codes.Unavailable, "volume %s failed and has no state entry; waiting for the kubelet to unstage it", volumeID)
	}
	entries, err := n.mountInfo()
	if err != nil {
		return status.Errorf(codes.Internal, "read mountinfo: %v", err)
	}
	// Any target counts, this request's own too: a failed publish never
	// binds, so a mount there is an earlier publish whose pod still runs.
	if e, ok := mount.LastAt(entries, m.StagePath); ok {
		if ts := mount.PublishTargets(entries, e.MajorMinor); len(ts) > 0 {
			return status.Errorf(codes.Unavailable,
				"volume %s failed and is still published at %s; waiting for that pod to go", volumeID, ts[0].Path)
		}
	}
	if m.StageContext == nil {
		n.publishEvent(ctx, req, "fileblock volume "+volumeID+
			" failed and was staged by an older version; delete this pod to recover")
		return status.Errorf(codes.Unavailable, "volume %s failed and was staged by an older version; delete the pod to recover", volumeID)
	}
	var secrets map[string]string
	if m.StageContext[ParamEncrypted] == "true" {
		n.mu.Lock()
		k, ok := n.keys[volumeID]
		n.mu.Unlock()
		if !ok {
			n.publishEvent(ctx, req, "fileblock volume "+volumeID+
				" failed and its key is not in memory (the node plugin restarted); delete this pod to recover")
			return status.Errorf(codes.Unavailable, "volume %s failed and cannot be re-staged without its key; delete the pod", volumeID)
		}
		secrets = map[string]string{secretKey: string(k.Current)}
		if len(k.Previous) > 0 {
			secrets[secretPreviousKey] = string(k.Previous)
		}
	}
	// Tear down. Every stacked stage mount must go, or stageLocked would
	// adopt the broken one.
	if err := n.popStageMounts(ctx, m.StagePath); err != nil {
		return status.Errorf(codes.Internal, "re-stage: %v", status.Convert(err).Message())
	}
	// An empty LoopDev means an earlier re-stage already tore this down.
	if m.LoopDev != "" {
		if m.CryptDev != "" {
			if err := n.luks.Close(ctx, filepath.Base(m.CryptDev)); err != nil {
				return status.Errorf(codes.Internal, "re-stage: cryptsetup close: %v", err)
			}
		}
		if err := n.losetup.Detach(ctx, m.LoopDev); err != nil {
			return status.Errorf(codes.Internal, "re-stage: losetup --detach: %v", err)
		}
		// Keep the entry, minus the devices just released, so a failed
		// stage below can be retried; a success overwrites it.
		m.LoopDev, m.CryptDev = "", ""
		if err := n.state.Put(m); err != nil {
			return status.Errorf(codes.Internal, "re-stage: persist state: %v", err)
		}
	}
	_, err = n.stageLocked(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: m.StagePath,
		VolumeContext:     m.StageContext,
		Secrets:           secrets,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{MountFlags: m.MountFlags}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	})
	if err != nil {
		return status.Errorf(codes.Internal, "re-stage: %v", status.Convert(err).Message())
	}
	n.log.Warn("re-staged failed volume from a fresh open of its image", "volumeID", volumeID)
	return nil
}

// Pod identity the kubelet adds to the publish volume context when the
// CSIDriver sets podInfoOnMount.
const (
	podInfoName      = "csi.storage.k8s.io/pod.name"
	podInfoNamespace = "csi.storage.k8s.io/pod.namespace"
	podInfoUID       = "csi.storage.k8s.io/pod.uid"
)

// publishEvent tells the publishing pod why its volume cannot recover. The
// CSIDriver ships with podInfoOnMount: false, so usually there is no pod
// to name and the message is only logged.
func (n *NodeServer) publishEvent(ctx context.Context, req *csi.NodePublishVolumeRequest, msg string) {
	vc := req.GetVolumeContext()
	var p kube.Pod
	p.Metadata.Name, p.Metadata.Namespace, p.Metadata.UID = vc[podInfoName], vc[podInfoNamespace], vc[podInfoUID]
	if n.kube == nil || p.Metadata.Name == "" || p.Metadata.Namespace == "" || p.Metadata.UID == "" {
		n.log.Warn(msg, "volumeID", req.GetVolumeId())
		return
	}
	if err := n.kube.PodEvent(ctx, p, EventReasonVolumeFailed, msg); err != nil {
		n.log.Warn("emit event", "pod", p.Metadata.Namespace+"/"+p.Metadata.Name, "err", err)
	}
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
	cond := &csi.VolumeCondition{Abnormal: false, Message: "volume is healthy"}
	if at, failed := n.failures.Get(volumeID); failed {
		cond = &csi.VolumeCondition{
			Abnormal: true,
			Message: "ext4 forced the staged filesystem read-only at " + at.UTC().Format(time.RFC3339) +
				"; the backing file is unusable until the volume is re-staged",
		}
	}
	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{Unit: csi.VolumeUsage_BYTES, Total: total, Available: free, Used: used},
			{Unit: csi.VolumeUsage_INODES, Total: int64(st.Files), Available: int64(st.Ffree), Used: int64(st.Files - st.Ffree)},
		},
		VolumeCondition: cond,
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
	target := m.LoopDev
	if m.CryptDev != "" {
		if err := n.luks.Resize(ctx, filepath.Base(m.CryptDev)); err != nil {
			return nil, status.Errorf(codes.Internal, "cryptsetup resize: %v", err)
		}
		target = m.CryptDev
	}
	if err := image.Resize2fs(ctx, n.exec, target); err != nil {
		return nil, status.Errorf(codes.Internal, "resize2fs: %v", err)
	}
	return &csi.NodeExpandVolumeResponse{
		CapacityBytes: req.GetCapacityRange().GetRequiredBytes(),
	}, nil
}

// loopSectorSize reads the block size an image's on-disk format already
// commits it to — the LUKS2 sector size for an encrypted image, the ext4
// block size otherwise — and turns it into a loop sector size. Nothing is
// assumed for existing volumes; only an image with nothing recorded yet
// gets the default.
func (n *NodeServer) loopSectorSize(ctx context.Context, imgPath string, size int64, encrypted bool) (int, error) {
	var recorded int
	var err error
	if encrypted {
		recorded, err = n.luks.SectorSize(ctx, imgPath)
	} else {
		recorded, err = image.FSBlockSize(imgPath)
	}
	if err != nil {
		return 0, err
	}
	return loop.SectorSizeFor(recorded, size), nil
}

// maxStageUnmounts bounds how many stacked mounts unstage pops.
const maxStageUnmounts = 8

// popStageMounts unmounts every mount stacked on stagePath (older versions
// could stack a second one), but gives up rather than loop on a path that
// stays mounted. Errors are gRPC statuses.
func (n *NodeServer) popStageMounts(ctx context.Context, stagePath string) error {
	for i := 0; ; i++ {
		mounted, err := n.mnt.IsMountPoint(ctx, stagePath)
		if err != nil {
			return status.Errorf(codes.Internal, "check stage: %v", err)
		}
		if !mounted {
			return nil
		}
		if i == maxStageUnmounts {
			return status.Errorf(codes.Internal, "%s still mounted after %d unmounts", stagePath, i)
		}
		if err := n.mnt.Unmount(ctx, stagePath); err != nil {
			return status.Errorf(codes.Internal, "umount stage: %v", err)
		}
	}
}

// loopBusy reports whether dev is mounted or held by another block device
// (a dm-crypt mapping).
func (n *NodeServer) loopBusy(ctx context.Context, dev string) (bool, error) {
	if mounted, err := n.mnt.IsSourceMounted(ctx, dev); err != nil || mounted {
		return mounted, err
	}
	// The pod shares the host PID namespace, so pid 1's mounts are the
	// host's; a check that cannot read them falls through to the others.
	if mounted, err := n.mnt.IsSourceMountedInHost(ctx, dev); err != nil {
		n.log.Warn("cannot check the host mount namespace for a loop device", "loopDev", dev, "err", err)
	} else if mounted {
		return true, nil
	}
	holders, err := os.ReadDir(filepath.Join(n.sysRoot, "block", filepath.Base(dev), "holders"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return len(holders) > 0, nil
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
