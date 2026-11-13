package main

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

func runPerfShell(t *testing.T, script string) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("performance shell checks require jq")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-eu", "-o", "pipefail", "-c", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("performance shell self-check: %v\n%s", err, out)
	}
}

func TestPerfGatesRejectInvalidMetrics(t *testing.T) {
	runPerfShell(t, `
eval "$(sed -n '/^perf_numbers()/,/^}/p' e2e-host.sh)"
eval "$(sed -n '/^check_max()/,/^}/p' e2e-host.sh)"
eval "$(sed '/^disk_stuck()/,$d' e2e-perf.sh)"
eval "$(sed -n '/^check_mib()/,/^}/p' e2e-perf.sh)"
eval "$(sed -n '/^ratio()/p' e2e-perf.sh)"
errors=0
for gate in check_min check_max check_mib; do
  for invalid in '' null garbage NaN nan Inf -Inf Infinity 1e999 '20junk' '"20000"'; do
    perf_fail=''
    "$gate" metric "$invalid" 15000
    if [ -z "$perf_fail" ]; then echo "$gate accepted invalid metric: $invalid"; errors=$((errors + 1)); fi
    perf_fail=''
    "$gate" metric 15000 "$invalid"
    if [ -z "$perf_fail" ]; then echo "$gate accepted invalid threshold: $invalid"; errors=$((errors + 1)); fi
  done
done
for invalid in '' null garbage NaN Inf 1e999 0; do
  perf_fail=''
  check_min ratio "$(ratio 20000 "$invalid")" 0.15
  if [ -z "$perf_fail" ]; then echo "ratio accepted invalid host metric: $invalid"; errors=$((errors + 1)); fi
done
for invalid in '' null garbage NaN Inf 1e999; do
  perf_fail=''
  check_min ratio "$(ratio "$invalid" 40000)" 0.15
  if [ -z "$perf_fail" ]; then echo "ratio accepted invalid guest metric: $invalid"; errors=$((errors + 1)); fi
done
[ "$errors" = 0 ]
perf_fail=''
check_min min 15000 15000
check_max max 5 5
check_mib mib 700 700
check_min exponent 1.5e4 15000
check_min ratio "$(ratio 20000 40000)" 0.5
check_min ratio_scale "$(ratio 250 500000000 1000000)" 0.5
[ -z "$perf_fail" ]
`)
}

func TestPerfDumpRequiresWriteOverlap(t *testing.T) {
	runPerfShell(t, `
eval "$(sed -n '/^perf_dump_overlap()/,/^}/p' e2e-perf.sh)"
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
check() {
  local want=$1; shift
  if printf '%s\n' "$@" | perf_dump_overlap source 2 10 >"$WORK/result"; then
    [ "$want" = pass ] || { cat "$WORK/result"; echo "invalid overlap accepted"; exit 1; }
    grep -q 'generation=7 .*overlap_ms=8000' "$WORK/result"
  else
    [ "$want" = fail ] || { cat "$WORK/result"; exit 1; }
    grep -q 'invalid dump overlap:' "$WORK/result"
  fi
}
start='2026/11/13 12:00:00 source: dump-write generation=7 phase=start unix_ms=1000'
end='2026/11/13 12:00:01 source: dump-write generation=7 phase=end unix_ms=11000 error=<nil>'
check pass "$start" "$end" 'other: dump-write generation=9 phase=start unix_ms=1'
check fail
check fail "$start"
check fail "$end"
check fail "$end" "$start"
check fail "$start" 'source: dump-write generation=8 phase=end unix_ms=11000 error=<nil>'
check fail "$start" "$end" "$start" "$end"
check fail "$start" "$start" "$end"
check fail "$start" "$end" "$end"
check fail "$start" 'source: dump-write generation=7 phase=end unix_ms=2000 error=<nil>'
check fail 'source: dump-write generation=7 phase=start unix_ms=10000' "$end"
check fail 'source: dump-write generation=7 phase=start unix_ms=11000' "$end"
check fail 'source: dump-write generation=7 phase=start unix_ms=12000' "$end"
check fail "$start" 'source: dump-write generation=7 phase=end unix_ms=invalid error=<nil>'
check fail "$start" "${end% error=*}"
check fail "$start" "${end%<nil>}firecracker: snapshot failed"
check fail "$start" "${end%<nil>}"
check fail "$start" "$end trailing"
check fail "$start error=<nil>" "$end"
check fail 'other: dump-write generation=7 phase=start unix_ms=1000' 'other: dump-write generation=7 phase=end unix_ms=11000 error=<nil>'
`)
}

func TestPerfDumpOverlapConsumesProducerLogs(t *testing.T) {
	for _, result := range []string{"success", "failure"} {
		t.Run(result, func(t *testing.T) {
			logs := captureDumpLogs(t)
			m, h := newTestManager(t, nil)
			v, err := m.Create(context.Background(), CreateReq{})
			if err != nil {
				t.Fatal(err)
			}
			defer m.setMemLimits(v)
			if result == "failure" {
				h.mu.Lock()
				h.failSnap = 1
				h.mu.Unlock()
			}
			if _, _, err := m.dumpSnapshot(context.Background(), v, false); (err != nil) != (result == "failure") {
				t.Fatalf("snapshot %s: %v", result, err)
			}
			// Fake dumps can finish in one millisecond; normalize only timestamps,
			// preserving the producer's complete event spelling and error suffix.
			output := regexp.MustCompile(`(phase=start unix_ms=)[0-9]+`).ReplaceAllString(logs(), "${1}1000")
			output = regexp.MustCompile(`(phase=end unix_ms=)[0-9]+`).ReplaceAllString(output, "${1}11000")
			t.Setenv("DUMP_LOG", output)
			t.Setenv("DUMP_VM", v.ID)
			t.Setenv("DUMP_RESULT", result)
			runPerfShell(t, `
eval "$(sed -n '/^perf_dump_overlap()/,/^}/p' e2e-perf.sh)"
if output=$(printf '%s\n' "$DUMP_LOG" | perf_dump_overlap "$DUMP_VM" 2 10); then
  [ "$DUMP_RESULT" = success ] || { echo "failed dump accepted: $DUMP_LOG"; exit 1; }
  [[ "$output" = *'overlap_ms=8000' ]] || { echo "$output"; exit 1; }
else
  [ "$DUMP_RESULT" = failure ] || { printf '%s\n' "$output" "$DUMP_LOG"; exit 1; }
  [[ "$output" = 'invalid dump overlap:'* ]] || { echo "$output"; exit 1; }
fi
`)
		})
	}
}

func TestPerfDumpDiagnosticSamplerStops(t *testing.T) {
	runPerfShell(t, `
eval "$(sed -n '/^perf_dump_diag()/,/^}/p' e2e-perf.sh)"
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
STATE=$WORK
sudo() { [ "$1" != -n ] || shift; "$@"; }
timeout() { [ "$1 $2 $3" = '-k 1 40' ]; shift 3; "$@"; }
perf_dump_diag source >"$WORK/diag" 2>&1 & pid=$!
for ((i=0; i<100; i+=1)); do
  grep -q '"phase":"sample"' "$WORK/diag" && break
  sleep .05
done
grep -q '"phase":"sample"' "$WORK/diag" || { cat "$WORK/diag"; exit 1; }
touch "$WORK/dump-diag.stop"
wait "$pid"
grep -q '"phase":"after"' "$WORK/diag"
grep -q 'unavailable' "$WORK/diag"
# Exercise the real iteration limit without spending thirty seconds sleeping.
rm "$WORK/dump-diag.stop"
python3() {
  shift 2 # -u -; keep the sampler arguments after Python -c.
  command python3 -c 'import sys; exec(compile(sys.stdin.read().replace("time.sleep(1)", "time.sleep(0)"), "sampler", "exec"))' "$@"
}
perf_dump_diag source >"$WORK/capped" 2>&1
[ "$(grep -c '\"phase\":\"sample\"' "$WORK/capped")" = 30 ]
grep -q '30 sample limit' "$WORK/capped"
`)
}

func TestPerfDumpTrialFailureCleanup(t *testing.T) {
	for _, failure := range []string{"none", "create", "create-sampler", "firstexec", "snapshot", "missing", "stale", "no-overlap", "sampler"} {
		t.Run(failure, func(t *testing.T) {
			runPerfShell(t, strings.ReplaceAll(`
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
export WORK TRIAL_FAILURE=FAILURE_CASE
mkdir "$WORK/tmp"
if bash -eu -o pipefail >"$WORK/result" 2>&1 <<'SH'
eval "$(sed -n '/^perf_dump_overlap()/,/^}/p' e2e-perf.sh)"
eval "$(sed -n '/^perf_dump_trial()/,/^)/p' e2e-perf.sh)"
STATE=$WORK
echo 100 >"$WORK/clock"
# A valid older generation must not rescue a missing new dump interval.
printf '%s\n' 'source: dump-write generation=6 phase=start unix_ms=100000' 'source: dump-write generation=6 phase=end unix_ms=110000 error=<nil>' >"$WORK/boatd.log"
tick() { local n; n=$(cat "$WORK/clock"); echo "$((n+$1))" >"$WORK/clock"; }
now() { cat "$WORK/clock"; }
secs() { echo "$(($2-$1))"; }
sleep() { [ "$1" = 1 ]; tick 1; }
sudo() { "$@"; }
fail() { echo "$1" >&2; exit 1; }
perf_vm() { case "$TRIAL_FAILURE" in create|create-sampler) return 19;; esac; tick 1; echo reader; }
vmf() { echo true; }
last_snap_log() { :; }
perf_del() { :; }
perf_dump_diag() {
  case "$TRIAL_FAILURE" in sampler|create-sampler) echo stopped >"$WORK/diag-stopped"; return 27;; esac
  while [ ! -e "$WORK/dump-diag.stop" ]; do /bin/sleep .01; done
  echo stopped >"$WORK/diag-stopped"
}
bx() {
  if [ "$2" = true ]; then
    tick 7
    [ "$TRIAL_FAILURE" != firstexec ] || return 23
    touch "$WORK/firstexec-ended"
  else
    [ -f "$WORK/firstexec-ended" ] || fail 'guest diagnostic ran before first exec ended'
  fi
}
bd() {
  [ "$1" = POST ] || return 0
  if [ "$TRIAL_FAILURE" != stale ]; then
    echo 'source: dump-write generation=7 phase=start unix_ms=100000' >>"$WORK/boatd.log"
    if [ "$TRIAL_FAILURE" != missing ]; then
      local end=110000
      [ "$TRIAL_FAILURE" != no-overlap ] || end=101000
      echo "source: dump-write generation=7 phase=end unix_ms=$end error=<nil>" >>"$WORK/boatd.log"
    fi
  fi
  [ "$TRIAL_FAILURE" != snapshot ] || return 29
  echo '{"id":"snapshot"}'
}
perf_dump_trial source
[ "$(cat "$WORK/tmp/cdump_s")" = 8 ] || fail 'timer excluded first exec'
SH
then
  case "$TRIAL_FAILURE" in none|sampler) :;; *) cat "$WORK/result"; echo 'failed trial was accepted'; exit 1;; esac
else
  status=$?
  cat "$WORK/result"
  case "$TRIAL_FAILURE" in
    create|create-sampler) [ "$status" = 19 ] || { echo "primary failure masked: $status"; exit 1; };;
    firstexec) grep -q 'create during a dump first exec failed' "$WORK/result";;
    snapshot) grep -q 'snapshot during a create failed' "$WORK/result";;
    missing|stale|no-overlap) grep -q 'create during a dump has no proven write overlap' "$WORK/result";;
    *) exit 1;;
  esac
  [ ! -e "$WORK/tmp/cdump_s" ] || { echo 'failed trial published a metric'; exit 1; }
fi
[ "$(cat "$WORK/diag-stopped")" = stopped ] || { echo 'sampler leaked'; exit 1; }
if [ "$TRIAL_FAILURE" = sampler ]; then grep -q 'sampler unavailable/failed (exit 27)' "$WORK/result"; fi
`, "FAILURE_CASE", failure))
		})
	}
}

func TestPerfLifecycleIncludesFirstExec(t *testing.T) {
	for _, label := range []string{"p", "q"} {
		t.Run(label, func(t *testing.T) {
			script := `
eval "$(sed -n '/^perf_dump_overlap()/,/^}/p' e2e-perf.sh)"
eval "$(sed -n '/^perf_dump_trial()/,/^)/p' e2e-perf.sh)"
eval "$(sed -n '/^perf_snap()/,/^}/p' e2e-perf.sh)"
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
mkdir "$WORK/tmp"
STATE=$WORK
touch "$WORK/boatd.log"
perf_dump_diag() {
  while [ ! -e "$WORK/dump-diag.stop" ]; do /bin/sleep .01; done
  echo stopped >"$WORK/diag-stopped"
}
echo 0 >"$WORK/clock"
tick() { local n; n=$(cat "$WORK/clock"); echo "$((n + $1))" >"$WORK/clock"; }
now() { cat "$WORK/clock"; }
secs() { echo "$(($2 - $1))"; }
perf_vm() { tick 1; echo "vm-$1"; }
put_disk() { :; }
perf_dirty() { :; }
vm_extents() { echo 1; }
last_snap_log() { :; }
perf_del() { :; }
vmf() { echo true; }
awk() { if [ "${@: -1}" != /proc/meminfo ]; then command awk "$@"; fi; }
sudo() { case "$1" in sync|filefrag) :;; *) "$@";; esac; }
sleep() { tick "$1"; }
fail() { echo "$1" >&2; exit 1; }
bd() {
  # The background dump must not race lifecycle calls writing the stub clock.
  if [ "${3:-}" = '{"name":"y"}' ]; then
    echo 'vm-sp: dump-write generation=7 phase=start unix_ms=33000' >>"$WORK/boatd.log"
    for ((i=0; i<200; i+=1)); do
      [ -f "$WORK/exec-vm-cd" ] && break
      /bin/sleep .01
    done
    echo 'vm-sp: dump-write generation=7 phase=end unix_ms=43000 error=<nil>' >>"$WORK/boatd.log"
    echo '{"id":"snapshot"}'; return
  fi
  tick 1
  case "$2" in
    /v1/vms) echo '{"id":"restored"}';;
    */fork) local n=0; [ ! -f "$WORK/forks" ] || n=$(cat "$WORK/forks"); echo "$((n+1))" >"$WORK/forks"; echo "{\"id\":\"fork$n\"}";;
    *) echo '{"id":"snapshot"}';;
  esac
}
bx() {
  if [ ! -f "$WORK/exec-$1" ]; then tick 7; touch "$WORK/exec-$1"; fi
  case "$2" in *'wc -l'*) echo 1;; esac
}
perf_snap LABEL
if [ LABEL = p ]; then [ "$(cat "$WORK/diag-stopped")" = stopped ]; fi
for metric in fromsnap_s fork_s fork2_s EXTRA; do
  value=${!metric}
  [ "$value" = 8 ] || fail "$metric=$value: first successful exec delay was not counted"
done
`
			script = strings.ReplaceAll(script, "LABEL", label)
			extra := ""
			if label == "p" {
				extra = "cdump_s"
			}
			runPerfShell(t, strings.ReplaceAll(script, "EXTRA", extra))
		})
	}
}

func TestDefaultLifecycleIncludesFirstExec(t *testing.T) {
	for _, failure := range []string{"none", "create", "resume", "fork"} {
		t.Run(failure, func(t *testing.T) {
			runPerfShell(t, strings.ReplaceAll(`
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
export WORK
export EXEC_FAILURE=FAILURE_CASE
if bash -eu -o pipefail >"$WORK/result" 2>&1 <<'SH'
eval "$(sed -n '/^perf_numbers()/,/^}/p' e2e-host.sh)"
eval "$(sed -n '/^check_max()/,/^}/p' e2e-host.sh)"
eval "$(sed -n '/^newvm()/p' e2e-host.sh)"
echo 0 >"$WORK/clock"
touch "$WORK/boatd.log"
tick() { local n; n=$(cat "$WORK/clock"); echo "$((n + $1))" >"$WORK/clock"; }
now() { cat "$WORK/clock"; }
secs() { echo "$(($2 - $1))"; }
log() { :; }
fail() { echo "$1" >&2; exit 1; }
vmf() { case "$2" in vcpu) echo 4;; mem_mib) echo 8192;; *) echo true;; esac; }
bd() {
  tick 1
  case "$2" in
    /v1/vms) echo create >"$WORK/pending-d1"; echo '{"id":"d1"}';;
    */start) echo resume >"$WORK/pending-d1";;
    */fork) echo fork >"$WORK/pending-d2"; echo '{"id":"d2"}';;
  esac
}
bx() {
  if [ -f "$WORK/pending-$1" ]; then
    tick 7
    if [ "$2" = true ] && [ "$(cat "$WORK/pending-$1")" = "$EXEC_FAILURE" ]; then return 1; fi
    rm "$WORK/pending-$1"
  fi
  case "$2" in *sha256sum*) echo checksum;; esac
}
perf_fail=''
E2E_DEFAULT_CREATE_MAX=5 RESUME_MAX=5 FORK_MAX=4
eval "$(sed -n '/^DEFAULT_CREATE_MAX=/,/^# Nested KVM spike:/p' e2e-limits.sh)"
errors=0
for metric in dcreate_s dresume_s dfork_s; do
  value=${!metric}
  if [ "$value" != 8 ]; then echo "$metric=$value: first successful exec delay was not counted"; errors=$((errors + 1)); fi
done
[ "$errors" = 0 ] || fail "default lifecycle timers missed guest readiness"
for gate in default_create default_resume default_fork; do
  [[ "$perf_fail" = *"$gate took 8s > 5s; "* ]] || fail "$gate did not reject delayed first exec: $perf_fail"
done
[[ "$perf_fail" != *default_stop* ]] || fail "stop gates changed: $perf_fail"
SH
then
  [ "$EXEC_FAILURE" = none ] || { cat "$WORK/result"; echo "failed $EXEC_FAILURE exec was accepted"; exit 1; }
else
  cat "$WORK/result"
  [ "$EXEC_FAILURE" != none ] || exit 1
  grep -Fx "default $EXEC_FAILURE first exec failed" "$WORK/result"
fi
`, "FAILURE_CASE", failure))
		})
	}
}
