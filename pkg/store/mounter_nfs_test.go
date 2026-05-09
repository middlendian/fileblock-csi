package store

import (
	"context"
	"errors"
	"testing"

	"github.com/middlendian/fileblock-csi/pkg/exec/exectest"
)

func TestNFSMounterInvokesMountWithFstype(t *testing.T) {
	fake := exectest.New()
	fake.SetDefault("", nil)
	m := NewNFSMounter(fake)
	cfg := Config{
		Type:            TypeNFS,
		NFSServer:       "nfs.example.internal",
		NFSPath:         "/exports/fileblock",
		NFSMountOptions: "nfsvers=4.1,hard",
	}
	if err := m.Mount(context.Background(), "/var/lib/fileblock/stores/abc", cfg); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %+v", len(fake.Calls), fake.Calls)
	}
	c := fake.Calls[0]
	if c.Name != "mount" {
		t.Errorf("cmd = %q, want mount", c.Name)
	}
	wantArgs := []string{"-t", "nfs", "-o", "nfsvers=4.1,hard", "nfs.example.internal:/exports/fileblock", "/var/lib/fileblock/stores/abc"}
	if !equalArgs(c.Args, wantArgs) {
		t.Errorf("args = %v\n want %v", c.Args, wantArgs)
	}
}

func TestNFSMounterOmitsOptsWhenEmpty(t *testing.T) {
	fake := exectest.New()
	fake.SetDefault("", nil)
	m := NewNFSMounter(fake)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	if err := m.Mount(context.Background(), "/t", cfg); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	c := fake.Calls[0]
	wantArgs := []string{"-t", "nfs", "s:/p", "/t"}
	if !equalArgs(c.Args, wantArgs) {
		t.Errorf("args = %v\n want %v", c.Args, wantArgs)
	}
}

func TestNFSMounterV3Variant(t *testing.T) {
	fake := exectest.New()
	fake.SetDefault("", nil)
	m := NewNFSMounter(fake)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSMountOptions: "nfsvers=3,nolock"}
	if err := m.Mount(context.Background(), "/t", cfg); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	c := fake.Calls[0]
	wantArgs := []string{"-t", "nfs", "-o", "nfsvers=3,nolock", "s:/p", "/t"}
	if !equalArgs(c.Args, wantArgs) {
		t.Errorf("args = %v\n want %v", c.Args, wantArgs)
	}
}

func TestNFSMounterRejectsNonNFS(t *testing.T) {
	m := NewNFSMounter(exectest.New())
	if err := m.Mount(context.Background(), "/t", Config{Type: TypeLocal}); err == nil {
		t.Fatal("expected error for non-nfs config")
	}
}

func TestNFSMounterRequiresServerAndPath(t *testing.T) {
	m := NewNFSMounter(exectest.New())
	if err := m.Mount(context.Background(), "/t", Config{Type: TypeNFS, NFSPath: "/p"}); err == nil {
		t.Fatal("expected error for missing server")
	}
	if err := m.Mount(context.Background(), "/t", Config{Type: TypeNFS, NFSServer: "s"}); err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestNFSMounterSurfacesExecError(t *testing.T) {
	fake := exectest.New()
	fake.SetDefault("server unreachable", errors.New("exit 32"))
	m := NewNFSMounter(fake)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	if err := m.Mount(context.Background(), "/t", cfg); err == nil {
		t.Fatal("expected error")
	}
}
