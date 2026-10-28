{ config, lib, pkgs, ... }:

let
  cfg = config.services.panda-mem;
in
{
  options.services.panda-mem = {
    enable = lib.mkEnableOption "panda-mem shared agent memory";
    dir = lib.mkOption {
      type = lib.types.str;
      default = "/mem";
      description = "Git repo holding the markdown memories.";
    };
    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./package.nix { }";
      description = "panda-mem package.";
    };
  };

  config = lib.mkIf cfg.enable {
    users.groups.panda-mem = { };

    systemd.tmpfiles.rules = [ "d ${cfg.dir} 2775 root panda-mem - -" ];

    systemd.services.panda-mem-init = {
      description = "Initialise the panda-mem git repo";
      wantedBy = [ "multi-user.target" ];
      after = [ "systemd-tmpfiles-setup.service" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        UMask = "0002";
        Group = "panda-mem";
        ExecStart = "${lib.getExe cfg.package} init -dir ${cfg.dir}";
        # Shared repo: let every panda-mem group member commit.
        ExecStartPost = "${lib.getExe pkgs.git} -C ${cfg.dir} config core.sharedRepository group";
      };
    };

    environment.systemPackages = [ cfg.package ];

    environment.etc."panda/mcp.json".text = builtins.toJSON {
      mcpServers.panda-mem = {
        command = "panda-mem";
        args = [ "mcp" "-dir" cfg.dir ];
      };
    };
  };
}
