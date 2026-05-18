package driver

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/exec/exectest"
	"github.com/middlendian/fileblock-csi/pkg/image"
	"github.com/middlendian/fileblock-csi/pkg/mount"
	"github.com/middlendian/fileblock-csi/pkg/store"
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
	if capacityBytes < m.CapacityBytes {
		return nil, errors.New("shrink not supported")
	}
	m.CapacityBytes = capacityBytes
	return m, nil
}

func (f *fakeImages) ImagePath(volumeID string) string { return f.root + "/" + volumeID + ".img" }

// newTestRegistry returns a Registry backed by a temp directory with
// fake mounters that accept any mount call.
func newTestRegistry(t *testing.T) *store.Registry {
	t.Helper()
	fake := exectest.New()
	fake.SetDefault("", nil)
	mnt := mount.New(fake)
	return store.NewRegistry(t.TempDir(), store.NewNFSMounter(fake), store.NewLocalMounter(mnt), mnt)
}

// newTestServer creates a ControllerServer wired to a test registry and a
// shared fakeImages instance. The same fakeImages is returned so tests can
// inspect or pre-populate it.
func newTestServer(t *testing.T) (*ControllerServer, *fakeImages) {
	t.Helper()
	reg := newTestRegistry(t)
	imgs := newFakeImages()
	c := NewControllerServer(reg, nil)
	c.newImages = func(string, fbexec.Runner) (image.Manager, error) { return imgs, nil }
	return c, imgs
}

// singleNodeWriterMount returns a minimal valid VolumeCapability for
// SINGLE_NODE_WRITER mount access.
func singleNodeWriterMount() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

// nfsParams returns minimal valid NFS StorageClass parameters.
func nfsParams() map[string]string {
	return map[string]string{
		store.ParamType:      "nfs",
		store.ParamNFSServer: "s",
		store.ParamNFSPath:   "/p",
	}
}

func TestControllerGetCapabilities(t *testing.T) {
	c, _ := newTestServer(t)
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
	c, _ := newTestServer(t)
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestCreateVolumeMissingCaps(t *testing.T) {
	c, _ := newTestServer(t)
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "x",
		Parameters: nfsParams(),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestCreateVolumeBlockRejected(t *testing.T) {
	c, _ := newTestServer(t)
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "x",
		Parameters: nfsParams(),
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
	c, _ := newTestServer(t)
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "x",
		Parameters: nfsParams(),
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
	c, _ := newTestServer(t)
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "x",
		Parameters: nfsParams(),
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
	c, _ := newTestServer(t)
	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "x",
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 256 * 1024 * 1024},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if resp.Volume.CapacityBytes != 256*1024*1024 {
		t.Fatalf("capacity=%d", resp.Volume.CapacityBytes)
	}
	// volumeID is now fb-<12hex>-<name>
	if len(resp.Volume.VolumeId) < len("fb-")+12+1 {
		t.Fatalf("volumeId=%q too short", resp.Volume.VolumeId)
	}
}

func TestCreateVolumeUsesLimitWhenRequiredZero(t *testing.T) {
	c, _ := newTestServer(t)
	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "x",
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
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
	c, _ := newTestServer(t)
	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "x",
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if resp.Volume.CapacityBytes != defaultCapacityBytes {
		t.Fatalf("capacity=%d want %d", resp.Volume.CapacityBytes, defaultCapacityBytes)
	}
}

func TestCreateVolumeIsIdempotentOnSameName(t *testing.T) {
	c, _ := newTestServer(t)
	req := &csi.CreateVolumeRequest{
		Name:               "pvcabc",
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 64 * 1024 * 1024},
	}
	first, err := c.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("first CreateVolume: %v", err)
	}
	second, err := c.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreateVolume: %v", err)
	}
	if first.Volume.VolumeId != second.Volume.VolumeId {
		t.Fatalf("VolumeIds differ: %q vs %q", first.Volume.VolumeId, second.Volume.VolumeId)
	}
}

func TestCreateVolumeSameNameDifferentCapacityAlreadyExists(t *testing.T) {
	c, _ := newTestServer(t)
	if _, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvcclash",
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 64 * 1024 * 1024},
	}); err != nil {
		t.Fatalf("first CreateVolume: %v", err)
	}
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvcclash",
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 128 * 1024 * 1024},
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("got %v, want AlreadyExists", err)
	}
}

func TestCreateVolumeMapsCapacityMismatchToAlreadyExists(t *testing.T) {
	reg := newTestRegistry(t)
	imgs := newFakeImages()
	imgs.createErr = &image.CapacityMismatchError{Requested: 1, Existing: 2}
	c := NewControllerServer(reg, nil)
	c.newImages = func(string, fbexec.Runner) (image.Manager, error) { return imgs, nil }
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "x",
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("got %v, want AlreadyExists", err)
	}
}

func TestDeleteVolume(t *testing.T) {
	c, _ := newTestServer(t)
	// Create a volume first to populate the registry.
	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "vol1",
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	volumeID := resp.Volume.VolumeId

	if _, err := c.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if _, err := c.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestListVolumes(t *testing.T) {
	c, _ := newTestServer(t)
	// Create two volumes to populate the registry and fakeImages.
	if _, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "vola",
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 100},
	}); err != nil {
		t.Fatalf("CreateVolume a: %v", err)
	}
	if _, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "volb",
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 200},
	}); err != nil {
		t.Fatalf("CreateVolume b: %v", err)
	}

	resp, err := c.ListVolumes(context.Background(), &csi.ListVolumesRequest{})
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("entries=%d, want 2", len(resp.Entries))
	}
	// VolumeContext is intentionally omitted in ListVolumes (no per-volume cfg).
	for _, e := range resp.Entries {
		if e.Volume.VolumeContext != nil {
			t.Errorf("ListVolumes entry has unexpected VolumeContext: %v", e.Volume.VolumeContext)
		}
	}
}

func TestListVolumesRejectsStartingToken(t *testing.T) {
	c, _ := newTestServer(t)
	_, err := c.ListVolumes(context.Background(), &csi.ListVolumesRequest{StartingToken: "garbage"})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("got %v, want Aborted", err)
	}
}

func TestValidateVolumeCapabilitiesEmptyCapsRejected(t *testing.T) {
	c, _ := newTestServer(t)
	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "vol1",
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	_, err = c.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: resp.Volume.VolumeId,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestValidateVolumeCapabilitiesNotFound(t *testing.T) {
	c, _ := newTestServer(t)
	// Must have a valid fb-<storeID>-<name> volumeID but a storeID not in registry.
	// Use parseStoreIDFromVolumeID indirectly: craft a well-formed ID that won't be registered.
	// Easiest: create a volume in a separate server so the registry doesn't know the storeID.
	reg2 := newTestRegistry(t)
	imgs2 := newFakeImages()
	c2 := NewControllerServer(reg2, nil)
	c2.newImages = func(string, fbexec.Runner) (image.Manager, error) { return imgs2, nil }
	resp, err := c2.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "nope",
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	if err != nil {
		t.Fatalf("CreateVolume in c2: %v", err)
	}
	// c doesn't know about this storeID, so imageManagerForVolumeID returns NotFound.
	_, err = c.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           resp.Volume.VolumeId,
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("got %v, want NotFound", err)
	}
}

func TestValidateVolumeCapabilitiesConfirms(t *testing.T) {
	c, _ := newTestServer(t)
	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "vol1",
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	vresp, err := c.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           resp.Volume.VolumeId,
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities: %v", err)
	}
	if vresp.Confirmed == nil {
		t.Fatalf("not confirmed: %+v", vresp)
	}
}

func TestValidateVolumeCapabilitiesUnsupportedReturnsMessage(t *testing.T) {
	c, _ := newTestServer(t)
	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "vol1",
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	vresp, err := c.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: resp.Volume.VolumeId,
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
	if vresp.Confirmed != nil {
		t.Fatalf("should not be confirmed: %+v", vresp)
	}
	if vresp.Message == "" {
		t.Fatal("expected explanatory Message")
	}
}

func TestControllerExpandVolumeRequiredBytesMissing(t *testing.T) {
	c, _ := newTestServer(t)
	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "vol1",
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	_, err = c.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{VolumeId: resp.Volume.VolumeId})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestControllerExpandVolumeOK(t *testing.T) {
	c, _ := newTestServer(t)
	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "vol1",
		Parameters:         nfsParams(),
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1024},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	eresp, err := c.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      resp.Volume.VolumeId,
		CapacityRange: &csi.CapacityRange{RequiredBytes: 4096},
	})
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}
	if eresp.CapacityBytes != 4096 {
		t.Fatalf("capacity=%d", eresp.CapacityBytes)
	}
	if !eresp.NodeExpansionRequired {
		t.Fatal("expected NodeExpansionRequired=true")
	}
}

// Task 3.2: new topology-specific tests.

func TestCreateVolumeNFSReturnsEmptyTopology(t *testing.T) {
	reg := newTestRegistry(t)
	imgFac := func(path string, _ fbexec.Runner) (image.Manager, error) {
		return newFakeImages(), nil
	}
	c := NewControllerServer(reg, nil)
	c.newImages = imgFac

	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "v1",
		Parameters: map[string]string{
			store.ParamType:      "nfs",
			store.ParamNFSServer: "s",
			store.ParamNFSPath:   "/p",
		},
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if len(resp.Volume.AccessibleTopology) != 0 {
		t.Errorf("AccessibleTopology = %v, want empty", resp.Volume.AccessibleTopology)
	}
	if resp.Volume.VolumeContext[store.ParamType] != "nfs" {
		t.Errorf("VolumeContext = %v", resp.Volume.VolumeContext)
	}
}

func TestCreateVolumeLocalPinsToPreferredNode(t *testing.T) {
	reg := newTestRegistry(t)
	imgFac := func(path string, _ fbexec.Runner) (image.Manager, error) {
		return newFakeImages(), nil
	}
	c := NewControllerServer(reg, nil)
	c.newImages = imgFac

	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "v1",
		Parameters: map[string]string{
			store.ParamType:      "local",
			store.ParamLocalPath: "/srv/data",
		},
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Preferred: []*csi.Topology{{Segments: map[string]string{"fileblock.csi/node": "node-a"}}},
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if len(resp.Volume.AccessibleTopology) != 1 {
		t.Fatalf("AccessibleTopology = %v", resp.Volume.AccessibleTopology)
	}
	if resp.Volume.AccessibleTopology[0].Segments["fileblock.csi/node"] != "node-a" {
		t.Errorf("pinned segment = %v", resp.Volume.AccessibleTopology[0].Segments)
	}
}

func TestCreateVolumeMissingTypeIsInvalidArgument(t *testing.T) {
	reg := newTestRegistry(t)
	c := NewControllerServer(reg, nil)
	c.newImages = func(string, fbexec.Runner) (image.Manager, error) { return newFakeImages(), nil }
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "v1",
		Parameters:         map[string]string{},
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}
