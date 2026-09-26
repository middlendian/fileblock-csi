package image

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
)

// Mkfs makes the ext4 filesystem every fileblock volume uses on target,
// which is either an .img file or a block device. The block size is pinned
// rather than left to mke2fs.conf, which picks 1 KiB for small filesystems
// in some configurations; it also bounds the loop sector size a volume can
// later be attached with (see loop.SectorSizeFor).
func Mkfs(ctx context.Context, r fbexec.Runner, target string) error {
	if _, err := r.Run(ctx, "mkfs.ext4", "-q", "-F",
		"-b", strconv.Itoa(SizeAlign),
		"-m", "0",
		"-E", "lazy_itable_init=1,lazy_journal_init=1",
		target); err != nil {
		return fmt.Errorf("mkfs.ext4 %s: %w", target, err)
	}
	return nil
}

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

const (
	ext4SuperblockOffset = 1024
	ext4Magic            = 0xEF53
)

// FSBlockSize reads the ext4 block size from the superblock of the image
// at path, without a shell-out. It returns 0 when there is no ext4
// superblock (a blank or encrypted image, or a corrupt one; e2fsck is what
// reports the last case).
func FSBlockSize(path string) (int, error) {
	f, err := os.Open(path) // #nosec G304 -- path is an image under our backing store
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sb := make([]byte, 64)
	if _, err := f.ReadAt(sb, ext4SuperblockOffset); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, nil
		}
		return 0, fmt.Errorf("read superblock of %s: %w", path, err)
	}
	if binary.LittleEndian.Uint16(sb[56:]) != ext4Magic {
		return 0, nil
	}
	// s_log_block_size: block size is 1024 << n; ext4 allows up to 64 KiB.
	n := binary.LittleEndian.Uint32(sb[24:])
	if n > 6 {
		return 0, nil
	}
	return 1024 << n, nil
}
