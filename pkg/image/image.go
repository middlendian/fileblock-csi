// Package image owns the on-disk contract for a fileblock volume: a sparse
// ext4 file at ${backingStore}/${volumeID}.img. The .img is the single source
// of truth — its apparent size (st_size) is the volume capacity. Nothing else
// in the driver writes to the backing store.
package image

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
)

const (
	ImageExt  = ".img"
	DefaultFs = "ext4"
)

// Metadata describes a volume to callers. It is derived on read from the
// .img file's name and stat — there is no separate persisted metadata.
type Metadata struct {
	VolumeID      string
	CapacityBytes int64
}

// Manager is the image-file CRUD interface. Tests substitute a fake.
type Manager interface {
	Create(ctx context.Context, volumeID string, capacityBytes int64) (*Metadata, error)
	Delete(ctx context.Context, volumeID string) error
	Get(ctx context.Context, volumeID string) (*Metadata, error)
	List(ctx context.Context) ([]*Metadata, error)
	Resize(ctx context.Context, volumeID string, capacityBytes int64) (*Metadata, error)
	ImagePath(volumeID string) string
}

type fsManager struct {
	root string
	exec fbexec.Runner
}

// New returns a Manager rooted at backingStorePath. The directory must exist
// and be writable.
func New(backingStorePath string, r fbexec.Runner) (Manager, error) {
	st, err := os.Stat(backingStorePath)
	if err != nil {
		return nil, fmt.Errorf("backing store %s: %w", backingStorePath, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("backing store %s is not a directory", backingStorePath)
	}
	return &fsManager{root: backingStorePath, exec: r}, nil
}

func (m *fsManager) ImagePath(volumeID string) string {
	return filepath.Join(m.root, volumeID+ImageExt)
}

// Create is idempotent. If the .img already exists with the requested
// capacity it is adopted as-is. Mismatched on-disk size is AlreadyExists
// (the caller maps it). If the .img is corrupt or otherwise unusable the
// problem surfaces at NodeStageVolume's fsck — that is the mount error.
func (m *fsManager) Create(ctx context.Context, volumeID string, capacityBytes int64) (*Metadata, error) {
	if err := validateVolumeID(volumeID); err != nil {
		return nil, err
	}
	if capacityBytes <= 0 {
		return nil, fmt.Errorf("capacityBytes must be > 0")
	}
	imgPath := m.ImagePath(volumeID)

	if st, err := os.Stat(imgPath); err == nil {
		if st.Size() != capacityBytes {
			return nil, &CapacityMismatchError{
				Requested: capacityBytes,
				Existing:  st.Size(),
			}
		}
		return &Metadata{VolumeID: volumeID, CapacityBytes: st.Size()}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("stat %s: %w", imgPath, err)
	}

	if err := truncateSparse(imgPath, capacityBytes); err != nil {
		return nil, err
	}
	if _, err := m.exec.Run(ctx, "mkfs.ext4", "-q", "-F",
		"-m", "0",
		"-E", "lazy_itable_init=1,lazy_journal_init=1",
		imgPath); err != nil {
		_ = os.Remove(imgPath)
		return nil, fmt.Errorf("mkfs.ext4 %s: %w", imgPath, err)
	}
	return &Metadata{VolumeID: volumeID, CapacityBytes: capacityBytes}, nil
}

func (m *fsManager) Delete(ctx context.Context, volumeID string) error {
	if err := validateVolumeID(volumeID); err != nil {
		return err
	}
	if err := os.Remove(m.ImagePath(volumeID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Pre-sidecar-removal deployments may still have a same-named .json
	// next to the .img. Best-effort sweep so a Create→Delete cycle leaves
	// nothing behind. Failures are ignored.
	_ = os.Remove(filepath.Join(m.root, volumeID+".json"))
	return nil
}

func (m *fsManager) Get(ctx context.Context, volumeID string) (*Metadata, error) {
	if err := validateVolumeID(volumeID); err != nil {
		return nil, err
	}
	st, err := os.Stat(m.ImagePath(volumeID))
	if err != nil {
		return nil, err
	}
	return &Metadata{VolumeID: volumeID, CapacityBytes: st.Size()}, nil
}

func (m *fsManager) List(ctx context.Context) ([]*Metadata, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil, err
	}
	var out []*Metadata
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ImageExt) {
			continue
		}
		volumeID := strings.TrimSuffix(e.Name(), ImageExt)
		st, err := os.Stat(filepath.Join(m.root, e.Name()))
		if err != nil {
			continue // race with Delete; skip rather than fail the list
		}
		out = append(out, &Metadata{VolumeID: volumeID, CapacityBytes: st.Size()})
	}
	return out, nil
}

// Resize implements offline expand: truncate the .img upward.
// resize2fs is run by the node plugin on next stage (it owns the loop device).
// The CSI external-resizer is responsible for waiting until the volume is
// unpublished before calling ControllerExpandVolume; we don't double-check.
// Refuses to shrink.
func (m *fsManager) Resize(ctx context.Context, volumeID string, capacityBytes int64) (*Metadata, error) {
	meta, err := m.Get(ctx, volumeID)
	if err != nil {
		return nil, err
	}
	if capacityBytes < meta.CapacityBytes {
		return nil, fmt.Errorf("shrink not supported: %d < %d", capacityBytes, meta.CapacityBytes)
	}
	if capacityBytes == meta.CapacityBytes {
		return meta, nil
	}
	if err := truncateSparse(m.ImagePath(volumeID), capacityBytes); err != nil {
		return nil, err
	}
	return &Metadata{VolumeID: volumeID, CapacityBytes: capacityBytes}, nil
}

func truncateSparse(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		return fmt.Errorf("truncate %s: %w", path, err)
	}
	return nil
}

func validateVolumeID(id string) error {
	if id == "" {
		return fmt.Errorf("empty volumeID")
	}
	if strings.ContainsAny(id, "/\\\x00") {
		return fmt.Errorf("invalid volumeID %q", id)
	}
	return nil
}

// CapacityMismatchError is returned when Create finds an existing volume with
// a different capacity. Maps to CSI AlreadyExists.
type CapacityMismatchError struct{ Requested, Existing int64 }

func (e *CapacityMismatchError) Error() string {
	return fmt.Sprintf("volume already exists with capacity %d, requested %d", e.Existing, e.Requested)
}
