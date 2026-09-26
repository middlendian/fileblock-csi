package crypt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/exec/exectest"
)

var (
	keyA = []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	keyB = []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	keyC = []byte("cccccccccccccccccccccccccccccccccccccccc")
)

// fakeLUKS models the header state cryptsetup would mutate, so tests
// assert outcomes (which slots exist, the label) rather than scripts.
type fakeLUKS struct {
	t      *testing.T
	luks   bool
	label  string
	slots  [][]byte
	opened bool
	subs   []string   // cryptsetup subcommands (and "mkfs") in call order
	argv   [][]string // full argv for each entry in subs, index-aligned
}

func exitErr(code int) error { return &fbexec.Error{Cmd: "cryptsetup", ExitCode: code} }

func argAfter(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func (f *fakeLUKS) has(k []byte) bool {
	return slices.ContainsFunc(f.slots, func(s []byte) bool { return bytes.Equal(s, k) })
}

// argvFor returns the full argv of the first call to sub, or nil.
func (f *fakeLUKS) argvFor(sub string) []string {
	i := slices.Index(f.subs, sub)
	if i < 0 {
		return nil
	}
	return f.argv[i]
}

func (f *fakeLUKS) run(_ context.Context, c fbexec.Cmd) (string, error) {
	for _, s := range c.Secrets {
		for _, a := range c.Args {
			if strings.Contains(a, string(s)) {
				f.t.Fatalf("secret leaked into argv: %v", c.Args)
			}
		}
	}
	if c.Name == "mkfs.ext4" {
		f.subs = append(f.subs, "mkfs")
		f.argv = append(f.argv, append([]string(nil), c.Args...))
		return "", nil
	}
	if c.Name != "cryptsetup" {
		return "", fmt.Errorf("unexpected command %s", c.Name)
	}
	if !slices.Contains(c.Env, "DM_DISABLE_UDEV=1") {
		f.t.Fatalf("cryptsetup %v without DM_DISABLE_UDEV=1", c.Args)
	}
	sub := c.Args[0]
	f.subs = append(f.subs, sub)
	f.argv = append(f.argv, append([]string(nil), c.Args...))
	switch sub {
	case "isLuks":
		if f.luks {
			return "", nil
		}
		return "", exitErr(1)
	case "luksFormat":
		f.luks, f.label, f.slots = true, argAfter(c.Args, "--label"), [][]byte{c.Secrets[0]}
	case "open":
		if !f.has(c.Secrets[0]) {
			return "", exitErr(2)
		}
		if !slices.Contains(c.Args, "--test-passphrase") {
			f.opened = true
		}
	case "luksAddKey":
		if !f.has(c.Secrets[0]) {
			return "", exitErr(2)
		}
		f.slots = append(f.slots, c.Secrets[1])
	case "luksRemoveKey":
		i := slices.IndexFunc(f.slots, func(s []byte) bool { return bytes.Equal(s, c.Secrets[0]) })
		if i < 0 {
			return "", exitErr(2)
		}
		f.slots = slices.Delete(f.slots, i, i+1)
	case "luksDump":
		return "LUKS header information\nVersion:       \t2\nLabel:          " + f.label + "\nSubsystem:      (no subsystem)\n", nil
	case "config":
		f.label = argAfter(c.Args, "--label")
	case "close":
		if !f.opened {
			return "", exitErr(4)
		}
		f.opened = false
	case "resize":
	default:
		return "", fmt.Errorf("unexpected cryptsetup %s", sub)
	}
	return "", nil
}

func newFake(t *testing.T, f *fakeLUKS) *Crypt {
	f.t = t
	r := exectest.New()
	r.CmdFunc = f.run
	r.Func = func(ctx context.Context, name string, args ...string) (string, error) {
		return f.run(ctx, fbexec.Cmd{Name: name, Args: args})
	}
	return NewAt(r, t.TempDir())
}

// blankDev is a sparse all-zero file standing in for a fresh loop device.
func blankDev(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dev")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(32 << 20); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPrepareFormatsBlankDevice(t *testing.T) {
	f := &fakeLUKS{}
	c := newFake(t, f)
	dev := blankDev(t)
	path, err := c.Prepare(context.Background(), dev, "fbcrypt-x", Keys{Current: keyA})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if path != "/dev/mapper/fbcrypt-x" {
		t.Fatalf("path = %q", path)
	}
	if !f.opened || f.label != "fileblock" || len(f.slots) != 1 || !f.has(keyA) {
		t.Fatalf("state after first stage: %+v", f)
	}
	if !slices.Contains(f.subs, "luksFormat") || !slices.Contains(f.subs, "mkfs") {
		t.Fatalf("subs = %v", f.subs)
	}
	// Review finding 2: the PBKDF hardening and label are exact, not just
	// present — a dropped flag would silently fall back to cryptsetup's
	// argon2id default, which is too expensive for a node plugin.
	want := []string{"luksFormat", "--batch-mode", "--type", "luks2", "--cipher", "aes-xts-plain64",
		"--key-size", "512", "--pbkdf", "pbkdf2", "--pbkdf-force-iterations", "1000",
		"--label", "fileblock-unformatted", "--key-file", "/dev/fd/3", dev}
	if got := f.argvFor("luksFormat"); !slices.Equal(got, want) {
		t.Fatalf("luksFormat argv = %v, want %v", got, want)
	}
}

func TestPrepareRefusesNonBlankNonLUKS(t *testing.T) {
	f := &fakeLUKS{}
	c := newFake(t, f)
	dev := blankDev(t)
	fh, _ := os.OpenFile(dev, os.O_WRONLY, 0)
	_, _ = fh.WriteAt([]byte{0x53}, 1080) // where an ext4 magic would sit
	_ = fh.Close()
	_, err := c.Prepare(context.Background(), dev, "fbcrypt-x", Keys{Current: keyA})
	if !errors.Is(err, ErrNotBlank) {
		t.Fatalf("err = %v, want ErrNotBlank", err)
	}
	if slices.Contains(f.subs, "luksFormat") {
		t.Fatal("formatted a non-blank device")
	}
}

// A crash between luksFormat and mkfs leaves the unformatted label; the
// next stage must finish the job.
func TestPrepareUnformattedLabelRunsMkfs(t *testing.T) {
	f := &fakeLUKS{luks: true, label: "fileblock-unformatted", slots: [][]byte{keyA}}
	c := newFake(t, f)
	if _, err := c.Prepare(context.Background(), "/dev/loop9", "fbcrypt-x", Keys{Current: keyA}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !slices.Contains(f.subs, "mkfs") || f.label != "fileblock" {
		t.Fatalf("subs=%v label=%q", f.subs, f.label)
	}
}

func TestPrepareFormattedSkipsMkfs(t *testing.T) {
	f := &fakeLUKS{luks: true, label: "fileblock", slots: [][]byte{keyA}}
	c := newFake(t, f)
	if _, err := c.Prepare(context.Background(), "/dev/loop9", "fbcrypt-x", Keys{Current: keyA}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if slices.Contains(f.subs, "mkfs") || slices.Contains(f.subs, "luksFormat") {
		t.Fatalf("subs = %v", f.subs)
	}
}

func TestPrepareRotatesPreviousToCurrent(t *testing.T) {
	f := &fakeLUKS{luks: true, label: "fileblock", slots: [][]byte{keyA}}
	c := newFake(t, f)
	if _, err := c.Prepare(context.Background(), "/dev/loop9", "fbcrypt-x", Keys{Current: keyB, Previous: keyA}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(f.slots) != 1 || !f.has(keyB) {
		t.Fatalf("slots after rotation: %q", f.slots)
	}
	add, rm := slices.Index(f.subs, "luksAddKey"), slices.Index(f.subs, "luksRemoveKey")
	if add < 0 || rm < 0 || add > rm {
		t.Fatalf("want luksAddKey before luksRemoveKey, subs = %v", f.subs)
	}
	// Review finding 2: same PBKDF hardening as luksFormat, plus the two
	// key fds (old key authorizes, new key is added) in order.
	want := []string{"luksAddKey", "--batch-mode", "--pbkdf", "pbkdf2", "--pbkdf-force-iterations", "1000",
		"--key-file", "/dev/fd/3", "/dev/loop9", "/dev/fd/4"}
	if got := f.argvFor("luksAddKey"); !slices.Equal(got, want) {
		t.Fatalf("luksAddKey argv = %v, want %v", got, want)
	}
}

func TestPrepareFinishesInterruptedRotation(t *testing.T) {
	f := &fakeLUKS{luks: true, label: "fileblock", slots: [][]byte{keyA, keyB}}
	c := newFake(t, f)
	if _, err := c.Prepare(context.Background(), "/dev/loop9", "fbcrypt-x", Keys{Current: keyB, Previous: keyA}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(f.slots) != 1 || !f.has(keyB) || slices.Contains(f.subs, "luksAddKey") {
		t.Fatalf("slots=%q subs=%v", f.slots, f.subs)
	}
}

// Review Focus 2: previousKey left in the Secret after rotation finished.
func TestPrepareStalePreviousIsIgnored(t *testing.T) {
	f := &fakeLUKS{luks: true, label: "fileblock", slots: [][]byte{keyB}}
	c := newFake(t, f)
	if _, err := c.Prepare(context.Background(), "/dev/loop9", "fbcrypt-x", Keys{Current: keyB, Previous: keyA}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(f.slots) != 1 || !f.has(keyB) {
		t.Fatalf("slots = %q", f.slots)
	}
	if slices.Contains(f.subs, "luksAddKey") || slices.Contains(f.subs, "luksRemoveKey") {
		t.Fatalf("header touched: %v", f.subs)
	}
}

func TestPrepareWrongKey(t *testing.T) {
	for _, k := range []Keys{{Current: keyB, Previous: keyA}, {Current: keyB}} {
		f := &fakeLUKS{luks: true, label: "fileblock", slots: [][]byte{keyC}}
		c := newFake(t, f)
		_, err := c.Prepare(context.Background(), "/dev/loop9", "fbcrypt-x", k)
		if !errors.Is(err, ErrWrongKey) {
			t.Fatalf("err = %v, want ErrWrongKey", err)
		}
		if f.opened || len(f.slots) != 1 || !f.has(keyC) {
			t.Fatalf("state changed on wrong key: %+v", f)
		}
	}
}

func TestPreparePreviousEqualsCurrent(t *testing.T) {
	f := &fakeLUKS{luks: true, label: "fileblock", slots: [][]byte{keyA}}
	c := newFake(t, f)
	if _, err := c.Prepare(context.Background(), "/dev/loop9", "fbcrypt-x", Keys{Current: keyA, Previous: keyA}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(f.slots) != 1 || slices.Contains(f.subs, "luksRemoveKey") {
		t.Fatalf("slots=%q subs=%v", f.slots, f.subs)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	f := &fakeLUKS{}
	c := newFake(t, f)
	if err := c.Close(context.Background(), "fbcrypt-x"); err != nil {
		t.Fatalf("Close of a closed mapping: %v", err)
	}
}

func TestMapperName(t *testing.T) {
	long := strings.Repeat("v", 300)
	a, b := MapperName("fb-abc-vol1"), MapperName("fb-abc-vol2")
	if a == b || a != MapperName("fb-abc-vol1") {
		t.Fatalf("not deterministic/unique: %s %s", a, b)
	}
	if !strings.HasPrefix(a, "fbcrypt-") || len(a) != len("fbcrypt-")+24 || len(MapperName(long)) > 127 {
		t.Fatalf("bad name %q", a)
	}
	if MapperPath(a) != "/dev/mapper/"+a {
		t.Fatalf("MapperPath = %q", MapperPath(a))
	}
}
