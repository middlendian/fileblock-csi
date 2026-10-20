package driver

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/middlendian/fileblock-csi/pkg/loop"
)

func TestFailuresMarkOnce(t *testing.T) {
	f := newFailures()
	t0 := time.Unix(1000, 0)
	if !f.Mark("v", t0) {
		t.Fatal("first Mark should report new")
	}
	if f.Mark("v", t0.Add(time.Minute)) {
		t.Fatal("second Mark should not report new")
	}
	if at, ok := f.Get("v"); !ok || !at.Equal(t0) {
		t.Fatalf("failedAt = %v ok=%v, want %v", at, ok, t0)
	}
}

func TestFailuresClearKeepsHold(t *testing.T) {
	f := newFailures()
	until := time.Unix(2000, 0)
	f.Mark("v", time.Unix(1000, 0))
	f.Hold("v", until)
	f.Clear("v")
	if _, ok := f.Get("v"); ok {
		t.Fatal("Clear should drop the failed state")
	}
	if !f.HeldUntil("v").Equal(until) {
		t.Fatal("Clear must not reset the hold-off")
	}
}

func TestNodeGetVolumeStatsReportsCondition(t *testing.T) {
	n := newStatsTestNode(t)
	n.failures.Mark("vol-1", time.Now())
	resp, err := n.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId: "vol-1", VolumePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats: %v", err)
	}
	if c := resp.GetVolumeCondition(); c == nil || !c.Abnormal || c.Message == "" {
		t.Fatalf("want an abnormal condition with a message, got %+v", c)
	}
}

func TestNodeGetVolumeStatsHealthyCondition(t *testing.T) {
	n := newStatsTestNode(t)
	resp, err := n.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId: "vol-1", VolumePath: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if c := resp.GetVolumeCondition(); c == nil || c.Abnormal {
		t.Fatalf("want a normal condition, got %+v", c)
	}
}

// newStatsTestNode builds a NodeServer whose state file has vol-1 staged.
func newStatsTestNode(t *testing.T) *NodeServer {
	t.Helper()
	st, err := loop.LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(loop.Mapping{VolumeID: "vol-1", LoopDev: "/dev/loop9", StagePath: "/stage"}); err != nil {
		t.Fatal(err)
	}
	return NewNodeServer("n", nil, nil, nil, st, discardLog(), newTestNodeRegistry(t))
}
