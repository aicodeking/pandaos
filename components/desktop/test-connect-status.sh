#!/bin/sh
# Fixture test for panda-connect-status. Usage: test-connect-status.sh [script]
# Needs jq. Exits non-zero on the first mismatch.
set -eu
script=${1:-$(dirname "$0")/bin/panda-connect-status}
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
cli=$dir/panda-connect

# fake OUTPUT: a panda-connect whose `status --json` prints OUTPUT.
fake() {
  printf '#!/bin/sh\ncat <<"JSON"\n%s\nJSON\n' "$1" >"$cli"
  chmod +x "$cli"
}

# check NAME EXPECTED-TEXT EXPECTED-CLASS: run the module against $cli.
check() {
  out=$(PANDA_CONNECT_CMD=$cli sh "$script")
  text=$(printf '%s' "$out" | jq -r .text)
  class=$(printf '%s' "$out" | jq -r .class)
  if [ "$text" != "$2" ] || [ "$class" != "$3" ]; then
    echo "FAIL $1: got '$text'/'$class', want '$2'/'$3'" >&2
    exit 1
  fi
  echo "ok $1"
}

check missing-cli "PandaCode: off" off

fake '{"signedIn":false,"service":"inactive","deviceName":"","version":"1.2.3"}'
check signed-out "PandaCode: sign in" off
out=$(PANDA_CONNECT_CMD=$cli sh "$script")
printf '%s' "$out" | jq -e '.tooltip | contains("1.2.3") and contains("sign in")' >/dev/null ||
  { echo "FAIL tooltip" >&2; exit 1; }

fake '{"signedIn":true,"service":"activating","deviceName":"lab","version":"1.2.3"}'
check activating "PandaCode: connecting" connecting

fake '{"signedIn":true,"service":"active","deviceName":"lab-box","version":"1.2.3"}'
check active "PandaCode: lab-box" active

fake '{"signedIn":true,"service":"failed","deviceName":"lab","version":"1.2.3"}'
check failed "PandaCode: error" failed

fake '{"signedIn":true,"service":"inactive","deviceName":"lab","version":"1.2.3"}'
check inactive "PandaCode: stopped" off

fake 'not json'
check garbage "PandaCode: ?" failed

fake '[1,2]'
check non-object "PandaCode: ?" failed

printf '#!/bin/sh\nexit 1\n' >"$cli"
check empty "PandaCode: ?" failed
