#!/bin/sh
# Fixture test for panda-dnd, with a fake fnottctl. Usage: test-dnd.sh [script]
# Exits non-zero on the first mismatch.
set -eu
script=${1:-$(dirname "$0")/bin/panda-dnd}
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
mkdir "$dir/bin" "$dir/run"
# shellcheck disable=SC2016 # $1 expands in the fake fnottctl
printf '#!/bin/sh\necho "$1" >>"%s/calls"\n' "$dir" >"$dir/bin/fnottctl"
chmod +x "$dir/bin/fnottctl"

dnd() {
  PATH=$dir/bin:$PATH XDG_RUNTIME_DIR=$dir/run sh "$script" "$@"
}

# expect NAME WANT GOT
expect() {
  [ "$3" = "$2" ] || { echo "FAIL $1: got '$3', want '$2'" >&2; exit 1; }
  echo "ok $1"
}

expect default-off off "$(dnd status)"
expect bar-hidden "" "$(dnd bar)"
dnd on
expect on on "$(dnd status)"
expect bar-shown DND "$(dnd bar)"
dnd toggle
expect toggle-off off "$(dnd status)"
dnd toggle
expect toggle-on on "$(dnd status)"
dnd off
expect off off "$(dnd status)"
expect fnottctl-calls "pause unpause pause unpause" "$(tr '\n' ' ' <"$dir/calls" | sed 's/ $//')"

# fnott not running: fnottctl fails, so the state must not change.
printf '#!/bin/sh\nexit 1\n' >"$dir/bin/fnottctl"
if dnd on 2>/dev/null; then echo "FAIL no-fnott: on succeeded" >&2; exit 1; fi
expect no-fnott off "$(dnd status)"
if dnd bogus 2>/dev/null; then echo "FAIL usage: bogus accepted" >&2; exit 1; fi
echo "ok usage"
