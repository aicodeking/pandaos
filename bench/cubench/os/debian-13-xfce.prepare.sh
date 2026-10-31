# shellcheck shell=bash disable=SC2154 # u, h: from common.sh (cubench.py prepends it)
# Debian 13 "generic" cloud image (full kernel, so virtio-gpu DRM is there) ->
# minimal Xfce on Xorg, lightdm autologin, no screen blanking or locker.
export DEBIAN_FRONTEND=noninteractive
apt_get() { apt-get -o DPkg::Lock::Timeout=900 -yq "$@"; }
apt_get update
apt_get install xorg xfce4 xfce4-terminal mousepad thunar lightdm firefox-esr dbus-user-session
# AT-SPI for --tools atspi and the validate oracles; dconf for checkers.
apt_get install at-spi2-core libatk-adaptor python3-gi gir1.2-atspi-2.0 dconf-cli dconf-gsettings-backend
apt_get purge light-locker xfce4-screensaver || true
install -d /etc/lightdm/lightdm.conf.d
printf '[Seat:*]\nautologin-user=%s\nautologin-session=xfce\nxserver-command=X -s 0 -dpms\n' "$u" \
  > /etc/lightdm/lightdm.conf.d/50-cubench.conf
groupadd -f autologin
usermod -aG autologin "$u"
# Skip the "default panel or empty panel?" first-login dialog.
install -d "$h/.config/xfce4/xfconf/xfce-perchannel-xml"
cp /etc/xdg/xfce4/panel/default.xml "$h/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-panel.xml"
chown -R "$u:" "$h/.config"
firefox_policies
finish
