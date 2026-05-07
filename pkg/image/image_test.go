package image

import (
	"context"
	"errors"
	"os"
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

	meta, err := mgr.Create(ctx, "fb-test", cap1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if meta.CapacityBytes != cap1 || meta.FsType != DefaultFs {
		t.Fatalf("unexpected metadata %+v", meta)
	}
	if _, err := os.Stat(mgr.ImagePath("fb-test")); err != nil {
		t.Fatalf("image missing: %v", err)
	}

	// Idempotent Create returns existing.
	meta2, err := mgr.Create(ctx, "fb-test", cap1)
	if err != nil || meta2.CreatedAt != meta.CreatedAt {
		t.Fatalf("Create not idempotent: meta2=%+v err=%v", meta2, err)
	}

	// Mismatched capacity is AlreadyExists.
	if _, err := mgr.Create(ctx, "fb-test", cap2); err == nil {
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
		t.Fatalf("Resize did not update sidecar: %+v", resized)
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
