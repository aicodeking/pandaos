# Daily unattended updates from the installed system's local flake, with a
# health check that rolls back on failure. Never reboots (boat hosts: see
# services.boat.maintenance in components/boat/ops.nix).
#
# Release channel: the installer writes /etc/pandaos/flake.nix with
#   inputs.pandaos.url = "github:AICodeKing/pandaos/release";
# plus a flake.lock pinned to the ISO's revision. The release workflow moves
# the `release` branch only after CI passes, so boxes never track main HEAD.
#
# Trust: the GitHub branch alone is not trusted. Before switching,
# panda-update-verify (update-verify.sh) requires the new release commit to be
# SSH-signed by a key in pandaos.updates.allowedSigners, to descend from the
# running release, and to be the tree Nix fetched; otherwise it restores
# flake.lock and the box keeps running what it runs.
# OWNER ITEM: the owner must create the release-signing key, publish its
# public half in allowedSigners (the default below, or local.nix), and sign
# every commit the `release` branch points at with that key (`git commit -S`
# with gpg.format=ssh; a signed tag is not enough, the commit itself must
# carry the signature).
# Until then, every update is refused and a build-time warning says so.
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
  verifyState = "/var/lib/pandaos/update-verify";
  stagedFile = "/var/lib/pandaos/update-staged";
  signersFile =
    if cfg.allowedSignersFile != null then
      cfg.allowedSignersFile
    else
      pkgs.writeText "pandaos-allowed-signers" (lib.concatLines cfg.allowedSigners);
  noSigners = cfg.allowedSigners == [ ] && cfg.allowedSignersFile == null;

  verify = pkgs.writeShellApplication {
    name = "panda-update-verify";
    runtimeInputs = [
      pkgs.coreutils
      pkgs.gnugrep
      pkgs.jq
      pkgs.gitMinimal
      pkgs.openssh # ssh-keygen -Y verify, for git verify-commit
      config.nix.package.out
    ];
    text = builtins.readFile ./update-verify.sh;
  };
  # Bump the pandaos input in flake.lock, verified (or refused, lock restored).
  fetchVerified = lib.escapeShellArgs [
    (lib.getExe verify)
    flakeDir
    "pandaos"
    signersFile
    "/var/lib/pandaos/release.git"
    verifyState
  ];
  rebuildFlags = toString config.system.autoUpgrade.flags;

  # Staged mode: fetch, verify and build the next release without switching to
  # it; `panda-update run` applies (it fetches and verifies again).
  stager = pkgs.writeShellApplication {
    name = "panda-update-stage";
    runtimeInputs = [
      pkgs.coreutils
      pkgs.diffutils
      pkgs.jq
      pkgs.gitMinimal
      config.nix.package.out
      config.system.build.nixos-rebuild
    ];
    text = ''
      lock=${flakeDir}/flake.lock
      saved=$(mktemp)
      cp -p "$lock" "$saved"
      # flake.lock always goes back: it names what runs, not what is staged.
      trap 'cp -p "$saved" "$lock"; rm -f "$saved"' EXIT
      ${fetchVerified}
      if cmp -s "$saved" "$lock"; then
        echo "panda-update: nothing new to stage"
        exit 0
      fi
      rev=$(jq -r '.nodes.pandaos.locked.rev' "$lock")
      mkdir -p /var/lib/pandaos/staged
      cd /var/lib/pandaos/staged || exit 1
      nixos-rebuild build ${rebuildFlags}
      echo "$rev $(readlink -f result)" >${stagedFile}.tmp
      mv ${stagedFile}.tmp ${stagedFile}
      echo "panda-update: staged release $rev ($(readlink -f result)). Apply with: sudo panda-update run"
    '';
  };

  # The switch runs inside this wrapper so the EXIT trap does the health check
  # and rollback whether the switch succeeded, failed half-way or was killed.
  runner = pkgs.writeShellApplication {
    name = "panda-update-run";
    runtimeInputs = [
      pkgs.coreutils
      pkgs.iproute2
      pkgs.gnugrep
      config.systemd.package
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

      # The box is going down (we got TERM from the shutdown, or one is
      # queued): don't start a rollback that the shutdown would cut short.
      # The bootloader already points at the new generation, and boot.nix
      # tries it once and falls back to the last good entry if it fails.
      shutting_down() {
        [ "$(systemctl is-system-running 2>/dev/null || true)" = stopping ] \
          || [ -n "$(systemctl list-jobs --no-legend shutdown.target 2>/dev/null || true)" ]
      }

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
        # agentd has restartIfChanged=false so an update never kills running
        # jobs. When it is idle, restart it so the health check tests the new
        # agentd instead of the old process that is still running.
        if [ "$switch_rc" -eq 0 ] && ! grep -Eqs '"running": ?[1-9]' /run/panda-agentd/status.json; then
          systemctl restart panda-agentd.service || true
        fi
        if [ "$switch_rc" -eq 0 ] && panda-health "''${health_args[@]}"; then
          echo "panda-update: $now is healthy"
          rm -f ${stagedFile}
          # Boat hosts: refresh boat_reboot_pending now, not within the hour.
          systemctl start --no-block boat-reboot-check.service 2>/dev/null || true
          exit 0
        fi
        if shutting_down; then
          echo "panda-update: interrupted by shutdown while on $now; not rolling back (the next boot is a one-shot trial with fallback)" >&2
          exit "$rc"
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
            echo "updates: ${cfg.mode}"
          fi
          echo "system:  $(readlink -f /run/current-system)"
          echo "signed releases: ${
            if !cfg.requireSignedRelease then "NOT required"
            else if noSigners then "required, but no allowed signers are configured (every update is refused)"
            else "required (allowed signers: ${signersFile})"
          }"
          if [ -s ${verifyState} ]; then echo "last release check: $(cat ${verifyState})"; fi
          if [ -s ${stagedFile} ]; then echo "staged: $(cat ${stagedFile})"; fi
          if [ -s /run/boat/reboot-pending ]; then
            echo "reboot pending: $(cat /run/boat/reboot-pending) (boat maintenance window applies it)"
          fi
          systemctl show nixos-upgrade.service -p Result -p InactiveEnterTimestamp || true
          systemctl list-timers --no-pager nixos-upgrade.timer panda-update-stage.timer boat-maintenance-reboot.timer || true
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
    mode = lib.mkOption {
      type = lib.types.enum [ "automatic" "staged" "manual" ];
      default = "automatic";
      description = ''
        `automatic`: the daily timer fetches, verifies and switches.
        `staged`: the daily timer fetches, verifies and builds the next release
        but does not switch; `sudo panda-update run` applies it.
        `manual`: no timer; only `sudo panda-update run` updates.
        Hosts that set `services.boat.untrustedTenants = true` themselves (not only the server profile's default) default to `staged`.
      '';
    };
    requireSignedRelease = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Refuse a release unless its commit is SSH-signed by a key in
        {option}`pandaos.updates.allowedSigners`, descends from the running
        release, and is the tree Nix fetched. Turning this off trusts whoever
        can move the GitHub branch with root on this machine.
      '';
    };
    allowedSigners = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ ''release@pandaos namespaces="git" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...'' ];
      description = ''
        Lines of an ssh `allowed_signers` file (`git config gpg.ssh.allowedSignersFile`):
        `<principal> [options] <keytype> <key>`. Release commits must be signed by
        one of these keys. The owner provides the key and signs releases; while
        this is empty, every update is refused.
      '';
    };
    allowedSignersFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "An allowed_signers file to use instead of {option}`allowedSigners`.";
    };
    switchCommand = lib.mkOption {
      type = lib.types.str;
      internal = true;
      # Flags from system.autoUpgrade: --refresh, --flake ... (and --update-input
      # pandaos only when signatures are not required).
      # Separate lines, not `&&`: panda-update-run's errexit must stop on a
      # refused release (the left side of `&&` never trips errexit).
      default =
        lib.optionalString cfg.requireSignedRelease "${fetchVerified}\n"
        + "nixos-rebuild switch ${rebuildFlags}";
      defaultText = lib.literalExpression ''"panda-update-verify ... && nixos-rebuild switch ''${toString config.system.autoUpgrade.flags}"'';
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
      # With signatures required, panda-update-verify bumps and checks the lock
      # first, and nixos-rebuild builds exactly that lock.
      flags = lib.optionals (!cfg.requireSignedRelease) [
        "--update-input"
        "pandaos"
      ];
      upgrade = false; # --upgrade only touches channels; we have none.
      dates = "daily";
      randomizedDelaySec = "1h";
      allowReboot = false;
    };

    warnings = lib.optional (cfg.requireSignedRelease && noSigners) ''
      pandaos.updates.allowedSigners is empty: every update will be refused, because no release
      signature can be trusted. Add the release-signing public key (an ssh allowed_signers line).'';

    systemd.timers.nixos-upgrade = {
      unitConfig.ConditionPathExists = "${flakeDir}/flake.nix";
      wantedBy = lib.mkIf (cfg.mode != "automatic") (lib.mkForce [ ]);
    };

    systemd.services.panda-update-stage = {
      description = "PandaOS: fetch, verify and build the next release without switching";
      unitConfig.ConditionPathExists = [
        "${flakeDir}/flake.nix"
        "!${holdFile}"
      ];
      environment.HOME = "/root";
      serviceConfig = {
        Type = "oneshot";
        ExecStart = lib.getExe stager;
      };
    };
    systemd.timers.panda-update-stage = lib.mkIf (cfg.mode == "staged") {
      wantedBy = [ "timers.target" ];
      unitConfig.ConditionPathExists = "${flakeDir}/flake.nix";
      timerConfig = {
        OnCalendar = config.system.autoUpgrade.dates;
        RandomizedDelaySec = config.system.autoUpgrade.randomizedDelaySec;
        Persistent = true;
      };
    };
    systemd.services.nixos-upgrade = {
      unitConfig.ConditionPathExists = [
        "${flakeDir}/flake.nix"
        "!${holdFile}"
      ];
      serviceConfig = {
        RuntimeDirectory = "panda-update";
        # On `systemctl stop` (not shutdown) the trap still rolls back, which
        # can take a couple of minutes; don't let systemd SIGKILL it at 90 s.
        TimeoutStopSec = "5min";
      };
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
