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
check 'stderr Token:' 'Token: s3cr3t' 'Token: [redacted]' stderr
check 'stderr ?token=' 'see https://h/?token=s3cr3t' 'see https://h/?token=[redacted]' stderr

# --- kept ---------------------------------------------------------------------
check 'device code kept' 'Code: ABCD-1234' 'Code: ABCD-1234'
check 'device code line kept' 'Open https://app.t3.codes/device and enter Code: ABCD-1234' 'Open https://app.t3.codes/device and enter Code: ABCD-1234'
check 'verification URL kept' 'https://app.t3.codes/device' 'https://app.t3.codes/device'
check 'user_code URL kept' 'https://app.t3.codes/device?user_code=ABCD-1234' 'https://app.t3.codes/device?user_code=ABCD-1234'
check 'plain log kept' 'Listening on http://127.0.0.1:3773' 'Listening on http://127.0.0.1:3773'
check 'stderr device code kept' 'Code: WXYZ-9876' 'Code: WXYZ-9876' stderr

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

echo "test-redact: $pass passed, $fail failed"
[ "$fail" = 0 ]
