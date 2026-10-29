# Installed systems only (lib.mkPandaSystem: nixosConfigurations and every
# box's /etc/pandaos flake), not the live ISO, which keeps the installer
# tools for panda-install.
#
# Drop NixOS's installer tools (nixos-generate-config's perl env,
# nixos-install, nixos-enter, nixos-option, nixos-build-vms), but keep:
#   - nixos-rebuild: updates (panda-update-run), `panda-update rollback`,
#     the desktop's "Roll back system" and panda-agents-install's fallback;
#   - nixos-version: troubleshooting and support.
# python3 stays in the closure regardless: systemd-boot's installer
# (systemd-boot-builder.py) needs it on every bootloader install.
{ lib, ... }:
{
  system.disableInstallerTools = lib.mkDefault true;
  system.tools.nixos-rebuild.enable = true;
  system.tools.nixos-version.enable = true;
}
