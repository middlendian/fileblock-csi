package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/exec/exectest"
	"github.com/middlendian/fileblock-csi/pkg/mount"
)

func TestRegistryGetMountsOnce(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	p1, err := reg.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Get #1: %v", err)
	}
	p2, err := reg.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Get #2: %v", err)
	}
	if p1 != p2 {
		t.Errorf("path mismatch: %q vs %q", p1, p2)
	}
	if p1 != filepath.Join(root, cfg.ID()) {
		t.Errorf("path = %q, want %q", p1, filepath.Join(root, cfg.ID()))
	}
	mountCalls := 0
	for _, c := range fake.Calls {
		if c.Name == "mount" {
			mountCalls++
		}
	}
	if mountCalls != 1 {
		t.Errorf("mount called %d times, want 1", mountCalls)
	}
}

func TestRegistryDistinctConfigsMountSeparately(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)
	a := Config{Type: TypeNFS, NFSServer: "s1", NFSPath: "/p"}
	b := Config{Type: TypeNFS, NFSServer: "s2", NFSPath: "/p"}
	pa, _ := reg.Get(context.Background(), a)
	pb, _ := reg.Get(context.Background(), b)
	if pa == pb {
		t.Errorf("distinct configs returned same path: %q", pa)
	}
}

func TestRegistryConcurrentGetSerializes(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reg.Get(context.Background(), cfg); err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}
	wg.Wait()
	mountCalls := 0
	for _, c := range fake.Calls {
		if c.Name == "mount" {
			mountCalls++
		}
	}
	if mountCalls != 1 {
		t.Errorf("under concurrent Get, mount called %d times, want 1", mountCalls)
	}
}

func TestRegistryRejectsUnknownType(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)
	_, err := reg.Get(context.Background(), Config{Type: "smb"})
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestRegistryConfigByStoreID(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	if _, ok := reg.ConfigByStoreID(cfg.ID()); ok {
		t.Fatal("ConfigByStoreID returned true before any Get")
	}
	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	got, ok := reg.ConfigByStoreID(cfg.ID())
	if !ok {
		t.Fatal("ConfigByStoreID returned false after Get")
	}
	if got != cfg {
		t.Errorf("config mismatch: got %+v, want %+v", got, cfg)
	}
}

func TestRegistryMountedPaths(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)

	if paths := reg.MountedPaths(); len(paths) != 0 {
		t.Fatalf("MountedPaths before any Get = %v, want empty", paths)
	}

	cfgA := Config{Type: TypeNFS, NFSServer: "s1", NFSPath: "/p"}
	cfgB := Config{Type: TypeNFS, NFSServer: "s2", NFSPath: "/p"}
	pA, _ := reg.Get(context.Background(), cfgA)
	pB, _ := reg.Get(context.Background(), cfgB)

	paths := reg.MountedPaths()
	if len(paths) != 2 {
		t.Fatalf("MountedPaths = %v, want 2 entries", paths)
	}
	got := map[string]bool{paths[0]: true, paths[1]: true}
	if !got[pA] || !got[pB] {
		t.Errorf("MountedPaths = %v, want {%q, %q}", paths, pA, pB)
	}
}

func TestRegistryAdoptExistingNoOpOnEmptyRoot(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)
	if err := reg.AdoptExisting(context.Background()); err != nil {
		t.Fatalf("AdoptExisting: %v", err)
	}
	if len(reg.MountedPaths()) != 0 {
		t.Errorf("expected empty mounted set, got %v", reg.MountedPaths())
	}
}

func TestRegistryAdoptExistingPreloadsKnownDirs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "abc123def456")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := exectest.New()
	fake.Set("findmnt", dir, nil)
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)
	if err := reg.AdoptExisting(context.Background()); err != nil {
		t.Fatalf("AdoptExisting: %v", err)
	}
	got := reg.MountedPaths()
	if len(got) != 1 || got[0] != dir {
		t.Errorf("MountedPaths = %v", got)
	}
}

// TestRegistryAdoptExistingSkipsNonStoreIDDirs guards against false
// adoption when the operator chooses a local-backing source path that
// happens to live under stores-root. AdoptExisting must only adopt
// directories whose name matches the storeID pattern (12 lowercase hex
// chars).
func TestRegistryAdoptExistingSkipsNonStoreIDDirs(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{
		"local",               // bare word
		"FOO",                 // uppercase
		"abc123",              // too short
		"abc123def4567",       // too long
		"abc123def45z",        // not hex
		"abcdefabcdef.bak",    // extra chars
		"abcdefabcdef-suffix", // hyphen
	} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// And one valid storeID-looking dir to make sure adoption still works.
	validDir := filepath.Join(root, "0123456789ab")
	if err := os.MkdirAll(validDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := exectest.New()
	fake.Set("findmnt", validDir, nil)
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)
	if err := reg.AdoptExisting(context.Background()); err != nil {
		t.Fatalf("AdoptExisting: %v", err)
	}
	got := reg.MountedPaths()
	if len(got) != 1 || got[0] != validDir {
		t.Errorf("MountedPaths = %v; want exactly the one storeID-shaped dir", got)
	}
}

func TestRegistryDoesNotCacheOnMountFailure(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	// FakeRunner.Func runs in place of the rules table when set. It must
	// not touch fake.Calls — FakeRunner.Run already records the call
	// under its own mutex before invoking Func.
	var calls atomic.Int32
	fake.Func = func(ctx context.Context, name string, args ...string) (string, error) {
		n := calls.Add(1)
		if n == 1 {
			return "boom", errors.New("mount failed")
		}
		return "", nil
	}
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	if _, err := reg.Get(context.Background(), cfg); err == nil {
		t.Fatal("expected first Get to fail")
	}
	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatalf("expected second Get to succeed, got %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("mount called %d times, want 2 (first failed, second retried)", calls.Load())
	}
}

// TestRegistryAdoptExistingSkipsNonMountedDirs verifies the fix for the
// emptyDir cache-poisoning bug: a storeID-shaped directory that exists
// but is not currently a mountpoint must NOT be adopted. Otherwise a
// stale leftover from a prior container run causes Get to short-circuit
// and NodeStageVolume to fail with NotFound on the .img file. The
// fixture wires findmnt to return exit 1 (the "not a mountpoint"
// sentinel that pkg/mount.Mounter.IsMountPoint recognizes) and a stub
// for the real mount(8) call that the recovery path needs.
func TestRegistryAdoptExistingSkipsNonMountedDirs(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	if err := os.MkdirAll(filepath.Join(root, cfg.ID()), 0o755); err != nil {
		t.Fatal(err)
	}

	fake := exectest.New()
	fake.Set("findmnt", "", &fbexec.Error{ExitCode: 1})
	fake.Set("mount", "", nil)

	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)

	if err := reg.AdoptExisting(context.Background()); err != nil {
		t.Fatalf("AdoptExisting: %v", err)
	}
	if paths := reg.MountedPaths(); len(paths) != 0 {
		t.Errorf("MountedPaths after AdoptExisting on non-mounted stale dir = %v, want empty", paths)
	}

	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatalf("Get: %v", err)
	}
	mountCalls := 0
	for _, c := range fake.Calls {
		if c.Name == "mount" {
			mountCalls++
		}
	}
	if mountCalls != 1 {
		t.Errorf("after non-adopted Get, mount called %d times, want 1", mountCalls)
	}
}

// TestRegistryAdoptExistingAdoptsMountedDirs asserts the positive path:
// when IsMountPoint reports a live mount, AdoptExisting caches the
// storeID and subsequent Get(cfg) short-circuits without issuing a real
// mount(8) call.
func TestRegistryAdoptExistingAdoptsMountedDirs(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	dir := filepath.Join(root, cfg.ID())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := exectest.New()
	fake.Set("findmnt", dir, nil)

	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)

	if err := reg.AdoptExisting(context.Background()); err != nil {
		t.Fatalf("AdoptExisting: %v", err)
	}
	got := reg.MountedPaths()
	if len(got) != 1 || got[0] != dir {
		t.Fatalf("MountedPaths after adopting verified mount = %v, want [%q]", got, dir)
	}

	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, c := range fake.Calls {
		if c.Name == "mount" {
			t.Errorf("Get after adoption should hit cache, but called mount: %v", c.Args)
		}
	}
}

// TestRegistryAdoptExistingSkipsOnCheckError verifies that if
// IsMountPoint returns an error that's NOT the exit-1 "not a mount"
// sentinel (e.g. findmnt missing, or an unexpected exit code),
// AdoptExisting skips that candidate but still returns nil. The next
// Get(cfg) triggers a real mount(8) call — the recovery is identical
// to the "not a mount" case.
func TestRegistryAdoptExistingSkipsOnCheckError(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	if err := os.MkdirAll(filepath.Join(root, cfg.ID()), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := exectest.New()
	fake.Set("findmnt", "", &fbexec.Error{ExitCode: 2})
	fake.Set("mount", "", nil)

	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt)

	if err := reg.AdoptExisting(context.Background()); err != nil {
		t.Fatalf("AdoptExisting should swallow per-candidate check errors, got %v", err)
	}
	if paths := reg.MountedPaths(); len(paths) != 0 {
		t.Errorf("MountedPaths after IsMountPoint error = %v, want empty", paths)
	}

	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatalf("Get: %v", err)
	}
	mountCalls := 0
	for _, c := range fake.Calls {
		if c.Name == "mount" {
			mountCalls++
		}
	}
	if mountCalls != 1 {
		t.Errorf("after non-adopted Get, mount called %d times, want 1", mountCalls)
	}
}
