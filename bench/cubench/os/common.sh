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

[org/gnome/desktop/interface]
toolkit-accessibility=true
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
  # A random password the agent cannot know (autologin needs none).
  echo "$u:$(head -c 18 /dev/urandom | base64)" | chpasswd
  systemctl set-default graphical.target
  # The writer audit: a root service watching ~/bench, its log in /var/tmp
  # (root-owned). audit.py was installed root-owned above this script.
  install -d -o "$u" -g "$u" "$h/bench"
  cat > /etc/systemd/system/cubench-audit.service <<UNIT
[Unit]
Description=cubench writer audit (fanotify)
After=local-fs.target
[Service]
ExecStartPre=/bin/rm -f /var/tmp/cubench-audit.log
ExecStart=/usr/bin/python3 /usr/local/lib/cubench/audit.py watch $h/bench /var/tmp/cubench-audit.log
Restart=always
[Install]
WantedBy=multi-user.target
UNIT
  cat > /etc/systemd/system/cubench-form.service <<UNIT
[Unit]
Description=cubench form server (127.0.0.1:8000)
After=network.target
[Service]
ExecStartPre=/bin/rm -f /var/tmp/cubench-form.log
ExecStart=/usr/bin/python3 /usr/local/lib/cubench/formserver.py /usr/local/lib/cubench/www /var/tmp/cubench-form.log
Restart=always
[Install]
WantedBy=multi-user.target
UNIT
  systemctl enable cubench-audit.service cubench-form.service
  # The agent drives this user, so it gets no sudo from here on: the harness
  # needs none after prepare. Later boots only take the run's ssh key from
  # cloud-init (no users-groups module, which would restore the sudo rule).
  rm -f /etc/sudoers.d/90-cloud-init-users
  for g in sudo wheel admin; do gpasswd -d "$u" "$g" >/dev/null 2>&1 || true; done
  cat > /etc/cloud/cloud.cfg.d/99-cubench.cfg <<'CFG'
cloud_init_modules: [seed_random, ssh]
cloud_config_modules: []
cloud_final_modules: []
CFG
  touch /var/lib/cubench-prepared
  systemd-run --on-active=5 systemctl poweroff
}
