# panda-connect: sign this box in to PandaCode Connect and manage the host.
# Set by module.nix (runtimeEnv): HOST_USER HOST_HOME SIGN_IN_NAME DEFAULT_NAME
# MARKER FULL_ACCESS_FILE VERSION.
unit=pandacode-host.service

usage() {
  cat >&2 <<'EOF'
usage: panda-connect sign-in [--device-name NAME] [--no-wait]
       panda-connect sign-out
       panda-connect full-access on|off
       panda-connect restart
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

# pandacode as the host user; it needs a display to start, even for one-shot flags.
# ponytail: one-shot runs may leave WebKit helpers behind (the service sweeps
# its cgroup; here they live until reboot); a transient unit would fix it.
as_host() {
  runuser -u "$HOST_USER" -- env HOME="$HOST_HOME" PANDACODE_SECRETS_FILE=1 \
    xvfb-run -a pandacode "$@"
}

# The sign-in runs as pandacode-sign-in.service (module.nix): the host's user,
# environment and sandbox, no terminal or display needed, every line in the
# journal. Here: start it and follow its journal until it ends, or with
# --no-wait return at once (read the code with `journalctl -u pandacode-sign-in`).
sign_in_unit=pandacode-sign-in.service
sign_in() {
  name=$DEFAULT_NAME
  wait=yes
  while [ $# -gt 0 ]; do
    case "$1" in
      --device-name) [ $# -ge 2 ] || usage; name=$2; shift ;;
      --no-wait) wait=no ;;
      *) usage ;;
    esac
    shift
  done
  [ -n "$name" ] || { echo "panda-connect: --device-name needs a name" >&2; exit 2; }
  case "$name" in
    *$'\n'*) echo "panda-connect: --device-name must be one line" >&2; exit 2 ;;
  esac

  printf '%s\n' "$name" >"$SIGN_IN_NAME"
  since=$(date +%s)
  # A sign-in already waiting for approval is replaced by this one.
  systemctl stop "$sign_in_unit" 2>/dev/null || true
  # Conflicts= stops pandacode-host (one instance per data dir). Returns once
  # the process runs (Type=simple), so is-active below sees it.
  systemctl start "$sign_in_unit"
  if [ "$wait" = no ]; then
    echo "panda-connect: signing in as \"$name\" in the background; the code: journalctl -u $sign_in_unit -o cat"
    return 0
  fi

  journalctl -f -q -o cat -u "$sign_in_unit" --since "@$since" &
  follow=$!
  trap 'kill "$follow" 2>/dev/null || true' EXIT
  # activating/active while it runs; inactive or failed once it ended.
  while state=$(systemctl is-active "$sign_in_unit"); [ "$state" = active ] || [ "$state" = activating ] || [ "$state" = deactivating ]; do
    sleep 1
  done
  sleep 1 # the last lines
  kill "$follow" 2>/dev/null || true
  if [ -e "$MARKER" ] && [ "$(systemctl show -P Result "$sign_in_unit")" = success ]; then
    echo "panda-connect: signed in as device \"$(head -n 1 "$MARKER")\"; $unit started."
    return 0
  fi
  echo "panda-connect: sign-in did not complete ($(systemctl show -P Result "$sign_in_unit")); see journalctl -u $sign_in_unit" >&2
  exit 1
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

# The unit has restartIfChanged=false so rebuilds never kill running agent
# tasks; this applies a new host version or unit setting now.
restart() {
  [ $# -eq 0 ] || usage
  if [ ! -e "$MARKER" ]; then
    echo "panda-connect: not signed in; run: panda-connect sign-in" >&2
    exit 1
  fi
  systemctl restart "$unit"
  echo "panda-connect: $unit restarted (running agent tasks were stopped)"
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
  restart) need_root "$cmd" "$@"; restart "$@" ;;
  status) status "$@" ;;
  logs) exec journalctl -u "$unit" "$@" ;;
  *) usage ;;
esac
