package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
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

// Registry mounts each unique store config once per process and hands
// out the resulting paths. Concurrency: per-storeID Mutex prevents two
// callers from racing into Mount; the global mu only guards the mounted
// map and the per-store mutex/config maps.
type Registry struct {
	root   string
	nfsM   Mounter
	localM Mounter
	mp     MountChecker

	mu      sync.Mutex
	mounted map[string]string      // storeID -> mounted path
	configs map[string]Config      // storeID -> Config that produced this storeID
	storeMu map[string]*sync.Mutex // storeID -> per-store lock
}

// NewRegistry returns a Registry that mounts under root. mounters may be
// nil if the corresponding type is not supported in this binary. mp is
// used by AdoptExisting to verify candidate directories are live mounts
// before adopting them — without it, a stale <storeID> directory in an
// emptyDir cache would poison the mounted-paths map after a container
// restart.
func NewRegistry(root string, nfs Mounter, local Mounter, mp MountChecker) *Registry {
	return &Registry{
		root:    root,
		nfsM:    nfs,
		localM:  local,
		mp:      mp,
		mounted: map[string]string{},
		configs: map[string]Config{},
		storeMu: map[string]*sync.Mutex{},
	}
}

// Get ensures cfg's source is mounted under <root>/<storeID>/ and
// returns that path. Idempotent: subsequent calls with the same cfg fast
// path on the cached mount. Also caches cfg keyed by storeID so callers
// that hold only a storeID (controller's DeleteVolume / Expand path) can
// resolve back to a Config via ConfigByStoreID.
func (r *Registry) Get(ctx context.Context, cfg Config) (string, error) {
	id := cfg.ID()
	storeMu := r.lockStore(id)
	storeMu.Lock()
	defer storeMu.Unlock()

	r.mu.Lock()
	if path, ok := r.mounted[id]; ok {
		r.mu.Unlock()
		return path, nil
	}
	r.mu.Unlock()

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

// AdoptExisting walks r.root and treats every immediate subdirectory as
// a previously-mounted store, populating the mounted-paths cache so
// MountedPaths() / DeleteVolume / etc. can find them. It does NOT
// re-mount anything — the directory is assumed to still hold a live
// mount (true under hostPath caches that survive pod restarts).
//
// Caveats:
//   - Cannot reconstruct the full Config for adopted stores; only the
//     storeID is recovered (from the directory name). DeleteVolume
//     against an adopted-but-not-Get-ed store will return NotFound from
//     ConfigByStoreID. After the next CreateVolume against the SC,
//     the Config is registered and Delete works.
//   - Under emptyDir (the recommended cache shape) the directory is
//     empty after restart, so this is always a no-op.
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
		r.mounted[id] = filepath.Join(r.root, id)
	}
	return nil
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
