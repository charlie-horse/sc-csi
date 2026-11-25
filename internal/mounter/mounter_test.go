package mounter

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	mount "k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"
	testingexec "k8s.io/utils/exec/testing"
)

// script returns a FakeExec that answers each command with the given output
// and error, in order, and records the argv of each call.
func script(t *testing.T, argv *[][]string, steps ...struct {
	out string
	err error
}) *testingexec.FakeExec {
	fe := &testingexec.FakeExec{}
	for _, s := range steps {
		s := s
		fe.CommandScript = append(fe.CommandScript, func(cmd string, args ...string) utilexec.Cmd {
			*argv = append(*argv, append([]string{cmd}, args...))
			fc := &testingexec.FakeCmd{CombinedOutputScript: []testingexec.FakeAction{
				func() ([]byte, []byte, error) { return []byte(s.out), nil, s.err },
			}}
			return testingexec.InitFakeCmd(fc, cmd, args...)
		})
	}
	return fe
}

type step = struct {
	out string
	err error
}

func TestBindMountDirAndFile(t *testing.T) {
	fm := mount.NewFakeMounter(nil)
	m := NewWith(fm, &testingexec.FakeExec{})
	tmp := t.TempDir()

	srcDir := filepath.Join(tmp, "staging")
	if err := os.Mkdir(srcDir, 0o750); err != nil {
		t.Fatal(err)
	}
	dirTarget := filepath.Join(tmp, "pods", "a", "mount")
	if err := m.BindMount(srcDir, dirTarget, true); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dirTarget); err != nil || !st.IsDir() {
		t.Fatalf("dir target not created: %v", err)
	}

	srcFile := filepath.Join(tmp, "dev-node")
	if err := os.WriteFile(srcFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fileTarget := filepath.Join(tmp, "pods", "b", "block", "vol")
	if err := m.BindMount(srcFile, fileTarget, false); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(fileTarget); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("file target not created: %v", err)
	}

	mps, _ := fm.List()
	if len(mps) != 2 {
		t.Fatalf("mounts %+v", mps)
	}
	if !reflect.DeepEqual(mps[0].Opts, []string{"bind", "ro"}) || !reflect.DeepEqual(mps[1].Opts, []string{"bind"}) {
		t.Fatalf("options %v %v", mps[0].Opts, mps[1].Opts)
	}

	// A second call on a mounted target is a no-op.
	if err := m.BindMount(srcDir, dirTarget, true); err != nil {
		t.Fatal(err)
	}
	if mps, _ := fm.List(); len(mps) != 2 {
		t.Fatalf("remounted: %+v", mps)
	}
	if err := m.BindMount(filepath.Join(tmp, "missing"), dirTarget, true); err == nil {
		t.Fatal("missing source accepted")
	}
}

func TestUnmountIdempotent(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "mnt")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	fm := mount.NewFakeMounter([]mount.MountPoint{{Device: "/dev/x", Path: target}})
	m := NewWith(fm, &testingexec.FakeExec{})
	if mnt, err := m.IsMountPoint(target); err != nil || !mnt {
		t.Fatalf("IsMountPoint = %v, %v", mnt, err)
	}
	if err := m.Unmount(target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target not removed: %v", err)
	}
	if err := m.Unmount(target); err != nil {
		t.Fatalf("second unmount: %v", err)
	}
	if mnt, err := m.IsMountPoint(target); err != nil || mnt {
		t.Fatalf("missing path: %v, %v", mnt, err)
	}
}

func TestFormatAndMount(t *testing.T) {
	target := filepath.Join(t.TempDir(), "staging")
	var argv [][]string
	fe := script(t, &argv,
		step{"", &testingexec.FakeExitError{Status: 2}}, // blkid: no filesystem
		step{"", nil}, // mkfs.ext4
	)
	fm := mount.NewFakeMounter(nil)
	m := NewWith(fm, fe)
	if err := m.FormatAndMount("/dev/mapper/x", target, "ext4", []string{"noatime"}); err != nil {
		t.Fatal(err)
	}
	if len(argv) != 2 || argv[0][0] != "blkid" || argv[1][0] != "mkfs.ext4" {
		t.Fatalf("commands %v", argv)
	}
	mps, _ := fm.List()
	if len(mps) != 1 || mps[0].Device != "/dev/mapper/x" || mps[0].Type != "ext4" {
		t.Fatalf("mounts %+v", mps)
	}
	// Already mounted: no commands run.
	if err := m.FormatAndMount("/dev/mapper/x", target, "ext4", nil); err != nil {
		t.Fatal(err)
	}
	if len(argv) != 2 {
		t.Fatalf("ran %v", argv)
	}
}

func TestDeviceSize(t *testing.T) {
	var argv [][]string
	m := NewWith(mount.NewFakeMounter(nil), script(t, &argv, step{"10737418240\n", nil}, step{"junk", nil}))
	n, err := m.DeviceSize("/dev/mapper/x")
	if err != nil || n != 10737418240 {
		t.Fatalf("got %d, %v", n, err)
	}
	if want := []string{"blockdev", "--getsize64", "/dev/mapper/x"}; !reflect.DeepEqual(argv[0], want) {
		t.Fatalf("argv %v", argv[0])
	}
	if _, err := m.DeviceSize("/dev/mapper/x"); err == nil {
		t.Fatal("bad output accepted")
	}
}

func TestStats(t *testing.T) {
	m := NewWith(mount.NewFakeMounter(nil), &testingexec.FakeExec{})
	s, err := m.Stats(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalBytes <= 0 || s.AvailBytes < 0 || s.UsedBytes < 0 || s.UsedBytes > s.TotalBytes {
		t.Fatalf("stats %+v", s)
	}
	if _, err := m.Stats(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing path accepted")
	}
}

func TestResizeFS(t *testing.T) {
	var argv [][]string
	m := NewWith(mount.NewFakeMounter(nil), script(t, &argv,
		step{"DEVNAME=/dev/mapper/x\nTYPE=ext4\n", nil}, // blkid
		step{"", nil}, // resize2fs
		step{"DEVNAME=/dev/mapper/x\nTYPE=vfat\n", nil}, // blkid
	))
	if err := m.ResizeFS("/dev/mapper/x", "/mnt"); err != nil {
		t.Fatal(err)
	}
	if len(argv) != 2 || argv[0][0] != "blkid" || !reflect.DeepEqual(argv[1], []string{"resize2fs", "/dev/mapper/x"}) {
		t.Fatalf("commands %v", argv)
	}
	if err := m.ResizeFS("/dev/mapper/x", "/mnt"); err == nil {
		t.Fatal("unsupported filesystem resized")
	}
}
