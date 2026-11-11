# shellcheck shell=bash
# Fork source, sourced by e2e-host.sh (uses its bd, vmf, newvm, bx): forks of a
# running VM while a long exec runs on it, then another long exec on it. Each
# fork pauses, snapshots and resumes the source through Firecracker's API, and
# Firecracker resets the guest's vsock transport on every snapshot. Both execs
# must end with their own exit status in about their own run time. boatbench
# 42818758694: boatd leaked an API connection per call, the 5th fork's resume
# got 503 "Too many open connections", the source stayed paused and its next
# exec hung until timeout_s+30.
FSRC_FORKS=7 # past Firecracker's 10 API connections at the old 2 per fork
fsrc_exec() { # ID CMD TIMEOUT_S OUT -> OUT (NDJSON), OUT.code (HTTP status); curl gives up at TIMEOUT_S+40
  curl -sS -m "$(($3 + 40))" -o "$4" -w '%{http_code}' --unix-socket "$SOCK" -X POST "http://boatd/v1/vms/$1/exec" \
    -H 'content-type: application/json' \
    -d "$(jq -nc --arg c "$2" --argjson t "$3" '{argv: ["bash", "-c", $c], user: "root", timeout_s: $t}')" \
    >"$4.code" 2>"$4.err" || true
}
fsrc_result() { # OUT -> "http=… exit=… out=…"
  printf 'http=%s exit=%s out=%s' "$(cat "$1.code")" \
    "$(jq -rs 'map(select(has("exit")))[0].exit // "none"' "$1" 2>/dev/null || echo none)" \
    "$(jq -rs 'map(select(.stream == "stdout").data_b64 | @base64d) | join("")' "$1" 2>/dev/null | tr '\n' ' ')"
}
fsrc_fail() { # MSG: boatd's lines for the source, what the guest saw, then fail
  grep -E "$fs" "$WORK/boatd.log" | tail -n 30 || true
  cat "$WORK/tmp/fsrc1" "$WORK/tmp/fsrc1.err" "$WORK/tmp/fsrc2" "$WORK/tmp/fsrc2.err" 2>/dev/null | tail -n 20 || true
  curl -sS -m 30 --unix-socket "$SOCK" -X POST "http://boatd/v1/vms/$fs/exec" -H 'content-type: application/json' \
    -d "$(jq -nc --arg c 'ps -eo pid,etimes,cmd | grep -E "[s]leep|[b]oat-agent"; dmesg | grep -i vsock | tail -5' \
      '{argv: ["bash", "-c", $c], user: "root", timeout_s: 20}')" |
    jq -rs 'map(select(.stream).data_b64 | @base64d) | join("")' 2>&1 || true
  fail "$1"
}
within() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a <= b) }'; } # SECONDS MAX

log "host e2e: fork source ($FSRC_FORKS forks during a long exec, a long exec after them)"
fs=$(newvm '{"name":"fs","team":"iso3"}')
[ "$(vmf "$fs" agent_ready)" = true ] || fail "fs not ready: $(vmf "$fs" error)"
t0=$(now)
fsrc_exec "$fs" 'echo begin; sleep 50; echo end; exit 7' 150 "$WORK/tmp/fsrc1" &
fsrc_pid=$!
sleep 3
for i in $(seq "$FSRC_FORKS"); do
  c=$(bd POST "/v1/vms/$fs/fork" "{\"name\":\"fs$i\"}" | jq -r .id) || fsrc_fail "fork $i of a running VM"
  [ "$(vmf "$c" agent_ready)" = true ] || fsrc_fail "fork $i not ready: $(vmf "$c" error)"
  bd DELETE "/v1/vms/$c" >/dev/null
done
forks_s=$(secs "$t0" "$(now)")
kill -0 "$fsrc_pid" 2>/dev/null && during=yes || during=no # did the forks all land inside the exec?
wait "$fsrc_pid"
e1_s=$(secs "$t0" "$(now)")
r1=$(fsrc_result "$WORK/tmp/fsrc1")
echo "fork source: $FSRC_FORKS forks in ${forks_s}s (all during the exec: $during); exec across them ${e1_s}s: $r1"
t1=$(now)
fsrc_exec "$fs" 'echo begin; sleep 40; echo end; exit 5' 150 "$WORK/tmp/fsrc2"
e2_s=$(secs "$t1" "$(now)")
r2=$(fsrc_result "$WORK/tmp/fsrc2")
echo "fork source: exec after the forks ${e2_s}s: $r2"
[ "$r2" = "http=200 exit=5 out=begin end " ] || fsrc_fail "exec on a fork source after $FSRC_FORKS forks: $r2"
within "$e2_s" 70 || fsrc_fail "exec on a fork source after $FSRC_FORKS forks took ${e2_s}s for a 40 s run"
[ "$r1" = "http=200 exit=7 out=begin end " ] || fsrc_fail "exec across $FSRC_FORKS forks of its VM: $r1"
within "$e1_s" 80 || fsrc_fail "exec across $FSRC_FORKS forks of its VM took ${e1_s}s for a 50 s run"
[ "$during" = yes ] || fsrc_fail "the $FSRC_FORKS forks took ${forks_s}s, past the 50 s exec they were meant to overlap"
bd DELETE "/v1/vms/$fs" >/dev/null
