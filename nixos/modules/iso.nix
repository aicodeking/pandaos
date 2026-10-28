# ISO tweaks layered on nixpkgs' installation-cd-minimal.nix.
{ lib, ... }:
{
  isoImage = {
    squashfsCompression = "zstd -Xcompression-level 19";
    volumeID = "PANDAOS";
  };

  # The installer enables wpa_supplicant, which conflicts with NetworkManager.
  networking.wireless.enable = lib.mkForce false;
}
