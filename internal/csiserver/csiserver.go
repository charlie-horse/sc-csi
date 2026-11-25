// Package csiserver serves CSI gRPC services on an endpoint such as
// unix:///csi/csi.sock, with a logging interceptor.
package csiserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// ParseEndpoint splits "unix:///path", "unix:/path" or "tcp://host:port" into
// a network and an address for net.Listen.
func ParseEndpoint(ep string) (network, addr string, err error) {
	switch {
	case strings.HasPrefix(ep, "unix://"):
		network, addr = "unix", strings.TrimPrefix(ep, "unix://")
	case strings.HasPrefix(ep, "unix:"):
		network, addr = "unix", strings.TrimPrefix(ep, "unix:")
	case strings.HasPrefix(ep, "tcp://"):
		network, addr = "tcp", strings.TrimPrefix(ep, "tcp://")
	default:
		return "", "", fmt.Errorf("endpoint %q: want unix:///path or tcp://host:port", ep)
	}
	if addr == "" {
		return "", "", fmt.Errorf("endpoint %q has no address", ep)
	}
	return network, addr, nil
}

// Listen opens the endpoint. A leftover unix socket file from an earlier run
// is removed first.
func Listen(ep string) (net.Listener, error) {
	network, addr, err := ParseEndpoint(ep)
	if err != nil {
		return nil, err
	}
	if network == "unix" {
		if err := os.Remove(addr); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove old socket: %w", err)
		}
	}
	return net.Listen(network, addr)
}

// NewServer returns a gRPC server that logs every call. Register services on
// it, then call Serve.
func NewServer(log *slog.Logger) *grpc.Server {
	return grpc.NewServer(grpc.UnaryInterceptor(Logger(log)))
}

// Logger is a unary interceptor that logs the method, gRPC code and duration
// of each call. Request bodies are not logged: they can carry secrets.
// Probe and capability calls, which sidecars make often, log at debug level.
func Logger(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := h(ctx, req)
		st := status.Convert(err)
		lvl := slog.LevelInfo
		switch {
		case err != nil:
			lvl = slog.LevelWarn
		case quiet(info.FullMethod):
			lvl = slog.LevelDebug
		}
		attrs := []any{"method", info.FullMethod, "code", st.Code().String(), "duration", time.Since(start)}
		if err != nil {
			attrs = append(attrs, "err", st.Message())
		}
		log.Log(ctx, lvl, "grpc", attrs...)
		return resp, err
	}
}

func quiet(method string) bool {
	for _, s := range []string{"/Probe", "Capabilities", "/GetPluginInfo", "/NodeGetInfo", "/NodeGetVolumeStats"} {
		if strings.HasSuffix(method, s) {
			return true
		}
	}
	return false
}

// Serve runs srv on ep until ctx ends, then stops it gracefully.
func Serve(ctx context.Context, ep string, srv *grpc.Server) error {
	lis, err := Listen(ep)
	if err != nil {
		return err
	}
	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(lis) }()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		srv.GracefulStop()
		return nil
	}
}
