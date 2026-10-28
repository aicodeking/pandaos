# panda-install, with its runtime tools pinned. `systems` maps a profile name
# to a prebuilt toplevel; they become part of this package's closure, so any
# ISO that ships panda-install can install them offline.
{
  lib,
  writeShellApplication,
  writeText,
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
  # Default for --profile (each ISO bakes its own).
  defaultProfile ? "headless",
  # flake.lock contents (an attrset, see flake.nix) written to /etc/pandaos, or null.
  flakeLock ? null,
  # Git revision of the source, written to /etc/pandaos/VERSION.
  revision ? "unknown",
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
    PANDA_DEFAULT_PROFILE = defaultProfile;
    PANDA_FLAKE_URL = flakeUrl;
    PANDA_FLAKE_LOCK = lib.optionalString (flakeLock != null) "${writeText "flake.lock" (builtins.toJSON flakeLock)}";
    PANDA_REVISION = revision;
  };
  text = builtins.readFile ./panda-install.sh;
  meta = {
    description = "Offline PandaOS installer";
    mainProgram = "panda-install";
    platforms = lib.platforms.linux;
  };
}
