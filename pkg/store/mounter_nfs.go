package store

import (
	"context"
	"fmt"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
)

// NFSMounter shells out to mount(8) with `-t nfs`, which dispatches to
// the generic mount.nfs helper from nfs-common. Both NFSv3 and NFSv4
// are supported — the version is selected via the nfsvers= option, or
// auto-negotiated when omitted.
//
// Why mount(8) and not mount.nfs(8) directly: csi-driver-nfs goes
// through mount(8) (via k8s.io/mount-utils), and an earlier draft of
// this driver invoked mount.nfs directly — which on debian:trixie-slim
// surfaced "Protocol not supported" failures that mount(8) does not
// reproduce. mount(8) does additional argument preprocessing before
// dispatching to the helper that, on this image at least, the helper
// requires to behave correctly. Match csi-driver-nfs's pattern.
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
	args := []string{"-t", "nfs"}
	if cfg.NFSMountOptions != "" {
		args = append(args, "-o", cfg.NFSMountOptions)
	}
	args = append(args, source, target)
	if _, err := m.exec.Run(ctx, "mount", args...); err != nil {
		return fmt.Errorf("mount -t nfs %s -> %s: %w", source, target, err)
	}
	return nil
}
