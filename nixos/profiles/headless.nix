# Headless: no GUI, no sound, console getty only.
{ ... }:
{
  services.xserver.enable = false;
  services.pipewire.enable = false;
  services.pulseaudio.enable = false;
  fonts.fontconfig.enable = false;
  xdg.sounds.enable = false;
}
