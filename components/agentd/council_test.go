package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pandaos/agentd/internal/fakerelay"
)

func noenv(string) string { return "" }

func TestStatusJSONSchema(t *testing.T) {
	relay, url := startRelay(t)
	d := startDaemon(t, url, 1, echoRunner)
	relay.Enqueue(fakerelay.Job{ID: "j", Argv: []string{"x"}})
	waitResult(t, relay, "j")
	waitStatus(t, d, func(s Status) bool { return s.Failed == 1 })
	b, _ := os.ReadFile(d.statusPath())
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range raw {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"completed", "failed", "lastError", "paired", "pairing", "relay", "running", "since", "slots", "version"}
	if !slices.Equal(keys, want) {
		t.Fatalf("status.json keys = %v, want %v", keys, want)
	}
}

// pendingRelay accepts pairing but never approves it.
func pendingRelay(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestPairingCodeFile(t *testing.T) {
	d := newTestDaemon(t, pendingRelay(t), 1, echoRunner)
	d.codeGID = os.Getgid() // exercises the chown path; root would use wheel
	runDaemon(t, d)
	st := waitStatus(t, d, func(s Status) bool { return s.Pairing })
	if st.Paired {
		t.Fatalf("status = %+v", st)
	}
	code, err := os.ReadFile(d.codePath())
	if err != nil {
		t.Fatal(err)
	}
	c := strings.TrimSpace(string(code))
	if len(c) != 6 {
		t.Fatalf("code %q", c)
	}
	fi, _ := os.Stat(d.codePath())
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("pairing-code mode = %v, want 0640", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(d.cfg.runDir); fi.Mode().Perm() != 0o755 {
		t.Fatalf("run dir mode = %v, want 0755", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(d.statusPath()); bytes.Contains(b, []byte(c)) {
		t.Fatalf("status.json leaks the pairing code: %s", b)
	}

	var out bytes.Buffer
	if err := statusCmd([]string{"-run-dir", d.cfg.runDir}, noenv, &out); err != nil || !strings.Contains(out.String(), "pairing code "+c) {
		t.Fatalf("status (can read code): %v\n%s", err, out.String())
	}
	if os.Geteuid() != 0 { // root reads anything
		os.Chmod(d.codePath(), 0)
		out.Reset()
		statusCmd([]string{"-run-dir", d.cfg.runDir}, noenv, &out)
		if s := out.String(); strings.Contains(s, c) || !strings.Contains(s, "waiting for approval") {
			t.Fatalf("status (cannot read code):\n%s", s)
		}
	}
}

func TestLastError(t *testing.T) {
	relay := fakerelay.New(100 * time.Millisecond)
	var broken atomic.Bool
	h := relay.Handler(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() && !strings.HasPrefix(r.URL.Path, "/v1/pair") {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	d := startDaemon(t, srv.URL, 1, echoRunner)
	waitStatus(t, d, func(s Status) bool { return s.Paired })

	broken.Store(true)
	st := waitStatus(t, d, func(s Status) bool { return s.LastError != "" })
	if !strings.Contains(st.LastError, "503") || st.LastErrorAt == nil || time.Since(*st.LastErrorAt) > time.Minute {
		t.Fatalf("status = %+v", st)
	}
	broken.Store(false)
	st = waitStatus(t, d, func(s Status) bool { return s.LastError == "" })
	if st.LastErrorAt != nil {
		t.Fatalf("lastErrorAt not cleared: %+v", st)
	}
	if b, _ := os.ReadFile(d.statusPath()); bytes.Contains(b, []byte("lastErrorAt")) {
		t.Fatalf("status.json = %s", b)
	}
}

func TestRelayFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "relay")
	parse := func(extra ...string) (config, error) {
		return parseConfig(append([]string{"-state-dir", dir}, extra...), noenv)
	}

	// Missing file: -relay is used, and the default path is <state-dir>/relay.
	c, err := parse("-relay", "https://flag.example")
	if err != nil || c.relay != "https://flag.example" || c.relayFile != file {
		t.Fatalf("no file: %+v, %v", c, err)
	}
	os.WriteFile(file, []byte("  \n"), 0o600) // empty: ignored
	if c, err := parse("-relay", "https://flag.example"); err != nil || c.relay != "https://flag.example" {
		t.Fatalf("empty file: %+v, %v", c, err)
	}
	os.WriteFile(file, []byte("https://file.example\n"), 0o600)
	if c, err := parse("-relay", "https://flag.example"); err != nil || c.relay != "https://file.example" {
		t.Fatalf("override: %+v, %v", c, err)
	}
	if c, err := parse(); err != nil || c.relay != "https://file.example" {
		t.Fatalf("file without -relay: %+v, %v", c, err)
	}
	for _, bad := range []string{"http://file.example", "ftp://x", "not a url"} {
		os.WriteFile(file, []byte(bad), 0o600)
		if _, err := parse("-relay", "https://flag.example", "-allow-insecure"); err == nil || !strings.Contains(err.Error(), file) {
			t.Errorf("relay file %q: err = %v", bad, err)
		}
	}
	os.WriteFile(file, []byte("http://10.0.0.5:8787"), 0o600)
	if _, err := parse("-relay", "https://flag.example"); err == nil {
		t.Error("private http accepted without -allow-insecure")
	}
	if c, err := parse("-relay", "https://flag.example", "-allow-insecure"); err != nil || c.relay != "http://10.0.0.5:8787" {
		t.Fatalf("insecure override: %+v, %v", c, err)
	}
	os.WriteFile(file, bytes.Repeat([]byte("x"), maxRelayFile+1), 0o600)
	if _, err := parse("-relay", "https://flag.example"); err == nil {
		t.Error("oversized relay file accepted")
	}
	other := filepath.Join(dir, "other")
	os.WriteFile(other, []byte("https://other.example"), 0o600)
	if c, err := parse("-relay-file", other); err != nil || c.relay != "https://other.example" {
		t.Fatalf("-relay-file: %+v, %v", c, err)
	}
}

func TestMinFreeGuard(t *testing.T) {
	relay, url := startRelay(t)
	d := newTestDaemon(t, url, 1, func(context.Context, Job, string) Result {
		t.Error("job ran on a full disk")
		return Result{}
	})
	d.cfg.minFreeMB = 1024
	d.freeMB = func(string) (int64, error) { return 10, nil }
	runDaemon(t, d)
	relay.Enqueue(fakerelay.Job{ID: "full", Argv: []string{"x"}})
	res := waitResult(t, relay, "full")
	if res.ExitCode != -1 || !strings.Contains(res.Stderr, "low disk space: 10 MiB free") {
		t.Fatalf("result = %+v", res)
	}
	waitStatus(t, d, func(s Status) bool { return s.Failed == 1 })

	d.cfg.minFreeMB = 0 // disabled: no statfs at all
	d.freeMB = func(string) (int64, error) { return 0, errors.New("boom") }
	if err := d.checkDisk(); err != nil {
		t.Fatal(err)
	}
	if mb, err := freeMB(t.TempDir()); err != nil || mb <= 0 {
		t.Fatalf("freeMB = %d, %v", mb, err)
	}
	if _, err := parseConfig([]string{"-relay", "https://r", "-min-free-mb", "-1"}, noenv); err == nil {
		t.Error("negative -min-free-mb accepted")
	}
}

type fakeSystemctl struct {
	mu    sync.Mutex
	calls []string
	units string
}

func (f *fakeSystemctl) run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	if args[0] == "list-units" {
		return []byte(f.units), nil
	}
	return nil, nil
}

func TestLeftoverUnitCleanup(t *testing.T) {
	relay, url := startRelay(t)
	sc := &fakeSystemctl{units: "panda-job-old1.service loaded active running /bin/sleep 600\n" +
		"panda-job-old2.service loaded failed failed /bin/false\n"}
	d := newTestDaemon(t, url, 1, echoRunner)
	d.systemctl = sc.run
	runDaemon(t, d)
	for _, id := range []string{"old1", "old2"} {
		if res := waitResult(t, relay, id); !res.Interrupted || res.ExitCode != -1 {
			t.Fatalf("%s: %+v", id, res)
		}
	}
	waitStatus(t, d, func(s Status) bool { return s.Failed == 2 })
	sc.mu.Lock()
	defer sc.mu.Unlock()
	want := []string{
		"list-units --all --plain --no-legend --no-pager panda-job-*.service",
		"stop panda-job-*.service",
		"reset-failed panda-job-*.service",
	}
	if !slices.Equal(sc.calls, want) {
		t.Fatalf("systemctl calls = %q", sc.calls)
	}
}

func TestLeftoverUnitsNone(t *testing.T) {
	sc := &fakeSystemctl{}
	ids, err := cleanupUnits(context.Background(), sc.run)
	if err != nil || ids != nil || len(sc.calls) != 1 {
		t.Fatalf("ids=%v err=%v calls=%q", ids, err, sc.calls)
	}
	failing := func(context.Context, ...string) ([]byte, error) { return nil, fmt.Errorf("no bus") }
	if _, err := cleanupUnits(context.Background(), failing); err == nil {
		t.Fatal("list error swallowed")
	}
}
