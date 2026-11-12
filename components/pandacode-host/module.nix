# PandaCode's headless host (`pandacode serve`): joins the owner's PandaCode
# Connect network, so the PandaCode app shows this box as a computer that can
# start, watch and approve tasks. Sign in once with `panda-connect sign-in`.
#
# The host and every agent it starts run as the dedicated, non-wheel system
# user `agent` (home /var/lib/agent), never as the owner: an agent that could
# write the owner's dotfiles could capture their sudo password.
#
# Sign-in runs headless as pandacode-sign-in.service: the same user, environment
# and sandbox as the host, so it reaches GitHub exactly as the host does, from a
# terminal or not (ssh without a tty, systemd-run, a script). The device code and
# every line PandaCode prints go to the journal: `journalctl -u pandacode-sign-in`.
{ config, lib, pkgs, options, ... }:
let
  cfg = config.services.pandacode-host;
  stateDir = "/var/lib/pandacode-host";
  home = config.users.users.${cfg.user}.home;
  # Mode 0644 files written by panda-connect (root). `signed-in` holds the
  # device name; `full-access` holds "on" or "off".
  fullAccessFile = "${stateDir}/full-access";
  notSignedIn = "This server is not signed in to PandaCode Connect";
  # Written by `panda-connect sign-in` (root, 0644): the device name to sign in as.
  signInName = "${stateDir}/sign-in-name";
  # PandaCode v0.3.75 prints "Signed in as <login> — this server is now hosting for your devices."
  # ponytail: matches PandaCode's message text; recheck on version bumps.
  signedInText = "now hosting for your devices";
  agentHome = "/var/lib/agent";

  enter = pkgs.writeShellApplication {
    name = "pandacode-host-enter";
    runtimeInputs = [
      pkgs.coreutils
      pkgs.attr # setfattr
      pkgs.gnugrep
    ];
    text = builtins.readFile ./cgroup-enter.sh;
  };

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
      SIGN_IN_NAME = signInName;
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
    rc=0
    ${lib.getExe pkgs.xvfb-run} -a ${lib.getExe cfg.package} serve "$flag" || rc=$?
    # serve exits but leaves WebKit helpers and Xvfb in the unit's cgroup (a
    # process-group kill misses them), so stop everything there but us.
    procs=/sys/fs/cgroup$(${pkgs.coreutils}/bin/cut -d: -f3 /proc/self/cgroup)/cgroup.procs
    for sig in TERM KILL; do
      while read -r p; do
        [ "$p" = $$ ] || kill -s "$sig" "$p" 2>/dev/null || true
      done <"$procs"
      ${pkgs.coreutils}/bin/sleep 1
    done
    exit "$rc"
  '';

  # pandacode-sign-in's main process: `serve --sign-in` prints the GitHub device
  # code, waits for approval, then keeps serving. Stop at the success line; the
  # unit's stop sweeps pandacode, Xvfb and WebKit helpers from its cgroup.
  signInRun = pkgs.writeShellScript "pandacode-sign-in" ''
    name=$(${pkgs.coreutils}/bin/head -n 1 ${signInName} 2>/dev/null || true)
    name=''${name:-${lib.escapeShellArg cfg.deviceName}}
    echo "pandacode-sign-in: signing in as device \"$name\"; open the GitHub link below and enter the code"
    while IFS= read -r line; do
      printf '%s\n' "$line"
      case "$line" in
        *${lib.escapeShellArg signedInText}*) exit 0 ;;
      esac
    done < <(${lib.getExe pkgs.xvfb-run} -a ${lib.getExe cfg.package} serve --sign-in --device-name "$name" 2>&1)
    echo "pandacode-sign-in: sign-in did not complete"
    # What this sandbox sees of GitHub, for the journal: DNS, TLS and HTTP in one line.
    ${lib.getExe pkgs.curl} -sS -o /dev/null --max-time 15 -X POST \
      -w 'pandacode-sign-in: curl https://github.com/login/device/code from here: HTTP %{http_code}, %{remote_ip}, %{time_total}s\n' \
      https://github.com/login/device/code 2>&1 | ${pkgs.gnused}/bin/sed 's/^curl:/pandacode-sign-in: curl:/' || true
    exit 1
  '';
  # Root, after the sign-in unit stops: on success record it and start the host.
  signInDone = pkgs.writeShellScript "pandacode-sign-in-done" ''
    if [ "$EXIT_STATUS" = 0 ] && [ "$SERVICE_RESULT" = success ]; then
      name=$(${pkgs.coreutils}/bin/head -n 1 ${signInName} 2>/dev/null || true)
      printf '%s\n' "''${name:-${cfg.deviceName}}" >${cfg.signedInMarker}
      ${config.systemd.package}/bin/systemctl start --no-block pandacode-host.service
      echo "pandacode-sign-in: signed in; pandacode-host.service started"
    fi
  '';

  # The host's environment and sandbox, shared with pandacode-sign-in.
  hostUnit = {
    # Agent CLIs the host starts find their tools through the user's PATH
    # (per-user profile, then system packages).
    path = [ "/etc/profiles/per-user/${cfg.user}" "/run/current-system/sw" ];
    environment = {
      # docs/headless.md: secrets in a 0600 file, not a Secret Service.
      PANDACODE_SECRETS_FILE = "1";
    };
    serviceConfig = {
      User = cfg.user;
      WorkingDirectory = home;
      # Agents need the user's home (checkouts, CLI logins), the network,
      # and exec of anything. appimageTools runs pandacode in a bubblewrap
      # FHS env, which needs user namespaces. Left out on purpose:
      # ProtectHome, RestrictNamespaces, PrivateUsers, CapabilityBoundingSet,
      # MemoryDenyWriteExecute (Node/JIT), SystemCallFilter, and
      # ProtectKernelTunables/ProtectKernelLogs/ProtectProc: their /proc
      # overmounts make the kernel refuse bwrap's fresh /proc mount.
      # ProtectControlGroups: /sys/fs/cgroup must stay writable for the
      # delegated subtree (only it is owned by the user).
      NoNewPrivileges = true; # no sudo from agents
      ProtectSystem = "strict";
      ReadWritePaths = [ home ];
      PrivateTmp = true;
      PrivateDevices = true;
      ProtectKernelModules = true;
      ProtectClock = true;
      ProtectHostname = true;
      RestrictRealtime = true;
      RestrictSUIDSGID = true;
      LockPersonality = true;
      SystemCallArchitectures = "native";
      RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" "AF_NETLINK" ];
    };
  };

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
      default = "agent";
      description = ''
        User the host and every agent CLI it starts run as. Its home holds the
        PandaCode data dir (sign-in, secrets.json), CLI logins and checkouts.
        The default `agent` is a system user created here (home ${agentHome},
        not in wheel). Never use a wheel/login user: agents could plant a
        fake `sudo` in its shell startup files.
      '';
    };
    serveCommand = lib.mkOption {
      type = lib.types.str;
      default = "${lib.getExe pkgs.xvfb-run} -a ${lib.getExe cfg.package} serve";
      defaultText = lib.literalExpression ''"xvfb-run -a pandacode serve"'';
      internal = true;
      description = "What pandacode-host-enter execs; tests swap in a stub host.";
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

  config = lib.mkIf cfg.enable (lib.mkMerge [
    {
      environment.systemPackages = [ panda-connect ];

      users.users = lib.mkIf (cfg.user == "agent") {
        agent = {
          isSystemUser = true;
          group = "agent";
          home = agentHome;
          # Made by tmpfiles below, not by the users activation: it must be
          # a btrfs subvolume so btrbk-agent can snapshot it (snapshots.nix).
          createHome = false;
          # For debugging only (`sudo -iu agent`); no password, no ssh keys.
          shell = pkgs.bashInteractive;
          description = "PandaCode host and agent CLIs";
        };
      };
      users.groups = lib.mkIf (cfg.user == "agent") { agent = { }; };

      # `v`: a subvolume when / is a btrfs subvolume (installed boxes), a
      # plain directory otherwise. Never converts an existing directory.
      systemd.tmpfiles.rules = [ "d ${stateDir} 0755 root root - -" ]
        ++ lib.optional (cfg.user == "agent") "v ${agentHome} 0700 agent agent - -";

      systemd.services.pandacode-host = {
        description = "PandaCode Connect headless host";
        # A rebuild must not kill running agent tasks. A new host version or
        # unit setting applies on reboot or `panda-connect restart`.
        restartIfChanged = false;
        stopIfChanged = false;
        wantedBy = [ "multi-user.target" ];
        wants = [ "network-online.target" ];
        after = [ "network-online.target" ];
        inherit (hostUnit) path environment;
        unitConfig = {
          # Not signed in: skipped (inactive, not failed). panda-connect sign-in
          # writes the marker and starts the unit.
          ConditionPathExists = cfg.signedInMarker;
          # Never give up; the backoff below keeps retries slow.
          StartLimitIntervalSec = 0;
        };
        serviceConfig = hostUnit.serviceConfig // {
          # Restart= skips Condition*= checks, so after forgetLostSignIn drops
          # the marker only this stops the next automatic restart.
          ExecCondition = "${pkgs.coreutils}/bin/test -e ${cfg.signedInMarker}";
          ExecStartPre = applyFullAccess;
          # Each agent task gets its own cgroup (see cgroup-enter.sh and the
          # agent CLI shims in agent-env, memory.oom.group=1, oom_score_adj
          # +500), so the kernel's cgroup OOM killer at agents.slice's
          # MemoryMax kills the offending task, not the host.
          Delegate = true;
          ExecStart = "${lib.getExe enter} ${cfg.serveCommand}";
          # oomd only on swap exhaustion. No ManagedOOMMemoryPressure: oomd
          # ranks by reclaim (pgscan) rate and killed an idle bystander task
          # in CI run 40876914383; the kernel picks by memory use.
          ManagedOOMSwap = "kill";
          # An OOM-killed task must not stop the unit (default OOMPolicy=stop).
          OOMPolicy = "continue";
          ExecStopPost = "+${forgetLostSignIn}";
          Restart = "on-failure";
          RestartSec = 5;
          RestartSteps = 6;
          RestartMaxDelaySec = "5min";
        };
      };

      # `panda-connect sign-in` starts it; also usable alone:
      #   echo NAME >${signInName}; systemctl start pandacode-sign-in; journalctl -fu pandacode-sign-in
      systemd.services.pandacode-sign-in = {
        description = "PandaCode Connect sign-in (GitHub device flow; the code is in the journal)";
        inherit (hostUnit) path environment;
        wants = [ "network-online.target" ];
        after = [ "network-online.target" ];
        # One PandaCode per data dir: starting this stops the host.
        conflicts = [ "pandacode-host.service" ];
        serviceConfig = hostUnit.serviceConfig // {
          ExecStart = signInRun;
          ExecStopPost = "+${signInDone}";
          # GitHub's device codes expire after 15 minutes.
          RuntimeMaxSec = "20min";
          SyslogIdentifier = "pandacode-sign-in";
        };
      };
    }
    # The agent user reaches /mem through the panda-mem socket.
    (lib.optionalAttrs (options.services ? panda-mem) {
      services.panda-mem.clients = [ cfg.user ];
    })
  ]);
}
