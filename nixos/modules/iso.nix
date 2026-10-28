# Live ISO: PandaOS itself (base + profile) plus panda-install, built on
# nixpkgs' bare iso-image.nix. installation-cd-minimal.nix is not used: its
# installer profile, docs and linux-firmware made the ISO about 1.4 GB.
{
  config,
  lib,
  pkgs,
  modulesPath,
  ...
}:
let
  cfg = config.pandaos.installer;
  panda-install = pkgs.callPackage ../installer/package.nix {
    inherit (cfg) systems flakeUrl;
    nix = config.nix.package;
  };
in
{
  imports = [ "${modulesPath}/installer/cd-dvd/iso-image.nix" ];

  options.pandaos.installer = {
    systems = lib.mkOption {
      type = lib.types.attrsOf lib.types.package;
      description = "Prebuilt system toplevels, by profile name, that panda-install copies to disk offline.";
    };
    flakeUrl = lib.mkOption {
      type = lib.types.str;
      default = "github:AICodeKing/pandaos";
      description = "The pandaos flake that the installed /etc/pandaos flake tracks.";
    };
  };

  config = {
    isoImage = {
      squashfsCompression = "zstd -Xcompression-level 19";
      # Not "PANDAOS": that is the installed btrfs label, and the live initrd
      # finds its root by label.
      volumeID = "PANDAOS-LIVE";
      makeEfiBootable = true;
      makeUsbBootable = true;
      # Already in panda-install's closure; listed so the intent is explicit.
      storeContents = lib.attrValues cfg.systems;
    };

    # Boot on most machines, but without linux-firmware (hundreds of MB).
    hardware.enableAllHardware = true;
    hardware.enableRedistributableFirmware = lib.mkForce false;

    # mkpasswd: for scripted installs (`--password-hash "$(mkpasswd -m yescrypt)"`).
    environment.systemPackages = [
      panda-install
      pkgs.mkpasswd
    ];

    # Live session: log straight in as panda, root via sudo with no password.
    services.getty.autologinUser = "panda";
    services.getty.helpLine = lib.mkForce ''
      Install PandaOS to disk (no network needed):  sudo panda-install
    '';
    security.sudo.wheelNeedsPassword = lib.mkForce false;
    # Desktop ISO: greetd owns tty1, so start the session for panda directly.
    services.greetd.settings.initial_session = lib.mkIf config.services.greetd.enable {
      command = "labwc";
      user = "panda";
    };
  };
}
