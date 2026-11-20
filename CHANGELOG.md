# Changelog

## 0.3.1

- Fix: replays taken by the array's snapshot schedule are no longer reported as CSI snapshots. The HPE driver refuses to delete a volume that lists any snapshot, so a volume with a snapshot profile (StorageClass `snapshotProfile`, or `array-default`) could not be deleted once the first scheduled replay existed.
- Fix: the `helm.sh/chart` label is valid when the chart version carries build metadata (`0.3.1+<commit>`, as Flux produces for a chart built from a Git repository); the install failed with "a valid label must ...".
- `networkPolicy.hostNetworkCIDRs`: admits the host-network HPE controller where the CNI applies NetworkPolicies to node traffic (Kube-OVN). Without it the controller could not reach the CSP there and every PVC stayed Pending.
- `storageClasses[].labels` and `storageClasses[].annotations`: extra metadata on each StorageClass. Platforms that classify classes by metadata need it; kube-dc otherwise treats a `csi.hpe.com` class as RWO filesystem only.
- `values-kube-dc.yaml` declares the kube-dc storage-class contract for `scseries-block` (tier, kind, capabilities incl. `rwx-block` and `live-migration`, snapshot class).

## 0.3.0

- Helm chart `charts/scseries-csp`: optional pinned HPE CSI Driver 3.3.0 dependency, StorageClass and VolumeSnapshotClass, PodDisruptionBudget, NetworkPolicy, ServiceMonitor, PrometheusRule, audit CronJob, CDI StorageProfile integration.
- Array health monitor: `scseries_array_*` metrics (status, disks, spares, alerts, capacity), polled by the leader.
- `audit --fix stale-mapping,stale-server` with `--fix-match`.
- `cdi-profiles` command: makes CDI StorageProfiles default to RWX raw block with CSI clones.
- Standby replicas forward to the leader instead of being unready, so rolling updates and node drains work.
- Storage and data-reduction profiles default to the array's settings; `array-default` keeps the array's snapshot schedule.
- Generic defaults (folders `scseries-csp`); `values-kube-dc.yaml` profile.

## 0.2.x

- Go implementation: sealed stateless tokens, Lease leader election, idempotent operations, per-array locking, read-only retries, TLS fingerprint pinning, Prometheus metrics, JSON logs.
- Distroless nonroot image.

## 0.1.0

- Python prototype of the CSP contract, used to validate the design against an SC5020.
