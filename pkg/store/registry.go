package store

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// MountChecker verifies whether a path is a live mountpoint. Implemented
// by pkg/mount.Mounter (which shells out to findmnt(8)); tests substitute
// a fake.
type MountChecker interface {
	IsMountPoint(ctx context.Context, target string) (bool, error)
}

// storeIDPattern matches a 12-char lowercase hex sha256 prefix — the only
// shape Config.ID() ever produces. AdoptExisting uses this to skip
// directories that happen to live under r.root but were not created by
// the Registry (e.g. an operator's local-backing source dir if they
// chose to put it under stores-root).
var storeIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// defaultMountCheckTimeout bounds one staleness check in Get.
// MountChecker implementations stat the target, and a stat against a
// hung hard-mounted NFS target blocks indefinitely and ignores context
// cancellation — so the check runs in its own goroutine and Get gives up
// waiting after this long.
const defaultMountCheckTimeout = 10 * time.Second

// Registry mounts each unique store config once per process and hands
// out the resulting paths, re-verifying on each handout that the mount
// is still there. Concurrency: per-storeID Mutex prevents two callers
// from racing into Mount; the global mu only guards the mounted,
// per-store mutex, config, and in-flight-check maps.
type Registry struct {
	root   string
	nfsM   Mounter
	localM Mounter
	mp     MountChecker
	log    *slog.Logger

	// Bounds one staleness check; a field rather than a constant so
	// tests can shorten it.
	checkTimeout time.Duration

	mu       sync.Mutex
	mounted  map[string]string        // storeID -> mounted path
	configs  map[string]Config        // storeID -> Config that produced this storeID
	storeMu  map[string]*sync.Mutex   // storeID -> per-store lock
	checking map[string]chan checkRes // storeID -> in-flight staleness check
}

// checkRes is one IsMountPoint answer handed back from the goroutine that
// ran it.
type checkRes struct {
	mounted bool
	err     error
}

// NewRegistry returns a Registry that mounts under root. nfs and local
// mounters may be nil if the corresponding backing-store type is not
// supported in this binary.
//
// mp verifies that a path is a live mount. Both AdoptExisting and Get
// depend on it: AdoptExisting will not adopt an unverified candidate,
// and Get will not hand back a cached path whose mount has gone away.
// Without it, a stale <storeID> directory — left under an emptyDir cache
// by a prior container, or exposed when a backing-store mount drops out
// from under a running process — reads as a healthy empty store and
// every volume lookup under it fails as "not found".
//
// mp may be nil only when the caller accepts both gaps (e.g. tests that
// exercise Get-only paths and pass nil for all three); a nil mp disables
// staleness detection rather than failing.
//
// log may be nil, in which case slog.Default() is used.
func NewRegistry(root string, nfs Mounter, local Mounter, mp MountChecker, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{
		root:         root,
		nfsM:         nfs,
		localM:       local,
		mp:           mp,
		log:          log,
		checkTimeout: defaultMountCheckTimeout,
		mounted:      map[string]string{},
		configs:      map[string]Config{},
		storeMu:      map[string]*sync.Mutex{},
		checking:     map[string]chan checkRes{},
	}
}

// Get ensures cfg's source is mounted under <root>/<storeID>/ and
// returns that path. Idempotent: subsequent calls with the same cfg fast
// path on the cached mount, but only after re-verifying that the cached
// path is still a live mountpoint — a backing-store mount can drop out
// from under a running process, and the mountpoint directory it leaves
// behind is readable and empty, so an unverified cache turns every later
// lookup into a spurious "not found". Also caches cfg keyed by storeID
// so callers that hold only a storeID (controller's DeleteVolume /
// Expand path) can resolve back to a Config via ConfigByStoreID.
func (r *Registry) Get(ctx context.Context, cfg Config) (string, error) {
	id := cfg.ID()
	storeMu := r.lockStore(id)
	storeMu.Lock()
	defer storeMu.Unlock()

	r.mu.Lock()
	path, cached := r.mounted[id]
	r.mu.Unlock()
	if cached {
		if r.stillMounted(ctx, id, path) {
			return path, nil
		}
		r.log.Warn("backing store is no longer mounted; remounting",
			"storeID", id, "path", path)
		r.mu.Lock()
		delete(r.mounted, id)
		r.mu.Unlock()
	}

	target := filepath.Join(r.root, id)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", target, err)
	}
	mnt, err := r.mounterFor(cfg.Type)
	if err != nil {
		return "", err
	}
	if err := mnt.Mount(ctx, target, cfg); err != nil {
		return "", err
	}

	r.mu.Lock()
	r.mounted[id] = target
	r.configs[id] = cfg
	r.mu.Unlock()
	return target, nil
}

// ConfigByStoreID returns the Config that produced the given storeID,
// if this Registry has seen it (i.e. Get(cfg) where cfg.ID() == id has
// previously succeeded in this process). Used by the controller to
// resolve DeleteVolume and ControllerExpandVolume — both of which carry
// only a volumeID, with the storeID encoded in the volumeID prefix.
func (r *Registry) ConfigByStoreID(id string) (Config, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg, ok := r.configs[id]
	return cfg, ok
}

// MountedPaths returns the absolute paths of every store currently
// mounted by this Registry. Order is unspecified.
func (r *Registry) MountedPaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.mounted))
	for _, p := range r.mounted {
		out = append(out, p)
	}
	return out
}

// AdoptExisting walks r.root and adopts each immediate subdirectory whose
// name matches the storeID pattern AND whose path is currently a live
// mountpoint (verified via r.mp.IsMountPoint). Adopted entries populate
// the mounted-paths cache so MountedPaths() / DeleteVolume / etc. can
// find them. It does NOT re-mount anything — adoption requires an
// existing live mount.
//
// The mountpoint check is load-bearing under the default emptyDir cache
// shape: emptyDir survives container restarts within the same pod (only
// pod recreation wipes it), so after a container restart the stores-root
// may still contain a <storeID> directory from a prior run with no NFS
// mount underneath. Treating that as "mounted" would short-circuit the
// next Get and break NodeStageVolume. Under hostPath, the mount itself
// survives, IsMountPoint returns true, and adoption proceeds.
//
// Caveats:
//   - Cannot reconstruct the full Config for adopted stores; only the
//     storeID is recovered (from the directory name). DeleteVolume
//     against an adopted-but-not-Get-ed store will return NotFound from
//     ConfigByStoreID. After the next CreateVolume against the SC,
//     the Config is registered and Delete works.
//   - Per-candidate IsMountPoint errors are absorbed silently and treated
//     as "not adopted". The worst case is a redundant mount(8) on the
//     next Get, which is safe.
func (r *Registry) AdoptExisting(ctx context.Context) error {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read stores root %s: %w", r.root, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if !storeIDPattern.MatchString(id) {
			// Not a Registry-managed dir; skip. This guards against
			// accidental adoption of bind-mount source dirs the
			// operator may have placed under stores-root.
			continue
		}
		target := filepath.Join(r.root, id)
		mounted, err := r.mp.IsMountPoint(ctx, target)
		if err != nil || !mounted {
			// Conservative: skip on any failure or non-mount. Worst
			// case is a redundant mount(8) call on the next Get,
			// which is safe.
			continue
		}
		r.mounted[id] = target
	}
	return nil
}

// stillMounted reports whether the cached path for id may still be
// handed out. Only a definitive "not a mountpoint" answers false: a
// check error, a check that outlives its timeout, and a nil MountChecker
// all preserve the cached path.
//
// That bias is the opposite of AdoptExisting's, deliberately. There, a
// wrong guess costs one redundant mount(8) onto a directory that is not
// mounted. Here it would stack a second mount over a target that is in
// fact still mounted, on every Get, with nothing that ever unstacks it.
// A definitive false cannot stack, because there is no mount under it.
func (r *Registry) stillMounted(ctx context.Context, id, path string) bool {
	if r.mp == nil {
		return true
	}
	res, ok := r.checkMountPoint(ctx, id, path)
	switch {
	case !ok:
		r.log.Warn("mountpoint check did not return in time; assuming backing store is still mounted",
			"storeID", id, "path", path, "timeout", r.checkTimeout)
		return true
	case res.err != nil:
		r.log.Warn("mountpoint check failed; assuming backing store is still mounted",
			"storeID", id, "path", path, "err", res.err)
		return true
	default:
		return res.mounted
	}
}

// checkMountPoint runs one IsMountPoint under r.checkTimeout, keeping at
// most one check in flight per storeID. The goroutine is necessary
// because a stat against a hung hard-mounted NFS target blocks forever
// and ignores context cancellation; abandoning it is the only way out.
// A subsequent Get therefore discards an abandoned check's result and
// re-checks, rather than starting a second concurrent check and leaking
// a goroutine per kubelet retry. ok is false when no usable answer is
// available.
func (r *Registry) checkMountPoint(ctx context.Context, id, path string) (checkRes, bool) {
	r.mu.Lock()
	ch, inFlight := r.checking[id]
	if !inFlight {
		ch = make(chan checkRes, 1)
		r.checking[id] = ch
	}
	r.mu.Unlock()

	if inFlight {
		// Collect an abandoned check's result only to release the slot,
		// never to act on: it describes the mount at some unknown
		// earlier moment, and evicting on it could tear down a mount
		// that has since come back. The next Get starts a fresh,
		// authoritative check.
		select {
		case <-ch:
			r.clearCheck(id)
		default:
		}
		return checkRes{}, false
	}

	go func() {
		// WithoutCancel: this check outlives the Get that started it, so
		// it must not die with that caller's RPC context — its result is
		// what unblocks the next Get.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.checkTimeout)
		defer cancel()
		mounted, err := r.mp.IsMountPoint(cctx, path)
		ch <- checkRes{mounted: mounted, err: err}
	}()

	timer := time.NewTimer(r.checkTimeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		r.clearCheck(id)
		return res, true
	case <-timer.C:
		return checkRes{}, false
	}
}

func (r *Registry) clearCheck(id string) {
	r.mu.Lock()
	delete(r.checking, id)
	r.mu.Unlock()
}

func (r *Registry) lockStore(id string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.storeMu[id]; ok {
		return m
	}
	m := &sync.Mutex{}
	r.storeMu[id] = m
	return m
}

func (r *Registry) mounterFor(t Type) (Mounter, error) {
	switch t {
	case TypeNFS:
		if r.nfsM == nil {
			return nil, fmt.Errorf("nfs mounter not available")
		}
		return r.nfsM, nil
	case TypeLocal:
		if r.localM == nil {
			return nil, fmt.Errorf("local mounter not available")
		}
		return r.localM, nil
	default:
		return nil, fmt.Errorf("unknown backing-store type %q", t)
	}
}
