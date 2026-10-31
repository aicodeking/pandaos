# shellcheck shell=bash disable=SC2154 # u, h: from common.sh (cubench.py prepends it)
# Fedora 44 Cloud Base -> the Workstation environment group (GNOME, Wayland),
# gdm autologin. Large (~2 GB of packages): run it only when CI time allows.
dnf -y install @workstation-product-environment
dnf -y install at-spi2-core python3-gobject dconf
dnf -y remove gnome-initial-setup gnome-tour || true
gdm_autologin /etc/gdm/custom.conf
gnome_defaults
firefox_policies
finish
# ~/bench on ext4: fanotify's file-handle mode (the writer audit) refuses
# btrfs subvolumes, and Fedora's root and /home are subvolumes.
truncate -s 512M /var/lib/cubench-bench.img
mkfs.ext4 -q -F /var/lib/cubench-bench.img
install -d -o "$u" -g "$u" "$h/bench"
echo "/var/lib/cubench-bench.img $h/bench ext4 loop,defaults,context=system_u:object_r:user_home_t:s0 0 0" >> /etc/fstab
mount "$h/bench"
rmdir "$h/bench/lost+found"
chown "$u:" "$h/bench"
