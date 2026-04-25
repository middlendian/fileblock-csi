// Package flock holds an OS-level advisory lock on a file for the lifetime of
// a Handle. It is the defense-in-depth that prevents the same .img from being
// loop-mounted on two nodes at once even if CSI topology pinning fails.
package flock

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Handle owns the open fd and the kernel-side advisory lock. Close releases
// both. Callers must call Close on shutdown or unstage.
type Handle struct {
	f *os.File
}

// TryLock opens path and acquires an exclusive non-blocking flock. If another
// process (typically a node plugin on a different host sharing the backing
// store) already holds the lock, returns ErrLocked.
func TryLock(path string) (*Handle, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("flock: open %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if err == unix.EWOULDBLOCK {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("flock: lock %s: %w", path, err)
	}
	return &Handle{f: f}, nil
}

// Close releases the advisory lock and the underlying fd. Safe to call on a
// nil receiver.
func (h *Handle) Close() error {
	if h == nil || h.f == nil {
		return nil
	}
	_ = unix.Flock(int(h.f.Fd()), unix.LOCK_UN)
	err := h.f.Close()
	h.f = nil
	return err
}

// ErrLocked is returned when another process already holds the advisory lock.
var ErrLocked = fmt.Errorf("flock: file is locked by another process")
