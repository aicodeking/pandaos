package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pandaos/agentd/internal/fakerelay"
)

func TestRelayURLPolicy(t *testing.T) {
	for _, tc := range []struct {
		url              string
		insecure, secure bool // accepted with / without -allow-insecure
	}{
		{"https://relay.example.com", true, true},
		{"https://8.8.8.8:443", true, true},
		{"http://relay.example.com", false, false},
		{"http://8.8.8.8", false, false},
		{"http://127.0.0.1:8787", true, false},
		{"http://[::1]:8787", true, false},
		{"http://10.1.2.3", true, false},
		{"http://172.16.0.1", true, false},
		{"http://192.168.1.10:8787", true, false},
		{"http://[fd00::1]", true, false},
		{"http://169.254.1.1", true, false},
		{"http://localhost:8787", true, false},
		{"http://relay:8787", true, false},
		{"http://panda.local", true, false},
		{"http://Panda.Local.", true, false},
		{"http://172.32.0.1", false, false}, // just outside 172.16/12
		{"http://local.example.com", false, false},
		{"ftp://relay", false, false},
		{"relay.example", false, false},
		{"https://", false, false},
		{"", false, false},
	} {
		if got := checkRelayURL(tc.url, true) == nil; got != tc.insecure {
			t.Errorf("checkRelayURL(%q, insecure) ok=%v, want %v", tc.url, got, tc.insecure)
		}
		if got := checkRelayURL(tc.url, false) == nil; got != tc.secure {
			t.Errorf("checkRelayURL(%q, secure) ok=%v, want %v", tc.url, got, tc.secure)
		}
	}
	if v := httpClient.Transport.(*http.Transport).TLSClientConfig.MinVersion; v != tls.VersionTLS12 {
		t.Errorf("TLS MinVersion = %x, want 1.2", v)
	}
}

func TestValidateJob(t *testing.T) {
	ok := []string{"x"}
	for _, tc := range []struct {
		name  string
		job   Job
		valid bool
	}{
		{"ok", Job{ID: "a-B_9", Argv: ok}, true},
		{"64-char id", Job{ID: strings.Repeat("a", 64), Argv: ok}, true},
		{"65-char id", Job{ID: strings.Repeat("a", 65), Argv: ok}, false},
		{"empty id", Job{ID: "", Argv: ok}, false},
		{"traversal id", Job{ID: "../etc", Argv: ok}, false},
		{"dot id", Job{ID: "a.b", Argv: ok}, false},
		{"slash id", Job{ID: "a/b", Argv: ok}, false},
		{"nil argv", Job{ID: "a"}, false},
		{"empty argv0", Job{ID: "a", Argv: []string{""}}, false},
		{"NUL", Job{ID: "a", Argv: []string{"x", "a\x00b"}}, false},
		{"4 KiB arg", Job{ID: "a", Argv: []string{strings.Repeat("x", 4096)}}, true},
		{"4 KiB+1 arg", Job{ID: "a", Argv: []string{strings.Repeat("x", 4097)}}, false},
		{"64 KiB total", Job{ID: "a", Argv: repeat(strings.Repeat("x", 4096), 16)}, true},
		{"64 KiB+1 total", Job{ID: "a", Argv: append(repeat(strings.Repeat("x", 4096), 16), "y")}, false},
	} {
		if err := validateJob(tc.job); (err == nil) != tc.valid {
			t.Errorf("%s: err=%v, want valid=%v", tc.name, err, tc.valid)
		}
	}
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestInvalidJobReported(t *testing.T) {
	relay, url := startRelay(t)
	ran := make(chan struct{}, 1)
	d := startDaemon(t, url, 1, func(context.Context, Job, string) Result { ran <- struct{}{}; return Result{} })
	relay.Enqueue(fakerelay.Job{ID: "bad.id", Argv: []string{"x"}})
	res := waitResult(t, relay, "bad.id")
	if res.ExitCode != -1 || !strings.Contains(res.Stderr, "invalid job id") {
		t.Fatalf("result = %+v", res)
	}
	select {
	case <-ran:
		t.Fatal("invalid job ran")
	default:
	}
	waitStatus(t, d, func(s Status) bool { return s.Failed == 1 })
}

func TestSystemdArgs(t *testing.T) {
	cfg := config{cpuQuota: "150%", jobRWPaths: []string{"/mem"}}
	args := strings.Join(systemdArgs(cfg, Job{ID: "j1", Argv: []string{"echo", "hi"}, TimeoutSec: 30}, "/var/lib/panda-agentd/jobs/j1"), " ")
	for _, want := range []string{
		"--unit=panda-job-j1.service", "--uid=panda-agent", "--working-directory=/var/lib/panda-agentd/jobs/j1",
		"-p PrivateTmp=yes", "-p ProtectSystem=strict", "-p ProtectHome=read-only", "-p NoNewPrivileges=yes",
		"-p ReadWritePaths=/var/lib/panda-agentd/jobs/j1", "-p MemoryMax=80%", "-p CPUQuota=150%",
		"-p TasksMax=512", "-p RuntimeMaxSec=30", "-p ReadWritePaths=-/mem", "-- echo hi",
		"-p BindsTo=panda-agentd.service", "-p PrivateDevices=yes", "-p ProtectProc=invisible", "-p ProcSubset=pid",
		"-p ProtectKernelTunables=yes", "-p ProtectKernelModules=yes", "-p ProtectKernelLogs=yes",
		"-p ProtectControlGroups=yes", "-p RestrictSUIDSGID=yes", "-p LockPersonality=yes", "-p RestrictRealtime=yes",
		"-p SystemCallArchitectures=native", "-p SystemCallFilter=@system-service",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("systemd-run args missing %q:\n%s", want, args)
		}
	}
	args = strings.Join(systemdArgs(config{}, Job{ID: "j", Argv: []string{"x"}, MemoryMaxMB: 64}, "/d"), " ")
	if !strings.Contains(args, "MemoryMax=64M") || strings.Contains(args, "CPUQuota") {
		t.Errorf("args = %s", args)
	}
}

func TestWorkdir(t *testing.T) {
	relay, url := startRelay(t)
	dirs := make(chan string, 2)
	d := startDaemon(t, url, 1, func(_ context.Context, j Job, dir string) Result {
		dirs <- dir
		if err := os.WriteFile(filepath.Join(dir, "out"), []byte("x"), 0o600); err != nil {
			return Result{ExitCode: 1, Stderr: err.Error()}
		}
		return Result{}
	})
	relay.Enqueue(fakerelay.Job{ID: "gone", Argv: []string{"x"}})
	relay.Enqueue(fakerelay.Job{ID: "kept", Argv: []string{"x"}, KeepWorkdir: true})
	for _, id := range []string{"gone", "kept"} {
		if res := waitResult(t, relay, id); res.ExitCode != 0 {
			t.Fatalf("%s: %+v", id, res)
		}
		if dir := <-dirs; dir != filepath.Join(d.cfg.stateDir, "jobs", id) {
			t.Fatalf("workdir = %s", dir)
		}
	}
	waitStatus(t, d, func(s Status) bool { return s.Completed == 2 })
	if _, err := os.Stat(filepath.Join(d.jobsDir(), "gone")); !os.IsNotExist(err) {
		t.Errorf("workdir of gone not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.jobsDir(), "kept", "out")); err != nil {
		t.Errorf("keepWorkdir dir missing: %v", err)
	}

	// execRunner runs in the workdir.
	dir := t.TempDir()
	if r := execRunner(context.Background(), Job{Argv: []string{"pwd"}}, dir); !strings.Contains(r.Stdout, filepath.Base(dir)) {
		t.Errorf("pwd = %q, want %s", r.Stdout, dir)
	}
}

func readStatus(t *testing.T, d *daemon) Status {
	t.Helper()
	b, err := os.ReadFile(d.statusPath())
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("status.json %q: %v", b, err)
	}
	return st
}

func waitStatus(t *testing.T, d *daemon, ok func(Status) bool) Status {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(d.statusPath()); err == nil {
			if st := readStatus(t, d); ok(st) {
				return st
			}
		}
	}
	t.Fatalf("status never matched; last %+v", d.status())
	return Status{}
}

func TestStatusFile(t *testing.T) {
	relay, url := startRelay(t)
	d := startDaemon(t, url, 3, echoRunner) // echoRunner exits 7 -> failed
	relay.Enqueue(fakerelay.Job{ID: "j1", Argv: []string{"x"}})
	waitResult(t, relay, "j1")
	st := waitStatus(t, d, func(s Status) bool { return s.Failed == 1 })
	if !st.Paired || st.Pairing || st.LastError != "" || st.LastErrorAt != nil || st.Relay != url || st.Slots != 3 || st.Running != 0 ||
		st.Completed != 0 || st.Version != version || st.Since.IsZero() {
		t.Fatalf("status = %+v", st)
	}
	fi, err := os.Stat(d.statusPath())
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("status.json mode = %v, %v; want 0644", fi.Mode().Perm(), err)
	}

	var out bytes.Buffer
	if err := statusCmd([]string{"-run-dir", d.cfg.runDir}, func(string) string { return "" }, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "paired:  yes") || !strings.Contains(s, "0/3 running, 0 completed, 1 failed") {
		t.Fatalf("status output:\n%s", s)
	}
	out.Reset()
	if err := statusCmd([]string{"-json", "-run-dir", d.cfg.runDir}, func(string) string { return "" }, &out); err != nil || !json.Valid(out.Bytes()) {
		t.Fatalf("status -json: %v %q", err, out.String())
	}
}

func TestStatusAtomicity(t *testing.T) {
	d := newTestDaemon(t, "https://r", 1, echoRunner)
	if err := d.setup(); err != nil {
		t.Fatal(err)
	}

	d.setStatus(func(s *Status) {})
	// Readers racing a writer must always see a complete document.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				d.setStatus(func(s *Status) { s.Completed = i; s.LastError = strings.Repeat("X", i%500) })
			}
		}
	}()
	for i := 0; i < 2000; i++ {
		b, err := os.ReadFile(d.statusPath())
		if err != nil || !json.Valid(b) {
			close(stop)
			wg.Wait()
			t.Fatalf("read %d: torn status.json (%v): %q", i, err, b)
		}
	}
	close(stop)
	wg.Wait()
	entries, _ := os.ReadDir(d.cfg.runDir)
	for _, e := range entries {
		if e.Name() != "status.json" {
			t.Errorf("leftover file %s", e.Name())
		}
	}
}

// blockRunner blocks until its job is cancelled, reporting starts on started.
func blockRunner(started chan<- string) Runner {
	return func(ctx context.Context, j Job, _ string) Result {
		started <- j.ID
		<-ctx.Done()
		return Result{ExitCode: -1, Stdout: "partial"}
	}
}

func TestCancel(t *testing.T) {
	relay, url := startRelay(t)
	started := make(chan string, 1)
	d := startDaemon(t, url, 1, blockRunner(started))
	relay.Enqueue(fakerelay.Job{ID: "long", Argv: []string{"x"}})
	<-started
	waitStatus(t, d, func(s Status) bool { return s.Running == 1 })
	if !relay.Cancel("long") {
		t.Fatal("relay refused cancel")
	}
	res := waitResult(t, relay, "long")
	if !res.Cancelled || res.ExitCode != exitCancelled || res.Stdout != "partial" || !strings.Contains(res.Stderr, "cancelled by relay") {
		t.Fatalf("result = %+v", res)
	}
	waitStatus(t, d, func(s Status) bool { return s.Running == 0 && s.Failed == 1 })
	if relay.Cancel("long") {
		t.Fatal("cancel of finished job accepted")
	}
}

func TestCancelQueued(t *testing.T) {
	relay, url := startRelay(t)
	relay.Enqueue(fakerelay.Job{ID: "q", Argv: []string{"x"}})
	relay.Cancel("q")
	startDaemon(t, url, 1, func(context.Context, Job, string) Result {
		t.Error("cancelled job ran")
		return Result{}
	})
	if res := waitResult(t, relay, "q"); !res.Cancelled {
		t.Fatalf("result = %+v", res)
	}
}

func TestGracefulShutdown(t *testing.T) {
	relay, url := startRelay(t)
	started := make(chan string, 2)
	block := blockRunner(started)
	d := newTestDaemon(t, url, 2, func(ctx context.Context, j Job, dir string) Result {
		if j.ID == "quick" {
			started <- j.ID
			time.Sleep(200 * time.Millisecond) // finishes inside the grace period
			return Result{Stdout: "done"}
		}
		return block(ctx, j, dir)
	})
	d.shutdownGrace = 500 * time.Millisecond
	stop := runDaemon(t, d)
	relay.Enqueue(fakerelay.Job{ID: "quick", Argv: []string{"x"}})
	relay.Enqueue(fakerelay.Job{ID: "stuck", Argv: []string{"x"}})
	<-started
	<-started

	begin := time.Now()
	if err := stop(); err != context.Canceled {
		t.Fatalf("Run = %v", err)
	}
	took := time.Since(begin)
	if took < d.shutdownGrace || took > d.shutdownGrace+3*time.Second {
		t.Errorf("shutdown took %v, want about %v", took, d.shutdownGrace)
	}
	// Both results are in before Run returns.
	if res, ok := relay.Result("quick"); !ok || res.Cancelled || res.Stdout != "done" {
		t.Errorf("quick = %+v, %v", res, ok)
	}
	if res, ok := relay.Result("stuck"); !ok || !res.Cancelled || !strings.Contains(res.Stderr, "shutting down") {
		t.Errorf("stuck = %+v, %v", res, ok)
	}
	// Polling stopped: a job queued now is never taken.
	relay.Enqueue(fakerelay.Job{ID: "late", Argv: []string{"x"}})
	time.Sleep(100 * time.Millisecond)
	if _, ok := relay.Result("late"); ok {
		t.Error("job taken after shutdown")
	}
}
