package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

func runPerfShell(t *testing.T, script string) {
	t.Helper()
	if _, err := os.Stat("../e2e.sh"); err != nil {
		if errors.Is(err, os.ErrNotExist) && os.Getenv("CI") == "" && os.Getenv("GITHUB_ACTIONS") == "" {
			t.Skip("not in the source tree (nix builds only this directory)")
		}
		t.Fatalf("stat ../e2e.sh: %v", err)
	}
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

func TestPerfDumpDiagnosticReaderSelection(t *testing.T) {
	runPerfShell(t, `
python3 - <<'PY'
import ast, contextlib, io, json, pathlib, re, tempfile
script = pathlib.Path('e2e-perf.sh').read_text()
sampler = re.search(r"^perf_dump_diag\(\).*?<<'PY'\n(.*?)\nPY\n}", script, re.M | re.S).group(1)
ast.parse(sampler, feature_version=(3, 9))
with tempfile.TemporaryDirectory() as tmp:
    root = pathlib.Path(tmp) / 'cgroup'
    parent = root / 'boat-e2e'
    parent.mkdir(parents=True)
    for i in range(20):
        (parent / ('controller-%02d' % i)).touch()
        old = parent / ('old-%02d' % i)
        old.mkdir()
        (old / 'cgroup.procs').write_text('')
    live = parent / 'zz-live-reader'
    live.mkdir()
    (live / 'cgroup.procs').write_text('123456789\n')
    (parent / 'source').mkdir()
    # Execute the sampler's actual functions against a fake cgroup tree.
    setup = sampler.split("mount(state)\n", 1)[0]
    setup = setup.replace("state, work, source = sys.argv[1:4]", "state, work, source = %r, %r, 'source'" % (tmp, tmp))
    setup = setup.replace("root = '/sys/fs/cgroup'", 'root = %r' % str(root))
    ns = {}
    exec(compile(setup, 'sampler', 'exec'), ns)
    def sample():
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            ns['sample']('sample')
        return json.loads(out.getvalue().removeprefix('dump-diag '))['cgroups']
    groups = sample()
    assert 'candidate:zz-live-reader' in groups, groups
    assert len(groups) <= 20, groups  # Four fixed groups plus at most sixteen readers.
    assert not any('controller-' in label for label in groups), groups
    # A new reader must be sampled while perf_vm is still blocked and has no ID.
    new = parent / 'zz-new-reader'
    new.mkdir()
    (new / 'cgroup.procs').write_text('')
    groups = sample()
    assert 'candidate:zz-new-reader' in groups, groups
    # The known reader remains first even when many other new live dirs exist.
    for i in range(20):
        other = parent / ('new-%02d' % i)
        other.mkdir()
        (other / 'cgroup.procs').write_text('123456787\n')
    pathlib.Path(tmp, 'dump-reader').write_text('zz-new-reader\n')
    groups = sample()
    assert 'boot-reader:zz-new-reader' in groups, groups
    assert len(groups) <= 20, groups
PY
`)
}

func TestPerfDumpEventsBoundedAndAllowlisted(t *testing.T) {
	runPerfShell(t, `
eval "$(sed -n '/^perf_dump_events()/,/^}/p' e2e-perf.sh)"
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
{
  echo 'source: dump-cap stale'
  echo 'source: dump-write stale'
  echo 'other: dump-cap dev=7:0 requested="secret"'
  echo 'not-source: dump-write generation=9 phase=end unix_ms=1 error=secret'
  echo 'source: guest secret'
  echo 'source: dump-cap dev=7:0 requested="cap" actual="" write_error=denied read_error=missing'
  echo '2026/11/13 12:00:01 source: dump-write generation=7 phase=end unix_ms=11000 error=failed'
} | perf_dump_events source 2 >"$WORK/events"
[ "$(wc -l <"$WORK/events" | tr -d ' ')" = 2 ]
grep -F 'write_error=denied read_error=missing' "$WORK/events"
grep -F 'error=failed' "$WORK/events"
! grep -Eq 'stale|secret' "$WORK/events"
python3 - <<'PY' >"$WORK/input"
for i in range(100):
    print('source: dump-cap dev=7:0 requested="' + 'x' * 10000 + '"')
PY
perf_dump_events source 0 <"$WORK/input" >"$WORK/bounded"
[ "$(grep -c '^dump-event source: dump-cap ' "$WORK/bounded")" = 32 ]
grep -q 'truncated' "$WORK/bounded"
awk 'length > 8300 {exit 1}' "$WORK/bounded"
`)
}

func TestPerfDumpTrialFailureCleanup(t *testing.T) {
	for _, failure := range []string{"none", "create", "create-sampler", "firstexec", "snapshot", "missing", "stale", "no-overlap", "sampler"} {
		t.Run(failure, func(t *testing.T) {
			runPerfShell(t, strings.ReplaceAll(`
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
export WORK TRIAL_FAILURE=FAILURE_CASE
mkdir "$WORK/tmp"
sed '$d' e2e-perf.sh >"$WORK/e2e-perf.sh"
sed '/^# Lifecycle uses these helpers/,$d' e2e-host.sh >"$WORK/e2e-host.sh"
grep '^set -' ../e2e.sh | head -1 >"$WORK/options.sh"
cat >"$WORK/driver.sh" <<'SH'
. "$WORK/options.sh"
. "$WORK/e2e-host.sh"
. "$WORK/e2e-perf.sh"
STATE=$WORK
echo 100 >"$WORK/clock"
# A valid older generation must not rescue a missing new dump interval.
printf '%s\n' 'source: dump-cap old-secret' 'source: dump-write generation=6 phase=start unix_ms=100000' 'source: dump-write generation=6 phase=end unix_ms=110000 error=<nil>' >"$WORK/boatd.log"
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
  case "$TRIAL_FAILURE" in sampler|create-sampler) echo stopped >"$WORK/diag-stopped"; bash -c 'exit 27';; esac
  while [ ! -e "$WORK/dump-diag.stop" ]; do /bin/sleep .01; done
  echo stopped >"$WORK/diag-stopped"
}
bx() {
  if [ "$2" = true ]; then
    [ "$(cat "$WORK/dump-reader")" = "$1" ] || fail 'reader ID unavailable before first exec'
    tick 7
    [ "$TRIAL_FAILURE" != firstexec ] || return 23
    touch "$WORK/firstexec-ended"
  else
    [ -f "$WORK/firstexec-ended" ] || fail 'guest diagnostic ran before first exec ended'
  fi
}
bd() {
  [ "$1" = POST ] || return 0
  case "$TRIAL_FAILURE" in create|create-sampler) /bin/sleep .05;; esac
  if [ "$TRIAL_FAILURE" != stale ]; then
    echo 'source: dump-cap dev=7:0 requested="cap" actual="" write_error=denied read_error=missing' >>"$WORK/boatd.log"
    echo 'other: dump-cap other-secret' >>"$WORK/boatd.log"
    echo 'source: guest guest-secret' >>"$WORK/boatd.log"
    echo 'source: dump-write generation=7 phase=start unix_ms=100000' >>"$WORK/boatd.log"
    if [ "$TRIAL_FAILURE" != missing ]; then
      local end=110000
      [ "$TRIAL_FAILURE" != no-overlap ] || end=101000
      local error='<nil>'
      [ "$TRIAL_FAILURE" != snapshot ] || error='snapshot-failed'
      echo "source: dump-write generation=7 phase=end unix_ms=$end error=$error" >>"$WORK/boatd.log"
    fi
  fi
  touch "$WORK/dump-ended"
  [ "$TRIAL_FAILURE" != snapshot ] || return 29
  echo '{"id":"snapshot"}'
}
perf_dump_trial source
[ "$(cat "$WORK/tmp/cdump_s")" = 8 ] || fail 'timer excluded first exec'
SH
if bash "$WORK/driver.sh" >"$WORK/result" 2>&1
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
diagnostics=$(grep -c '^e2e: exit ' "$WORK/result" || true)
[ "$diagnostics" -le 1 ] || { cat "$WORK/result"; echo 'duplicate diagnostic'; exit 1; }
case "$TRIAL_FAILURE" in
  none|sampler) [ "$diagnostics" = 0 ] || { cat "$WORK/result"; exit 1; };;
  create|create-sampler) grep -Eq '^e2e: exit 19 at .*:[0-9]+: f=\$\(perf_vm cd\)' "$WORK/result";;
esac
[ "$(cat "$WORK/diag-stopped")" = stopped ] || { echo 'sampler leaked'; exit 1; }
[ -f "$WORK/dump-ended" ] || { echo 'snapshot command leaked'; exit 1; }
! grep -Eq 'old-secret|other-secret|guest-secret|generation=6' "$WORK/result"
if [ "$TRIAL_FAILURE" != stale ]; then
  grep -F 'dump-event source: dump-cap dev=7:0 requested="cap" actual="" write_error=denied read_error=missing' "$WORK/result"
  grep -F 'dump-event source: dump-write generation=7 phase=start' "$WORK/result"
fi
if [ "$TRIAL_FAILURE" = snapshot ]; then grep -F 'error=snapshot-failed' "$WORK/result"; fi
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
eval "$(sed -n '/^perf_dump_events()/,/^}/p' e2e-perf.sh)"
eval "$(sed -n '/^perf_dump_trial()/,/^)/p' e2e-perf.sh)"
eval "$(sed -n '/^perf_boundary()/,/^}/p' e2e-perf.sh)"
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
put_disk() { echo put >>"$WORK/conditioning"; }
perf_dirty() { echo dirty >>"$WORK/conditioning"; }
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
  case "$2" in
    'bash /tmp/disk.sh') echo fio >>"$WORK/conditioning";;
    'head -c 2G /dev/urandom >/dev/shm/d/dirty') echo rewrite >>"$WORK/conditioning";;
  esac
  if [ ! -f "$WORK/exec-$1" ]; then tick 7; touch "$WORK/exec-$1"; fi
  case "$2" in *'wc -l'*) echo 1;; esac
}
perf_snap LABEL
expected=$(printf '%s\n' put fio dirty)
[ LABEL != p ] || expected=$(printf '%s\n' put fio dirty rewrite)
[ "$(cat "$WORK/conditioning")" = "$expected" ] || fail 'snapshot conditioning changed'
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

func TestPerfPhaseDispatch(t *testing.T) {
	runPerfShell(t, `
eval "$(sed -n '/^e2e_dispatch()/,/^}/p' ../e2e.sh)"
e2e_full() { echo full; }
e2e_lifecycle() { echo lifecycle; }
fail() { echo "$*" >&2; exit 1; }
unset E2E_PHASE
[ "$(e2e_dispatch)" = full ]
E2E_PHASE=full; [ "$(e2e_dispatch)" = full ]
E2E_PHASE=lifecycle; [ "$(e2e_dispatch)" = lifecycle ]
E2E_PHASE=typo; ! (e2e_dispatch)
eval "$(sed -n '/^perf_dispatch()/,/^}/p' e2e-perf.sh)"
perf_full() { echo full; }
perf_lifecycle() { echo lifecycle; }
unset E2E_PHASE
[ "$(perf_dispatch)" = full ]
E2E_PHASE=lifecycle; [ "$(perf_dispatch)" = lifecycle ]
E2E_PHASE=typo; ! (perf_dispatch)
`)
}

func TestPerfLifecycleInitializesMirror(t *testing.T) {
	for _, mirror := range []string{"", "http://test.ubuntu.com/ubuntu"} {
		t.Run(mirror, func(t *testing.T) {
			t.Setenv("TEST_MIRROR", mirror)
			runPerfShell(t, `
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
export WORK
mkdir "$WORK/tmp"
sed '$d' e2e-perf.sh >"$WORK/e2e-perf.sh"
if bash -eu -o pipefail >"$WORK/result" 2>&1 <<'SH'
HERE=$(cd .. && pwd)
SOCK=$WORK/socket
E2E_PHASE=lifecycle
unset HOST_MIRROR
fail() { echo "$*" >&2; exit 1; }
log() { :; }
sudo() { [ "$1" = apt-get ]; }
python3() { printf '# prepared disk script\n'; }
grep() {
  if [ "$1" = -rhoE ]; then printf '%s\n' "$TEST_MIRROR"; else command grep "$@"; fi
}
curl() {
  [ "$*" = "-fsS --unix-socket $SOCK -X PUT --data-binary @$WORK/tmp/disk.sh http://boatd/v1/vms/source/files?path=/tmp/disk.sh&mode=755" ]
  grep -q 'prepared disk script' "$WORK/tmp/disk.sh"
  echo upload >>"$WORK/calls"
}
bx() {
  case "$2" in
    sed*) [[ "$2" = *"$TEST_MIRROR"* ]]; echo mirror >>"$WORK/calls";;
    'command -v fio'*) [ "$3" = 600 ]; echo install >>"$WORK/calls";;
    'bash /tmp/disk.sh')
      [ "$HOST_MIRROR" = "$TEST_MIRROR" ]
      echo conditioned >>"$WORK/calls"
      exit 0;;
    *) fail "unexpected guest command: $2";;
  esac
}
# Stop at the real guest fio boundary, after the real prepare/snap/put_disk path.
# Only host/VM operations outside that path are replaced.
. "$WORK/e2e-perf.sh"
perf_boundary() { :; }
perf_ceiling() { :; }
perf_cfg() { :; }
perf_vm() { echo source; }
perf_dispatch
SH
then
  expected=$(printf '%s\n' upload install conditioned)
  [ -z "$TEST_MIRROR" ] || expected=$(printf '%s\n' upload mirror install conditioned)
  [ "$(cat "$WORK/calls")" = "$expected" ] || { cat "$WORK/result" "$WORK/calls"; exit 1; }
else
  cat "$WORK/result"; exit 1
fi
`)
		})
	}
}

func TestPerfSourcedFailureDiagnostics(t *testing.T) {
	for _, phase := range []string{"full", "lifecycle"} {
		t.Run(phase, func(t *testing.T) {
			t.Setenv("TEST_PHASE", phase)
			runPerfShell(t, `
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
export WORK
mkdir "$WORK/tmp" "$WORK/fixture" "$WORK/bin"
python3 - "$WORK/fixture" <<'PY'
import pathlib, re, sys
out = pathlib.Path(sys.argv[1])
# Keep actual definitions and source line numbers; omit only host startup.
source = pathlib.Path('../e2e.sh').read_text()
keep = set()
for pattern in [r'^set -[^\n]*', r'^trap .* ERR$', r'^trap cleanup EXIT$',
                r'^(?:log|fail|cleanup|pilot_cleanup_owned|e2e_full|e2e_lifecycle|e2e_dispatch)\(\) \{.*?^}']:
    for match in re.finditer(pattern, source, re.M | re.S if '.*?' in pattern else re.M):
        keep.update(range(source.count('\n', 0, match.start()), source.count('\n', 0, match.end())+1))
(out/'e2e.sh').write_text(''.join(line if i in keep else '\n' for i,line in enumerate(source.splitlines(True))))
host = pathlib.Path('e2e-host.sh').read_text()
(out/'e2e-host.sh').write_text(host.split('# Lifecycle uses these helpers', 1)[0])
PY
printf '#!/bin/sh\nexit 31\n' >"$WORK/bin/python3"
chmod +x "$WORK/bin/python3"
  phase=$TEST_PHASE
  rm -f "$WORK/cleaned"
  cat >"$WORK/driver.sh" <<'SH'
set -euo pipefail
HERE=$(cd .. && pwd)
export PATH="$WORK/bin:$PATH"
STATE=$WORK/state
E2E_PHASE=$TEST_PHASE
E2E_PILOT=0
HOOK_PORT=9911 API=unused
. "$WORK/fixture/e2e.sh"
PIDS=(2147483647)
# Exercise expected failures and the real cleanup without touching the host.
sudo() { if [ "${1:-}" = apt-get ]; then return 0; fi; echo cleanup >>"$WORK/cleaned"; return 27; }
ip() { return 0; }
sleep() { :; }
mountpoint() { return 1; }
kill() { return 1; }
if [ "$TEST_PHASE" = full ]; then
  . "$WORK/fixture/e2e-host.sh"
  e2e_full() { . "$HERE/boatd/e2e-perf.sh"; }
fi
e2e_dispatch
SH
  if bash "$WORK/driver.sh" >"$WORK/result" 2>&1
  then echo "accepted nested failure: $phase"; exit 1
  else status=$?; fi
  cat "$WORK/result"
  [ "$status" = 31 ] || { echo "primary failure masked: $status"; exit 1; }
  [ -s "$WORK/cleaned" ] || { echo 'cleanup skipped'; exit 1; }
  [ "$(grep -c '^e2e: exit ' "$WORK/result")" = 1 ] || { echo 'missing or duplicate diagnostic'; exit 1; }
  grep -Eq '^e2e: exit 31 at e2e-perf.sh:[0-9]+: python3 -c' "$WORK/result"
`)
		})
	}
}

func TestPerfLifecyclePhase(t *testing.T) {
	for _, failure := range []string{"none", "conditioning", "snapshot", "gate"} {
		t.Run(failure, func(t *testing.T) {
			runPerfShell(t, strings.ReplaceAll(`
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
export WORK
if bash -eu -o pipefail >"$WORK/result" 2>&1 <<'SH'
eval "$(sed -n '/^perf_lifecycle()/,/^}/p' e2e-perf.sh)"
fail() { echo "$*" >&2; exit 1; }
log() { :; }
perf_boundary() { :; }
perf_prepare_disk() { echo prepare >>"$WORK/order"; }
perf_ceiling() { echo ceiling >>"$WORK/order"; c_sw=1; c_rw=2; c_rr=3; }
perf_cfg() { [ "$1 $2" = '. again' ]; echo config >>"$WORK/order"; }
perf_vm() { [ "$1" = pv ]; echo idle >>"$WORK/order"; echo idle; }
perf_snap() {
  [ "$1" = p ]; echo snapshot >>"$WORK/order"
  case FAILURE_CASE in conditioning|snapshot) fail FAILURE_CASE;; esac
  snap_s=1 fromsnap_s=2 fork_s=3 fork2_s=4 cdump_s=5
}
perf_del() { [ "$1" = idle ]; echo delete >>"$WORK/order"; }
sudo() { "$@"; }
touch "$WORK/boatd.log"
check_max() { echo "$1" >>"$WORK/gates"; [ FAILURE_CASE != gate ] || perf_fail=miss; }
perf_fail=''
PERF_SNAPSHOT_MAX=18 PERF_FROM_MAX=5 PERF_FORK_MAX=5 PERF_CREATE_DUMP_MAX=9
perf_lifecycle
[ -z "$perf_fail" ] || fail "lifecycle timing targets: $perf_fail"
SH
then
  [ FAILURE_CASE = none ] || { cat "$WORK/result"; exit 1; }
  [ "$(cat "$WORK/order")" = "$(printf '%s\n' prepare ceiling config idle snapshot delete)" ]
  [ "$(cat "$WORK/gates")" = "$(printf '%s\n' perf_snapshot perf_from_snap perf_fork perf_create_dump)" ]
  grep '^METRIC ' "$WORK/result" | cut -c8- | jq -e '.test == "boat-lifecycle" and .complete == false and .candidate == false and .createDuringDumpS == 5' >/dev/null
else
  [ FAILURE_CASE != none ] || { cat "$WORK/result"; exit 1; }
fi
! grep -Eq 'boat-perf|boat-e2e|e2e: PASS' "$WORK/result"
`, "FAILURE_CASE", failure))
		})
	}
}

func TestPerfLifecycleEvidenceFailures(t *testing.T) {
	runPerfShell(t, `
eval "$(sed -n '/^perf_lifecycle()/,/^}/p' e2e-perf.sh)"
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
perf_boundary() { :; }
perf_prepare_disk() { :; }
perf_ceiling() { :; }
perf_cfg() { :; }
perf_vm() { echo idle; }
perf_snap() { snap_s=1 fromsnap_s=2 fork_s=3 fork2_s=4 cdump_s=5; }
perf_del() { :; }
check_max() { :; }
c_sw=1 c_rw=2 c_rr=3
PERF_SNAPSHOT_MAX=18 PERF_FROM_MAX=5 PERF_FORK_MAX=5 PERF_CREATE_DUMP_MAX=9
sudo() { "$@"; }
fail() { echo "$*" >&2; exit 1; }
for condition in missing fallback; do
  rm -f "$WORK/boatd.log"
  [ "$condition" != fallback ] || echo 'Async disk engine refused' >"$WORK/boatd.log"
  if (perf_lifecycle) >"$WORK/result" 2>&1; then cat "$WORK/result"; echo 'missing/Sync evidence accepted'; exit 1; fi
  ! grep '^METRIC ' "$WORK/result"
done
`)
}

func TestPerfPilotWorkflowCompleteness(t *testing.T) {
	runPerfShell(t, `
python3 - <<'PY'
import json, pathlib, re, subprocess, sys, tempfile, textwrap
workflow = pathlib.Path('../../../.github/workflows/boat-e2e.yml').read_text()
check = textwrap.dedent(re.search(r'cat >"\$archive/check.py" <<\x27PY\x27\n(.*?)\n          PY', workflow, re.S).group(1))
with tempfile.TemporaryDirectory() as tmp:
    root = pathlib.Path(tmp)
    script = root / 'check.py'; script.write_text(check)
    for phase, cap, rc, seconds, want in [('full',150,0,5,True), ('full',150,1,10,True), ('full',50,1,10,False), ('lifecycle',50,0,5,True), ('lifecycle',150,1,10,True), ('lifecycle',50,1,10,False), ('partial',50,0,5,False), ('forged',50,0,5,False), ('no-lead',50,0,5,False), ('after-write',50,0,5,False)]:
        mode = phase if phase in ('full','lifecycle') else 'lifecycle'
        start, end, create, ready = 1000, 6000, 2000, 7000
        if phase == 'no-lead': start = 2500
        stamp = 6500 if phase == 'after-write' else 3000
        metric = dict(test='boat-perf' if mode == 'full' else 'boat-lifecycle', createDuringDumpS=seconds, complete=False, candidate=False)
        lines = ['METRIC '+json.dumps(metric)]
        if mode == 'full' or phase == 'forged': lines += ['METRIC {"test":"boat-e2e"}']
        lines += [f'e2e: perf_create_dump {seconds:.2f}s (max 9s)', 'dump-trial source=vm-source reader=vm-reader create_start_unix_s=2 firstexec_end_unix_s=7', f'dump-write vm=vm-source generation=7 start_unix_ms={start} end_unix_ms={end} create_start_unix_ms={create} firstexec_end_unix_ms={ready} overlap_ms={min(end,ready)-max(start,create)}', f'dump-event vm-source: dump-write generation=7 phase=start unix_ms={start}', f'dump-event vm-source: dump-write generation=7 phase=end unix_ms={end} error=<nil>', f'dump-event vm-source: dump-cap dev=7:0 requested="7:0 wbps={cap<<20}" actual="7:0 wbps={cap<<20}" write_error=<nil> read_error=<nil> capped=true booting=1 dumps=1 generation=7']
        lines += ['dump-diag '+json.dumps(dict(unix_ms=stamp, sample_start_unix_ms=stamp-1, phase='sample', cgroups={'source':{'io.max':f'7:0 wbps={cap<<20}'}}, processes={'boot-reader:vm-reader:123':dict(pid='123',schedstat='1 2 3')}))]
        if rc:
            prefix = '' if mode == 'full' else 'lifecycle '
            lines += [f'e2e FAIL: {prefix}timing targets: perf_create_dump took {seconds:.2f}s > 9s; ']
        elif phase != 'partial': lines += ['e2e: PASS' if mode == 'full' else 'e2e lifecycle: COMPLETE (exploratory; full gates still required)']
        (root / 'e2e.log').write_text('\n'.join(lines)+'\n')
        for name in ('dmesg-before.log','dmesg-after.log'): (root / name).write_text('')
        result = subprocess.run([sys.executable,str(script),str(root),str(cap),str(rc),mode],capture_output=True,text=True)
        assert (result.returncode == 0) == want, (phase,cap,rc,result.stdout,result.stderr)
        if want:
            verdict=json.loads((root/'result.json').read_text())
            assert verdict['candidate'] is False and verdict['phase'] == mode
            assert verdict['status'] == ('pass' if rc == 0 else 'failed-control')
            assert verdict['full_validation_pass'] == (mode == 'full' and rc == 0)
PY
`)
}

func TestPerfPilotEvidenceAllowlist(t *testing.T) {
	runPerfShell(t, `
python3 - <<'PY'
import os, pathlib, re, subprocess, sys, tarfile, tempfile, textwrap
workflow=pathlib.Path('../../../.github/workflows/boat-e2e.yml').read_text()
script=textwrap.dedent(re.search(r'cat >"\$archive/archive-work.py" <<\x27PY\x27\n(.*?)\n          PY',workflow,re.S).group(1))
with tempfile.TemporaryDirectory() as tmp:
    root=pathlib.Path(tmp); work=root/'work'; work.mkdir(); (work/'tmp').mkdir()
    helper=root/'archive.py'; helper.write_text(script)
    (work/'desktop.key').write_text('secret'); (work/'cli.json').write_text('token')
    (work/'boatd.log').write_text('diagnostic'); (work/'tmp/dump-diag.log').write_text('counters')
    def run(): return subprocess.run([sys.executable,str(helper),str(work),str(root)],capture_output=True,timeout=3)
    assert run().returncode == 0
    with tarfile.open(root/'work-evidence.tgz') as bundle: assert set(bundle.getnames()) == {'boatd.log','tmp/dump-diag.log'}
    (work/'boatd.log').unlink(); (work/'boatd.log').symlink_to(work/'desktop.key')
    assert run().returncode != 0, 'symlink archived'
    (work/'boatd.log').unlink(); os.link(work/'desktop.key', work/'boatd.log')
    assert run().returncode != 0, 'hardlink archived'
    (work/'boatd.log').unlink()
    os.mkfifo(work/'boatd.log')
    assert run().returncode != 0, 'special file archived'
PY
`)
}

func TestPerfPilotCleanupRefusesUnknownArtifacts(t *testing.T) {
	runPerfShell(t, `
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
export WORK
python3 - <<'PY' >"$WORK/clean.sh"
import pathlib, re, textwrap
workflow=pathlib.Path('../../../.github/workflows/boat-e2e.yml').read_text()
script=textwrap.dedent(re.search(r'^          clean\(\).*?\n          }',workflow,re.M|re.S).group())
print(script.replace('/sys/fs/cgroup/boat-e2e','"$WORK/cg"').replace('/sys/fs/cgroup/boat-edge','"$WORK/edge-cg"'))
PY
. "$WORK/clean.sh"
mkdir "$WORK/cg"
pgrep() { case "$MODE" in vmm) return 0;; pgrep-error) return 2;; *) return 1;; esac; }
systemctl() { if [ "$1" = list-units ]; then [ "$MODE" != service ] || echo boatd-e2e; else echo 123; fi; }
findmnt() { case "$MODE" in mount) echo /var/lib/boat-e2e;; small) echo /var/lib/boat-e2e-small;; own-mount) echo "$WORK/state";; mount-error) return 2;; esac; return 0; }
ip() { if [ "$1" = netns ]; then [ "$MODE" != netns ] || echo boat123; return 0; fi; [ "$MODE" = bridge ]; }
sudo() {
  case "$1 $2" in
    'losetup -j') [ "$MODE" != loop ] || echo '/dev/loop7: owned';;
    'nft list') [ "$MODE" != nft-error ] || return 2; [ "$MODE" != nft ] || echo 'table inet boat';;
    *) echo "unexpected mutation: $*"; exit 1;;
  esac
  return 0
}
for MODE in clean vmm service mount small own-mount cgroup loop netns bridge nft pgrep-error mount-error nft-error; do
  echo 'populated 0' >"$WORK/cg/cgroup.events"
  [ "$MODE" != cgroup ] || echo 'populated 1' >"$WORK/cg/cgroup.events"
  if clean "$WORK"; then [ "$MODE" = clean ] || { echo "accepted $MODE artifacts"; exit 1; }
  else [ "$MODE" != clean ] || { echo 'rejected clean fixture'; exit 1; }; fi
done
`)
}

func TestPerfPilotBoundarySampling(t *testing.T) {
	runPerfShell(t, `
eval "$(sed -n '/^perf_dump_diag()/,/^}/p' e2e-perf.sh)"
eval "$(sed -n '/^perf_boundary()/,/^}/p' e2e-perf.sh)"
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
STATE=$WORK
mkdir -p "$STATE/images/slim" "$STATE/vms/source/firecracker/source/root"
printf 'disk' >"$STATE/images/slim/rootfs.ext4"
printf 'memory' >"$STATE/vms/source/firecracker/source/root/mem"
sudo() { [ "$1" != -n ] || shift; "$@"; }
timeout() { [ "$1 $2 $3" = '-k 1 40' ]; shift 3; "$@"; }
unset E2E_PILOT
[ -z "$(perf_boundary noop source)" ]
E2E_PILOT=1 perf_boundary snapshot-after source >"$WORK/boundary"
python3 - "$WORK/boundary" <<'PY'
import json, pathlib, sys
samples=[json.loads(line[10:]) for line in pathlib.Path(sys.argv[1]).read_text().splitlines() if line.startswith('dump-diag ')]
boundary=[s for s in samples if s['phase']=='boundary']
assert len(boundary)==1 and boundary[0]['label']=='snapshot-after'
assert boundary[0]['files']['image']['size']==4 and boundary[0]['files']['mem']['size']==6
assert sum(s['phase']=='boundary:snapshot-after' for s in samples)==1
assert not any(s['phase']=='sample' for s in samples)
PY
`)
}

func TestPerfPilotPreflightRemainsBlocked(t *testing.T) {
	runPerfShell(t, `
python3 - <<'PY'
import json, os, pathlib, re, subprocess, sys, tempfile, textwrap
workflow=pathlib.Path('../../../.github/workflows/boat-e2e.yml').read_text()
assert 'default: false' in workflow.split('calibrate_boot_cap:',1)[1].split('permissions:',1)[0]
assert 'timeout-minutes: 180' in workflow and '+ 160 * 60' in workflow
block=workflow.split('- name: Require reviewed calibration execution preconditions',1)[1].split('- uses: actions/checkout',1)[0]
script=textwrap.dedent(re.search(r"python3 - <<'PY'\n(.*?)\n          PY",block,re.S).group(1))
with tempfile.TemporaryDirectory() as tmp:
    output=pathlib.Path(tmp)/'output'
    result=subprocess.run([sys.executable,'-c',script],env=dict(os.environ,RUNNER_TEMP=tmp,GITHUB_OUTPUT=str(output)),capture_output=True,text=True)
    assert result.returncode == 1, result
    root=pathlib.Path(output.read_text().strip().removeprefix('archive='))
    pre=json.loads((root/'precondition.json').read_text())
    assert pre['status']=='blocked-preconditions' and pre['candidate'] is False
    assert pre['pilot']==[['full',150],['full',50],['lifecycle',150],['lifecycle',50],['lifecycle',50],['lifecycle',150]]
    assert pre['confirmatory_caps']==[150,50,50,150,150,50,50,150]
    assert (root/'status').read_text().strip()=='blocked-preconditions'
PY
`)
}

func TestPerfPilotCleanupOwnership(t *testing.T) {
	runPerfShell(t, `
eval "$(sed -n '/^pilot_cleanup_owned()/,/^}/p' ../e2e.sh)"
WORK=/owned STATE=/state
PIDS=(100)
pgrep() {
  if [ "$1" = -x ]; then
    case "$MODE" in owned|unknown|unreadable) echo 42;; error) return 2;; *) return 1;; esac
  else
    case "$MODE" in http-owned) echo 100;; http-unknown) echo 200;; *) return 1;; esac
  fi
}
sudo() {
  if [ "$1" = readlink ]; then
    case "$MODE" in owned) echo /owned/bin/boatd;; unknown) echo /elsewhere/firecracker;; unreadable) return 1;; *) echo 'net:[1]';; esac
  elif [ "$1 $2" = 'ip netns' ]; then echo 'net:[2]'; else echo 'unexpected command'; exit 1; fi
}
for MODE in empty owned unknown unreadable error http-owned http-unknown; do
  if pilot_cleanup_owned; then
    case "$MODE" in empty|owned|http-owned) :;; *) echo "accepted $MODE ownership"; exit 1;; esac
  else
    case "$MODE" in unknown|unreadable|error|http-unknown) :;; *) echo "rejected $MODE ownership"; exit 1;; esac
  fi
done
`)
}

func TestPerfOnlineSSHPolicyGate(t *testing.T) {
	runPerfShell(t, `
eval "$(sed -n '/^perf_numbers()/,/^}/p' e2e-host.sh)"
eval "$(sed -n '/^check_max()/,/^}/p' e2e-host.sh)"
eval "$(sed '/^disk_stuck()/,$d' e2e-perf.sh)"
eval "$(sed -n '/^perf_ssh_policy()/,/^}/p' e2e-perf.sh)"
HERE=$(cd .. && pwd)
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
echo 10 >"$WORK/clock"
now() { cat "$WORK/clock"; }
secs() { echo "$(($2-$1))"; }
fail() { echo "$1" >&2; exit 1; }
perf_cfg() { [ "$1 $2" = '.standaloneSSH = false sshpol' ]; }
perf_vm() { [ "$1" = sshp0 ]; echo vm-single; }
echo false >"$WORK/ready_single"
vmf() {
  [ "$1" = vm-single ]
  case "$2" in
    ssh_policy_ready) cat "$WORK/ready_single" ;;
    ssh_boot_epoch) echo epoch-single ;;
    *) exit 1 ;;
  esac
}
bd() {
  case "$1 $2" in
    'POST /v1/vms/vm-single/ssh-policy')
      [ "$(jq -r '.boot_epoch + ":" + .config.vm_id' <<<"$3")" = "epoch-single:vm-single" ]
      echo 11 >"$WORK/clock"
      echo true >"$WORK/ready_single"
      echo '{"version":1,"config_digest":"d","boot_epoch":"epoch-single","readiness":true}'
      ;;
    'POST /v1/vms')
      echo '{"vms":[{"id":"vm-b1","ssh_policy_ready":false,"ssh_boot_epoch":"e1"},{"id":"vm-b2","ssh_policy_ready":false,"ssh_boot_epoch":"e2"},{"id":"vm-b3","ssh_policy_ready":false,"ssh_boot_epoch":"e3"},{"id":"vm-b4","ssh_policy_ready":false,"ssh_boot_epoch":"e4"}]}'
      ;;
    'POST /v1/vms/ssh-policy-batch')
      [ "$(jq '.items | length' <<<"$3")" = 4 ]
      echo 13 >"$WORK/clock"
      echo '{"results":{"vm-b1":{"readiness":true},"vm-b2":{"readiness":true},"vm-b3":{"readiness":true},"vm-b4":{"readiness":true}},"errors":{}}'
      ;;
    *) echo "unexpected bd call: $*" >&2; exit 1 ;;
  esac
}
deleted=''
perf_del() { deleted+="$* "; }
perf_ssh_policy
[ "$ssh_pol_s" = 1 ]
[ "$ssh_batch_s" = 2 ]
[ "$deleted" = "vm-single vm-b1 vm-b2 vm-b3 vm-b4 " ]
perf_fail=''
check_max perf_ssh_policy "$ssh_pol_s" "$PERF_SSH_POLICY_MAX"
check_max perf_ssh_batch "$ssh_batch_s" "$PERF_SSH_BATCH_MAX"
[ -z "$perf_fail" ]
check_max perf_ssh_batch 6 "$PERF_SSH_BATCH_MAX"
[[ "$perf_fail" = *'perf_ssh_batch took 6s > 5s'* ]]
`)
}

func TestPerfPilotBootCapOverlayAndGuestCacheParity(t *testing.T) {
	runPerfShell(t, `
python3 - <<'PY'
import json, pathlib, re, subprocess, sys, tempfile, textwrap

e2e_wf = pathlib.Path('../../../.github/workflows/boat-e2e.yml').read_text()
bench_wf = pathlib.Path('../../../.github/workflows/boatbench.yml').read_text()

# 1. Guest cache keys: slim-only jobs use boat-guest-slim-<hash>, the full-rootfs job alone
# uses boat-guest-full-<hash>; both share one source hash (incl. ssh-authorize.py + cli/**),
# and slim-only cache paths never list rootfs-full (so a slim save can't shadow the full image).
key_re = r'key:\s*boat-guest-(slim|full)-(\$\{\{[^\n]+\}\})'
e2e_keys = re.findall(key_re, e2e_wf)
bench_keys = re.findall(key_re, bench_wf)
assert {v for v, _ in e2e_keys} == {'slim', 'full'}, e2e_keys
assert bench_keys and all(v == 'slim' for v, _ in bench_keys), bench_keys
hashes = {h for _, h in e2e_keys + bench_keys}
assert len(hashes) == 1, hashes
key = hashes.pop()
assert 'components/boat/guest/ssh-authorize.py' in key and 'components/boat/cli/**' in key, key
for wf in (e2e_wf, bench_wf):
    for m in re.finditer(r'path:\s*\|\n((?:\s+/mnt/guest-out/[^\n]+\n)+)\s*key:\s*boat-guest-slim-', wf):
        assert 'rootfs-full' not in m.group(1), m.group(1)
assert len(re.findall(r'key:\s*boat-guest-full-', e2e_wf)) >= 1
# 2. calibrate_boot_cap overlay snippet executes cleanly against real writeback.go.
snippet = textwrap.dedent(re.search(r'python3 - "\$archive" <<\x27PY\x27\n(.*?)\n          PY', e2e_wf, re.S).group(1))
with tempfile.TemporaryDirectory() as tmp:
    root = pathlib.Path(tmp)
    dst = root / 'source/components/boat/boatd'
    dst.mkdir(parents=True)
    (root / 'overlays').mkdir()
    real_wb = pathlib.Path('writeback.go').read_text()
    (dst / 'writeback.go').write_text(real_wb)
    res = subprocess.run([sys.executable, '-c', snippet, str(root)], capture_output=True, text=True)
    assert res.returncode == 0, res.stderr
    for cap in (150, 64, 50):
        ov = json.loads((root / f'overlays/{cap}.json').read_text())
        replaced = pathlib.Path(list(ov['Replace'].values())[0]).read_text()
        assert f'\tdumpWbpsWhileBooting = {cap} << 20' in replaced
    assert (root / 'overlays/writeback-64.go').read_text() == real_wb
PY
`)
}
