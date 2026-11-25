package csiserver

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func TestParseEndpoint(t *testing.T) {
	for _, tc := range []struct{ in, network, addr string }{
		{"unix:///csi/csi.sock", "unix", "/csi/csi.sock"},
		{"unix:/tmp/x.sock", "unix", "/tmp/x.sock"},
		{"tcp://127.0.0.1:10000", "tcp", "127.0.0.1:10000"},
	} {
		n, a, err := ParseEndpoint(tc.in)
		if err != nil || n != tc.network || a != tc.addr {
			t.Errorf("%s: got %q %q %v", tc.in, n, a, err)
		}
	}
	for _, bad := range []string{"", "/csi/csi.sock", "unix://", "http://x"} {
		if _, _, err := ParseEndpoint(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

type identity struct {
	csi.UnimplementedIdentityServer
}

func (identity) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

func TestServeReplacesStaleSocketAndStops(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "csi.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil { // stale file from an earlier run
		t.Fatal(err)
	}
	srv := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	csi.RegisterIdentityServer(srv, identity{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, "unix://"+sock, srv) }()

	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
	defer ccancel()
	c := csi.NewIdentityClient(conn)
	if _, err := c.Probe(cctx, &csi.ProbeRequest{}, grpc.WaitForReady(true)); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if _, err := c.GetPluginInfo(cctx, &csi.GetPluginInfoRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("GetPluginInfo: want Unimplemented, got %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop")
	}
}
