# Extra settings for the generic, prebuilt system that panda-install copies
# from the ISO store to disk (nixosConfigurations.{headless,desktop}). It runs
# before any hardware-configuration.nix exists, so it boots on most machines.
# The first `nixos-rebuild --flake /etc/pandaos#pandaos` replaces it with the
# machine's own config (hardware-configuration.nix + local.nix, not this file).
{ lib, ... }:
{
  # The initrd storage/USB drivers from nixpkgs' all-hardware.nix...
  hardware.enableAllHardware = true;
  # ...but not all of linux-firmware (~1.9 GB on the ISO): firmware.nix, which
  # every installed system gets, ships the curated wifi/GPU/microcode subset.
  hardware.enableRedistributableFirmware = lib.mkForce false;

  # An empty hostName leaves /etc/hostname unmanaged, so panda-install can
  # write the chosen name without a rebuild. local.nix sets it declaratively.
  networking.hostName = lib.mkOverride 900 "";
}
