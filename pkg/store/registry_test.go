package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/exec/exectest"
	"github.com/middlendian/fileblock-csi/pkg/mount"
)

// findmntReportsMounted makes the fake answer findmnt(8) the way it does
// for a live mountpoint: pkg/mount.Mounter.IsMountPoint compares the
// trimmed output of `findmnt -n -o TARGET <path>` against the path it
// asked about. A fake that returns empty output instead reads as "not a
// mountpoint", which makes Get evict and remount on every call.
func findmntReportsMounted(f *exectest.FakeRunner) {
	f.Func = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "findmnt" {
			return args[len(args)-1], nil
		}
		return "", nil
	}
}

func TestRegistryGetMountsOnce(t *testing.T) {
	root := t.TempDir()
	fake := exectest.New()
	fake.SetDefault("", nil)
	findmntReportsMounted(fake)
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)
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
	if p1 != filepath.Join(root, cfg.StoreID()) {
		t.Errorf("path = %q, want %q", p1, filepath.Join(root, cfg.StoreID()))
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
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)
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
	findmntReportsMounted(fake)
	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)
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
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)
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
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	if _, ok := reg.ConfigByStoreID(cfg.StoreID()); ok {
		t.Fatal("ConfigByStoreID returned true before any Get")
	}
	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	got, ok := reg.ConfigByStoreID(cfg.StoreID())
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
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)

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
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)
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
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)
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
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)
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
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)
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
	if err := os.MkdirAll(filepath.Join(root, cfg.StoreID()), 0o755); err != nil {
		t.Fatal(err)
	}

	fake := exectest.New()
	fake.Set("findmnt", "", &fbexec.Error{ExitCode: 1})
	fake.Set("mount", "", nil)

	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)

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
	dir := filepath.Join(root, cfg.StoreID())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := exectest.New()
	fake.Set("findmnt", dir, nil)

	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)

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
	if err := os.MkdirAll(filepath.Join(root, cfg.StoreID()), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := exectest.New()
	fake.Set("findmnt", "", &fbexec.Error{ExitCode: 2})
	fake.Set("mount", "", nil)

	mnt := mount.New(fake)
	reg := NewRegistry(root, NewNFSMounter(fake), NewLocalMounter(mnt), mnt, nil)

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

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeChecker is a MountChecker scripted directly by the test. Driving
// the real checker through findmnt strings cannot express the three
// answers Get distinguishes — mounted, definitively not mounted, and
// "no answer" (an error, or a call that never returns).
type fakeChecker struct {
	mu      sync.Mutex
	mounted bool
	err     error
	entered int           // incremented on entry, before any blocking
	block   chan struct{} // when non-nil, IsMountPoint waits on it
	returns chan struct{} // when non-nil, one token per completed call
}

func (c *fakeChecker) IsMountPoint(_ context.Context, _ string) (bool, error) {
	c.mu.Lock()
	c.entered++
	block := c.block
	c.mu.Unlock()
	if block != nil {
		<-block
	}
	c.mu.Lock()
	mounted, err, returns := c.mounted, c.err, c.returns
	c.mu.Unlock()
	if returns != nil {
		returns <- struct{}{}
	}
	return mounted, err
}

func (c *fakeChecker) set(mounted bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mounted, c.err = mounted, err
}

func (c *fakeChecker) enteredCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entered
}

func countMounts(f *exectest.FakeRunner) int {
	n := 0
	for _, c := range f.Calls {
		if c.Name == "mount" {
			n++
		}
	}
	return n
}

// newCheckedRegistry wires a Registry whose staleness check is chk and
// whose mount(8) calls are stubbed out.
func newCheckedRegistry(t *testing.T, chk MountChecker) (*Registry, *exectest.FakeRunner) {
	t.Helper()
	fake := exectest.New()
	fake.SetDefault("", nil)
	reg := NewRegistry(t.TempDir(), NewNFSMounter(fake), NewLocalMounter(mount.New(fake)), chk, discardLog())
	return reg, fake
}

// TestRegistryGetRemountsAfterMountDisappears is the core regression: a
// backing-store mount that drops out from under a running process must
// not leave Get handing back the now-empty mountpoint directory forever.
func TestRegistryGetRemountsAfterMountDisappears(t *testing.T) {
	chk := &fakeChecker{mounted: true}
	reg, fake := newCheckedRegistry(t, chk)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	p1, err := reg.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Get #1: %v", err)
	}

	// The mount goes away; the directory underneath it does not.
	chk.set(false, nil)

	p2, err := reg.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Get #2: %v", err)
	}
	if p1 != p2 {
		t.Errorf("path changed across remount: %q vs %q", p1, p2)
	}
	if n := countMounts(fake); n != 2 {
		t.Errorf("mount called %d times, want 2 (initial + remount)", n)
	}
}

func TestRegistryGetDoesNotRemountWhileMounted(t *testing.T) {
	chk := &fakeChecker{mounted: true}
	reg, fake := newCheckedRegistry(t, chk)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	for i := 0; i < 3; i++ {
		if _, err := reg.Get(context.Background(), cfg); err != nil {
			t.Fatalf("Get #%d: %v", i, err)
		}
	}
	if n := countMounts(fake); n != 1 {
		t.Errorf("mount called %d times, want 1", n)
	}
	if got := chk.enteredCount(); got != 2 {
		t.Errorf("IsMountPoint called %d times, want 2 (the first Get has nothing cached to verify)", got)
	}
}

// TestRegistryGetKeepsCachedPathOnCheckError pins the deliberate
// asymmetry with AdoptExisting: there, an unusable answer costs one
// redundant mount(8) onto an unmounted directory. Here it would stack a
// second mount on a target that is still mounted, on every Get, with
// nothing to unstack it — so only a definitive "not mounted" evicts.
func TestRegistryGetKeepsCachedPathOnCheckError(t *testing.T) {
	chk := &fakeChecker{mounted: true}
	reg, fake := newCheckedRegistry(t, chk)
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatalf("Get #1: %v", err)
	}
	chk.set(false, errors.New("findmnt exploded"))
	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatalf("Get #2: %v", err)
	}
	if n := countMounts(fake); n != 1 {
		t.Errorf("mount called %d times, want 1 (an errored check must not evict)", n)
	}
}

// TestRegistryGetKeepsCachedPathWhenCheckHangs covers the hung
// hard-mounted NFS target: the stat inside IsMountPoint never returns.
// Get must fall back to the cached path rather than block, and must not
// spawn a fresh check while one is still outstanding.
func TestRegistryGetKeepsCachedPathWhenCheckHangs(t *testing.T) {
	block := make(chan struct{})
	chk := &fakeChecker{mounted: true, block: block}
	reg, fake := newCheckedRegistry(t, chk)
	reg.checkTimeout = 20 * time.Millisecond
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	want, err := reg.Get(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Get #1: %v", err)
	}

	for i := 0; i < 3; i++ {
		done := make(chan struct{})
		var got string
		var gErr error
		go func() {
			got, gErr = reg.Get(context.Background(), cfg)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("Get #%d blocked on a hung mountpoint check", i+2)
		}
		if gErr != nil {
			t.Fatalf("Get #%d: %v", i+2, gErr)
		}
		if got != want {
			t.Errorf("Get #%d = %q, want cached %q", i+2, got, want)
		}
	}
	if n := countMounts(fake); n != 1 {
		t.Errorf("mount called %d times, want 1 (a hung check must not evict)", n)
	}
	if got := chk.enteredCount(); got != 1 {
		t.Errorf("IsMountPoint entered %d times, want 1 (checks must not pile up behind a wedged one)", got)
	}
	close(block)
}

// TestRegistryConcurrentGetOnStaleEntryRemountsOnce is the race the
// issue asks about. The fake couples the checker to mount(8) so the
// store really does come back up mid-flight, rather than reporting
// stale forever and inviting one remount per caller.
func TestRegistryConcurrentGetOnStaleEntryRemountsOnce(t *testing.T) {
	chk := &fakeChecker{mounted: true}
	reg, fake := newCheckedRegistry(t, chk)
	fake.Func = func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "mount" {
			chk.set(true, nil)
		}
		return "", nil
	}
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatalf("Get #1: %v", err)
	}
	chk.set(false, nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reg.Get(context.Background(), cfg); err != nil {
				t.Errorf("concurrent Get: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := countMounts(fake); n != 2 {
		t.Errorf("mount called %d times, want 2 (initial + exactly one remount)", n)
	}
}

func TestRegistryGetSkipsCheckWithNilMountChecker(t *testing.T) {
	fake := exectest.New()
	fake.SetDefault("", nil)
	reg := NewRegistry(t.TempDir(), NewNFSMounter(fake), nil, nil, discardLog())
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	for i := 0; i < 2; i++ {
		if _, err := reg.Get(context.Background(), cfg); err != nil {
			t.Fatalf("Get #%d: %v", i, err)
		}
	}
	if n := countMounts(fake); n != 1 {
		t.Errorf("mount called %d times, want 1", n)
	}
}

// TestRegistryGetIgnoresAbandonedCheckResult pins the rule that a check
// which outlived its timeout is never acted on. Its answer describes the
// mount at some unknown earlier moment; here the store was gone when the
// hung check looked and is back by the time the answer lands, so acting
// on it would tear down a healthy mount.
func TestRegistryGetIgnoresAbandonedCheckResult(t *testing.T) {
	block := make(chan struct{})
	chk := &fakeChecker{mounted: true, block: block, returns: make(chan struct{}, 4)}
	reg, fake := newCheckedRegistry(t, chk)
	reg.checkTimeout = 20 * time.Millisecond
	cfg := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}

	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatalf("Get #1: %v", err)
	}

	// Get #2 starts a check that hangs past its timeout.
	if _, err := reg.Get(context.Background(), cfg); err != nil {
		t.Fatalf("Get #2: %v", err)
	}

	// The store was unmounted while that check was stuck...
	chk.set(false, nil)
	close(block)
	select {
	case <-chk.returns:
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned check never returned")
	}
	// ...and has come back by the time anyone looks again.
	chk.set(true, nil)

	for i := 0; i < 2; i++ {
		if _, err := reg.Get(context.Background(), cfg); err != nil {
			t.Fatalf("Get #%d: %v", i+3, err)
		}
	}
	if n := countMounts(fake); n != 1 {
		t.Errorf("mount called %d times, want 1 (a timed-out check's answer must not evict)", n)
	}
}
