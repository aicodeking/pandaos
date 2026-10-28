# Placeholder disk layout so nixosConfigurations evaluate. Real installs
# replace this with their generated hardware-configuration.nix.
{ lib, ... }:
{
  boot.loader.systemd-boot.enable = lib.mkDefault true;
  fileSystems."/" = lib.mkDefault {
    device = "/dev/disk/by-label/pandaos";
    fsType = "btrfs";
  };
  fileSystems."/boot" = lib.mkDefault {
    device = "/dev/disk/by-label/BOOT";
    fsType = "vfat";
  };
}
