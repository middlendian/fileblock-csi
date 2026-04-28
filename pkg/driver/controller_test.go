package driver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/middlendian/fileblock-csi/pkg/image"
)

// fakeImages is an in-memory image.Manager for unit tests.
type fakeImages struct {
	mu        sync.Mutex
	root      string
	store     map[string]*image.Metadata
	createErr error
	resizeErr error
}

func newFakeImages() *fakeImages {
	return &fakeImages{root: "/srv/fb", store: map[string]*image.Metadata{}}
}

func (f *fakeImages) Create(_ context.Context, volumeID string, capacityBytes int64) (*image.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	if existing, ok := f.store[volumeID]; ok {
		if existing.CapacityBytes != capacityBytes {
			return nil, &image.CapacityMismatchError{Requested: capacityBytes, Existing: existing.CapacityBytes}
		}
		return existing, nil
	}
	m := &image.Metadata{
		VolumeID:      volumeID,
		CapacityBytes: capacityBytes,
		FsType:        image.DefaultFs,
		CreatedAt:     time.Now().UTC(),
	}
	f.store[volumeID] = m
	return m, nil
}

func (f *fakeImages) Delete(_ context.Context, volumeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.store, volumeID)
	return nil
}

func (f *fakeImages) Get(_ context.Context, volumeID string) (*image.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.store[volumeID]
	if !ok {
		return nil, errors.New("not found")
	}
	return m, nil
}

func (f *fakeImages) List(_ context.Context) ([]*image.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*image.Metadata, 0, len(f.store))
	for _, m := range f.store {
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeImages) Resize(_ context.Context, volumeID string, capacityBytes int64) (*image.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resizeErr != nil {
		return nil, f.resizeErr
	}
	m, ok := f.store[volumeID]
	if !ok {
		return nil, errors.New("not found")
	}
	if m.AttachedNode != "" {
		return nil, &image.VolumeInUseError{Node: m.AttachedNode}
	}
	if capacityBytes < m.CapacityBytes {
		return nil, errors.New("shrink not supported")
	}
	m.CapacityBytes = capacityBytes
	return m, nil
}

func (f *fakeImages) ImagePath(volumeID string) string   { return f.root + "/" + volumeID + ".img" }
func (f *fakeImages) SidecarPath(volumeID string) string { return f.root + "/" + volumeID + ".json" }

func (f *fakeImages) SetAttachedNode(_ context.Context, volumeID, nodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.store[volumeID]
	if !ok {
		return errors.New("not found")
	}
	m.AttachedNode = nodeID
	return nil
}

func mountCap() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
	}
}

func TestControllerGetCapabilities(t *testing.T) {
	c := NewControllerServer(newFakeImages(), "/srv/fb", "")
	resp, err := c.ControllerGetCapabilities(context.Background(), nil)
	if err != nil {
		t.Fatalf("ControllerGetCapabilities: %v", err)
	}
	want := map[csi.ControllerServiceCapability_RPC_Type]bool{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME: false,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME:        false,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES:         false,
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

func TestCreateVolumeMissingName(t *testing.T) {
	c := NewControllerServer(newFakeImages(), "/srv/fb", "")
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		VolumeCapabilities: []*csi.VolumeCapability{mountCap()},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestCreateVolumeMissingCaps(t *testing.T) {
	c := NewControllerServer(newFakeImages(), "/srv/fb", "")
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{Name: "x"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestCreateVolumeBlockRejected(t *testing.T) {
	c := NewControllerServer(newFakeImages(), "/srv/fb", "")
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "x",
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
				AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
			},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestCreateVolumeBadFsType(t *testing.T) {
	c := NewControllerServer(newFakeImages(), "/srv/fb", "")
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "x",
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
				AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"}},
			},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestCreateVolumeWrongAccessMode(t *testing.T) {
	c := NewControllerServer(newFakeImages(), "/srv/fb", "")
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "x",
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
				AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
			},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestCreateVolumeUsesRequestedCapacity(t *testing.T) {
	imgs := newFakeImages()
	c := NewControllerServer(imgs, "/srv/fb", "")
	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "x",
		VolumeCapabilities: []*csi.VolumeCapability{mountCap()},
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 256 * 1024 * 1024},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if resp.Volume.CapacityBytes != 256*1024*1024 {
		t.Fatalf("capacity=%d", resp.Volume.CapacityBytes)
	}
	if !strings.HasPrefix(resp.Volume.VolumeId, "fb-") {
		t.Fatalf("volumeId=%q", resp.Volume.VolumeId)
	}
	if resp.Volume.VolumeContext[ParamBackingStorePath] != "/srv/fb" {
		t.Fatalf("volumeContext=%v", resp.Volume.VolumeContext)
	}
}

func TestCreateVolumeUsesLimitWhenRequiredZero(t *testing.T) {
	c := NewControllerServer(newFakeImages(), "/srv/fb", "")
	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "x",
		VolumeCapabilities: []*csi.VolumeCapability{mountCap()},
		CapacityRange:      &csi.CapacityRange{LimitBytes: 64 * 1024 * 1024},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if resp.Volume.CapacityBytes != 64*1024*1024 {
		t.Fatalf("capacity=%d", resp.Volume.CapacityBytes)
	}
}

func TestCreateVolumeDefaultsCapacity(t *testing.T) {
	c := NewControllerServer(newFakeImages(), "/srv/fb", "")
	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "x",
		VolumeCapabilities: []*csi.VolumeCapability{mountCap()},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if resp.Volume.CapacityBytes != defaultCapacityBytes {
		t.Fatalf("capacity=%d want %d", resp.Volume.CapacityBytes, defaultCapacityBytes)
	}
}

func TestCreateVolumePreservesPreferredTopology(t *testing.T) {
	c := NewControllerServer(newFakeImages(), "/srv/fb", "fileblock.csi/backing-store")
	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "x",
		VolumeCapabilities: []*csi.VolumeCapability{mountCap()},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Preferred: []*csi.Topology{
				{Segments: map[string]string{"fileblock.csi/backing-store": "shared"}},
			},
			Requisite: []*csi.Topology{
				{Segments: map[string]string{"fileblock.csi/backing-store": "other"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if got := resp.Volume.AccessibleTopology; len(got) != 1 ||
		got[0].Segments["fileblock.csi/backing-store"] != "shared" {
		t.Fatalf("AccessibleTopology=%v", got)
	}
}

func TestCreateVolumeFallsBackToRequisiteTopology(t *testing.T) {
	c := NewControllerServer(newFakeImages(), "/srv/fb", "")
	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "x",
		VolumeCapabilities: []*csi.VolumeCapability{mountCap()},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{Segments: map[string]string{TopologyKeyNode: "n1"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if got := resp.Volume.AccessibleTopology; len(got) != 1 ||
		got[0].Segments[TopologyKeyNode] != "n1" {
		t.Fatalf("AccessibleTopology=%v", got)
	}
}

func TestCreateVolumeMapsCapacityMismatchToAlreadyExists(t *testing.T) {
	imgs := newFakeImages()
	imgs.createErr = &image.CapacityMismatchError{Requested: 1, Existing: 2}
	c := NewControllerServer(imgs, "/srv/fb", "")
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "x",
		VolumeCapabilities: []*csi.VolumeCapability{mountCap()},
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("got %v, want AlreadyExists", err)
	}
}

func TestDeleteVolume(t *testing.T) {
	imgs := newFakeImages()
	_, _ = imgs.Create(context.Background(), "fb-1", 1024)
	c := NewControllerServer(imgs, "/srv/fb", "")
	if _, err := c.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "fb-1"}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if _, err := c.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestListVolumes(t *testing.T) {
	imgs := newFakeImages()
	_, _ = imgs.Create(context.Background(), "fb-a", 100)
	_, _ = imgs.Create(context.Background(), "fb-b", 200)
	c := NewControllerServer(imgs, "/srv/fb", "")
	resp, err := c.ListVolumes(context.Background(), &csi.ListVolumesRequest{})
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("entries=%d", len(resp.Entries))
	}
	for _, e := range resp.Entries {
		if e.Volume.VolumeContext[ParamBackingStorePath] != "/srv/fb" {
			t.Errorf("missing volumeContext: %v", e.Volume.VolumeContext)
		}
	}
}

func TestValidateVolumeCapabilitiesNotFound(t *testing.T) {
	c := NewControllerServer(newFakeImages(), "/srv/fb", "")
	_, err := c.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           "nope",
		VolumeCapabilities: []*csi.VolumeCapability{mountCap()},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("got %v, want NotFound", err)
	}
}

func TestValidateVolumeCapabilitiesConfirms(t *testing.T) {
	imgs := newFakeImages()
	_, _ = imgs.Create(context.Background(), "fb-1", 1024)
	c := NewControllerServer(imgs, "/srv/fb", "")
	resp, err := c.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           "fb-1",
		VolumeCapabilities: []*csi.VolumeCapability{mountCap()},
	})
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities: %v", err)
	}
	if resp.Confirmed == nil {
		t.Fatalf("not confirmed: %+v", resp)
	}
}

func TestValidateVolumeCapabilitiesUnsupportedReturnsMessage(t *testing.T) {
	imgs := newFakeImages()
	_, _ = imgs.Create(context.Background(), "fb-1", 1024)
	c := NewControllerServer(imgs, "/srv/fb", "")
	resp, err := c.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "fb-1",
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
				AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities: %v", err)
	}
	if resp.Confirmed != nil {
		t.Fatalf("should not be confirmed: %+v", resp)
	}
	if resp.Message == "" {
		t.Fatal("expected explanatory Message")
	}
}

func TestControllerExpandVolumeRequiredBytesMissing(t *testing.T) {
	c := NewControllerServer(newFakeImages(), "/srv/fb", "")
	_, err := c.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{VolumeId: "x"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestControllerExpandVolumeInUse(t *testing.T) {
	imgs := newFakeImages()
	_, _ = imgs.Create(context.Background(), "fb-1", 1024)
	_ = imgs.SetAttachedNode(context.Background(), "fb-1", "node-x")
	c := NewControllerServer(imgs, "/srv/fb", "")
	_, err := c.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      "fb-1",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 4096},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v, want FailedPrecondition", err)
	}
}

func TestControllerExpandVolumeOK(t *testing.T) {
	imgs := newFakeImages()
	_, _ = imgs.Create(context.Background(), "fb-1", 1024)
	c := NewControllerServer(imgs, "/srv/fb", "")
	resp, err := c.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      "fb-1",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 4096},
	})
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}
	if resp.CapacityBytes != 4096 {
		t.Fatalf("capacity=%d", resp.CapacityBytes)
	}
	if !resp.NodeExpansionRequired {
		t.Fatal("expected NodeExpansionRequired=true")
	}
}
