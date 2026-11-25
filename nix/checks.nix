# Every check `nix flake check` builds.
{ pkgs, sc-csi }:
let
  inherit (pkgs) lib;
  fs = lib.fileset;
in
{
  # Builds the binary and runs `go test ./...` (nix/package.nix).
  package = sc-csi;

  # gofmt, and go vet with and without the `fakearray` tag. Vet only
  # compiles the tagged tests; it starts no fake array.
  go-lint = sc-csi.overrideAttrs (_: {
    pname = "sc-csi-lint";
    inherit (sc-csi) goModules;
    doCheck = false;
    buildPhase = ''
      runHook preBuild
      unformatted=$(gofmt -l cmd internal test)
      if [ -n "$unformatted" ]; then
        echo "gofmt needed on:"; echo "$unformatted"; exit 1
      fi
      go vet ./...
      go vet -tags fakearray ./...
      runHook postBuild
    '';
    installPhase = "touch $out";
    postInstall = "";
  });

  # deploy/kubernetes renders with kustomize, and its image tag is the
  # version in nix/version.nix.
  deploy =
    pkgs.runCommand "check-deploy"
      {
        nativeBuildInputs = [
          pkgs.kustomize
          pkgs.yq-go
        ];
        src = ../deploy/kubernetes;
        version = sc-csi.version;
      }
      ''
        kustomize build "$src" > rendered.yaml
        tag=$(yq '.images[] | select(.name == "cr.neuros.is/sc-csi") | .newTag' "$src/kustomization.yaml")
        if [ "$tag" != "$version" ]; then
          echo "deploy/kubernetes/kustomization.yaml tag $tag != nix/version.nix $version"; exit 1
        fi
        grep -q "image: cr.neuros.is/sc-csi:$version" rendered.yaml
        touch $out
      '';

  # Every .nix file is nixfmt-formatted (`nix fmt` fixes it).
  nix-format =
    pkgs.runCommand "check-nix-format"
      {
        nativeBuildInputs = [ pkgs.nixfmt ];
        src = fs.toSource {
          root = ../.;
          fileset = fs.fileFilter (f: f.hasExt "nix") ../.;
        };
      }
      ''
        cd "$src"
        find . -name '*.nix' -print0 | xargs -0 nixfmt --check
        touch $out
      '';
}
