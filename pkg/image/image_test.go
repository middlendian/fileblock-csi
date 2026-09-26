package image

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/exec/exectest"
)

// TestRoundTrip exercises Create → Get → List → Resize → Delete against the
// real OS. Requires mkfs.ext4 (e2fsprogs); skips if unavailable.
func TestRoundTrip(t *testing.T) {
	if _, err := os.Stat("/usr/sbin/mkfs.ext4"); errors.Is(err, os.ErrNotExist) {
		t.Skip("mkfs.ext4 not available")
	}

	root := t.TempDir()
	mgr, err := New(root, fbexec.New(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	const cap1 = 8 * 1024 * 1024  // 8 MiB
	const cap2 = 16 * 1024 * 1024 // 16 MiB

	meta, err := mgr.Create(ctx, "fb-test", cap1, CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if meta.CapacityBytes != cap1 || meta.VolumeID != "fb-test" {
		t.Fatalf("unexpected metadata %+v", meta)
	}
	imgPath := mgr.ImagePath("fb-test")
	st1, err := os.Stat(imgPath)
	if err != nil {
		t.Fatalf("image missing: %v", err)
	}

	// Idempotent Create adopts the existing .img without rewriting it.
	meta2, err := mgr.Create(ctx, "fb-test", cap1, CreateOptions{})
	if err != nil {
		t.Fatalf("Create not idempotent: %v", err)
	}
	if meta2.CapacityBytes != cap1 {
		t.Fatalf("Create returned wrong capacity: %+v", meta2)
	}
	st2, err := os.Stat(imgPath)
	if err != nil {
		t.Fatalf("image missing after re-Create: %v", err)
	}
	if !st2.ModTime().Equal(st1.ModTime()) {
		t.Fatalf("re-Create rewrote the .img (mtime %v -> %v)", st1.ModTime(), st2.ModTime())
	}

	// Mismatched capacity is AlreadyExists.
	if _, err := mgr.Create(ctx, "fb-test", cap2, CreateOptions{}); err == nil {
		t.Fatal("expected CapacityMismatchError")
	} else {
		var m *CapacityMismatchError
		if !errors.As(err, &m) {
			t.Fatalf("wanted CapacityMismatchError, got %T: %v", err, err)
		}
	}

	list, err := mgr.List(ctx)
	if err != nil || len(list) != 1 || list[0].VolumeID != "fb-test" {
		t.Fatalf("List wrong: %v err=%v", list, err)
	}

	// Resize grows.
	resized, err := mgr.Resize(ctx, "fb-test", cap2)
	if err != nil {
		t.Fatalf("Resize grow: %v", err)
	}
	if resized.CapacityBytes != cap2 {
		t.Fatalf("Resize did not update size: %+v", resized)
	}
	stResized, err := os.Stat(imgPath)
	if err != nil {
		t.Fatalf("image missing after Resize: %v", err)
	}
	if stResized.Size() != cap2 {
		t.Fatalf("on-disk size %d != %d", stResized.Size(), cap2)
	}

	// Resize refuses to shrink.
	if _, err := mgr.Resize(ctx, "fb-test", cap1); err == nil {
		t.Fatal("expected shrink to be refused")
	}

	if err := mgr.Delete(ctx, "fb-test"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Idempotent delete.
	if err := mgr.Delete(ctx, "fb-test"); err != nil {
		t.Fatalf("Delete not idempotent: %v", err)
	}
}

// TestCreateAdoptsExistingImage verifies the idempotency contract: if a
// .img with the requested size already exists, Create returns it as-is and
// does not re-run mkfs (which would erase the filesystem). The fake runner
// will fail the test if mkfs.ext4 is invoked.
func TestCreateAdoptsExistingImage(t *testing.T) {
	root := t.TempDir()
	const size = 4 * 1024 * 1024
	imgPath := root + "/fb-pre.img"
	f, err := os.OpenFile(imgPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	f.Close()

	fake := exectest.New() // any Run call will fail with "unexpected call"
	mgr, err := New(root, fake)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	meta, err := mgr.Create(context.Background(), "fb-pre", size, CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if meta.CapacityBytes != size || meta.VolumeID != "fb-pre" {
		t.Fatalf("unexpected metadata %+v", meta)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("Create should not shell out when adopting; got %v", fake.Calls)
	}
}

// TestCreateMismatchOnDiskSize verifies that an existing .img whose size
// disagrees with the requested capacity surfaces as CapacityMismatchError —
// no silent re-mkfs (which would wipe the filesystem).
func TestCreateMismatchOnDiskSize(t *testing.T) {
	root := t.TempDir()
	imgPath := root + "/fb-mm.img"
	f, err := os.OpenFile(imgPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	if err := f.Truncate(4 * 1024 * 1024); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	f.Close()

	fake := exectest.New()
	mgr, err := New(root, fake)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = mgr.Create(context.Background(), "fb-mm", 8*1024*1024, CreateOptions{})
	var mm *CapacityMismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("want CapacityMismatchError, got %T: %v", err, err)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("mismatch should not shell out; got %v", fake.Calls)
	}
}

// TestGetMissingImage: Get on an absent volume returns a fs.ErrNotExist
// so the controller can tell apart "not provisioned" from other errors.
func TestGetMissingImage(t *testing.T) {
	root := t.TempDir()
	mgr, err := New(root, exectest.New())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = mgr.Get(context.Background(), "fb-missing")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}

func TestNewRejectsNonExistentRoot(t *testing.T) {
	if _, err := New("/this/path/should/not/exist", fbexec.New(0)); err == nil {
		t.Fatal("expected error for missing dir")
	}
}

func TestNewRejectsFile(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/notadir"
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(f, fbexec.New(0)); err == nil {
		t.Fatal("expected error for file path")
	}
}

func TestFsckSuccess(t *testing.T) {
	fake := exectest.New()
	fake.Set("e2fsck", "", nil)
	if err := Fsck(context.Background(), fake, "/dev/loop0"); err != nil {
		t.Fatalf("Fsck: %v", err)
	}
}

func TestFsckExit1IsClean(t *testing.T) {
	fake := exectest.New()
	fake.Set("e2fsck", "fixed", &fbexec.Error{Cmd: "e2fsck", ExitCode: 1, Err: errors.New("exit status 1")})
	if err := Fsck(context.Background(), fake, "/dev/loop0"); err != nil {
		t.Fatalf("Fsck exit 1 should be success, got %v", err)
	}
}

func TestFsckFatalExit(t *testing.T) {
	fake := exectest.New()
	fake.Set("e2fsck", "broken", &fbexec.Error{Cmd: "e2fsck", ExitCode: 8, Err: errors.New("exit status 8")})
	if err := Fsck(context.Background(), fake, "/dev/loop0"); err == nil {
		t.Fatal("expected error for fatal exit code")
	}
}

func TestResize2fs(t *testing.T) {
	fake := exectest.New()
	fake.Set("resize2fs", "", nil)
	if err := Resize2fs(context.Background(), fake, "/dev/loop0"); err != nil {
		t.Fatalf("Resize2fs: %v", err)
	}
}

func TestResize2fsFails(t *testing.T) {
	fake := exectest.New()
	fake.Set("resize2fs", "boom", errors.New("nope"))
	if err := Resize2fs(context.Background(), fake, "/dev/loop0"); err == nil {
		t.Fatal("expected error")
	}
}

func TestCapacityMismatchErrorMessage(t *testing.T) {
	e := &CapacityMismatchError{Requested: 100, Existing: 200}
	if e.Error() == "" {
		t.Fatal("empty Error()")
	}
}

func TestMkfsArgs(t *testing.T) {
	fake := exectest.New()
	fake.SetDefault("", nil)
	if err := Mkfs(context.Background(), fake, "/dev/mapper/fbcrypt-x"); err != nil {
		t.Fatalf("Mkfs: %v", err)
	}
	want := []string{"-q", "-F", "-b", strconv.Itoa(DefaultBlockSize), "-m", "0", "-E", "lazy_itable_init=1,lazy_journal_init=1", "/dev/mapper/fbcrypt-x"}
	if len(fake.Calls) != 1 || fake.Calls[0].Name != "mkfs.ext4" || !slices.Equal(fake.Calls[0].Args, want) {
		t.Fatalf("calls = %+v", fake.Calls)
	}
}

// Encrypted volumes are formatted by the node inside the LUKS mapping, so
// the controller must leave the sparse file exactly as truncated: all
// zeros, which is what the node's blank-header check relies on.
func TestCreateUnformattedSkipsMkfs(t *testing.T) {
	fake := exectest.New() // any call fails the test via "unexpected call"
	mgr, err := New(t.TempDir(), fake)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := mgr.Create(context.Background(), "fb-enc", 32<<20, CreateOptions{Unformatted: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if meta.CapacityBytes != 32<<20 {
		t.Fatalf("capacity %d", meta.CapacityBytes)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("unexpected shell-outs: %+v", fake.Calls)
	}
	st, err := os.Stat(mgr.ImagePath("fb-enc"))
	if err != nil || st.Size() != 32<<20 {
		t.Fatalf("stat: %v size=%v", err, st)
	}
}

func TestValidateVolumeID(t *testing.T) {
	cases := []struct {
		in  string
		bad bool
	}{
		{"", true},
		{"foo/bar", true},
		{"a\x00b", true},
		{"fb-1234", false},
	}
	for _, c := range cases {
		err := validateVolumeID(c.in)
		if (err != nil) != c.bad {
			t.Errorf("validateVolumeID(%q) err=%v, wantBad=%v", c.in, err, c.bad)
		}
	}
}

func newUnformattedMgr(t *testing.T) Manager {
	t.Helper()
	mgr, err := New(t.TempDir(), exectest.New()) // no shell-outs expected
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}

func fileSize(t *testing.T, mgr Manager, id string) int64 {
	t.Helper()
	st, err := os.Stat(mgr.ImagePath(id))
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

// A DefaultBlockSize LUKS2 sector needs the image to be whole sectors, and
// a PVC can ask for any byte count ("1G" is 10^9). Create rounds up; CSI
// allows returning more than was required.
func TestCreateRoundsUpToAlignment(t *testing.T) {
	mgr := newUnformattedMgr(t)
	ctx := context.Background()
	const req = 32<<20 + 1
	const want = 32<<20 + DefaultBlockSize
	meta, err := mgr.Create(ctx, "fb-odd", req, CreateOptions{Unformatted: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if meta.CapacityBytes != want || fileSize(t, mgr, "fb-odd") != want {
		t.Fatalf("capacity %d, file %d, want %d", meta.CapacityBytes, fileSize(t, mgr, "fb-odd"), want)
	}
	// A retried CreateVolume with the original request adopts the image.
	again, err := mgr.Create(ctx, "fb-odd", req, CreateOptions{Unformatted: true})
	if err != nil || again.CapacityBytes != want {
		t.Fatalf("retry: %+v, %v", again, err)
	}
}

// Images created before rounding existed may be unaligned; a retried
// CreateVolume for one must still adopt it rather than report a mismatch.
func TestCreateAdoptsPreexistingUnalignedImage(t *testing.T) {
	mgr := newUnformattedMgr(t)
	const size = 10_000_001
	if err := truncateSparse(mgr.ImagePath("fb-old"), size); err != nil {
		t.Fatal(err)
	}
	meta, err := mgr.Create(context.Background(), "fb-old", size, CreateOptions{})
	if err != nil || meta.CapacityBytes != size {
		t.Fatalf("got %+v, %v; want adoption at %d", meta, err, size)
	}
}

// ControllerExpandVolume can't tell whether a volume is encrypted, so
// every resize rounds up, and the resizer's retry with the original
// request must be a no-op rather than a "shrink".
func TestResizeRoundsUpAndRetryIsNoop(t *testing.T) {
	mgr := newUnformattedMgr(t)
	ctx := context.Background()
	if _, err := mgr.Create(ctx, "fb-grow", 32<<20, CreateOptions{Unformatted: true}); err != nil {
		t.Fatal(err)
	}
	const req = 40_000_001
	const want = 40_001_536 // 9766 blocks of 4 KiB
	meta, err := mgr.Resize(ctx, "fb-grow", req)
	if err != nil || meta.CapacityBytes != want || fileSize(t, mgr, "fb-grow") != want {
		t.Fatalf("Resize: %+v, %v, file %d; want %d", meta, err, fileSize(t, mgr, "fb-grow"), want)
	}
	if meta, err := mgr.Resize(ctx, "fb-grow", req); err != nil || meta.CapacityBytes != want {
		t.Fatalf("retry: %+v, %v", meta, err)
	}
	if _, err := mgr.Resize(ctx, "fb-grow", 32<<20); err == nil {
		t.Fatal("expected a real shrink to be refused")
	}
}

// writeSuperblock writes just enough of an ext4 superblock for
// FSBlockSize: the magic and s_log_block_size.
func writeSuperblock(t *testing.T, path string, size int64, logBlockSize uint32) {
	t.Helper()
	if err := truncateSparse(path, size); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sb := make([]byte, 1024)
	binary.LittleEndian.PutUint32(sb[24:], logBlockSize)
	binary.LittleEndian.PutUint16(sb[56:], 0xEF53)
	if _, err := f.WriteAt(sb, 1024); err != nil {
		t.Fatal(err)
	}
}

func TestFSBlockSize(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		log  uint32
		want int
	}{{"4k", 2, 4096}, {"1k", 0, 1024}, {"2k", 1, 2048}} {
		p := filepath.Join(dir, tc.name+".img")
		writeSuperblock(t, p, 1<<20, tc.log)
		if got, err := FSBlockSize(p); err != nil || got != tc.want {
			t.Errorf("%s: got %d, %v; want %d", tc.name, got, err, tc.want)
		}
	}
	blank := filepath.Join(dir, "blank.img")
	if err := truncateSparse(blank, 1<<20); err != nil {
		t.Fatal(err)
	}
	if got, err := FSBlockSize(blank); err != nil || got != 0 {
		t.Errorf("blank image: got %d, %v; want 0 (no ext4 magic)", got, err)
	}
	short := filepath.Join(dir, "short.img")
	if err := truncateSparse(short, 512); err != nil {
		t.Fatal(err)
	}
	if got, err := FSBlockSize(short); err != nil || got != 0 {
		t.Errorf("short image: got %d, %v; want 0", got, err)
	}
	if _, err := FSBlockSize(filepath.Join(dir, "missing.img")); err == nil {
		t.Error("missing image: expected an error")
	}
}

// A real mkfs.ext4 through Mkfs yields 4 KiB blocks even for a small
// filesystem, where some mke2fs configs would pick 1 KiB.
func TestMkfsPinsBlockSize(t *testing.T) {
	if _, err := osexec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not installed")
	}
	p := filepath.Join(t.TempDir(), "small.img")
	if err := truncateSparse(p, 8<<20); err != nil {
		t.Fatal(err)
	}
	if err := Mkfs(context.Background(), fbexec.New(0), p); err != nil {
		t.Fatal(err)
	}
	if got, err := FSBlockSize(p); err != nil || got != DefaultBlockSize {
		t.Fatalf("got %d, %v; want %d", got, err, DefaultBlockSize)
	}
}
