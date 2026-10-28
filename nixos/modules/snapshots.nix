# Hourly btrfs snapshots of /mem and /home into /.snapshots (24 hourly +
# 7 daily kept), plus `panda-rollback-mem <snapshot>` to restore /mem.
#
# btrbk over snapper: btrbk is a oneshot run from a timer (0 idle RAM) and
# writes snapshots to any dir on the same filesystem. Its cost is Perl
# (~55 MB closure; nothing else in the base system needs it now).
# snapper wants a .snapshots inside each subvolume and a D-Bus daemon.
#
# Only runs when /.snapshots is a mountpoint, i.e. on installs whose disk
# layout has a snapshots subvolume (same btrfs filesystem as /mem and /home).
{ lib, pkgs, ... }:
let
  rollbackMem = pkgs.writeShellApplication {
    name = "panda-rollback-mem";
    runtimeInputs = [
      pkgs.btrfs-progs
      pkgs.coreutils
      pkgs.rsync
    ];
    text = ''
      snapdir=/.snapshots
      if [ $# -ne 1 ]; then
        echo "usage: panda-rollback-mem <snapshot>   (e.g. mem.20261028T1400)" >&2
        echo "available:" >&2
        for s in "$snapdir"/mem.*; do
          [ -e "$s" ] && echo "  ''${s##*/}" >&2
        done
        exit 2
      fi
      if [ "$(id -u)" -ne 0 ]; then
        echo "panda-rollback-mem: run as root (sudo)" >&2
        exit 1
      fi
      name=''${1##*/}
      if ! [[ "$name" =~ ^mem\.[0-9A-Za-z_]+$ ]] || [ ! -d "$snapdir/$name" ]; then
        echo "panda-rollback-mem: no such snapshot: $snapdir/$name" >&2
        exit 1
      fi
      # Keep the current state so the rollback itself can be undone.
      safety="$snapdir/mem-pre-rollback.$(date +%Y%m%dT%H%M%S)"
      btrfs subvolume snapshot -r /mem "$safety"
      rsync -aHAX --delete "$snapdir/$name/" /mem/
      echo "Restored /mem from $name. Previous state saved as $safety (delete it by hand)."
    '';
  };
in
{
  services.btrbk = {
    extraPackages = [ pkgs.btrfs-progs ];
    instances.panda = {
      onCalendar = "hourly";
      settings = {
        # Run as root without sudo: execWheelOnly (security.nix) makes sudo
        # unusable for the non-wheel btrbk user.
        backend = "btrfs-progs";
        timestamp_format = "long"; # hourly names need the time, not just the date
        snapshot_preserve_min = "latest";
        snapshot_preserve = "24h 7d";
        volume."/" = {
          snapshot_dir = ".snapshots";
          subvolume = {
            mem = { };
            home = { };
          };
        };
      };
    };
  };

  systemd.services.btrbk-panda = {
    unitConfig.ConditionPathIsMountPoint = "/.snapshots";
    serviceConfig = {
      User = lib.mkForce "root";
      Group = lib.mkForce "root";
    };
  };

  environment.systemPackages = [ rollbackMem ];
}
