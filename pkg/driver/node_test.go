package driver

import (
	"context"
	"io"
	"log/slog"
	"testing"
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
