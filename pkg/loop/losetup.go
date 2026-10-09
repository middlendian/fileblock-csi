// Package loop manages loop-device attach/detach and the persistent state
// file that maps volumeID -> loop device. losetup(8) is the source of truth
// for what is actually attached; the state file is a cache used to make
// unstage cheap and to drive startup orphan cleanup.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/image"
)

type Losetup struct{ exec fbexec.Runner }

func NewLosetup(r fbexec.Runner) *Losetup { return &Losetup{exec: r} }

// Attach calls `losetup --find --show --sector-size <n> <imagePath>` and
// returns the chosen /dev/loopN. The sector size is always explicit (see
// SectorSizeFor), never losetup's default. Returns ErrPoolExhausted when
// the kernel has no free loop devices (LOOP_CTL_GET_FREE returns ENOSPC).
func (l *Losetup) Attach(ctx context.Context, imagePath string, sectorSize int) (string, error) {
	out, err := l.exec.Run(ctx, "losetup", "--find", "--show",
		"--sector-size", strconv.Itoa(sectorSize), imagePath)
	if err != nil {
		var e *fbexec.Error
		if errors.As(err, &e) && strings.Contains(strings.ToLower(e.Output), "no such device") {
			return "", ErrPoolExhausted
		}
		return "", fmt.Errorf("losetup --find --show %s: %w", imagePath, err)
	}
	dev := strings.TrimSpace(out)
	if !strings.HasPrefix(dev, "/dev/loop") {
		return "", fmt.Errorf("losetup returned unexpected device %q", dev)
	}
	return dev, nil
}

// minSectorSize is the smallest logical block size the kernel supports.
const minSectorSize = 512

// SectorSizeFor picks the loop device sector size for an image whose
// on-disk format records recorded bytes (its ext4 block size, or its LUKS2
// sector size; 0 when nothing is recorded yet). New images get
// image.DefaultBlockSize. The result never exceeds the page size, the largest a
// loop device supports, and always divides imageSize, halving for images
// that predate 4 KiB size rounding. Never exceeding recorded is what keeps
// existing volumes mountable: the kernel refuses a filesystem whose block
// size is smaller than its device's sectors.
func SectorSizeFor(recorded int, imageSize int64) int {
	s := recorded
	if s <= 0 {
		s = image.DefaultBlockSize
	}
	s = min(s, os.Getpagesize())
	for s > minSectorSize && imageSize%int64(s) != 0 {
		s /= 2
	}
	return s
}

// Detach calls `losetup --detach <dev>`. Idempotent: a device that's already
// detached returns nil.
func (l *Losetup) Detach(ctx context.Context, dev string) error {
	_, err := l.exec.Run(ctx, "losetup", "--detach", dev)
	if err == nil {
		return nil
	}
	var e *fbexec.Error
	if errors.As(err, &e) && strings.Contains(strings.ToLower(e.Output), "no such device") {
		return nil
	}
	return fmt.Errorf("losetup --detach %s: %w", dev, err)
}

// Attachment is one row from `losetup --json --list`.
type Attachment struct {
	Device   string `json:"name"`
	BackFile string `json:"back-file"`
}

// deletedSuffix is what the kernel appends to the path of an unlinked file.
const deletedSuffix = " (deleted)"

// List returns every currently-attached loop device. A back-file whose
// image was unlinked while attached is reported without the kernel's
// " (deleted)" suffix, so it still matches its image.
func (l *Losetup) List(ctx context.Context) ([]Attachment, error) {
	out, err := l.exec.Run(ctx, "losetup", "--json", "--list")
	if err != nil {
		return nil, fmt.Errorf("losetup --list: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	var wire struct {
		Loopdevices []Attachment `json:"loopdevices"`
	}
	if err := json.Unmarshal([]byte(out), &wire); err != nil {
		return nil, fmt.Errorf("parse losetup output: %w", err)
	}
	for i := range wire.Loopdevices {
		wire.Loopdevices[i].BackFile = strings.TrimSuffix(wire.Loopdevices[i].BackFile, deletedSuffix)
	}
	return wire.Loopdevices, nil
}

// FindImage returns every loop device whose backing file is named name, a
// volume's "<volumeID>.img". It matches the name, not the full path, for
// callers that do not know the image's store: a loop attached by an earlier
// node-plugin process reports its file relative to that process's
// backing-store mount (see Backs), and volume IDs already carry their store.
func (l *Losetup) FindImage(ctx context.Context, name string) ([]string, error) {
	return l.Find(ctx, func(a Attachment) bool { return filepath.Base(a.BackFile) == name })
}

// FindBacking returns every loop device attached to the image at imagePath
// (see Backs).
func (l *Losetup) FindBacking(ctx context.Context, imagePath string) ([]string, error) {
	return l.Find(ctx, func(a Attachment) bool { return Backs(a.BackFile, imagePath) })
}

// Find returns every attached loop device for which match is true.
func (l *Losetup) Find(ctx context.Context, match func(Attachment) bool) ([]string, error) {
	live, err := l.List(ctx)
	if err != nil {
		return nil, err
	}
	var devs []string
	for _, a := range live {
		if match(a) {
			devs = append(devs, a.Device)
		}
	}
	return devs, nil
}

// SetCapacity refreshes the loop device's view of the backing file's size
// after the file has been truncated larger. Required before resize2fs.
func (l *Losetup) SetCapacity(ctx context.Context, dev string) error {
	if _, err := l.exec.Run(ctx, "losetup", "--set-capacity", dev); err != nil {
		return fmt.Errorf("losetup --set-capacity %s: %w", dev, err)
	}
	return nil
}

// ErrPoolExhausted is returned when the kernel has no free loop devices.
// Operators fix this by raising max_loop or modprobe loop max_loop=64.
var ErrPoolExhausted = errors.New("loop device pool exhausted; raise max_loop")
