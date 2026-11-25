package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// defaultFSType is the filesystem made on a mount volume that names none.
const defaultFSType = "ext4"

// metaSuffix names the state file kept next to a staging or target path,
// never inside it: "<path>.sc-csi.json".
const metaSuffix = ".sc-csi.json"

// stageMeta is written next to the staging path by NodeStageVolume. Later
// calls that get only the staging path (unstage, expand) read the volume's
// serial and device from it.
type stageMeta struct {
	VolumeID  string   `json:"volumeID"`
	Serial    string   `json:"serial"`
	LUN       int      `json:"lun"`
	Targets   []string `json:"targets"`
	Block     bool     `json:"block"`
	Device    string   `json:"device"`
	FSType    string   `json:"fsType,omitempty"`
	Published []string `json:"published,omitempty"` // target paths
}

// pubMeta is written next to a target path by NodePublishVolume so that calls
// given only the target path can find the staging path.
type pubMeta struct {
	VolumeID    string `json:"volumeID"`
	StagingPath string `json:"stagingPath"`
}

func metaPath(p string) string {
	p = filepath.Clean(p)
	return filepath.Join(filepath.Dir(p), filepath.Base(p)+metaSuffix)
}

// readMeta loads the JSON state file for path into v. It returns false when
// there is no such file.
func readMeta(path string, v any) (bool, error) {
	b, err := os.ReadFile(metaPath(path))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("%s: %w", metaPath(path), err)
	}
	return true, nil
}

// writeMeta stores v as the JSON state file for path, replacing it atomically.
func writeMeta(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dst := metaPath(path)
	f, err := os.CreateTemp(filepath.Dir(dst), ".sc-csi-*")
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(f.Name(), dst)
	}
	if werr != nil {
		os.Remove(f.Name())
	}
	return werr
}

func removeMeta(path string) error {
	err := os.Remove(metaPath(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// isMounted asks the Mounter; a path that does not exist is not mounted.
func (d *Driver) isMounted(path string) (bool, error) {
	ok, err := d.Mount.IsMountPoint(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return ok, err
}

func internal(format string, a ...any) error {
	return status.Errorf(codes.Internal, format, a...)
}

// parsePublishContext reads the FC location ControllerPublishVolume sent.
func parsePublishContext(pc map[string]string) (FCVolume, error) {
	serial := strings.ToLower(strings.TrimSpace(pc[PubSerial]))
	if serial == "" {
		return FCVolume{}, status.Errorf(codes.InvalidArgument, "publish context has no %q", PubSerial)
	}
	lun, err := strconv.Atoi(strings.TrimSpace(pc[PubLUN]))
	if err != nil || lun < 0 {
		return FCVolume{}, status.Errorf(codes.InvalidArgument, "publish context %q is not a LUN number", PubLUN)
	}
	var targets []string
	for t := range strings.SplitSeq(pc[PubTargets], ",") {
		if t = strings.TrimSpace(t); t != "" {
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		return FCVolume{}, status.Errorf(codes.InvalidArgument, "publish context has no %q", PubTargets)
	}
	return FCVolume{Serial: serial, LUN: lun, Targets: targets}, nil
}

// checkCapability accepts block or mount access and returns true for block.
// A mount filesystem can be written from one node only.
func checkCapability(c *csi.VolumeCapability) (bool, error) {
	if c == nil {
		return false, status.Error(codes.InvalidArgument, "volume capability is missing")
	}
	switch c.GetAccessType().(type) {
	case *csi.VolumeCapability_Block:
		return true, nil
	case *csi.VolumeCapability_Mount:
		switch c.GetAccessMode().GetMode() {
		case csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER:
			return false, status.Error(codes.InvalidArgument, "a mount volume cannot be written from several nodes")
		}
		return false, nil
	}
	return false, status.Error(codes.InvalidArgument, "volume capability has no block or mount access type")
}

// NodeGetInfo returns "<node>;<wwpn>,..." so the controller can map LUNs to this node.
func (d *Driver) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	if d.WWPNs == nil {
		return nil, internal("no FC initiator lookup configured")
	}
	wwpns, err := d.WWPNs()
	if err != nil {
		return nil, internal("reading FC initiator ports: %v", err)
	}
	if len(wwpns) == 0 {
		return nil, status.Error(codes.FailedPrecondition, "node has no FC initiator ports")
	}
	return &csi.NodeGetInfoResponse{NodeId: NodeID(d.Cfg.NodeName, wwpns)}, nil
}

// NodeGetCapabilities lists the optional node RPCs this driver serves.
func (d *Driver) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	var caps []*csi.NodeServiceCapability
	for _, t := range []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	} {
		caps = append(caps, &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{Rpc: &csi.NodeServiceCapability_RPC{Type: t}},
		})
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: caps}, nil
}

// NodeStageVolume attaches the LUN and, for mount access, formats it if blank
// and mounts it on the staging path. Block volumes are only attached here.
func (d *Driver) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is missing")
	}
	staging := req.GetStagingTargetPath()
	if staging == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is missing")
	}
	block, err := checkCapability(req.GetVolumeCapability())
	if err != nil {
		return nil, err
	}
	fcv, err := parsePublishContext(req.GetPublishContext())
	if err != nil {
		return nil, err
	}

	var meta stageMeta
	found, err := readMeta(staging, &meta)
	if err != nil {
		return nil, internal("reading stage state: %v", err)
	}
	if found && (meta.VolumeID != req.GetVolumeId() || meta.Block != block) {
		return nil, status.Errorf(codes.AlreadyExists,
			"staging path %s holds volume %s with other settings", staging, meta.VolumeID)
	}
	if err := os.MkdirAll(staging, 0o750); err != nil {
		return nil, internal("creating staging path: %v", err)
	}

	dev, err := d.Conn.Connect(ctx, fcv)
	if err != nil {
		return nil, toStatus(fmt.Errorf("attaching volume %s: %w", fcv.Serial, err), codes.Internal)
	}
	fsType := ""
	if !block {
		fsType = req.GetVolumeCapability().GetMount().GetFsType()
		if fsType == "" {
			fsType = defaultFSType
		}
	}
	// State goes to disk before the mount, so a failed mount can still be unstaged.
	meta = stageMeta{
		VolumeID: req.GetVolumeId(), Serial: fcv.Serial, LUN: fcv.LUN, Targets: fcv.Targets,
		Block: block, Device: dev.Path, FSType: fsType, Published: meta.Published,
	}
	if err := writeMeta(staging, &meta); err != nil {
		return nil, internal("writing stage state: %v", err)
	}
	if block {
		return &csi.NodeStageVolumeResponse{}, nil
	}

	mounted, err := d.isMounted(staging)
	if err != nil {
		return nil, internal("checking staging mount: %v", err)
	}
	if !mounted {
		flags := req.GetVolumeCapability().GetMount().GetMountFlags()
		if err := d.Mount.FormatAndMount(dev.Path, staging, fsType, flags); err != nil {
			return nil, internal("mounting %s on %s: %v", dev.Path, staging, err)
		}
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

// NodeUnstageVolume unmounts the staging path, then flushes the multipath map
// and deletes the SCSI devices. It succeeds when nothing is staged.
func (d *Driver) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is missing")
	}
	staging := req.GetStagingTargetPath()
	if staging == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is missing")
	}
	var meta stageMeta
	found, err := readMeta(staging, &meta)
	if err != nil {
		return nil, internal("reading stage state: %v", err)
	}
	mounted, err := d.isMounted(staging)
	if err != nil {
		return nil, internal("checking staging mount: %v", err)
	}
	if mounted {
		if err := d.Mount.Unmount(staging); err != nil {
			return nil, internal("unmounting %s: %v", staging, err)
		}
	}
	if !found {
		return &csi.NodeUnstageVolumeResponse{}, nil
	}
	if err := d.Conn.Disconnect(ctx, meta.Serial); err != nil {
		return nil, toStatus(fmt.Errorf("detaching volume %s: %w", meta.Serial, err), codes.Internal)
	}
	if err := removeMeta(staging); err != nil {
		return nil, internal("removing stage state: %v", err)
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodePublishVolume bind-mounts the staging path (mount access) or the device
// (block access) on the target path.
func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is missing")
	}
	target := req.GetTargetPath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is missing")
	}
	block, err := checkCapability(req.GetVolumeCapability())
	if err != nil {
		return nil, err
	}
	staging := req.GetStagingTargetPath()
	if staging == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is missing")
	}

	var meta stageMeta
	found, err := readMeta(staging, &meta)
	if err != nil {
		return nil, internal("reading stage state: %v", err)
	}
	if !found || meta.VolumeID != req.GetVolumeId() {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s is not staged at %s", req.GetVolumeId(), staging)
	}
	if meta.Block != block {
		return nil, status.Error(codes.InvalidArgument, "access type differs from the one the volume was staged with")
	}

	mounted, err := d.isMounted(target)
	if err != nil {
		return nil, internal("checking target mount: %v", err)
	}
	if mounted {
		return &csi.NodePublishVolumeResponse{}, d.recordPublish(staging, target, &meta)
	}
	if req.GetVolumeCapability().GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER {
		for _, other := range meta.Published {
			if other == target {
				continue
			}
			if busy, err := d.isMounted(other); err != nil {
				return nil, internal("checking target mount: %v", err)
			} else if busy {
				return nil, status.Errorf(codes.FailedPrecondition,
					"single-writer volume %s is already published at %s", req.GetVolumeId(), other)
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return nil, internal("creating target parent: %v", err)
	}
	source := staging
	if block {
		source, err = d.blockDevice(ctx, &meta, req.GetPublishContext())
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_RDONLY, 0o640)
		if err != nil {
			return nil, internal("creating target file: %v", err)
		}
		f.Close()
	} else {
		if ok, err := d.isMounted(staging); err != nil {
			return nil, internal("checking staging mount: %v", err)
		} else if !ok {
			return nil, status.Errorf(codes.FailedPrecondition, "staging path %s is not mounted", staging)
		}
		if err := os.MkdirAll(target, 0o750); err != nil {
			return nil, internal("creating target path: %v", err)
		}
	}
	// Target state goes to disk first so a failed bind can still be unpublished.
	if err := writeMeta(target, &pubMeta{VolumeID: req.GetVolumeId(), StagingPath: staging}); err != nil {
		return nil, internal("writing publish state: %v", err)
	}
	if err := d.Mount.BindMount(source, target, req.GetReadonly()); err != nil {
		return nil, internal("bind-mounting %s on %s: %v", source, target, err)
	}
	return &csi.NodePublishVolumeResponse{}, d.recordPublish(staging, target, &meta)
}

// blockDevice returns the device path of a staged block volume, attaching it
// again when the recorded device is gone.
func (d *Driver) blockDevice(ctx context.Context, meta *stageMeta, pc map[string]string) (string, error) {
	if meta.Device != "" {
		if _, err := os.Stat(meta.Device); err == nil {
			return meta.Device, nil
		}
	}
	fcv := FCVolume{Serial: meta.Serial, LUN: meta.LUN, Targets: meta.Targets}
	if len(pc) > 0 {
		if v, err := parsePublishContext(pc); err == nil {
			fcv = v
		}
	}
	dev, err := d.Conn.Connect(ctx, fcv)
	if err != nil {
		return "", toStatus(fmt.Errorf("attaching volume %s: %w", fcv.Serial, err), codes.Internal)
	}
	return dev.Path, nil
}

// recordPublish adds target to the staged volume's list of targets.
func (d *Driver) recordPublish(staging, target string, meta *stageMeta) error {
	if slices.Contains(meta.Published, target) {
		return nil
	}
	meta.Published = append(meta.Published, target)
	if err := writeMeta(staging, meta); err != nil {
		return internal("writing stage state: %v", err)
	}
	return nil
}

// NodeUnpublishVolume unmounts and removes the target path. It succeeds when
// the target is already gone.
func (d *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is missing")
	}
	target := req.GetTargetPath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is missing")
	}
	mounted, err := d.isMounted(target)
	if err != nil {
		return nil, internal("checking target mount: %v", err)
	}
	if mounted {
		if err := d.Mount.Unmount(target); err != nil {
			return nil, internal("unmounting %s: %v", target, err)
		}
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, internal("removing target path: %v", err)
	}

	var pm pubMeta
	if found, err := readMeta(target, &pm); err != nil {
		return nil, internal("reading publish state: %v", err)
	} else if found {
		var sm stageMeta
		if ok, err := readMeta(pm.StagingPath, &sm); err == nil && ok {
			if i := slices.Index(sm.Published, target); i >= 0 {
				sm.Published = slices.Delete(sm.Published, i, i+1)
				if err := writeMeta(pm.StagingPath, &sm); err != nil {
					return nil, internal("writing stage state: %v", err)
				}
			}
		}
		if err := removeMeta(target); err != nil {
			return nil, internal("removing publish state: %v", err)
		}
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeExpandVolume grows the multipath device after the array volume grew,
// then the filesystem for mount access.
func (d *Driver) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is missing")
	}
	path := req.GetVolumePath()
	if path == "" {
		return nil, status.Error(codes.InvalidArgument, "volume path is missing")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, status.Errorf(codes.NotFound, "volume path %s: %v", path, err)
	}
	staging := req.GetStagingTargetPath()
	if staging == "" {
		var pm pubMeta
		found, err := readMeta(path, &pm)
		if err != nil {
			return nil, internal("reading publish state: %v", err)
		}
		if !found {
			return nil, status.Errorf(codes.NotFound, "volume %s is not published at %s", req.GetVolumeId(), path)
		}
		staging = pm.StagingPath
	}
	var meta stageMeta
	found, err := readMeta(staging, &meta)
	if err != nil {
		return nil, internal("reading stage state: %v", err)
	}
	if !found || meta.VolumeID != req.GetVolumeId() {
		return nil, status.Errorf(codes.NotFound, "volume %s is not staged on this node", req.GetVolumeId())
	}

	if err := d.Conn.Resize(ctx, meta.Serial); err != nil {
		return nil, toStatus(fmt.Errorf("rescanning volume %s: %w", meta.Serial, err), codes.Internal)
	}
	block := meta.Block
	if c := req.GetVolumeCapability(); c != nil {
		block = c.GetBlock() != nil
	}
	if !block {
		if err := d.Mount.ResizeFS(meta.Device, path); err != nil {
			return nil, internal("growing filesystem on %s: %v", meta.Device, err)
		}
	}
	size, err := d.Mount.DeviceSize(meta.Device)
	if err != nil {
		return nil, internal("reading size of %s: %v", meta.Device, err)
	}
	return &csi.NodeExpandVolumeResponse{CapacityBytes: size}, nil
}

// NodeGetVolumeStats reports filesystem usage for a mount path, or the size of
// a block volume.
func (d *Driver) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is missing")
	}
	path := req.GetVolumePath()
	if path == "" {
		return nil, status.Error(codes.InvalidArgument, "volume path is missing")
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, status.Errorf(codes.NotFound, "volume path %s does not exist", path)
	}
	if err != nil {
		return nil, internal("checking volume path: %v", err)
	}
	if !fi.IsDir() {
		size, err := d.Mount.DeviceSize(path)
		if err != nil {
			return nil, internal("reading size of %s: %v", path, err)
		}
		return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{
			{Unit: csi.VolumeUsage_BYTES, Total: size},
		}}, nil
	}
	st, err := d.Mount.Stats(path)
	if err != nil {
		return nil, internal("reading filesystem stats of %s: %v", path, err)
	}
	return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{
		{Unit: csi.VolumeUsage_BYTES, Total: st.TotalBytes, Used: st.UsedBytes, Available: st.AvailBytes},
		{Unit: csi.VolumeUsage_INODES, Total: st.TotalInodes, Used: st.UsedInodes, Available: st.FreeInodes},
	}}, nil
}
