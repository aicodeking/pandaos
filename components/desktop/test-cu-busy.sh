#!/bin/sh
# Test for panda-cu-busy. Usage: test-cu-busy.sh [script]
# Exits non-zero on the first mismatch.
set -eu
script=${1:-$(dirname "$0")/bin/panda-cu-busy}
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT

cu() {
  XDG_RUNTIME_DIR=$dir sh "$script" "$@"
}

# expect NAME WANT: `status` must print WANT (and exit 0 only when busy).
expect() {
  if got=$(cu status); then rc=0; else rc=1; fi
  if [ "$got" != "$2" ] || { [ "$2" = busy ] && [ $rc -ne 0 ]; } ||
    { [ "$2" = idle ] && [ $rc -eq 0 ]; }; then
    echo "FAIL $1: got '$got' (exit $rc), want '$2'" >&2
    exit 1
  fi
  echo "ok $1"
}

expect fresh idle
cu on
expect manual-on busy
cu off
expect manual-off idle

# Busy while CMD runs, idle after; CMD's exit code is passed through.
got=$(cu run sh "$script" status) || true
[ "$got" = busy ] || { echo "FAIL inside-run: got '$got', want 'busy'" >&2; exit 1; }
echo "ok inside-run"
expect after-run idle
if cu run false; then echo "FAIL run-exit: false succeeded" >&2; exit 1; fi
echo "ok run-exit"

# A mark left by a dead run (kill -9) is ignored and cleaned up.
sh -c 'exit 0' &
dead=$!
wait "$dead"
mkdir -p "$dir/panda-cu-busy"
: >"$dir/panda-cu-busy/run.$dead"
expect stale-mark idle
[ ! -e "$dir/panda-cu-busy/run.$dead" ] || { echo "FAIL stale mark kept" >&2; exit 1; }
echo "ok stale-mark-removed"

if cu run 2>/dev/null; then echo "FAIL usage: run without CMD" >&2; exit 1; fi
echo "ok usage"

# The cap: a mark older than PANDA_CU_BUSY_MAX_MIN no longer counts (and a
# manual one is removed); `on` again starts a fresh mark.
cu on
touch -t 202001010000 "$dir/panda-cu-busy/manual"
expect manual-expired idle
[ ! -e "$dir/panda-cu-busy/manual" ] || { echo "FAIL expired manual mark kept" >&2; exit 1; }
echo "ok expired-mark-removed"
cu on
expect manual-fresh busy
cu off
sleep 30 &
live=$!
: >"$dir/panda-cu-busy/run.$live"
touch -t 202001010000 "$dir/panda-cu-busy/run.$live"
expect stuck-run-expired idle
kill "$live"
