# Extra settings for the generic, prebuilt system that panda-install copies
# from the ISO store to disk (nixosConfigurations.{headless,desktop}). It runs
# before any hardware-configuration.nix exists, so it boots on most machines.
# The first `nixos-rebuild --flake /etc/pandaos#pandaos` replaces it with the
# machine's own config (hardware-configuration.nix + local.nix, not this file).
{ lib, ... }:
{
  # The initrd storage/USB drivers from nixpkgs' all-hardware.nix...
  hardware.enableAllHardware = true;
  # ...but not linux-firmware, which is hundreds of MB on the ISO. Wired
  # network and VMs work without it. The local flake's hardware-configuration
  # imports not-detected.nix, which turns firmware back on at the first rebuild.
  hardware.enableRedistributableFirmware = lib.mkForce false;

  # An empty hostName leaves /etc/hostname unmanaged, so panda-install can
  # write the chosen name without a rebuild. local.nix sets it declaratively.
  networking.hostName = lib.mkOverride 900 "";
}
