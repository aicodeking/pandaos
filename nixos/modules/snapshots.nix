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
#
# The agent user's home (services.pandacode-host.user) is covered by @home
# when it lives under /home. The default user `agent` lives in /var/lib/agent,
# so a second instance, btrbk-agent, snapshots it with the same retention. btrfs
# can only snapshot subvolumes, so btrbk-agent is skipped (and says so in the
# journal) until that directory IS one; pandacode-host creates /var/lib/agent
# with tmpfiles `v` (a subvolume when / is one, as on installed boxes).
#
# Snapshots are not recursive, so nested subvolumes are separate entries:
# ~/work (made by panda-ckpt-init) and every workspace ~/work/<name>
# (btrbk glob; it skips read-only subvolumes, i.e. panda-ckpt's checkpoints,
# and only warns while there are no workspaces). Workspace snapshots go to
# /.snapshots/agent-workspaces/<name>.<time>, so a workspace named like
# another snapshot (mem, home) cannot mix up retention.
{ config, lib, pkgs, ... }:
let
  hostUser = lib.attrByPath [ "services" "pandacode-host" "user" ] "panda" config;
  # Not users.users.<user>.home: services.btrbk.instances defines
  # users.users.btrbk, so reading users.users here is infinite recursion.
  # pandacode-host makes the default `agent` user with this home (and other
  # host users live under /home, covered by the `home` snapshot).
  agentHome = if hostUser == "agent" then "/var/lib/agent" else null;
  agentRel = lib.removePrefix "/" agentHome;
  # panda-ckpt keeps the agent's workspaces in ~/work/<name>.
  ckptWork =
    (config.services.panda-ckpt.enable or false)
    && lib.elem hostUser (config.services.panda-ckpt.users or [ ]);
  workspaceDir = ".snapshots/agent-workspaces";

  retention = {
    # Run as root without sudo: execWheelOnly (security.nix) makes sudo
    # unusable for the non-wheel btrbk user.
    backend = "btrfs-progs";
    timestamp_format = "long"; # hourly names need the time, not just the date
    snapshot_preserve_min = "latest";
    snapshot_preserve = "24h 7d";
  };
  asRoot = {
    User = lib.mkForce "root";
    Group = lib.mkForce "root";
  };
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
    instances = {
      panda = {
        onCalendar = "hourly";
        settings = retention // {
          volume."/" = {
            snapshot_dir = ".snapshots";
            subvolume = {
              mem = { };
              home = { };
            };
          };
        };
      };
    }
    // lib.optionalAttrs (agentHome != null) {
      agent = {
        onCalendar = "hourly";
        settings = retention // {
          volume."/" = {
            snapshot_dir = ".snapshots";
            subvolume = {
              ${agentRel} = { };
            }
            // lib.optionalAttrs ckptWork {
              "${agentRel}/work".snapshot_name = "agent-work";
              "${agentRel}/work/*".snapshot_dir = workspaceDir;
            };
          };
        };
      };
    };
  };

  systemd.services = {
    btrbk-panda = {
      unitConfig.ConditionPathIsMountPoint = "/.snapshots";
      serviceConfig = asRoot;
    };
  }
  // lib.optionalAttrs (agentHome != null) {
    btrbk-agent = {
      unitConfig.ConditionPathIsMountPoint = "/.snapshots";
      serviceConfig = asRoot // {
        ExecCondition = pkgs.writeShellScript "btrbk-agent-is-subvolume" ''
          if ! ${pkgs.btrfs-progs}/bin/btrfs subvolume show ${agentHome} >/dev/null 2>&1; then
            echo "${agentHome} is not a btrfs subvolume; the agent home is NOT snapshotted" >&2
            exit 1
          fi
        '';
        # btrbk needs the snapshot_dir to exist.
        ExecStartPre = lib.optional ckptWork "${pkgs.coreutils}/bin/mkdir -p /${workspaceDir}";
      };
    };
  };

  environment.systemPackages = [ rollbackMem ];
}
