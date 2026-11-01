#!/usr/bin/env bash
# Live SDK check against a running boat: the Python and TypeScript SDKs each
# create a VM, exec, add a private port and mint its URL, fork, stop, and
# delete everything again. Called by boat-e2e.yml; also usable by hand:
#
#   BOAT_URL=https://api.boat.example BOAT_KEY=boat_… components/boat/sdk/e2e-live.sh
#
# Env:
#   BOAT_URL                       API base URL (required)
#   BOAT_API_KEY or BOAT_KEY       key with vms:write + exec (required)
#   BOAT_GATEWAY_ADDR=host:port    optional: also fetch the port route through the
#                                  gateway (with and without the route token)
#   BOAT_LIVE_PREFIX               VM name prefix (default sdk-live)
# Needs python3 and node >= 18. At most 2 VMs exist at a time. Exit 0 = pass.
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
echo "e2e-live: ok"
