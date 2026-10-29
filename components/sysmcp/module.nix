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
      description = "Allow mutating tools (sys_updates action=run). They still need confirm:true and a root or wheel caller.";
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
