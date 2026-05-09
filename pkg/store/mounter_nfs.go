package store

import (
	"context"
	"fmt"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
)

// NFSMounter shells out to mount.nfs (the generic helper from
// nfs-common). The same binary handles both NFSv3 and NFSv4 — the
// version is selected via the nfsvers= option, or auto-negotiated when
// omitted.
type NFSMounter struct {
	exec fbexec.Runner
}

func NewNFSMounter(r fbexec.Runner) *NFSMounter {
	return &NFSMounter{exec: r}
}

func (m *NFSMounter) Mount(ctx context.Context, target string, cfg Config) error {
	if cfg.Type != TypeNFS {
		return fmt.Errorf("NFSMounter: cfg.Type = %q, want nfs", cfg.Type)
	}
	if cfg.NFSServer == "" {
		return fmt.Errorf("NFSMounter: cfg.NFSServer is empty")
	}
	if cfg.NFSPath == "" {
		return fmt.Errorf("NFSMounter: cfg.NFSPath is empty")
	}
	source := cfg.NFSServer + ":" + cfg.NFSPath
	args := make([]string, 0, 4)
	if cfg.NFSMountOptions != "" {
		args = append(args, "-o", cfg.NFSMountOptions)
	}
	args = append(args, source, target)
	if _, err := m.exec.Run(ctx, "mount.nfs", args...); err != nil {
		return fmt.Errorf("mount.nfs %s -> %s: %w", source, target, err)
	}
	return nil
}
