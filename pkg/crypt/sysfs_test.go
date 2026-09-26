package crypt

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func mkDM(t *testing.T, root, dm, name, slave string) {
	t.Helper()
	d := filepath.Join(root, "block", dm)
	if err := os.MkdirAll(filepath.Join(d, "dm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "dm", "name"), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if slave != "" {
		if err := os.MkdirAll(filepath.Join(d, "slaves", slave), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListAndIsOpen(t *testing.T) {
	root := t.TempDir()
	mkDM(t, root, "dm-0", "fbcrypt-abc", "loop3")
	mkDM(t, root, "dm-1", "ubuntu--vg-root", "sda3")
	c := NewAt(nil, root)
	ms, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Name != "fbcrypt-abc" || ms[0].Backing != "/dev/loop3" {
		t.Fatalf("List = %+v", ms)
	}
	if open, _ := c.IsOpen(context.Background(), "fbcrypt-abc"); !open {
		t.Fatal("IsOpen(fbcrypt-abc) = false")
	}
	if open, _ := c.IsOpen(context.Background(), "fbcrypt-zzz"); open {
		t.Fatal("IsOpen(fbcrypt-zzz) = true")
	}
}

func TestListEmptySysfs(t *testing.T) {
	ms, err := NewAt(nil, t.TempDir()).List(context.Background())
	if err != nil || len(ms) != 0 {
		t.Fatalf("List = %v, %v", ms, err)
	}
}
