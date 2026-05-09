package store

import (
	"context"
	"errors"
	"testing"

	"github.com/middlendian/fileblock-csi/pkg/exec/exectest"
	"github.com/middlendian/fileblock-csi/pkg/mount"
)

func TestLocalMounterBindMounts(t *testing.T) {
	fake := exectest.New()
	fake.SetDefault("", nil)
	m := NewLocalMounter(mount.New(fake))
	cfg := Config{Type: TypeLocal, LocalPath: "/srv/data"}
	if err := m.Mount(context.Background(), "/var/lib/fileblock/stores/abc", cfg); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	// pkg/mount.BindMount calls `mount --bind <src> <target>` with no
	// readOnly remount when readOnly=false. Expect exactly one call.
	if len(fake.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %+v", len(fake.Calls), fake.Calls)
	}
	c := fake.Calls[0]
	if c.Name != "mount" {
		t.Errorf("cmd = %q, want mount", c.Name)
	}
	if !equalArgs(c.Args, []string{"--bind", "/srv/data", "/var/lib/fileblock/stores/abc"}) {
		t.Errorf("args = %v", c.Args)
	}
}

func TestLocalMounterRejectsNonLocalConfig(t *testing.T) {
	m := NewLocalMounter(mount.New(exectest.New()))
	cfg := Config{Type: TypeNFS}
	err := m.Mount(context.Background(), "/x", cfg)
	if err == nil {
		t.Fatal("expected error for non-local config")
	}
}

func TestLocalMounterRejectsEmptyLocalPath(t *testing.T) {
	m := NewLocalMounter(mount.New(exectest.New()))
	cfg := Config{Type: TypeLocal}
	err := m.Mount(context.Background(), "/x", cfg)
	if err == nil {
		t.Fatal("expected error for empty LocalPath")
	}
}

func TestLocalMounterSurfacesExecError(t *testing.T) {
	fake := exectest.New()
	fake.SetDefault("permission denied", errors.New("exit 32"))
	m := NewLocalMounter(mount.New(fake))
	cfg := Config{Type: TypeLocal, LocalPath: "/srv/data"}
	err := m.Mount(context.Background(), "/x", cfg)
	if err == nil {
		t.Fatal("expected error")
	}
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
