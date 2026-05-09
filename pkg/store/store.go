// Package store owns the lookup from a StorageClass parameters / volume
// context map to a mounted directory the rest of the driver can write
// .img files into. Each unique backing-store config is mounted once per
// process; multiple StorageClasses pointing at the same source share
// one mount, keyed by a deterministic ID.
package store

// Type is the discriminator value of the `backingStore.type` SC parameter.
type Type string

const (
	TypeNFS   Type = "nfs"
	TypeLocal Type = "local"
)

// Config is the parsed shape of an SC's backingStore.* parameters. It is
// the input to ID(), Canonical(), and Mounter.Mount.
type Config struct {
	Type Type

	// NFS-only.
	NFSServer       string
	NFSPath         string
	NFSMountOptions string

	// Local-only.
	LocalPath string
}
