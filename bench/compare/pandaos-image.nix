# The installed PandaOS headless system (nixosConfigurations.headless:
# mkPandaSystem + target.nix) as a compressed qcow2, for bench/compare/run.py.
#
#   BENCH_SSH_PUBKEY="$(cat key.pub)" nix build --impure -L \
#     --expr "import ./bench/compare/pandaos-image.nix { flake = builtins.getFlake \"git+file://$PWD\"; }"
#
# Differences from a panda-install'ed box, all for the benchmark:
#   - disk layout: make-disk-image's ext4 root + ESP (with growpart), not the
#     btrfs subvolumes; btrbk snapshots then skip themselves (no /.snapshots).
#   - the interactive first-boot wizard (tty8) is off, as in the VM tests;
#     the cloud images' cloud-init does the same job unattended.
#   - the ssh key goes to `panda` here instead of via cloud-init.
{ flake, pubkey ? builtins.getEnv "BENCH_SSH_PUBKEY" }:
assert pubkey != "" || throw "set BENCH_SSH_PUBKEY (and pass --impure)";
let
  inherit (flake.inputs) nixpkgs;
  sys = flake.lib.mkPandaSystem {
    profile = "headless";
    modules = [
      "${flake}/nixos/installer/target.nix"
      ({ lib, ... }: {
        fileSystems = lib.mkForce {
          "/" = {
            device = "/dev/disk/by-label/nixos";
            fsType = "ext4";
            autoResize = true;
          };
          "/boot" = {
            device = "/dev/disk/by-label/ESP";
            fsType = "vfat";
            options = [ "fmask=0077" "dmask=0077" ];
          };
        };
        boot.growPartition = true;
        pandaos.firstboot.enable = false;
        users.users.panda.openssh.authorizedKeys.keys = [ pubkey ];
      })
    ];
  };
in
import "${nixpkgs}/nixos/lib/make-disk-image.nix" {
  inherit (sys) config pkgs;
  inherit (nixpkgs) lib;
  format = "qcow2-compressed";
  partitionTableType = "efi";
  diskSize = "auto";
  additionalSpace = "1024M";
  copyChannel = false;
}
