//go:build fakearray

// Package sanity runs the kubernetes-csi sanity suite against a controller
// and a node Driver served in-process: the controller talks to the scfake
// array, the node uses the fake Connector and Mounter. No root is needed.
// Run with: go test -tags fakearray ./test/sanity/
package sanity

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"google.golang.org/grpc"

	"github.com/charlie-horse/sc-csi/internal/csiserver"
	"github.com/charlie-horse/sc-csi/internal/csp"
	"github.com/charlie-horse/sc-csi/internal/driver"
	"github.com/charlie-horse/sc-csi/internal/driver/fake"
	"github.com/charlie-horse/sc-csi/internal/sc"
	"github.com/charlie-horse/sc-csi/internal/sc/scfake"
)

// nodeWWPNs are the fake node's FC initiator ports. scfake accepts any WWPN
// that no server outside the driver's folder owns.
var nodeWWPNs = []string{"21000024ff000001", "21000024ff000002"}

func logger() *slog.Logger {
	if os.Getenv("SANITY_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// serve runs the services that register adds on a unix socket until the
// test ends, and returns the endpoint.
func serve(t *testing.T, log *slog.Logger, sock string, register func(*grpc.Server)) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := csiserver.NewServer(log)
	register(srv)
	done := make(chan error, 1)
	go func() { done <- csiserver.Serve(ctx, "unix://"+sock, srv) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve %s: %v", sock, err)
		}
	})
	return "unix://" + sock
}

func TestSanity(t *testing.T) {
	log := logger()
	dir := t.TempDir()

	a := scfake.New()
	a.AutoActive = true // sanity snapshots volumes nothing has written to
	t.Cleanup(a.Close)
	host, port := a.HostPort()
	ctx := context.Background()
	array, err := sc.New(ctx, sc.Config{Host: host, Port: port, User: a.User, Password: a.Password,
		Fingerprint: a.Fingerprint()}, log, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctrl := &driver.Driver{
		Cfg:   driver.Config{Version: "sanity"},
		Log:   log,
		CSP:   &csp.Service{Cfg: csp.Defaults(), Log: log},
		Array: array,
	}

	devDir := filepath.Join(dir, "dev")
	if err := os.Mkdir(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mnt := fake.NewMounter()
	conn := fake.NewConnector(devDir)
	conn.InUse = mnt.InUse
	node := &driver.Driver{
		Cfg:   driver.Config{NodeName: "sanity-node", Version: "sanity"},
		Log:   log,
		Conn:  conn,
		Mount: mnt,
		WWPNs: func() ([]string, error) { return nodeWWPNs, nil },
	}

	ctrlEP := serve(t, log, filepath.Join(dir, "c.sock"), func(s *grpc.Server) {
		csi.RegisterIdentityServer(s, ctrl)
		csi.RegisterControllerServer(s, ctrl)
	})
	// Sanity reads the plugin capabilities from the node endpoint, but only a
	// controller instance reports the controller service, so the node socket
	// serves the controller's Identity service.
	nodeEP := serve(t, log, filepath.Join(dir, "n.sock"), func(s *grpc.Server) {
		csi.RegisterIdentityServer(s, ctrl)
		csi.RegisterNodeServer(s, node)
	})

	cfg := sanity.NewTestConfig()
	cfg.Address = nodeEP
	cfg.ControllerAddress = ctrlEP
	cfg.TargetPath = filepath.Join(dir, "target")
	cfg.StagingPath = filepath.Join(dir, "staging")
	cfg.TestVolumeSize = 1 << 30
	cfg.TestVolumeExpandSize = 2 << 30
	cfg.IdempotentCount = 2
	sanity.Test(t, cfg)
}
