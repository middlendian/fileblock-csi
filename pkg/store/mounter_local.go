package store

import (
	"context"
	"fmt"

	"github.com/middlendian/fileblock-csi/pkg/mount"
)

// LocalMounter bind-mounts a host directory into the per-store target
// via pkg/mount.Mounter.BindMount — the same code path NodePublishVolume
// uses. The host directory must be visible inside the driver pod's
// mount namespace; with the default emptyDir cache mount, operators
// adding type=local SCs need to add a hostPath patch on the controller
// Deployment and node DaemonSet to surface that source dir into the
// pod (see README "Local backing-store: required overlay").
type LocalMounter struct {
	mnt *mount.Mounter
}

func NewLocalMounter(m *mount.Mounter) *LocalMounter {
	return &LocalMounter{mnt: m}
}

func (m *LocalMounter) Mount(ctx context.Context, target string, cfg Config) error {
	if cfg.Type != TypeLocal {
		return fmt.Errorf("LocalMounter: cfg.Type = %q, want local", cfg.Type)
	}
	if cfg.LocalPath == "" {
		return fmt.Errorf("LocalMounter: cfg.LocalPath is empty")
	}
	return m.mnt.BindMount(ctx, cfg.LocalPath, target, false)
}
