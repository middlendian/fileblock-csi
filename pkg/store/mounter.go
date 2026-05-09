package store

import "context"

// Mounter knows how to mount one Type's source into a target directory.
// Implementations must be idempotent over a target that is already
// mounted with the same source — but the Registry guarantees Mount is
// not called twice for the same storeID, so the typical impl can assume
// target is empty.
type Mounter interface {
	Mount(ctx context.Context, target string, cfg Config) error
}
