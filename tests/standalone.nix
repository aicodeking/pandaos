# The boat NixOS tests too heavy for the shared `nix flake check`: each gets its
# own runner in .github/workflows/boat-e2e.yml.
#   nix build --impure -f tests/standalone.nix boat-install -L
{
  flake ? "git+file://${toString ../.}",
  guestDir ? null, # boat-real only: holds vmlinux and rootfs-slim.ext4
}:
let
  self = builtins.getFlake flake;
  pkgs = self.inputs.nixpkgs.legacyPackages.x86_64-linux;
  t = import ./lib.nix { inherit (pkgs) lib; };
in
{
  boat-install = import ./boat-install.nix { inherit pkgs self t; };
  boat-ops = import ./boat-ops.nix { inherit pkgs self t; };
  boat-real = import ./boat-real.nix {
    inherit pkgs self t;
    guest = builtins.path {
      path = /. + guestDir;
      name = "boat-guest";
    };
  };
}
