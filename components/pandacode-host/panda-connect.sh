# panda-connect: sign this box in to PandaCode Connect and manage the host.
# Set by module.nix (runtimeEnv): HOST_USER HOST_HOME DEFAULT_NAME MARKER
# FULL_ACCESS_FILE VERSION.
unit=pandacode-host.service

usage() {
  cat >&2 <<'EOF'
usage: panda-connect sign-in [--device-name NAME]
       panda-connect sign-out
       panda-connect full-access on|off
       panda-connect status [--json]
       panda-connect logs [journalctl args, e.g. -f]
EOF
  exit 2
}

need_root() {
  if [ "$(id -u)" -ne 0 ]; then
    exec sudo "$0" "$@"
  fi
}

# pandacode as the host user; it needs a display to start, even for one-shot
# flags. Own process group: one-shot flags leave WebKit helpers behind.
as_host() {
  setsid runuser -u "$HOST_USER" -- env HOME="$HOST_HOME" PANDACODE_SECRETS_FILE=1 \
    xvfb-run -a pandacode "$@" &
  host_pid=$!
  host_rc=0
  wait "$host_pid" || host_rc=$?
  kill -TERM -- "-$host_pid" 2>/dev/null || true
  return "$host_rc"
}

sign_in() {
  name=$DEFAULT_NAME
  while [ $# -gt 0 ]; do
    case "$1" in
      --device-name) [ $# -ge 2 ] || usage; name=$2; shift ;;
      *) usage ;;
    esac
    shift
  done
  [ -n "$name" ] || { echo "panda-connect: --device-name needs a name" >&2; exit 2; }

  # One instance per data dir.
  systemctl stop "$unit" || true

  dir=$(mktemp -d)
  pid=
  cleanup() {
    if [ -n "$pid" ]; then kill -TERM -- "-$pid" 2>/dev/null || true; fi
    rm -rf "$dir"
  }
  trap cleanup EXIT
  mkfifo "$dir/out"

  # `serve --sign-in` signs in and then keeps serving. Its own process group,
  # so it can be stopped as a whole once it reports success; the service
  # takes over from there.
  setsid runuser -u "$HOST_USER" -- env HOME="$HOST_HOME" PANDACODE_SECRETS_FILE=1 \
    xvfb-run -a pandacode serve --sign-in --device-name "$name" >"$dir/out" 2>&1 &
  pid=$!

  ok=no
  while IFS= read -r line; do
    printf '%s\n' "$line"
    # PandaCode v0.3.75 prints "Signed in as <login>; this server is now hosting for your devices."
    case "$line" in
      *"now hosting for your devices"*) ok=yes; break ;;
    esac
  done <"$dir/out"

  if [ "$ok" != yes ]; then
    rc=0
    wait "$pid" || rc=$?
    pid=
    echo "panda-connect: sign-in did not complete (pandacode exited $rc)" >&2
    exit 1
  fi

  kill -TERM -- "-$pid" 2>/dev/null || true
  wait "$pid" || true
  pid=
  printf '%s\n' "$name" >"$MARKER"
  systemctl start "$unit"
  echo "panda-connect: signed in as device \"$name\"; $unit started."
}

sign_out() {
  [ $# -eq 0 ] || usage
  systemctl stop "$unit" || true
  rm -f "$MARKER"
  as_host serve --sign-out
}

full_access() {
  [ $# -eq 1 ] || usage
  case "$1" in
    on | off) ;;
    *) usage ;;
  esac
  # Overrides services.pandacode-host.fullAccess; the service applies it at start.
  printf '%s\n' "$1" >"$FULL_ACCESS_FILE"
  if [ -e "$MARKER" ]; then
    systemctl restart "$unit"
  fi
  echo "panda-connect: full access $1"
}

status() {
  json=no
  case "${1-}" in
    "") ;;
    --json) json=yes ;;
    *) usage ;;
  esac
  signed_in=false
  name=$DEFAULT_NAME
  if [ -e "$MARKER" ]; then
    signed_in=true
    name=$(head -n 1 "$MARKER")
  fi
  # is-active prints e.g. "inactive" and exits non-zero for anything but active.
  service=$(systemctl is-active "$unit" || true)
  case "$service" in
    active | inactive | failed | activating) ;;
    reloading | refreshing) service=active ;;
    *) service=inactive ;;
  esac
  if [ "$json" = yes ]; then
    jq -cn --argjson signedIn "$signed_in" --arg service "$service" \
      --arg deviceName "$name" --arg version "$VERSION" \
      '{signedIn: $signedIn, service: $service, deviceName: $deviceName, version: $version}'
  else
    printf 'signed in:   %s\nservice:     %s\ndevice name: %s\nversion:     %s\n' \
      "$signed_in" "$service" "$name" "$VERSION"
  fi
}

[ $# -ge 1 ] || usage
cmd=$1
shift
case "$cmd" in
  sign-in) need_root "$cmd" "$@"; sign_in "$@" ;;
  sign-out) need_root "$cmd" "$@"; sign_out "$@" ;;
  full-access) need_root "$cmd" "$@"; full_access "$@" ;;
  status) status "$@" ;;
  logs) exec journalctl -u "$unit" "$@" ;;
  *) usage ;;
esac
