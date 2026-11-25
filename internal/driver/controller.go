package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/charlie-horse/sc-csi/internal/csp"
)

const gib = int64(1) << 30

// StorageClass parameters the controller passes to the array. Other keys are ignored.
const (
	ParamStorageProfile       = "storageProfile"
	ParamDataReductionProfile = "dataReductionProfile"
	ParamSnapshotProfile      = "snapshotProfile"
	ParamDescription          = "description"
)

var controllerCaps = []csi.ControllerServiceCapability_RPC_Type{
	csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
	csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
	csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
	csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
	csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
	csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
	csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
}

// ControllerGetCapabilities lists the controller RPCs this driver implements.
func (d *Driver) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	out := make([]*csi.ControllerServiceCapability, 0, len(controllerCaps))
	for _, c := range controllerCaps {
		out = append(out, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{Rpc: &csi.ControllerServiceCapability_RPC{Type: c}},
		})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: out}, nil
}

func (d *Driver) controllerReady() error {
	if d.CSP == nil || d.Array == nil {
		return status.Error(codes.FailedPrecondition, "this instance has no storage center connection")
	}
	return nil
}

// checkCaps accepts single-node modes and multi-node reader-only for both access types, and
// multi-node writer modes for raw block only.
func checkCaps(caps []*csi.VolumeCapability) error {
	if len(caps) == 0 {
		return status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	for _, c := range caps {
		if err := checkCap(c); err != nil {
			return err
		}
	}
	return nil
}

func checkCap(c *csi.VolumeCapability) error {
	if c == nil || c.GetAccessMode() == nil {
		return status.Error(codes.InvalidArgument, "volume capability needs an access mode")
	}
	block := c.GetBlock() != nil
	if !block && c.GetMount() == nil {
		return status.Error(codes.InvalidArgument, "volume capability needs block or mount access type")
	}
	switch m := c.GetAccessMode().GetMode(); m {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
		return nil
	case csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
		if block {
			return nil
		}
		return status.Errorf(codes.InvalidArgument, "access mode %s needs block access type", m)
	default:
		return status.Errorf(codes.InvalidArgument, "access mode %s is not supported", m)
	}
}

func singleNode(c *csi.VolumeCapability) bool {
	switch c.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
		return true
	}
	return false
}

// sizeFor rounds a capacity range up to whole GiB (at least min, at least 1 GiB) and checks the limit.
func sizeFor(r *csi.CapacityRange, min int64) (int64, error) {
	req, lim := r.GetRequiredBytes(), r.GetLimitBytes()
	if req < 0 || lim < 0 {
		return 0, status.Error(codes.InvalidArgument, "capacity range must not be negative")
	}
	if lim > 0 && req > lim {
		return 0, status.Errorf(codes.InvalidArgument, "required bytes %d exceed limit bytes %d", req, lim)
	}
	if req < min {
		req = min
	}
	g := (req + gib - 1) / gib
	if g < 1 {
		g = 1
	}
	size := g * gib
	if lim > 0 && size > lim {
		return 0, status.Errorf(codes.OutOfRange, "%d bytes rounded up to whole GiB exceed limit bytes %d", req, lim)
	}
	return size, nil
}

func fits(size int64, r *csi.CapacityRange) bool {
	return size >= r.GetRequiredBytes() && (r.GetLimitBytes() == 0 || size <= r.GetLimitBytes())
}

func volumeConfig(params map[string]string) (map[string]any, string) {
	conf := map[string]any{}
	for _, k := range []string{ParamStorageProfile, ParamDataReductionProfile, ParamSnapshotProfile} {
		if v, ok := params[k]; ok {
			conf[k] = v
		}
	}
	return conf, params[ParamDescription]
}

// CreateVolume creates a volume, a restore of a snapshot, or a clone of a volume. The volume ID
// is the SC volume instance ID. A volume of the same name is returned if its size fits.
func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := checkCaps(req.GetVolumeCapabilities()); err != nil {
		return nil, err
	}
	if err := d.controllerReady(); err != nil {
		return nil, err
	}
	if _, err := sizeFor(req.GetCapacityRange(), 0); err != nil {
		return nil, err
	}
	name, src := req.GetName(), req.GetVolumeContentSource()

	if found, err := d.CSP.ListVolumes(ctx, d.Array, name); err == nil && len(found) > 0 {
		v := found[0]
		if !fits(v.Size, req.GetCapacityRange()) {
			return nil, status.Errorf(codes.AlreadyExists, "volume %s exists with size %d", name, v.Size)
		}
		return createResponse(v, src), nil
	} else if err != nil && !isNotFound(err) {
		return nil, toStatus(err, codes.Aborted)
	}

	var v csp.Volume
	var err error
	switch {
	case src.GetSnapshot() != nil:
		v, err = d.restore(ctx, req, src.GetSnapshot().GetSnapshotId())
	case src.GetVolume() != nil:
		v, err = d.clone(ctx, req, src.GetVolume().GetVolumeId())
	case src != nil:
		return nil, status.Error(codes.InvalidArgument, "unknown volume content source")
	default:
		v, err = d.plainVolume(ctx, req, 0)
	}
	if err != nil {
		return nil, err
	}
	return createResponse(v, src), nil
}

func (d *Driver) plainVolume(ctx context.Context, req *csi.CreateVolumeRequest, min int64) (csp.Volume, error) {
	size, err := sizeFor(req.GetCapacityRange(), min)
	if err != nil {
		return csp.Volume{}, err
	}
	conf, desc := volumeConfig(req.GetParameters())
	v, err := d.CSP.CreateVolume(ctx, d.Array, csp.CreateVolumeRequest{
		Name: req.GetName(), Size: size, Description: desc, Config: conf,
	})
	return v, toStatus(err, codes.AlreadyExists)
}

func (d *Driver) restore(ctx context.Context, req *csi.CreateVolumeRequest, snapID string) (csp.Volume, error) {
	if snapID == "" {
		return csp.Volume{}, status.Error(codes.InvalidArgument, "snapshot source needs a snapshot id")
	}
	snap, err := d.CSP.GetSnapshot(ctx, d.Array, snapID)
	if err != nil {
		return csp.Volume{}, toStatus(err, codes.FailedPrecondition)
	}
	size, err := sizeFor(req.GetCapacityRange(), snap.Size)
	if err != nil {
		return csp.Volume{}, err
	}
	v, err := d.CSP.CreateVolume(ctx, d.Array, csp.CreateVolumeRequest{
		Name: req.GetName(), Size: size, BaseSnapshotID: snap.ID, Clone: true,
	})
	return v, toStatus(err, codes.AlreadyExists)
}

// clone copies a volume through a temporary snapshot, which the clone then expires. A volume
// that was never written has no data, so its clone is a new empty volume.
func (d *Driver) clone(ctx context.Context, req *csi.CreateVolumeRequest, srcID string) (csp.Volume, error) {
	if srcID == "" {
		return csp.Volume{}, status.Error(codes.InvalidArgument, "volume source needs a volume id")
	}
	src, err := d.CSP.GetVolume(ctx, d.Array, srcID)
	if err != nil {
		return csp.Volume{}, toStatus(err, codes.FailedPrecondition)
	}
	size, err := sizeFor(req.GetCapacityRange(), src.Size)
	if err != nil {
		return csp.Volume{}, err
	}
	written, err := d.CSP.VolumeWritten(ctx, d.Array, srcID)
	if err != nil {
		return csp.Volume{}, toStatus(err, codes.FailedPrecondition)
	}
	if !written {
		return d.plainVolume(ctx, req, src.Size)
	}
	helper, err := d.CSP.CreateSnapshot(ctx, d.Array, csp.SnapshotRequest{
		Name: csp.CloneHelperPrefix + req.GetName(), VolumeID: srcID,
	})
	if err != nil {
		return csp.Volume{}, toStatus(err, codes.FailedPrecondition)
	}
	v, err := d.CSP.CreateVolume(ctx, d.Array, csp.CreateVolumeRequest{
		Name: req.GetName(), Size: size, BaseSnapshotID: helper.ID, Clone: true,
	})
	return v, toStatus(err, codes.AlreadyExists)
}

func createResponse(v csp.Volume, src *csi.VolumeContentSource) *csi.CreateVolumeResponse {
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId: v.ID, CapacityBytes: v.Size, ContentSource: src,
	}}
}

// DeleteVolume deletes a volume; an unknown volume counts as deleted.
func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if err := d.controllerReady(); err != nil {
		return nil, err
	}
	if err := d.CSP.DeleteVolume(ctx, d.Array, req.GetVolumeId()); err != nil && !isNotFound(err) {
		return nil, toStatus(err, codes.FailedPrecondition)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerPublishVolume registers the node's FC initiators on the array, maps the volume to
// the node and returns where the node finds the LUN. A single-node volume mapped to another
// node is refused.
func (d *Driver) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	volID, nodeID := req.GetVolumeId(), req.GetNodeId()
	if volID == "" || nodeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id and node id are required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}
	if err := checkCap(req.GetVolumeCapability()); err != nil {
		return nil, err
	}
	if err := d.controllerReady(); err != nil {
		return nil, err
	}
	node, wwpns, err := ParseNodeID(nodeID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "node %q not found: %v", nodeID, err)
	}
	vol, err := d.CSP.GetVolume(ctx, d.Array, volID)
	if err != nil {
		return nil, toStatus(err, codes.FailedPrecondition)
	}
	host, err := d.CSP.AssertHost(ctx, d.Array, csp.HostRequest{Name: node, UUID: node, WWPNs: wwpns})
	if err != nil {
		return nil, toStatus(err, codes.FailedPrecondition)
	}
	if vol.Published && singleNode(req.GetVolumeCapability()) {
		servers, err := d.CSP.MappedServers(ctx, d.Array, volID)
		if err != nil {
			return nil, toStatus(err, codes.FailedPrecondition)
		}
		for _, s := range servers {
			if s != host.ID {
				return nil, status.Errorf(codes.FailedPrecondition, "volume %s is published to another node", volID)
			}
		}
	}
	info, err := d.CSP.Publish(ctx, d.Array, volID, csp.PublishRequest{HostUUID: node, AccessProtocol: "fc"})
	if err != nil {
		return nil, toStatus(err, codes.FailedPrecondition)
	}
	return &csi.ControllerPublishVolumeResponse{PublishContext: map[string]string{
		PubSerial:  info.SerialNumber,
		PubLUN:     strconv.Itoa(info.LunID),
		PubTargets: strings.Join(info.TargetNames, ","),
	}}, nil
}

// ControllerUnpublishVolume unmaps the volume from the node, or from every node when the node
// ID is empty. An unknown volume or node counts as unpublished.
func (d *Driver) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	volID := req.GetVolumeId()
	if volID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if err := d.controllerReady(); err != nil {
		return nil, err
	}
	var err error
	if req.GetNodeId() == "" {
		err = d.CSP.UnpublishAll(ctx, d.Array, volID)
	} else if node, _, perr := ParseNodeID(req.GetNodeId()); perr == nil {
		err = d.CSP.Unpublish(ctx, d.Array, volID, csp.PublishRequest{HostUUID: node, AccessProtocol: "fc"})
	}
	if err != nil {
		return nil, toStatus(err, codes.FailedPrecondition)
	}
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ValidateVolumeCapabilities confirms the capabilities checkCaps accepts.
func (d *Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	if err := d.controllerReady(); err != nil {
		return nil, err
	}
	if _, err := d.CSP.GetVolume(ctx, d.Array, req.GetVolumeId()); err != nil {
		return nil, toStatus(err, codes.FailedPrecondition)
	}
	if err := checkCaps(req.GetVolumeCapabilities()); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{Message: status.Convert(err).Message()}, nil
	}
	return &csi.ValidateVolumeCapabilitiesResponse{Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
		VolumeContext:      req.GetVolumeContext(),
		VolumeCapabilities: req.GetVolumeCapabilities(),
		Parameters:         req.GetParameters(),
	}}, nil
}

// ControllerExpandVolume grows the volume to whole GiB; the node then rescans and grows the filesystem.
func (d *Driver) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if req.GetCapacityRange() == nil {
		return nil, status.Error(codes.InvalidArgument, "capacity range is required")
	}
	if c := req.GetVolumeCapability(); c != nil {
		if err := checkCap(c); err != nil {
			return nil, err
		}
	}
	if err := d.controllerReady(); err != nil {
		return nil, err
	}
	size, err := sizeFor(req.GetCapacityRange(), 0)
	if err != nil {
		return nil, err
	}
	v, err := d.CSP.ExpandVolume(ctx, d.Array, req.GetVolumeId(), size)
	if err != nil {
		return nil, toStatus(err, codes.FailedPrecondition)
	}
	return &csi.ControllerExpandVolumeResponse{CapacityBytes: v.Size, NodeExpansionRequired: true}, nil
}

// CreateSnapshot takes a replay of the source volume. A snapshot of the same name is returned if
// it has the same source. Storage Center cannot snapshot a volume that was never written.
func (d *Driver) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	name, srcID := req.GetName(), req.GetSourceVolumeId()
	if name == "" || srcID == "" {
		return nil, status.Error(codes.InvalidArgument, "name and source volume id are required")
	}
	if err := d.controllerReady(); err != nil {
		return nil, err
	}
	name = replayName(name)
	found, err := d.CSP.ListSnapshots(ctx, d.Array, "", name)
	if err != nil && !isNotFound(err) {
		return nil, toStatus(err, codes.Aborted)
	}
	for _, s := range found {
		if s.VolumeID != srcID {
			return nil, status.Errorf(codes.AlreadyExists, "snapshot %s exists for volume %s", name, s.VolumeID)
		}
		return &csi.CreateSnapshotResponse{Snapshot: toCSISnapshot(s)}, nil
	}
	written, err := d.CSP.VolumeWritten(ctx, d.Array, srcID)
	if err != nil {
		return nil, toStatus(err, codes.FailedPrecondition)
	}
	if !written {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s has never been written; Storage Center cannot snapshot it", srcID)
	}
	s, err := d.CSP.CreateSnapshot(ctx, d.Array, csp.SnapshotRequest{Name: name, VolumeID: srcID})
	if err != nil {
		return nil, toStatus(err, codes.AlreadyExists)
	}
	return &csi.CreateSnapshotResponse{Snapshot: toCSISnapshot(s)}, nil
}

// replayName turns a CSI snapshot name into a replay description. Storage
// Center keeps only 31 characters, so a longer name becomes its first 22
// characters, "-" and 8 hex digits of its SHA-256.
func replayName(name string) string {
	const max = 31
	if len(name) <= max {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:max-9] + "-" + hex.EncodeToString(sum[:4])
}

func toCSISnapshot(s csp.Snapshot) *csi.Snapshot {
	return &csi.Snapshot{
		SnapshotId: s.ID, SourceVolumeId: s.VolumeID, SizeBytes: s.Size, ReadyToUse: s.ReadyToUse,
		CreationTime: timestamppb.New(time.Unix(s.CreationTime, 0)),
	}
}

// DeleteSnapshot expires the replay; an unknown snapshot counts as deleted.
func (d *Driver) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	if req.GetSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot id is required")
	}
	if err := d.controllerReady(); err != nil {
		return nil, err
	}
	if err := d.CSP.DeleteSnapshot(ctx, d.Array, req.GetSnapshotId()); err != nil && !isNotFound(err) {
		return nil, toStatus(err, codes.FailedPrecondition)
	}
	return &csi.DeleteSnapshotResponse{}, nil
}

// ListSnapshots lists snapshots, optionally of one ID or one source volume. The page token is
// the offset into the list sorted by snapshot ID.
func (d *Driver) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	if err := d.controllerReady(); err != nil {
		return nil, err
	}
	start, err := pageStart(req.GetStartingToken(), req.GetMaxEntries())
	if err != nil {
		return nil, err
	}
	var snaps []csp.Snapshot
	switch {
	case req.GetSnapshotId() != "":
		s, err := d.CSP.GetSnapshot(ctx, d.Array, req.GetSnapshotId())
		if err != nil && !isNotFound(err) {
			return nil, toStatus(err, codes.Internal)
		}
		if err == nil && (req.GetSourceVolumeId() == "" || req.GetSourceVolumeId() == s.VolumeID) {
			snaps = []csp.Snapshot{s}
		}
	default:
		snaps, err = d.CSP.ListSnapshots(ctx, d.Array, req.GetSourceVolumeId(), "")
		if err != nil && !isNotFound(err) {
			return nil, toStatus(err, codes.Internal)
		}
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].ID < snaps[j].ID })
	page, next, err := paginate(len(snaps), start, req.GetMaxEntries())
	if err != nil {
		return nil, err
	}
	out := &csi.ListSnapshotsResponse{NextToken: next}
	for _, s := range snaps[page[0]:page[1]] {
		out.Entries = append(out.Entries, &csi.ListSnapshotsResponse_Entry{Snapshot: toCSISnapshot(s)})
	}
	return out, nil
}

// ListVolumes lists the volumes in the driver's folder. The page token is the offset into the
// list sorted by volume ID.
func (d *Driver) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	if err := d.controllerReady(); err != nil {
		return nil, err
	}
	start, err := pageStart(req.GetStartingToken(), req.GetMaxEntries())
	if err != nil {
		return nil, err
	}
	vols, err := d.CSP.ListVolumes(ctx, d.Array, "")
	if err != nil {
		return nil, toStatus(err, codes.Internal)
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].ID < vols[j].ID })
	page, next, err := paginate(len(vols), start, req.GetMaxEntries())
	if err != nil {
		return nil, err
	}
	out := &csi.ListVolumesResponse{NextToken: next}
	for _, v := range vols[page[0]:page[1]] {
		out.Entries = append(out.Entries, &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{VolumeId: v.ID, CapacityBytes: v.Size},
		})
	}
	return out, nil
}

func pageStart(token string, max int32) (int, error) {
	if max < 0 {
		return 0, status.Error(codes.InvalidArgument, "max entries must not be negative")
	}
	if token == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(token)
	if err != nil || n < 0 {
		return 0, status.Errorf(codes.Aborted, "invalid starting token %q", token)
	}
	return n, nil
}

// paginate returns the [from, to) bounds of a page of a list of n items and the next token.
func paginate(n, start int, max int32) ([2]int, string, error) {
	if start > n {
		return [2]int{}, "", status.Errorf(codes.Aborted, "starting token %d is past the end (%d)", start, n)
	}
	end := n
	if max > 0 && start+int(max) < n {
		end = start + int(max)
	}
	next := ""
	if end < n {
		next = strconv.Itoa(end)
	}
	return [2]int{start, end}, next, nil
}
