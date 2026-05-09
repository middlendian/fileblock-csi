// fileblock-controller is the CSI controller plugin. It owns the lifecycle
// of .img files on the backing store and never touches the kernel.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/middlendian/fileblock-csi/pkg/driver"
	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/mount"
	"github.com/middlendian/fileblock-csi/pkg/store"
)

func main() {
	endpoint := flag.String("endpoint", "unix:///csi/csi.sock", "CSI endpoint (unix:// or tcp://)")
	storesRoot := flag.String("stores-root", "/var/lib/fileblock/stores", "directory under which each backing store is mounted at <id>/")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	flag.Parse()

	log := newLogger(*logLevel)

	if err := os.MkdirAll(*storesRoot, 0o755); err != nil {
		log.Error("create stores root", "err", err)
		os.Exit(2)
	}

	exec := fbexec.New(0)
	mnt := mount.New(exec)
	registry := store.NewRegistry(*storesRoot, store.NewNFSMounter(exec), store.NewLocalMounter(mnt))

	identity := driver.NewIdentityServer(true)
	controller := driver.NewControllerServer(registry, exec)
	srv := driver.NewServer(*endpoint, log, identity, controller, nil)

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
