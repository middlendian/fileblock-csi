// Package store owns the lookup from a StorageClass parameters / volume
// context map to a mounted directory the rest of the driver can write
// .img files into. Each unique backing-store config is mounted once per
// process; multiple StorageClasses pointing at the same source share
// one mount, keyed by a deterministic ID.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Type is the discriminator value of the `backingStore.type` SC parameter.
type Type string

const (
	TypeNFS   Type = "nfs"
	TypeLocal Type = "local"
)

// Config is the parsed shape of an SC's backingStore.* parameters. It is
// the input to MountID(), StoreID(), and Mounter.Mount.
type Config struct {
	Type Type

	// NFS-only.
	NFSServer       string
	NFSPath         string
	NFSMountOptions string
	// NFSSubDir places this store's .img files in a subdirectory of the
	// export rather than at its root. Empty means the export root, which
	// is the 0.3.x behaviour. Already substituted and cleaned by
	// ConfigFromParams.
	NFSSubDir string

	// Local-only.
	LocalPath   string
	LocalShared bool // true if local.path is shared across nodes (e.g. via OS-level
	// shared FS or kind extraMount); the controller then advertises
	// this PV as schedulable on any node, like NFS-type.
}

// mountKey is a stable string identifying the mount *source*: type,
// server, path and mount options. Field order is fixed; mountOptions are
// split on commas, empties dropped, sorted lexicographically, then
// rejoined so that "a,b" and "b,a" hash identically. Deliberately
// excludes NFSSubDir — every subDir under one export shares a single
// mount.
func (c Config) mountKey() string {
	switch c.Type {
	case TypeNFS:
		return strings.Join([]string{
			"nfs",
			c.NFSServer,
			c.NFSPath,
			canonicalOptions(c.NFSMountOptions),
		}, "|")
	case TypeLocal:
		return strings.Join([]string{
			"local",
			c.LocalPath,
		}, "|")
	}
	return "invalid|" + string(c.Type)
}

// storeKey is a stable string identifying the *store*: the mount source
// plus the subDir that .img files actually live in.
//
// The empty-subDir branch is load-bearing, not an optimization. It keeps
// the string byte-identical to the 0.3.x canonical form, so every
// storeID issued by an earlier release still resolves. Appending a
// separator unconditionally would re-hash every existing config and
// leave DeleteVolume unable to find images it then reports as deleted.
func (c Config) storeKey() string {
	if c.NFSSubDir == "" {
		return c.mountKey()
	}
	return c.mountKey() + "|" + c.NFSSubDir
}

func hashID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// MountID identifies the mount source. Two configs differing only in
// subDir share one MountID, and therefore one mount: mounting the same
// export once per namespace would be wasteful and would multiply the
// blast radius of a hung mount. Names the mount directory under
// --stores-root.
func (c Config) MountID() string { return hashID(c.mountKey()) }

// StoreID identifies the directory .img files live in — the mount source
// plus subDir. This is the storeID in a volumeID's "fb-<storeID>-"
// prefix and in volume context, which is how DeleteVolume and
// ControllerExpandVolume find a volume's home store.
//
// Invariant: StoreID() == MountID() exactly when no subDir is set, and
// both equal the value 0.3.x produced. See storeKey.
func (c Config) StoreID() string { return hashID(c.storeKey()) }

func canonicalOptions(opts string) string {
	parts := strings.Split(opts, ",")
	cleaned := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		cleaned = append(cleaned, p)
	}
	sort.Strings(cleaned)
	return strings.Join(cleaned, ",")
}
