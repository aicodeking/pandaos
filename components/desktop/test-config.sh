#!/bin/sh
# Static checks of the computer-use friendly desktop config.
# Usage: test-config.sh [dir]  (this directory, or /etc/xdg on a booted system)
# Exits non-zero on the first failed check.
set -eu
dir=${1:-$(dirname "$0")}

# conf NAME: the file in the repo layout or the /etc/xdg layout.
conf() {
  for f in "$dir/$1" "$dir/${1%.ini}/$1"; do
    [ -f "$f" ] && { echo "$f"; return 0; }
  done
  echo "FAIL missing $1 under $dir" >&2
  exit 1
}

# has NAME FILE PATTERN / lacks NAME FILE PATTERN: grep -E checks.
has() {
  grep -Eq -- "$3" "$2" || { echo "FAIL $1: no /$3/ in $2" >&2; exit 1; }
  echo "ok $1"
}
lacks() {
  if grep -Eq -- "$3" "$2"; then echo "FAIL $1: /$3/ in $2" >&2; exit 1; fi
  echo "ok $1"
}

rc=$(conf labwc/rc.xml)
theme=$(conf labwc/themerc-override)
env=$(conf labwc/environment)
gtk=$(conf gtk-3.0/settings.ini)
fuzzel=$(conf fuzzel.ini)
fnott=$(conf fnott.ini)

has no-shadows "$rc" '<dropShadows>no</dropShadows>'
has theme-no-shadow "$theme" '^window\.active\.shadow\.size: 0$'
has centered "$rc" '<policy>center</policy>'
has focus-rule "$rc" '<windowRule identifier="\*">'
has alt-tab "$rc" '<default ?/>'
has flat-accel "$rc" '<accelProfile>flat</accelProfile>'
has key-repeat "$rc" '<repeatDelay>600</repeatDelay>'
has focus-ring "$theme" '^border\.width: 2$'
has big-buttons "$theme" '^window\.button\.width: 32$'
has us-layout "$env" '^XKB_DEFAULT_LAYOUT=us$'
has cursor-size "$env" '^XCURSOR_SIZE=24$'
has scale-1 "$env" '^GDK_SCALE=1$'
has qt-a11y "$env" '^QT_ACCESSIBILITY=1$'
lacks gtk-a11y-on "$env" '^(GTK_A11Y|NO_AT_BRIDGE)='
has gtk-no-animations "$gtk" '^gtk-enable-animations=false$'
has gtk-no-blink "$gtk" '^gtk-cursor-blink=false$'
has launcher-no-cache "$fuzzel" '^cache=/dev/null$'
has notify-opaque "$fnott" '^background=[0-9a-f]{6}ff$'
