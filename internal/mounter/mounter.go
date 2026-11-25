// Package mounter formats, mounts and resizes volumes on a node with
// k8s.io/mount-utils. It runs mount, umount, blkid, fsck, mkfs.*,
// resize2fs, xfs_growfs and blockdev from the process PATH.
package mounter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
	mount "k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"

	"github.com/charlie-horse/sc-csi/internal/driver"
)

// Mounter implements driver.Mounter.
type Mounter struct {
	sfm  *mount.SafeFormatAndMount
	exec utilexec.Interface
}

var _ driver.Mounter = (*Mounter)(nil)

// New returns a Mounter that runs the real mount tools, without systemd-run.
func New() *Mounter {
	return NewWith(mount.NewWithoutSystemd(""), utilexec.New())
}

// NewWith returns a Mounter on the given mount and exec back ends (tests use
// mount.FakeMounter and the k8s.io/utils/exec/testing fakes).
func NewWith(m mount.Interface, e utilexec.Interface) *Mounter {
	return &Mounter{sfm: mount.NewSafeFormatAndMount(m, e), exec: e}
}

// FormatAndMount creates the target directory, formats source if it has no
// filesystem, and mounts it. It does nothing if target is already a mount.
func (m *Mounter) FormatAndMount(source, target, fsType string, options []string) error {
	if err := os.MkdirAll(target, 0o750); err != nil {
		return err
	}
	if mnt, err := m.IsMountPoint(target); err != nil || mnt {
		return err
	}
	return m.sfm.FormatAndMount(source, target, fsType, options)
}

// BindMount bind-mounts source on target. A directory source gets a
// directory target and anything else (a block device) gets a file target;
// missing targets and parents are created. It does nothing if target is
// already a mount.
func (m *Mounter) BindMount(source, target string, readOnly bool) error {
	st, err := os.Stat(source)
	if err != nil {
		return err
	}
	if st.IsDir() {
		if err := os.MkdirAll(target, 0o750); err != nil {
			return err
		}
	} else if err := makeFile(target); err != nil {
		return err
	}
	if mnt, err := m.IsMountPoint(target); err != nil || mnt {
		return err
	}
	opts := []string{"bind"}
	if readOnly {
		opts = append(opts, "ro")
	}
	return m.sfm.Mount(source, target, "", opts)
}

func makeFile(p string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDONLY, 0o640)
	if err != nil {
		return err
	}
	return f.Close()
}

// Unmount unmounts target if it is mounted and removes it. A missing target
// is not an error.
func (m *Mounter) Unmount(target string) error {
	return mount.CleanupMountPoint(target, m.sfm.Interface, true)
}

// IsMountPoint reports whether path is a mount point; false if it does not exist.
func (m *Mounter) IsMountPoint(path string) (bool, error) {
	notMnt, err := mount.IsNotMountPoint(m.sfm.Interface, path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !notMnt, nil
}

// ResizeFS grows the filesystem on device (mounted at mountPath) to fill it.
func (m *Mounter) ResizeFS(device, mountPath string) error {
	if _, err := mount.NewResizeFs(m.exec).Resize(device, mountPath); err != nil {
		return fmt.Errorf("resize filesystem on %s: %w", device, err)
	}
	return nil
}

// Stats returns usage of the filesystem holding path. For a block device
// node it returns only the device size as TotalBytes.
func (m *Mounter) Stats(path string) (driver.VolumeStats, error) {
	st, err := os.Stat(path)
	if err != nil {
		return driver.VolumeStats{}, err
	}
	if st.Mode()&os.ModeDevice != 0 {
		n, err := m.DeviceSize(path)
		return driver.VolumeStats{TotalBytes: n}, err
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return driver.VolumeStats{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	bs := int64(fs.Bsize)
	return driver.VolumeStats{
		TotalBytes:  int64(fs.Blocks) * bs,
		AvailBytes:  int64(fs.Bavail) * bs,
		UsedBytes:   int64(fs.Blocks-fs.Bfree) * bs,
		TotalInodes: int64(fs.Files),
		FreeInodes:  int64(fs.Ffree),
		UsedInodes:  int64(fs.Files - fs.Ffree),
	}, nil
}

// DeviceSize returns the size of a block device in bytes (blockdev --getsize64).
func (m *Mounter) DeviceSize(device string) (int64, error) {
	out, err := m.exec.Command("blockdev", "--getsize64", device).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("blockdev --getsize64 %s: %w: %s", device, err, strings.TrimSpace(string(out)))
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("blockdev --getsize64 %s: %w", device, err)
	}
	return n, nil
}
