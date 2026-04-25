package image

import (
	"context"
	"errors"
	"fmt"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
)

// Fsck runs `e2fsck -p` on the given block device. Exit codes 0 (clean) and 1
// (errors corrected) are treated as success. Anything >= 2 is fatal — callers
// should detach the loop device and surface the error.
func Fsck(ctx context.Context, r fbexec.Runner, dev string) error {
	out, err := r.Run(ctx, "e2fsck", "-p", "-f", dev)
	if err == nil {
		return nil
	}
	var e *fbexec.Error
	if errors.As(err, &e) && e.ExitCode == 1 {
		return nil
	}
	return fmt.Errorf("e2fsck %s: %s: %w", dev, out, err)
}

// Resize2fs grows the ext4 filesystem on dev to fill the underlying block
// device. Must be called after Fsck.
func Resize2fs(ctx context.Context, r fbexec.Runner, dev string) error {
	if _, err := r.Run(ctx, "resize2fs", dev); err != nil {
		return fmt.Errorf("resize2fs %s: %w", dev, err)
	}
	return nil
}
