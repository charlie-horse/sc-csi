# Testing

| Level | Command | Needs |
| --- | --- | --- |
| Unit | `nix flake check`, or `go test ./...` | Nothing. Pure tests with in-process fakes |
| Fake array | `go test -tags fakearray ./...` | Nothing. Starts an in-memory Storage Center on a local HTTPS port |
| csi-sanity | `go test -tags fakearray ./test/sanity/` | Nothing. Part of the line above |
| End to end | `hack/csi-e2e.sh all` | A cluster with the driver deployed and a real array |
| Path loss | `hack/failover-test.sh all`, run on the node that hosts the test pod | As above, plus root on that node |

## Unit tests

`nix flake check` builds the binary and runs `go test ./...` without build
tags, plus `gofmt`, `go vet` and the deploy and Nix format checks
(`nix/checks.nix`). It is the merge gate. Tests there use no network, no
root and no array.

## The `fakearray` tag

Tests that start the fake array (`internal/sc/scfake`) carry the build tag
`fakearray`, so the default `go test ./...` skips them. The fake serves the
Storage Center REST calls the driver uses and copies the array's quirks;
the list is in the package comment of `internal/sc/scfake/fake.go`.

New Storage Center behaviour goes into the fake, with a tagged test that
shows it.

## csi-sanity

`test/sanity` runs the kubernetes-csi sanity suite (`csi-test`) against
the controller and node services, served in-process on unix sockets. The
controller talks to the fake array; the node uses the fake Connector and
Mounter from `internal/driver/fake`, so no FC hardware or root is needed.
Set `SANITY_LOG=1` to see the driver's log.

## End to end on a cluster

These tests run against real hardware and change state on the array and
the nodes.

- `hack/csi-e2e.sh basic|snapshot|all` applies the manifests in
  `deploy/tests` and checks: a ReadWriteOnce filesystem volume, a raw block
  volume shared by two pods, online expansion, snapshot and restore, and a
  clone. Set `STORAGE_CLASS` and `SNAPSHOT_CLASS` to your class names. Each
  test deletes its namespace and checks that no PV is left.
- `hack/failover-test.sh path|restarts|allpaths|all` runs on the node that
  hosts the I/O pod (`deploy/tests/io-load.yaml`). It takes one path or all
  paths of the test volume offline, or restarts the driver pods, while the
  pod writes. It only touches SCSI paths of the test volume.
- `deploy/tests/rwx-two-node.yaml` shares a raw block volume between pods
  on two nodes; `deploy/tests/kubevirt-vm.yaml` boots a live-migratable
  KubeVirt VM from a raw block volume.
