# Daily unattended updates from the installed system's local flake, with a
# health check that rolls back on failure. Never reboots.
#
# Release channel: the installer writes /etc/pandaos/flake.nix with
#   inputs.pandaos.url = "github:AICodeKing/pandaos/release";
# plus a flake.lock pinned to the ISO's revision. The release workflow moves
# the `release` branch only after CI passes, so boxes never track main HEAD.
#
# Runtime conditions, not eval-time checks (the flake is written after this
# config is built; the live ISO and VM tests have none):
#   - /etc/pandaos/flake.nix must exist.
#   - /var/lib/pandaos/update-hold must NOT exist. `panda-update rollback`
#     (desktop menu) creates it; clear it with `sudo panda-update unhold`
#     (or `sudo rm /var/lib/pandaos/update-hold`).
{ config, lib, pkgs, ... }:
let
  cfg = config.pandaos.updates;
  flakeDir = "/etc/pandaos";
  holdFile = "/var/lib/pandaos/update-hold";
  rundir = "/run/panda-update";
  health = config.system.build.panda-health;

  # The switch runs inside this wrapper so the EXIT trap does the health check
  # and rollback whether the switch succeeded, failed half-way or was killed.
  runner = pkgs.writeShellApplication {
    name = "panda-update-run";
    runtimeInputs = [
      pkgs.coreutils
      pkgs.iproute2
      config.nix.package.out
      config.system.build.nixos-rebuild
      health
    ];
    text = ''
      lock=${flakeDir}/flake.lock
      before=$(readlink -f /run/current-system)
      # Only demand a default route afterwards if there was one before.
      health_args=(--wait 90)
      if [ -n "$(ip -4 route show default; ip -6 route show default)" ]; then
        health_args+=(--route)
      fi
      if [ -f "$lock" ]; then
        cp -p "$lock" ${rundir}/flake.lock
      fi
      switch_rc=1 # pessimistic until the switch returns 0

      restore_lock() {
        if [ -f ${rundir}/flake.lock ]; then
          cp -p ${rundir}/flake.lock "$lock"
        fi
      }

      finish() {
        rc=$?
        set +e
        trap - EXIT
        now=$(readlink -f /run/current-system)
        if [ "$now" = "$before" ]; then
          if [ "$switch_rc" -ne 0 ]; then
            echo "panda-update: update failed before activation (exit $rc); system unchanged" >&2
            restore_lock
          else
            echo "panda-update: system unchanged"
          fi
          exit "$rc"
        fi
        if [ "$switch_rc" -eq 0 ] && panda-health "''${health_args[@]}"; then
          echo "panda-update: $now is healthy"
          exit 0
        fi
        echo "panda-update: switch to $now failed (exit $switch_rc) or is unhealthy; rolling back to $before" >&2
        restore_lock
        nix-env -p /nix/var/nix/profiles/system --set "$before"
        "$before/bin/switch-to-configuration" switch
        echo "panda-update: rolled back to $(readlink -f /run/current-system)" >&2
        exit 1
      }
      trap finish EXIT
      trap 'exit 143' TERM INT

      ${cfg.switchCommand}
      switch_rc=0
    '';
  };

  panda-update = pkgs.writeShellApplication {
    name = "panda-update";
    runtimeInputs = [
      pkgs.coreutils
      config.systemd.package
      config.system.build.nixos-rebuild
    ];
    text = ''
      hold=${holdFile}
      need_root() {
        if [ "$(id -u)" -ne 0 ]; then
          echo "panda-update $1: run as root (sudo)" >&2
          exit 1
        fi
      }
      set_hold() {
        mkdir -p "$(dirname "$hold")"
        echo "held $(date -Is) by ''${SUDO_USER:-root}: $1" > "$hold"
      }

      case "''${1:-status}" in
        run)
          need_root run
          if [ -e "$hold" ]; then
            echo "Updates are on hold ($(cat "$hold")). Run 'panda-update unhold' first." >&2
            exit 1
          fi
          if [ ! -e ${flakeDir}/flake.nix ]; then
            echo "No ${flakeDir}/flake.nix: this is not an installed PandaOS system." >&2
            exit 1
          fi
          echo "Updating. Log: journalctl -u nixos-upgrade.service"
          if ! systemctl start nixos-upgrade.service; then
            echo "Update failed or was rolled back. See: journalctl -u nixos-upgrade.service" >&2
            exit 1
          fi
          echo "Update finished."
          ;;
        hold)
          need_root hold
          set_hold manual
          echo "Automatic updates are on hold. Resume with: panda-update unhold"
          ;;
        unhold)
          need_root unhold
          rm -f "$hold"
          echo "Automatic updates resumed."
          ;;
        rollback)
          need_root rollback
          set_hold rollback
          nixos-rebuild switch --rollback
          echo "Rolled back. Automatic updates are on hold until: panda-update unhold"
          ;;
        status)
          if [ -e "$hold" ]; then
            echo "updates: on hold ($(cat "$hold"))"
          else
            echo "updates: automatic"
          fi
          echo "system:  $(readlink -f /run/current-system)"
          systemctl show nixos-upgrade.service -p Result -p InactiveEnterTimestamp || true
          systemctl list-timers --no-pager nixos-upgrade.timer || true
          ;;
        *)
          echo "usage: panda-update run|hold|unhold|rollback|status" >&2
          exit 2
          ;;
      esac
    '';
  };
in
{
  options.pandaos.updates = {
    enable = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Daily auto-upgrade of the `pandaos` input of /etc/pandaos, with health-checked rollback.";
    };
    switchCommand = lib.mkOption {
      type = lib.types.str;
      internal = true;
      # Flags from system.autoUpgrade: --refresh, --flake ..., --update-input pandaos.
      default = "nixos-rebuild switch ${toString config.system.autoUpgrade.flags}";
      defaultText = lib.literalExpression ''"nixos-rebuild switch ''${toString config.system.autoUpgrade.flags}"'';
      description = ''
        Shell command that builds and activates the new system. VM tests
        override it to switch to a prebuilt generation offline; the health
        check and rollback around it still run.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    system.autoUpgrade = {
      enable = true;
      flake = "${flakeDir}#pandaos";
      # Bump only the PandaOS input; nixpkgs follows whatever PandaOS pins.
      # nixos-rebuild-ng (26.05) accepts --update-input and passes it to Nix
      # 2.34, which still honours it but marks it deprecated in favour of
      # `nix flake update pandaos --flake /etc/pandaos`.
      flags = [
        "--update-input"
        "pandaos"
      ];
      upgrade = false; # --upgrade only touches channels; we have none.
      dates = "daily";
      randomizedDelaySec = "1h";
      allowReboot = false;
    };

    systemd.timers.nixos-upgrade.unitConfig.ConditionPathExists = "${flakeDir}/flake.nix";
    systemd.services.nixos-upgrade = {
      unitConfig.ConditionPathExists = [
        "${flakeDir}/flake.nix"
        "!${holdFile}"
      ];
      serviceConfig.RuntimeDirectory = "panda-update";
      script = lib.mkForce "exec ${lib.getExe runner}";
    };

    environment.systemPackages = [ panda-update ];

    # Network-light: fail fast on bad links rather than hang.
    nix.settings = {
      connect-timeout = 5; # seconds per connection attempt (default: none)
      download-attempts = 3; # default 5; a dead mirror costs less retry time
      # Trust "path is in the cache" answers for 30 days: fewer narinfo
      # round trips on repeat evaluations.
      narinfo-cache-positive-ttl = 2592000;
      # If a substitute exists but its download fails, fail the update rather
      # than rebuild it locally. Honest caveat: paths that are in NO cache are
      # still built locally. Until pandaos.binaryCache (cache.nix) points at
      # a cache CI pushes to, that includes the PandaOS packages themselves
      # (agentd, mem, ...), so an update may compile them.
      fallback = false;
    };
  };
}
