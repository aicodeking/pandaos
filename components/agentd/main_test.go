package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pandaos/agentd/internal/fakerelay"
)

func startRelay(t *testing.T) (*fakerelay.Relay, string) {
	t.Helper()
	r := fakerelay.New(200 * time.Millisecond)
	srv := httptest.NewServer(r.Handler(true))
	t.Cleanup(srv.Close)
	return r, srv.URL
}

func newTestDaemon(t *testing.T, relay string, slots int, run Runner) *daemon {
	t.Helper()
	dir := t.TempDir()
	d := newDaemon(config{relay: relay, slots: slots, stateDir: filepath.Join(dir, "state"), runDir: filepath.Join(dir, "run")}, run)
	d.heartbeatEvery = 50 * time.Millisecond
	d.pairPollEvery = 10 * time.Millisecond
	return d
}

// runDaemon runs d in the background. stop cancels it (like SIGTERM) and
// returns Run's error; the test cleanup calls it if the test did not.
func runDaemon(t *testing.T, d *daemon) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() { cancel(); err = <-done })
		return err
	}
	t.Cleanup(func() {
		if err := stop(); err != context.Canceled {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	})
	return stop
}

// startDaemon runs a daemon with fast intervals until the test ends.
func startDaemon(t *testing.T, relay string, slots int, run Runner) *daemon {
	t.Helper()
	d := newTestDaemon(t, relay, slots, run)
	runDaemon(t, d)
	return d
}

func waitResult(t *testing.T, r *fakerelay.Relay, id string) fakerelay.Result {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if res, ok := r.Result(id); ok {
			return res
		}
	}
	t.Fatalf("no result for job %s", id)
	return fakerelay.Result{}
}

func echoRunner(_ context.Context, j Job, _ string) Result {
	return Result{ExitCode: 7, Stdout: strings.Join(j.Argv, " "), Stderr: "err"}
}

func TestPairAndJobRoundTrip(t *testing.T) {
	relay, url := startRelay(t)
	d := startDaemon(t, url, 2, echoRunner)
	relay.Enqueue(fakerelay.Job{ID: "j1", Argv: []string{"echo", "hi"}})

	res := waitResult(t, relay, "j1")
	if res != (fakerelay.Result{ExitCode: 7, Stdout: "echo hi", Stderr: "err"}) {
		t.Fatalf("result = %+v", res)
	}
	st, err := os.Stat(d.tokenPath())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %v, want 0600", st.Mode().Perm())
	}
	if relay.Pairings() != 1 {
		t.Fatalf("pairings = %d, want 1", relay.Pairings())
	}
}

func TestPairingCode(t *testing.T) {
	code, err := pairingCode()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 6 || strings.Trim(code, codeAlphabet) != "" {
		t.Fatalf("bad code %q", code)
	}
}

func TestTruncation(t *testing.T) {
	relay, url := startRelay(t)
	big := strings.Repeat("x", 100_000)
	startDaemon(t, url, 1, func(context.Context, Job, string) Result { return Result{Stdout: big, Stderr: big} })
	relay.Enqueue(fakerelay.Job{ID: "big", Argv: []string{"x"}})
	res := waitResult(t, relay, "big")
	if len(res.Stdout) != maxOutput || len(res.Stderr) != maxOutput {
		t.Fatalf("got stdout=%d stderr=%d, want %d each", len(res.Stdout), len(res.Stderr), maxOutput)
	}

	// The real runner caps output while capturing.
	r := execRunner(context.Background(), Job{Argv: []string{"sh", "-c", "head -c 100000 /dev/zero; exit 3"}}, "")
	if len(r.Stdout) != maxOutput || r.ExitCode != 3 {
		t.Fatalf("execRunner: stdout=%d exit=%d", len(r.Stdout), r.ExitCode)
	}
}

func TestExecRunnerTimeout(t *testing.T) {
	r := run(context.Background(), 50*time.Millisecond, "sleep", []string{"5"}, "", nil)
	if r.ExitCode != 124 {
		t.Fatalf("exit = %d, want 124", r.ExitCode)
	}
}

func TestUnauthorizedRepairs(t *testing.T) {
	relay, url := startRelay(t)
	d := startDaemon(t, url, 1, echoRunner)
	relay.Enqueue(fakerelay.Job{ID: "a", Argv: []string{"a"}})
	waitResult(t, relay, "a")
	old, _ := d.loadToken()

	relay.Revoke()
	relay.Enqueue(fakerelay.Job{ID: "b", Argv: []string{"b"}})
	waitResult(t, relay, "b")

	if relay.Pairings() != 2 {
		t.Fatalf("pairings = %d, want 2", relay.Pairings())
	}
	if now, _ := d.loadToken(); now == old || now == "" {
		t.Fatalf("token not replaced: old=%q new=%q", old, now)
	}
}

func TestSlotLimit(t *testing.T) {
	relay, url := startRelay(t)
	var cur, peak atomic.Int32
	startDaemon(t, url, 2, func(context.Context, Job, string) Result {
		n := cur.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(100 * time.Millisecond)
		cur.Add(-1)
		return Result{}
	})
	ids := []string{"1", "2", "3", "4", "5", "6"}
	for _, id := range ids {
		relay.Enqueue(fakerelay.Job{ID: id, Argv: []string{"x"}})
	}
	for _, id := range ids {
		waitResult(t, relay, id)
	}
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak concurrency = %d, want 2", p)
	}
}

func TestConfig(t *testing.T) {
	noenv := func(string) string { return "" }
	for _, relay := range []string{"", "ftp://x", "relay.example", "http://", "://bad", "http://r:8787"} {
		if _, err := parseConfig([]string{"-relay", relay}, noenv); err == nil {
			t.Errorf("relay %q accepted", relay)
		}
	}
	if _, err := parseConfig([]string{"-relay", "https://r", "-slots", "0"}, noenv); err == nil {
		t.Error("slots 0 accepted")
	}
	env := func(k string) string {
		return map[string]string{"PANDA_AGENTD_RELAY": "http://r:8787", "PANDA_AGENTD_SLOTS": "4", "PANDA_AGENTD_ALLOW_INSECURE": "1"}[k]
	}
	c, err := parseConfig(nil, env)
	if err != nil || c.relay != "http://r:8787" || !c.allowInsecure || c.slots != 4 || c.stateDir != "/var/lib/panda-agentd" || c.cpuQuota != "200%" {
		t.Fatalf("env config = %+v, %v", c, err)
	}
	for _, q := range []string{"200", "0%", "x%", "-5%"} {
		if _, err := parseConfig([]string{"-relay", "https://r", "-cpu-quota", q}, noenv); err == nil {
			t.Errorf("cpu-quota %q accepted", q)
		}
	}
	if _, err := parseConfig([]string{"-relay", "https://r", "-job-rw-path", "mem"}, noenv); err == nil {
		t.Error("relative -job-rw-path accepted")
	}
	c, err = parseConfig([]string{"-relay", "https://r", "-cpu-quota", "", "-job-rw-path", "/mem/"}, noenv)
	if err != nil || c.cpuQuota != "" || len(c.jobRWPaths) != 1 || c.jobRWPaths[0] != "/mem" {
		t.Fatalf("config = %+v, %v", c, err)
	}
}
