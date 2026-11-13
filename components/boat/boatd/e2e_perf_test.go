package main

import (
	"context"
	"os/exec"
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

func TestPerfLifecycleIncludesFirstExec(t *testing.T) {
	for _, label := range []string{"p", "q"} {
		t.Run(label, func(t *testing.T) {
			script := `
eval "$(sed -n '/^perf_snap()/,/^}/p' e2e-perf.sh)"
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
mkdir "$WORK/tmp"
STATE=$WORK
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
awk() { :; } # /proc/meminfo logging is unrelated to lifecycle timing
sudo() { :; }
sleep() { :; }
fail() { echo "$1" >&2; exit 1; }
bd() {
  # The background dump must not race lifecycle calls writing the stub clock.
  if [ "${3:-}" = '{"name":"y"}' ]; then echo '{"id":"snapshot"}'; return; fi
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
