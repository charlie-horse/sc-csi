# Deploying

`deploy/kubernetes` is a kustomize directory: plain YAML plus a
`kustomization.yaml` that lists the files and sets the image tag. Apply it
with `kubectl apply -k`, or through your own overlay (a kustomize directory
that points at this one and changes only what your cluster needs).

## Before you start

- The array is set up as in [array-setup.md](array-setup.md).
- Every node that attaches volumes meets
  [node-requirements.md](node-requirements.md).
- For snapshots: the VolumeSnapshot CRDs and the snapshot-controller from
  `kubernetes-csi/external-snapshotter` are installed in the cluster.
- The driver image is in a registry the nodes can pull from
  ([building.md](building.md)). Point the overlay's `images:` entry at it.

## Steps

1. **Create the array Secret.** The controller reads the array address,
   user, password and certificate fingerprint from the Secret that
   `deploy/kubernetes/secret.example.yaml` shows (name and keys). That file
   is not in `kustomization.yaml`: create the Secret yourself, for example
   from a sops-encrypted file. Never commit real values.
2. **Review the StorageClass** in `deploy/kubernetes/storageclass.yaml` and
   the VolumeSnapshotClass in `volumesnapshotclass.yaml`. Change or add
   classes in your overlay.
3. **Limit the node DaemonSet to FC nodes** if some nodes have no FC ports
   (a `nodeSelector` patch in your overlay).
4. **Apply:** `kubectl apply -k <overlay>`.
5. **Check:** the controller and node pods are Ready, and
   `kubectl get csinode <node> -o yaml` lists the driver on each FC node.
   Then create a test PVC and a pod that uses it.

Other controller settings (folders, default profiles, server OS type, TLS
options, log level) are flags and environment variables. They are listed
in the usage text of `cmd/sc-csi/main.go` (`sc-csi --help`).

## StorageClass parameters

The provisioner name is the driver name, `Name` in
`internal/driver/driver.go`. A parameter that is left out uses the
controller's setting of the same kind. Other keys are ignored.

| Parameter | Meaning |
| --- | --- |
| `csi.storage.k8s.io/fstype` | Filesystem for filesystem volumes (`ext4` or `xfs`) |
| `storageProfile` | Name of a storage profile on the array (which disk tiers hold the data) |
| `dataReductionProfile` | Name of a data-reduction profile on the array (for example compression) |
| `snapshotProfile` | Name of a snapshot schedule on the array. Empty sets no schedule; `array-default` keeps the array's own |
| `description` | Text added to the volume's notes on the array |

The parameters apply when a new empty volume is created. A volume restored
from a snapshot or cloned from a volume is a view volume of a replay.

Set `allowVolumeExpansion: true` to allow growing volumes. Use
`volumeMode: Block` in a PVC for a raw block volume; only raw block
volumes may use `ReadWriteMany`.

## Operations

- **Metrics.** The controller serves Prometheus metrics on its metrics
  port: Storage Center REST calls (`cmd/sc-csi/metrics.go`) and array
  health (`internal/monitor/monitor.go`).
- **Audit.** `sc-csi audit` compares the driver's folders on the array
  with the cluster's PVs, nodes and VolumeAttachments, prints the findings
  and exits with status 2 if any remain. `--fix stale-mapping,stale-server`
  repairs those two kinds; `--fix-match <regex>` limits repairs by object
  name. Volumes are only reported, never deleted.
- **Password change.** Set a new password on the array
  (`python3 -I hack/sc-maint.py csp-user --reset-password`), update the
  Secret, then restart the controller pod.
- **Certificate change.** Add the new fingerprint to the Secret and
  restart the controller pod. The controller log names a certificate that
  is not pinned.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| Node pod crashes: "no FC host ports" | The node has no FC ports; keep the DaemonSet off it |
| Controller log: "WWPNs … belong to SC servers outside folder" | The node's FC ports belong to another server object ([array-setup.md](array-setup.md#ports-owned-by-other-server-objects)) |
| Pod stuck attaching: "timeout waiting device" | No FC path to the array (zoning), or the node's multipath configuration does not claim the volume ([node-requirements.md](node-requirements.md)) |
| Pod: "Permission denied" on `/dev/...` | containerd does not hand device ownership to the pod ([node-requirements.md](node-requirements.md#raw-block-volumes-for-non-root-pods)) |
| Snapshot fails: "never been written" | The array cannot snapshot a volume without data |
| PVC deletion stuck: "still published" | The volume is still mapped to a node; check its VolumeAttachments |
