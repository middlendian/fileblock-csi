// Package crypt layers LUKS2 (dm-crypt) between a volume's loop device and
// its ext4 filesystem. It is the only package that runs cryptsetup.
package crypt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/image"
)

// MinKeyLen is the shortest accepted passphrase. It is what makes the
// cheap PBKDF below safe: 32 random bytes need no key stretching.
const MinKeyLen = 32

const (
	mapperPrefix     = "fbcrypt-"
	labelUnformatted = "fileblock-unformatted"
	labelFormatted   = "fileblock"
	blankCheckBytes  = 16 << 20 // the LUKS2 header area
	pbkdfIterations  = "1000"
)

var (
	// ErrWrongKey means neither supplied key opens the volume.
	ErrWrongKey = errors.New("no supplied key opens the volume")
	// ErrNotBlank means the device is neither LUKS nor a fresh image.
	ErrNotBlank = errors.New("not a LUKS volume and not blank; refusing to format")
)

// Format is what luksFormat is told about the cipher. It only matters on
// first stage: afterwards the LUKS header records it.
type Format struct {
	Cipher  string
	KeySize int // bits; 0 lets cryptsetup pick its default for Cipher
}

// DefaultFormat is AES-XTS with a 512-bit key (AES-256).
var DefaultFormat = Format{Cipher: "aes-xts-plain64", KeySize: 512}

// Keys are the passphrases from a volume's node-stage secret.
type Keys struct{ Current, Previous []byte }

// Outcome reports what Prepare did to the header this call, so callers can
// log rotation and format progress without Prepare taking a logger.
// OutcomeNone means the header was already in its target state.
type Outcome string

const (
	OutcomeNone                        Outcome = ""
	OutcomeFormatted                   Outcome = "formatted"
	OutcomeRotated                     Outcome = "rotated"
	OutcomeFinishedInterruptedRotation Outcome = "finished-interrupted-rotation"
)

// Crypt runs cryptsetup and reads dm state from sysfs.
type Crypt struct {
	exec    fbexec.Runner
	sysRoot string
}

func New(r fbexec.Runner) *Crypt { return NewAt(r, "/sys") }

// NewAt reads dm state under sysRoot instead of /sys; tests use it.
func NewAt(r fbexec.Runner, sysRoot string) *Crypt { return &Crypt{exec: r, sysRoot: sysRoot} }

// MapperName is deterministic and length-bounded: dm names cap at 127
// bytes and volumeIDs don't.
func MapperName(volumeID string) string {
	sum := sha256.Sum256([]byte(volumeID))
	return mapperPrefix + hex.EncodeToString(sum[:])[:24]
}

func MapperPath(name string) string { return "/dev/mapper/" + name }

// Without DM_DISABLE_UDEV libdevmapper waits on a udev cookie that never
// arrives: the node container has no udevd.
func (c *Crypt) cryptsetup(ctx context.Context, secrets [][]byte, args ...string) (string, error) {
	return c.exec.RunCmd(ctx, fbexec.Cmd{
		Name:    "cryptsetup",
		Args:    args,
		Env:     []string{"DM_DISABLE_UDEV=1"},
		Secrets: secrets,
	})
}

func exitCode(err error) int {
	var e *fbexec.Error
	if errors.As(err, &e) {
		return e.ExitCode
	}
	return -1
}

// Prepare formats dev on first use, moves its key slot to k.Current, opens
// it as name, and makes the filesystem if it has none yet. It returns the
// mapper path and what it did to the header. Every step converges if a
// previous attempt crashed midway.
func (c *Crypt) Prepare(ctx context.Context, dev, name string, k Keys, f Format) (string, Outcome, error) {
	luks, err := c.isLuks(ctx, dev)
	if err != nil {
		return "", OutcomeNone, err
	}
	outcome := OutcomeNone
	if !luks {
		// Only a freshly truncated image is formatted; a header we can't
		// read is never overwritten.
		blank, err := isBlank(dev)
		if err != nil {
			return "", OutcomeNone, err
		}
		if !blank {
			return "", OutcomeNone, fmt.Errorf("%s: %w", dev, ErrNotBlank)
		}
		args := []string{"luksFormat", "--batch-mode", "--type", "luks2", "--cipher", f.Cipher}
		if f.KeySize > 0 {
			args = append(args, "--key-size", strconv.Itoa(f.KeySize))
		}
		args = append(args, "--pbkdf", "pbkdf2", "--pbkdf-force-iterations", pbkdfIterations,
			"--label", labelUnformatted, "--key-file", fbexec.SecretFD(0), dev)
		if _, err := c.cryptsetup(ctx, [][]byte{k.Current}, args...); err != nil {
			return "", OutcomeNone, fmt.Errorf("luksFormat %s: %w", dev, err)
		}
		outcome = OutcomeFormatted
	}
	// rotate is a no-op right after a fresh format (the only slot is
	// already k.Current), so a format outcome is never overwritten.
	rotated, err := c.rotate(ctx, dev, k)
	if err != nil {
		return "", OutcomeNone, err
	}
	if outcome == OutcomeNone {
		outcome = rotated
	}
	if _, err := c.cryptsetup(ctx, [][]byte{k.Current}, "open", "--type", "luks2",
		"--disable-keyring", "--key-file", fbexec.SecretFD(0), dev, name); err != nil {
		return "", OutcomeNone, fmt.Errorf("open %s: %w", dev, err)
	}
	path := MapperPath(name)
	if err := c.ensureFilesystem(ctx, dev, path); err != nil {
		return "", OutcomeNone, errors.Join(err, c.Close(ctx, name))
	}
	return path, outcome, nil
}

// The label, not blkid, says whether mkfs is still owed: a real ext4 with
// a damaged superblock also shows no signature.
func (c *Crypt) ensureFilesystem(ctx context.Context, dev, path string) error {
	out, err := c.cryptsetup(ctx, nil, "luksDump", dev)
	if err != nil {
		return fmt.Errorf("luksDump %s: %w", dev, err)
	}
	if luksLabel(out) != labelUnformatted {
		return nil
	}
	if err := image.Mkfs(ctx, c.exec, path); err != nil {
		return err
	}
	if _, err := c.cryptsetup(ctx, nil, "config", "--label", labelFormatted, dev); err != nil {
		return fmt.Errorf("relabel %s: %w", dev, err)
	}
	return nil
}

func luksLabel(dump string) string {
	for _, line := range strings.Split(dump, "\n") {
		if v, ok := strings.CutPrefix(line, "Label:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// rotate leaves exactly the slot k.Current opens. Add-then-remove, so a
// crash between the two leaves both slots and the next call removes the
// old one.
func (c *Crypt) rotate(ctx context.Context, dev string, k Keys) (Outcome, error) {
	cur, err := c.opens(ctx, dev, k.Current)
	if err != nil {
		return OutcomeNone, err
	}
	if len(k.Previous) == 0 || bytes.Equal(k.Previous, k.Current) {
		if !cur {
			return OutcomeNone, fmt.Errorf("%s: %w", dev, ErrWrongKey)
		}
		return OutcomeNone, nil
	}
	prev, err := c.opens(ctx, dev, k.Previous)
	if err != nil {
		return OutcomeNone, err
	}
	if !cur && !prev {
		return OutcomeNone, fmt.Errorf("%s: %w", dev, ErrWrongKey)
	}
	if !prev {
		return OutcomeNone, nil
	}
	if !cur {
		if _, err := c.cryptsetup(ctx, [][]byte{k.Previous, k.Current}, "luksAddKey", "--batch-mode",
			"--pbkdf", "pbkdf2", "--pbkdf-force-iterations", pbkdfIterations,
			"--key-file", fbexec.SecretFD(0), dev, fbexec.SecretFD(1)); err != nil {
			return OutcomeNone, fmt.Errorf("luksAddKey %s: %w", dev, err)
		}
	}
	if _, err := c.cryptsetup(ctx, [][]byte{k.Previous}, "luksRemoveKey", "--batch-mode",
		"--key-file", fbexec.SecretFD(0), dev); err != nil {
		return OutcomeNone, fmt.Errorf("luksRemoveKey %s: %w", dev, err)
	}
	if cur {
		// Both slots were already open: this call only finished a
		// rotation that crashed between add and remove.
		return OutcomeFinishedInterruptedRotation, nil
	}
	return OutcomeRotated, nil
}

// opens reports whether key unlocks a slot. cryptsetup exits 2 (EPERM)
// for a wrong passphrase.
func (c *Crypt) opens(ctx context.Context, dev string, key []byte) (bool, error) {
	_, err := c.cryptsetup(ctx, [][]byte{key}, "open", "--test-passphrase", "--type", "luks2",
		"--key-file", fbexec.SecretFD(0), dev)
	switch {
	case err == nil:
		return true, nil
	case exitCode(err) == 2:
		return false, nil
	default:
		return false, fmt.Errorf("test passphrase on %s: %w", dev, err)
	}
}

// isLuks: cryptsetup exits 1 (EINVAL) for a device with no LUKS header.
func (c *Crypt) isLuks(ctx context.Context, dev string) (bool, error) {
	_, err := c.cryptsetup(ctx, nil, "isLuks", dev)
	switch {
	case err == nil:
		return true, nil
	case exitCode(err) == 1:
		return false, nil
	default:
		return false, fmt.Errorf("isLuks %s: %w", dev, err)
	}
}

func isBlank(dev string) (bool, error) {
	f, err := os.Open(dev) // #nosec G304 -- dev is the loop device this stage attached
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for read := 0; read < blankCheckBytes; {
		n, err := f.Read(buf[:min(len(buf), blankCheckBytes-read)])
		if bytes.ContainsFunc(buf[:n], func(r rune) bool { return r != 0 }) {
			return false, nil
		}
		read += n
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, fmt.Errorf("read %s: %w", dev, err)
		}
	}
	return true, nil
}

// Close is idempotent: cryptsetup exits 4 (ENODEV) for an inactive name.
func (c *Crypt) Close(ctx context.Context, name string) error {
	_, err := c.cryptsetup(ctx, nil, "close", name)
	if err == nil || exitCode(err) == 4 {
		return nil
	}
	return fmt.Errorf("close %s: %w", name, err)
}

// Resize grows an open mapping to its backing device. It needs no key
// because Prepare opens with --disable-keyring.
func (c *Crypt) Resize(ctx context.Context, name string) error {
	if _, err := c.cryptsetup(ctx, nil, "resize", name); err != nil {
		return fmt.Errorf("resize %s: %w", name, err)
	}
	return nil
}
