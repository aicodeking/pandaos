#!/usr/bin/env bash
# Dashboard smoke test: loads the page in headless Chromium, signs in, lists a VM.
#
#   BOAT_URL=https://api.example.com BOAT_KEY=boat_... ./e2e.sh
#
# Optional: CHROME=/path/to/chrome, BOAT_INSECURE=1 for a self-signed API cert.
# With no browser or Node < 22 it prints SKIP and exits 0, unless
# BOAT_E2E_REQUIRE_BROWSER=1, when it fails instead. It creates one VM and deletes it.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"
: "${BOAT_URL:?set BOAT_URL}"
: "${BOAT_KEY:?set BOAT_KEY}"

skip() {
	if [ "${BOAT_E2E_REQUIRE_BROWSER:-0}" = 1 ]; then
		echo "FAIL dashboard e2e: $1" >&2
		exit 1
	fi
	echo "SKIP dashboard e2e: $1"
	exit 0
}

command -v node >/dev/null || skip "node not found"
node -e 'process.exit(typeof WebSocket === "function" ? 0 : 1)' || skip "Node 22+ is needed for its built-in WebSocket"

chrome=$(node --input-type=module -e 'import { findChrome } from "./testlib/harness.mjs"; console.log(findChrome() ?? "")')
[ -n "$chrome" ] || skip "no Chromium or Chrome found"

CHROME_PATH="$chrome" exec node testlib/smoke.mjs
