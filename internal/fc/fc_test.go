package fc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dell/gobrick"

	"github.com/charlie-horse/sc-csi/internal/driver"
)

const (
	serial = "6000d31000fa0c000000000000000123"
	wwid   = "3" + serial
	tgtA   = "5000d31000fa0c01"
	tgtB   = "5000d31000fa0c02"
)

// write creates root/rel with content, making parent directories.
func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeSys builds a sysfs tree with dm-3 over sdb and sdc for serial, plus an
// unrelated sdd.
func fakeSys(t *testing.T) string {
	sys := t.TempDir()
	write(t, sys, "block/dm-3/dm/uuid", "mpath-"+wwid+"\n")
	write(t, sys, "block/dm-3/dm/name", wwid+"\n")
	write(t, sys, "block/dm-3/slaves/sdb", "")
	write(t, sys, "block/dm-3/slaves/sdc", "")
	write(t, sys, "block/dm-4/dm/uuid", "part1-mpath-"+wwid+"\n")
	write(t, sys, "block/sdb/device/wwid", "naa."+serial+"\n")
	write(t, sys, "block/sdc/device/wwid", "naa."+strings.ToUpper(serial)+"\n")
	write(t, sys, "block/sdd/device/wwid", "naa.6000d31000fa0c000000000000000999\n")
	for _, sd := range []string{"sdb", "sdc", "sdd"} {
		write(t, sys, "block/"+sd+"/device/rescan", "")
	}
	return sys
}

type fakeBrick struct {
	dev       gobrick.Device
	err       error
	got       gobrick.FCVolumeInfo
	discNames []string
	onDisc    func(name string)
}

func (f *fakeBrick) ConnectVolume(_ context.Context, info gobrick.FCVolumeInfo) (gobrick.Device, error) {
	f.got = info
	return f.dev, f.err
}

func (f *fakeBrick) DisconnectVolumeByDeviceName(_ context.Context, name string) error {
	f.discNames = append(f.discNames, name)
	if f.onDisc != nil {
		f.onDisc(name)
	}
	return nil
}

type call struct {
	name string
	args []string
}

type fakeExec struct {
	calls []call
	out   string
}

func (f *fakeExec) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{name, args})
	return []byte(f.out), nil
}

func TestNormalizeWWN(t *testing.T) {
	for _, in := range []string{
		serial, strings.ToUpper(serial), wwid, "naa." + serial, "0x" + serial, " " + wwid + "\n",
	} {
		if got := NormalizeWWN(in); got != serial {
			t.Errorf("NormalizeWWN(%q) = %q", in, got)
		}
	}
	if SameWWN("", "") {
		t.Error("empty WWNs must not match")
	}
	if SameWWN(serial, "6000d31000fa0c000000000000000124") {
		t.Error("different WWNs matched")
	}
}

func TestVolumeInfo(t *testing.T) {
	info, err := volumeInfo(driver.FCVolume{Serial: serial, LUN: 7,
		Targets: []string{"0x5000D31000FA0C01", "50:00:d3:10:00:fa:0c:02"}})
	if err != nil {
		t.Fatal(err)
	}
	want := gobrick.FCVolumeInfo{Lun: 7, Targets: []gobrick.FCTargetInfo{{WWPN: tgtA}, {WWPN: tgtB}}}
	if !reflect.DeepEqual(info, want) {
		t.Fatalf("got %+v want %+v", info, want)
	}
	for _, bad := range []driver.FCVolume{
		{LUN: 1},
		{LUN: -1, Targets: []string{tgtA}},
		{LUN: 1, Targets: []string{"nothex"}},
	} {
		if _, err := volumeInfo(bad); err == nil {
			t.Errorf("volumeInfo(%+v) succeeded", bad)
		}
	}
}

func TestConnectMapperPath(t *testing.T) {
	sys, dev := fakeSys(t), t.TempDir()
	write(t, dev, "mapper/"+wwid, "")
	b := &fakeBrick{dev: gobrick.Device{WWN: wwid, Name: "dm-3", MultipathID: wwid}}
	c := newWithBrick(Options{SysRoot: sys, DevRoot: dev}, b)
	d, err := c.Connect(context.Background(), driver.FCVolume{Serial: strings.ToUpper(serial), LUN: 3, Targets: []string{tgtA}})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dev, "mapper", wwid); d.Path != want || d.WWN != serial {
		t.Fatalf("got %+v, want path %s", d, want)
	}
	if b.got.Lun != 3 || b.got.Targets[0].WWPN != tgtA {
		t.Fatalf("gobrick got %+v", b.got)
	}
}

func TestConnectFallbacks(t *testing.T) {
	sys, dev := fakeSys(t), t.TempDir()
	b := &fakeBrick{dev: gobrick.Device{WWN: wwid, Name: "dm-3"}}
	c := newWithBrick(Options{SysRoot: sys, DevRoot: dev}, b)
	v := driver.FCVolume{Serial: serial, LUN: 3, Targets: []string{tgtA}}
	d, err := c.Connect(context.Background(), v)
	if err != nil || d.Path != filepath.Join(dev, "dm-3") {
		t.Fatalf("no /dev/mapper node: got %+v, %v", d, err)
	}
	b.dev = gobrick.Device{WWN: wwid, Name: "sdb"}
	d, err = c.Connect(context.Background(), v)
	if err != nil || d.Path != filepath.Join(dev, "sdb") {
		t.Fatalf("single path: got %+v, %v", d, err)
	}
}

func TestConnectWrongWWN(t *testing.T) {
	b := &fakeBrick{dev: gobrick.Device{WWN: "36000d31000fa0c000000000000000999", Name: "dm-9"}}
	c := newWithBrick(Options{SysRoot: t.TempDir()}, b)
	_, err := c.Connect(context.Background(), driver.FCVolume{Serial: serial, LUN: 3, Targets: []string{tgtA}})
	if err == nil || !strings.Contains(err.Error(), "want "+serial) {
		t.Fatalf("got %v", err)
	}
	b.err, b.dev = errors.New("boom"), gobrick.Device{}
	if _, err := c.Connect(context.Background(), driver.FCVolume{Serial: serial, LUN: 3, Targets: []string{tgtA}}); err == nil {
		t.Fatal("gobrick error not returned")
	}
}

func TestDisconnect(t *testing.T) {
	sys := fakeSys(t)
	b := &fakeBrick{onDisc: func(string) {
		for _, p := range []string{"block/dm-3", "block/sdb", "block/sdc"} {
			_ = os.RemoveAll(filepath.Join(sys, p))
		}
	}}
	c := newWithBrick(Options{SysRoot: sys}, b)
	if err := c.Disconnect(context.Background(), wwid); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b.discNames, []string{"dm-3"}) {
		t.Fatalf("disconnected %v", b.discNames)
	}
	b.discNames = nil
	if err := c.Disconnect(context.Background(), serial); err != nil || b.discNames != nil {
		t.Fatalf("second disconnect: %v, %v", err, b.discNames)
	}
}

func TestDisconnectPathsWithoutMap(t *testing.T) {
	sys := fakeSys(t)
	_ = os.RemoveAll(filepath.Join(sys, "block/dm-3"))
	b := &fakeBrick{}
	c := newWithBrick(Options{SysRoot: sys}, b)
	err := c.Disconnect(context.Background(), serial)
	if !reflect.DeepEqual(b.discNames, []string{"sdb"}) {
		t.Fatalf("disconnected %v", b.discNames)
	}
	if err == nil {
		t.Fatal("devices left behind must be an error")
	}
}

func TestResize(t *testing.T) {
	sys := fakeSys(t)
	ex := &fakeExec{out: "ok\n"}
	c := newWithBrick(Options{SysRoot: sys, Chroot: "/host", Exec: ex.run}, &fakeBrick{})
	if err := c.Resize(context.Background(), serial); err != nil {
		t.Fatal(err)
	}
	for _, sd := range []string{"sdb", "sdc"} {
		b, _ := os.ReadFile(filepath.Join(sys, "block", sd, "device", "rescan"))
		if string(b) != "1" {
			t.Errorf("%s not rescanned", sd)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(sys, "block/sdd/device/rescan")); len(b) != 0 {
		t.Error("unrelated sdd rescanned")
	}
	want := []call{{"chroot", []string{"/host", "multipathd", "resize", "map", wwid}}}
	if !reflect.DeepEqual(ex.calls, want) {
		t.Fatalf("calls %+v want %+v", ex.calls, want)
	}
	ex.out = "fail\n"
	if err := c.Resize(context.Background(), serial); err == nil {
		t.Fatal("multipathd fail not reported")
	}
	if err := c.Resize(context.Background(), "6000d31000fa0c000000000000000555"); !errors.Is(err, ErrNotAttached) {
		t.Fatalf("got %v", err)
	}
}

func TestCommandWithoutChroot(t *testing.T) {
	c := newWithBrick(Options{}, &fakeBrick{})
	name, args := c.command("multipathd", "show", "maps")
	if name != "multipathd" || !reflect.DeepEqual(args, []string{"show", "maps"}) {
		t.Fatalf("got %s %v", name, args)
	}
}

func TestPrependPath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	prependPath([]string{"/run/current-system/sw/bin", "/bin"})
	if got := os.Getenv("PATH"); got != "/run/current-system/sw/bin:/bin:/usr/bin" {
		t.Fatalf("PATH = %q", got)
	}
}

func TestWWPNs(t *testing.T) {
	sys := t.TempDir()
	write(t, sys, "class/fc_host/host1/port_name", "0x21000024FF7A0001\n")
	write(t, sys, "class/fc_host/host1/port_state", "Online\n")
	write(t, sys, "class/fc_host/host2/port_name", "0x21000024ff7a0002\n")
	write(t, sys, "class/fc_host/host2/port_state", "Linkdown\n")
	write(t, sys, "class/fc_host/host3/port_name", "0x21000024ff7a0000\n")
	write(t, sys, "class/fc_host/host3/port_state", "Online\n")
	write(t, sys, "class/fc_host/host4/port_name", "garbage\n")
	got, err := WWPNs(sys)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"21000024ff7a0000", "21000024ff7a0001"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("online: got %v want %v", got, want)
	}

	write(t, sys, "class/fc_host/host1/port_state", "Linkdown\n")
	write(t, sys, "class/fc_host/host3/port_state", "Linkdown\n")
	got, _ = newWithBrick(Options{SysRoot: sys}, &fakeBrick{}).WWPNs()
	if want := []string{"21000024ff7a0000", "21000024ff7a0001", "21000024ff7a0002"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("none online: got %v want %v", got, want)
	}

	if _, err := WWPNs(t.TempDir()); err == nil {
		t.Fatal("no fc_host must be an error")
	}
}
