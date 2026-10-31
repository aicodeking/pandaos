# panda-cu: computer use for agents. `panda-cu mcp` (registered as
# /etc/panda/mcp.d/panda-cu.json) takes screenshots, moves the mouse, types
# and reads the AT-SPI tree of the agent user's own display session.
#
# mode = "headless": panda-cu.service runs labwc on wlroots' headless backend
# (pixman, one output) as the agent user: an invisible desktop that nobody
# else sees or touches. mode = "attach": the caller's own logged-in session
# (/run/user/$UID), e.g. pandaos.desktop.autologinUser = "agent".
#
# Determinism in headless mode (each item is what makes screenshots and
# a11y boxes repeat run to run):
#   - No animations: labwc has none; GTK gets gtk-enable-animations=false
#     (settings.ini and the GSettings keyfile), cursor blink off.
#   - Fixed DPI and fonts: a private fonts.conf with DejaVu only (one family:
#     Sans/Serif/Mono), 96 dpi, hinting slight, grayscale AA; scale fixed.
#   - Windows open fullscreen (labwc window rule): no decorations, the window
#     origin is the screen origin, so AT-SPI boxes (window-relative on
#     Wayland) are screen coordinates.
#   - Notifications suppressed: the session D-Bus is private and only knows
#     the AT-SPI bus, so no notification daemon can be activated.
#   - Dark theme: GTK_THEME=Adwaita:dark (built into GTK 3 and 4).
#   - No first-run or keyring dialogs: fresh XDG config/cache/data dirs in
#     /run/panda-cu on every start; no gnome-keyring/secret service on the
#     bus; GSettings uses the keyfile backend (no dconf); Chromium gets
#     --no-first-run --password-store=basic.
#   - Portals off: GTK_USE_PORTAL=0 (GTK 3), GDK_DEBUG=no-portals (GTK 4), and
#     no xdg-desktop-portal on the bus.
{ config, lib, pkgs, ... }:

let
  cfg = config.services.panda-cu;
  headless = cfg.mode == "headless";
  runDir = "/run/panda-cu";
  home = config.users.users.${cfg.user}.home;
  res = lib.splitString "x" cfg.resolution;
  width = lib.toInt (lib.elemAt res 0);
  height = lib.toInt (lib.elemAt res 1);

  # Same no-Xwayland labwc as components/desktop (same store path when both
  # are on); stock labwc only when Xwayland is wanted.
  labwc =
    if cfg.xwayland then
      pkgs.labwc
    else
      let
        wlroots = pkgs.wlroots_0_19.overrideAttrs { enableXWayland = false; };
      in
      (pkgs.labwc.override { wlroots_0_19 = wlroots; }).overrideAttrs (old: {
        buildInputs = lib.filter (p: (p.pname or "") != "xwayland") old.buildInputs;
        mesonFlags = [ (lib.mesonEnable "xwayland" false) ];
      });

  # Read by GTK, Qt, Chromium and Firefox to expose an AT-SPI tree.
  a11yEnv = {
    GTK_A11Y = "atspi"; # GTK 4
    GNOME_ACCESSIBILITY = "1"; # GTK 3, Firefox
    QT_ACCESSIBILITY = "1";
    QT_LINUX_ACCESSIBILITY_ALWAYS_ON = "1";
    ACCESSIBILITY_ENABLED = "1"; # Chromium
  };

  fontsConf = pkgs.writeText "panda-cu-fonts.conf" ''
    <?xml version="1.0"?>
    <!DOCTYPE fontconfig SYSTEM "urn:fontconfig:fonts.dtd">
    <fontconfig>
      <dir>${pkgs.dejavu_fonts}/share/fonts</dir>
      <cachedir>${runDir}/cache/fontconfig</cachedir>
      <alias binding="strong"><family>sans-serif</family><prefer><family>DejaVu Sans</family></prefer></alias>
      <alias binding="strong"><family>serif</family><prefer><family>DejaVu Serif</family></prefer></alias>
      <alias binding="strong"><family>monospace</family><prefer><family>DejaVu Sans Mono</family></prefer></alias>
      <match target="pattern"><edit name="dpi" mode="assign"><double>96</double></edit></match>
      <match target="font">
        <edit name="antialias" mode="assign"><bool>true</bool></edit>
        <edit name="hinting" mode="assign"><bool>true</bool></edit>
        <edit name="hintstyle" mode="assign"><const>hintslight</const></edit>
        <edit name="autohint" mode="assign"><bool>false</bool></edit>
        <edit name="rgba" mode="assign"><const>none</const></edit>
        <edit name="lcdfilter" mode="assign"><const>lcdnone</const></edit>
        <edit name="embeddedbitmap" mode="assign"><bool>false</bool></edit>
      </match>
    </fontconfig>
  '';

  gtkSettings = ''
    [Settings]
    gtk-theme-name=Adwaita
    gtk-application-prefer-dark-theme=true
    gtk-font-name=DejaVu Sans 10
    gtk-enable-animations=false
    gtk-cursor-blink=false
    gtk-error-bell=false
    gtk-enable-event-sounds=false
    gtk-recent-files-enabled=false
    gtk-dialogs-use-header=false
    gtk-xft-antialias=1
    gtk-xft-hinting=1
    gtk-xft-hintstyle=hintslight
    gtk-xft-rgba=none
    gtk-xft-dpi=98304
  '';

  # GTK on Wayland takes these from GSettings, not settings.ini.
  gsettingsKeyfile = ''
    [org/gnome/desktop/interface]
    enable-animations=false
    cursor-blink=false
    color-scheme='prefer-dark'
    gtk-theme='Adwaita'
    font-name='DejaVu Sans 10'
    document-font-name='DejaVu Sans 10'
    monospace-font-name='DejaVu Sans Mono 10'
    font-antialiasing='grayscale'
    font-hinting='slight'
    text-scaling-factor=1.0
    overlay-scrolling=false
    toolkit-accessibility=true

    [org/gnome/desktop/a11y/applications]
    screen-reader-enabled=true

    [org/gnome/desktop/notifications]
    show-banners=false

    [org/gnome/desktop/privacy]
    remember-recent-files=false
  '';

  xdgConfig = pkgs.linkFarm "panda-cu-xdg-config" [
    { name = "gtk-3.0/settings.ini"; path = pkgs.writeText "gtk3.ini" gtkSettings; }
    { name = "gtk-4.0/settings.ini"; path = pkgs.writeText "gtk4.ini" gtkSettings; }
    { name = "glib-2.0/settings/keyfile"; path = pkgs.writeText "keyfile" gsettingsKeyfile; }
  ];

  # Private session bus: only the AT-SPI bus launcher can be activated.
  busConf = pkgs.writeText "panda-cu-bus.conf" ''
    <!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
     "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
    <busconfig>
      <type>session</type>
      <listen>unix:path=${runDir}/bus</listen>
      <auth>EXTERNAL</auth>
      <servicedir>${pkgs.at-spi2-core}/share/dbus-1/services</servicedir>
      <policy context="default">
        <allow send_destination="*" eavesdrop="true"/>
        <allow eavesdrop="true"/>
        <allow own="*"/>
      </policy>
    </busconfig>
  '';

  sessionEnv = a11yEnv // {
    XDG_RUNTIME_DIR = runDir;
    WAYLAND_DISPLAY = "wayland-0"; # first socket labwc creates in a fresh runDir
    DBUS_SESSION_BUS_ADDRESS = "unix:path=${runDir}/bus";
    XDG_SESSION_TYPE = "wayland";
    XDG_CURRENT_DESKTOP = "labwc:wlroots";
    XDG_CONFIG_HOME = "${runDir}/config";
    XDG_CACHE_HOME = "${runDir}/cache";
    XDG_DATA_HOME = "${runDir}/data";
    XDG_STATE_HOME = "${runDir}/state";
    FONTCONFIG_FILE = "${fontsConf}";
    GTK_THEME = "Adwaita:dark";
    GSETTINGS_BACKEND = "keyfile";
    GTK_USE_PORTAL = "0";
    GDK_DEBUG = "no-portals";
    GTK_OVERLAY_SCROLLING = "0"; # scrollbars always drawn
    GDK_BACKEND = "wayland";
    GSK_RENDERER = "cairo"; # GTK 4 without GL
    QT_QPA_PLATFORM = "wayland";
    NIXOS_OZONE_WL = "1";
    MOZ_ENABLE_WAYLAND = "1";
  };

  labwcDir = pkgs.linkFarm "panda-cu-labwc" [
    {
      name = "rc.xml";
      path = pkgs.writeText "rc.xml" ''
        <?xml version="1.0" encoding="UTF-8"?>
        <labwc_config>
          <core><gap>0</gap></core>
          <theme><cornerRadius>0</cornerRadius><font><name>DejaVu Sans</name><size>10</size></font></theme>
          <focus><followMouse>no</followMouse></focus>
          <placement><policy>center</policy></placement>
          ${lib.optionalString cfg.fullscreenWindows ''
            <windowRules>
              <windowRule identifier="*"><action name="ToggleFullscreen" /></windowRule>
            </windowRules>
          ''}
        </labwc_config>
      '';
    }
    { name = "menu.xml"; path = pkgs.writeText "menu.xml" "<openbox_menu></openbox_menu>\n"; }
  ];

  # Run by `labwc -s` (no shell; absolute shebang) once the compositor is up.
  # An autostart file never ran here (checks.panda-cu, CI run 41184234354):
  # labwc runs it through `sh`, which is not on a NixOS unit's PATH. Output
  # goes to the unit's journal (journalctl -u panda-cu).
  startup = pkgs.writeShellScript "panda-cu-startup" ''
    ${pkgs.systemd}/bin/systemd-cat -t panda-cu-session ${pkgs.wlr-randr}/bin/wlr-randr --output HEADLESS-1 --custom-mode ${cfg.resolution} --scale ${toString cfg.scale}
    exec ${pkgs.systemd}/bin/systemd-cat -t panda-cu-session ${lib.getExe cfg.package} session
  '';

  session = pkgs.writeShellApplication {
    name = "panda-cu-session";
    runtimeInputs = [ pkgs.coreutils pkgs.dbus ];
    text = ''
      cd "$XDG_RUNTIME_DIR"
      rm -rf ready launch.sock bus wayland-* config cache data state
      mkdir -p "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME" "$XDG_DATA_HOME" "$XDG_STATE_HOME"
      cp -rL --no-preserve=mode ${xdgConfig}/. "$XDG_CONFIG_HOME"/
      dbus-daemon --fork --config-file=${busConf}
      cd "$HOME"
      exec ${labwc}/bin/labwc -C ${labwcDir} -s ${startup}
    '';
  };

  browserApp =
    let
      b = cfg.browser;
      chromium = lib.hasInfix "chromium" (b.pname or b.name);
    in
    {
      name = "Web Browser";
      argv =
        [ (lib.getExe b) ]
        ++ lib.optionals chromium (
          [
            "--ozone-platform=wayland"
            "--force-renderer-accessibility"
            "--no-first-run"
            "--no-default-browser-check"
            "--password-store=basic"
            "--disable-gpu"
          ]
          ++ lib.optional headless "--user-data-dir=${runDir}/chromium"
        );
    };

  builtinApps = {
    editor = { name = "Text Editor"; argv = [ (lib.getExe (pkgs.mousepad or pkgs.xfce.mousepad)) ]; };
    files = { name = "File Manager"; argv = [ (lib.getExe pkgs.pcmanfm) ]; };
  } // lib.optionalAttrs (cfg.browser != null) { browser = browserApp; };

  apps = builtinApps // lib.mapAttrs (_: a: { inherit (a) name; argv = a.command; }) cfg.extraApps;

  desktopItems = lib.mapAttrsToList (
    id: a:
    pkgs.makeDesktopItem {
      name = "panda-cu-${id}";
      desktopName = "${a.name} (panda-cu)";
      exec = lib.concatStringsSep " " a.argv;
      noDisplay = true;
    }
  ) apps;

  cuConfig = {
    inherit (cfg) mode;
    inherit apps;
    scale = cfg.scale;
    env = if headless then sessionEnv else a11yEnv;
    open = { dir = "files"; file = "editor"; } // lib.optionalAttrs (cfg.browser != null) { url = "browser"; };
  } // lib.optionalAttrs headless { inherit width height; };
in
{
  options.services.panda-cu = {
    enable = lib.mkEnableOption "panda-cu, computer use (screenshots, mouse, keyboard, AT-SPI) for agents over MCP";
    mode = lib.mkOption {
      type = lib.types.enum [ "headless" "attach" ];
      default = "headless";
      description = ''
        headless: a private, invisible labwc desktop for `user` (panda-cu.service).
        attach: the caller's own logged-in Wayland session in /run/user/$UID;
        panda-cu refuses a display socket owned by another user.
      '';
    };
    user = lib.mkOption {
      type = lib.types.str;
      default = "agent";
      description = "User that owns the headless session (the agent user of pandacode-host).";
    };
    resolution = lib.mkOption {
      type = lib.types.strMatching "[0-9]{3,5}x[0-9]{3,5}";
      default = "1280x800";
      description = "Headless output size in pixels (= screenshot size).";
    };
    scale = lib.mkOption {
      type = lib.types.numbers.positive;
      default = 1;
      description = "Headless output scale. Keep 1: screenshot pixels then equal app pixels.";
    };
    xwayland = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Run X11 apps through Xwayland in the headless session (adds Xwayland to the closure).";
    };
    fullscreenWindows = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Open every window fullscreen in the headless session. AT-SPI boxes are
        window-relative on Wayland, so this is what makes them screen
        coordinates; turn it off only if you click by pixels from screenshots.
      '';
    };
    browser = lib.mkOption {
      type = lib.types.nullOr lib.types.package;
      default = if config.services.panda-browser.enable or false then config.services.panda-browser.package else null;
      defaultText = lib.literalExpression "services.panda-browser.package if services.panda-browser.enable, else null";
      example = lib.literalExpression "pkgs.firefox";
      description = "Browser for cu_launch app=browser and cu_open url (null: none).";
    };
    extraApps = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.submodule {
          options = {
            name = lib.mkOption { type = lib.types.str; description = "Display name."; };
            command = lib.mkOption {
              type = lib.types.nonEmptyListOf lib.types.str;
              description = "argv; the first element should be an absolute path.";
            };
          };
        }
      );
      default = { };
      example = lib.literalExpression ''{ calc = { name = "Calculator"; command = [ "''${pkgs.gnome-calculator}/bin/gnome-calculator" ]; }; }'';
      description = "More apps for the cu_launch allowlist (besides editor, files and browser).";
    };
    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./package.nix { }";
      description = "panda-cu package.";
    };
  };

  config = lib.mkIf cfg.enable (lib.mkMerge [
    {
      environment.systemPackages = [ cfg.package ] ++ desktopItems;
      environment.etc."panda/cu.json".text = builtins.toJSON cuConfig;
      services.gnome.at-spi2-core.enable = true;
      fonts.packages = [ pkgs.dejavu_fonts ];

      # Registry entry (agent-env): /etc/panda/mcp.d/panda-cu.json.
      pandaos.agents.mcpServers.panda-cu = {
        command = "panda-cu";
        args = [ "mcp" ];
      };
    }

    (lib.mkIf (!headless) {
      environment.sessionVariables = a11yEnv;
    })

    (lib.mkIf headless {
      # at-spi-bus-launcher execs dbus-daemon from the system profile.
      environment.systemPackages = [ pkgs.dbus ];

      systemd.services.panda-cu = {
        description = "panda-cu headless desktop for ${cfg.user} (labwc, ${cfg.resolution})";
        wantedBy = [ "multi-user.target" ];
        environment = removeAttrs sessionEnv [ "WAYLAND_DISPLAY" ] // {
          WLR_BACKENDS = "headless";
          WLR_RENDERER = "pixman";
          WLR_HEADLESS_OUTPUTS = "1";
          WLR_LIBINPUT_NO_DEVICES = "1";
        };
        serviceConfig = {
          User = cfg.user;
          ExecStart = lib.getExe session;
          RuntimeDirectory = "panda-cu";
          RuntimeDirectoryMode = "0700"; # only the agent user reaches the display, bus and launcher
          Restart = "on-failure";
          RestartSec = 2;
          # Apps run here (via panda-cu's launcher socket), so they may write
          # only the agent's home and the session's runtime dir.
          NoNewPrivileges = true;
          ProtectSystem = "strict";
          ReadWritePaths = [ home ];
          PrivateTmp = true;
          ProtectKernelModules = true;
          ProtectClock = true;
          ProtectHostname = true;
          LockPersonality = true;
          RestrictRealtime = true;
          RestrictSUIDSGID = true;
        };
      };
    })
  ]);
}
