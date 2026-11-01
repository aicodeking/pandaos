#!/usr/bin/env bash
# Live authorization check against a running boat-api (called by
# boat-e2e.yml; also usable by hand). It proves:
#   - a read-only key gets 403 on VM create
#   - scope escalation via key minting or member invites is refused
#   - a member and a team admin/owner (not the operator) cannot create teams
#   - a second team cannot see or act on the first team's VMs (404)
#
#   BOAT_URL=http://127.0.0.1:8740 BOAT_KEY=<operator key> components/boat/api/e2e-auth.sh
#
# Env: BOAT_URL; BOAT_API_KEY or BOAT_KEY = the operator key printed by
# `boat-api admin init`. Creates 1 VM, 1 team and a few keys; deletes them
# on exit. Needs curl and jq. Exit 0 = pass.
set -euo pipefail
: "${BOAT_URL:?set BOAT_URL}"
OP=${BOAT_API_KEY:-${BOAT_KEY:-}}
[ -n "$OP" ] || { echo "e2e-auth: set BOAT_API_KEY or BOAT_KEY (operator key)" >&2; exit 2; }
API=${BOAT_URL%/}
tmp=$(mktemp -d)
vm="" team_b="" ro_id=""
fails=0

# req KEY METHOD PATH [JSON] -> body on stdout, status in $tmp/status
req() {
  local key=$1 method=$2 path=$3 body=${4:-}
  local args=(-sS -o "$tmp/body" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $key")
  [ -z "$body" ] || args+=(-H 'Content-Type: application/json' --data "$body")
  curl "${args[@]}" "$API$path" >"$tmp/status"
  cat "$tmp/body"
}
status() { cat "$tmp/status"; }

# expect CODE WHAT KEY METHOD PATH [JSON]
expect() {
  local want=$1 what=$2
  shift 2
  req "$@" >/dev/null
  if [ "$(status)" = "$want" ]; then
    echo "ok - $what ($want)"
  else
    echo "FAIL - $what: want $want, got $(status): $(head -c 300 "$tmp/body")"
    fails=$((fails + 1))
  fi
}

# must KEY METHOD PATH [JSON]: a setup step that has to succeed
must() {
  local out
  out=$(req "$@")
  case $(status) in 2??) printf '%s' "$out" ;; *)
    echo "e2e-auth: setup $2 $3 failed: $(status) $out" >&2
    exit 1
    ;;
  esac
}

cleanup() {
  set +e
  [ -z "$vm" ] || req "$OP" DELETE "/v1/vms/$vm" >/dev/null
  [ -z "$ro_id" ] || req "$OP" DELETE "/v1/keys/$ro_id" >/dev/null
  [ -z "$team_b" ] || req "$OP" DELETE "/v1/teams/$team_b" >/dev/null
  rm -rf "$tmp"
}
trap cleanup EXIT

me=$(must "$OP" GET /v1/me)
[ "$(jq -r .operator <<<"$me")" = true ] || { echo "e2e-auth: BOAT_KEY must be the operator key (boat-api admin init)" >&2; exit 2; }
suffix=$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')

echo "== read-only key"
ro=$(must "$OP" POST /v1/keys '{"name":"e2e-ro","scopes":["vms:read"]}')
RO=$(jq -r .token <<<"$ro") ro_id=$(jq -r .id <<<"$ro")
expect 200 "read-only key lists VMs" "$RO" GET /v1/vms
expect 403 "read-only key cannot create a VM" "$RO" POST /v1/vms '{"name":"e2e-ro-vm"}'
expect 403 "read-only key cannot set secrets" "$RO" PUT /v1/secrets/E2E_X '{"value":"x"}'

echo "== scope escalation"
expect 403 "read-only key cannot mint an admin key" "$RO" POST /v1/keys '{"scopes":["admin"]}'
expect 403 "read-only key cannot mint a vms:write key" "$RO" POST /v1/keys '{"scopes":["vms:write"]}'

echo "== tenants"
b=$(must "$OP" POST /v1/teams "{\"name\":\"e2e-auth-$suffix\",\"owner\":\"e2e-b-$suffix\"}")
team_b=$(jq -r .id <<<"$b")
B=$(jq -r .owner.key.token <<<"$b")
expect 403 "team owner (not operator) cannot create teams" "$B" POST /v1/teams '{"name":"e2e-nope"}'
m=$(must "$B" POST "/v1/teams/$team_b/members" '{"name":"e2e-member","role":"member"}')
M=$(jq -r .key.token <<<"$m")
expect 403 "member cannot create teams" "$M" POST /v1/teams '{"name":"e2e-nope"}'
expect 403 "member cannot mint an admin key" "$M" POST /v1/keys '{"scopes":["admin"]}'
expect 403 "member cannot invite users" "$M" POST "/v1/teams/$team_b/members" '{"name":"x","role":"owner"}'
expect 403 "member cannot create webhooks" "$M" POST /v1/webhooks '{"url":"https://example.com/h"}'
adm=$(must "$B" POST "/v1/teams/$team_b/members" '{"name":"e2e-admin","role":"admin"}')
ADM=$(jq -r .key.token <<<"$adm")
expect 403 "team admin cannot add an owner" "$ADM" POST "/v1/teams/$team_b/members" '{"name":"x","role":"owner"}'
expect 403 "team admin cannot create teams" "$ADM" POST /v1/teams '{"name":"e2e-nope"}'
expect 403 "tenant cannot delete teams" "$B" DELETE "/v1/teams/$team_b"
expect 404 "tenant cannot read another team" "$B" GET "/v1/teams/$(jq -r .team.id <<<"$me")"

echo "== VM isolation (one VM in the operator's team)"
v=$(must "$OP" POST /v1/vms "{\"name\":\"e2e-auth-$suffix\",\"secrets\":[]}")
vm=$(jq -r .id <<<"$v")
expect 404 "other team: get" "$B" GET "/v1/vms/$vm"
expect 404 "other team: stop" "$B" POST "/v1/vms/$vm/stop"
expect 404 "other team: start" "$B" POST "/v1/vms/$vm/start"
expect 404 "other team: exec" "$B" POST "/v1/vms/$vm/exec" '{"cmd":["id"]}'
expect 404 "other team: prompt" "$B" POST "/v1/vms/$vm/prompt" '{"prompt":"hi"}'
expect 404 "other team: read file" "$B" GET "/v1/vms/$vm/files?path=/etc/hostname"
expect 404 "other team: fork" "$B" POST "/v1/vms/$vm/fork" '{}'
expect 404 "other team: ssh" "$B" POST "/v1/vms/$vm/ssh" '{}'
expect 404 "other team: add port" "$B" POST "/v1/vms/$vm/ports" '{"port":8080}'
expect 404 "other team: snapshots" "$B" GET "/v1/vms/$vm/snapshots"
expect 404 "other team: delete" "$B" DELETE "/v1/vms/$vm"
req "$B" GET /v1/vms >"$tmp/list"
if jq -e --arg id "$vm" '.vms[] | select(.id == $id)' "$tmp/list" >/dev/null; then
  echo "FAIL - other team's list shows the VM"
  fails=$((fails + 1))
else
  echo "ok - other team's list does not show the VM"
fi
expect 404 "other team: start a command" "$B" POST "/v1/vms/$vm/commands" '{"cmd":["id"]}'
expect 404 "other team: list commands" "$B" GET "/v1/vms/$vm/commands"
expect 404 "other team: command history" "$B" GET "/v1/vms/$vm/command-history"
expect 404 "other team: list files" "$B" GET "/v1/vms/$vm/files/list?path=/"
expect 404 "other team: delete a file" "$B" DELETE "/v1/vms/$vm/files?path=/home/dev/x"
expect 404 "other team: start a prompt" "$B" POST "/v1/vms/$vm/prompts" '{"prompt":"hi"}'
expect 404 "other team: conversations" "$B" GET "/v1/vms/$vm/conversations"
expect 404 "other team: resize" "$B" POST "/v1/vms/$vm/resize" '{"disk_gib":99}'
expect 404 "other team: AI usage" "$B" GET "/v1/vms/$vm/ai-usage"
expect 404 "other team: AI block" "$B" PUT "/v1/vms/$vm/ai-access" '{"enabled":false}'
expect 404 "other team: rotate another team's key" "$B" POST "/v1/keys/$ro_id/rotate"
expect 403 "read-only key cannot start a command" "$RO" POST "/v1/vms/$vm/commands" '{"cmd":["id"]}'
expect 403 "read-only key cannot start a prompt" "$RO" POST "/v1/vms/$vm/prompts" '{"prompt":"hi"}'
expect 403 "read-only key cannot read files" "$RO" GET "/v1/vms/$vm/files/list?path=/"
expect 403 "read-only key cannot set AI credentials" "$RO" PUT /v1/ai/credentials/anthropic '{"value":"sk-12345678"}'
expect 403 "read-only key cannot create environments" "$RO" POST /v1/environments '{"name":"nope"}'
expect 403 "read-only key cannot set telemetry" "$RO" PUT /v1/telemetry '{"endpoint":"https://example.com"}'
expect 403 "member cannot set AI credentials" "$M" PUT /v1/ai/credentials/anthropic '{"value":"sk-12345678"}'
expect 403 "member cannot turn the AI proxy off" "$M" PUT /v1/ai/proxy '{"enabled":false}'
expect 200 "read-only key of the owning team can read it" "$RO" GET "/v1/vms/$vm"
expect 403 "read-only key cannot stop it" "$RO" POST "/v1/vms/$vm/stop"
expect 200 "operator deletes it" "$OP" DELETE "/v1/vms/$vm"
vm=""
expect 200 "operator deletes the empty team" "$OP" DELETE "/v1/teams/$team_b"
expect 401 "keys of the deleted team stop working" "$B" GET /v1/me
team_b=""

if [ "$fails" -gt 0 ]; then
  echo "e2e-auth: $fails check(s) failed"
  exit 1
fi
echo "e2e-auth: ok"
