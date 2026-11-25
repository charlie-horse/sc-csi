# Building

The flake (`flake.nix`, with the build files in `nix/`) builds everything.
The release version is in `nix/version.nix`; it is both the binary's
`sc-csi version` output and the image tag.

| Command | Result |
| --- | --- |
| `nix build` (or `.#sc-csi`) | The `sc-csi` binary, after its unit tests pass |
| `nix build .#image` | The container image for both controller and node, as a `docker load` archive in `./result` |
| `nix flake check` | All checks ([testing.md](testing.md)) |
| `nix develop` | A shell with Go and the Nix formatter |
| `nix fmt` | Formats the `.nix` files |

## Publishing the image

Load or copy the archive into your registry, for example:

```bash
nix build .#image
skopeo copy docker-archive:result docker://<registry>/sc-csi:<version>
```

Then point the `images:` entry of your deploy overlay at that registry
([deploying.md](deploying.md)).

## Changing the version

Change `nix/version.nix` and the image tag in
`deploy/kubernetes/kustomization.yaml` together; a flake check fails when
they differ.

## Changing Go dependencies

After a change to `go.mod` or `go.sum`, update `vendorHash` in
`nix/package.nix`: set it to an empty string, run `nix build`, and copy
the hash from the error message.
