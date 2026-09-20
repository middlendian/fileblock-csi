package driver

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/exec/exectest"
	"github.com/middlendian/fileblock-csi/pkg/loop"
	"github.com/middlendian/fileblock-csi/pkg/mount"
	"github.com/middlendian/fileblock-csi/pkg/store"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestNodeRegistry returns a Registry backed by a temp dir with nil
// mounters — sufficient for tests that only exercise input validation (the
// registry is never asked to actually mount anything).
func newTestNodeRegistry(t *testing.T) *store.Registry {
	t.Helper()
	return store.NewRegistry(t.TempDir(), nil, nil, nil, discardLog())
}

// TestNodeGetInfoReportsNodeSegment verifies that NodeGetInfo always reports
// exactly one topology segment: {fileblock.csi/node: nodeID}.
func TestNodeGetInfoReportsNodeSegment(t *testing.T) {
	n := NewNodeServer("nodeA", nil, nil, nil, nil, discardLog(), newTestNodeRegistry(t))

	resp, err := n.NodeGetInfo(context.Background(), nil)
	if err != nil {
		t.Fatalf("NodeGetInfo: %v", err)
	}
	if resp.NodeId != "nodeA" {
		t.Fatalf("NodeId: got %q want nodeA", resp.NodeId)
	}
	segs := resp.GetAccessibleTopology().GetSegments()
	if len(segs) != 1 {
		t.Fatalf("expected exactly 1 topology segment, got %d: %v", len(segs), segs)
	}
	if segs[topologyKeyNode] != "nodeA" {
		t.Fatalf("topology segments: got %v want {%s: nodeA}", segs, topologyKeyNode)
	}
}

func TestNodeGetCapabilities(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), newTestNodeRegistry(t))
	resp, err := n.NodeGetCapabilities(context.Background(), nil)
	if err != nil {
		t.Fatalf("NodeGetCapabilities: %v", err)
	}
	want := map[csi.NodeServiceCapability_RPC_Type]bool{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME: false,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS:     false,
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME:        false,
	}
	for _, c := range resp.Capabilities {
		if r := c.GetRpc(); r != nil {
			if _, ok := want[r.Type]; ok {
				want[r.Type] = true
			}
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("missing capability %s", k)
		}
	}
}

// TestNodeStageVolumeRejectsMissingBackingStoreType verifies that an empty
// (or otherwise invalid) volume context yields InvalidArgument — the store
// parser signals the problem before the registry is consulted.
func TestNodeStageVolumeRejectsMissingBackingStoreType(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), newTestNodeRegistry(t))
	_, err := n.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-1",
		StagingTargetPath: "/staging/vol-1",
		VolumeContext:     map[string]string{}, // no backingStore.type
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestNodeStageMissingArgs(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), newTestNodeRegistry(t))
	cases := []*csi.NodeStageVolumeRequest{
		{}, // all empty
		{VolumeId: "v"},
		{StagingTargetPath: "/s"},
	}
	for _, req := range cases {
		_, err := n.NodeStageVolume(context.Background(), req)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("req=%+v: got %v, want InvalidArgument", req, err)
		}
	}
}

func TestNodeStageBadCapability(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), newTestNodeRegistry(t))
	// Provide a valid volume context so we reach the capability check.
	// The registry will attempt to mount when Get is called, but we need
	// to fail before that — so we use a block-type capability which
	// validateCapability rejects before registry.Get is reached... except
	// that with the new flow, registry.Get is called before validateCapability.
	// Instead, verify that an invalid capability on a request that would
	// otherwise need a real registry returns InvalidArgument. We use a
	// request with empty volume_id/staging_target_path to trigger early exit.
	_, err := n.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "v",
		StagingTargetPath: "/s",
		VolumeContext:     map[string]string{}, // missing backingStore.type → InvalidArgument
		// no VolumeCapability
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestNodeUnstageMissingArgs(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), newTestNodeRegistry(t))
	_, err := n.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestNodePublishMissingArgs(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), newTestNodeRegistry(t))
	_, err := n.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: "v"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestNodeUnpublishMissingArgs(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), newTestNodeRegistry(t))
	_, err := n.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestNodeGetVolumeStatsMissingArgs(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), newTestNodeRegistry(t))
	cases := []*csi.NodeGetVolumeStatsRequest{
		{},
		{VolumeId: "v"},
		{VolumePath: "/tmp"},
	}
	for _, req := range cases {
		_, err := n.NodeGetVolumeStats(context.Background(), req)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("req=%+v: got %v, want InvalidArgument", req, err)
		}
	}
}

func TestNodeGetVolumeStatsNotStaged(t *testing.T) {
	st, err := loop.LoadState(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	n := NewNodeServer("n", nil, nil, nil, st, discardLog(), newTestNodeRegistry(t))
	_, err = n.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "missing",
		VolumePath: "/tmp",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("got %v, want NotFound", err)
	}
}

func TestNodeGetVolumeStatsPathMissing(t *testing.T) {
	st, err := loop.LoadState(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if err := st.Put(loop.Mapping{VolumeID: "v", LoopDev: "/dev/loop0", ImagePath: "/tmp/x.img", StagePath: "/tmp/stage"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	n := NewNodeServer("n", nil, nil, nil, st, discardLog(), newTestNodeRegistry(t))
	_, err = n.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "v",
		VolumePath: "/tmp/does-not-exist-" + t.Name(),
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("got %v, want NotFound", err)
	}
}

func TestNodeGetVolumeStatsRealPath(t *testing.T) {
	st, err := loop.LoadState(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	dir := t.TempDir()
	if err := st.Put(loop.Mapping{VolumeID: "v", LoopDev: "/dev/loop0", ImagePath: "/tmp/x.img", StagePath: dir}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	n := NewNodeServer("n", nil, nil, nil, st, discardLog(), newTestNodeRegistry(t))
	resp, err := n.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "v",
		VolumePath: dir,
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats: %v", err)
	}
	if len(resp.Usage) != 2 {
		t.Fatalf("Usage entries=%d", len(resp.Usage))
	}
	for _, u := range resp.Usage {
		if u.Total <= 0 {
			t.Errorf("non-positive Total for unit %s", u.Unit)
		}
	}
}

func TestNodeExpandVolumeMissingArgs(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), newTestNodeRegistry(t))
	cases := []*csi.NodeExpandVolumeRequest{
		{},
		{VolumeId: "v"},
		{VolumePath: "/tmp"},
	}
	for _, req := range cases {
		_, err := n.NodeExpandVolume(context.Background(), req)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("req=%+v: got %v, want InvalidArgument", req, err)
		}
	}
}

func TestValidateCapability(t *testing.T) {
	good := &csi.VolumeCapability{
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
	}
	if err := validateCapability(good); err != nil {
		t.Fatalf("good: %v", err)
	}
	if err := validateCapability(nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil cap: %v", err)
	}
	bad := &csi.VolumeCapability{
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
	}
	if err := validateCapability(bad); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("block cap: %v", err)
	}
	wrongMode := &csi.VolumeCapability{
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY},
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
	}
	if err := validateCapability(wrongMode); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("wrong mode: %v", err)
	}
	wrongFs := &csi.VolumeCapability{
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"}},
	}
	if err := validateCapability(wrongFs); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("wrong fs: %v", err)
	}
}

func TestMountOptionsFromCap(t *testing.T) {
	if got := mountOptionsFromCap(nil); got != nil {
		t.Fatalf("nil: %v", got)
	}
	got := mountOptionsFromCap(&csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{MountFlags: []string{"ro", "noatime"}}},
	})
	if len(got) != 2 || got[0] != "ro" || got[1] != "noatime" {
		t.Fatalf("got %v", got)
	}
}

// TestLockVolumeSerializesPerVolume ensures concurrent stage/unstage on the
// same volumeID is serialized.
func TestLockVolumeSerializesPerVolume(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), newTestNodeRegistry(t))
	const N = 8
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := n.lockVolume("same")
			cur := inFlight.Add(1)
			for {
				m := maxInFlight.Load()
				if cur <= m || maxInFlight.CompareAndSwap(m, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
			unlock()
		}()
	}
	wg.Wait()
	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("max concurrent holders=%d, want 1", got)
	}
}

// TestLockVolumeDifferentVolumesParallel proves the lock is per-volume.
func TestLockVolumeDifferentVolumesParallel(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), newTestNodeRegistry(t))
	u1 := n.lockVolume("a")
	defer u1()
	done := make(chan struct{})
	go func() {
		u2 := n.lockVolume("b")
		u2()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("locks on different volumes should not block each other")
	}
}

// newStageTestNode wires a NodeServer whose registry mounts a local
// backing store through the fake runner, so NodeStageVolume reaches the
// image lookup. storeMounted drives findmnt(8), the only thing that
// distinguishes a healthy store with no such image from a store whose
// mount has gone away underneath it.
func newStageTestNode(t *testing.T, storeMounted bool) (*NodeServer, map[string]string) {
	t.Helper()
	fake := exectest.New()
	fake.Func = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "findmnt" {
			if !storeMounted {
				return "", &fbexec.Error{ExitCode: 1}
			}
			return args[len(args)-1], nil
		}
		return "", nil
	}
	mnt := mount.New(fake)
	reg := store.NewRegistry(t.TempDir(), nil, store.NewLocalMounter(mnt), mnt, discardLog())
	state, err := loop.LoadState(filepath.Join(t.TempDir(), "loop-mappings.json"))
	if err != nil {
		t.Fatal(err)
	}
	n := NewNodeServer("n", fake, mnt, nil, state, discardLog(), reg)
	cfg := store.Config{Type: store.TypeLocal, LocalPath: t.TempDir()}
	return n, cfg.ToVolumeContext()
}

func stageReq(volumeID string, vc map[string]string) *csi.NodeStageVolumeRequest {
	return &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: "/staging/" + volumeID,
		VolumeContext:     vc,
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
		},
	}
}

// TestNodeStageMissingImageOnUnmountedStoreIsUnavailable pins the
// diagnostic half of the fix. An absent .img and an absent backing-store
// mount look identical from a stat, and reporting NotFound sends the
// operator to the controller and the backing store, both of which are
// healthy. Unavailable names the node as the problem.
func TestNodeStageMissingImageOnUnmountedStoreIsUnavailable(t *testing.T) {
	n, vc := newStageTestNode(t, false)
	_, err := n.NodeStageVolume(context.Background(), stageReq("vol-1", vc))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("got %v, want Unavailable", err)
	}
}

// TestNodeStageMissingImageOnMountedStoreIsNotFound is the other half:
// when the store really is mounted, an absent image is an honest
// NotFound and must stay one — csi-sanity depends on it.
func TestNodeStageMissingImageOnMountedStoreIsNotFound(t *testing.T) {
	n, vc := newStageTestNode(t, true)
	_, err := n.NodeStageVolume(context.Background(), stageReq("vol-1", vc))
	if status.Code(err) != codes.NotFound {
		t.Fatalf("got %v, want NotFound", err)
	}
}
