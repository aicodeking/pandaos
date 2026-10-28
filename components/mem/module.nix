{ config, lib, pkgs, ... }:

let
  cfg = config.services.panda-mem;
  socket = "/run/panda-mem/sock";
in
{
  options.services.panda-mem = {
    enable = lib.mkEnableOption "panda-mem shared agent memory";
    dir = lib.mkOption {
      type = lib.types.str;
      default = "/mem";
      description = "Git repo holding the markdown memories. Owned and written only by the panda-mem service user.";
    };
    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./package.nix { }";
      description = "panda-mem package.";
    };
    clients = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "panda" "panda-agent" ];
      description = "Users added to the panda-mem group, i.e. allowed to use ${socket}. They get no write access to the repo itself.";
    };
  };

  config = lib.mkIf cfg.enable {
    users.groups.panda-mem.members = cfg.clients;
    users.users.panda-mem = {
      isSystemUser = true;
      group = "panda-mem";
      home = cfg.dir;
      description = "panda-mem memory store (sole writer)";
    };

    # Repo (incl. .git) belongs to the service user and is not group-writable,
    # so clients cannot plant hooks or config; they only reach it via the socket.
    systemd.tmpfiles.rules = [ "d ${cfg.dir} 0750 panda-mem panda-mem - -" ];

    systemd.services.panda-mem = {
      description = "panda-mem shared agent memory (single writer)";
      wantedBy = [ "multi-user.target" ];
      after = [ "systemd-tmpfiles-setup.service" ];
      serviceConfig = {
        User = "panda-mem";
        Group = "panda-mem";
        ExecStart = "${lib.getExe cfg.package} serve -dir ${cfg.dir} -socket ${socket}";
        Restart = "on-failure";
        RuntimeDirectory = "panda-mem";
        RuntimeDirectoryMode = "0750";
        UMask = "0022";
        NoNewPrivileges = true;
        CapabilityBoundingSet = "";
        ProtectSystem = "strict";
        ReadWritePaths = [ cfg.dir ];
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        PrivateNetwork = true;
        RestrictAddressFamilies = [ "AF_UNIX" ];
        RestrictSUIDSGID = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
      };
    };

    environment.systemPackages = [ cfg.package ];

    environment.etc."panda/mcp.json".text = builtins.toJSON {
      mcpServers.panda-mem = {
        command = "panda-mem";
        args = [ "mcp" "-socket" socket ];
      };
    };
  };
}
