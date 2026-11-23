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

# tooltip NAME JQ-CONDITION: the module's tooltip satisfies the condition.
tooltip() {
  out=$(T3_CONNECT_CMD=$cli sh "$script")
  printf '%s' "$out" | jq -e ".tooltip | $2" >/dev/null ||
    { echo "FAIL $1 tooltip: $out" >&2; exit 1; }
  echo "ok $1 tooltip"
}

# status JSON with ENABLED SIGNED-IN MODE SERVICE [SIGNING-IN (default false)].
st() {
  printf '{"enabled":%s,"signedIn":%s,"signingIn":%s,"mode":"%s","service":"%s","host":"127.0.0.1","port":3773,"version":"0.1.11"}' \
    "$1" "$2" "${5:-false}" "$3" "$4"
}

check missing-cli "T3: off" off

fake "$(st false false off inactive)"
check off "T3: sign in" off
tooltip off 'contains("0.1.11") and contains("sign in")'

fake "$(st false true off inactive)"
check signed-in-stopped "T3: stopped" off
tooltip signed-in-stopped 'contains("0.1.11") and contains("Click to start")'

# A sign-in stops the host (enabled may be false): signingIn wins.
fake "$(st false false off inactive true)"
check signing-in-from-off "T3: signing in" connecting
tooltip signing-in-from-off 'contains("device code")'

fake "$(st true false local inactive true)"
check signing-in-from-local "T3: signing in" connecting

fake "$(st true true connect active)"
check signed-in-active "T3: connected" active
tooltip signed-in-active 'contains("mode: connect") and (contains("no relay") | not)'

fake "$(st true true publish-only active)"
check publish-only-active "T3: connected" active
tooltip publish-only-active 'contains("no relay") and contains("127.0.0.1:3773")'

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

# Click: a fake foot prints the command it would run in the terminal
# (everything after `--hold --title TITLE`).
printf '#!/bin/sh\nshift 3\necho "$@"\n' >"$dir/foot"
chmod +x "$dir/foot"
# click NAME EXPECTED-ARGS: the t3-connect arguments foot would run.
click() {
  got=$(PATH=$dir:$PATH T3_CONNECT_CMD=$cli sh "$script" click)
  if [ "$got" != "$cli $2" ]; then
    echo "FAIL click $1: got '$got', want '$cli $2'" >&2
    exit 1
  fi
  echo "ok click $1"
}

fake "$(st false false off inactive)"
click off sign-in
fake "$(st false true off inactive)"
click signed-in-stopped start
fake "$(st false false off inactive true)"
click signing-in-from-off "logs --sign-in -b -o cat -f"
fake "$(st true false local inactive true)"
click signing-in-from-local "logs --sign-in -b -o cat -f"
fake "$(st true true connect active)"
click signed-in-active "logs -b -e"
fake "$(st true false local inactive)"
click enabled-stopped "logs -b -e"
fake 'not json'
click invalid-json sign-in
