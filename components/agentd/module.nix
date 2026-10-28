{ config, lib, pkgs, ... }:
let
  cfg = config.services.panda-agentd;
in
{
  options.services.panda-agentd = {
    enable = lib.mkEnableOption "panda-agentd, the PandaOS agent job daemon";
    relayUrl = lib.mkOption {
      type = lib.types.str;
      example = "https://relay.pandacode.example";
      description = "PandaConnect relay base URL (http or https).";
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
    # Jobs run as this user inside transient systemd units.
    users.users.panda-agent = {
      isSystemUser = true;
      group = "panda-agent";
      description = "PandaOS agent jobs";
    };
    users.groups.panda-agent = { };

    environment.systemPackages = [ cfg.package ];

    systemd.services.panda-agentd = {
      description = "PandaOS agent daemon";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      path = [ config.systemd.package ]; # systemd-run
      serviceConfig = {
        ExecStart = lib.escapeShellArgs [
          (lib.getExe cfg.package)
          "-relay" cfg.relayUrl
          "-slots" (toString cfg.slots)
          "-state-dir" "/var/lib/panda-agentd"
          "-run-dir" "/run/panda-agentd"
        ];
        Restart = "always";
        RestartSec = 5;
        StateDirectory = "panda-agentd";
        StateDirectoryMode = "0700";
        RuntimeDirectory = "panda-agentd";

        # Runs as root so systemd-run can start units as panda-agent. Jobs get
        # their own transient units, so this sandbox does not constrain them.
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
