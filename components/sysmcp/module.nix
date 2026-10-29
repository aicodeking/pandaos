{ config, lib, pkgs, ... }:

let
  cfg = config.services.panda-sys;
in
{
  options.services.panda-sys = {
    enable = lib.mkEnableOption "panda-sys box status/ops MCP server for agents";
    allowMutations = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = ''
        Allow mutating tools (sys_updates action=run). They still need
        confirm:true and a caller in group panda-sys-update. Enabling this
        adds updateUsers to that group and a polkit rule that lets the group
        start nixos-upgrade.service, and nothing else.
      '';
    };
    updateUsers = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ "agent" ];
      description = "Users put in group panda-sys-update (only when allowMutations is set), i.e. allowed to start nixos-upgrade.service via sys_updates.";
    };
    connectivityUrl = lib.mkOption {
      type = lib.types.str;
      default = "https://cache.nixos.org/";
      description = "http(s) URL that sys_net sends a HEAD request to (5 s timeout).";
    };
    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./package.nix { }";
      description = "panda-sys package.";
    };
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ cfg.package ];

    users.groups.panda-sys-update.members = lib.mkIf cfg.allowMutations cfg.updateUsers;

    # sudo cannot work under NoNewPrivileges (agent sandboxes), so the upgrade
    # is started over D-Bus: `systemctl start --no-block nixos-upgrade.service`,
    # allowed for this group, this unit and the start verb only. Not wheel.
    security.polkit = lib.mkIf cfg.allowMutations {
      enable = true;
      extraConfig = ''
        polkit.addRule(function (action, subject) {
          if (action.id == "org.freedesktop.systemd1.manage-units" &&
              action.lookup("unit") == "nixos-upgrade.service" &&
              action.lookup("verb") == "start" &&
              subject.isInGroup("panda-sys-update")) {
            return polkit.Result.YES;
          }
        });
      '';
    };

    # Read by `panda-sys mcp` (root-owned, so agents cannot enable mutations).
    environment.etc."panda/sys.json".text = builtins.toJSON {
      inherit (cfg) allowMutations connectivityUrl;
    };

    # Registry entry (agent-env): /etc/panda/mcp.d/panda-sys.json.
    pandaos.agents.mcpServers.panda-sys = {
      command = "panda-sys";
      args = [ "mcp" ];
    };
  };
}
