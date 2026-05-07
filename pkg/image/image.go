// Package image owns the on-disk contract for a fileblock volume: a sparse
// ext4 file at ${backingStore}/${volumeID}.img and a sidecar metadata file at
// ${backingStore}/${volumeID}.json. Nothing else in the driver writes to the
// backing store.
package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
)

const (
	ImageExt    = ".img"
	SidecarExt  = ".json"
	DefaultFs   = "ext4"
	sidecarPerm = 0o644
)

// Metadata is persisted to ${volumeID}.json. Schema is intentionally tiny —
// k8s remains the source of truth for everything except local sanity.
type Metadata struct {
	VolumeID      string    `json:"volumeId"`
	CapacityBytes int64     `json:"capacityBytes"`
	FsType        string    `json:"fsType"`
	CreatedAt     time.Time `json:"createdAt"`
}

// Manager is the image-file CRUD interface. Tests substitute a fake.
type Manager interface {
	Create(ctx context.Context, volumeID string, capacityBytes int64) (*Metadata, error)
	Delete(ctx context.Context, volumeID string) error
	Get(ctx context.Context, volumeID string) (*Metadata, error)
	List(ctx context.Context) ([]*Metadata, error)
	Resize(ctx context.Context, volumeID string, capacityBytes int64) (*Metadata, error)
	ImagePath(volumeID string) string
	SidecarPath(volumeID string) string
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

func (m *fsManager) SidecarPath(volumeID string) string {
	return filepath.Join(m.root, volumeID+SidecarExt)
}

// Create is idempotent. If both files already exist with the requested
// capacity, the existing metadata is returned. Mismatched capacity is an
// error (CSI AlreadyExists semantics; the caller maps it).
func (m *fsManager) Create(ctx context.Context, volumeID string, capacityBytes int64) (*Metadata, error) {
	if err := validateVolumeID(volumeID); err != nil {
		return nil, err
	}
	if capacityBytes <= 0 {
		return nil, fmt.Errorf("capacityBytes must be > 0")
	}
	imgPath := m.ImagePath(volumeID)
	sidePath := m.SidecarPath(volumeID)

	if existing, err := readSidecar(sidePath); err == nil {
		if existing.CapacityBytes != capacityBytes {
			return nil, &CapacityMismatchError{
				Requested: capacityBytes,
				Existing:  existing.CapacityBytes,
			}
		}
		return existing, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	// Cleanup partial state if image exists without a sidecar.
	_ = os.Remove(imgPath)

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
	meta := &Metadata{
		VolumeID:      volumeID,
		CapacityBytes: capacityBytes,
		FsType:        DefaultFs,
		CreatedAt:     time.Now().UTC(),
	}
	if err := writeSidecarAtomic(sidePath, meta); err != nil {
		_ = os.Remove(imgPath)
		return nil, err
	}
	return meta, nil
}

func (m *fsManager) Delete(ctx context.Context, volumeID string) error {
	if err := validateVolumeID(volumeID); err != nil {
		return err
	}
	imgErr := os.Remove(m.ImagePath(volumeID))
	sideErr := os.Remove(m.SidecarPath(volumeID))
	for _, e := range []error{imgErr, sideErr} {
		if e != nil && !errors.Is(e, fs.ErrNotExist) {
			return e
		}
	}
	return nil
}

func (m *fsManager) Get(ctx context.Context, volumeID string) (*Metadata, error) {
	if err := validateVolumeID(volumeID); err != nil {
		return nil, err
	}
	return readSidecar(m.SidecarPath(volumeID))
}

func (m *fsManager) List(ctx context.Context) ([]*Metadata, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil, err
	}
	var out []*Metadata
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), SidecarExt) {
			continue
		}
		meta, err := readSidecar(filepath.Join(m.root, e.Name()))
		if err != nil {
			continue // skip malformed sidecars rather than failing the list
		}
		out = append(out, meta)
	}
	return out, nil
}

// Resize implements offline expand: truncate the file, update the sidecar.
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
	meta.CapacityBytes = capacityBytes
	if err := writeSidecarAtomic(m.SidecarPath(volumeID), meta); err != nil {
		return nil, err
	}
	return meta, nil
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

func readSidecar(path string) (*Metadata, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Metadata
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse sidecar %s: %w", path, err)
	}
	return &m, nil
}

func writeSidecarAtomic(path string, m *Metadata) error {
	tmp := path + ".tmp"
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, sidecarPerm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
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
