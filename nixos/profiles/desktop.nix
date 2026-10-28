# Desktop: very light Wayland session (labwc), no GNOME/KDE, no X server.
{ pkgs, ... }:
{
  programs.labwc.enable = true;
  services.xserver.enable = false;
  # labwc's module turns on graphical-desktop, which brings these in by default.
  # No X server means no Xwayland either; speechd (TTS engines + python) is
  # dead weight without a screen reader; dejavu below is the only font we ship.
  programs.xwayland.enable = false;
  services.speechd.enable = false;
  fonts.enableDefaultPackages = false;
  hardware.graphics.enable = true;

  services.greetd = {
    enable = true;
    settings.default_session = {
      command = "${pkgs.tuigreet}/bin/tuigreet --time --remember --cmd labwc";
      user = "greeter";
    };
  };

  # Minimal audio.
  security.rtkit.enable = true;
  services.pipewire = {
    enable = true;
    alsa.enable = true;
    pulse.enable = true;
  };

  # The ISO's minimal profile turns these off; a desktop needs them.
  fonts.fontconfig.enable = true;
  fonts.packages = [ pkgs.dejavu_fonts ];
  xdg.icons.enable = true;
  xdg.mime.enable = true;

  environment.systemPackages = with pkgs; [
    foot
    fuzzel
    # Placeholder dock until the Panda shell lands.
    waybar
  ];
}
