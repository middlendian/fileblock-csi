package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// Mapping is one entry in the state file.
type Mapping struct {
	VolumeID  string `json:"volumeId"`
	LoopDev   string `json:"loopDev"`
	ImagePath string `json:"imagePath"`
	StagePath string `json:"stagePath"`
}

// State persists volumeID -> loop device + staging path. The on-disk file is
// guarded by an OS-level flock so a crashed plugin doesn't leave a half-
// written file. In-process callers are serialized by the embedded mutex.
type State struct {
	mu   sync.Mutex
	path string
	data map[string]Mapping
}

// LoadState opens (or initializes) the state file at path.
func LoadState(path string) (*State, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &State{path: path, data: map[string]Mapping{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

// All returns a snapshot copy. Safe to range over after release.
func (s *State) All() []Mapping {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Mapping, 0, len(s.data))
	for _, m := range s.data {
		out = append(out, m)
	}
	return out
}

func (s *State) Get(volumeID string) (Mapping, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.data[volumeID]
	return m, ok
}

func (s *State) Put(m Mapping) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[m.VolumeID] = m
	return s.flushLocked()
}

func (s *State) Delete(volumeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, volumeID)
	return s.flushLocked()
}

func (s *State) flushLocked() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
