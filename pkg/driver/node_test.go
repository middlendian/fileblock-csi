package driver

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/middlendian/fileblock-csi/pkg/loop"
)

// TestNodeGetInfoDefaultTopology verifies that an unset topology key/value
// preserves the legacy per-node pin: the segment is fileblock.csi/node and
// the value is the nodeID.
func TestNodeGetInfoDefaultTopology(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := NewNodeServer("node-a", nil, nil, nil, nil, log, "", "")

	resp, err := n.NodeGetInfo(context.Background(), nil)
	if err != nil {
		t.Fatalf("NodeGetInfo: %v", err)
	}
	if resp.NodeId != "node-a" {
		t.Fatalf("NodeId: got %q want node-a", resp.NodeId)
	}
	segs := resp.GetAccessibleTopology().GetSegments()
	if len(segs) != 1 || segs[TopologyKeyNode] != "node-a" {
		t.Fatalf("topology segments: got %v want {%s: node-a}", segs, TopologyKeyNode)
	}
}

// TestNodeGetInfoCustomTopology verifies that operators can advertise a
// shared backing-store segment so multiple nodes appear interchangeable to
// the provisioner.
func TestNodeGetInfoCustomTopology(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := NewNodeServer("node-b", nil, nil, nil, nil, log,
		"fileblock.csi/backing-store", "nfs-shared")

	resp, err := n.NodeGetInfo(context.Background(), nil)
	if err != nil {
		t.Fatalf("NodeGetInfo: %v", err)
	}
	if resp.NodeId != "node-b" {
		t.Fatalf("NodeId: got %q want node-b", resp.NodeId)
	}
	segs := resp.GetAccessibleTopology().GetSegments()
	if len(segs) != 1 || segs["fileblock.csi/backing-store"] != "nfs-shared" {
		t.Fatalf("topology segments: got %v want {fileblock.csi/backing-store: nfs-shared}", segs)
	}
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNodeGetCapabilities(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), "", "")
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

func TestNodeStageMissingArgs(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), "", "")
	cases := []*csi.NodeStageVolumeRequest{
		{}, // all empty
		{VolumeId: "v"},
		{VolumeId: "v", StagingTargetPath: "/s"},
	}
	for _, req := range cases {
		_, err := n.NodeStageVolume(context.Background(), req)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("req=%+v: got %v, want InvalidArgument", req, err)
		}
	}
}

func TestNodeStageBadCapability(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), "", "")
	_, err := n.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "v",
		StagingTargetPath: "/s",
		VolumeContext:     map[string]string{ParamBackingStorePath: "/srv"},
		// no VolumeCapability
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestNodeUnstageMissingArgs(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), "", "")
	_, err := n.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestNodePublishMissingArgs(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), "", "")
	_, err := n.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: "v"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestNodeUnpublishMissingArgs(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), "", "")
	_, err := n.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestNodeGetVolumeStatsMissingArgs(t *testing.T) {
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), "", "")
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
	n := NewNodeServer("n", nil, nil, nil, st, discardLog(), "", "")
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
	n := NewNodeServer("n", nil, nil, nil, st, discardLog(), "", "")
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
	n := NewNodeServer("n", nil, nil, nil, st, discardLog(), "", "")
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
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), "", "")
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
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), "", "")
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
	n := NewNodeServer("n", nil, nil, nil, nil, discardLog(), "", "")
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
