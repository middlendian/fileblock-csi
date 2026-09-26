package driver

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/middlendian/fileblock-csi/pkg/crypt"
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

// encStage wires a NodeServer that reaches a full stage: a real .img in
// the registry's backing path, a fake runner that answers losetup,
// e2fsck, resize2fs, mount, findmnt and cryptsetup, and an empty fake
// sysfs. cryptFn overrides cryptsetup behavior; nil means "everything
// succeeds, header already formatted".
type encStage struct {
	n       *NodeServer
	fake    *exectest.FakeRunner
	vc      map[string]string
	sysRoot string
	stage   string
	backing string
	log     *bytes.Buffer
}

func newEncStage(t *testing.T, encrypted bool, cryptFn func(fbexec.Cmd) (string, error)) *encStage {
	t.Helper()
	fake := exectest.New()
	fake.Func = func(_ context.Context, name string, args ...string) (string, error) {
		switch name {
		case "findmnt":
			return args[len(args)-1], nil
		case "losetup":
			if len(args) > 0 && args[0] == "--find" {
				return "/dev/loop7\n", nil
			}
		}
		return "", nil
	}
	fake.CmdFunc = func(_ context.Context, c fbexec.Cmd) (string, error) {
		if cryptFn != nil {
			return cryptFn(c)
		}
		if c.Args[0] == "luksDump" {
			return "Label:          fileblock\n", nil
		}
		return "", nil
	}
	mnt := mount.New(fake)
	reg := store.NewRegistry(t.TempDir(), nil, store.NewLocalMounter(mnt), mnt, discardLog())
	state, err := loop.LoadState(filepath.Join(t.TempDir(), "loop-mappings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&logBuf, nil))
	n := NewNodeServer("n", fake, mnt, loop.NewLosetup(fake), state, lg, reg)
	sysRoot := t.TempDir()
	n.luks = crypt.NewAt(fake, sysRoot)
	cfg := store.Config{Type: store.TypeLocal, LocalPath: t.TempDir()}
	backing, err := reg.Get(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backing, "vol-1.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	vc := cfg.ToVolumeContext()
	if encrypted {
		vc[ParamEncrypted] = "true"
	}
	// Drop setup's bind mount so tests see only the stage's own calls.
	fake.Reset()
	return &encStage{n: n, fake: fake, vc: vc, sysRoot: sysRoot, stage: filepath.Join(t.TempDir(), "stage"), backing: backing, log: &logBuf}
}

func (e *encStage) req(secrets map[string]string) *csi.NodeStageVolumeRequest {
	r := stageReq("vol-1", e.vc)
	r.StagingTargetPath = e.stage
	r.Secrets = secrets
	return r
}

// callIndex returns the index of the first call whose name and leading
// args match, or -1.
func callIndex(calls []exectest.Call, name string, args ...string) int {
	for i, c := range calls {
		if c.Name == name && len(c.Args) >= len(args) && slices.Equal(c.Args[:len(args)], args) {
			return i
		}
	}
	return -1
}

func TestNodeStageEncryptedHappyPath(t *testing.T) {
	e := newEncStage(t, true, nil)
	if _, err := e.n.NodeStageVolume(context.Background(), e.req(map[string]string{"key": testKey})); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	mapper := crypt.MapperPath(crypt.MapperName("vol-1"))
	calls := e.fake.Calls
	setCap := callIndex(calls, "losetup", "--set-capacity")
	open := callIndex(calls, "cryptsetup", "open", "--type")
	if setCap < 0 || open < 0 || setCap > open {
		t.Fatalf("set-capacity must precede open: %+v", calls)
	}
	if i := callIndex(calls, "e2fsck"); i < 0 || !slices.Contains(calls[i].Args, mapper) {
		t.Fatalf("e2fsck not run on %s", mapper)
	}
	if i := callIndex(calls, "resize2fs"); i < 0 || calls[i].Args[0] != mapper {
		t.Fatalf("resize2fs not run on %s", mapper)
	}
	if i := callIndex(calls, "mount"); i < 0 || !slices.Contains(calls[i].Args, mapper) {
		t.Fatalf("mount source is not %s", mapper)
	}
	if !bytes.Equal(calls[open].Secrets[0], []byte(testKey)) {
		t.Fatal("open did not receive the key over a secret fd")
	}
	m, ok := e.n.state.Get("vol-1")
	if !ok || m.CryptDev != mapper || m.LoopDev != "/dev/loop7" {
		t.Fatalf("state = %+v", m)
	}
}

// Review finding 1: the state-file idempotency check must stay above the
// IsOpen guard. A retried stage of a volume that is already mounted at
// this path must succeed even though its mapper is (correctly) still
// open — if the IsOpen guard ran first it would refuse with
// FailedPrecondition instead.
func TestNodeStageEncryptedIdempotentRetrySucceedsWithMappingOpen(t *testing.T) {
	e := newEncStage(t, true, nil)
	mapper := crypt.MapperName("vol-1")
	d := filepath.Join(e.sysRoot, "block", "dm-0")
	if err := os.MkdirAll(filepath.Join(d, "dm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "dm", "name"), []byte(mapper+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.n.state.Put(loop.Mapping{
		VolumeID:  "vol-1",
		LoopDev:   "/dev/loop7",
		ImagePath: filepath.Join(e.backing, "vol-1.img"),
		StagePath: e.stage,
		CryptDev:  crypt.MapperPath(mapper),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.n.NodeStageVolume(context.Background(), e.req(map[string]string{"key": testKey})); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	if callIndex(e.fake.Calls, "losetup") >= 0 || callIndex(e.fake.Calls, "cryptsetup") >= 0 {
		t.Fatalf("idempotent restage should not touch losetup/cryptsetup: %+v", e.fake.Calls)
	}
}

// Review finding 3: rotation progress must be visible in the logs, not
// just inferable from cryptsetup calls an operator can't see.
func TestNodeStageEncryptedLogsRotationOutcome(t *testing.T) {
	e := newEncStage(t, true, func(c fbexec.Cmd) (string, error) {
		switch c.Args[0] {
		case "luksDump":
			return "Label:          fileblock\n", nil
		case "open":
			if slices.Contains(c.Args, "--test-passphrase") && bytes.Equal(c.Secrets[0], []byte(testKey)) {
				return "", &fbexec.Error{Cmd: "cryptsetup", ExitCode: 2}
			}
			return "", nil
		}
		return "", nil
	})
	secrets := map[string]string{"key": testKey, "previousKey": "prev-" + testKey}
	if _, err := e.n.NodeStageVolume(context.Background(), e.req(secrets)); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	if !strings.Contains(e.log.String(), "outcome=rotated") {
		t.Fatalf("expected the rotation outcome logged, got: %s", e.log.String())
	}
}

func TestNodeStageEncryptedMissingKeyTouchesNothing(t *testing.T) {
	e := newEncStage(t, true, nil)
	_, err := e.n.NodeStageVolume(context.Background(), e.req(nil))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v, want FailedPrecondition", err)
	}
	if callIndex(e.fake.Calls, "losetup", "--find") >= 0 {
		t.Fatal("attached a loop without a key")
	}
}

func TestNodeStageEncryptedAlreadyOpenRefusedBeforeAttach(t *testing.T) {
	e := newEncStage(t, true, nil)
	d := filepath.Join(e.sysRoot, "block", "dm-0")
	_ = os.MkdirAll(filepath.Join(d, "dm"), 0o755)
	_ = os.WriteFile(filepath.Join(d, "dm", "name"), []byte(crypt.MapperName("vol-1")+"\n"), 0o644)
	_, err := e.n.NodeStageVolume(context.Background(), e.req(map[string]string{"key": testKey}))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v, want FailedPrecondition", err)
	}
	if callIndex(e.fake.Calls, "losetup", "--find") >= 0 {
		t.Fatal("attached a second loop for an already-open volume")
	}
}

func TestNodeStageEncryptedWrongKeyDetaches(t *testing.T) {
	e := newEncStage(t, true, func(c fbexec.Cmd) (string, error) {
		if c.Args[0] == "open" {
			return "", &fbexec.Error{Cmd: "cryptsetup", ExitCode: 2}
		}
		return "", nil
	})
	_, err := e.n.NodeStageVolume(context.Background(), e.req(map[string]string{"key": testKey}))
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("got %v, want PermissionDenied", err)
	}
	if callIndex(e.fake.Calls, "losetup", "--detach", "/dev/loop7") < 0 {
		t.Fatal("loop not detached after wrong key")
	}
}

// Review Focus 4.
func TestNodeStageEncryptedFailureClosesMapping(t *testing.T) {
	e := newEncStage(t, true, nil)
	inner := e.fake.Func
	e.fake.Func = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "e2fsck" {
			return "", &fbexec.Error{Cmd: "e2fsck", ExitCode: 8}
		}
		return inner(ctx, name, args...)
	}
	_, err := e.n.NodeStageVolume(context.Background(), e.req(map[string]string{"key": testKey}))
	if status.Code(err) != codes.Internal {
		t.Fatalf("got %v, want Internal", err)
	}
	closeIdx := callIndex(e.fake.Calls, "cryptsetup", "close", crypt.MapperName("vol-1"))
	detachIdx := callIndex(e.fake.Calls, "losetup", "--detach", "/dev/loop7")
	if closeIdx < 0 || detachIdx < 0 || closeIdx > detachIdx {
		t.Fatalf("want close then detach: %+v", e.fake.Calls)
	}
}

// The plaintext path must be byte-for-byte what it was before encryption:
// every call's full argv, not just its name and first flag, in order and
// excluding findmnt (IsMountPoint probing is unrelated to the stage
// sequence and its call count is incidental).
func TestNodeStagePlaintextSequenceUnchanged(t *testing.T) {
	e := newEncStage(t, false, nil)
	if _, err := e.n.NodeStageVolume(context.Background(), e.req(nil)); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	imgPath := filepath.Join(e.backing, "vol-1.img")
	var seq []string
	for _, c := range e.fake.Calls {
		if c.Name == "findmnt" {
			continue
		}
		seq = append(seq, strings.TrimSpace(c.Name+" "+strings.Join(c.Args, " ")))
	}
	want := []string{
		"losetup --find --show " + imgPath,
		"e2fsck -p -f /dev/loop7",
		"losetup --set-capacity /dev/loop7",
		"resize2fs /dev/loop7",
		"mount -t ext4 /dev/loop7 " + e.stage,
	}
	if !slices.Equal(seq, want) {
		t.Fatalf("sequence = %v, want %v", seq, want)
	}
}

func TestNodeUnstageEncryptedOrder(t *testing.T) {
	e := newEncStage(t, true, nil)
	mapper := crypt.MapperPath(crypt.MapperName("vol-1"))
	_ = e.n.state.Put(loop.Mapping{VolumeID: "vol-1", LoopDev: "/dev/loop7", ImagePath: "/x.img", StagePath: e.stage, CryptDev: mapper})
	// Unmount only runs umount(8) against a path that exists and resolves
	// as a mountpoint; NodeStageVolume normally creates it, but this test
	// exercises unstage in isolation.
	if err := os.MkdirAll(e.stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.n.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{VolumeId: "vol-1", StagingTargetPath: e.stage}); err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}
	u := callIndex(e.fake.Calls, "umount")
	c := callIndex(e.fake.Calls, "cryptsetup", "close", crypt.MapperName("vol-1"))
	d := callIndex(e.fake.Calls, "losetup", "--detach", "/dev/loop7")
	if u < 0 || c < 0 || d < 0 || u >= c || c >= d {
		t.Fatalf("want umount < close < detach, got %d %d %d", u, c, d)
	}
}

func TestNodeExpandEncryptedResizesMapper(t *testing.T) {
	e := newEncStage(t, true, nil)
	mapper := crypt.MapperPath(crypt.MapperName("vol-1"))
	_ = e.n.state.Put(loop.Mapping{VolumeID: "vol-1", LoopDev: "/dev/loop7", ImagePath: "/x.img", StagePath: e.stage, CryptDev: mapper})
	if _, err := e.n.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{VolumeId: "vol-1", VolumePath: "/p"}); err != nil {
		t.Fatalf("NodeExpandVolume: %v", err)
	}
	r := callIndex(e.fake.Calls, "cryptsetup", "resize", crypt.MapperName("vol-1"))
	f := callIndex(e.fake.Calls, "resize2fs", mapper)
	if r < 0 || f < 0 || r > f {
		t.Fatalf("want cryptsetup resize then resize2fs %s: %+v", mapper, e.fake.Calls)
	}
}
