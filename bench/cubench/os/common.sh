# shellcheck shell=bash
# Prepended to every os/*.prepare.sh by `cubench.py prepare`; runs as root.
set -euxo pipefail
u=$CUBENCH_USER
h=$(getent passwd "$u" | cut -d: -f6)

# Same Firefox setup everywhere: no first-run pages, no crash-restore prompt.
firefox_policies() {
  for d in /etc/firefox/policies /etc/firefox-esr/policies; do
    install -d "$d"
    cat > "$d/policies.json" <<'JSON'
{"policies": {"DisableTelemetry": true, "DisableFirefoxStudies": true, "DontCheckDefaultBrowser": true,
  "OverrideFirstRunPage": "", "OverridePostUpdatePage": "", "NoDefaultBookmarks": true,
  "UserMessaging": {"WhatsNew": false, "ExtensionRecommendations": false, "FeatureRecommendations": false,
                    "UrlbarInterventions": false, "SkipOnboarding": true, "MoreFromMozilla": false},
  "Preferences": {"browser.sessionstore.resume_from_crash": {"Value": false, "Status": "default"},
                  "browser.aboutwelcome.enabled": {"Value": false, "Status": "default"},
                  "datareporting.policy.dataSubmissionPolicyBypassNotification": {"Value": true, "Status": "default"}}}}
JSON
  done
}

# GNOME: no idle blank/lock, no welcome tour, no notification banners.
gnome_defaults() {
  install -d /etc/dconf/profile /etc/dconf/db/local.d
  [ -f /etc/dconf/profile/user ] || printf 'user-db:user\nsystem-db:local\n' > /etc/dconf/profile/user
  cat > /etc/dconf/db/local.d/00-cubench <<'INI'
[org/gnome/desktop/session]
idle-delay=uint32 0

[org/gnome/desktop/screensaver]
lock-enabled=false

[org/gnome/desktop/notifications]
show-banners=false

[org/gnome/shell]
welcome-dialog-last-shown-version='999'

[org/gnome/settings-daemon/plugins/power]
sleep-inactive-ac-type='nothing'
INI
  dconf update
  install -d -o "$u" -g "$u" "$h/.config"
  echo yes > "$h/.config/gnome-initial-setup-done"
  chown "$u:" "$h/.config/gnome-initial-setup-done"
}

gdm_autologin() {  # $1: gdm config path
  printf '[daemon]\nAutomaticLoginEnable=true\nAutomaticLogin=%s\n' "$u" > "$1"
}

finish() {
  echo "$u:cubench" | chpasswd
  systemctl set-default graphical.target
  touch /var/lib/cubench-prepared
}
