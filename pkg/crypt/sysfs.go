package crypt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// Mapping is an open fileblock dm-crypt device.
type Mapping struct {
	Name    string
	Backing string // "/dev/loopN", or "" if sysfs lists no slave
}

// List returns open fbcrypt-* mappings, read from sysfs so it needs no
// dmsetup and no udev.
func (c *Crypt) List(_ context.Context) ([]Mapping, error) {
	dms, err := filepath.Glob(filepath.Join(c.sysRoot, "block", "dm-*"))
	if err != nil {
		return nil, err
	}
	var out []Mapping
	for _, d := range dms {
		b, err := os.ReadFile(filepath.Join(d, "dm", "name")) // #nosec G304 -- sysfs path from Glob
		if err != nil {
			continue // removed while we scanned
		}
		name := strings.TrimSpace(string(b))
		if !strings.HasPrefix(name, mapperPrefix) {
			continue
		}
		m := Mapping{Name: name}
		if slaves, _ := os.ReadDir(filepath.Join(d, "slaves")); len(slaves) > 0 {
			m.Backing = "/dev/" + slaves[0].Name()
		}
		out = append(out, m)
	}
	return out, nil
}

func (c *Crypt) IsOpen(ctx context.Context, name string) (bool, error) {
	ms, err := c.List(ctx)
	if err != nil {
		return false, err
	}
	for _, m := range ms {
		if m.Name == name {
			return true, nil
		}
	}
	return false, nil
}
