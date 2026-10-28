# Panda shell: labwc + waybar configs, wallpaper and repair scripts, all
# installed system-wide under /etc/xdg so per-user configs still override them.
{ lib, pkgs, ... }:
let
  footExe = "${pkgs.foot}/bin/foot";
  inTerm = cmd: "${footExe} --title 'Panda: ${cmd}' ${cmd}";
  held = cmd: "${footExe} --hold --title 'Panda: ${cmd}' ${cmd}";

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

  panda-agents-status = pkgs.writeShellApplication {
    name = "panda-agents-status";
    runtimeInputs = [ pkgs.jq ];
    text = builtins.readFile ./bin/panda-agents-status;
  };

  # panda-mem, panda-agentd and nixos-rebuild come from the system PATH:
  # they belong to the running system generation, not to this script.
  panda-menu = pkgs.writeShellApplication {
    name = "panda-menu";
    runtimeInputs = [
      pkgs.foot
      pkgs.fuzzel
      pkgs.ncdu
      pkgs.util-linux # lsblk
      pkgs.coreutils # df
      pkgs.networkmanager # nmtui
    ];
    text = builtins.readFile ./bin/panda-menu;
  };

  # Rendered at build time; the PNG has no runtime references (librsvg is
  # build-only), and swaybg loads PNG via gdk-pixbuf, already in waybar's closure.
  wallpaper = pkgs.runCommand "panda-wallpaper.png" { nativeBuildInputs = [ pkgs.librsvg ]; } ''
    rsvg-convert ${./wallpaper.svg} -o $out
  '';

  waybarConfig = [
    {
      name = "top";
      layer = "top";
      position = "top";
      height = 26;
      modules-left = [ "custom/panda" "custom/agents" ];
      modules-center = [ "clock" ];
      modules-right = [ "network" "cpu" "memory" "disk" "battery" "pulseaudio" ];
      "custom/panda" = {
        format = "panda";
        tooltip-format = "Repair menu (Super+X)";
        on-click = lib.getExe panda-menu;
      };
      "custom/agents" = {
        exec = lib.getExe panda-agents-status;
        return-type = "json";
        interval = 5;
        on-click = held "panda-agentd status";
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
        "custom/monitor"
        "custom/network"
        "custom/logs"
        "custom/repair"
      ];
      "custom/terminal" = { format = "Terminal"; on-click = footExe; tooltip = false; };
      "custom/files" = { format = "Files"; on-click = inTerm "lf"; tooltip = false; };
      "custom/monitor" = { format = "Monitor"; on-click = inTerm "btop"; tooltip = false; };
      "custom/network" = { format = "Network"; on-click = inTerm "nmtui"; tooltip = false; };
      "custom/logs" = { format = "Logs"; on-click = inTerm "journalctl -f -n 200"; tooltip = false; };
      "custom/repair" = { format = "Repair"; on-click = lib.getExe panda-menu; tooltip = false; };
    }
  ];
in
{
  environment.systemPackages = [
    waybar
    pkgs.swaybg
    pkgs.lf
    pkgs.btop
    panda-agents-status
    panda-menu
  ];

  environment.etc = {
    "xdg/labwc/rc.xml".source = ./labwc/rc.xml;
    "xdg/labwc/menu.xml".source = ./labwc/menu.xml;
    "xdg/labwc/themerc-override".source = ./labwc/themerc-override;
    "xdg/labwc/autostart".text =
      builtins.replaceStrings
        [ "@swaybg@" "@wallpaper@" ]
        [ "${pkgs.swaybg}/bin/swaybg" "${wallpaper}" ]
        (builtins.readFile ./labwc/autostart);
    "xdg/waybar/config".text = builtins.toJSON waybarConfig;
    "xdg/waybar/style.css".source = ./waybar/style.css;
  };
}
