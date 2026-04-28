package mount

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/exec/exectest"
)

func TestMountBuildsArgs(t *testing.T) {
	fake := exectest.New()
	fake.Set("mount", "", nil)
	if err := New(fake).Mount(context.Background(), "/dev/loop0", "/stage/x", "ext4", []string{"noatime", "nodev"}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("calls=%d", len(fake.Calls))
	}
	got := fake.Calls[0].Args
	want := []string{"-t", "ext4", "-o", "noatime,nodev", "/dev/loop0", "/stage/x"}
	if len(got) != len(want) {
		t.Fatalf("args=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestMountNoOptions(t *testing.T) {
	fake := exectest.New()
	fake.Set("mount", "", nil)
	if err := New(fake).Mount(context.Background(), "/d", "/t", "ext4", nil); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	got := fake.Calls[0].Args
	want := []string{"-t", "ext4", "/d", "/t"}
	if len(got) != len(want) {
		t.Fatalf("args=%v want %v", got, want)
	}
}

func TestBindMountRO(t *testing.T) {
	fake := exectest.New()
	fake.Set("mount", "", nil)
	if err := New(fake).BindMount(context.Background(), "/src", "/dst", true); err != nil {
		t.Fatalf("BindMount: %v", err)
	}
	if len(fake.Calls) != 2 {
		t.Fatalf("expected 2 mount calls, got %d", len(fake.Calls))
	}
	if fake.Calls[0].Args[0] != "--bind" {
		t.Fatalf("first call: %v", fake.Calls[0].Args)
	}
	remount := fake.Calls[1].Args
	if remount[0] != "-o" || remount[1] != "remount,ro,bind" {
		t.Fatalf("remount call: %v", remount)
	}
}

func TestBindMountRW(t *testing.T) {
	fake := exectest.New()
	fake.Set("mount", "", nil)
	if err := New(fake).BindMount(context.Background(), "/src", "/dst", false); err != nil {
		t.Fatalf("BindMount: %v", err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("rw bind should be one call, got %d", len(fake.Calls))
	}
}

// TestUnmountSkipsWhenNotMounted ensures we don't shell out to umount when the
// path isn't a mount point. IsMountPoint sees a non-existent path and returns
// (false, nil), so Unmount is a no-op.
func TestUnmountSkipsWhenNotMounted(t *testing.T) {
	fake := exectest.New()
	if err := New(fake).Unmount(context.Background(), filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	for _, c := range fake.Calls {
		if c.Name == "umount" {
			t.Fatalf("umount called unexpectedly: %v", c)
		}
	}
}

// TestIsMountPointReturnsFalseForMissingPath: stat ENOENT must short-circuit
// to (false, nil).
func TestIsMountPointReturnsFalseForMissingPath(t *testing.T) {
	fake := exectest.New()
	got, err := New(fake).IsMountPoint(context.Background(), filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("IsMountPoint: %v", err)
	}
	if got {
		t.Fatal("expected false for missing path")
	}
}

// TestIsMountPointFindmntExit1: when findmnt --target returns exit 1 (not
// found / not a mount), IsMountPoint reports false without error.
func TestIsMountPointFindmntExit1(t *testing.T) {
	dir := t.TempDir()
	fake := exectest.New()
	fake.Set("findmnt", "", &fbexec.Error{Cmd: "findmnt", ExitCode: 1, Err: errors.New("exit status 1")})
	got, err := New(fake).IsMountPoint(context.Background(), dir)
	if err != nil {
		t.Fatalf("IsMountPoint: %v", err)
	}
	if got {
		t.Fatal("expected false")
	}
}
