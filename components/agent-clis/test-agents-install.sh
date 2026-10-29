#!/usr/bin/env bash
# Unit test for panda-agents-install, against a temp dir and a fake updater.
# Usage: test-agents-install.sh [script]. Exits non-zero on the first failure.
set -euo pipefail
script=${1:-$(dirname "$0")/panda-agents-install.sh}
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
mkdir "$t/etc" "$t/bin"
touch "$t/etc/flake.nix"
echo '{ imports = [ ./disk.nix ] ++ lib.optional (builtins.pathExists ./agents.nix) ./agents.nix; }' >"$t/etc/local.nix"
# Fake updater: logs each call; fails while $t/fail exists.
cat >"$t/update" <<'UP'
#!/bin/sh
echo run >>"$(dirname "$0")/calls"
[ ! -e "$(dirname "$0")/fail" ]
UP
chmod +x "$t/update"
export PANDA_AGENTS_DIR=$t/etc PANDA_AGENTS_BIN=$t/bin PANDA_AGENTS_UPDATE=$t/update
export PANDA_AGENTS_KNOWN="claude-code:claude codex:codex gemini-cli:gemini opencode:opencode"
export PANDA_AGENTS_UNPACKAGED=kimi

run() { bash "$script" "$@" >"$t/out" 2>&1; }
calls() { cat "$t/calls" 2>/dev/null | wc -l | tr -d ' '; }
fail() {
  echo "FAIL $1" >&2
  cat "$t/out" >&2
  exit 1
}
expect() { # NAME WANT-RC CMD...
  local name=$1 want=$2 rc=0
  shift 2
  run "$@" || rc=$?
  [ "$rc" = "$want" ] || fail "$name: exit $rc, want $want"
  echo "ok $name"
}

expect no-args 2
expect unknown 2 codex nope
grep -q "unknown CLI 'nope'" "$t/out" || fail "unknown: message"
expect unpackaged 2 kimi
grep -q "not packaged" "$t/out" || fail "unpackaged: message"
# shellcheck disable=SC2016 # literal on purpose
expect injection 2 'codex]' '$(touch '"$t"'/pwned)' '../x'
[ ! -e "$t/pwned" ] || fail "injection: command ran"
expect check-ok 0 --check codex claude-code
expect check-bad 2 --check codex nope
[ ! -e "$t/etc/agents.nix" ] && [ "$(calls)" = 0 ] || fail "validation wrote or applied"

expect install 0 codex
grep -q '\[ "codex" \]' "$t/etc/agents.nix" || fail "install: agents.nix"
[ "$(calls)" = 1 ] || fail "install: updater calls $(calls)"

# Same selection but codex not on PATH yet (last apply lost): apply again.
expect reapply 0 codex
[ "$(calls)" = 2 ] || fail "reapply: updater calls $(calls)"
touch "$t/bin/codex" && chmod +x "$t/bin/codex"
expect idempotent 0 codex codex
grep -q "Already installed" "$t/out" && [ "$(calls)" = 2 ] || fail "idempotent: reapplied"

expect merge 0 opencode codex
grep -q '\[ "codex" "opencode" \]' "$t/etc/agents.nix" || fail "merge: $(cat "$t/etc/agents.nix")"

cp "$t/etc/agents.nix" "$t/before"
touch "$t/fail"
expect apply-fails 1 gemini-cli
cmp -s "$t/before" "$t/etc/agents.nix" || fail "apply-fails: agents.nix not restored"
rm "$t/fail"

echo '{ imports = [ ./disk.nix ]; }' >"$t/etc/local.nix"
expect no-import 1 gemini-cli
grep -q "does not import ./agents.nix" "$t/out" || fail "no-import: message"
rm "$t/etc/flake.nix"
expect not-installed 1 codex
grep -q "pandaos.agents.clis" "$t/out" || fail "not-installed: message"
echo "all ok"
