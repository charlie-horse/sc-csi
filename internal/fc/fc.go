// Package fc attaches and detaches Fibre Channel LUNs on a node. It wraps the
// gobrick FC connector and reads the node's FC initiator ports from sysfs.
package fc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dell/gobrick"

	"github.com/charlie-horse/sc-csi/internal/driver"
)

// ErrNotAttached means no SCSI or multipath device carries the serial.
var ErrNotAttached = errors.New("volume is not attached on this node")

// Options configure a Connector. The zero value runs the multipath tools in
// the driver's own container and reads /sys and /dev.
type Options struct {
	// Chroot is the directory the host root is mounted at (e.g. /host).
	// When set, multipath, multipathd and dmsetup run as
	// "chroot <Chroot> <tool> ...", so they use the host's binaries and
	// /etc/multipath*. dd and all sysfs and /dev access stay in the container.
	Chroot string
	// ExtraPath lists directories put in front of the process PATH. PATH is
	// process wide and is also what chroot searches inside the host root, so
	// on NixOS this is /run/current-system/sw/bin.
	ExtraPath []string
	// SysRoot and DevRoot are where this package reads sysfs and /dev
	// (default /sys and /dev). gobrick itself always uses /sys and /dev.
	SysRoot string
	DevRoot string
	// WaitDeviceTimeout bounds how long gobrick waits for the SCSI and
	// multipath devices to appear (gobrick default 15s).
	WaitDeviceTimeout time.Duration
	// FlushTimeout bounds the whole multipath flush on detach (gobrick
	// default 120s).
	FlushTimeout time.Duration
	// Log receives gobrick's log lines. gobrick's logger is global, so the
	// last Connector created with a Log wins.
	Log *slog.Logger
	// Exec runs a command and returns its combined output. Tests replace it.
	Exec func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// brick is the part of gobrick's FCConnector this package uses.
type brick interface {
	ConnectVolume(ctx context.Context, info gobrick.FCVolumeInfo) (gobrick.Device, error)
	DisconnectVolumeByDeviceName(ctx context.Context, name string) error
}

// Connector implements driver.Connector with gobrick.
type Connector struct {
	opts  Options
	brick brick
	log   *slog.Logger
}

var _ driver.Connector = (*Connector)(nil)

var pathOnce sync.Once

// New builds a Connector. It prepends ExtraPath to PATH once per process.
func New(o Options) *Connector {
	o = withDefaults(o)
	if len(o.ExtraPath) > 0 {
		pathOnce.Do(func() { prependPath(o.ExtraPath) })
	}
	if o.Log != nil {
		gobrick.SetLogger(slogAdapter{o.Log})
	}
	b := gobrick.NewFCConnector(gobrick.FCConnectorParams{
		Chroot:                    o.Chroot,
		WaitDeviceRegisterTimeout: o.WaitDeviceTimeout,
		MultipathFlushTimeout:     o.FlushTimeout,
	})
	return newWithBrick(o, b)
}

func newWithBrick(o Options, b brick) *Connector {
	o = withDefaults(o)
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	return &Connector{opts: o, brick: b, log: log}
}

func withDefaults(o Options) Options {
	if o.SysRoot == "" {
		o.SysRoot = "/sys"
	}
	if o.DevRoot == "" {
		o.DevRoot = "/dev"
	}
	if o.Exec == nil {
		o.Exec = runCommand
	}
	return o
}

// prependPath puts dirs in front of PATH, skipping ones already present.
func prependPath(dirs []string) {
	cur := filepath.SplitList(os.Getenv("PATH"))
	seen := map[string]bool{}
	var out []string
	for _, d := range append(append([]string{}, dirs...), cur...) {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	_ = os.Setenv("PATH", strings.Join(out, string(os.PathListSeparator)))
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// command returns the program and arguments that run tool, through chroot
// when a host root is configured.
func (c *Connector) command(tool string, args ...string) (string, []string) {
	if c.opts.Chroot == "" {
		return tool, args
	}
	return "chroot", append([]string{c.opts.Chroot, tool}, args...)
}

func (c *Connector) run(ctx context.Context, tool string, args ...string) ([]byte, error) {
	name, argv := c.command(tool, args...)
	out, err := c.opts.Exec(ctx, name, argv...)
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(argv, " "), err, bytes.TrimSpace(out))
	}
	return out, nil
}

// Connect scans the FC targets for the LUN, waits for the multipath device and
// checks that it carries the expected serial.
func (c *Connector) Connect(ctx context.Context, v driver.FCVolume) (driver.Device, error) {
	want := NormalizeWWN(v.Serial)
	if want == "" {
		return driver.Device{}, errors.New("volume serial is empty")
	}
	info, err := volumeInfo(v)
	if err != nil {
		return driver.Device{}, err
	}
	d, err := c.brick.ConnectVolume(ctx, info)
	if err != nil {
		return driver.Device{}, fmt.Errorf("connect LUN %d: %w", v.LUN, err)
	}
	if !SameWWN(d.WWN, want) {
		return driver.Device{}, fmt.Errorf("device %s at LUN %d has WWN %s, want %s",
			d.Name, v.LUN, d.WWN, want)
	}
	if !strings.HasPrefix(d.Name, "dm-") {
		c.log.Warn("volume attached without multipath", "serial", want, "device", d.Name)
		return driver.Device{Path: filepath.Join(c.opts.DevRoot, d.Name), WWN: want}, nil
	}
	return driver.Device{Path: c.dmPath(d.Name), WWN: want}, nil
}

// volumeInfo builds the gobrick request. Target WWPNs are lower case hex
// without 0x or colons, as gobrick compares them to sysfs port_name.
func volumeInfo(v driver.FCVolume) (gobrick.FCVolumeInfo, error) {
	if len(v.Targets) == 0 {
		return gobrick.FCVolumeInfo{}, errors.New("no FC targets")
	}
	if v.LUN < 0 {
		return gobrick.FCVolumeInfo{}, fmt.Errorf("invalid LUN %d", v.LUN)
	}
	info := gobrick.FCVolumeInfo{Lun: v.LUN}
	for _, t := range v.Targets {
		w := NormalizeWWPN(t)
		if !isHex(w, 16) {
			return gobrick.FCVolumeInfo{}, fmt.Errorf("invalid target WWPN %q", t)
		}
		info.Targets = append(info.Targets, gobrick.FCTargetInfo{WWPN: w})
	}
	return info, nil
}

// dmPath returns /dev/mapper/<map name> when that node exists, else /dev/dm-N.
func (c *Connector) dmPath(dm string) string {
	if name := c.readSys("block", dm, "dm", "name"); name != "" {
		p := filepath.Join(c.opts.DevRoot, "mapper", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(c.opts.DevRoot, dm)
}

// Disconnect flushes the multipath map and deletes the SCSI devices that carry
// the serial. It succeeds when none are present.
func (c *Connector) Disconnect(ctx context.Context, serial string) error {
	dm, sds, err := c.find(serial)
	if err != nil {
		return err
	}
	name := dm
	if name == "" {
		if len(sds) == 0 {
			return nil
		}
		name = sds[0]
	}
	if err := c.brick.DisconnectVolumeByDeviceName(ctx, name); err != nil {
		return fmt.Errorf("disconnect %s: %w", name, err)
	}
	dm, sds, err = c.find(serial)
	if err != nil {
		return err
	}
	if dm != "" || len(sds) > 0 {
		return fmt.Errorf("devices for %s still present after disconnect: %s %v", NormalizeWWN(serial), dm, sds)
	}
	return nil
}

// Resize rescans every path of the volume and grows its multipath map.
func (c *Connector) Resize(ctx context.Context, serial string) error {
	dm, sds, err := c.find(serial)
	if err != nil {
		return err
	}
	if dm == "" && len(sds) == 0 {
		return fmt.Errorf("%s: %w", NormalizeWWN(serial), ErrNotAttached)
	}
	if dm != "" {
		sds = c.slaves(dm)
	}
	for _, sd := range sds {
		p := filepath.Join(c.opts.SysRoot, "block", sd, "device", "rescan")
		if err := os.WriteFile(p, []byte("1"), 0o200); err != nil {
			return fmt.Errorf("rescan %s: %w", sd, err)
		}
	}
	if dm == "" {
		return nil
	}
	name := c.readSys("block", dm, "dm", "name")
	if name == "" {
		return fmt.Errorf("no map name for %s", dm)
	}
	out, err := c.run(ctx, "multipathd", "resize", "map", name)
	if err != nil {
		return err
	}
	if r := strings.TrimSpace(string(out)); r != "ok" {
		return fmt.Errorf("multipathd resize map %s: %s", name, r)
	}
	return nil
}

// find returns the multipath dm-N and the sdX devices whose WWN is serial.
func (c *Connector) find(serial string) (string, []string, error) {
	want := NormalizeWWN(serial)
	if want == "" {
		return "", nil, errors.New("volume serial is empty")
	}
	block := filepath.Join(c.opts.SysRoot, "block")
	dms, err := filepath.Glob(filepath.Join(block, "dm-*"))
	if err != nil {
		return "", nil, err
	}
	var dm string
	for _, p := range dms {
		uuid := c.readSys("block", filepath.Base(p), "dm", "uuid")
		if id, ok := strings.CutPrefix(uuid, "mpath-"); ok && SameWWN(id, want) {
			dm = filepath.Base(p)
			break
		}
	}
	sdPaths, err := filepath.Glob(filepath.Join(block, "sd*"))
	if err != nil {
		return "", nil, err
	}
	var sds []string
	for _, p := range sdPaths {
		sd := filepath.Base(p)
		if SameWWN(c.readSys("block", sd, "device", "wwid"), want) {
			sds = append(sds, sd)
		}
	}
	sort.Strings(sds)
	return dm, sds, nil
}

func (c *Connector) slaves(dm string) []string {
	m, _ := filepath.Glob(filepath.Join(c.opts.SysRoot, "block", dm, "slaves", "*"))
	var out []string
	for _, p := range m {
		out = append(out, filepath.Base(p))
	}
	sort.Strings(out)
	return out
}

func (c *Connector) readSys(parts ...string) string {
	b, err := os.ReadFile(filepath.Join(append([]string{c.opts.SysRoot}, parts...)...))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// NormalizeWWN turns an SC device ID in any common spelling (6000D31...,
// 36000d31..., naa.6000d31..., 0x6000d31...) into lower case hex without the
// NAA designator prefix "3" that scsi_id and multipath put in front.
func NormalizeWWN(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "naa.")
	s = strings.TrimPrefix(s, "0x")
	s = strings.ReplaceAll(s, ":", "")
	if len(s) == 33 && s[0] == '3' {
		s = s[1:]
	}
	return s
}

// SameWWN reports whether a and b name the same device.
func SameWWN(a, b string) bool {
	na, nb := NormalizeWWN(a), NormalizeWWN(b)
	return na != "" && na == nb
}

// NormalizeWWPN returns a port name as 16 lower case hex digits without 0x
// or colons (if it was well formed).
func NormalizeWWPN(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "0x")
	return strings.ReplaceAll(s, ":", "")
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// slogAdapter sends gobrick's printf-style log lines to slog. gobrick logs
// every step at info level, so its Info lines go to slog at debug level.
type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Info(ctx context.Context, f string, args ...interface{}) {
	a.l.DebugContext(ctx, "gobrick: "+fmt.Sprintf(f, args...))
}

func (a slogAdapter) Debug(ctx context.Context, f string, args ...interface{}) {
	a.l.DebugContext(ctx, "gobrick: "+fmt.Sprintf(f, args...))
}

func (a slogAdapter) Error(ctx context.Context, f string, args ...interface{}) {
	a.l.WarnContext(ctx, "gobrick: "+fmt.Sprintf(f, args...))
}
