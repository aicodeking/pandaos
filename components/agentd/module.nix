{ config, lib, pkgs, ... }:
let
  cfg = config.services.panda-agentd;
  mem = config.services.panda-mem or { enable = false; };
in
{
  options.services.panda-agentd = {
    enable = lib.mkEnableOption "panda-agentd, the PandaOS agent job daemon";
    relayUrl = lib.mkOption {
      type = lib.types.str;
      example = "https://relay.pandacode.example";
      description = ''
        PandaConnect relay base URL. Must be https unless {option}`allowInsecure`
        is set, which permits http to loopback, private, `.local` or
        single-label hosts only.
      '';
    };
    allowInsecure = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Allow a plain-http relayUrl on a local/private host (VM tests, LAN dev relays).";
    };
    cpuQuota = lib.mkOption {
      type = lib.types.strMatching "[1-9][0-9]*%";
      default = "200%";
      description = "systemd CPUQuota= for each job unit (100% = one CPU).";
    };
    jobReadWritePaths = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = lib.optional mem.enable mem.dir;
      defaultText = lib.literalExpression ''lib.optional config.services.panda-mem.enable config.services.panda-mem.dir'';
      description = ''
        Extra paths job units may write (they run with ProtectSystem=strict).
        Paths that do not exist are skipped.
      '';
    };
    slots = lib.mkOption {
      type = lib.types.ints.positive;
      default = 2;
      description = "Maximum number of jobs run at once.";
    };
    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./package.nix { }";
      description = "panda-agentd package.";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.allowInsecure || lib.hasPrefix "https://" cfg.relayUrl;
        message = "services.panda-agentd.relayUrl must be https:// unless allowInsecure = true.";
      }
    ];

    # Jobs run as this user inside transient systemd units.
    users.users.panda-agent = {
      isSystemUser = true;
      group = "panda-agent";
      # Lets jobs use the shared memory repo.
      extraGroups = lib.optional mem.enable "panda-mem";
      description = "PandaOS agent jobs";
      # Jobs may use /mem via the panda-mem group. If panda-mem moves to a
      # socket service, adjust jobReadWritePaths; nothing else assumes /mem.
    };
    users.groups.panda-agent = { };

    environment.systemPackages = [ cfg.package ];

    systemd.services.panda-agentd = {
      description = "PandaOS agent daemon";
      # Never restart on `nixos-rebuild switch` / daily auto-upgrades: that
      # would cancel running jobs (job units are BindsTo= this service). A new
      # agentd takes effect on the next reboot or `systemctl restart panda-agentd`.
      restartIfChanged = false;
      stopIfChanged = false;
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      path = [ config.systemd.package ]; # systemd-run
      serviceConfig = {
        ExecStart = lib.escapeShellArgs (
          [
            (lib.getExe cfg.package)
            "-relay" cfg.relayUrl
            "-slots" (toString cfg.slots)
            "-cpu-quota" cfg.cpuQuota
            "-state-dir" "/var/lib/panda-agentd"
            "-run-dir" "/run/panda-agentd"
            # Written by `panda-install --relay URL`; overrides -relay without a rebuild.
            "-relay-file" "/var/lib/panda-agentd/relay"
          ]
          ++ lib.optional cfg.allowInsecure "-allow-insecure"
          ++ lib.concatMap (p: [ "-job-rw-path" p ]) cfg.jobReadWritePaths
        );
        Restart = "always";
        RestartSec = 5;
        # SIGTERM: 30s for running jobs, then up to 15s to report them cancelled.
        TimeoutStopSec = 60;
        StateDirectory = "panda-agentd";
        # 0711: the job user must traverse to jobs/<id>; the token file stays 0600.
        StateDirectoryMode = "0711";
        RuntimeDirectory = "panda-agentd";
        # 0755 so the dock can read status.json (0644, no secrets); the
        # pairing-code file in here is 0640 root:wheel.
        RuntimeDirectoryMode = "0755";
        ReadWritePaths = [ "/var/lib/panda-agentd" ];

        # Runs as root so systemd-run can start units as panda-agent. Jobs get
        # their own transient units, so this sandbox does not constrain them;
        # their sandbox is in runner.go (systemdArgs). Jobs may reach localhost
        # on purpose (no IPAddressDeny=localhost): they drive the browser's CDP.
        # Left out on purpose: PrivateUsers, CapabilityBoundingSet and tight
        # RestrictAddressFamilies, which would break talking to systemd.
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        ProtectClock = true;
        ProtectHostname = true;
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
        RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" "AF_NETLINK" ];
      };
    };
  };
}
