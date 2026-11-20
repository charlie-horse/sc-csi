# kube-dc profile

[Kube-DC](https://kube-dc.com) uses scseries-csp for KubeVirt VM disks and container PVCs on SC Series arrays. [charts/scseries-csp/values-kube-dc.yaml](../charts/scseries-csp/values-kube-dc.yaml) carries the platform choices; nothing in the CSP depends on kube-dc.

| Setting | kube-dc choice | Why |
|---|---|---|
| `csp.volumeFolder` / `serverFolder` | `kube-dc` | one folder pair per platform on shared arrays |
| StorageClass `scseries-block` | `allowedTopologies` on `csi.hpe.com/zone=sc` | only FC-labelled nodes attach SC volumes |
| StorageClass labels and annotations | `storage.kube-dc.com/tier`, `kind`, `capabilities`, `snapshot-class`, `display-name` | kube-dc reads what a class can do from this metadata, not from its name; without the tier label a `csi.hpe.com` class is offered as RWO filesystem only, so the console would not offer live migration on it |
| `networkPolicy.hostNetworkCIDRs` | the cluster's Kube-OVN join subnet (`kubectl get subnet join`), in the site file | the HPE controller is a host-network pod and Kube-OVN applies the CSP's NetworkPolicy to node traffic; without it provisioning times out |
| `cdi.storageProfiles.enabled` | true | VM DataVolumes default to RWX raw block (live migration) |
| `serviceMonitor`, `prometheusRule` | enabled | CSP and array health in platform monitoring |
| `audit.enabled` | true | hourly consistency check |

Site specifics (array address, certificate pin, credentials via `existingSecret`, storage profiles per tier) belong in a separate values file that is not committed:

```bash
helm pull oci://ghcr.io/kube-dc/charts/scseries-csp --version 0.3.1 --untar   # for values-kube-dc.yaml
helm upgrade --install scseries-csp oci://ghcr.io/kube-dc/charts/scseries-csp --version 0.3.1 \
  -n hpe-storage --create-namespace -f scseries-csp/values-kube-dc.yaml -f site.yaml
```

A site with several tiers declares one class per storage profile, each with its own tier label (`performance`, `balanced`, `capacity`) and the same capability list. To run the HPE node plugin only on FC nodes, add `hpeCsiDriver.node.nodeSelector` with the zone label; a node then gets the plugin when it is labelled.

Node preparation (multipath drop-ins, containerd device ownership, inotify, the zone label) belongs in the node image or provisioning; see [node-requirements.md](node-requirements.md).
