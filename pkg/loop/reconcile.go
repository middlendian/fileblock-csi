package loop

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/middlendian/fileblock-csi/pkg/crypt"
)

// CryptMappings is the view of open dm-crypt mappings the reconciler
// needs; *crypt.Crypt satisfies it.
type CryptMappings interface {
	List(ctx context.Context) ([]crypt.Mapping, error)
	Close(ctx context.Context, name string) error
}

// disconnectedImage matches the back-file of a volume image ("fb-<storeID>-
// <name>.img") attached through a store mount that is no longer reachable:
// at the mount root, or one subDir level down. Deeper subDirs are not
// matched, so their orphans are left for unstage to detach.
var disconnectedImage = regexp.MustCompile(`^/([^/]+/)?fb-[0-9a-f]{12}-[^/]+\.img$`)

// Backs reports whether back, a loop's backing file as losetup reports it,
// is the image at imagePath. The node plugin's backing-store mounts exist
// only in its own mount namespace, so once the process that attached a loop
// is gone the kernel reports the file relative to that mount instead
// ("/fb-….img", or "/<subDir>/fb-….img"): a trailing match on a path
// boundary counts too.
func Backs(back, imagePath string) bool {
	if back == imagePath {
		return true
	}
	return strings.HasPrefix(back, "/") && strings.HasSuffix(imagePath, back) &&
		filepath.Base(back) == filepath.Base(imagePath)
}

// Reconciler runs at node-plugin start and after kubelet-driven retries to
// keep the state file, the kernel's loop table, and the staging mounts
// consistent. It is conservative: when in doubt, drop the mapping rather than
// detach a loop someone else might own.
type Reconciler struct {
	state            *State
	losetup          *Losetup
	crypt            CryptMappings
	backingStorePath string
}

// NewReconciler builds a Reconciler. cm may be nil, which skips crypt
// mappings entirely.
func NewReconciler(state *State, losetup *Losetup, cm CryptMappings, backingStorePath string) *Reconciler {
	return &Reconciler{state: state, losetup: losetup, crypt: cm, backingStorePath: backingStorePath}
}

// Reconcile drops state entries whose loop device (or crypt mapping) is no
// longer what the entry says, closes fbcrypt mappings over our loops that
// no entry tracks, and then detaches loop devices backed by one of our
// images that no entry tracks. An untracked loop that is still mounted is
// only marked autoclear by the kernel and goes away when it is unmounted.
// The staging mount itself is left alone; the kubelet retries
// publish/unstage.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	live, err := r.losetup.List(ctx)
	if err != nil {
		return err
	}
	liveByDev := map[string]string{} // dev -> back-file
	for _, a := range live {
		liveByDev[a.Device] = a.BackFile
	}
	var mappings []crypt.Mapping
	if r.crypt != nil {
		if mappings, err = r.crypt.List(ctx); err != nil {
			return err
		}
	}
	backingByName := map[string]string{}
	for _, m := range mappings {
		backingByName[m.Name] = m.Backing
	}

	// 1. Drop stale state entries.
	for _, m := range r.state.All() {
		back, ok := liveByDev[m.LoopDev]
		stale := !ok || !Backs(back, m.ImagePath)
		if m.CryptDev != "" {
			b, open := backingByName[filepath.Base(m.CryptDev)]
			stale = stale || !open || b != m.LoopDev
		}
		if stale {
			_ = r.state.Delete(m.VolumeID)
		}
	}

	trackedLoops := map[string]bool{}
	trackedCrypt := map[string]bool{}
	for _, m := range r.state.All() {
		trackedLoops[m.LoopDev] = true
		if m.CryptDev != "" {
			trackedCrypt[filepath.Base(m.CryptDev)] = true
		}
	}
	cleanRoot := filepath.Clean(r.backingStorePath)
	// Ours: under the stores root, or a volume image attached by an earlier
	// plugin process, whose path no longer starts with the root (see Backs).
	ours := func(dev string) bool {
		back, ok := liveByDev[dev]
		return ok && (strings.HasPrefix(filepath.Clean(back), cleanRoot+string(filepath.Separator)) ||
			disconnectedImage.MatchString(back))
	}

	// 2. Close orphan crypt mappings first: an open mapping holds its loop.
	for _, m := range mappings {
		if trackedCrypt[m.Name] || !ours(m.Backing) {
			continue
		}
		_ = r.crypt.Close(ctx, m.Name)
	}

	// 3. Detach orphan loops backed by a .img under our backing store.
	for dev := range liveByDev {
		if trackedLoops[dev] || !ours(dev) {
			continue
		}
		_ = r.losetup.Detach(ctx, dev)
	}
	return nil
}
