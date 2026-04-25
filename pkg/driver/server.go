package driver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
)

// Server wires a gRPC server onto a unix socket and registers the CSI
// services that were passed in. Pass nil for a service to skip it.
type Server struct {
	endpoint   string
	identity   csi.IdentityServer
	controller csi.ControllerServer
	node       csi.NodeServer
	log        *slog.Logger

	grpc *grpc.Server
}

func NewServer(endpoint string, log *slog.Logger,
	identity csi.IdentityServer,
	controller csi.ControllerServer,
	node csi.NodeServer,
) *Server {
	return &Server{
		endpoint:   endpoint,
		identity:   identity,
		controller: controller,
		node:       node,
		log:        log,
	}
}

// Serve blocks until the context is cancelled or the server fails.
func (s *Server) Serve(ctx context.Context) error {
	scheme, addr, err := parseEndpoint(s.endpoint)
	if err != nil {
		return err
	}
	if scheme == "unix" {
		// A stale socket from a previous run blocks Listen.
		_ = os.Remove(addr)
	}
	lis, err := net.Listen(scheme, addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.endpoint, err)
	}

	s.grpc = grpc.NewServer(grpc.UnaryInterceptor(s.logInterceptor))
	if s.identity != nil {
		csi.RegisterIdentityServer(s.grpc, s.identity)
	}
	if s.controller != nil {
		csi.RegisterControllerServer(s.grpc, s.controller)
	}
	if s.node != nil {
		csi.RegisterNodeServer(s.grpc, s.node)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- s.grpc.Serve(lis) }()

	s.log.Info("csi server listening", "endpoint", s.endpoint)
	select {
	case <-ctx.Done():
		s.grpc.GracefulStop()
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) logInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	if err != nil {
		s.log.Error("rpc failed", "method", info.FullMethod, "err", err)
	} else {
		s.log.Debug("rpc ok", "method", info.FullMethod)
	}
	return resp, err
}

func parseEndpoint(ep string) (string, string, error) {
	if strings.HasPrefix(ep, "/") {
		return "unix", ep, nil
	}
	u, err := url.Parse(ep)
	if err != nil {
		return "", "", err
	}
	switch u.Scheme {
	case "unix":
		path := u.Path
		if path == "" {
			path = u.Host + u.Path
		}
		return "unix", path, nil
	case "tcp":
		return "tcp", u.Host, nil
	default:
		return "", "", errors.New("unsupported endpoint scheme: " + u.Scheme)
	}
}
