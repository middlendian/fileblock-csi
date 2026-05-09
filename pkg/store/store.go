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

// Canonical returns a stable byte representation of the config used as
// the input to ID(). Field order is fixed; mountOptions are split on
// commas, empties dropped, sorted lexicographically, then rejoined so
// that "a,b" and "b,a" hash identically.
func (c Config) Canonical() []byte {
	switch c.Type {
	case TypeNFS:
		return []byte(strings.Join([]string{
			"nfs",
			c.NFSServer,
			c.NFSPath,
			canonicalOptions(c.NFSMountOptions),
		}, "|"))
	case TypeLocal:
		return []byte(strings.Join([]string{
			"local",
			c.LocalPath,
		}, "|"))
	}
	return []byte("invalid|" + string(c.Type))
}

// ID is a deterministic 12-char hex truncation of sha256(Canonical()).
func (c Config) ID() string {
	sum := sha256.Sum256(c.Canonical())
	return hex.EncodeToString(sum[:])[:12]
}

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
