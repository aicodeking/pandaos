# T3 Code's headless host (`t3 serve`): serves the T3 Code web UI on
# host:port (loopback by default) and, once signed in with
# `t3-connect sign-in`, joins the owner's T3 Code Connect network through a
# pinned cloudflared. `t3-connect start` runs it local-only (SSH tunnel or
# `t3-connect pair --tailscale`) without signing in.
#
# Like pandacode-host, the host and every agent it starts run as the dedicated,
# non-wheel system user `agent` (home /var/lib/agent), never as the owner.
#
# Sign-in runs headless as t3code-sign-in.service: the same user, environment
# and sandbox as the host. The device code goes to the journal:
# `journalctl -u t3code-sign-in`. Pairing tokens never do: `t3-connect pair`
# prints them on the operator's terminal and the host's output is redacted.
{ config, lib, pkgs, options, ... }:
let
  cfg = config.services.t3code-host;
  stateDir = "/var/lib/t3code-host";
  home = config.users.users.${cfg.user}.home;
  agentHome = "/var/lib/agent";
  # Written by `t3-connect sign-in` (root, 0644): `managed` or `publish-only`.
  signInModeFile = "${stateDir}/sign-in-mode";
  # Written by t3code-sign-in-done and `t3-connect stop` (root, 0644): the
  # signed-in mode (`connect`/`publish-only`) `t3-connect start` resumes.
  # Removed by `t3-connect unlink`/`sign-out`.
  lastModeFile = "${stateDir}/last-mode";
  cloudflared =
    if cfg.cloudflaredPackage == null then "" else "${cfg.cloudflaredPackage}/bin/cloudflared";
  # pandacode-host already creates the `agent` user and its home subvolume.
  pandacodeOwnsAgent =
    (config.services.pandacode-host.enable or false)
    && (config.services.pandacode-host.user or "") == "agent";
  ownAgent = cfg.user == "agent" && !pandacodeOwnsAgent;

  # pandacode-host's cgroup setup (oomd-avoided host/ leaf, per-task cgroups
  # under $PANDA_TASK_CGROUP_ROOT, empty task-* sweeper), then exec.
  enter = pkgs.writeShellApplication {
    name = "t3code-host-enter";
    runtimeInputs = [
      pkgs.coreutils
      pkgs.attr # setfattr
      pkgs.gnugrep
    ];
    text = builtins.readFile ../pandacode-host/cgroup-enter.sh;
  };

  # Keeps `Token: …`, `Pairing URL: …`, `?token=…` and JSON token fields out
  # of the journal (test-redact.sh).
  redact = pkgs.writeShellApplication {
    name = "t3code-host-redact";
    runtimeInputs = [ pkgs.gnused pkgs.coreutils ];
    text = builtins.readFile ./redact-run.sh;
  };

  t3-connect = pkgs.writeShellApplication {
    name = "t3-connect";
    runtimeInputs = [
      cfg.package
      pkgs.coreutils
      pkgs.gnused # sanitize_tty
      pkgs.curl
      pkgs.jq
      config.systemd.package
    ];
    runtimeEnv = {
      HOST_USER = cfg.user;
      HOST_HOME = home;
      T3CODE_HOME = "${home}/.t3";
      CLOUDFLARED_PATH = cloudflared;
      MARKER = cfg.enabledMarker;
      SIGN_IN_MODE_FILE = signInModeFile;
      LAST_MODE_FILE = lastModeFile;
      HOST = if cfg.host != null then cfg.host else "";
      PORT = toString cfg.port;
      LAN = if cfg.lan then "true" else "false";
      VERSION = lib.getVersion cfg.package;
      # PATH for `t3` under systemd-run (as_host_raw): the host unit's path,
      # cfg.package first so `t3` is always the configured package.
      HOST_PATH = "${lib.makeBinPath [ cfg.package pkgs.gitMinimal pkgs.openssh pkgs.procps pkgs.lsof pkgs.gnugrep ]}:/etc/profiles/per-user/${cfg.user}/bin:/run/current-system/sw/bin";
    };
    text = builtins.readFile ./t3-connect.sh;
  };

  # t3code-sign-in's main process: OAuth device code to the journal, then link.
  # Redacted like the host in case a token is printed; the device code and
  # verification URL pass through. A stopped (cancelled) sign-in keeps its
  # non-zero status so t3code-sign-in-done does not take it for success.
  signInRun = pkgs.writeShellScript "t3code-sign-in" ''
    mode=$(${pkgs.coreutils}/bin/head -n 1 ${signInModeFile} 2>/dev/null || true)
    case "$mode" in
      publish-only) set -- --publish-only ;;
      *) mode=managed; set -- ;;
    esac
    echo "t3code-sign-in: linking this box to T3 Connect ($mode); open the link below and enter the code"
    export T3CODE_HOST_REDACT_SIGNAL_OK=0
    exec ${lib.getExe redact} ${lib.getExe cfg.package} connect link --headless "$@"
  '';
  # Root, after the sign-in unit stops: on success record the mode and start
  # the host; on failure bring back a host that was enabled before.
  signInDone = pkgs.writeShellScript "t3code-sign-in-done" ''
    if [ "$EXIT_STATUS" = 0 ] && [ "$SERVICE_RESULT" = success ]; then
      mode=$(${pkgs.coreutils}/bin/head -n 1 ${signInModeFile} 2>/dev/null || true)
      case "$mode" in
        publish-only) ;;
        *) mode=connect ;;
      esac
      printf '%s\n' "$mode" >${cfg.enabledMarker}
      ${pkgs.coreutils}/bin/chmod 0644 ${cfg.enabledMarker}
      printf '%s\n' "$mode" >${lastModeFile}
      ${pkgs.coreutils}/bin/chmod 0644 ${lastModeFile}
      ${config.systemd.package}/bin/systemctl start --no-block t3code-host.service
      echo "t3code-sign-in: signed in ($mode); t3code-host.service started"
    elif [ -e ${cfg.enabledMarker} ]; then
      ${config.systemd.package}/bin/systemctl start --no-block t3code-host.service
      echo "t3code-sign-in: sign-in did not complete; t3code-host.service restarted as before"
    else
      echo "t3code-sign-in: sign-in did not complete"
    fi
  '';

  # The host's environment and sandbox, shared with t3code-sign-in.
  hostUnit = {
    # Agent CLIs the host starts find their tools through the user's PATH
    # (per-user profile, then system packages); T3 itself shells out to git,
    # ssh, ps, lsof and grep.
    path = [
      "/etc/profiles/per-user/${cfg.user}"
      "/run/current-system/sw"
      pkgs.gitMinimal
      pkgs.openssh
      pkgs.procps
      pkgs.lsof
      pkgs.gnugrep
    ];
    environment = {
      T3CODE_HOME = "${home}/.t3";
      T3CODE_PORT = toString cfg.port;
      T3CODE_LAN = if cfg.lan then "true" else "false";
      T3CODE_MODE = "web";
      T3CODE_NO_BROWSER = "true";
    } // lib.optionalAttrs (cfg.host != null) {
      T3CODE_HOST = cfg.host;
    } // lib.optionalAttrs (cfg.cloudflaredPackage != null) {
      # Also keeps `t3 connect link --headless` from offering a download.
      T3CODE_CLOUDFLARED_PATH = cloudflared;
    };
    # Same sandbox as pandacode-host (see its module.nix for what is left out
    # on purpose and why).
    serviceConfig = {
      User = cfg.user;
      WorkingDirectory = home;
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
in
{
  options.services.t3code-host = {
    enable = lib.mkEnableOption "the T3 Code Connect headless host (t3 serve)";
    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./package.nix { }";
      description = "T3 Code package (provides bin/t3).";
    };
    cloudflaredPackage = lib.mkOption {
      type = lib.types.nullOr lib.types.package;
      default = cfg.package.passthru.cloudflared;
      defaultText = lib.literalExpression "config.services.t3code-host.package.passthru.cloudflared";
      description = ''
        cloudflared for T3 Connect (T3CODE_CLOUDFLARED_PATH). null: T3 Code
        resolves or downloads its own, which headless sign-in cannot confirm.
      '';
    };
    user = lib.mkOption {
      type = lib.types.str;
      default = config.services.pandacode-host.user or "agent";
      defaultText = lib.literalExpression ''config.services.pandacode-host.user or "agent"'';
      description = ''
        User the host and every agent it starts run as. Its home holds T3
        Code's data dir (~/.t3), CLI logins and checkouts. The default `agent`
        is a system user (home ${agentHome}, not in wheel), shared with
        pandacode-host. Never use a wheel/login user.
      '';
    };
    host = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = if cfg.lan then null else "127.0.0.1";
      defaultText = lib.literalExpression ''if config.services.t3code-host.lan then null else "127.0.0.1"'';
      description = "Explicit address for `t3 serve`. A null value delegates address selection to T3 Code.";
    };
    port = lib.mkOption {
      type = lib.types.port;
      default = 3773;
      description = "Port `t3 serve` listens on.";
    };
    lan = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Let T3 Code expose itself on the LAN when `host` is null.";
    };
    openFirewall = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Open `port` (TCP) in the firewall. Only useful with a non-loopback `host`.";
    };
    serveCommand = lib.mkOption {
      type = lib.types.str;
      default = "${lib.getExe cfg.package} serve${lib.optionalString (cfg.host != null) " --host ${lib.escapeShellArg cfg.host}"} --port ${toString cfg.port} --no-browser";
      defaultText = lib.literalExpression ''"t3 serve [--host <host>] --port <port> --no-browser"'';
      internal = true;
      description = "What t3code-host-enter execs (through the redactor); tests swap in a stub host.";
    };
    enabledMarker = lib.mkOption {
      type = lib.types.str;
      default = "${stateDir}/enabled";
      readOnly = true;
      internal = true;
      description = "Exists while the host should run; holds its mode: connect, publish-only or local.";
    };
  };

  config = lib.mkIf cfg.enable (lib.mkMerge [
    {
      assertions = [
        {
          assertion = cfg.user != "root"
            && !(builtins.elem "wheel" (config.users.users.${cfg.user}.extraGroups or [ ]))
            && (config.users.users.${cfg.user}.group or "") != "wheel";
          message = "services.t3code-host.user must not be root or in wheel (agents run as this user)";
        }
      ];

      warnings =
        lib.optional
          (cfg.openFirewall
            && ((cfg.host == null && !cfg.lan)
              || (cfg.host != null
                && (lib.hasPrefix "127." cfg.host || builtins.elem cfg.host [ "::1" "localhost" ]))))
          "services.t3code-host.openFirewall has no effect while the host listens on loopback only; set lan = true (with host = null) or a non-loopback host."
        ++ lib.optional (cfg.lan && cfg.host != null)
          "services.t3code-host.lan has no effect while services.t3code-host.host is set; leave host = null for LAN binding.";

      environment.systemPackages = [ t3-connect cfg.package ];

      users.users = lib.mkIf ownAgent {
        agent = {
          isSystemUser = true;
          group = "agent";
          home = agentHome;
          # Made by tmpfiles below: a btrfs subvolume for snapshots.nix.
          createHome = false;
          shell = pkgs.bashInteractive;
          description = "PandaCode host and agent CLIs";
        };
      };
      users.groups = lib.mkIf ownAgent { agent = { }; };

      systemd.tmpfiles.rules = [ "d ${stateDir} 0755 root root - -" ]
        ++ lib.optional ownAgent "v ${agentHome} 0700 agent agent - -";

      networking.firewall.allowedTCPPorts = lib.mkIf cfg.openFirewall [ cfg.port ];

      systemd.services.t3code-host = {
        description = "T3 Code Connect headless host";
        # A rebuild must not kill running agent sessions. A new version or
        # unit setting applies on reboot or `t3-connect restart`.
        restartIfChanged = false;
        stopIfChanged = false;
        wantedBy = [ "multi-user.target" ];
        wants = [ "network-online.target" ];
        after = [ "network-online.target" ];
        inherit (hostUnit) path environment;
        unitConfig = {
          # Off: skipped (inactive, not failed). t3-connect start/sign-in
          # writes the marker and starts the unit.
          ConditionPathExists = cfg.enabledMarker;
          StartLimitIntervalSec = 0;
        };
        serviceConfig = hostUnit.serviceConfig // {
          # Restart= skips Condition*= checks; this one is not skipped.
          ExecCondition = "${pkgs.coreutils}/bin/test -e ${cfg.enabledMarker}";
          # Per-task cgroups under a delegated subtree, as in pandacode-host.
          Delegate = true;
          ExecStart = "${lib.getExe enter} ${lib.getExe redact} ${cfg.serveCommand}";
          # Stop signals only the redactor, which forwards TERM to `t3 serve`
          # and waits for it; the sed filters live on until it exits, so a
          # late write cannot SIGPIPE the host mid-shutdown.
          KillMode = "mixed";
          ManagedOOMSwap = "kill";
          # An OOM-killed task must not stop the unit.
          OOMPolicy = "continue";
          Restart = "on-failure";
          RestartSec = 5;
          RestartSteps = 6;
          RestartMaxDelaySec = "5min";
        };
      };

      # `t3-connect sign-in` starts it; also usable alone:
      #   echo managed >${signInModeFile}; systemctl start t3code-sign-in; journalctl -fu t3code-sign-in
      systemd.services.t3code-sign-in = {
        description = "T3 Code Connect sign-in (device code flow; the code is in the journal)";
        # A rebuild must not abort a pending device code.
        restartIfChanged = false;
        stopIfChanged = false;
        inherit (hostUnit) path environment;
        wants = [ "network-online.target" ];
        after = [ "network-online.target" ];
        # One T3 per data dir: starting this stops the host.
        conflicts = [ "t3code-host.service" ];
        serviceConfig = hostUnit.serviceConfig // {
          ExecStart = signInRun;
          ExecStopPost = "+${signInDone}";
          # As for the host: TERM to the redactor only, filters drain.
          KillMode = "mixed";
          # Device codes expire after 15 minutes.
          RuntimeMaxSec = "20min";
          SyslogIdentifier = "t3code-sign-in";
        };
      };
    }
    # The agent user reaches /mem through the panda-mem socket.
    (lib.optionalAttrs (options.services ? panda-mem) {
      services.panda-mem.clients = [ cfg.user ];
    })
  ]);
}
