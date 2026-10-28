# Daily unattended updates from the installed system's local flake, with a
# post-switch health check that rolls back on failure. Never reboots.
#
# The live ISO and VM tests have no /etc/pandaos, so the units are skipped at
# runtime by ConditionPathExists (not an eval-time check: the flake is written
# by the installer after this config is built).
{ config, lib, pkgs, ... }:
let
  cfg = config.pandaos.updates;
  flakeFile = "/etc/pandaos/flake.nix";
  before = "/run/panda-update/before";

  units =
    lib.optional config.services.panda-agentd.enable "panda-agentd.service"
    ++ lib.optional config.services.panda-mem.enable "panda-mem-init.service";

  healthCheck = pkgs.writeShellApplication {
    name = "panda-update-health";
    runtimeInputs = [
      pkgs.coreutils
      pkgs.gnugrep
      pkgs.iproute2
      config.systemd.package
      config.system.build.nixos-rebuild
    ];
    text = ''
      now=$(readlink -f /run/current-system)
      if [ "$(cat ${before} 2>/dev/null || true)" = "$now" ]; then
        echo "panda-update: system unchanged, nothing to check"
        exit 0
      fi

      healthy() {
        for u in ${lib.escapeShellArgs units}; do
          systemctl is-active --quiet "$u" || return 1
        done
        { ip -4 route show default; ip -6 route show default; } | grep -q .
      }

      # Give restarted services up to 90 s to settle.
      for _ in $(seq 18); do
        if healthy; then
          echo "panda-update: $now is healthy"
          exit 0
        fi
        sleep 5
      done

      echo "panda-update: health check failed after switching to $now, rolling back" >&2
      systemctl --no-pager --lines=20 status ${lib.escapeShellArgs units} >&2 || true
      ip route >&2 || true
      nixos-rebuild switch --rollback
      echo "panda-update: rolled back to $(readlink -f /run/current-system)" >&2
      exit 1
    '';
  };
in
{
  options.pandaos.updates.enable = lib.mkOption {
    type = lib.types.bool;
    default = true;
    description = "Daily auto-upgrade of the `pandaos` input of /etc/pandaos, with health-checked rollback.";
  };

  config = lib.mkIf cfg.enable {
    system.autoUpgrade = {
      enable = true;
      flake = "/etc/pandaos#pandaos";
      # Bump only the PandaOS input; nixpkgs follows whatever PandaOS pins.
      flags = [
        "--update-input"
        "pandaos"
      ];
      upgrade = false; # --upgrade only touches channels; we have none.
      dates = "daily";
      randomizedDelaySec = "1h";
      allowReboot = false;
    };

    systemd.timers.nixos-upgrade.unitConfig.ConditionPathExists = flakeFile;
    systemd.services.nixos-upgrade = {
      unitConfig.ConditionPathExists = flakeFile;
      serviceConfig.RuntimeDirectory = "panda-update";
      preStart = "${pkgs.coreutils}/bin/readlink -f /run/current-system > ${before}";
      postStart = lib.getExe healthCheck;
    };

    # Network-light: updates should fetch from the binary cache and fail fast
    # on bad links rather than hang or compile for hours.
    nix.settings = {
      connect-timeout = 5; # seconds per connection attempt (default: none)
      download-attempts = 3; # default 5; a dead mirror costs less retry time
      # Trust "path is in the cache" answers for 30 days: fewer narinfo
      # round trips on repeat evaluations.
      narinfo-cache-positive-ttl = 2592000;
      # Binary cache only: if a substitute download fails, fail the update
      # instead of building it locally.
      fallback = false;
    };
  };
}
