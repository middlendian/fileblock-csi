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
	"strings"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
)

type Losetup struct{ exec fbexec.Runner }

func NewLosetup(r fbexec.Runner) *Losetup { return &Losetup{exec: r} }

// Attach calls `losetup --find --show <imagePath>` and returns the chosen
// /dev/loopN. Returns ErrPoolExhausted when the kernel has no free loop
// devices (LOOP_CTL_GET_FREE returns ENOSPC).
func (l *Losetup) Attach(ctx context.Context, imagePath string) (string, error) {
	out, err := l.exec.Run(ctx, "losetup", "--find", "--show", imagePath)
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

// List returns every currently-attached loop device.
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
	return wire.Loopdevices, nil
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
