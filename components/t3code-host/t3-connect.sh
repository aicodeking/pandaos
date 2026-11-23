# t3-connect: sign this box in to T3 Code Connect and manage the T3 Code host.
# Set by module.nix (runtimeEnv): HOST_USER HOST_HOME T3CODE_HOME
# CLOUDFLARED_PATH MARKER SIGN_IN_MODE_FILE HOST PORT LAN VERSION.
#
# $MARKER holds the mode the host runs in: `connect` or `publish-only` (signed
# in to T3 Connect), or `local` (direct/SSH access only). No marker: off.
unit=t3code-host.service
sign_in_unit=t3code-sign-in.service
ready_timeout=30

usage() {
  cat >&2 <<'EOF'
usage: t3-connect sign-in [--publish-only] [--no-wait]
       t3-connect start | stop | restart
       t3-connect pair [t3 pair args, e.g. --tailscale]
       t3-connect approve CODE
       t3-connect devices
       t3-connect unlink
       t3-connect sign-out
       t3-connect status [--json] [--live]
       t3-connect logs [journalctl args, e.g. -f]
EOF
  exit 2
}

die() {
  echo "t3-connect: $*" >&2
  exit 1
}

need_root() {
  if [ "$(id -u)" -ne 0 ]; then
    exec sudo "$0" "$@"
  fi
}

# t3 as the host user, with the host's data dir and cloudflared, attached to
# the caller's terminal: whatever it prints (pairing tokens) stays here.
as_host() {
  local env=(HOME="$HOST_HOME" T3CODE_HOME="$T3CODE_HOME"
    T3CODE_PORT="$PORT" T3CODE_LAN="$LAN"
    T3CODE_MODE=web T3CODE_NO_BROWSER=true)
  if [ -n "$HOST" ]; then
    env+=(T3CODE_HOST="$HOST")
  fi
  if [ -n "$CLOUDFLARED_PATH" ]; then
    env+=(T3CODE_CLOUDFLARED_PATH="$CLOUDFLARED_PATH")
  fi
  (cd "$HOST_HOME" && runuser -u "$HOST_USER" -- env "${env[@]}" t3 "$@")
}

current_mode() {
  if [ -e "$MARKER" ]; then
    head -n 1 "$MARKER"
  else
    echo off
  fi
}

# Local-only unless already signed in: never downgrade a connect marker.
ensure_marker() {
  if [ ! -e "$MARKER" ]; then
    printf 'local\n' >"$MARKER"
    chmod 0644 "$MARKER"
  fi
}

service_state() {
  local s
  # is-active prints e.g. "inactive" and exits non-zero for anything but active.
  s=$(systemctl is-active "$unit" || true)
  case "$s" in
    active | inactive | failed | activating) ;;
    reloading | refreshing) s=active ;;
    *) s=inactive ;;
  esac
  printf '%s\n' "$s"
}

url_host() {
  case "$HOST" in
    0.0.0.0 | "") echo 127.0.0.1 ;;
    ::) echo '[::1]' ;;
    *:*) echo "[$HOST]" ;;
    *) echo "$HOST" ;;
  esac
}

is_loopback() {
  case "$HOST" in
    127.* | localhost | ::1) return 0 ;;
    *) return 1 ;;
  esac
}

# The sign-in runs as t3code-sign-in.service (module.nix): the host's user,
# environment and sandbox, no terminal needed, every line (the device code)
# in the journal. Follow it until it ends, or with --no-wait return at once.
sign_in() {
  local mode=managed wait=yes since follow state
  while [ $# -gt 0 ]; do
    case "$1" in
      --publish-only) mode=publish-only ;;
      --no-wait) wait=no ;;
      *) usage ;;
    esac
    shift
  done

  printf '%s\n' "$mode" >"$SIGN_IN_MODE_FILE"
  chmod 0644 "$SIGN_IN_MODE_FILE"
  since=$(date +%s)
  # A sign-in already waiting for approval is replaced by this one.
  systemctl stop "$sign_in_unit" 2>/dev/null || true
  # Conflicts= stops t3code-host (one T3 per data dir).
  systemctl start "$sign_in_unit"
  if [ "$wait" = no ]; then
    echo "t3-connect: signing in ($mode) in the background; the code: journalctl -u t3code-sign-in -o cat"
    return 0
  fi

  journalctl -f -q -o cat -u "$sign_in_unit" --since "@$since" &
  follow=$!
  trap 'kill "$follow" 2>/dev/null || true' EXIT
  while state=$(systemctl is-active "$sign_in_unit" || true); [ "$state" = active ] || [ "$state" = activating ] || [ "$state" = deactivating ]; do
    sleep 1
  done
  sleep 1 # the last lines
  kill "$follow" 2>/dev/null || true
  case "$(current_mode)" in
    connect | publish-only)
      if [ "$(systemctl show -P Result "$sign_in_unit")" = success ]; then
        echo "t3-connect: signed in ($(current_mode)); $unit started."
        return 0
      fi
      ;;
  esac
  die "sign-in did not complete ($(systemctl show -P Result "$sign_in_unit")); see journalctl -u $sign_in_unit"
}

start() {
  [ $# -eq 0 ] || usage
  ensure_marker
  systemctl start "$unit"
  echo "t3-connect: $unit started ($(current_mode))"
}

stop() {
  [ $# -eq 0 ] || usage
  systemctl stop "$unit"
  rm -f "$MARKER"
  echo "t3-connect: $unit stopped"
}

# The unit has restartIfChanged=false so rebuilds never kill running agent
# sessions; this applies a new T3 Code version or unit setting now.
restart() {
  [ $# -eq 0 ] || usage
  [ -e "$MARKER" ] || die "host is off; run: t3-connect start (or sign-in)"
  systemctl restart "$unit"
  echo "t3-connect: $unit restarted (running agent sessions were stopped)"
}

wait_ready() {
  local url i
  url="http://$(url_host):$PORT/"
  for ((i = 0; i < ready_timeout; i++)); do
    if curl -fsS -o /dev/null --max-time 2 "$url" 2>/dev/null; then
      return 0
    fi
    sleep 1
  done
  die "$unit did not answer on $url within ${ready_timeout}s; see t3-connect logs"
}

pair() {
  local arg tailscale=no
  for arg in "$@"; do
    if [ "$arg" = --tailscale ]; then tailscale=yes; fi
  done
  if [ "$(service_state)" != active ]; then
    ensure_marker
    systemctl start "$unit"
  fi
  wait_ready
  as_host pair "$@"
  if [ "$tailscale" = no ] && is_loopback; then
    echo "t3-connect: the host listens on $HOST only; from your machine: ssh -L $PORT:$HOST:$PORT <user>@<this box>" >&2
  fi
}

approve() {
  [ $# -eq 1 ] || usage
  [[ "$1" =~ ^[A-Za-z0-9-]{1,64}$ ]] || die "approve: CODE must be letters, digits and dashes"
  as_host auth connect-device approve "$1"
}

devices() {
  [ $# -eq 0 ] || usage
  as_host auth connect-device list
}

# `t3 connect unlink` leaves T3 Connect but keeps the T3 sign-in; like
# sign-out, the host stops so nothing keeps serving in an unexpected mode.
unlink_connect() {
  [ $# -eq 0 ] || usage
  systemctl stop "$unit" || true
  rm -f "$MARKER"
  as_host connect unlink
  echo "t3-connect: unlinked from T3 Connect (run 't3-connect start' for local/SSH-only access)"
}

sign_out() {
  [ $# -eq 0 ] || usage
  systemctl stop "$unit" || true
  rm -f "$MARKER"
  as_host connect logout
  echo "t3-connect: signed out; $unit stopped"
}

status() {
  local json=no live=no mode enabled=false signed_in=false service cloud=null
  local display_host=${HOST:-0.0.0.0}
  while [ $# -gt 0 ]; do
    case "$1" in
      --json) json=yes ;;
      --live) live=yes ;;
      *) usage ;;
    esac
    shift
  done
  mode=$(current_mode)
  case "$mode" in
    connect | publish-only) enabled=true signed_in=true ;;
    local) enabled=true ;;
    off) ;;
    *) enabled=true mode=local ;; # unknown content: the unit still starts
  esac
  service=$(service_state)
  if [ "$live" = yes ]; then
    # One JSON document, or null: never let a bad reply break our own JSON.
    cloud=$(as_host connect status --json 2>/dev/null |
      jq -cs 'if length == 1 then .[0] else null end' 2>/dev/null) || cloud=
    [ -n "$cloud" ] || cloud=null
  fi
  if [ "$json" = yes ]; then
    jq -cn --argjson enabled "$enabled" --argjson signedIn "$signed_in" \
      --arg mode "$mode" --arg service "$service" --arg host "$display_host" \
      --argjson port "$PORT" --arg version "$VERSION" \
      --argjson cloud "$cloud" --arg live "$live" \
      '{enabled: $enabled, signedIn: $signedIn, mode: $mode, service: $service,
        host: $host, port: $port, version: $version}
       + (if $live == "yes" then {cloud: $cloud} else {} end)'
  else
    printf 'enabled:   %s\nsigned in: %s\nmode:      %s\nservice:   %s\nlisten:    %s:%s\nversion:   %s\n' \
      "$enabled" "$signed_in" "$mode" "$service" "$display_host" "$PORT" "$VERSION"
    if [ "$live" = yes ]; then
      printf 'cloud:\n'
      jq -r 'if type == "object" then to_entries[] | "  \(.key): \(.value | tostring)"
             else "  \(tostring)" end' <<<"$cloud"
    fi
  fi
}

has_live() {
  local a
  for a in "$@"; do
    if [ "$a" = --live ]; then return 0; fi
  done
  return 1
}

[ $# -ge 1 ] || usage
cmd=$1
shift
case "$cmd" in
  sign-in) need_root "$cmd" "$@"; sign_in "$@" ;;
  start) need_root "$cmd" "$@"; start "$@" ;;
  stop) need_root "$cmd" "$@"; stop "$@" ;;
  restart) need_root "$cmd" "$@"; restart "$@" ;;
  pair) need_root "$cmd" "$@"; pair "$@" ;;
  approve) need_root "$cmd" "$@"; approve "$@" ;;
  devices) need_root "$cmd" "$@"; devices "$@" ;;
  unlink) need_root "$cmd" "$@"; unlink_connect "$@" ;;
  sign-out) need_root "$cmd" "$@"; sign_out "$@" ;;
  status)
    if has_live "$@"; then need_root "$cmd" "$@"; fi
    status "$@"
    ;;
  logs) exec journalctl -u "$unit" "$@" ;;
  *) usage ;;
esac
