# t3code-host-redact CMD...: runs CMD with stdout and stderr filtered so
# pairing secrets never reach the journal: the rest of a `Token: ...` or
# `Pairing URL: ...` line, and any `?token=`/`&token=`/`#token=` value.
# Forwards TERM/INT to CMD and exits with CMD's status; it never waits on the
# filters, so a grandchild holding the pipes open cannot hang the exit.
[ $# -ge 1 ] || {
  echo "t3code-host-redact: usage: t3code-host-redact CMD [ARGS...]" >&2
  exit 2
}

redact() {
  sed -u -E \
    -e 's/(token:[[:space:]]*).*/\1[redacted]/I' \
    -e 's/(pairing url:[[:space:]]*).*/\1[redacted]/I' \
    -e 's/([?&#]token=)[^&#[:space:]"'\'']*/\1[redacted]/Ig'
}

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
if [ "$saw_signal" = 1 ] && { [ "$rc" = 130 ] || [ "$rc" = 143 ]; }; then
  exit 0
fi
exit "$rc"
