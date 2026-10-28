#!/bin/sh
# Fixture test for panda-agents-status. Usage: test-agents-status.sh [script]
# Needs jq. Exits non-zero on the first mismatch.
set -eu
script=${1:-$(dirname "$0")/bin/panda-agents-status}
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT

# check NAME EXPECTED-TEXT EXPECTED-CLASS: run the module against $dir.
check() {
  out=$(PANDA_AGENTD_RUN_DIR=$dir sh "$script")
  text=$(printf '%s' "$out" | jq -r .text)
  class=$(printf '%s' "$out" | jq -r .class)
  if [ "$text" != "$2" ] || [ "$class" != "$3" ]; then
    echo "FAIL $1: got '$text'/'$class', want '$2'/'$3'" >&2
    exit 1
  fi
  echo "ok $1"
}

check missing "Agents: off" off

echo '{"paired":false,"pairing":true,"relay":"r","slots":2,"running":0,"lastError":""}' >"$dir/status.json"
echo 'ABCD-1234' >"$dir/pairing-code"
check pairing "Pairing: ABCD-1234" pairing

rm "$dir/pairing-code"
check pairing-no-code "Pairing: (code unreadable)" pairing

echo '{"paired":true,"pairing":false,"slots":2,"running":0,"lastError":"dial tcp: connection refused","lastErrorAt":"2026-10-28T10:00:00Z"}' >"$dir/status.json"
check relay-down "Relay unreachable" failed
out=$(PANDA_AGENTD_RUN_DIR=$dir sh "$script")
printf '%s' "$out" | jq -e '.tooltip | contains("connection refused")' >/dev/null || { echo "FAIL tooltip" >&2; exit 1; }

echo '{"paired":true,"pairing":false,"slots":4,"running":3,"lastError":""}' >"$dir/status.json"
check running "Agents 3/4" busy

echo '{"paired":true,"slots":2,"running":0}' >"$dir/status.json"
check idle "Agents 0/2" idle

echo '{"paired":false,"pairing":false,"lastError":""}' >"$dir/status.json"
check unpaired "Agents: unpaired" off

echo 'not json' >"$dir/status.json"
check garbage "Agents: ?" failed
