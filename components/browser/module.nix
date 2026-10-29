{ config, lib, pkgs, ... }:

let
  cfg = config.services.panda-browser;
  runDir = "/run/panda-browser";
  args = [
    "--headless=new"
    "--remote-debugging-address=127.0.0.1"
    "--remote-debugging-port=${toString cfg.port}"
    "--user-data-dir=${runDir}/profile"
    "--disable-gpu"
    "--no-first-run"
    "--no-default-browser-check"
    "--disable-dev-shm-usage"
    "--disable-background-networking"
    "--disable-sync"
    "--disable-extensions"
    "--window-size=1280,800"
    "--force-device-scale-factor=1"
  ] ++ cfg.extraArgs ++ [ "about:blank" ];
in
{
  options.services.panda-browser = {
    enable = lib.mkEnableOption "panda-browser, a headless Chromium exposing CDP on localhost (adds ~500 MB to the closure)";
    port = lib.mkOption {
      type = lib.types.port;
      default = 9222;
      description = "Chrome DevTools Protocol port, bound to 127.0.0.1 only. Never opened in the firewall.";
    };
    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.chromium;
      defaultText = lib.literalExpression "pkgs.chromium";
      example = lib.literalExpression "pkgs.ungoogled-chromium";
      description = "Chromium package. `pkgs.ungoogled-chromium` also works.";
    };
    extraArgs = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "--lang=en-US" ];
      description = "Extra Chromium command-line flags.";
    };
    memoryMax = lib.mkOption {
      type = lib.types.str;
      default = "2G";
      description = "systemd MemoryMax for the browser.";
    };
    mcp = lib.mkOption {
      type = lib.types.nullOr (
        lib.types.submodule {
          options = {
            command = lib.mkOption { type = lib.types.str; };
            args = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [ ];
            };
          };
        }
      );
      default = null;
      example = lib.literalExpression ''{ command = "chrome-devtools-mcp"; args = [ "--browserUrl" "http://127.0.0.1:9222" ]; }'';
      description = ''
        An MCP server that drives this browser over CDP. When set, it is
        registered in /etc/panda/mcp.d/panda-browser.json for every agent CLI.
        None ships by default; agents get the CDP URL from /etc/panda/browser.json.
      '';
    };
    fonts = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Install a minimal font set (DejaVu) so pages render text.";
    };
  };

  config = lib.mkIf cfg.enable {
    fonts.packages = lib.mkIf cfg.fonts [ pkgs.dejavu_fonts ];

    systemd.services.panda-browser = {
      description = "Headless Chromium for agents (CDP on 127.0.0.1:${toString cfg.port})";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];
      environment = {
        HOME = runDir;
        XDG_CONFIG_HOME = "${runDir}/config";
        XDG_CACHE_HOME = "${runDir}/cache";
      };
      serviceConfig = {
        ExecStart = "${lib.getExe cfg.package} ${lib.escapeShellArgs args}";
        DynamicUser = true;
        # Removed on stop, so every start gets a fresh profile.
        RuntimeDirectory = "panda-browser";
        RuntimeDirectoryMode = "0700";
        Restart = "on-failure";
        RestartSec = 2;
        MemoryMax = cfg.memoryMax;

        # Hardening, limited to what Chromium's own userns sandbox tolerates.
        # Chromium keeps its sandbox (no --no-sandbox): it needs to create user,
        # pid and net namespaces, so RestrictNamespaces stays off and
        # PrivateUsers/CapabilityBoundingSet/SystemCallFilter are left unset.
        # MemoryDenyWriteExecute would break the V8 JIT.
        RestrictNamespaces = false;
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
        LockPersonality = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" "AF_NETLINK" ];
        UMask = "0077";
      };
    };

    pandaos.agents.mcpServers.panda-browser = lib.mkIf (cfg.mcp != null) cfg.mcp;

    environment.etc."panda/browser.json".text = builtins.toJSON {
      cdp = "http://127.0.0.1:${toString cfg.port}";
    };
  };
}
