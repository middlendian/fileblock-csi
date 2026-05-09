package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Registry mounts each unique store config once per process and hands
// out the resulting paths. Concurrency: per-storeID Mutex prevents two
// callers from racing into Mount; the global mu only guards the mounted
// map and the per-store mutex/config maps.
type Registry struct {
	root   string
	nfsM   Mounter
	localM Mounter

	mu      sync.Mutex
	mounted map[string]string      // storeID -> mounted path
	configs map[string]Config      // storeID -> Config that produced this storeID
	storeMu map[string]*sync.Mutex // storeID -> per-store lock
}

// NewRegistry returns a Registry that mounts under root. mounters may be
// nil if the corresponding type is not supported in this binary.
func NewRegistry(root string, nfs Mounter, local Mounter) *Registry {
	return &Registry{
		root:    root,
		nfsM:    nfs,
		localM:  local,
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
