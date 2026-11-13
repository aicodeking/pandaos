# shellcheck shell=bash
# Sourced by e2e-host.sh: a guest calls boat-api at its public URL with its own
# token (BOAT_URL + BOAT_API_KEY in /etc/boat/env), as `boat self` / `host` do.
#
# In production BOAT_URL is https://api.<domain>, which resolves to this host,
# so the guest's packet is host input on boatbr0, not forward. Here a second
# boat-api listens on the host's own address, tcp/443 (Caddy's port), with
# -vm-api-url http://<host_ip>:443: the same route without TLS. boatd runs with
# hostHttpsPort=443, as the NixOS module sets whenever vmApiUrl is set. Controls:
# without hostHttpsPort the same call is dropped, and with it every other host
# port still is.
# shellcheck disable=SC2154 # host_ip, PIDS, bx, blocked, tcp, restart_boatd come from e2e.sh / e2e-host.sh

log "host e2e: guest -> boat-api at its public URL (host address, tcp/443) with its self token"
SA_URL=http://$host_ip:443
sa_port_start=$(sysctl -n net.ipv4.ip_unprivileged_port_start)
sudo sysctl -qw net.ipv4.ip_unprivileged_port_start=443 # boat-api stays unprivileged, as in production
rm -rf "$WORK/api-self" && mkdir -p "$WORK/api-self"
sa_key=$(boat-api admin init -data "$WORK/api-self" -user self -team self | jq -r .token)
[[ $sa_key == boat_* ]] || fail "self api: admin init: $sa_key"
boat-api serve -data "$WORK/api-self" -listen "$host_ip:443" -boatd "$SOCK" -rate 0 \
  -images slim -ssh-user dev -vm-api-url "$SA_URL" >"$WORK/api-self.log" 2>&1 &
sa_pid=$!
PIDS+=("$sa_pid")
for _ in $(seq 100); do curl -fs "$SA_URL/healthz" >/dev/null && break; sleep 0.1; done
curl -fs "$SA_URL/healthz" >/dev/null || fail "self api did not start: $(tail -5 "$WORK/api-self.log")"
sa=$(curl -fsS -X POST "$SA_URL/v1/vms" -H "Authorization: Bearer $sa_key" -H 'content-type: application/json' \
  -d '{"name":"sa","image":"slim","env":{"KEEP_ME":"r16 custom value"}}' | jq -r .id)
[ -n "$sa" ] && [ "$sa" != null ] || fail "self api: create"
# shellcheck disable=SC2016 # expanded in the guest, from /etc/boat/env
sa_get='curl -sS --max-time 5 -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $BOAT_API_KEY" "$BOAT_URL/v1/vms/$BOAT_ID"'
# shellcheck disable=SC2016
bx "$sa" '. /etc/boat/env; [ -n "$BOAT_API_KEY" ] && [ "$BOAT_URL" = '"'$SA_URL'"' ]' || fail "guest env has no BOAT_URL=$SA_URL + token"

# Control: boatd without hostHttpsPort drops the guest's packet in input.
jq '.hostHttpsPort = 0' "$WORK/boatd.json" >"$WORK/boatd-noapi.json"
restart_boatd "$WORK/boatd-noapi.json"
code=$(bx "$sa" ". /etc/boat/env; $sa_get || true")
[ "$code" = 000 ] || fail "guest reached the host's tcp/443 without hostHttpsPort (HTTP $code)"

jq '.hostHttpsPort = 443' "$WORK/boatd.json" >"$WORK/boatd-api.json"
restart_boatd "$WORK/boatd-api.json"
code=$(bx "$sa" ". /etc/boat/env; $sa_get || true")
[ "$code" = 200 ] || fail "guest -> $SA_URL with its self token: HTTP $code"
got=$(bx "$sa" '. /etc/boat/env; curl -fsS --max-time 5 -H "Authorization: Bearer $BOAT_API_KEY" "$BOAT_URL/v1/vms/$BOAT_ID"' | jq -r .id)
[ "$got" = "$sa" ] || fail "self token read $got, not $sa"
# The token reaches only its own VM, and only tcp/443 opened on the host.
code=$(bx "$sa" '. /etc/boat/env; curl -sS --max-time 5 -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $BOAT_API_KEY" "$BOAT_URL/v1/vms" || true')
[ "$code" = 404 ] || [ "$code" = 403 ] || fail "self token listed the team's VMs: HTTP $code"
blocked "$sa" "$(tcp "$host_ip" 8743)"
blocked "$sa" "$(tcp 10.200.0.1 8740)"
echo "guest -> $SA_URL/v1/vms/$sa with its self token: 200"

# Keep captured credentials entirely inside the guest and its disk copies. The
# probes return only HTTP status / VM identity, never a managed credential.
# shellcheck disable=SC2016
sa_save_token='umask 077; . /etc/boat/env; [ -n "$BOAT_API_KEY" ] && printf %s "$BOAT_API_KEY" >/root/r16-old-self-token'
sa_operator() {
  curl -fsS --max-time 120 -X "$1" "$SA_URL$2" -H "Authorization: Bearer $sa_key" \
    -H 'content-type: application/json' ${3:+-d "$3"}
}
sa_old_rejected() { # VM holding the captured token; always target the parent
  local status
  # shellcheck disable=SC2016 # command substitution and token expansion are guest-only
  status=$(bx "$1" 'test -s /root/r16-old-self-token && curl -sS --max-time 5 -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $(cat /root/r16-old-self-token)" '"'$SA_URL/v1/vms/$sa'" ) || fail "self api: captured-token probe failed"
  [ "$status" = 401 ] || fail "self api: captured parent token from $1 returned HTTP $status"
}
sa_identity() { # VM; read its own identity using its current managed credential
  local identity
  # shellcheck disable=SC2016
  identity=$(bx "$1" '. /etc/boat/env; [ "$BOAT_ID" = '"'$1'"' ] && curl -fsS --max-time 5 -H "Authorization: Bearer $BOAT_API_KEY" "$BOAT_URL/v1/vms/$BOAT_ID"' | jq -r .id) || return 1
  [ "$identity" = "$1" ]
}
sa_parent_refreshed() {
  local _
  # The refresh reads the real guest env through get_env, then replaces only
  # managed credentials. Bound the asynchronous push and verify custom data too.
  for _ in $(seq 30); do
    # shellcheck disable=SC2016
    if bx "$sa" '. /etc/boat/env; [ "$KEEP_ME" = "r16 custom value" ] && [ "$BOAT_ID" = '"'$sa'"' ] && [ -n "$BOAT_API_KEY" ] && [ "$BOAT_API_KEY" != "$(cat /root/r16-old-self-token)" ]' && sa_identity "$sa"; then
      return
    fi
    sleep 1
  done
  fail "self api: parent env/identity not refreshed after copy"
}

log "host e2e: copied self token isolation (fork)"
bx "$sa" "$sa_save_token" || fail "self api: save pre-fork token"
sa_child=$(sa_operator POST "/v1/vms/$sa/fork" '{"name":"sa-child"}' | jq -r .id)
[ -n "$sa_child" ] && [ "$sa_child" != null ] || fail "self api: fork"
sa_identity "$sa_child" || fail "self api: fork identity"
sa_old_rejected "$sa_child"
sa_old_rejected "$sa"
sa_parent_refreshed
sa_operator DELETE "/v1/vms/$sa_child" >/dev/null

log "host e2e: copied self token isolation (saved snapshot / restore)"
bx "$sa" "$sa_save_token" || fail "self api: save pre-snapshot token"
sa_snapshot=$(sa_operator POST "/v1/vms/$sa/snapshots" '{"name":"sa-saved"}' | jq -r .id)
[ -n "$sa_snapshot" ] && [ "$sa_snapshot" != null ] || fail "self api: saved snapshot"
sa_old_rejected "$sa" # release must already have revoked the captured token
sa_parent_refreshed
sa_restored=$(sa_operator POST /v1/vms "$(jq -nc --arg s "$sa_snapshot" '{name:"sa-restored",snapshot:$s}')" | jq -r .id)
[ -n "$sa_restored" ] && [ "$sa_restored" != null ] || fail "self api: restore saved snapshot"
sa_identity "$sa_restored" || fail "self api: restored identity"
sa_old_rejected "$sa_restored"
sa_parent_refreshed
sa_operator DELETE "/v1/vms/$sa_restored" >/dev/null
sa_operator DELETE "/v1/vms/$sa/snapshots/$sa_snapshot" >/dev/null
echo "copied self tokens: fork and saved snapshot rejected; child identities and parent env/refresh verified"

curl -fsS -X DELETE "$SA_URL/v1/vms/$sa" -H "Authorization: Bearer $sa_key" >/dev/null || fail "self api: delete"
kill "$sa_pid" 2>/dev/null || true
sudo sysctl -qw net.ipv4.ip_unprivileged_port_start="$sa_port_start"
restart_boatd "$WORK/boatd.json"
