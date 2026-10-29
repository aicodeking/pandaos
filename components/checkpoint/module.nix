{ config, lib, pkgs, ... }:

let
  cfg = config.services.panda-ckpt;
  home = u: config.users.users.${u}.home or "/home/${u}";
  group =
    u:
    let
      g = config.users.users.${u}.group or "";
    in
    if g == "" then "users" else g;
  unit = u: "panda-ckpt-auto-${u}";
  forUsers = f: lib.listToAttrs (map (u: lib.nameValuePair (unit u) (f u)) cfg.users);
in
{
  options.services.panda-ckpt = {
    enable = lib.mkEnableOption "panda-ckpt btrfs workspace checkpoints";
    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./package.nix { }";
      description = "panda-ckpt package.";
    };
    users = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ "panda" ];
      description = "Users that get ~/work (a btrfs subvolume), ~/.checkpoints and an auto-checkpoint timer.";
    };
    interval = lib.mkOption {
      type = lib.types.str;
      default = "10min";
      description = "How often changed workspaces are auto-checkpointed (systemd time span).";
    };
    keepAuto = lib.mkOption {
      type = lib.types.ints.positive;
      default = 24;
      description = "Auto checkpoints kept per workspace. Manual and pre-restore checkpoints are never pruned automatically.";
    };
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ cfg.package ];

    # Registry entry (agent-env): /etc/panda/mcp.d/panda-ckpt.json.
    pandaos.agents.mcpServers.panda-ckpt = {
      command = "panda-ckpt";
      args = [ "mcp" ];
    };

    # ~/work is made by panda-ckpt-init-<user> below: tmpfiles `v` makes a
    # subvolume only when / itself is one, not for a btrfs /home on another root. Deleting checkpoints as the owner
    # is fastest with the btrfs mount option user_subvol_rm_allowed (see
    # README.md); without it panda-ckpt empties the snapshot and rmdirs it.
    systemd.tmpfiles.rules = map (u: "d ${home u}/.checkpoints 0700 ${u} ${group u} - -") cfg.users;

    # ~/work as a btrfs subvolume (a plain directory elsewhere); never touches
    # an existing ~/work. Root only for this, then everything runs as the user.
    systemd.services = lib.listToAttrs (map (u: lib.nameValuePair "panda-ckpt-init-${u}" {
      description = "Create ~/work for panda-ckpt (${u})";
      wantedBy = [ "multi-user.target" ];
      before = [ "${unit u}.service" ];
      after = [ "local-fs.target" "systemd-tmpfiles-setup.service" ];
      path = [ pkgs.btrfs-progs pkgs.coreutils ];
      script = ''
        w=${lib.escapeShellArg "${home u}/work"}
        [ -d ${lib.escapeShellArg (home u)} ] && [ ! -e "$w" ] || exit 0
        if [ "$(stat -f -c %T ${lib.escapeShellArg (home u)})" = btrfs ]; then
          btrfs subvolume create "$w"
        else
          mkdir "$w"
        fi
        chown ${u}:${group u} "$w"
        chmod 0755 "$w"
      '';
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ProtectSystem = "strict";
        # "-": no failure if the home does not exist (the script then skips).
        ReadWritePaths = [ "-${home u}" ];
        PrivateNetwork = true;
      };
    }) cfg.users) // forUsers (u: {
      description = "panda-ckpt auto-checkpoints for ${u}";
      wants = [ "panda-ckpt-init-${u}.service" ];
      after = [ "local-fs.target" "systemd-tmpfiles-setup.service" "panda-ckpt-init-${u}.service" ];
      serviceConfig = {
        Type = "oneshot";
        User = u;
        ExecStart = "${lib.getExe cfg.package} auto --keep-auto ${toString cfg.keepAuto}";
        Nice = 10;
        IOSchedulingClass = "idle";
        NoNewPrivileges = true;
        CapabilityBoundingSet = "";
        ProtectSystem = "strict";
        ReadWritePaths = [ (home u) ];
        PrivateTmp = true;
        PrivateNetwork = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        SystemCallArchitectures = "native";
      };
    });

    systemd.timers = forUsers (u: {
      wantedBy = [ "timers.target" ];
      timerConfig = {
        OnBootSec = cfg.interval;
        OnUnitActiveSec = cfg.interval;
        Unit = "${unit u}.service";
      };
    });
  };
}
