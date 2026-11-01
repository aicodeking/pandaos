# The boat NixOS tests too heavy for the shared `nix flake check`: each gets its
# own runner in .github/workflows/boat-e2e.yml.
#   nix build --impure -f tests/standalone.nix boat-install -L
{
  flake ? "git+file://${toString ../.}",
}:
let
  self = builtins.getFlake flake;
  pkgs = self.inputs.nixpkgs.legacyPackages.x86_64-linux;
  t = import ./lib.nix { inherit (pkgs) lib; };
in
{
  boat-install = import ./boat-install.nix { inherit pkgs self t; };
  boat-ops = import ./boat-ops.nix { inherit pkgs self t; };
}
