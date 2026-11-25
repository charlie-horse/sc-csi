package driver_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/charlie-horse/sc-csi/internal/driver"
	"github.com/charlie-horse/sc-csi/internal/driver/fake"
)

const (
	volID  = "vol-1"
	serial = "6000d31000abcdef0000000000000001"
)

type env struct {
	d       *driver.Driver
	conn    *fake.FakeConnector
	mnt     *fake.FakeMounter
	staging string
	target  string
	pc      map[string]string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	conn := fake.NewConnector(filepath.Join(dir, "dev"))
	if err := os.Mkdir(conn.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	mnt := fake.NewMounter()
	conn.InUse = mnt.InUse
	staging := filepath.Join(dir, "pv", "globalmount")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	return &env{
		d: &driver.Driver{
			Cfg:   driver.Config{NodeName: "node1"},
			Conn:  conn,
			Mount: mnt,
			WWPNs: func() ([]string, error) { return []string{"21000024ff000001", "21000024ff000002"}, nil },
		},
		conn: conn, mnt: mnt, staging: staging,
		target: filepath.Join(dir, "pod", "mount"),
		pc: map[string]string{
			driver.PubSerial: serial, driver.PubLUN: "3", driver.PubTargets: "5000d31000aa0001,5000d31000aa0002",
		},
	}
}

func mountCap(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

func blockCap() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

func wantCode(t *testing.T, err error, c codes.Code) {
	t.Helper()
	if status.Code(err) != c {
		t.Fatalf("got %v, want code %v", err, c)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) stage(c *csi.VolumeCapability) error {
	_, err := e.d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId: volID, StagingTargetPath: e.staging, VolumeCapability: c, PublishContext: e.pc,
	})
	return err
}

func (e *env) publish(c *csi.VolumeCapability, target string, ro bool) error {
	_, err := e.d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: volID, StagingTargetPath: e.staging, TargetPath: target,
		VolumeCapability: c, PublishContext: e.pc, Readonly: ro,
	})
	return err
}

func (e *env) unpublish(target string) error {
	_, err := e.d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{VolumeId: volID, TargetPath: target})
	return err
}

func (e *env) unstage() error {
	_, err := e.d.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{VolumeId: volID, StagingTargetPath: e.staging})
	return err
}

func TestNodeGetInfo(t *testing.T) {
	e := newEnv(t)
	r, err := e.d.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	must(t, err)
	if want := "node1;21000024ff000001,21000024ff000002"; r.GetNodeId() != want {
		t.Fatalf("node id %q, want %q", r.GetNodeId(), want)
	}
	e.d.WWPNs = func() ([]string, error) { return nil, nil }
	_, err = e.d.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	wantCode(t, err, codes.FailedPrecondition)
	e.d.WWPNs = func() ([]string, error) { return nil, errors.New("no sysfs") }
	_, err = e.d.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	wantCode(t, err, codes.Internal)
}

func TestNodeGetCapabilities(t *testing.T) {
	e := newEnv(t)
	r, err := e.d.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	must(t, err)
	got := map[csi.NodeServiceCapability_RPC_Type]bool{}
	for _, c := range r.GetCapabilities() {
		got[c.GetRpc().GetType()] = true
	}
	for _, w := range []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME, csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS, csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	} {
		if !got[w] {
			t.Errorf("missing capability %v", w)
		}
	}
}

func TestMountLifecycle(t *testing.T) {
	e := newEnv(t)
	c := mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)

	for range 2 { // second call checks idempotency
		must(t, e.stage(c))
	}
	dev, ok := e.conn.Device(serial)
	if !ok {
		t.Fatal("volume not attached after stage")
	}
	m := e.mnt.Mounts()
	if got := m[e.staging]; got.Source != dev.Path || got.FSType != "xfs" {
		t.Fatalf("staging mount %+v, want %s xfs", got, dev.Path)
	}

	for range 2 {
		must(t, e.publish(c, e.target, true))
	}
	if got := e.mnt.Mounts()[e.target]; got.Source != e.staging || !got.ReadOnly {
		t.Fatalf("target mount %+v, want read-only bind of staging", got)
	}

	// Unstage must unmount before it disconnects: the fake connector refuses
	// a device that is still mounted.
	for range 2 {
		must(t, e.unpublish(e.target))
	}
	if _, err := os.Stat(e.target); !os.IsNotExist(err) {
		t.Fatalf("target still exists: %v", err)
	}
	for range 2 {
		must(t, e.unstage())
	}
	if a := e.conn.Attached(); len(a) != 0 {
		t.Fatalf("still attached: %v", a)
	}
	if len(e.mnt.Mounts()) != 0 {
		t.Fatalf("mounts left: %v", e.mnt.Mounts())
	}
	entries, _ := os.ReadDir(filepath.Dir(e.staging))
	if len(entries) != 1 {
		t.Fatalf("state files left next to staging path: %v", entries)
	}
}

func TestBlockLifecycle(t *testing.T) {
	e := newEnv(t)
	c := blockCap()
	must(t, e.stage(c))
	must(t, e.stage(c))
	if len(e.mnt.Mounts()) != 0 {
		t.Fatalf("block stage mounted something: %v", e.mnt.Mounts())
	}
	dev, _ := e.conn.Device(serial)
	must(t, e.publish(c, e.target, false))
	must(t, e.publish(c, e.target, false))
	fi, err := os.Stat(e.target)
	must(t, err)
	if !fi.Mode().IsRegular() {
		t.Fatalf("block target is not a file: %v", fi.Mode())
	}
	if got := e.mnt.Mounts()[e.target]; got.Source != dev.Path {
		t.Fatalf("target bound from %q, want %q", got.Source, dev.Path)
	}

	r, err := e.d.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: volID, VolumePath: e.target})
	must(t, err)
	if u := r.GetUsage(); len(u) != 1 || u[0].GetTotal() != fake.DefaultDeviceSize {
		t.Fatalf("block stats %v", u)
	}

	// Unstaging while still published must fail, since the device is in use.
	if err := e.unstage(); err == nil {
		t.Fatal("unstage of a published block volume succeeded")
	}
	must(t, e.unpublish(e.target))
	must(t, e.unstage())
	if a := e.conn.Attached(); len(a) != 0 {
		t.Fatalf("still attached: %v", a)
	}
}

func TestUnstageNothingStaged(t *testing.T) {
	e := newEnv(t)
	must(t, e.unstage())
	must(t, e.unpublish(e.target))
}

func TestPublishNotStaged(t *testing.T) {
	e := newEnv(t)
	wantCode(t, e.publish(mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER), e.target, false), codes.FailedPrecondition)
}

func TestSingleWriterSecondTarget(t *testing.T) {
	e := newEnv(t)
	c := mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER)
	must(t, e.stage(c))
	must(t, e.publish(c, e.target, false))
	other := e.target + "2"
	wantCode(t, e.publish(c, other, false), codes.FailedPrecondition)
	must(t, e.unpublish(e.target))
	must(t, e.publish(c, other, false))

	multi := mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER)
	must(t, e.publish(multi, e.target, false))
}

func TestStageOtherVolumeSamePath(t *testing.T) {
	e := newEnv(t)
	c := mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	must(t, e.stage(c))
	_, err := e.d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId: "vol-2", StagingTargetPath: e.staging, VolumeCapability: c, PublishContext: e.pc,
	})
	wantCode(t, err, codes.AlreadyExists)
}

func TestInvalidArguments(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	noKind := &csi.VolumeCapability{AccessMode: c.AccessMode}
	multiMount := mountCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)
	call := map[string]func() error{
		"stage no id": func() error {
			_, err := e.d.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{StagingTargetPath: e.staging, VolumeCapability: c, PublishContext: e.pc})
			return err
		},
		"stage no path": func() error {
			_, err := e.d.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{VolumeId: volID, VolumeCapability: c, PublishContext: e.pc})
			return err
		},
		"stage no cap": func() error {
			_, err := e.d.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{VolumeId: volID, StagingTargetPath: e.staging, PublishContext: e.pc})
			return err
		},
		"stage no access type": func() error {
			_, err := e.d.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{VolumeId: volID, StagingTargetPath: e.staging, VolumeCapability: noKind, PublishContext: e.pc})
			return err
		},
		"stage multi-node mount": func() error {
			_, err := e.d.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{VolumeId: volID, StagingTargetPath: e.staging, VolumeCapability: multiMount, PublishContext: e.pc})
			return err
		},
		"stage no serial": func() error {
			pc := map[string]string{driver.PubLUN: "1", driver.PubTargets: "a"}
			_, err := e.d.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{VolumeId: volID, StagingTargetPath: e.staging, VolumeCapability: c, PublishContext: pc})
			return err
		},
		"stage bad lun": func() error {
			pc := map[string]string{driver.PubSerial: serial, driver.PubLUN: "x", driver.PubTargets: "a"}
			_, err := e.d.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{VolumeId: volID, StagingTargetPath: e.staging, VolumeCapability: c, PublishContext: pc})
			return err
		},
		"stage no targets": func() error {
			pc := map[string]string{driver.PubSerial: serial, driver.PubLUN: "1"}
			_, err := e.d.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{VolumeId: volID, StagingTargetPath: e.staging, VolumeCapability: c, PublishContext: pc})
			return err
		},
		"unstage no id": func() error {
			_, err := e.d.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{StagingTargetPath: e.staging})
			return err
		},
		"unstage no path": func() error {
			_, err := e.d.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{VolumeId: volID})
			return err
		},
		"publish no id": func() error {
			_, err := e.d.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{})
			return err
		},
		"publish no target": func() error {
			_, err := e.d.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{VolumeId: volID})
			return err
		},
		"publish no cap": func() error {
			_, err := e.d.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{VolumeId: volID, TargetPath: e.target, StagingTargetPath: e.staging})
			return err
		},
		"publish no staging": func() error {
			_, err := e.d.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{VolumeId: volID, TargetPath: e.target, VolumeCapability: c})
			return err
		},
		"unpublish no id": func() error {
			_, err := e.d.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{TargetPath: e.target})
			return err
		},
		"unpublish no target": func() error {
			_, err := e.d.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{VolumeId: volID})
			return err
		},
		"expand no id": func() error {
			_, err := e.d.NodeExpandVolume(ctx, &csi.NodeExpandVolumeRequest{VolumePath: e.target})
			return err
		},
		"expand no path": func() error {
			_, err := e.d.NodeExpandVolume(ctx, &csi.NodeExpandVolumeRequest{VolumeId: volID})
			return err
		},
		"stats no id": func() error {
			_, err := e.d.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{VolumePath: e.target})
			return err
		},
		"stats no path": func() error {
			_, err := e.d.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{VolumeId: volID})
			return err
		},
	}
	for name, f := range call {
		t.Run(name, func(t *testing.T) { wantCode(t, f(), codes.InvalidArgument) })
	}
	if a := e.conn.Attached(); len(a) != 0 {
		t.Fatalf("invalid calls attached volumes: %v", a)
	}
}

func TestExpandAndStats(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	must(t, e.stage(c))
	must(t, e.publish(c, e.target, false))

	const grown = 2 << 30
	must(t, e.conn.SetSize(serial, grown))
	// No staging path: the driver finds it from the target path.
	r, err := e.d.NodeExpandVolume(ctx, &csi.NodeExpandVolumeRequest{
		VolumeId: volID, VolumePath: e.target, CapacityRange: &csi.CapacityRange{RequiredBytes: grown},
	})
	must(t, err)
	if r.GetCapacityBytes() != grown {
		t.Fatalf("capacity %d, want %d", r.GetCapacityBytes(), grown)
	}
	dev, _ := e.conn.Device(serial)
	if e.conn.Resizes(serial) != 1 || e.mnt.Resizes(dev.Path) != 1 {
		t.Fatalf("resizes: connector %d, fs %d", e.conn.Resizes(serial), e.mnt.Resizes(dev.Path))
	}

	s, err := e.d.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{VolumeId: volID, VolumePath: e.target})
	must(t, err)
	if u := s.GetUsage(); len(u) != 2 || u[0].GetTotal() != grown || u[0].GetUsed()+u[0].GetAvailable() != grown {
		t.Fatalf("stats %v", u)
	}

	_, err = e.d.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{VolumeId: volID, VolumePath: "some/path"})
	wantCode(t, err, codes.NotFound)
	_, err = e.d.NodeExpandVolume(ctx, &csi.NodeExpandVolumeRequest{VolumeId: volID, VolumePath: "some/path"})
	wantCode(t, err, codes.NotFound)
	_, err = e.d.NodeExpandVolume(ctx, &csi.NodeExpandVolumeRequest{VolumeId: "vol-other", VolumePath: e.target})
	wantCode(t, err, codes.NotFound)
}

func TestExpandBlock(t *testing.T) {
	e := newEnv(t)
	c := blockCap()
	must(t, e.stage(c))
	must(t, e.publish(c, e.target, false))
	must(t, e.conn.SetSize(serial, 3<<30))
	r, err := e.d.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId: volID, VolumePath: e.target, StagingTargetPath: e.staging, VolumeCapability: c,
	})
	must(t, err)
	if r.GetCapacityBytes() != 3<<30 {
		t.Fatalf("capacity %d", r.GetCapacityBytes())
	}
	dev, _ := e.conn.Device(serial)
	if e.mnt.Resizes(dev.Path) != 0 {
		t.Fatal("block expand resized a filesystem")
	}
}
