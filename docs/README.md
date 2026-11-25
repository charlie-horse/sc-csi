# Docs

These docs explain the code in this repository, with only the array,
fabric and node detail needed to use it. Anything the code states
(versions, options, defaults) is read from the code itself; the docs point
to where. Rules for writing them:
[CLAUDE.md](../CLAUDE.md#documentation-policy).

| Doc | What it explains |
| --- | --- |
| [how-it-works.md](how-it-works.md) | Terms, the controller and node parts, and how a volume gets from the array to a pod and back |
| [array-setup.md](array-setup.md) | Storage Center REST access, TLS pinning, the driver's account and folders, FC zoning, ports held by other server objects |
| [node-requirements.md](node-requirements.md) | What a node needs: FC ports, multipath settings, device ownership for raw block, KubeVirt |
| [deploying.md](deploying.md) | Deploying with kustomize, the array Secret, StorageClass parameters, operations, troubleshooting |
| [testing.md](testing.md) | Unit tests, the `fakearray` tag, csi-sanity, end-to-end and path-loss tests on a cluster |
| [building.md](building.md) | Nix build targets, publishing the image, changing the version and Go dependencies |
