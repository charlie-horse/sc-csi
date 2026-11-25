package driver

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/charlie-horse/sc-csi/internal/csp"
	"github.com/charlie-horse/sc-csi/internal/sc"
)

func code(err error) codes.Code { return status.Code(err) }

func mountCap(m csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: m},
	}
}

func blockCap(m csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: m},
	}
}

func TestIdentity(t *testing.T) {
	ctx := context.Background()
	d := &Driver{Cfg: Config{Version: "1.2.3"}}
	info, err := d.GetPluginInfo(ctx, &csi.GetPluginInfoRequest{})
	if err != nil || info.Name != Name || info.VendorVersion != "1.2.3" {
		t.Fatalf("plugin info: %v %v", info, err)
	}
	caps, _ := d.GetPluginCapabilities(ctx, &csi.GetPluginCapabilitiesRequest{})
	for _, c := range caps.Capabilities {
		if c.GetService() != nil {
			t.Error("a node-only instance must not report the controller service")
		}
	}
	if len(caps.Capabilities) != 1 || caps.Capabilities[0].GetVolumeExpansion().GetType() != csi.PluginCapability_VolumeExpansion_ONLINE {
		t.Errorf("online expansion missing: %v", caps.Capabilities)
	}
	d.CSP = &csp.Service{}
	caps, _ = d.GetPluginCapabilities(ctx, &csi.GetPluginCapabilitiesRequest{})
	var ctrl bool
	for _, c := range caps.Capabilities {
		ctrl = ctrl || c.GetService().GetType() == csi.PluginCapability_Service_CONTROLLER_SERVICE
	}
	if !ctrl {
		t.Error("controller instance must report CONTROLLER_SERVICE")
	}
	p, err := (&Driver{}).Probe(ctx, &csi.ProbeRequest{})
	if err != nil || !p.GetReady().GetValue() {
		t.Errorf("node probe: %v %v", p, err)
	}
}

func TestControllerCapabilities(t *testing.T) {
	r, _ := (&Driver{}).ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	got := map[csi.ControllerServiceCapability_RPC_Type]bool{}
	for _, c := range r.Capabilities {
		got[c.GetRpc().GetType()] = true
	}
	for _, want := range []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME, csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT, csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
		csi.ControllerServiceCapability_RPC_CLONE_VOLUME, csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
	} {
		if !got[want] {
			t.Errorf("missing %s", want)
		}
	}
	if got[csi.ControllerServiceCapability_RPC_GET_CAPACITY] {
		t.Error("GET_CAPACITY is not implemented")
	}
}

func TestCheckCaps(t *testing.T) {
	m := csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
	ok := []*csi.VolumeCapability{
		mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER),
		mountCap(csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY),
		blockCap(m),
		blockCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY),
	}
	for _, c := range ok {
		if err := checkCap(c); err != nil {
			t.Errorf("%v: %v", c, err)
		}
	}
	bad := []*csi.VolumeCapability{
		nil,
		mountCap(m),
		mountCap(csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER),
		mountCap(csi.VolumeCapability_AccessMode_UNKNOWN),
		{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
		{AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}},
	}
	for _, c := range bad {
		if err := checkCap(c); code(err) != codes.InvalidArgument {
			t.Errorf("%v: want InvalidArgument, got %v", c, err)
		}
	}
	if err := checkCaps(nil); code(err) != codes.InvalidArgument {
		t.Errorf("empty caps: %v", err)
	}
}

func TestSizeFor(t *testing.T) {
	cases := []struct {
		req, lim, min, want int64
		code                codes.Code
	}{
		{0, 0, 0, gib, codes.OK},
		{1, 0, 0, gib, codes.OK},
		{gib + 1, 0, 0, 2 * gib, codes.OK},
		{0, 0, 3 * gib, 3 * gib, codes.OK},
		{gib, gib, 0, gib, codes.OK},
		{gib + 1, 2*gib - 1, 0, 0, codes.OutOfRange},
		{2 * gib, gib, 0, 0, codes.InvalidArgument},
		{-1, 0, 0, 0, codes.InvalidArgument},
		{0, gib, 2 * gib, 0, codes.OutOfRange},
	}
	for _, c := range cases {
		got, err := sizeFor(&csi.CapacityRange{RequiredBytes: c.req, LimitBytes: c.lim}, c.min)
		if code(err) != c.code || got != c.want {
			t.Errorf("%+v: got %d %v", c, got, err)
		}
	}
}

func TestPaginate(t *testing.T) {
	if _, err := pageStart("x", 0); code(err) != codes.Aborted {
		t.Errorf("bad token: %v", err)
	}
	if _, err := pageStart("", -1); code(err) != codes.InvalidArgument {
		t.Errorf("negative max: %v", err)
	}
	p, next, err := paginate(5, 0, 2)
	if err != nil || p != [2]int{0, 2} || next != "2" {
		t.Errorf("first page: %v %q %v", p, next, err)
	}
	p, next, _ = paginate(5, 4, 2)
	if p != [2]int{4, 5} || next != "" {
		t.Errorf("last page: %v %q", p, next)
	}
	if _, _, err := paginate(5, 6, 0); code(err) != codes.Aborted {
		t.Errorf("past end: %v", err)
	}
}

func TestToStatus(t *testing.T) {
	cases := []struct {
		err  error
		want codes.Code
	}{
		{nil, codes.OK},
		{&csp.Error{Status: http.StatusNotFound}, codes.NotFound},
		{&csp.Error{Status: http.StatusBadRequest}, codes.InvalidArgument},
		{&csp.Error{Status: http.StatusConflict}, codes.AlreadyExists},
		{&csp.Error{Status: http.StatusServiceUnavailable}, codes.Unavailable},
		{&csp.Error{Status: http.StatusInternalServerError}, codes.Internal},
		{&sc.Error{Status: 503}, codes.Unavailable},
		{&sc.Error{Status: 401}, codes.Unauthenticated},
		{&sc.Error{Status: 400}, codes.Internal},
		{context.DeadlineExceeded, codes.DeadlineExceeded},
		{status.Error(codes.Aborted, "x"), codes.Aborted},
		{errors.New("boom"), codes.Internal},
	}
	for _, c := range cases {
		if got := code(toStatus(c.err, codes.AlreadyExists)); got != c.want {
			t.Errorf("%v: got %s want %s", c.err, got, c.want)
		}
	}
	if got := code(toStatus(&csp.Error{Status: 409}, codes.FailedPrecondition)); got != codes.FailedPrecondition {
		t.Errorf("conflict code not used: %s", got)
	}
}

// Argument checks run before the array is touched, so a driver without one answers them.
func TestControllerArgumentValidation(t *testing.T) {
	ctx := context.Background()
	d := &Driver{}
	sn := mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	check := func(name string, err error, want codes.Code) {
		t.Helper()
		if code(err) != want {
			t.Errorf("%s: want %s, got %v", name, want, err)
		}
	}
	_, err := d.CreateVolume(ctx, &csi.CreateVolumeRequest{VolumeCapabilities: []*csi.VolumeCapability{sn}})
	check("create no name", err, codes.InvalidArgument)
	_, err = d.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: "v"})
	check("create no caps", err, codes.InvalidArgument)
	_, err = d.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: "v", VolumeCapabilities: []*csi.VolumeCapability{
		mountCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)}})
	check("create RWX mount", err, codes.InvalidArgument)
	_, err = d.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: "v", VolumeCapabilities: []*csi.VolumeCapability{sn}})
	check("create without array", err, codes.FailedPrecondition)
	_, err = d.DeleteVolume(ctx, &csi.DeleteVolumeRequest{})
	check("delete no id", err, codes.InvalidArgument)
	_, err = d.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{NodeId: "n;w", VolumeCapability: sn})
	check("publish no volume", err, codes.InvalidArgument)
	_, err = d.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{VolumeId: "v", VolumeCapability: sn})
	check("publish no node", err, codes.InvalidArgument)
	_, err = d.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{VolumeId: "v", NodeId: "n;w"})
	check("publish no cap", err, codes.InvalidArgument)
	_, err = d.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{NodeId: "n;w"})
	check("unpublish no volume", err, codes.InvalidArgument)
	_, err = d.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{VolumeCapabilities: []*csi.VolumeCapability{sn}})
	check("validate no id", err, codes.InvalidArgument)
	_, err = d.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{VolumeId: "v"})
	check("validate no caps", err, codes.InvalidArgument)
	_, err = d.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{CapacityRange: &csi.CapacityRange{RequiredBytes: gib}})
	check("expand no id", err, codes.InvalidArgument)
	_, err = d.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{VolumeId: "v"})
	check("expand no range", err, codes.InvalidArgument)
	_, err = d.CreateSnapshot(ctx, &csi.CreateSnapshotRequest{SourceVolumeId: "v"})
	check("snapshot no name", err, codes.InvalidArgument)
	_, err = d.CreateSnapshot(ctx, &csi.CreateSnapshotRequest{Name: "s"})
	check("snapshot no source", err, codes.InvalidArgument)
	_, err = d.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{})
	check("delete snapshot no id", err, codes.InvalidArgument)
	_, err = d.GetCapacity(ctx, &csi.GetCapacityRequest{})
	check("get capacity", err, codes.Unimplemented)
}
