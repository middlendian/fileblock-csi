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
	"github.com/middlendian/fileblock-csi/pkg/image"
)

func main() {
	endpoint := flag.String("endpoint", "unix:///csi/csi.sock", "CSI endpoint (unix:// or tcp://)")
	backingStore := flag.String("backing-store", "", "directory where .img files are stored (must be readable from every node)")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	flag.Parse()

	log := newLogger(*logLevel)

	if *backingStore == "" {
		log.Error("--backing-store is required")
		os.Exit(2)
	}
	if err := os.MkdirAll(*backingStore, 0o755); err != nil {
		log.Error("create backing store", "err", err)
		os.Exit(2)
	}

	exec := fbexec.New(0)
	images, err := image.New(*backingStore, exec)
	if err != nil {
		log.Error("open backing store", "err", err)
		os.Exit(2)
	}

	identity := driver.NewIdentityServer(true)
	controller := driver.NewControllerServer(images, *backingStore)
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
