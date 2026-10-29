# First boot, once per box, on a dedicated console (tty8):
# 1. Password: only on systems not made by panda-install (no
#    /etc/pandaos/local.nix) whose panda user has no usable hash yet; the
#    installer already set the real user's password (and may lock panda on
#    purpose). Asks `passwd panda` until it succeeds.
# 2. Which agent CLIs to install (panda-agents-install, needs a network).
#    PANDA_FIRSTBOOT_AGENTS (unit environment) answers it unattended; ""
#    skips it.
# 3. Offers `panda-connect sign-in` when a network is up.
# Then it writes /var/lib/pandaos/firstboot-done and never runs again.
# Killed half-way (a reboot), it starts over at the next boot; the password
# step skips itself once a hash exists.
#
# Why tty8 and not blocking getty on tty1: blocking would hold multi-user.target
# (and greetd on the desktop) until someone types at the console, which hangs
# headless VMs and tests. tty8 is outside logind's autovt range (1-6), so
# nothing else fights for it; getty/greetd keep running on tty1 untouched.
#
# Off by default on the live ISO: iso.nix autologins panda on the console
# there, and with no password panda cannot log in over ssh (key-only).
{
  config,
  lib,
  options,
  pkgs,
  ...
}:
let
  cfg = config.pandaos.firstboot;
  vt = "8";
  connect = config.services.pandacode-host.enable;
  agents = config.pandaos.agents;
  doneMarker = "/var/lib/pandaos/firstboot-done";
in
{
  # panda-agents-install and pandaos.agents.clis.
  imports = [ ../../components/agent-clis/module.nix ];

  options.pandaos.firstboot.enable = lib.mkOption {
    type = lib.types.bool;
    default = !(options ? isoImage);
    defaultText = lib.literalMD "`true`, except on the live ISO";
    description = "Run the first-boot console steps (password if unset, agent CLIs, PandaCode Connect) once.";
  };

  config = lib.mkIf (cfg.enable && config.users.mutableUsers) {
    systemd.services.panda-firstboot = {
      description = "PandaOS first boot: password, agent CLIs, PandaCode Connect";
      wantedBy = [ "multi-user.target" ];
      unitConfig.ConditionPathExists = "!${doneMarker}";
      # Start after the login managers so their VT switch to tty1 is done.
      after = [
        "systemd-user-sessions.service"
        "getty.target"
        "display-manager.service"
      ];
      path = [
        pkgs.coreutils
        pkgs.gnugrep
        pkgs.iproute2
        pkgs.kbd
        pkgs.shadow
      ];
      serviceConfig = {
        # simple, not oneshot: must never hold up multi-user.target.
        Type = "simple";
        StandardInput = "tty";
        StandardOutput = "tty";
        StandardError = "journal";
        TTYPath = "/dev/tty${vt}";
        TTYReset = true;
        TTYVHangup = true;
        TTYVTDisallocate = true;
      };
      script = ''
        # ponytail: fixed sleep to let greetd finish its own VT switch first.
        sleep 3
        chvt ${vt}
        echo
        echo "Welcome to PandaOS."
        # Boxes from panda-install already have their user's password.
        needs_password() {
          [ ! -e /etc/pandaos/local.nix ] || return 1
          case "$(grep '^panda:' /etc/shadow | cut -d: -f2)" in
            "" | "!" | "*" | "!*" | "!!") return 0 ;;
            *) return 1 ;;
          esac
        }
        if needs_password; then
          until
            echo "Set a password for the 'panda' user (it is also your sudo password)."
            passwd panda
          do
            echo "Password not set, try again."
            sleep 1
          done
        fi
        online() { [ -n "$(ip -4 route show default; ip -6 route show default)" ]; }
        install=${lib.getExe agents.installer}
        # Ctrl-C stops the install or sign-in, not this script. 2>&1 below:
        # stderr is the journal here, the console should see why it failed.
        trap echo INT
        echo
        if [ -n "''${PANDA_FIRSTBOOT_AGENTS+x}" ]; then
          clis=$PANDA_FIRSTBOOT_AGENTS
        else
          # Ask again until the names are valid or blank.
          while
            printf 'Which agent CLIs should this box run? [%s] (space-separated, blank to skip) ' \
              "${toString agents.installable}"
            read -r clis || clis=
            # shellcheck disable=SC2086 # one word per CLI
            [ -n "$clis" ] && ! "$install" --check $clis 2>&1
          do :; done
        fi
        if [ -z "$clis" ]; then
          echo "No agent CLIs. Add some later: sudo panda-agents-install codex"
        elif online; then
          # shellcheck disable=SC2086
          "$install" $clis 2>&1 ||
            echo "Not installed. Retry later: sudo panda-agents-install $clis"
        else
          echo "No network yet. When online, run: sudo panda-agents-install $clis"
        fi
        ${lib.optionalString connect ''
          echo
          if online; then
            # Not read -p: its prompt goes to stderr, which is the journal here.
            printf 'Connect this box to PandaCode now? [y/N] '
            read -r answer || answer=
            case "$answer" in
              [yY]*)
                /run/current-system/sw/bin/panda-connect sign-in ||
                  echo "Sign-in did not finish. Try again later: sudo panda-connect sign-in" ;;
              *) echo "Later: run 'sudo panda-connect sign-in', or use the desktop menu (PandaCode Connect)." ;;
            esac
          else
            echo "No network yet. To connect this box to PandaCode later, run"
            echo "'sudo panda-connect sign-in', or use the desktop menu (PandaCode Connect)."
          fi
        ''}
        mkdir -p ${dirOf doneMarker}
        touch ${doneMarker}
        echo "Done. Log in on the main console."
        sleep 2
        chvt 1
      '';
    };
  };
}
