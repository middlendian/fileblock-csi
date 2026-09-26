package loop

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/middlendian/fileblock-csi/pkg/crypt"
	"github.com/middlendian/fileblock-csi/pkg/exec/exectest"
)

// TestReconcileDropsStaleEntries verifies a state entry whose loop is no
// longer attached gets removed.
func TestReconcileDropsStaleEntries(t *testing.T) {
	dir := t.TempDir()
	state, err := LoadState(filepath.Join(dir, "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.Put(Mapping{VolumeID: "v1", LoopDev: "/dev/loop0", ImagePath: "/srv/v1.img", StagePath: "/s/v1"})

	fake := exectest.New()
	fake.Set("losetup", `{"loopdevices":[]}`, nil)
	rec := NewReconciler(state, NewLosetup(fake), nil, "/srv")

	if err := rec.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := state.Get("v1"); ok {
		t.Fatal("stale entry not dropped")
	}
}

// TestReconcileDropsMismatchedBackFile keeps state honest when a loop dev was
// reused for a different image.
func TestReconcileDropsMismatchedBackFile(t *testing.T) {
	dir := t.TempDir()
	state, _ := LoadState(filepath.Join(dir, "s.json"))
	_ = state.Put(Mapping{VolumeID: "v1", LoopDev: "/dev/loop0", ImagePath: "/srv/v1.img", StagePath: "/s/v1"})

	fake := exectest.New()
	fake.Set("losetup", `{"loopdevices":[{"name":"/dev/loop0","back-file":"/srv/something-else.img"}]}`, nil)
	rec := NewReconciler(state, NewLosetup(fake), nil, "/srv")
	if err := rec.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := state.Get("v1"); ok {
		t.Fatal("mismatched mapping not dropped")
	}
}

// TestReconcileDetachesOrphanLoop verifies a loop pointing into our backing
// store with no matching state entry gets detached.
func TestReconcileDetachesOrphanLoop(t *testing.T) {
	dir := t.TempDir()
	state, _ := LoadState(filepath.Join(dir, "s.json"))

	fake := exectest.New()
	called := 0
	fake.Func = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "losetup" && len(args) >= 1 && args[0] == "--json" {
			return `{"loopdevices":[{"name":"/dev/loop3","back-file":"/srv/orphan.img"}]}`, nil
		}
		if name == "losetup" && len(args) >= 1 && args[0] == "--detach" {
			if args[1] != "/dev/loop3" {
				t.Errorf("Detach got %v, want /dev/loop3", args)
			}
			called++
			return "", nil
		}
		t.Errorf("unexpected call: %s %v", name, args)
		return "", nil
	}
	rec := NewReconciler(state, NewLosetup(fake), nil, "/srv")
	if err := rec.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if called != 1 {
		t.Fatalf("Detach called %d times, want 1", called)
	}
}

// TestReconcileLeavesUnrelatedLoops alone — anything backed by a path outside
// our backingStorePath must not be touched.
func TestReconcileLeavesUnrelatedLoops(t *testing.T) {
	dir := t.TempDir()
	state, _ := LoadState(filepath.Join(dir, "s.json"))

	fake := exectest.New()
	fake.Func = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "losetup" && args[0] == "--json" {
			return `{"loopdevices":[{"name":"/dev/loop4","back-file":"/other/foo.img"}]}`, nil
		}
		t.Errorf("unexpected call: %s %v", name, args)
		return "", nil
	}
	rec := NewReconciler(state, NewLosetup(fake), nil, "/srv")
	if err := rec.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// TestReconcileKeepsHealthyEntries — a state row whose loop is still live and
// pointing at the right image must survive.
func TestReconcileKeepsHealthyEntries(t *testing.T) {
	dir := t.TempDir()
	state, _ := LoadState(filepath.Join(dir, "s.json"))
	_ = state.Put(Mapping{VolumeID: "v1", LoopDev: "/dev/loop0", ImagePath: "/srv/v1.img", StagePath: "/s/v1"})

	fake := exectest.New()
	fake.Set("losetup", `{"loopdevices":[{"name":"/dev/loop0","back-file":"/srv/v1.img"}]}`, nil)
	rec := NewReconciler(state, NewLosetup(fake), nil, "/srv")
	if err := rec.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := state.Get("v1"); !ok {
		t.Fatal("healthy entry was dropped")
	}
}

type fakeCrypt struct {
	mappings []crypt.Mapping
	log      *[]string
}

func (f *fakeCrypt) List(context.Context) ([]crypt.Mapping, error) { return f.mappings, nil }
func (f *fakeCrypt) Close(_ context.Context, name string) error {
	*f.log = append(*f.log, "close "+name)
	return nil
}

// losetupFake lists live loops and records detaches into log.
func losetupFake(live string, log *[]string) *exectest.FakeRunner {
	fake := exectest.New()
	fake.Func = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "losetup" && len(args) > 0 && args[0] == "--json" {
			return live, nil
		}
		if name == "losetup" && len(args) > 1 && args[0] == "--detach" {
			*log = append(*log, "detach "+args[1])
			return "", nil
		}
		return "", fmt.Errorf("unexpected %s %v", name, args)
	}
	return fake
}

func TestReconcileDropsEntryWhoseCryptMappingVanished(t *testing.T) {
	state, _ := LoadState(filepath.Join(t.TempDir(), "s.json"))
	_ = state.Put(Mapping{VolumeID: "v1", LoopDev: "/dev/loop0", ImagePath: "/srv/v1.img", StagePath: "/s/v1", CryptDev: "/dev/mapper/fbcrypt-a"})
	var log []string
	fake := losetupFake(`{"loopdevices":[{"name":"/dev/loop0","back-file":"/srv/v1.img"}]}`, &log)
	rec := NewReconciler(state, NewLosetup(fake), &fakeCrypt{log: &log}, "/srv")
	if err := rec.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Get("v1"); ok {
		t.Fatal("entry kept although its crypt mapping is gone")
	}
}

func TestReconcileKeepsTrackedCryptEntry(t *testing.T) {
	state, _ := LoadState(filepath.Join(t.TempDir(), "s.json"))
	_ = state.Put(Mapping{VolumeID: "v1", LoopDev: "/dev/loop0", ImagePath: "/srv/v1.img", StagePath: "/s/v1", CryptDev: "/dev/mapper/fbcrypt-a"})
	var log []string
	fake := losetupFake(`{"loopdevices":[{"name":"/dev/loop0","back-file":"/srv/v1.img"}]}`, &log)
	cm := &fakeCrypt{mappings: []crypt.Mapping{{Name: "fbcrypt-a", Backing: "/dev/loop0"}}, log: &log}
	if err := NewReconciler(state, NewLosetup(fake), cm, "/srv").Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Get("v1"); !ok || len(log) != 0 {
		t.Fatalf("tracked volume disturbed: log=%v", log)
	}
}

// Review Focus 5: a live mapping holds its loop open, so the orphan
// mapping must be closed before the loop detach is attempted.
func TestReconcileClosesOrphanCryptBeforeDetach(t *testing.T) {
	state, _ := LoadState(filepath.Join(t.TempDir(), "s.json"))
	var log []string
	fake := losetupFake(`{"loopdevices":[{"name":"/dev/loop5","back-file":"/srv/v9.img"}]}`, &log)
	cm := &fakeCrypt{mappings: []crypt.Mapping{{Name: "fbcrypt-z", Backing: "/dev/loop5"}}, log: &log}
	if err := NewReconciler(state, NewLosetup(fake), cm, "/srv").Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"close fbcrypt-z", "detach /dev/loop5"}
	if !slices.Equal(log, want) {
		t.Fatalf("log = %v, want %v", log, want)
	}
}

func TestReconcileLeavesCryptOverForeignLoop(t *testing.T) {
	state, _ := LoadState(filepath.Join(t.TempDir(), "s.json"))
	var log []string
	fake := losetupFake(`{"loopdevices":[{"name":"/dev/loop6","back-file":"/elsewhere/x.img"}]}`, &log)
	cm := &fakeCrypt{mappings: []crypt.Mapping{{Name: "fbcrypt-q", Backing: "/dev/loop6"}}, log: &log}
	if err := NewReconciler(state, NewLosetup(fake), cm, "/srv").Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(log) != 0 {
		t.Fatalf("touched a foreign mapping/loop: %v", log)
	}
}
