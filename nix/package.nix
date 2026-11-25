# The sc-csi binary. The check phase runs `go test ./...` without build
# tags, so tests tagged `fakearray` (fake array, csi-sanity) do not run.
{ lib, buildGo127Module }:
let
  fs = lib.fileset;
in
buildGo127Module {
  pname = "sc-csi";
  version = import ./version.nix;

  src = fs.toSource {
    root = ../.;
    fileset = fs.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../internal
      ../test
    ];
  };

  vendorHash = "sha256-thqmvaqTeKe0SKnfy4AWDEkv6tN2cU7Ld5iewwyB7Xg=";

  subPackages = [ "cmd/sc-csi" ];
  env.CGO_ENABLED = "0";
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${import ./version.nix}"
  ];

  doCheck = true;
  # Test every package, not only subPackages.
  checkPhase = ''
    runHook preCheck
    go test ./...
    runHook postCheck
  '';

  meta = {
    description = "CSI driver for Dell SC Series arrays over Fibre Channel";
    license = lib.licenses.asl20;
    mainProgram = "sc-csi";
  };
}
