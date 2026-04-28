package loop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadStateMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "loop-mappings.json")
	s, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := s.All(); len(got) != 0 {
		t.Fatalf("expected empty state, got %+v", got)
	}
}

func TestLoadStateEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loop-mappings.json")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(s.All()) != 0 {
		t.Fatal("expected empty state")
	}
}

func TestLoadStateMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loop-mappings.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestPutGetDeleteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loop-mappings.json")
	s, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	m := Mapping{VolumeID: "fb-1", LoopDev: "/dev/loop0", ImagePath: "/srv/a.img", StagePath: "/stage/a"}
	if err := s.Put(m); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := s.Get("fb-1")
	if !ok || got != m {
		t.Fatalf("Get: got=%+v ok=%v", got, ok)
	}

	// File on disk must reflect the change.
	s2, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s2.Get("fb-1"); !ok || got != m {
		t.Fatalf("reload: got=%+v ok=%v", got, ok)
	}

	if err := s.Delete("fb-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Get("fb-1"); ok {
		t.Fatal("Get after Delete should miss")
	}
}

func TestAllReturnsSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loop-mappings.json")
	s, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Put(Mapping{VolumeID: "a", LoopDev: "/dev/loop0", ImagePath: "/x/a.img", StagePath: "/s/a"})
	_ = s.Put(Mapping{VolumeID: "b", LoopDev: "/dev/loop1", ImagePath: "/x/b.img", StagePath: "/s/b"})

	snap := s.All()
	if len(snap) != 2 {
		t.Fatalf("len=%d", len(snap))
	}
	// Mutating returned slice must not affect internal state.
	snap[0].VolumeID = "tampered"
	if got, _ := s.Get("a"); got.VolumeID != "a" {
		t.Fatalf("snapshot leaked: got %+v", got)
	}
}
