// Package fake has in-process stand-ins for the node's Connector and Mounter.
// They need no root, no FC hardware and no real mounts: a "device" is a
// regular file in a temp dir, and a "mount" is a map entry.
package fake

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/charlie-horse/sc-csi/internal/driver"
)

// DefaultDeviceSize is the size of a newly attached fake device.
const DefaultDeviceSize = 1 << 30

// FakeConnector attaches volumes by creating a file named after the serial.
type FakeConnector struct {
	Dir  string // where device files are created
	Size int64  // size of new devices; DefaultDeviceSize when 0

	// InUse, when set, makes Disconnect fail while it reports the device
	// path as mounted (for example FakeMounter.InUse).
	InUse func(devicePath string) bool

	mu      sync.Mutex
	devs    map[string]driver.Device
	resized map[string]int
}

var _ driver.Connector = (*FakeConnector)(nil)

// NewConnector returns a FakeConnector that keeps device files in dir.
func NewConnector(dir string) *FakeConnector {
	return &FakeConnector{Dir: dir, devs: map[string]driver.Device{}, resized: map[string]int{}}
}

// Connect creates the device file for v.Serial, or returns the existing one.
func (c *FakeConnector) Connect(ctx context.Context, v driver.FCVolume) (driver.Device, error) {
	if v.Serial == "" || strings.ContainsAny(v.Serial, `/\`) || v.Serial[0] == '.' {
		return driver.Device{}, fmt.Errorf("bad serial %q", v.Serial)
	}
	if v.LUN < 0 || len(v.Targets) == 0 {
		return driver.Device{}, fmt.Errorf("volume %s: need a LUN and target ports", v.Serial)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.devs[v.Serial]; ok {
		return d, nil
	}
	size := c.Size
	if size == 0 {
		size = DefaultDeviceSize
	}
	path := filepath.Join(c.Dir, "dm-"+v.Serial)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return driver.Device{}, err
	}
	err = f.Truncate(size)
	f.Close()
	if err != nil {
		return driver.Device{}, err
	}
	d := driver.Device{Path: path, WWN: v.Serial}
	c.devs[v.Serial] = d
	return d, nil
}

// Disconnect removes the device file. It succeeds if nothing is attached.
func (c *FakeConnector) Disconnect(ctx context.Context, serial string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.devs[serial]
	if !ok {
		return nil
	}
	if c.InUse != nil && c.InUse(d.Path) {
		return fmt.Errorf("device %s is busy", d.Path)
	}
	if err := os.Remove(d.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	delete(c.devs, serial)
	return nil
}

// Resize counts the call; grow the device first with SetSize.
func (c *FakeConnector) Resize(ctx context.Context, serial string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.devs[serial]; !ok {
		return fmt.Errorf("volume %s is not attached", serial)
	}
	c.resized[serial]++
	return nil
}

// SetSize changes the size of an attached device, as an array expand would.
func (c *FakeConnector) SetSize(serial string, size int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.devs[serial]
	if !ok {
		return fmt.Errorf("volume %s is not attached", serial)
	}
	return os.Truncate(d.Path, size)
}

// Device returns the attached device for serial.
func (c *FakeConnector) Device(serial string) (driver.Device, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.devs[serial]
	return d, ok
}

// Attached returns the serials that are attached now.
func (c *FakeConnector) Attached() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(maps.Keys(c.devs))
}

// Resizes returns how often Resize was called for serial.
func (c *FakeConnector) Resizes(serial string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resized[serial]
}

// Mount is one entry in the FakeMounter's table.
type Mount struct {
	Source   string
	FSType   string // empty for a bind mount
	Options  []string
	ReadOnly bool
}

// FakeMounter keeps mounts as a table of target -> source. Targets and
// sources must exist, as with real mounts.
type FakeMounter struct {
	mu        sync.Mutex
	mounts    map[string]Mount
	formatted map[string]string // device -> filesystem type
	resized   map[string]int    // device -> ResizeFS calls
}

var _ driver.Mounter = (*FakeMounter)(nil)

// NewMounter returns an empty FakeMounter.
func NewMounter() *FakeMounter {
	return &FakeMounter{mounts: map[string]Mount{}, formatted: map[string]string{}, resized: map[string]int{}}
}

func clean(p string) string { return filepath.Clean(p) }

// FormatAndMount records a filesystem on source and mounts it on the target dir.
func (m *FakeMounter) FormatAndMount(source, target, fsType string, options []string) error {
	source, target = clean(source), clean(target)
	if _, err := os.Stat(source); err != nil {
		return err
	}
	if fi, err := os.Stat(target); err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("mount target %s is not a directory", target)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.mounts[target]; ok {
		if cur.Source == source {
			return nil
		}
		return fmt.Errorf("%s is already mounted from %s", target, cur.Source)
	}
	if have, ok := m.formatted[source]; ok && have != fsType {
		return fmt.Errorf("%s holds %s, not %s", source, have, fsType)
	}
	m.formatted[source] = fsType
	m.mounts[target] = Mount{Source: source, FSType: fsType, Options: options, ReadOnly: hasRO(options)}
	return nil
}

func hasRO(opts []string) bool {
	for _, o := range opts {
		if o == "ro" {
			return true
		}
	}
	return false
}

// BindMount mounts source on target. A directory needs a directory target and
// a file needs a file target.
func (m *FakeMounter) BindMount(source, target string, readOnly bool) error {
	source, target = clean(source), clean(target)
	sfi, err := os.Stat(source)
	if err != nil {
		return err
	}
	tfi, err := os.Stat(target)
	if err != nil {
		return err
	}
	if sfi.IsDir() != tfi.IsDir() {
		return fmt.Errorf("bind %s on %s: file and directory mismatch", source, target)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.mounts[target]; ok {
		if cur.Source == source {
			return nil
		}
		return fmt.Errorf("%s is already mounted from %s", target, cur.Source)
	}
	m.mounts[target] = Mount{Source: source, ReadOnly: readOnly}
	return nil
}

// Unmount removes target from the table. It succeeds if target is not mounted.
func (m *FakeMounter) Unmount(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mounts, clean(target))
	return nil
}

// IsMountPoint reports whether path is in the table. A missing path returns
// an fs.ErrNotExist error.
func (m *FakeMounter) IsMountPoint(path string) (bool, error) {
	path = clean(path)
	if _, err := os.Stat(path); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.mounts[path]
	return ok, nil
}

// ResizeFS counts the call. mountPath must be mounted.
func (m *FakeMounter) ResizeFS(device, mountPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mounts[clean(mountPath)]; !ok {
		return fmt.Errorf("%s is not mounted", mountPath)
	}
	m.resized[clean(device)]++
	return nil
}

// Stats returns usage numbers derived from the size of the device behind path.
func (m *FakeMounter) Stats(path string) (driver.VolumeStats, error) {
	if _, err := os.Stat(path); err != nil {
		return driver.VolumeStats{}, err
	}
	total, err := m.DeviceSize(path)
	if err != nil || total <= 0 {
		total = DefaultDeviceSize
	}
	used := total / 10
	inodes := total / 16384
	return driver.VolumeStats{
		TotalBytes: total, UsedBytes: used, AvailBytes: total - used,
		TotalInodes: inodes, UsedInodes: 11, FreeInodes: inodes - 11,
	}, nil
}

// DeviceSize follows mounts from path back to a regular file and returns its size.
func (m *FakeMounter) DeviceSize(device string) (int64, error) {
	p := m.resolve(clean(device))
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	if !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a device", device)
	}
	return fi.Size(), nil
}

func (m *FakeMounter) resolve(p string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for range 16 {
		mt, ok := m.mounts[p]
		if !ok {
			break
		}
		p = mt.Source
	}
	return p
}

// InUse reports whether any mount has source as its source.
func (m *FakeMounter) InUse(source string) bool {
	source = clean(source)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mt := range m.mounts {
		if mt.Source == source {
			return true
		}
	}
	return false
}

// Mounts returns a copy of the mount table.
func (m *FakeMounter) Mounts() map[string]Mount {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.mounts)
}

// Resizes returns how often ResizeFS was called for device.
func (m *FakeMounter) Resizes(device string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resized[clean(device)]
}
