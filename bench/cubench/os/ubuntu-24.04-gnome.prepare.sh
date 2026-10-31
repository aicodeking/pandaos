# shellcheck shell=bash disable=SC2154 # u, h: from common.sh (cubench.py prepends it)
# Ubuntu 24.04 cloud image -> ubuntu-desktop-minimal, gdm autologin into the
# default GNOME session (Wayland). Firefox is Mozilla's .deb: Ubuntu's own
# "firefox" package only installs the snap, which is slow to start in a VM.
export DEBIAN_FRONTEND=noninteractive
apt_get() { apt-get -o DPkg::Lock::Timeout=900 -yq "$@"; }
install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://packages.mozilla.org/apt/repo-signing-key.gpg -o /etc/apt/keyrings/packages.mozilla.org.asc
echo "deb [signed-by=/etc/apt/keyrings/packages.mozilla.org.asc] https://packages.mozilla.org/apt mozilla main" \
  > /etc/apt/sources.list.d/mozilla.list
printf 'Package: *\nPin: origin packages.mozilla.org\nPin-Priority: 1000\n' > /etc/apt/preferences.d/mozilla
apt_get update
apt_get install ubuntu-desktop-minimal firefox
apt_get purge gnome-initial-setup update-notifier || true
gdm_autologin /etc/gdm3/custom.conf
gnome_defaults
firefox_policies
finish
