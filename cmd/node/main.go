// fileblock-node is the CSI node plugin. It owns loop-device attach/detach,
// the mount stack inside the pod, and the persistent state file.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/middlendian/fileblock-csi/pkg/crypt"
	"github.com/middlendian/fileblock-csi/pkg/driver"
	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/kube"
	"github.com/middlendian/fileblock-csi/pkg/loop"
	"github.com/middlendian/fileblock-csi/pkg/mount"
	"github.com/middlendian/fileblock-csi/pkg/store"
)

func main() {
	endpoint := flag.String("endpoint", "unix:///csi/csi.sock", "CSI endpoint (unix:// or tcp://)")
	nodeID := flag.String("node-id", os.Getenv("NODE_NAME"), "node identifier; defaults to $NODE_NAME")
	stateDir := flag.String("state-dir", "/var/lib/kubelet/plugins/fileblock.csi", "directory for the loop-mappings state file")
	storesRoot := flag.String("stores-root", "/var/lib/fileblock/stores", "directory under which each backing store is mounted at <id>/")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	autoDelete := flag.Bool("auto-delete-pod-when-volume-failed", true,
		"delete the controller-owned pods on this node that use a volume whose ext4 was forced read-only, so it is re-staged; needs the fileblock-node-pod-delete admission policy")
	flag.Parse()

	log := newLogger(*logLevel)

	if *nodeID == "" {
		log.Error("--node-id (or $NODE_NAME) is required")
		os.Exit(2)
	}
	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		log.Error("create state dir", "err", err)
		os.Exit(2)
	}
	if err := os.MkdirAll(*storesRoot, 0o755); err != nil {
		log.Error("create stores root", "err", err)
		os.Exit(2)
	}

	exec := fbexec.New(0)
	mnt := mount.New(exec)
	losetup := loop.NewLosetup(exec)
	state, err := loop.LoadState(filepath.Join(*stateDir, "loop-mappings.json"))
	if err != nil {
		log.Error("load state", "err", err)
		os.Exit(2)
	}

	luks := crypt.New(exec)
	// cryptsetup takes its LUKS2 locks here and only warns without it.
	if err := os.MkdirAll("/run/cryptsetup", 0o700); err != nil {
		log.Warn("create /run/cryptsetup", "err", err)
	}

	// Reconcile any orphan loop devices anywhere under storesRoot. The
	// reconciler's prefix check handles all per-store subdirs uniformly.
	rec := loop.NewReconciler(state, losetup, luks, *storesRoot)
	if err := rec.Reconcile(context.Background()); err != nil {
		log.Warn("reconcile failed at startup", "err", err)
	}

	registry := store.NewRegistry(*storesRoot, store.NewNFSMounter(exec), store.NewLocalMounter(mnt), mnt, log)
	if err := registry.AdoptExisting(context.Background()); err != nil {
		log.Warn("adopt existing stores failed at startup", "err", err)
	}

	identity := driver.NewIdentityServer(false)
	node := driver.NewNodeServer(*nodeID, exec, mnt, losetup, state, log, registry)
	srv := driver.NewServer(*endpoint, log, identity, nil, node)

	// Without API access the plugin still serves CSI; failures are only logged.
	kc, err := kube.InCluster()
	if err != nil {
		log.Warn("no Kubernetes API access; failed volumes are only logged", "err", err)
		kc = nil
	}
	deleting := false
	if kc != nil {
		node.SetKube(kc)
		if *autoDelete {
			// Checked once at startup; applying the policy later needs a restart.
			ok, perr := kc.PolicyEnforced(context.Background(), driver.PodDeletePolicyName)
			switch {
			case perr != nil:
				log.Error("cannot verify the pod-delete admission policy; failed volumes are only reported", "err", perr)
			case !ok:
				log.Error("admission policy " + driver.PodDeletePolicyName + " or its binding is missing, or does not Deny; " +
					"failed volumes are only reported until it is applied and the plugin restarts")
			default:
				deleting = true
			}
		}
	}
	mon := &driver.Monitor{
		Node:       *nodeID,
		State:      state,
		Failures:   node.Failures(),
		AutoDelete: deleting,
		MountInfo:  func() ([]mount.MountInfo, error) { return mount.ReadMountInfo("/proc/self/mountinfo") },
		Now:        time.Now,
		Log:        log,
	}
	if kc != nil {
		// Assign only when non-nil: a typed-nil *HTTPClient in the interface is not nil.
		mon.Kube = kc
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go mon.Run(ctx)
	if err := srv.Serve(ctx); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
