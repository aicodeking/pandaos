#!/bin/sh
# Fixture test for t3-connect-status. Usage: test-t3-connect-status.sh [script]
# Needs jq. Exits non-zero on the first mismatch.
set -eu
script=${1:-$(dirname "$0")/bin/t3-connect-status}
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
cli=$dir/t3-connect

# fake OUTPUT: a t3-connect whose `status --json` prints OUTPUT.
fake() {
  printf '#!/bin/sh\ncat <<"JSON"\n%s\nJSON\n' "$1" >"$cli"
  chmod +x "$cli"
}

# check NAME EXPECTED-TEXT EXPECTED-CLASS: run the module against $cli.
check() {
  out=$(T3_CONNECT_CMD=$cli sh "$script")
  text=$(printf '%s' "$out" | jq -r .text)
  class=$(printf '%s' "$out" | jq -r .class)
  if [ "$text" != "$2" ] || [ "$class" != "$3" ]; then
    echo "FAIL $1: got '$text'/'$class', want '$2'/'$3'" >&2
    exit 1
  fi
  echo "ok $1"
}

# status JSON with ENABLED SIGNED-IN MODE SERVICE.
st() {
  printf '{"enabled":%s,"signedIn":%s,"mode":"%s","service":"%s","host":"127.0.0.1","port":3773,"version":"0.1.11"}' \
    "$1" "$2" "$3" "$4"
}

check missing-cli "T3: off" off

fake "$(st false false off inactive)"
check off "T3: sign in" off
out=$(T3_CONNECT_CMD=$cli sh "$script")
printf '%s' "$out" | jq -e '.tooltip | contains("0.1.11") and contains("sign in")' >/dev/null ||
  { echo "FAIL tooltip" >&2; exit 1; }

fake "$(st false true off inactive)"
check signed-in-stopped "T3: stopped" off
out=$(T3_CONNECT_CMD=$cli sh "$script")
printf '%s' "$out" | jq -e '.tooltip | contains("0.1.11") and contains("Click to start")' >/dev/null ||
  { echo "FAIL signed-in-stopped tooltip" >&2; exit 1; }

fake "$(st true true connect active)"
check signed-in-active "T3: connected" active

fake "$(st true true publish-only active)"
check publish-only-active "T3: connected" active

fake "$(st true false local active)"
check local-active "T3: local" active

fake "$(st true false local activating)"
check activating "T3: starting" connecting

fake "$(st true true connect failed)"
check failed "T3: error" failed

fake "$(st true false local inactive)"
check stopped "T3: stopped" off

fake "$(st true false connect active)"
check active-not-signed-in "T3: stopped" off

fake 'not json'
check invalid-json "T3: ?" failed

fake '[1,2]'
check non-object "T3: ?" failed

printf '#!/bin/sh\nexit 1\n' >"$cli"
check empty "T3: ?" failed

# Click: a fake foot prints the command it would run in the terminal.
printf '#!/bin/sh\nshift 3\necho "$@"\n' >"$dir/foot"
chmod +x "$dir/foot"
# click NAME EXPECTED-SUBCOMMAND: the last word foot would run.
click() {
  got=$(PATH=$dir:$PATH T3_CONNECT_CMD=$cli sh "$script" click | awk '{print $NF}')
  if [ "$got" != "$2" ]; then
    echo "FAIL click $1: got '$got', want '$2'" >&2
    exit 1
  fi
  echo "ok click $1"
}

fake "$(st false false off inactive)"
click off sign-in
fake "$(st false true off inactive)"
click signed-in-stopped start
fake "$(st true true connect active)"
click signed-in-active logs
fake "$(st true false local inactive)"
click enabled-stopped logs
