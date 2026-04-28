package flock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTryLockAndRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "img")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	h, err := TryLock(path)
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	if h == nil {
		t.Fatal("nil handle")
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// After Close, a re-lock must succeed.
	h2, err := TryLock(path)
	if err != nil {
		t.Fatalf("re-lock: %v", err)
	}
	_ = h2.Close()
}

func TestTryLockReturnsErrLockedWhenHeld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "img")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := TryLock(path)
	if err != nil {
		t.Fatalf("first TryLock: %v", err)
	}
	defer h.Close()

	// flock(2) is per-open-file-description on Linux; opening the file again
	// from this process and locking it must contend with the first holder.
	if _, err := TryLock(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second TryLock: got %v, want ErrLocked", err)
	}
}

func TestTryLockMissingFile(t *testing.T) {
	if _, err := TryLock(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestCloseNilSafe(t *testing.T) {
	var h *Handle
	if err := h.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
	h2 := &Handle{}
	if err := h2.Close(); err != nil {
		t.Fatalf("zero Close: %v", err)
	}
}
