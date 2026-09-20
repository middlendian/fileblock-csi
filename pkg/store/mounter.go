package store

import "context"

// Mounter knows how to mount one Type's source into a target directory.
// The Registry calls Mount once per storeID, and again only when a
// previously established mount is found to have gone away — so an
// implementation can assume the target is unmounted, but not that it
// has never been mounted before. Neither current implementation is
// idempotent over a target that is still mounted: mount(8) would stack
// a second mount there rather than no-op, and nothing unstacks it. That
// is why Registry.Get remounts only on a definitive "not a mountpoint"
// and treats every unusable answer as "still mounted".
type Mounter interface {
	Mount(ctx context.Context, target string, cfg Config) error
}
