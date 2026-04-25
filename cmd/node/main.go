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

	"github.com/middlendian/fileblock-csi/pkg/driver"
	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/loop"
	"github.com/middlendian/fileblock-csi/pkg/mount"
)

func main() {
	endpoint := flag.String("endpoint", "unix:///csi/csi.sock", "CSI endpoint (unix:// or tcp://)")
	nodeID := flag.String("node-id", os.Getenv("NODE_NAME"), "node identifier; defaults to $NODE_NAME")
	stateDir := flag.String("state-dir", "/var/lib/kubelet/plugins/fileblock.csi", "directory for the loop-mappings state file")
	backingStore := flag.String("backing-store", "", "directory where .img files live; used only for reconciliation")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
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

	exec := fbexec.New(0)
	mnt := mount.New(exec)
	losetup := loop.NewLosetup(exec)
	state, err := loop.LoadState(filepath.Join(*stateDir, "loop-mappings.json"))
	if err != nil {
		log.Error("load state", "err", err)
		os.Exit(2)
	}

	if *backingStore != "" {
		rec := loop.NewReconciler(state, losetup, *backingStore)
		if err := rec.Reconcile(context.Background()); err != nil {
			log.Warn("reconcile failed at startup", "err", err)
		}
	} else {
		log.Warn("--backing-store not set; skipping startup reconciliation")
	}

	identity := driver.NewIdentityServer(false)
	node := driver.NewNodeServer(*nodeID, exec, mnt, losetup, state, log)
	srv := driver.NewServer(*endpoint, log, identity, nil, node)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
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
