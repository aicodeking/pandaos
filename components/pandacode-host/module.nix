# PandaCode's headless host (`pandacode serve`): joins the owner's PandaCode
# Connect network, so the PandaCode app shows this box as a computer that can
# start, watch and approve tasks. Sign in once with `panda-connect sign-in`.
{ config, lib, pkgs, ... }:
let
  cfg = config.services.pandacode-host;
  stateDir = "/var/lib/pandacode-host";
  home = config.users.users.${cfg.user}.home;
  # Mode 0644 files written by panda-connect (root). `signed-in` holds the
  # device name; `full-access` holds "on" or "off".
  fullAccessFile = "${stateDir}/full-access";
  notSignedIn = "This server is not signed in to PandaCode Connect";

  panda-connect = pkgs.writeShellApplication {
    name = "panda-connect";
    runtimeInputs = [
      cfg.package
      pkgs.xvfb-run
      pkgs.util-linux # runuser, setsid
      pkgs.coreutils
      pkgs.jq
      config.systemd.package
    ];
    runtimeEnv = {
      HOST_USER = cfg.user;
      HOST_HOME = home;
      DEFAULT_NAME = cfg.deviceName;
      MARKER = cfg.signedInMarker;
      FULL_ACCESS_FILE = fullAccessFile;
      VERSION = lib.getVersion cfg.package;
    };
    text = builtins.readFile ./panda-connect.sh;
  };

  # `--full-access` / `--no-full-access` save and exit. panda-connect's
  # choice overrides the option.
  applyFullAccess = pkgs.writeShellScript "pandacode-host-full-access" ''
    mode=${if cfg.fullAccess then "on" else "off"}
    if [ -r ${fullAccessFile} ]; then mode=$(${pkgs.coreutils}/bin/head -n 1 ${fullAccessFile}); fi
    case "$mode" in
      on) flag=--full-access ;;
      *) flag=--no-full-access ;;
    esac
    # serve exits but leaves WebKit helpers (and so Xvfb) running in the
    # unit's cgroup; its own process group lets us stop them all.
    ${pkgs.util-linux}/bin/setsid ${lib.getExe pkgs.xvfb-run} -a ${lib.getExe cfg.package} serve "$flag" &
    pid=$!
    rc=0
    wait "$pid" || rc=$?
    kill -TERM -- "-$pid" 2>/dev/null || true
    exit "$rc"
  '';

  # A sign-in revoked or lost outside panda-connect makes `serve` exit
  # non-zero with this message. Drop the marker so the unit stops being
  # started (ConditionPathExists) and panda-health ignores it, instead of a
  # restart loop that fails every update's health check.
  # ponytail: matches PandaCode's message text; recheck on version bumps.
  forgetLostSignIn = pkgs.writeShellScript "pandacode-host-check-sign-in" ''
    if ${config.systemd.package}/bin/journalctl -q -o cat _SYSTEMD_INVOCATION_ID="$INVOCATION_ID" \
        | ${pkgs.gnugrep}/bin/grep -qF ${lib.escapeShellArg notSignedIn}; then
      ${pkgs.coreutils}/bin/rm -f ${cfg.signedInMarker}
      echo "pandacode-host: not signed in any more; run: panda-connect sign-in"
    fi
  '';
in
{
  options.services.pandacode-host = {
    enable = lib.mkEnableOption "the PandaCode Connect headless host (pandacode serve)";
    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./package.nix { }";
      description = "PandaCode package (provides bin/pandacode).";
    };
    user = lib.mkOption {
      type = lib.types.str;
      default = "panda";
      description = ''
        User the host and every agent CLI it starts run as. Its home holds the
        PandaCode data dir (sign-in, secrets.json), CLI logins and checkouts.
      '';
    };
    deviceName = lib.mkOption {
      type = lib.types.str;
      default = config.networking.hostName;
      defaultText = lib.literalExpression "config.networking.hostName";
      description = "Default name shown to your devices; used by `panda-connect sign-in`.";
    };
    fullAccess = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = ''
        Let remote devices pick Full access (no approval prompts). Applied at
        every service start; `panda-connect full-access on|off` overrides it.
      '';
    };
    signedInMarker = lib.mkOption {
      type = lib.types.str;
      default = "${stateDir}/signed-in";
      readOnly = true;
      internal = true;
      description = "Exists once `panda-connect sign-in` succeeded; holds the device name.";
    };
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ panda-connect ];

    systemd.tmpfiles.rules = [ "d ${stateDir} 0755 root root - -" ];

    systemd.services.pandacode-host = {
      description = "PandaCode Connect headless host";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      # Agent CLIs the host starts find their tools through the user's PATH
      # (per-user profile, then system packages).
      path = [ "/etc/profiles/per-user/${cfg.user}" "/run/current-system/sw" ];
      environment = {
        # docs/headless.md: secrets in a 0600 file, not a Secret Service.
        PANDACODE_SECRETS_FILE = "1";
      };
      unitConfig = {
        # Not signed in: skipped (inactive, not failed). panda-connect sign-in
        # writes the marker and starts the unit.
        ConditionPathExists = cfg.signedInMarker;
        # Never give up; the backoff below keeps retries slow.
        StartLimitIntervalSec = 0;
      };
      serviceConfig = {
        # Restart= skips Condition*= checks, so after forgetLostSignIn drops
        # the marker only this stops the next automatic restart.
        ExecCondition = "${pkgs.coreutils}/bin/test -e ${cfg.signedInMarker}";
        User = cfg.user;
        WorkingDirectory = home;
        ExecStartPre = applyFullAccess;
        ExecStart = "${lib.getExe pkgs.xvfb-run} -a ${lib.getExe cfg.package} serve";
        ExecStopPost = "+${forgetLostSignIn}";
        Restart = "on-failure";
        RestartSec = 5;
        RestartSteps = 6;
        RestartMaxDelaySec = "5min";

        # Agents need the user's home (checkouts, CLI logins), the network,
        # and exec of anything. appimageTools runs pandacode in a bubblewrap
        # FHS env, which needs user namespaces. Left out on purpose:
        # ProtectHome, RestrictNamespaces, PrivateUsers, CapabilityBoundingSet,
        # MemoryDenyWriteExecute (Node/JIT), SystemCallFilter, and
        # ProtectKernelTunables/ProtectKernelLogs/ProtectProc: their /proc
        # overmounts make the kernel refuse bwrap's fresh /proc mount.
        NoNewPrivileges = true; # no sudo from agents
        ProtectSystem = "strict";
        ReadWritePaths = [ home ];
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        ProtectClock = true;
        ProtectHostname = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        SystemCallArchitectures = "native";
        RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" "AF_NETLINK" ];
      };
    };
  };
}
