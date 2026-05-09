package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/middlendian/fileblock-csi/pkg/exec/exectest"
	"github.com/middlendian/fileblock-csi/pkg/mount"
)

func TestRegistryGetMountsOnce(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
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
		if c.Name == "mount.nfs" {
			mountCalls++
		}
	}
	if mountCalls != 1 {
		t.Errorf("mount.nfs called %d times, want 1", mountCalls)
	}
}

func TestRegistryDistinctConfigsMountSeparately(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
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
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
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
		if c.Name == "mount.nfs" {
			mountCalls++
		}
	}
	if mountCalls != 1 {
		t.Errorf("under concurrent Get, mount.nfs called %d times, want 1", mountCalls)
	}
}

func TestRegistryRejectsUnknownType(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
	_, err := reg.Get(context.Background(), Config{Type: "smb"})
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestRegistryConfigByStoreID(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
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
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))

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
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
	if err := reg.AdoptExisting(); err != nil {
		t.Fatalf("AdoptExisting: %v", err)
	}
	if len(reg.MountedPaths()) != 0 {
		t.Errorf("expected empty mounted set, got %v", reg.MountedPaths())
	}
}

func TestRegistryAdoptExistingPreloadsKnownDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "abc123def456"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := exectest.New()
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
	if err := reg.AdoptExisting(); err != nil {
		t.Fatalf("AdoptExisting: %v", err)
	}
	got := reg.MountedPaths()
	if len(got) != 1 || got[0] != filepath.Join(root, "abc123def456") {
		t.Errorf("MountedPaths = %v", got)
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
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mount.New(fake)))
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
