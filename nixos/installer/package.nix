# panda-install, with its runtime tools pinned. `systems` maps a profile name
# to a prebuilt toplevel; they become part of this package's closure, so any
# ISO that ships panda-install can install them offline.
{
  lib,
  writeShellApplication,
  linkFarm,
  btrfs-progs,
  coreutils,
  dosfstools,
  gptfdisk,
  mkpasswd,
  nix,
  nixos-install-tools,
  systemd,
  util-linux,
  systems,
  flakeUrl,
}:
writeShellApplication {
  name = "panda-install";
  runtimeInputs = [
    btrfs-progs
    coreutils
    dosfstools
    gptfdisk
    mkpasswd
    nix
    nixos-install-tools
    systemd
    util-linux
  ];
  runtimeEnv = {
    PANDA_SYSTEMS = "${linkFarm "panda-install-systems" systems}";
    PANDA_FLAKE_URL = flakeUrl;
  };
  text = builtins.readFile ./panda-install.sh;
  meta = {
    description = "Offline PandaOS installer";
    mainProgram = "panda-install";
    platforms = lib.platforms.linux;
  };
}
