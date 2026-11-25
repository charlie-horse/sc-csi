# How the driver works

## Terms

| Term | Meaning |
| --- | --- |
| CSI | Container Storage Interface: the gRPC API Kubernetes uses to ask a storage driver for volumes |
| Sidecar | A small standard container from the Kubernetes project that watches Kubernetes objects and calls the driver over a local socket |
| Storage Center (SC) | The array's management software. sc-csi talks to its REST API |
| Volume | A disk on the array. One Kubernetes PersistentVolume is one SC volume |
| Server object | The array's record of a host and its FC ports. The array only shows a volume to a server it is mapped to |
| Mapping | A link between a volume and a server object; it gives the volume a LUN number |
| WWPN | World Wide Port Name: the fixed address of one Fibre Channel port, on the node (initiator) or on the array (target) |
| LUN | Logical unit number: the number a volume has behind a target port |
| Multipath | Linux `dm-multipath`: joins the several FC paths to one volume into one device, `/dev/mapper/<wwid>`, that survives the loss of a path |
| Replay | The array's name for a snapshot |

## Parts

One binary, `sc-csi`, runs in two modes (`deploy/kubernetes`):

- **Controller** (`sc-csi controller`, a Deployment). It holds the array
  login and creates, maps, expands, snapshots and deletes volumes. Its
  sidecars are csi-provisioner, csi-attacher, csi-snapshotter, csi-resizer
  and livenessprobe. Code: `internal/driver/controller.go`, which calls
  `internal/csp` and `internal/sc`.
- **Node** (`sc-csi node`, a DaemonSet on every node). It finds the LUN on
  the fabric, builds the multipath device, makes the filesystem and mounts
  it for the pod. It never talks to the array. Its sidecars are
  node-driver-registrar and livenessprobe. Code: `internal/driver/node.go`,
  `internal/fc` (FC scan and multipath, through Dell's gobrick library) and
  `internal/mounter`.

The driver touches only objects in its own volume folder and server folder
on the array. Everything else on the array is invisible to it.

## From array to pod

1. **Node registration.** The node plugin reads the node's FC port WWPNs
   from `/sys/class/fc_host` and reports a node ID of the form
   `<node name>;<wwpn>,<wwpn>`.
2. **Create** (provisioner → `CreateVolume`). The controller creates the
   volume in its volume folder and applies the StorageClass profiles. A
   volume with the same name is returned instead of a second one.
3. **Map** (attacher → `ControllerPublishVolume`). The controller finds or
   creates the node's server object from the WWPNs in the node ID and maps
   the volume to it. It hands the node the volume's serial (its WWN), the
   LUN and the array's target WWPNs.
4. **Attach** (kubelet → `NodeStageVolume`). The node scans the FC targets
   for the LUN, adds the volume's WWID to multipath, waits for the
   multipath device and checks that it carries the expected serial. For a
   filesystem volume it makes the filesystem if the device is blank and
   mounts it on the staging path. A raw block volume is only attached.
5. **Publish** (kubelet → `NodePublishVolume`). The node bind-mounts the
   staging path (filesystem) or the device (raw block) into the pod.

The node keeps a small state file next to each staging and target path
(`<path>.sc-csi.json`), so later calls know which device belongs to which
path.

## Teardown

The steps run in reverse, and the order matters:

1. `NodeUnpublishVolume` removes the bind mount.
2. `NodeUnstageVolume` unmounts the staging path, flushes the multipath map
   and deletes the SCSI devices of every path.
3. `ControllerUnpublishVolume` removes the mapping on the array. Other
   nodes of a shared raw block volume keep theirs.
4. `DeleteVolume` deletes the volume. The driver refuses while the volume
   is still mapped to any server.

The node must release its devices before the array unmaps the volume.
Removing a mapping first leaves dead paths on the node.

## Other operations

- **Expand.** The controller grows the volume; the node rescans every path,
  grows the multipath map and then the filesystem.
- **Snapshot.** A snapshot is a replay that never expires. The array cannot
  snapshot a volume that was never written; the driver refuses that.
  Replays taken by an array snapshot schedule are not CSI snapshots and the
  driver never deletes them.
- **Restore and clone.** A volume from a snapshot is a view volume of that
  replay. A clone of a volume goes through a temporary replay, which is
  expired once the clone exists. A clone of a never-written volume is a new
  empty volume.

## Access modes

Filesystem volumes are for one node at a time. Raw block volumes may also
be shared by several nodes (ReadWriteMany), for example for virtual machine
live migration. The full check is `checkCap` in
`internal/driver/controller.go`.
