# Agent CLIs that the PandaCode host (and you) start by name from PATH.
# Pick them declaratively with `pandaos.agents.clis`, or at runtime with
# `sudo panda-agents-install codex ...`, which writes /etc/pandaos/agents.nix
# and rebuilds (README.md has the local.nix contract).
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.pandaos.agents;
  # name -> nixpkgs 26.05 package; null: not packaged there.
  packages = {
    claude-code = pkgs.claude-code;
    codex = pkgs.codex;
    gemini-cli = pkgs.gemini-cli;
    opencode = pkgs.opencode;
    kimi = null;
  };
  # name -> binary on PATH (meta.mainProgram).
  binaries = {
    claude-code = "claude";
    codex = "codex";
    gemini-cli = "gemini";
    opencode = "opencode";
  };
  unfree = [ "claude-code" ];
  installable = lib.filter (n: packages.${n} != null) (lib.attrNames packages);
  unpackaged = lib.filter (n: packages.${n} == null) (lib.attrNames packages);
  selected = lib.filter (n: packages.${n} != null) cfg.clis;
  selectedUnfree = lib.intersectLists unfree cfg.clis;

  installer = pkgs.writeShellApplication {
    name = "panda-agents-install";
    runtimeInputs = [
      pkgs.coreutils
      pkgs.gnugrep
    ];
    runtimeEnv = {
      PANDA_AGENTS_KNOWN = toString (map (n: "${n}:${binaries.${n}}") installable);
      PANDA_AGENTS_UNPACKAGED = toString unpackaged;
      # Apply through panda-update (health check + rollback) when it exists.
      PANDA_AGENTS_UPDATE =
        if config.pandaos.updates.enable or false then
          "/run/current-system/sw/bin/panda-update run"
        else
          "${config.system.build.nixos-rebuild}/bin/nixos-rebuild switch --flake /etc/pandaos#pandaos";
    };
    text = builtins.readFile ./panda-agents-install.sh;
  };
in
{
  options.pandaos.agents = {
    clis = lib.mkOption {
      type = lib.types.listOf (lib.types.enum (lib.attrNames packages));
      default = [ ];
      example = [
        "claude-code"
        "codex"
      ];
      description = ''
        Agent CLIs to install system-wide. Unfree ones (claude-code) are
        allowed by name only, not via a global allowUnfree. At runtime,
        `sudo panda-agents-install <cli>...` sets this through
        /etc/pandaos/agents.nix and rebuilds.
      '';
    };
    installable = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = installable;
      readOnly = true;
      internal = true;
      description = "CLI names that have a package (what the first-boot picker offers).";
    };
    installer = lib.mkOption {
      type = lib.types.package;
      default = installer;
      readOnly = true;
      internal = true;
      description = "The panda-agents-install command.";
    };
  };

  config = {
    assertions = map (n: {
      assertion = !(lib.elem n cfg.clis);
      message = "pandaos.agents.clis: ${n} is not packaged in this nixpkgs (26.05); install it by hand for now.";
    }) unpackaged;

    nixpkgs.config = lib.mkIf (selectedUnfree != [ ]) {
      allowUnfreePredicate = pkg: lib.elem (lib.getName pkg) selectedUnfree;
    };

    # /run/current-system/sw/bin: on every user's PATH and on the
    # pandacode-host PATH, after the agent-env shims (which exec these).
    environment.systemPackages = [ installer ] ++ map (n: packages.${n}) selected;
  };
}
