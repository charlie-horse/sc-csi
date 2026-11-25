# sc-csi

sc-csi is a CSI driver for **Dell SC Series** (Compellent) storage arrays
over **Fibre Channel**. CSI (Container Storage Interface) is the standard
way Kubernetes asks a storage system for volumes. With sc-csi, a
PersistentVolumeClaim becomes a volume on the array that is attached to the
node over Fibre Channel and handed to the pod as a filesystem or a raw
block device.

How it works, how to set up the array and the nodes, and how to deploy,
test and build it: [docs/README.md](docs/README.md).

## Credit

sc-csi is a fork of [kube-dc/scseries-csp](https://github.com/kube-dc/scseries-csp)
(Apache-2.0), developed by the [Kube-DC](https://kube-dc.com) team. The
array client and volume logic come from it; the Kubernetes front end is
replaced by a CSI driver. See [NOTICE](NOTICE).

## Disclaimer

This is an independent open-source project. It is not affiliated with,
endorsed by or supported by Dell Technologies. Dell, Compellent and Storage
Center are trademarks of Dell Inc. or its subsidiaries.

## License

[Apache License 2.0](LICENSE)
