# A safe, well-wired home for agents:
# - agents.slice caps what pandacode-host (and every agent CLI it starts)
#   may use. PATH shims for the agent CLIs (wrapCommands) give every agent
#   run its own task cgroup under the host's delegated cgroup, so the kernel's
#   cgroup OOM killer (at MemoryMax) kills the offending task, not the host
#   and the other tasks; systemd-oomd acts only on swap exhaustion. System
#   daemons get a low OOMScoreAdjust for the global OOM killer.
# - /etc/panda/mcp.d/<name>.json is the MCP registry; `panda-agents sync`
#   merges it into each agent CLI's config as panda-<name> and links the
#   system brief /etc/panda/AGENTS.md.
#
# No earlyoom: inside the slice, the kernel's cgroup OOM killer (MemoryMax)
# and oomd (swap) already act before the box thrashes, and they pick victims
# from agents.slice only. earlyoom watches the whole box and kills by RSS, so
# it would duplicate oomd and could pick a system daemon over an agent.
{ config, lib, pkgs, ... }:
let
  cfg = config.pandaos.agents;
  package = pkgs.callPackage ./package.nix { };
  hostEnabled = config.services.pandacode-host.enable or false;
  memEnabled = config.services.panda-mem.enable or false;
  ckptEnabled = config.services.panda-ckpt.enable or false;
  sysEnabled = config.services.panda-sys.enable or false;
  cu = config.services.panda-cu or { enable = false; };
  cuBrief = lib.optionalString cu.enable ''
    - Computer use: the `panda-cu` MCP tools (`cu_screenshot`, `cu_click`,
      `cu_type`, `cu_key`, `cu_tree`, `cu_find`, `cu_act`, `cu_wait`,
      `cu_launch`, `cu_open`, `cu_windows`, ...), or `panda-cu call TOOL '{json}'`.
      ${
        if cu.mode == "headless" then
          "They drive a private ${cu.resolution} desktop of your own."
        else
          "They drive the logged-in desktop session of the user who runs them."
      }
      Prefer `cu_find`/`cu_tree` + `cu_act` (the accessibility tree, no
      pixels) over pixel clicks. When you must click, take
      `cu_screenshot{marks:true}` and click a mark with `cu_click{element_id}`.
      Call `cu_wait` after anything that opens a window or dialog, and pass
      `screenshot:true` to an action to see its result in the same call.
  '';
  # Not -1000: sshd sessions inherit it, so keep them killable when huge.
  protect = -500;

  shims = pkgs.symlinkJoin {
    name = "panda-agent-shims";
    paths = map (
      name:
      pkgs.writeShellApplication {
        inherit name;
        # No runtimeInputs: they would be prepended to the agent's PATH.
        text = "name=${name}\n" + builtins.readFile ./shim.sh;
      }
    ) cfg.wrapCommands;
  };

  # mcp.d entry; env only when set, so plain entries stay {command, args}.
  entry = s: { inherit (s) command args; } // lib.optionalAttrs (s.env != { }) { inherit (s) env; };
  key = name: "panda-${lib.removePrefix "panda-" name}";
  registry = lib.mapAttrs' (
    name: s: lib.nameValuePair "panda/mcp.d/${name}.json" { text = builtins.toJSON (entry s); }
  ) cfg.mcpServers;
  # Compat: the pre-mcp.d /etc/panda/mcp.json (Claude-style), generated from the registry.
  mcpJson = builtins.toJSON {
    mcpServers = lib.mapAttrs' (name: s: lib.nameValuePair (key name) (entry s)) cfg.mcpServers;
  };

  agentsMd = pkgs.writeText "AGENTS.md" ''
    # PandaOS: notes for agents

    You are running on PandaOS, a NixOS box made for coding agents. You run as
    user `${cfg.user}`. The system is read-only to you: change it through
    `/etc/pandaos` (a Nix flake) and a rebuild, never by editing `/etc` or `/nix`.

    ## Tools
    - Shared memory: the `panda-mem` MCP server (`mem_search`, `mem_read`,
      `mem_list`, `mem_history`, `mem_write`, `mem_append`, `mem_delete`,
      `mem_log_episode`), or the `panda-mem` CLI. Every agent on the box shares
      `/mem`; search it before you start, write what you learn.
    - Checkpoints: ${
      if ckptEnabled then
        "keep each project in `~/work/<name>` (`panda-ckpt init <name>`, or\n      `panda-ckpt adopt` for an existing dir); then `panda-ckpt create/list/diff/\n      restore/fork`, or the `panda-ckpt` MCP tools. Changed workspaces are\n      auto-checkpointed."
      else
        "`panda-ckpt` is not enabled on this box."
    } `/mem` and `/home` also get hourly snapshots in `/.snapshots`.
    - Box status: ${
      if sysEnabled then "the `panda-sys` MCP tools (status, services, logs, top, net, ports, updates)." else "`systemctl`, `journalctl`."
    }
    ${cuBrief}- MCP servers on this box: ${lib.concatMapStringsSep ", " (n: "`${key n}`") (lib.attrNames cfg.mcpServers)}.
      Run `panda-agents doctor` to check your setup.

    ## Limits
    You share `agents.slice` with every other agent: memory is hard-capped
    at ${cfg.memoryMax} of RAM${lib.optionalString (cfg.memoryHigh != "infinity") " (throttled above ${cfg.memoryHigh})"}, at most
    ${toString cfg.tasksMax} tasks. Each agent run (${lib.concatStringsSep ", " cfg.wrapCommands})
    gets its own cgroup. At the cap the kernel's cgroup OOM killer kills the
    offending task (the one using the most memory), with all its child
    processes and without warning; systemd-oomd also kills a task when swap
    runs out. The host keeps running.
    Keep builds and test runs bounded (`-j`, worker counts) and stream large
    data instead of loading it.

    ## Rules
    - Checkpoint before risky changes (mass edits, migrations, deletes).
    - At the end of every task, call `mem_log_episode` (task, outcome
      success|partial|failure, a summary of what worked and what did not,
      files touched). Facts go in `semantic` memories, recipes in `procedural`.
    - Do not try to escape the limits or sudo; ask the owner instead.
    - Keep secrets out of memory, logs and commits.
  '';
in
{
  options.pandaos.agents = {
    enable = lib.mkEnableOption "agents.slice limits, OOM protection and MCP wiring for agent CLIs" // {
      default = true;
    };
    user = lib.mkOption {
      type = lib.types.str;
      default = config.services.pandacode-host.user or "agent";
      defaultText = lib.literalExpression ''config.services.pandacode-host.user or "agent"'';
      description = "User whose agent CLI configs `panda-agents-sync.service` keeps in sync.";
    };
    memoryHigh = lib.mkOption {
      type = lib.types.str;
      # Not a throttle by default: memory.high puts the allocating task to
      # sleep, so its reclaim (pgscan) rate drops to ~0 and systemd-oomd,
      # which kills the cgroup with the highest pgscan rate, picked an idle
      # bystander task instead of the hog (agent-env VM test, CI run
      # 40876914383). Uncapped up to memoryMax, the kernel's cgroup OOM
      # killer takes the biggest task (memory.oom.group: all of it).
      default = "infinity";
      description = "agents.slice MemoryHigh= (throttle point), bytes, % of RAM or infinity.";
    };
    memoryMax = lib.mkOption {
      type = lib.types.str;
      default = "75%";
      description = "agents.slice MemoryMax= (kernel OOM-kills inside the slice above this).";
    };
    memorySwapMax = lib.mkOption {
      type = lib.types.str;
      default = "10%";
      description = ''
        agents.slice MemorySwapMax=. Bounds how much an agent may push into
        (z)swap; zram memory is not charged to MemoryMax, and incompressible
        data costs zram about its full size, so the slice can hold up to
        memoryMax + memorySwapMax of physical RAM. Keep the sum well under
        100% (the defaults leave 15% for the system), or a hog triggers the
        global OOM killer instead of the slice's.
      '';
    };
    cpuWeight = lib.mkOption {
      type = lib.types.ints.between 1 10000;
      default = 50;
      description = "agents.slice CPUWeight= (system units default to 100, so agents yield under contention).";
    };
    tasksMax = lib.mkOption {
      type = lib.types.ints.positive;
      default = 8192;
      description = "agents.slice TasksMax= (fork-bomb guard).";
    };
    wrapCommands = lib.mkOption {
      type = lib.types.listOf (lib.types.strMatching "[A-Za-z0-9._-]+");
      default = [ "claude" "codex" "kimi" "opencode" "gemini" ];
      description = ''
        Agent CLIs shimmed first on pandacode-host's PATH: each run gets its
        own task cgroup, so an OOM kill takes that task, not the host.
      '';
    };
    mcpServers = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.submodule {
          options = {
            command = lib.mkOption { type = lib.types.str; };
            args = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [ ];
            };
            env = lib.mkOption {
              type = lib.types.attrsOf lib.types.str;
              default = { };
              description = "Environment for the server. World-readable in /etc: no secrets.";
            };
          };
        }
      );
      default = { };
      example = lib.literalExpression ''{ mem = { command = "panda-mem"; args = [ "mcp" ]; }; }'';
      description = ''
        MCP servers written to /etc/panda/mcp.d/<name>.json. Agent CLIs see
        them as `panda-<name>` (a leading `panda-` in the name is not doubled).
        Names: lowercase letters, digits, `-`, `_`.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ package ];
    environment.etc = registry // {
      "panda/AGENTS.md".source = agentsMd;
      "panda/mcp.json".text = mcpJson;
    };

    pandaos.agents.mcpServers.mem = lib.mkIf memEnabled {
      command = "panda-mem";
      args = [ "mcp" "-socket" "/run/panda-mem/sock" ];
    };

    # PandaCode may start agent CLIs through a login shell (`bash -lc`), which
    # rebuilds PATH from /etc/profile; put the shims first there too, for the
    # agent user only (set-environment runs this after PATH is set).
    environment.extraInit = lib.mkIf hostEnabled ''
      if [ "$(id -un 2>/dev/null)" = ${lib.escapeShellArg cfg.user} ]; then
        export PATH="${shims}/bin:$PATH"
      fi
    '';

    systemd.slices.agents = {
      description = "Agent CLIs (pandacode-host and everything it starts)";
      sliceConfig = {
        MemoryAccounting = true;
        MemoryHigh = cfg.memoryHigh;
        MemoryMax = cfg.memoryMax;
        MemorySwapMax = cfg.memorySwapMax;
        CPUWeight = cfg.cpuWeight;
        TasksMax = cfg.tasksMax;
        # No ManagedOOM* here: the kernel's cgroup OOM killer at MemoryMax
        # picks the task (components/pandacode-host); oomd watches only
        # pandacode-host.service's swap, so it picks a task cgroup inside it.
      };
    };
    # NixOS default, but this module relies on it.
    systemd.oomd.enable = true;

    systemd.services = lib.mkMerge [
      { systemd-journald.serviceConfig.OOMScoreAdjust = protect; }
      (lib.mkIf config.services.openssh.enable { sshd.serviceConfig.OOMScoreAdjust = protect; })
      (lib.mkIf config.networking.networkmanager.enable {
        NetworkManager.serviceConfig.OOMScoreAdjust = protect;
      })
      (lib.mkIf memEnabled { panda-mem.serviceConfig.OOMScoreAdjust = protect; })
      (lib.mkIf hostEnabled {
        pandacode-host = {
          wants = [ "panda-agents-sync.service" ];
          after = [ "panda-agents-sync.service" ];
          # Shims before the CLIs they wrap.
          path = lib.mkBefore [ shims ];
          serviceConfig = {
            Slice = "agents.slice";
            # Agents are the first pick for the global OOM killer too.
            OOMScoreAdjust = 500;
          };
        };
      })
      {
        # Re-syncs at boot and, through restartTriggers, whenever a rebuild
        # changes the registry or the brief. CLIs installed later: run
        # `panda-agents sync` (doctor says so).
        panda-agents-sync = {
          description = "Sync /etc/panda/mcp.d and AGENTS.md into agent CLI configs";
          wantedBy = [ "multi-user.target" ];
          restartTriggers = [ agentsMd mcpJson ];
          path = [ "/etc/profiles/per-user/${cfg.user}" "/run/current-system/sw" ];
          serviceConfig = {
            Type = "oneshot";
            RemainAfterExit = true;
            User = cfg.user;
            ExecStart = "${lib.getExe package} sync";
          };
        };
      }
    ];
  };
}
