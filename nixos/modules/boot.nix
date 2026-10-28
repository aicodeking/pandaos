# Boot assessment: a new generation that cannot boot falls back to the last
# generation that booted healthy, with no user action.
#
# nixos-26.05's systemd-boot module has no boot-counting option (entries are
# never named `+N-M`), so this does the same with systemd-boot's own EFI
# variables:
#   - LoaderEntryDefault (EFI var) = last known-good entry. Set only by
#     panda-boot-check after a healthy boot.
#   - Installing a new generation sets LoaderEntryOneShot to it, so it is
#     tried exactly once. If the kernel panics or the initrd fails, the
#     machine reboots (panic=10, boot.panic_on_fail) and the firmware picks
#     the known-good default again.
#   - If the trial boot comes up but fails the health check, panda-boot-check
#     reboots into the known-good entry after 1 min.
# The first boot after install has no known-good entry yet, so it just uses
# loader.conf and never auto-reboots. Ceiling: if more than
# configurationLimit generations are installed without a reboot, the
# known-good entry is garbage-collected and systemd-boot falls back to
# loader.conf's default (the newest generation).
#
# Also defines `panda-health`, the health check shared with updates.nix.
{ config, lib, pkgs, ... }:
let
  efiVars = "/sys/firmware/efi/efivars";
  loaderGuid = "4a67b082-0a4c-41cf-b6c7-440b29bb8c4f";
  esp = config.boot.loader.efi.efiSysMountPoint;

  units =
    lib.optional config.services.panda-agentd.enable "panda-agentd.service"
    ++ lib.optional config.services.panda-mem.enable "panda-mem.service"
    ++ lib.optional config.services.greetd.enable "greetd.service"
    ++ lib.optional config.networking.networkmanager.enable "NetworkManager.service";

  # panda-health [--route] [--wait SECONDS] [--settle SECONDS]
  # 0 when every core unit is active and stays up (no restarts) for --settle
  # seconds (default 10), and with --route a default route exists; retries
  # every 5 s for up to --wait seconds (default 0). The settle window catches
  # a unit that is "active" for an instant and then crash-loops.
  health = pkgs.writeShellApplication {
    name = "panda-health";
    runtimeInputs = [
      pkgs.coreutils
      pkgs.iproute2
      config.systemd.package
    ];
    text = ''
      route=no
      max_wait=0
      settle=10
      while [ $# -gt 0 ]; do
        case "$1" in
          --route) route=yes ;;
          --wait) max_wait=$2; shift ;;
          --settle) settle=$2; shift ;;
          *) echo "usage: panda-health [--route] [--wait SECONDS] [--settle SECONDS]" >&2; exit 2 ;;
        esac
        shift
      done

      units=(${lib.escapeShellArgs units})
      problems() {
        for u in "''${units[@]}"; do
          systemctl is-active --quiet "$u" || echo "$u is $(systemctl is-active "$u" || true)"
        done
        if [ "$route" = yes ] && [ -z "$(ip -4 route show default; ip -6 route show default)" ]; then
          echo "no default route"
        fi
      }

      restarts() {
        for u in "''${units[@]}"; do
          echo "$u $(systemctl show -P NRestarts "$u")"
        done
      }

      deadline=$(( $(date +%s) + max_wait ))
      while :; do
        p=$(problems)
        if [ -z "$p" ]; then
          before=$(restarts)
          sleep "$settle"
          p=$(problems)
          if [ -z "$p" ] && [ "$before" = "$(restarts)" ]; then
            echo "panda-health: ok"
            exit 0
          fi
          p=''${p:-"a core unit restarted within ''${settle}s"}
        fi
        [ "$(date +%s)" -ge "$deadline" ] && break
        sleep 5
      done
      echo "panda-health: FAILED" >&2
      echo "$p" >&2
      exit 1
    '';
  };

  bootCheck = pkgs.writeShellApplication {
    name = "panda-boot-check";
    runtimeInputs = [
      pkgs.coreutils
      config.systemd.package
      health
    ];
    text = ''
      efivar() {
        f=${efiVars}/$1-${loaderGuid}
        # 4-byte attribute header, then a UTF-16LE string.
        [ -r "$f" ] && tail -c +5 "$f" | tr -d '\0'
      }
      selected=$(efivar LoaderEntrySelected || true)
      good=$(efivar LoaderEntryDefault || true)

      if panda-health --wait 120; then
        if [ -n "$selected" ] && [ "$selected" != "$good" ]; then
          bootctl set-default "$selected"
          echo "panda-boot-check: $selected is now the known-good boot entry"
        fi
        exit 0
      fi

      if [ -n "$good" ] && [ "$selected" != "$good" ]; then
        echo "panda-boot-check: trial boot $selected is unhealthy, rebooting into $good" >&2
        shutdown -r +1 "PandaOS: this system update failed its boot check. Rebooting into the last good system in 1 minute."
      else
        echo "panda-boot-check: unhealthy, but no other known-good entry to fall back to" >&2
      fi
      exit 1
    '';
  };
in
{
  config = lib.mkMerge [
    {
      system.build.panda-health = health;
      environment.systemPackages = [ health ];
    }

    (lib.mkIf config.boot.loader.systemd-boot.enable {
      # Kernel panic, or initrd emergency mode (systemd initrd's built-in
      # panic-on-fail unit), reboots after 10 s instead of hanging. With a
      # dead disk every entry fails, so this becomes a reboot loop with the
      # error on screen for ~10 s each time.
      boot.kernelParams = [
        "panic=10"
        "boot.panic_on_fail"
      ];

      # Try each newly installed generation once (see the top of this file).
      # Only once a known-good default exists; best-effort, so installs
      # without EFI variable access (chroot, BIOS) still succeed.
      boot.loader.systemd-boot.extraInstallCommands = ''
        good_f=${efiVars}/LoaderEntryDefault-${loaderGuid}
        new=$(${pkgs.gnused}/bin/sed -n 's/^default //p' ${esp}/loader/loader.conf 2>/dev/null || true)
        if [ -r "$good_f" ] && [ -n "$new" ]; then
          good=$(${pkgs.coreutils}/bin/tail -c +5 "$good_f" | ${pkgs.coreutils}/bin/tr -d '\0')
          if [ "$new" != "$good" ]; then
            ${config.systemd.package}/bin/bootctl set-oneshot "$new" \
              && echo "panda: next boot tries $new once; $good stays the fallback" \
              || echo "panda: could not set a one-shot boot entry; $new boots by default" >&2
          fi
        fi
      '';

      systemd.services.panda-boot-check = {
        description = "PandaOS boot health check (marks the boot entry good)";
        wantedBy = [ "multi-user.target" ];
        requiredBy = [ "boot-complete.target" ];
        before = [ "boot-complete.target" "shutdown.target" ];
        # DefaultDependencies off so multi-user.target does not wait for us.
        after = [ "multi-user.target" ] ++ units;
        conflicts = [ "shutdown.target" ];
        unitConfig = {
          DefaultDependencies = false;
          # Only when booted by systemd-boot (not VM tests' direct kernel boot).
          ConditionPathExists = "${efiVars}/LoaderEntrySelected-${loaderGuid}";
        };
        serviceConfig = {
          Type = "oneshot";
          RemainAfterExit = true;
          ExecStart = lib.getExe bootCheck;
        };
      };
    })
  ];
}
