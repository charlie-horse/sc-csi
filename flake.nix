{
  description = "sc-csi: CSI driver for Dell SC Series (Compellent) arrays over Fibre Channel";

  # nixpkgs follows the nixos-26.05 channel (pinned in flake.lock).
  inputs.nixpkgs.url = "https://channels.nixos.org/nixos-26.05/nixexprs.tar.xz";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAll (
        pkgs:
        let
          sc-csi = pkgs.callPackage ./nix/package.nix { };
        in
        {
          default = sc-csi;
          inherit sc-csi;
          image = pkgs.callPackage ./nix/image.nix { inherit sc-csi; };
        }
      );

      checks = forAll (
        pkgs:
        import ./nix/checks.nix {
          inherit pkgs;
          sc-csi = self.packages.${pkgs.stdenv.hostPlatform.system}.sc-csi;
        }
      );

      formatter = forAll (pkgs: pkgs.nixfmt-tree);

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go_1_27
            pkgs.gopls
            pkgs.nixfmt
          ];
          env.GOTOOLCHAIN = "local";
        };
      });
    };
}
