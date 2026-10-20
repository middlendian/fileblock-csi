package driver

import (
	"sync"
	"time"
)

// failures holds volumes whose ext4 was forced read-only, and each
// volume's remediation hold-off. In memory only: a plugin restart
// rediscovers failures from mountinfo.
type failures struct {
	mu     sync.Mutex
	failed map[string]time.Time
	held   map[string]time.Time
}

func newFailures() *failures {
	return &failures{failed: map[string]time.Time{}, held: map[string]time.Time{}}
}

// Mark records volumeID as failed at t; it returns true only the first time.
func (f *failures) Mark(volumeID string, t time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.failed[volumeID]; ok {
		return false
	}
	f.failed[volumeID] = t
	return true
}

func (f *failures) Get(volumeID string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.failed[volumeID]
	return t, ok
}

func (f *failures) Clear(volumeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.failed, volumeID)
}

// HeldUntil and Hold keep the hold-off apart from the failed state so that
// Clear does not reset it.
func (f *failures) HeldUntil(volumeID string) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held[volumeID]
}

func (f *failures) Hold(volumeID string, until time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[volumeID] = until
}
