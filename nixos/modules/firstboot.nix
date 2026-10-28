# First boot: the panda user ships without a password (installed systems get
# a hashed one from the installer's local.nix). If it is still unset at boot,
# prompt for one with `passwd panda` on a dedicated console (tty8) and switch
# to it. Once a password exists the unit is skipped by ExecCondition.
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
in
{
  options.pandaos.firstboot.enable = lib.mkOption {
    type = lib.types.bool;
    default = !(options ? isoImage);
    defaultText = lib.literalMD "`true`, except on the live ISO";
    description = "Prompt for the panda user's password on the console when it has none.";
  };

  config = lib.mkIf (cfg.enable && config.users.mutableUsers) {
    systemd.services.panda-firstboot = {
      description = "PandaOS first boot: set the panda password";
      wantedBy = [ "multi-user.target" ];
      # Installed systems: panda-install sets the real user's password there
      # (and may lock panda on purpose when installing for another user).
      unitConfig.ConditionPathExists = "!/etc/pandaos/local.nix";
      # Start after the login managers so their VT switch to tty1 is done.
      after = [
        "systemd-user-sessions.service"
        "getty.target"
        "display-manager.service"
      ];
      path = [
        pkgs.coreutils
        pkgs.gnugrep
        pkgs.kbd
        pkgs.shadow
      ];
      serviceConfig = {
        # simple, not oneshot: must never hold up multi-user.target.
        Type = "simple";
        # Exit 0 (run) only while panda has no usable password hash.
        ExecCondition = pkgs.writeShellScript "panda-needs-password" ''
          hash=$(${pkgs.gnugrep}/bin/grep '^panda:' /etc/shadow | ${pkgs.coreutils}/bin/cut -d: -f2)
          case "$hash" in
            "" | "!" | "*" | "!*" | "!!") exit 0 ;;
            *) exit 1 ;;
          esac
        '';
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
        until
          echo
          echo "Welcome to PandaOS. Set a password for the 'panda' user."
          echo "(It is also your sudo password.)"
          passwd panda
        do
          echo "Password not set, try again."
          sleep 1
        done
        echo "Done. Log in as 'panda' on the main console."
        sleep 2
        chvt 1
      '';
    };
  };
}
