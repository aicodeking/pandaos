# Panda shell: labwc session, waybar top bar + dock, wallpaper, lock/idle,
# notifications and repair scripts. Configs go under /etc/xdg so per-user
# configs still override them.
{ config, lib, pkgs, ... }:
let
  cfg = config.pandaos.desktop;

  footExe = "${pkgs.foot}/bin/foot";
  inTerm = cmd: "${footExe} --title 'Panda: ${cmd}' ${cmd}";

  # No Xwayland: drops Xwayland and its X/GL libs from labwc's closure.
  # wlroots reads enableXWayland through finalAttrs, so overrideAttrs is enough.
  wlroots = pkgs.wlroots_0_19.overrideAttrs { enableXWayland = false; };
  labwc = (pkgs.labwc.override { wlroots_0_19 = wlroots; }).overrideAttrs (old: {
    buildInputs = lib.filter (p: (p.pname or "") != "xwayland") old.buildInputs;
    mesonFlags = [ (lib.mesonEnable "xwayland" false) ];
  });

  # No icon themes ship, so fuzzel's SVG icon support (resvg) is dead weight.
  fuzzel = pkgs.fuzzel.override { svgSupport = false; };

  # Waybar's default build pulls cava (fftw, SDL, portaudio), mpd, jack,
  # sndio, tray (dbusmenu) and compositor-specific IPC that labwc never uses.
  # intersectAttrs drops any flag this nixpkgs' waybar doesn't know, so a
  # renamed flag costs a little closure instead of an eval error.
  waybar = pkgs.waybar.override (
    lib.intersectAttrs (lib.functionArgs pkgs.waybar.override) {
      cavaSupport = false;
      enableManpages = false;
      evdevSupport = false;
      gpsSupport = false;
      hyprlandSupport = false;
      inputSupport = false;
      jackSupport = false;
      mpdSupport = false;
      mprisSupport = false;
      niriSupport = false;
      pipewireSupport = false;
      sndioSupport = false;
      swaySupport = false;
      traySupport = false;
      upowerSupport = false;
      withMediaPlayer = false;
      wireplumberSupport = false;
    }
  );

  # Without hardware.graphics there is no Mesa driver, so wlroots must use
  # its software renderer; foot, fuzzel and waybar (cairo) never need GL.
  panda-session = pkgs.writeShellApplication {
    name = "panda-session";
    text = ''
      ${lib.optionalString (!cfg.gpu) "export WLR_RENDERER=pixman"}
      exec ${labwc}/bin/labwc "$@"
    '';
  };

  # swaylock's librsvg/cairo/gdk-pixbuf are already in labwc's closure.
  # -f forks once locked, so swayidle -w holds sleep until the lock is up.
  # A second instance while locked would just fail, so skip it.
  panda-lock = pkgs.writeShellApplication {
    name = "panda-lock";
    runtimeInputs = [ pkgs.swaylock pkgs.procps ];
    text = ''
      pgrep -x swaylock >/dev/null && exit 0
      exec swaylock -f -c 14161b --inside-color 1d2027ff --ring-color 5ec39aff \
        --key-hl-color e6e6e6ff --line-color 00000000 --separator-color 00000000
    '';
  };

  # Do not disturb (fnottctl pause/unpause), for agents, Super+N and the menu.
  panda-dnd = pkgs.writeShellApplication {
    name = "panda-dnd";
    runtimeInputs = [ pkgs.fnott pkgs.procps ];
    text = builtins.readFile ./bin/panda-dnd;
  };

  # Marks a computer-use task as running; the idle lock skips while busy.
  panda-cu-busy = pkgs.writeShellApplication {
    name = "panda-cu-busy";
    runtimeInputs = [ pkgs.coreutils ];
    text = builtins.readFile ./bin/panda-cu-busy;
  };

  panda-agents-status = pkgs.writeShellApplication {
    name = "panda-agents-status";
    runtimeInputs = [ pkgs.jq pkgs.coreutils ];
    text = builtins.readFile ./bin/panda-agents-status;
  };

  panda-connect-status = pkgs.writeShellApplication {
    name = "panda-connect-status";
    runtimeInputs = [ pkgs.jq pkgs.foot ];
    text = builtins.readFile ./bin/panda-connect-status;
  };

  # panda-mem, panda-agentd, panda-connect, panda-update, panda-rollback-mem and
  # nixos-rebuild come from the system PATH: they belong to the running
  # system generation, not to this script.
  panda-menu = pkgs.writeShellApplication {
    name = "panda-menu";
    runtimeInputs = [
      pkgs.foot
      fuzzel
      pkgs.jq
      pkgs.less
      pkgs.ncdu
      pkgs.findutils
      pkgs.util-linux # lsblk
      pkgs.coreutils # df
      config.networking.networkmanager.package # nmtui; the configured NM (no ModemManager)
      panda-lock
      panda-dnd
    ];
    text = builtins.readFile ./bin/panda-menu;
  };

  # Rendered at build time; the PNG has no runtime references (librsvg is
  # build-only), and swaybg loads PNG via gdk-pixbuf, already in waybar's closure.
  wallpaper = pkgs.runCommand "panda-wallpaper.png" { nativeBuildInputs = [ pkgs.librsvg ]; } ''
    rsvg-convert ${./wallpaper.svg} -o $out
  '';

  lockCmd = "${panda-lock}/bin/panda-lock";
  # ponytail: a lock skipped while busy is not retried when the task ends; the
  # next idle period locks. Add a resume/retry loop if that gap matters.
  idleLockCmd = "${panda-cu-busy}/bin/panda-cu-busy status >/dev/null || ${lockCmd}";
  idleTimeout = lib.optionalString cfg.autoLock "timeout 600 '${idleLockCmd}'";
  autostart = ''
    ${pkgs.swaybg}/bin/swaybg -c '#14161b' -m center -i ${wallpaper} >/dev/null 2>&1 &
    waybar >/dev/null 2>&1 &
    ${pkgs.fnott}/bin/fnott >/dev/null 2>&1 &
    ${pkgs.swayidle}/bin/swayidle -w ${idleTimeout} before-sleep '${lockCmd}' >/dev/null 2>&1 &
  '';

  waybarConfig = [
    {
      name = "top";
      layer = "top";
      position = "top";
      height = 26;
      # panda-agents-status stays installed; agentd is off by default, so its
      # module is not on the default bar.
      modules-left = [ "custom/panda" "custom/connect" ];
      modules-center = [ "clock" ];
      modules-right = [ "custom/dnd" "network" "cpu" "memory" "disk" "battery" "pulseaudio" ];
      "custom/panda" = {
        format = "panda";
        tooltip-format = "Repair menu (Super+X)";
        on-click = lib.getExe panda-menu;
      };
      "custom/connect" = {
        exec = lib.getExe panda-connect-status;
        return-type = "json";
        interval = 5;
        on-click = "${lib.getExe panda-connect-status} click";
      };
      # Shown only while do-not-disturb is on; panda-dnd sends signal 8.
      "custom/dnd" = {
        exec = "${lib.getExe panda-dnd} bar";
        interval = "once";
        signal = 8;
        tooltip-format = "Notifications paused (Super+N)";
        on-click = "${lib.getExe panda-dnd} toggle";
      };
      clock = {
        format = "{:%a %d %b  %H:%M}";
        tooltip-format = "{:%Y-%m-%d %H:%M:%S %Z}";
      };
      network = {
        format-wifi = "{essid} {signalStrength}%";
        format-ethernet = "{ifname} up";
        format-disconnected = "offline";
        tooltip-format = "{ifname} {ipaddr}/{cidr} via {gwaddr}";
        on-click = inTerm "nmtui";
      };
      cpu = {
        format = "CPU {usage}%";
        interval = 5;
        on-click = inTerm "btop";
      };
      memory = {
        format = "RAM {percentage}%";
        tooltip-format = "{used:0.1f} / {total:0.1f} GiB";
        interval = 5;
      };
      disk = {
        format = "Disk {percentage_used}%";
        path = "/";
        interval = 60;
      };
      battery = {
        format = "Bat {capacity}%";
        format-charging = "Bat {capacity}%+";
        states.critical = 15;
      };
      pulseaudio = {
        format = "Vol {volume}%";
        format-muted = "Vol muted";
        on-click = "${pkgs.wireplumber}/bin/wpctl set-mute @DEFAULT_AUDIO_SINK@ toggle";
      };
    }
    {
      name = "dock";
      layer = "top";
      position = "bottom";
      modules-center = [
        "custom/terminal"
        "custom/files"
        "custom/editor"
        "custom/monitor"
        "custom/network"
        "custom/logs"
        "custom/repair"
      ];
      "custom/terminal" = { format = "Terminal"; on-click = footExe; tooltip = false; };
      "custom/files" = { format = "Files"; on-click = "pcmanfm"; tooltip = false; };
      "custom/editor" = { format = "Editor"; on-click = "mousepad"; tooltip = false; };
      "custom/monitor" = { format = "Monitor"; on-click = inTerm "btop"; tooltip = false; };
      "custom/network" = { format = "Network"; on-click = inTerm "nmtui"; tooltip = false; };
      "custom/logs" = { format = "Logs"; on-click = inTerm "journalctl -f -n 200"; tooltip = false; };
      "custom/repair" = { format = "Repair"; on-click = lib.getExe panda-menu; tooltip = false; };
    }
  ];
in
{
  options.pandaos.desktop = {
    gpu = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = ''
        GPU acceleration: Mesa drivers (hardware.graphics) and labwc's GL
        renderer. Off by default: software rendering is enough for this shell
        and saves about 800 MB of closure (Mesa + LLVM).
      '';
    };
    autoLock = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Lock the screen after 10 minutes idle. It never locks while
        `panda-cu-busy` marks a computer-use task as running. Turn off for
        benchmark or kiosk images; the lock before suspend stays.
      '';
    };
    autologinUser = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "panda";
      description = "Start the Panda session for this user once at boot, skipping the greeter.";
    };
  };

  config = {
    programs.labwc = {
      enable = true;
      package = labwc;
    };
    hardware.graphics.enable = cfg.gpu;

    services.greetd = {
      enable = true;
      settings = {
        default_session = {
          command = "${pkgs.tuigreet}/bin/tuigreet --time --remember --cmd ${panda-session}/bin/panda-session";
          user = "greeter";
        };
        initial_session = lib.mkIf (cfg.autologinUser != null) {
          command = "${panda-session}/bin/panda-session";
          user = cfg.autologinUser;
        };
      };
    };

    # programs.labwc already declares security.pam.services.swaylock.

    # Accessibility bus on (screen readers, agents reading the AT-SPI tree).
    # Off, NixOS would set NO_AT_BRIDGE=1 and GTK_A11Y=none for everyone.
    # at-spi2-core is already in GTK 3's closure.
    services.gnome.at-spi2-core.enable = true;

    environment.systemPackages = [
      waybar
      fuzzel
      pkgs.foot
      # GTK 3 apps that agents and people can both drive: a text editor and a
      # file manager. GTK 3 is already here for waybar. Adwaita gives icons and
      # the fixed cursor theme; it is in pcmanfm's closure anyway.
      pkgs.mousepad
      pkgs.pcmanfm
      pkgs.adwaita-icon-theme
      pkgs.swaybg
      pkgs.lf
      pkgs.btop
      pkgs.brightnessctl
      pkgs.wireplumber # wpctl for the volume keys
      panda-session
      panda-lock
      panda-agents-status
      panda-connect-status
      panda-menu
      panda-dnd
      panda-cu-busy
    ];

    environment.etc = {
      "xdg/labwc/rc.xml".source = ./labwc/rc.xml;
      "xdg/labwc/menu.xml".source = ./labwc/menu.xml;
      "xdg/labwc/themerc-override".source = ./labwc/themerc-override;
      "xdg/labwc/environment".source = ./labwc/environment;
      "xdg/labwc/autostart".text = autostart;
      "xdg/fuzzel/fuzzel.ini".source = ./fuzzel.ini;
      "xdg/gtk-3.0/settings.ini".source = ./gtk-3.0/settings.ini;
      "xdg/waybar/config".text = builtins.toJSON waybarConfig;
      "xdg/waybar/style.css".source = ./waybar/style.css;
      "xdg/fnott/fnott.ini".source = ./fnott.ini;
    };
  };
}
