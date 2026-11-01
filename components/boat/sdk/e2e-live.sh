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
  boat rm "$vm" --json >/dev/null || fail "rm"
  trap - EXIT
  echo "ok - CLI commands"
fi
echo "e2e-live: ok"
