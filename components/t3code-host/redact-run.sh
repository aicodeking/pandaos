# t3code-host-redact CMD...: runs CMD with stdout and stderr filtered so
# pairing secrets never reach the journal: the rest of a `Token: ...` or
# `Pairing URL: ...` line (ANSI colours allowed around the label), any
# `?token=`/`&token=`/`#token=`/` token=` value (also `access_token=` etc.),
# and JSON `"token"`/`"pairingToken"`/`"access_token"`... string values.
# OAuth device codes (`Code: XXXX-XXXX`) and verification URLs pass through.
# Needs GNU sed (`-u`, `\x1b`, the `I` flag).
#
# Forwards TERM/INT to CMD and exits with CMD's status; it never waits on the
# filters, so a grandchild holding the pipes open cannot hang the exit.
# T3CODE_HOST_REDACT_SIGNAL_OK=0 keeps a 130/143 exit after a stop signal
# non-zero (t3code-sign-in: a cancelled sign-in must not look successful).
[ $# -ge 1 ] || {
  echo "t3code-host-redact: usage: t3code-host-redact CMD [ARGS...]" >&2
  exit 2
}

redact() {
  sed -u -E \
    -e 's/("(access|pairing|auth|refresh)?_?token"[[:space:]]*:[[:space:]]*")[^"]*"/\1[redacted]"/Ig' \
    -e 's/((token|pairing url)([[:space:]]|\x1b\[[0-9;]*m)*:([[:space:]]|\x1b\[[0-9;]*m)*).*/\1[redacted]/I' \
    -e 's/([?&#[:space:]]([[:alpha:]]+_)?token=)[^&#[:space:]"'\'']+/\1[redacted]/Ig' \
    -e 's/^(([[:alpha:]]+_)?token=)[^&#[:space:]"'\'']+/\1[redacted]/I'
}

signal_ok=${T3CODE_HOST_REDACT_SIGNAL_OK:-1}

"$@" > >(redact) 2> >(redact >&2) &
child=$!
signalled=0
saw_signal=0
trap 'signalled=1; saw_signal=1; kill -TERM "$child" 2>/dev/null || true' TERM INT

# A trapped signal interrupts `wait`; wait again for CMD's real exit status
# (bash keeps it for an already-reaped child).
while :; do
  rc=0
  wait "$child" || rc=$?
  if [ "$signalled" = 1 ]; then
    signalled=0
    continue
  fi
  break
done
# Effect's NodeRuntime exits 130 (or 143) on graceful SIGTERM/SIGINT shutdown;
# only map that to 0 when we ourselves received the stop signal from systemd so
# `t3-connect stop` leaves the unit inactive rather than failed, while an
# unrequested non-zero exit still propagates and triggers Restart=on-failure.
if [ "$signal_ok" = 1 ] && [ "$saw_signal" = 1 ] && { [ "$rc" = 130 ] || [ "$rc" = 143 ]; }; then
  exit 0
fi
exit "$rc"
