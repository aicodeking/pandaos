# Desktop: very light Wayland session (labwc), no GNOME/KDE, no X server.
# The Panda shell (labwc session, greetd, waybar, lock, menus) and the
# pandaos.desktop.gpu option live in components/desktop.
{ lib, pkgs, ... }:
{
  imports = [ ../../components/desktop ];

  services.xserver.enable = false;
  # labwc's module turns on graphical-desktop, which brings these in by default.
  # No X server means no Xwayland either; speechd (TTS engines + python) is
  # dead weight without a screen reader; dejavu below is the only font we ship.
  programs.xwayland.enable = false;
  services.speechd.enable = false;
  fonts.enableDefaultPackages = false;

  # labwc's module forces the wlr and gtk portals on; nothing here needs them
  # (no Flatpak, no screen sharing), and the gtk one pulls ~200 MB of GNOME
  # libraries. wayland-session.nix sets wlr.enable without mkDefault, hence mkForce.
  xdg.portal.enable = lib.mkForce false;
  xdg.portal.wlr.enable = lib.mkForce false;

  # Minimal audio.
  security.rtkit.enable = true;
  services.pipewire = {
    enable = true;
    alsa.enable = true;
    pulse.enable = true;
  };

  # The ISO's minimal profile turns these off; a desktop needs them.
  fonts.packages = [ pkgs.dejavu_fonts ];
  # Crisp, predictable text in screenshots: slight hinting, greyscale
  # antialiasing (no colour fringes), DejaVu for everything.
  fonts.fontconfig = {
    enable = true;
    antialias = true;
    hinting = {
      enable = true;
      style = "slight";
    };
    subpixel.rgba = "none";
    defaultFonts = {
      sansSerif = [ "DejaVu Sans" ];
      serif = [ "DejaVu Serif" ];
      monospace = [ "DejaVu Sans Mono" ];
    };
  };
  # Computer use in the person's own session: `panda-cu call` / `panda-cu mcp`
  # run by an agent CLI inside the logged-in labwc session act on that screen.
  # attach mode adds no service and no RAM (only the package and the AT-SPI
  # env); a private headless desktop for the `agent` user is one option away:
  #   services.panda-cu.mode = "headless";
  services.panda-cu = {
    enable = lib.mkDefault true;
    mode = lib.mkDefault "attach";
  };

  xdg.icons.enable = true;
  xdg.mime.enable = true;
}
