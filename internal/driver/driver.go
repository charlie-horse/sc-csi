// Package driver is the CSI front end: the gRPC Identity, Controller and Node
// services. The controller side calls internal/csp against one Storage Center;
// the node side attaches Fibre Channel LUNs through a Connector and mounts
// them through a Mounter.
package driver

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/charlie-horse/sc-csi/internal/csp"
	"github.com/charlie-horse/sc-csi/internal/sc"
)

// Name is the CSI driver name (the CSIDriver object and StorageClass provisioner).
const Name = "sc.csi.neuros.is"

// Keys in the publish context that ControllerPublishVolume hands to the node.
const (
	PubSerial  = "serial"  // volume device ID (NAA WWN), lower case hex
	PubLUN     = "lun"     // LUN number on every path
	PubTargets = "targets" // array FC port WWPNs, comma separated
)

// Config holds the settings shared by all services.
type Config struct {
	NodeName string // Kubernetes node name; the node part of the node ID
	Version  string
}

// Driver serves the CSI services. Controller fields are nil on a node-only
// instance; node fields are nil on a controller-only instance.
type Driver struct {
	Cfg Config
	Log *slog.Logger

	// Controller side.
	CSP   *csp.Service
	Array *sc.Client

	// Node side.
	Conn  Connector
	Mount Mounter
	WWPNs func() ([]string, error) // local FC initiator port WWPNs
}

// FCVolume says where a LUN is on the fabric.
type FCVolume struct {
	Serial  string   // device ID (NAA WWN) the multipath device must carry
	LUN     int
	Targets []string // array port WWPNs
}

// Device is an attached block device on the node.
type Device struct {
	Path string // e.g. /dev/mapper/36000d31...; what is mounted or bind-mounted
	WWN  string
}

// Connector attaches and detaches FC LUNs on the node (gobrick in production).
type Connector interface {
	Connect(ctx context.Context, v FCVolume) (Device, error)
	// Disconnect flushes the multipath map and deletes the SCSI devices of the
	// volume with this serial. It succeeds if nothing is attached.
	Disconnect(ctx context.Context, serial string) error
	// Resize rescans the paths and grows the multipath map after an expand.
	Resize(ctx context.Context, serial string) error
}

// Mounter formats, mounts and resizes filesystems (k8s mount-utils in production).
type Mounter interface {
	FormatAndMount(source, target, fsType string, options []string) error
	BindMount(source, target string, readOnly bool) error // file or directory target
	Unmount(target string) error                          // succeeds if not mounted
	IsMountPoint(path string) (bool, error)
	ResizeFS(device, mountPath string) error
	Stats(path string) (VolumeStats, error)
	DeviceSize(device string) (int64, error)
}

// VolumeStats are filesystem usage numbers for NodeGetVolumeStats.
type VolumeStats struct {
	TotalBytes, UsedBytes, AvailBytes    int64
	TotalInodes, UsedInodes, FreeInodes int64
}

// NodeID is "<node name>;<wwpn>,<wwpn>...": the controller learns the node's
// FC initiators from it and registers them on the array at publish time.
func NodeID(name string, wwpns []string) string {
	return name + ";" + strings.Join(wwpns, ",")
}

// ParseNodeID splits a node ID made by NodeID.
func ParseNodeID(id string) (name string, wwpns []string, err error) {
	name, list, ok := strings.Cut(id, ";")
	if !ok || name == "" || list == "" {
		return "", nil, fmt.Errorf("node id %q is not <name>;<wwpn>,...", id)
	}
	return name, strings.Split(list, ","), nil
}
