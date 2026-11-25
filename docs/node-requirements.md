# Node requirements

What each node that attaches SC volumes needs. The nodes run NixOS. The
driver installs no packages and writes no multipath configuration; the
host's NixOS configuration provides both.

## What the node plugin uses on the host

Read the exact mounts and settings from `deploy/kubernetes/node.yaml`. In
short, the node container:

- runs privileged, with the host's `/dev`, `/sys`, `/run/udev` and the
  kubelet directory mounted in;
- mounts the host root at a fixed path (`HOST_ROOT`) and runs `multipath`,
  `multipathd` and `dmsetup` there through `chroot`, so it uses the host's
  own binaries and `/etc/multipath*` files;
- searches the host's system profile (`HOST_PATH`) for those tools;
- uses host networking and host IPC, so it reaches the `multipathd`
  socket and syncs with device-mapper and udev on the host;
- carries its own filesystem tools (`nix/image.nix`) for `mkfs`, `mount`,
  `fsck` and resizing.

On attach, the driver adds the volume's WWID to the host's
`/etc/multipath/wwids` (`multipath -a`) and asks `multipathd` to add the
paths. On detach it flushes the map and deletes the SCSI devices.

## Rules for the host

1. **FC ports.** The node has at least one FC port in
   `/sys/class/fc_host`, zoned to the array
   ([array-setup.md](array-setup.md#fibre-channel-zoning)). A node without
   FC ports cannot register the driver: run the node DaemonSet only on FC
   nodes (add a `nodeSelector` in your kustomize overlay), and keep pods
   that use these volumes on the same nodes.
2. **Multipath is on.** The host's NixOS configuration enables
   `services.multipath`, so `multipathd` runs and `multipath`,
   `multipathd` and `dmsetup` are in the system profile.
3. **Finite queueing.** Multipath's built-in entry for `COMPELNT` queues
   I/O forever when every path is lost. That hangs pods and blocks unstage.
   Set `no_path_retry` for vendor `COMPELNT`, product `Compellent Vol`, to
   a finite count (for example 24: with a 5 s polling interval, about two
   minutes of queueing, then I/O errors).
4. **Claim SC volumes, and only those.** If the blacklist excludes every
   WWID (`blacklist { wwid ".*" }`, a safe choice when the node also sees
   other arrays), add a `blacklist_exceptions` WWID entry per array:
   `^36000d31<array serial as 7 hex digits>00`. This also keeps the array's
   vVol protocol endpoints out, as their WWIDs continue with `01`.
   `hack/multipath-config.sh <serial>...` prints the matching settings as a
   `multipath.conf` drop-in; carry them over into the NixOS
   configuration.
5. **Verify** on the node:
   ```bash
   multipath -v3 -d 2>&1 | grep -E 'whitelisted|blacklisted'
   ```
   SC volume paths are whitelisted; other arrays' LUNs and local disks are
   blacklisted.

## Raw block volumes for non-root pods

The driver publishes a raw block volume as a device node owned by root.
Pods that run as another user (for example KubeVirt's virt-launcher or
the CDI importer) can only open it when the container runtime hands device
ownership to the pod. For containerd:

```toml
[plugins."io.containerd.cri.v1.runtime"]
  device_ownership_from_security_context = true
```

Without it, such pods fail with "Permission denied" on `/dev/...`. It
applies to containers created after containerd restarts, on every node the
pod may run on.

## KubeVirt live migration

- VM disks must be raw block volumes with ReadWriteMany access.
- VMs with `host-model` CPUs cannot move between CPU generations. Set a
  common CPU model in the KubeVirt CR (`spec.configuration.cpuModel`) or
  per VM.
- KubeVirt's device plugins need `fs.inotify.max_user_instances` well
  above the kernel default (for example 8192); otherwise the node is marked
  `kubevirt.io/schedulable=false`.
