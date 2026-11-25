package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// GetPluginInfo returns the driver name and version.
func (d *Driver) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	v := d.Cfg.Version
	if v == "" {
		v = "dev"
	}
	return &csi.GetPluginInfoResponse{Name: Name, VendorVersion: v}, nil
}

// GetPluginCapabilities reports the controller service (on a controller instance) and online expansion.
func (d *Driver) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	caps := []*csi.PluginCapability{{
		Type: &csi.PluginCapability_VolumeExpansion_{VolumeExpansion: &csi.PluginCapability_VolumeExpansion{
			Type: csi.PluginCapability_VolumeExpansion_ONLINE,
		}},
	}}
	if d.CSP != nil {
		caps = append(caps, &csi.PluginCapability{
			Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{
				Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
			}},
		})
	}
	return &csi.GetPluginCapabilitiesResponse{Capabilities: caps}, nil
}

// Probe checks the array session on a controller instance; a node instance is always ready.
// An unreachable array is logged as a warning; the driver still reports ready.
func (d *Driver) Probe(ctx context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	if d.Array != nil {
		var scs []map[string]any
		if err := d.Array.Get(ctx, "StorageCenter/StorageCenter", &scs); err != nil {
			d.Log.Warn("probe: storage center not reachable", "err", err)
		}
	}
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
