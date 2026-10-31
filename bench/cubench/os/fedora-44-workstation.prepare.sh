# shellcheck shell=bash disable=SC2154 # u, h: from common.sh (cubench.py prepends it)
# Fedora 44 Cloud Base -> the Workstation environment group (GNOME, Wayland),
# gdm autologin. Large (~2 GB of packages): run it only when CI time allows.
dnf -y install @workstation-product-environment
dnf -y remove gnome-initial-setup gnome-tour || true
gdm_autologin /etc/gdm/custom.conf
gnome_defaults
firefox_policies
finish
