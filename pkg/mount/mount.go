// Package mount wraps mount(8), umount(8), and findmnt(8). v1 shells out
// rather than depending on k8s.io/mount-utils to keep the dependency tree
// small; the surface we need is tiny and idempotent.
package mount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
)

type Mounter struct {
	exec fbexec.Runner
}

func New(r fbexec.Runner) *Mounter { return &Mounter{exec: r} }

// IsMountPoint returns true when target is itself a mount point. Uses
// findmnt(8) which is tolerant of bind mounts and submounts.
func (m *Mounter) IsMountPoint(ctx context.Context, target string) (bool, error) {
	if _, err := os.Stat(target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	_, err := m.exec.Run(ctx, "findmnt", "-n", "-o", "TARGET", "--target", target)
	if err == nil {
		// findmnt prints something — but `--target` will also resolve to the
		// nearest ancestor mount, so confirm it is *exactly* this target.
		out, _ := m.exec.Run(ctx, "findmnt", "-n", "-o", "TARGET", target)
		return strings.TrimSpace(out) == target, nil
	}
	var e *fbexec.Error
	if errors.As(err, &e) && e.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

// Mount runs mount(8) with the given source, target, fstype, and options.
// Caller is responsible for ensuring target exists.
func (m *Mounter) Mount(ctx context.Context, source, target, fstype string, opts []string) error {
	args := []string{"-t", fstype}
	if len(opts) > 0 {
		args = append(args, "-o", strings.Join(opts, ","))
	}
	args = append(args, source, target)
	if _, err := m.exec.Run(ctx, "mount", args...); err != nil {
		return fmt.Errorf("mount %s -> %s: %w", source, target, err)
	}
	return nil
}

// BindMount creates a bind mount from source to target, optionally read-only.
func (m *Mounter) BindMount(ctx context.Context, source, target string, readOnly bool) error {
	if _, err := m.exec.Run(ctx, "mount", "--bind", source, target); err != nil {
		return fmt.Errorf("bind mount %s -> %s: %w", source, target, err)
	}
	if readOnly {
		if _, err := m.exec.Run(ctx, "mount", "-o", "remount,ro,bind", target); err != nil {
			return fmt.Errorf("remount ro %s: %w", target, err)
		}
	}
	return nil
}

// Unmount is idempotent: a target that is not mounted returns nil.
func (m *Mounter) Unmount(ctx context.Context, target string) error {
	mounted, err := m.IsMountPoint(ctx, target)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	if _, err := m.exec.Run(ctx, "umount", target); err != nil {
		return fmt.Errorf("umount %s: %w", target, err)
	}
	return nil
}
