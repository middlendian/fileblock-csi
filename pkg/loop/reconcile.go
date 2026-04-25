package loop

import (
	"context"
	"path/filepath"
	"strings"
)

// Reconciler runs at node-plugin start and after kubelet-driven retries to
// keep the state file, the kernel's loop table, and the staging mounts
// consistent. It is conservative: when in doubt, drop the mapping rather than
// detach a loop someone else might own.
type Reconciler struct {
	state            *State
	losetup          *Losetup
	backingStorePath string
}

func NewReconciler(state *State, losetup *Losetup, backingStorePath string) *Reconciler {
	return &Reconciler{state: state, losetup: losetup, backingStorePath: backingStorePath}
}

// Reconcile drops state entries whose loop device is no longer attached or no
// longer points at the expected .img, and detaches loop devices that are
// backed by a .img under our backing store but have no live state entry. The
// staging mount itself is left alone; the kubelet retries publish/unstage.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	live, err := r.losetup.List(ctx)
	if err != nil {
		return err
	}
	liveByDev := map[string]string{} // dev -> back-file
	for _, a := range live {
		liveByDev[a.Device] = a.BackFile
	}

	// 1. Drop stale state entries.
	for _, m := range r.state.All() {
		back, ok := liveByDev[m.LoopDev]
		if !ok || back != m.ImagePath {
			_ = r.state.Delete(m.VolumeID)
		}
	}

	// 2. Detach orphan loops backed by a .img under our backing store.
	tracked := map[string]bool{}
	for _, m := range r.state.All() {
		tracked[m.LoopDev] = true
	}
	cleanRoot := filepath.Clean(r.backingStorePath)
	for dev, back := range liveByDev {
		if tracked[dev] {
			continue
		}
		if !strings.HasPrefix(filepath.Clean(back), cleanRoot+string(filepath.Separator)) {
			continue
		}
		_ = r.losetup.Detach(ctx, dev)
	}
	return nil
}
