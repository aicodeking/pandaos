# boat operations: pending-reboot detection, scheduled maintenance reboots,
# backups. Imported by ./module.nix.
#
# Updates never restart boatd (persistent VMs), so a new kernel, boatd or
# Firecracker only takes effect on the next reboot. boat-reboot-check notices
# that, logs it, and exports boat_reboot_pending for node-exporter's textfile
# collector. A maintenance reboot drains by itself: stopping boatd pauses and
# snapshots every VM, and boatd resumes them after boot.
{ config, lib, pkgs, ... }:
let
  boat = config.services.boat;
  inherit (lib) mkOption types;
  mcfg = boat.maintenance;

  # Read-only snapshots for the backup: dataDir itself as .backup/top, and
  # every nested subvolume (each team's teams/t_<team> with btrfsQgroups) as
  # .backup/sub/<path>, since a btrfs snapshot does not include nested
  # subvolumes. Each team is crash-consistent to its own instant.
  snapScript = pkgs.writeShellScript "boat-backup-snap" ''
    set -euo pipefail
    export PATH=${lib.makeBinPath [ pkgs.btrfs-progs pkgs.coreutils pkgs.findutils ]}
    d=${boat.dataDir} b=${boat.dataDir}/.backup
    drop() {
      [ -d "$b" ] || return 0
      find "$b" -mindepth 1 -depth -inum 256 -type d -exec btrfs -q subvolume delete {} +
      rm -rf "$b"
    }
    drop
    [ "$1" = drop ] && exit 0
    mkdir -m 0700 "$b"
    btrfs subvolume snapshot -r "$d" "$b/top" >/dev/null
    find "$d" -mindepth 1 -maxdepth 4 \( -path "$b" -prune \) -o \( -inum 256 -type d -print0 \) |
      while IFS= read -r -d "" s; do
        rel=''${s#"$d"/}
        mkdir -p "$b/sub/$(dirname "$rel")"
        btrfs subvolume snapshot -r "$s" "$b/sub/$rel" >/dev/null
      done
  '';
  bcfg = boat.backup;
  metricsDir = "/var/lib/boat-metrics";
  startedWith = "/run/boat/started-with";

  # boatd's config (names the firecracker/jailer store paths) + its binary.
  # Written when boatd starts; compared with the installed one later.
  fingerprint = pkgs.writeShellScript "boat-fingerprint" ''
    cat /etc/boat/boatd.json
    echo
    echo "boatd=${lib.getExe' boat.boatdPackage "boatd"}"
  '';


  checkScript = pkgs.writeShellScript "boat-reboot-check" ''
    set -u
    export PATH=${lib.makeBinPath [ pkgs.coreutils pkgs.diffutils config.systemd.package ]}
    reasons=()
    for f in kernel initrd kernel-modules; do
      [ "$(readlink -f /run/booted-system/$f)" = "$(readlink -f /run/current-system/$f)" ] || reasons+=("$f")
    done
    # This unit is replaced on every switch, so the fingerprint script here is the installed one.
    if systemctl -q is-active boatd.service && [ -r ${startedWith} ] &&
      ! ${fingerprint} | cmp -s - ${startedWith}; then
      reasons+=(boatd-or-firecracker)
    fi
    mkdir -p ${metricsDir}
    {
      echo "# HELP boat_reboot_pending 1 if the running kernel/boatd/firecracker differ from the installed ones."
      echo "# TYPE boat_reboot_pending gauge"
      echo "boat_reboot_pending $(( ''${#reasons[@]} > 0 ))"
      for r in "''${reasons[@]}"; do echo "boat_reboot_pending_reason{reason=\"$r\"} 1"; done
    } >${metricsDir}/boat.prom.tmp
    mv ${metricsDir}/boat.prom.tmp ${metricsDir}/boat.prom
    if [ ''${#reasons[@]} -gt 0 ]; then
      echo "boat: reboot pending (changed: ''${reasons[*]}). VMs are snapshotted and resumed across a reboot." >&2
      mkdir -p /run/boat && echo "''${reasons[*]}" >/run/boat/reboot-pending
      exit 0
    fi
    rm -f /run/boat/reboot-pending
  '';

  maintScript = pkgs.writeShellScript "boat-maintenance-reboot" ''
    export PATH=${lib.makeBinPath [ pkgs.coreutils config.systemd.package ]}
    ${checkScript}
    if ${lib.boolToString mcfg.onlyWhenPending} && [ ! -e /run/boat/reboot-pending ]; then
      echo "boat-maintenance: nothing pending, not rebooting"
      exit 0
    fi
    echo "boat-maintenance: rebooting (pending: $(cat /run/boat/reboot-pending 2>/dev/null || echo forced)); boatd snapshots all VMs first"
    exec systemctl reboot
  '';
in
{
  options.services.boat = {
    maintenance = {
      reboot = lib.mkEnableOption "scheduled maintenance reboots (VMs are snapshotted and resumed)";
      onCalendar = mkOption {
        type = types.str;
        default = "Sun *-*-* 04:00:00";
        description = "systemd calendar time for the maintenance window.";
      };
      onlyWhenPending = mkOption {
        type = types.bool;
        default = true;
        description = "Reboot only when boat-reboot-check reports a pending kernel/boatd/firecracker change.";
      };
    };
    backup = {
      enable = lib.mkEnableOption "restic backups of crash-consistent btrfs snapshots of dataDir";
      repository = mkOption {
        type = types.str;
        example = "sftp:backup@backup.example.net:/srv/restic/boat";
        description = "restic repository (sftp:, s3:, rest:, b2:, a local path ...). The owner provides it.";
      };
      passwordFile = mkOption {
        type = types.str;
        example = "/var/lib/secrets/restic-boat.pass";
        description = "File with the restic repository password. Keep a copy off this machine.";
      };
      environmentFile = mkOption {
        type = types.nullOr types.str;
        default = null;
        description = "Credentials for the repository (e.g. AWS_ACCESS_KEY_ID=...).";
      };
      onCalendar = mkOption {
        type = types.str;
        default = "*-*-* 03:00:00";
      };
      pruneOpts = mkOption {
        type = types.listOf types.str;
        # Backups cannot forget one tenant: what a tenant deletes stays until the snapshots
        # holding it are pruned, so this is how long deleted tenant data can live on.
        default = [ "--keep-daily 7" "--keep-weekly 4" ];
        description = "restic retention. Longer retention also keeps deleted tenants' data that much longer; state it in your terms.";
      };
    };
  };

  config = lib.mkIf boat.enable (lib.mkMerge [
    {
      systemd.tmpfiles.rules = [ "d ${metricsDir} 0755 root root - -" ];

      # Record what boatd started with (config incl. the firecracker path).
      systemd.services.boatd.serviceConfig.ExecStartPost = [ "${pkgs.bash}/bin/sh -c '${fingerprint} >${startedWith}'" ];

      systemd.services.boat-reboot-check = {
        description = "boat: detect a pending reboot (kernel, boatd, firecracker)";
        after = [ "boatd.service" ];
        serviceConfig = {
          Type = "oneshot";
          ExecStart = checkScript;
        };
      };
      systemd.timers.boat-reboot-check = {
        wantedBy = [ "timers.target" ];
        timerConfig = {
          OnBootSec = "5min";
          OnUnitActiveSec = "1h";
        };
      };
    }

    (lib.mkIf mcfg.reboot {
      systemd.services.boat-maintenance-reboot = {
        description = "boat: maintenance reboot (drain = snapshot all VMs, reboot, resume)";
        serviceConfig = {
          Type = "oneshot";
          ExecStart = maintScript;
        };
      };
      systemd.timers.boat-maintenance-reboot = {
        wantedBy = [ "timers.target" ];
        timerConfig = {
          OnCalendar = mcfg.onCalendar;
          RandomizedDelaySec = "10min";
        };
      };
    })

    (lib.mkIf bcfg.enable {
      # restic reads a read-only snapshot, so VM disks and the API database are
      # consistent to one instant (crash-consistent; memory snapshots too).
      # Images are re-downloadable and left out.
      services.restic.backups.boat = {
        inherit (bcfg) repository passwordFile environmentFile pruneOpts;
        initialize = true;
        paths = [ "${boat.dataDir}/.backup" ];
        exclude = [
          "${boat.dataDir}/.backup/top/images"
          "${boat.dataDir}/.backup/top/export.key" # moved into secrets/ on the first start, but not out of a snapshot being taken
        ];
        timerConfig = {
          OnCalendar = bcfg.onCalendar;
          Persistent = true;
        };
        backupPrepareCommand = "${snapScript} take";
        backupCleanupCommand = "${snapScript} drop";
      };
      systemd.services.restic-backups-boat.unitConfig.RequiresMountsFor = boat.dataDir;
    })
  ]);
}
