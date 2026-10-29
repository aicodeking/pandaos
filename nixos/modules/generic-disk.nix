# The PandaOS disk layout, as created by panda-install (nixos/installer):
# GPT, a 512M ESP labelled PANDABOOT, and the rest as btrfs labelled PANDAOS
# with one subvolume per mount.
#
# Devices default to the labels, which is what the prebuilt system that
# panda-install copies to disk uses. The installer also writes
# /etc/pandaos/disk.nix, which overrides each `device` with the disk's UUID
# (mkForce), so from the first `nixos-rebuild` on, a second disk with the same
# labels can't be mounted instead. Only `device` is overridden there; fsType
# and options (including user_subvol_rm_allowed) stay defined here.
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
    device = lib.mkDefault "/dev/disk/by-label/PANDAOS";
    fsType = "btrfs";
    options = [
      "subvol=${subvol}"
      "compress=zstd:1"
      "noatime"
      # Lets panda-ckpt delete its snapshots as the owner. btrfs applies
      # mount options per filesystem, so every subvolume mount carries it.
      "user_subvol_rm_allowed"
    ];
  };
in
{
  boot.loader.systemd-boot = {
    enable = lib.mkDefault true;
    configurationLimit = lib.mkDefault 10;
  };

  fileSystems = lib.mapAttrs (_: btrfs) subvolumes // {
    "/boot" = {
      device = lib.mkDefault "/dev/disk/by-label/PANDABOOT";
      fsType = "vfat";
      options = [
        "fmask=0077"
        "dmask=0077"
      ];
    };
  };
}
