# The PandaOS disk layout, as created by panda-install (nixos/installer):
# GPT, a 512M ESP labelled PANDABOOT, and the rest as btrfs labelled PANDAOS
# with one subvolume per mount. Mounting by label means no per-machine
# fileSystems: the installer runs nixos-generate-config --no-filesystems.
{ lib, ... }:
let
  subvolumes = {
    "/" = "@";
    "/nix" = "@nix";
    "/home" = "@home";
    "/mem" = "@mem";
    "/var/log" = "@log";
    "/.snapshots" = "@snapshots";
  };
  btrfs = subvol: {
    device = "/dev/disk/by-label/PANDAOS";
    fsType = "btrfs";
    options = [
      "subvol=${subvol}"
      "compress=zstd:1"
      "noatime"
    ];
  };
in
{
  boot.loader.systemd-boot = {
    enable = lib.mkDefault true;
    configurationLimit = lib.mkDefault 10;
  };

  fileSystems = lib.mapAttrs (_: subvol: lib.mkDefault (btrfs subvol)) subvolumes // {
    "/boot" = lib.mkDefault {
      device = "/dev/disk/by-label/PANDABOOT";
      fsType = "vfat";
      options = [
        "fmask=0077"
        "dmask=0077"
      ];
    };
  };
}
