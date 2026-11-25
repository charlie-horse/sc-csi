//go:build fakearray

package driver

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"

	"github.com/charlie-horse/sc-csi/internal/csp"
	"github.com/charlie-horse/sc-csi/internal/sc"
	"github.com/charlie-horse/sc-csi/internal/sc/scfake"
)

func setup(t *testing.T) (*scfake.Array, *Driver, context.Context) {
	t.Helper()
	a := scfake.New()
	t.Cleanup(a.Close)
	host, port := a.HostPort()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := sc.New(ctx, sc.Config{Host: host, Port: port, User: a.User, Password: a.Password, Fingerprint: a.Fingerprint(), MaxRetries: 1}, log, nil)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return a, &Driver{Cfg: Config{Version: "test"}, Log: log, CSP: &csp.Service{Cfg: csp.Defaults(), Log: log}, Array: c}, ctx
}

var rwo = mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)

func create(t *testing.T, d *Driver, ctx context.Context, name string, bytes int64, src *csi.VolumeContentSource) *csi.Volume {
	t.Helper()
	r, err := d.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: name, CapacityRange: &csi.CapacityRange{RequiredBytes: bytes},
		VolumeCapabilities: []*csi.VolumeCapability{rwo}, VolumeContentSource: src})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return r.Volume
}

func TestProbeChecksArray(t *testing.T) {
	a, d, ctx := setup(t)
	if r, err := d.Probe(ctx, &csi.ProbeRequest{}); err != nil || !r.GetReady().GetValue() {
		t.Fatalf("probe: %v %v", r, err)
	}
	a.Close()
	if r, err := d.Probe(ctx, &csi.ProbeRequest{}); err != nil || !r.GetReady().GetValue() {
		t.Errorf("probe with array down: want ready=true, got %v %v", r, err)
	}
}

func TestReplayNameFitsDescription(t *testing.T) {
	if got := replayName("short"); got != "short" {
		t.Errorf("short name changed: %q", got)
	}
	a, b := replayName("snapshot-unrelated-1-aaaaaaaaaaaa"), replayName("snapshot-unrelated-2-aaaaaaaaaaaa")
	if len(a) != 31 || a == b {
		t.Errorf("long names: %q %q", a, b)
	}
}

func TestCreateVolumeIdempotencyAndParams(t *testing.T) {
	a, d, ctx := setup(t)
	req := &csi.CreateVolumeRequest{Name: "pvc-1", CapacityRange: &csi.CapacityRange{RequiredBytes: gib + 1},
		VolumeCapabilities: []*csi.VolumeCapability{rwo},
		Parameters: map[string]string{ParamStorageProfile: "High Priority (Tier 1)", ParamDataReductionProfile: "None",
			ParamDescription: "hello", "csi.storage.k8s.io/pvc/name": "data"}}
	r1, err := d.CreateVolume(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Volume.CapacityBytes != 2*gib || r1.Volume.VolumeId == "" {
		t.Fatalf("bad volume: %v", r1.Volume)
	}
	if a.Volume(r1.Volume.VolumeId)["name"] != "pvc-1" {
		t.Error("volume id must be the SC instance id")
	}
	v, _ := d.CSP.GetVolume(ctx, d.Array, r1.Volume.VolumeId)
	if v.Config["storage_profile"] != "High Priority (Tier 1)" || v.Config["data_reduction_profile"] != "None" {
		t.Errorf("profiles not applied: %v", v.Config)
	}
	r2, err := d.CreateVolume(ctx, req)
	if err != nil || r2.Volume.VolumeId != r1.Volume.VolumeId {
		t.Fatalf("second create must return the same volume: %v %v", r2, err)
	}
	req.CapacityRange = &csi.CapacityRange{RequiredBytes: 5 * gib}
	if _, err := d.CreateVolume(ctx, req); code(err) != codes.AlreadyExists {
		t.Errorf("same name other size: %v", err)
	}
	if _, err := d.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: "pvc-x", VolumeCapabilities: []*csi.VolumeCapability{rwo},
		Parameters: map[string]string{ParamStorageProfile: "nope"}}); code(err) != codes.InvalidArgument {
		t.Errorf("unknown profile: %v", err)
	}
	if _, err := d.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: "pvc-y", VolumeCapabilities: []*csi.VolumeCapability{rwo},
		CapacityRange: &csi.CapacityRange{RequiredBytes: gib + 1, LimitBytes: gib + 2}}); code(err) != codes.OutOfRange {
		t.Errorf("limit below whole GiB: %v", err)
	}
	def := create(t, d, ctx, "pvc-default", 0, nil)
	if def.CapacityBytes != gib {
		t.Errorf("default size: %d", def.CapacityBytes)
	}
}

func TestDeleteVolumeIdempotent(t *testing.T) {
	a, d, ctx := setup(t)
	v := create(t, d, ctx, "pvc-del", gib, nil)
	for i := 0; i < 2; i++ {
		if _, err := d.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: v.VolumeId}); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	if a.Volume(v.VolumeId) != nil {
		t.Error("volume still there")
	}
	if _, err := d.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: "64012.999999"}); err != nil {
		t.Errorf("unknown id: %v", err)
	}
}

func TestPublishUnpublish(t *testing.T) {
	a, d, ctx := setup(t)
	v := create(t, d, ctx, "pvc-pub", gib, nil)
	nodeA := NodeID("node-a", []string{"21:00:00:24:ff:00:00:01", "21000024ff000002"})
	nodeB := NodeID("node-b", []string{"21000024ff0000b1"})
	pub := func(node string, c *csi.VolumeCapability, ro bool) (*csi.ControllerPublishVolumeResponse, error) {
		return d.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{VolumeId: v.VolumeId, NodeId: node, VolumeCapability: c, Readonly: ro})
	}
	r, err := pub(nodeA, rwo, false)
	if err != nil {
		t.Fatal(err)
	}
	pc := r.PublishContext
	if pc[PubLUN] != "1" || !strings.HasPrefix(pc[PubSerial], "6000d31") || len(strings.Split(pc[PubTargets], ",")) != 2 {
		t.Errorf("publish context: %v", pc)
	}
	if srv := a.Servers()["node-a"]; srv == nil || !strings.Contains(srv["notes"].(string), "csp-node-uuid=node-a") {
		t.Errorf("SC server not created for node: %v", srv)
	}
	again, err := pub(nodeA, rwo, true)
	if err != nil || again.PublishContext[PubLUN] != "1" || len(a.Mappings(v.VolumeId)) != 1 {
		t.Errorf("publish must be idempotent: %v %v %v", again, err, a.Mappings(v.VolumeId))
	}
	if _, err := pub(nodeB, rwo, false); code(err) != codes.FailedPrecondition {
		t.Errorf("single-node volume on a second node: %v", err)
	}
	rwx := blockCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)
	if _, err := pub(nodeB, rwx, false); err != nil {
		t.Fatalf("multi-node block publish: %v", err)
	}
	if got := a.Mappings(v.VolumeId); len(got) != 2 {
		t.Fatalf("both nodes mapped: %v", got)
	}
	if _, err := d.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{VolumeId: "64012.999999", NodeId: nodeA, VolumeCapability: rwo}); code(err) != codes.NotFound {
		t.Errorf("unknown volume: %v", err)
	}
	if _, err := pub("some-fake-node-id", rwo, false); code(err) != codes.NotFound {
		t.Errorf("malformed node: %v", err)
	}
	if _, err := d.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: v.VolumeId}); code(err) != codes.FailedPrecondition {
		t.Errorf("delete while published: %v", err)
	}
	unpub := func(node string) error {
		_, err := d.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{VolumeId: v.VolumeId, NodeId: node})
		return err
	}
	if err := unpub(nodeA); err != nil {
		t.Fatal(err)
	}
	if got := a.Mappings(v.VolumeId); len(got) != 1 || got[0] != "node-b" {
		t.Errorf("unpublish must keep the other node: %v", got)
	}
	for _, n := range []string{nodeA, "garbage", NodeID("node-z", []string{"aa"})} {
		if err := unpub(n); err != nil {
			t.Errorf("unpublish %q must be idempotent: %v", n, err)
		}
	}
	a.AddServer("legacy-esxi", "", "21000024ff0000e1")
	if err := unpub(""); err != nil {
		t.Fatal(err)
	}
	if got := a.Mappings(v.VolumeId); len(got) != 0 {
		t.Errorf("empty node id unpublishes everywhere: %v", got)
	}
	if _, err := d.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: v.VolumeId}); err != nil {
		t.Fatal(err)
	}
	if err := unpub(nodeB); err != nil {
		t.Errorf("unpublish of a deleted volume: %v", err)
	}
}

func TestValidateAndExpand(t *testing.T) {
	_, d, ctx := setup(t)
	v := create(t, d, ctx, "pvc-exp", gib, nil)
	r, err := d.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{VolumeId: v.VolumeId, VolumeCapabilities: []*csi.VolumeCapability{rwo}})
	if err != nil || r.Confirmed == nil {
		t.Errorf("validate rwo: %v %v", r, err)
	}
	r, err = d.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{VolumeId: v.VolumeId,
		VolumeCapabilities: []*csi.VolumeCapability{mountCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)}})
	if err != nil || r.Confirmed != nil || r.Message == "" {
		t.Errorf("validate rwx mount must not be confirmed: %v %v", r, err)
	}
	if _, err := d.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{VolumeId: "64012.999999",
		VolumeCapabilities: []*csi.VolumeCapability{rwo}}); code(err) != codes.NotFound {
		t.Errorf("validate unknown: %v", err)
	}
	e, err := d.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{VolumeId: v.VolumeId, CapacityRange: &csi.CapacityRange{RequiredBytes: 2*gib + 5}})
	if err != nil || e.CapacityBytes != 3*gib || !e.NodeExpansionRequired {
		t.Errorf("expand: %v %v", e, err)
	}
	e, err = d.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{VolumeId: v.VolumeId, CapacityRange: &csi.CapacityRange{RequiredBytes: gib}})
	if err != nil || e.CapacityBytes != 3*gib {
		t.Errorf("shrink is a no-op: %v %v", e, err)
	}
	if _, err := d.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{VolumeId: "64012.999999", CapacityRange: &csi.CapacityRange{RequiredBytes: gib}}); code(err) != codes.NotFound {
		t.Errorf("expand unknown: %v", err)
	}
}

func TestSnapshotsRestoreClone(t *testing.T) {
	a, d, ctx := setup(t)
	src := create(t, d, ctx, "pvc-src", gib, nil)
	other := create(t, d, ctx, "pvc-other", gib, nil)
	name := "snapshot-8e26c65c-8a91-4823-9de6-aba59c3923c7"
	snapReq := &csi.CreateSnapshotRequest{Name: name, SourceVolumeId: src.VolumeId}
	if _, err := d.CreateSnapshot(ctx, snapReq); code(err) != codes.FailedPrecondition {
		t.Errorf("snapshot of a never-written volume: %v", err)
	}
	if _, err := d.CreateSnapshot(ctx, &csi.CreateSnapshotRequest{Name: "s", SourceVolumeId: "64012.999999"}); code(err) != codes.NotFound {
		t.Errorf("snapshot of unknown volume: %v", err)
	}
	a.SetActive(src.VolumeId)
	a.SetActive(other.VolumeId)
	s1, err := d.CreateSnapshot(ctx, snapReq)
	if err != nil {
		t.Fatal(err)
	}
	snap := s1.Snapshot
	if snap.SourceVolumeId != src.VolumeId || !snap.ReadyToUse || snap.SizeBytes != gib || snap.CreationTime == nil {
		t.Errorf("snapshot: %v", snap)
	}
	s2, err := d.CreateSnapshot(ctx, snapReq)
	if err != nil || s2.Snapshot.SnapshotId != snap.SnapshotId {
		t.Errorf("snapshot idempotency: %v %v", s2, err)
	}
	if _, err := d.CreateSnapshot(ctx, &csi.CreateSnapshotRequest{Name: name, SourceVolumeId: other.VolumeId}); code(err) != codes.AlreadyExists {
		t.Errorf("same name other source: %v", err)
	}
	s3, _ := d.CreateSnapshot(ctx, &csi.CreateSnapshotRequest{Name: "snap-other", SourceVolumeId: other.VolumeId})

	list := func(r *csi.ListSnapshotsRequest) []string {
		t.Helper()
		out, err := d.ListSnapshots(ctx, r)
		if err != nil {
			t.Fatalf("list %v: %v", r, err)
		}
		var ids []string
		for _, e := range out.Entries {
			ids = append(ids, e.Snapshot.SnapshotId)
		}
		return ids
	}
	if got := list(&csi.ListSnapshotsRequest{}); len(got) != 2 {
		t.Errorf("list all: %v", got)
	}
	if got := list(&csi.ListSnapshotsRequest{SnapshotId: snap.SnapshotId}); len(got) != 1 || got[0] != snap.SnapshotId {
		t.Errorf("list by id: %v", got)
	}
	if got := list(&csi.ListSnapshotsRequest{SnapshotId: "64012.999.9"}); len(got) != 0 {
		t.Errorf("list unknown id: %v", got)
	}
	if got := list(&csi.ListSnapshotsRequest{SourceVolumeId: other.VolumeId}); len(got) != 1 || got[0] != s3.Snapshot.SnapshotId {
		t.Errorf("list by source: %v", got)
	}
	if got := list(&csi.ListSnapshotsRequest{SourceVolumeId: "64012.999999"}); len(got) != 0 {
		t.Errorf("list unknown source: %v", got)
	}
	page, _ := d.ListSnapshots(ctx, &csi.ListSnapshotsRequest{MaxEntries: 1})
	if len(page.Entries) != 1 || page.NextToken != "1" {
		t.Fatalf("first page: %v", page)
	}
	page2, _ := d.ListSnapshots(ctx, &csi.ListSnapshotsRequest{MaxEntries: 1, StartingToken: page.NextToken})
	if len(page2.Entries) != 1 || page2.NextToken != "" || page2.Entries[0].Snapshot.SnapshotId == page.Entries[0].Snapshot.SnapshotId {
		t.Errorf("second page: %v", page2)
	}

	// restore, grown to the requested size
	restored := create(t, d, ctx, "pvc-restored", 2*gib, &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
		Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: snap.SnapshotId}}})
	if restored.CapacityBytes != 2*gib || restored.ContentSource.GetSnapshot() == nil {
		t.Errorf("restore: %v", restored)
	}
	if _, err := d.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: "pvc-r2", VolumeCapabilities: []*csi.VolumeCapability{rwo},
		VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "64012.999.9"}}}}); code(err) != codes.NotFound {
		t.Errorf("restore from unknown snapshot: %v", err)
	}

	// clone of a written volume goes through a helper snapshot that is expired afterwards
	before := a.Replays(src.VolumeId)
	cl := create(t, d, ctx, "pvc-clone", 0, &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
		Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: src.VolumeId}}})
	if cl.CapacityBytes != gib || cl.ContentSource.GetVolume() == nil {
		t.Errorf("clone: %v", cl)
	}
	if after := a.Replays(src.VolumeId); len(after) != len(before) {
		t.Errorf("helper snapshot left behind: %v -> %v", before, after)
	}
	if a.Volume(cl.VolumeId)["parentReplay"] == "" {
		t.Error("clone of a written volume must be a view of a replay")
	}
	// clone of a never-written volume is a new empty volume
	empty := create(t, d, ctx, "pvc-empty", 3*gib, nil)
	ec := create(t, d, ctx, "pvc-empty-clone", gib, &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
		Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: empty.VolumeId}}})
	if ec.CapacityBytes != 3*gib {
		t.Errorf("clone is at least the source size: %d", ec.CapacityBytes)
	}
	if _, err := d.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: "pvc-c2", VolumeCapabilities: []*csi.VolumeCapability{rwo},
		VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
			Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: "64012.999999"}}}}); code(err) != codes.NotFound {
		t.Errorf("clone of unknown volume: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := d.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{SnapshotId: snap.SnapshotId}); err != nil {
			t.Fatalf("delete snapshot %d: %v", i, err)
		}
	}
	if got := list(&csi.ListSnapshotsRequest{SnapshotId: snap.SnapshotId}); len(got) != 0 {
		t.Errorf("deleted snapshot still listed: %v", got)
	}
}

func TestListVolumesPagination(t *testing.T) {
	a, d, ctx := setup(t)
	a.AddVolume("foreign", "", 1)
	for _, n := range []string{"a", "b", "c"} {
		create(t, d, ctx, "pvc-"+n, gib, nil)
	}
	var ids []string
	token := ""
	for {
		r, err := d.ListVolumes(ctx, &csi.ListVolumesRequest{MaxEntries: 2, StartingToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range r.Entries {
			ids = append(ids, e.Volume.VolumeId)
		}
		if token = r.NextToken; token == "" {
			break
		}
	}
	if len(ids) != 3 {
		t.Errorf("want the 3 owned volumes, got %v", ids)
	}
	if _, err := d.ListVolumes(ctx, &csi.ListVolumesRequest{StartingToken: "99"}); code(err) != codes.Aborted {
		t.Errorf("token past end: %v", err)
	}
}
