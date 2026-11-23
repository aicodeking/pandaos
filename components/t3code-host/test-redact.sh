#!/usr/bin/env bash
# Tests redact-run.sh (t3code-host-redact): what it redacts, what it must keep
# (OAuth device codes, verification URLs) and its exit-status semantics.
#   bash components/t3code-host/test-redact.sh
# Needs GNU sed (as module.nix provides): `sed` on Linux, `gsed` on macOS
# (brew install gnu-sed). With only BSD sed it reports SKIP and exits 0,
# or exits 1 when REQUIRE_GNU_SED=1.
# Child commands are single-quoted on purpose: they expand in the child.
# shellcheck disable=SC2016
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script=$here/redact-run.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

is_gnu_sed() {
  "$1" --version 2>/dev/null | grep -q 'GNU sed'
}

if is_gnu_sed sed; then
  :
elif command -v gsed >/dev/null 2>&1 && is_gnu_sed gsed; then
  mkdir -p "$tmp/bin"
  ln -s "$(command -v gsed)" "$tmp/bin/sed"
  PATH=$tmp/bin:$PATH
else
  echo "test-redact: SKIP: GNU sed not found (macOS: brew install gnu-sed)" >&2
  if [ "${REQUIRE_GNU_SED:-0}" = 1 ]; then exit 1; fi
  exit 0
fi

pass=0
fail=0

ok() {
  pass=$((pass + 1))
  printf 'ok   %s\n' "$1"
}

not_ok() {
  fail=$((fail + 1))
  printf 'FAIL %s\n' "$1"
  shift
  printf '       %s\n' "$@"
}

# As writeShellApplication runs it.
redact_run() {
  bash -euo pipefail "$script" "$@"
}

# Feeds $2 through CMD's stdout (or stderr with $3=stderr) and compares.
check() {
  local name=$1 input=$2 want=$3 stream=${4:-stdout} got
  if [ "$stream" = stderr ]; then
    got=$(INPUT=$input redact_run bash -c 'printf "%s\n" "$INPUT" >&2' 2>&1 >/dev/null)
  else
    got=$(INPUT=$input redact_run bash -c 'printf "%s\n" "$INPUT"' 2>/dev/null)
  fi
  if [ "$got" = "$want" ]; then
    ok "$name"
  else
    not_ok "$name" "input: $(printf '%q' "$input")" "want:  $(printf '%q' "$want")" "got:   $(printf '%q' "$got")"
  fi
}

esc=$'\e'

# --- redacted -----------------------------------------------------------------
check 'plain Token:' 'Token: s3cr3t-abc' 'Token: [redacted]'
check 'lower-case token:' '  token:s3cr3t' '  token:[redacted]'
check 'Pairing URL:' 'Pairing URL: https://app.t3.codes/pair#token=s3cr3t' 'Pairing URL: [redacted]'
check 'ANSI-coloured Token:' "${esc}[1mToken${esc}[0m: ${esc}[32ms3cr3t${esc}[0m" "${esc}[1mToken${esc}[0m: ${esc}[32m[redacted]"
check 'ANSI before colon and value' "Token${esc}[0m:${esc}[1m s3cr3t" "Token${esc}[0m:${esc}[1m [redacted]"
check 'ANSI-coloured Pairing URL:' "${esc}[36mPairing URL:${esc}[0m https://x/?token=s3cr3t" "${esc}[36mPairing URL:${esc}[0m [redacted]"
check 'ANSI inside label gap' "Token ${esc}[2m :${esc}[0m s3cr3t" "Token ${esc}[2m :${esc}[0m [redacted]"
check '?token=' 'open http://127.0.0.1:3773/?token=s3cr3t now' 'open http://127.0.0.1:3773/?token=[redacted] now'
check '&token=' 'https://h/p?a=1&token=s3cr3t&b=2' 'https://h/p?a=1&token=[redacted]&b=2'
check '#token=' 'https://h/pair#token=s3cr3t' 'https://h/pair#token=[redacted]'
check 'bare token= after space' 'pairing ready token=s3cr3t' 'pairing ready token=[redacted]'
check 'bare token= at line start' 'token=s3cr3t' 'token=[redacted]'
check 'access_token=' 'cb?access_token=s3cr3t&x=1' 'cb?access_token=[redacted]&x=1'
check 'quoted URL keeps quote' "url='https://h/?token=s3cr3t' done" "url='https://h/?token=[redacted]' done"
check 'JSON "token"' '{"token":"s3cr3t","ok":true}' '{"token":"[redacted]","ok":true}'
check 'JSON "pairingToken"' '{"pairingToken": "s3cr3t", "ttl": 300}' '{"pairingToken": "[redacted]", "ttl": 300}'
check 'JSON "access_token"' '{"access_token":"a","refresh_token":"b"}' '{"access_token":"[redacted]","refresh_token":"[redacted]"}'
check 'JSON "authToken"' '{"authToken" : "s3cr3t"}' '{"authToken" : "[redacted]"}'
# JSON: any "*token" key.
check 'JSON "refresh_token" alone' '{"refresh_token":"s3cr3t"}' '{"refresh_token":"[redacted]"}'
check 'JSON "sessionToken"' '{"sessionToken":"s3cr3t","id":1}' '{"sessionToken":"[redacted]","id":1}'
check 'JSON "bearerToken"' '{"bearerToken": "s3cr3t"}' '{"bearerToken": "[redacted]"}'
check 'JSON "refreshToken" camelCase' '{"refreshToken":"s3cr3t"}' '{"refreshToken":"[redacted]"}'
check 'JSON "TOKEN" upper case' '{"TOKEN":"s3cr3t"}' '{"TOKEN":"[redacted]"}'
# Labels with ANSI or extra spaces.
check 'Pairing URL: ANSI between words' "Pairing${esc}[0m URL: https://h/pair/s3cr3t" "Pairing${esc}[0m URL: [redacted]"
check 'Pairing URL: two spaces' 'Pairing  URL: https://h/?token=s3cr3t' 'Pairing  URL: [redacted]'
check 'Pairing URL: ANSI-only gap' "Pairing${esc}[1mURL${esc}[0m: s3cr3t" "Pairing${esc}[1mURL${esc}[0m: [redacted]"
check 'PAIRING URL: upper case' 'PAIRING URL: s3cr3t' 'PAIRING URL: [redacted]'
check 'Token  : spaces before colon' 'Token  :  s3cr3t' 'Token  :  [redacted]'
# Authorization: Bearer.
check 'Authorization: Bearer' 'Authorization: Bearer s3cr3t' 'Authorization: Bearer [redacted]'
check 'authorization: bearer lower case' 'authorization:bearer  s3cr3t rest' 'authorization:bearer  [redacted] rest'
check 'Authorization Bearer with ANSI' "Authorization${esc}[0m : ${esc}[1mBearer${esc}[0m s3cr3t" "Authorization${esc}[0m : ${esc}[1mBearer${esc}[0m [redacted]"
check 'curl -H Authorization: Bearer' "curl -H 'Authorization: Bearer s3cr3t' https://h/" "curl -H 'Authorization: Bearer [redacted]' https://h/"
# *token= anywhere a key can start.
check '?pairingToken=' 'https://h/?pairingToken=s3cr3t&x=1' 'https://h/?pairingToken=[redacted]&x=1'
check '&authToken=' 'https://h/?a=1&authToken=s3cr3t' 'https://h/?a=1&authToken=[redacted]'
check '#refresh_token=' 'https://h/cb#refresh_token=s3cr3t' 'https://h/cb#refresh_token=[redacted]'
check 'token = X with spaces' 'token = s3cr3t' 'token = [redacted]'
check 'TOKEN=X upper case' 'env TOKEN=s3cr3t ok' 'env TOKEN=[redacted] ok'
check 'access_token= at line start' 'access_token=s3cr3t' 'access_token=[redacted]'
check 'sessionToken= after space' 'set sessionToken=s3cr3t;next' 'set sessionToken=[redacted];next'
check 'two tokens on one line' 'a?token=one&authToken=two' 'a?token=[redacted]&authToken=[redacted]'
# /pair/<token> path segment.
check '/pair/<token>' 'open https://app.t3.codes/pair/s3cr3t now' 'open https://app.t3.codes/pair/[redacted] now'
check '/pair/<token>?query' 'https://h/pair/s3cr3t?x=1' 'https://h/pair/[redacted]?x=1'
check '/pair/<token>/rest' 'https://h/pair/s3cr3t/ok' 'https://h/pair/[redacted]/ok'
check 'quoted /pair/<token>' '"https://h/pair/s3cr3t"' '"https://h/pair/[redacted]"'
check 'stderr Authorization: Bearer' 'Authorization: Bearer s3cr3t' 'Authorization: Bearer [redacted]' stderr
check 'stderr /pair/<token>' 'https://h/pair/s3cr3t' 'https://h/pair/[redacted]' stderr
check 'stderr Token:' 'Token: s3cr3t' 'Token: [redacted]' stderr
check 'stderr ?token=' 'see https://h/?token=s3cr3t' 'see https://h/?token=[redacted]' stderr

# --- kept ---------------------------------------------------------------------
check 'device code kept' 'Code: ABCD-1234' 'Code: ABCD-1234'
check 'device code line kept' 'Open https://app.t3.codes/device and enter Code: ABCD-1234' 'Open https://app.t3.codes/device and enter Code: ABCD-1234'
check 'verification URL kept' 'https://app.t3.codes/device' 'https://app.t3.codes/device'
check 'user_code URL kept' 'https://app.t3.codes/device?user_code=ABCD-1234' 'https://app.t3.codes/device?user_code=ABCD-1234'
check 'plain log kept' 'Listening on http://127.0.0.1:3773' 'Listening on http://127.0.0.1:3773'
check 'stderr device code kept' 'Code: WXYZ-9876' 'Code: WXYZ-9876' stderr
check 'ANSI device code kept' "${esc}[1mCode:${esc}[0m ABCD-1234" "${esc}[1mCode:${esc}[0m ABCD-1234"
check 'tokens count kept' 'used 1200 tokens: ok' 'used 1200 tokens: ok'
check 'tokenizer= kept' 'tokenizer=cl100k' 'tokenizer=cl100k'
check 'pair route without token kept' 'GET /pair 200' 'GET /pair 200'
check 'Bearer without Authorization kept' 'auth scheme: Bearer' 'auth scheme: Bearer'
check 'JSON non-token key kept' '{"tokenCount":3,"user_code":"ABCD-1234"}' '{"tokenCount":3,"user_code":"ABCD-1234"}'

# --- exit status --------------------------------------------------------------
# CMD: a bash that exits $1 on TERM after touching $2 (ready).
child_cmd='trap "exit $1" TERM; : >"$2"; while :; do sleep 0.05; done'

# Starts redact-run on the child, sends TERM once the child is ready and
# prints redact-run's exit status.
signalled_status() {
  local child_rc=$1 ready=$tmp/ready.$1.$RANDOM pid i rc=0
  shift
  env "$@" bash -euo pipefail "$script" bash -c "$child_cmd" child "$child_rc" "$ready" >/dev/null 2>&1 &
  pid=$!
  for ((i = 0; i < 200; i++)); do
    [ -e "$ready" ] && break
    sleep 0.05
  done
  sleep 0.2 # redact-run's own trap is in place by now
  kill -TERM "$pid"
  wait "$pid" || rc=$?
  echo "$rc"
}

check_status() {
  local name=$1 want=$2 got=$3
  if [ "$got" = "$want" ]; then
    ok "$name"
  else
    not_ok "$name" "want exit $want, got $got"
  fi
}

rc=0
redact_run bash -c 'exit 130' >/dev/null 2>&1 || rc=$?
check_status 'unrequested exit 130 -> 130' 130 "$rc"
rc=0
redact_run bash -c 'exit 143' >/dev/null 2>&1 || rc=$?
check_status 'unrequested exit 143 -> 143' 143 "$rc"
rc=0
redact_run bash -c 'exit 0' >/dev/null 2>&1 || rc=$?
check_status 'exit 0 -> 0' 0 "$rc"
rc=0
redact_run >/dev/null 2>&1 || rc=$?
check_status 'no CMD -> usage, 2' 2 "$rc"
check_status 'TERM + child exit 130 -> 0' 0 "$(signalled_status 130)"
check_status 'TERM + child exit 143 -> 0' 0 "$(signalled_status 143)"
check_status 'TERM + child exit 1 -> 1' 1 "$(signalled_status 1)"
check_status 'TERM + child exit 0 -> 0' 0 "$(signalled_status 0)"
check_status 'SIGNAL_OK=0: TERM + child exit 130 -> 130' 130 \
  "$(signalled_status 130 T3CODE_HOST_REDACT_SIGNAL_OK=0)"
check_status 'SIGNAL_OK=0: TERM + child exit 143 -> 143' 143 \
  "$(signalled_status 143 T3CODE_HOST_REDACT_SIGNAL_OK=0)"

# --- KillMode=control-group ---------------------------------------------------
# systemd's default KillMode=control-group sends SIGTERM to every process in
# the unit at once: redact-run, CMD, CMD's descendants (agent CLIs) and the
# sed filters. The filters must ignore it and still redact CMD's shutdown
# output; the grandchild must get a graceful TERM.
# CMD: starts a grandchild that touches $1/grandchild-term on TERM; on TERM
# prints a secret and goodbye, waits for the grandchild and exits 130.
cgroup_grandchild='trap ": >\"$1/grandchild-term\"; exit 0" TERM; while :; do sleep 0.05; done'
cgroup_child='
  bash -c "$2" grandchild "$1" &
  gc=$!
  printf "%s\n" "$gc" >"$1/grandchild.pid"
  trap "trap \"\" TERM; echo \"Token: shutdown-secret\"; echo goodbye; wait $gc; exit 130" TERM
  : >"$1/cg-ready"
  while :; do sleep 0.05; done'

# Every descendant of $1, as systemd would find them in the unit's cgroup.
descendants() {
  local c
  for c in $(pgrep -P "$1"); do
    echo "$c"
    descendants "$c"
  done
}

any_alive() {
  local p
  for p in "$@"; do
    if kill -0 "$p" 2>/dev/null; then return 0; fi
  done
  return 1
}

control_group_term() {
  local pid i rc=0 pids=()
  bash -euo pipefail "$script" bash -c "$cgroup_child" child "$tmp" "$cgroup_grandchild" \
    >"$tmp/cg-out" 2>/dev/null &
  pid=$!
  for ((i = 0; i < 200; i++)); do
    [ -e "$tmp/cg-ready" ] && break
    sleep 0.05
  done
  sleep 0.2 # redact-run's own trap is in place by now
  # Everything below redact-run: CMD, the grandchild and both sed filters.
  while read -r i; do pids+=("$i"); done < <(descendants "$pid")
  kill -TERM "$pid" "${pids[@]}" 2>/dev/null || true
  wait "$pid" || rc=$?
  # The filters exit once CMD and the grandchild have closed the pipes.
  for ((i = 0; i < 100; i++)); do
    any_alive "${pids[@]}" || break
    sleep 0.05
  done
  echo "$rc"
}

check_status 'control-group TERM: redact-run exits 0' 0 "$(control_group_term)"
if [ -e "$tmp/grandchild-term" ]; then
  ok 'control-group TERM: grandchild got graceful TERM'
else
  not_ok 'control-group TERM: grandchild got graceful TERM' "no $tmp/grandchild-term"
fi
cg_out=$(cat "$tmp/cg-out")
if [[ "$cg_out" == *'Token: [redacted]'* ]] && [[ "$cg_out" == *goodbye* ]] &&
  [[ "$cg_out" != *shutdown-secret* ]]; then
  ok 'control-group TERM: filters drain and redact shutdown output'
else
  not_ok 'control-group TERM: filters drain and redact shutdown output' "got: $(printf '%q' "$cg_out")"
fi

echo "test-redact: $pass passed, $fail failed"
[ "$fail" = 0 ]
