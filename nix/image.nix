# The OCI image for both the controller and the node plugin. It holds the
# tools the node runs inside the container to format, mount and resize.
# Multipath tools are not in the image: the node runs the host's through
# chroot into the host root (HOST_ROOT in deploy/kubernetes/node.yaml).
{
  lib,
  dockerTools,
  runCommand,
  sc-csi,
  coreutils,
  util-linuxMinimal,
  e2fsprogs,
  xfsprogs,
}:
let
  # Only the XFS tools the mounter runs; the full package pulls in Python.
  xfsTools = runCommand "xfs-tools" { } ''
    mkdir -p $out/bin
    cp ${lib.getBin xfsprogs}/bin/{mkfs.xfs,xfs_growfs,xfs_repair} $out/bin/
  '';
in
dockerTools.buildLayeredImage {
  name = "cr.neuros.is/sc-csi";
  tag = sc-csi.version;
  contents = [
    sc-csi
    coreutils
    (lib.getBin util-linuxMinimal)
    (lib.getBin e2fsprogs)
    xfsTools
  ];
  extraCommands = ''
    mkdir -p tmp
    chmod 1777 tmp
  '';
  config = {
    Entrypoint = [ (lib.getExe sc-csi) ];
    Env = [
      "PATH=/bin"
      "TMPDIR=/tmp"
    ];
  };
}
