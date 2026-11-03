#!/usr/bin/env bash
# Live check against a running boat. The Python and TypeScript SDKs each go
# through create, exec, private port URL, detached commands, files, async
# prompts (stub agent), resize, snapshot share, environments, rotation, fork,
# stop and rm, and delete everything again. If `boat` is on PATH, the CLI's new
# commands run too. Called by boat-e2e.yml; also usable by hand:
#
#   BOAT_URL=https://api.boat.example BOAT_KEY=boat_… components/boat/sdk/e2e-live.sh
#
# Env:
#   BOAT_URL                       API base URL (required)
#   BOAT_API_KEY or BOAT_KEY       key with vms:write + exec (required)
#   BOAT_GATEWAY_ADDR=host:port    optional: also fetch the port route through the
#                                  gateway (with and without the route token)
#   BOAT_LIVE_PREFIX               VM name prefix (default sdk-live)
# Needs python3 and node >= 18. At most 3 VMs run at a time; builders stay stopped
# until their environment is deleted. Exit 0 = pass.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
: "${BOAT_URL:?set BOAT_URL}"
BOAT_API_KEY=${BOAT_API_KEY:-${BOAT_KEY:-}}
[ -n "$BOAT_API_KEY" ] || { echo "e2e-live: set BOAT_API_KEY or BOAT_KEY" >&2; exit 2; }
export BOAT_URL BOAT_API_KEY
export BOAT_LIVE_PREFIX=${BOAT_LIVE_PREFIX:-sdk-live}

echo "== python SDK"
python3 "$here/python/examples/live.py"
echo "== typescript SDK"
node "$here/typescript/examples/live.mjs"
if command -v boat >/dev/null 2>&1; then
  echo "== boat CLI"
  fail() { echo "cli live: FAIL: $*" >&2; exit 1; }
  name=$BOAT_LIVE_PREFIX-cli-$$
  vm=$(boat new --name "$name" --no-secrets --json | jq -r .id)
  trap 'boat rm "$vm" --json >/dev/null 2>&1 || true' EXIT
  cid=$(boat exec "$vm" --detach --json -- sleep 300 | jq -r .id)
  [ -n "$cid" ] && [ "$cid" != null ] || fail "exec --detach"
  boat cmd ls "$vm" --json | jq -e --arg c "$cid" 'select(.id == $c)' >/dev/null || fail "cmd ls"
  boat cmd stop "$vm" "$cid" --json >/dev/null || fail "cmd stop"
  boat cmd history "$vm" --json | jq -e --arg c "$cid" 'select(.id == $c)' >/dev/null || fail "cmd history"
  boat files mkdir "$vm" /home/dev/clitest --json >/dev/null || fail "files mkdir"
  boat files ls "$vm" /home/dev --json >/dev/null || fail "files ls"
  boat conversations "$vm" --json >/dev/null || fail "conversations"
  boat env --json >/dev/null || fail "env ls"
  boat ai proxy --json | jq -e 'has("enabled")' >/dev/null || fail "ai proxy"
  boat ai usage --json >/dev/null || fail "ai usage"
  boat telemetry --json | jq -e 'has("configured")' >/dev/null || fail "telemetry"
  boat snapshots --json >/dev/null || fail "snapshots team"
  boat ps "$vm" --json | jq -e 'has("pid")' >/dev/null || fail "ps"
  # The fake boatd of api/livescripts_test.go answers every exec with "hello", so
  # these need a real guest: push, host, ssh-config; current; run deletes its own VM.
  if [ -z "${BOAT_LIVE_FAKE:-}" ]; then
  work=$(mktemp -d)
  mkdir "$work/proj" && echo pushed >"$work/proj/f.txt" && printf '#!/bin/sh\necho ran\n' >"$work/proj/run.sh" && chmod +x "$work/proj/run.sh"
  boat push "$vm" "$work/proj" --json | jq -e '.path == "/home/dev/proj" and .files == 2' >/dev/null || fail "push"
  [ "$(boat exec "$vm" -- /home/dev/proj/run.sh)" = ran ] || fail "push kept the exec bit"
  boat host "$vm" 8123 --json | jq -e '.access == "private" and .isProtected and (.url | length > 0)' >/dev/null || fail "host"
  boat host "$vm" 8123 --public --json | jq -e '.access == "public"' >/dev/null || fail "host --public"
  boat ports rm "$vm" 8123 --json >/dev/null || fail "ports rm"
  boat ssh-config "$vm" --print | grep -q "HostKeyAlias boat-$vm" || fail "ssh-config"
  fi
  boat rm "$vm" --json >/dev/null || fail "rm"
  if [ -z "${BOAT_LIVE_FAKE:-}" ]; then
  # current is per process that calls boat, so no $(...) here: this script is the "shell".
  trap 'boat rm current >/dev/null 2>&1 || true' EXIT
  boat new --name "$name-c" --no-secrets >/dev/null || fail "new for current"
  boat exec current -- true || fail "exec current"
  boat exec -- true || fail "exec with no id uses current"
  boat rm current >/dev/null || fail "rm current"
  trap - EXIT
  before=$(boat list --json | wc -l)
  out=$(cd "$work" && boat run --no-env --push proj -e RUN_VAR=yes -- sh -c 'cat f.txt; echo "$RUN_VAR"; pwd; exit 7' 2>"$work/err") && fail "run should exit 7" || code=$?
  [ "$code" = 7 ] || fail "run exit $code: $(cat "$work/err")"
  [ "$out" = "pushed
yes
/home/dev/proj" ] || fail "run output: $out"
  grep -q '^exit 7 · .* deleted$' "$work/err" || fail "run summary: $(cat "$work/err")"
  [ "$(boat list --json | wc -l)" = "$before" ] || fail "run left a VM behind"
  code=0; boat run --no-env --timeout 1 -- sleep 30 >/dev/null 2>&1 || code=$?
  [ "$code" = 124 ] || fail "run --timeout exit $code"
  rm -rf "$work"
  fi
  echo "ok - CLI commands"
fi
echo "e2e-live: ok"
