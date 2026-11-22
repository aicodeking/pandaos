package main

import (
	"sync"
	"testing"
	"time"
)

// r31 hot path: command history is lazy bookkeeping, a no_env run ends only on
// a stop that completed, and AI proxy admission honours the team's proxy-off.

const (
	r31FsyncDelay = 1500 * time.Millisecond
	r31Budget     = 500 * time.Millisecond
)

// (a) A detached command must not wait for a state.json fsync.
func TestR31DetachedCommandDoesNotWaitForFsync(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	slowFsync(t, r31FsyncDelay)
	t0 := time.Now()
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/commands", map[string]any{"cmd": []string{"echo", "hi"}})
	took := time.Since(t0)
	if st != 201 {
		t.Fatalf("command: %d %v", st, out)
	}
	if took > r31Budget {
		t.Errorf("detached command took %v under %v fsync (budget %v): command history waits for the disk", took, r31FsyncDelay, r31Budget)
	}
	// The history is visible at once even though it is not on disk yet.
	if recs := e.srv.store.Snapshot().Commands[id]; len(recs) != 1 || recs[0].ID != out["id"] {
		t.Errorf("history = %+v, want the new command", recs)
	}
}

// (b) A no_env create is not queued behind a command-history write.
func TestR31NoEnvCreateNotQueuedBehindCommandHistory(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	slowFsync(t, r31FsyncDelay)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.do(e.owner, "POST", "/v1/vms/"+id+"/commands", map[string]any{"cmd": []string{"true"}})
		}()
	}
	time.Sleep(20 * time.Millisecond) // let the commands reach the store
	t0 := time.Now()
	st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "noenv", "secrets": []string{}, "no_env": true})
	took := time.Since(t0)
	wg.Wait()
	if st != 201 {
		t.Fatalf("no_env create: %d %v", st, out)
	}
	if took > r31Budget {
		t.Errorf("no_env create took %v behind command writes under %v fsync (budget %v)", took, r31FsyncDelay, r31Budget)
	}
}

// (c) Create with an environment setup_script starts the setup command without
// waiting for the disk.
func TestR31SetupScriptCreateDoesNotWaitForFsync(t *testing.T) {
	e := setup(t)
	if st, env := e.do(e.owner, "POST", "/v1/environments", map[string]any{"name": "app", "setup_script": "npm ci"}); st != 201 {
		t.Fatalf("environment: %d %v", st, env)
	}
	slowFsync(t, r31FsyncDelay)
	t0 := time.Now()
	st, vm := e.do(e.owner, "POST", "/v1/vms", map[string]any{"environment": "app"})
	took := time.Since(t0)
	if st != 201 || vm["setup_cmd_id"] == nil {
		t.Fatalf("create with setup script: %d %v", st, vm)
	}
	if took > r31Budget {
		t.Errorf("create with setup_script took %v under %v fsync (budget %v)", took, r31FsyncDelay, r31Budget)
	}
}

// A stop, pending, failing or completed, never ends no_env (r46: sticky).
func TestR31NoEnvRunEndsOnlyOnCompletedStop(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	id := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("no_env start: %d %v", st, out)
	}
	if !e.srv.noEnvRun(id) {
		t.Fatal("no_env run not recognised after start")
	}
	time.Sleep(5 * time.Millisecond) // the stop op must be requested after the marker

	op := e.srv.stops.begin(e.teamID(), id)
	if !e.srv.noEnvRun(id) {
		t.Fatal("a pending stop ended the no_env run")
	}
	e.srv.stops.failAsync(id, op, "boatd unreachable")
	if !e.srv.noEnvRun(id) {
		t.Fatal("a failing stop ended the no_env run")
	}
	if _, ok := e.srv.store.Snapshot().NoEnvRuns[id]; !ok {
		t.Fatal("a failing stop dropped the no_env marker")
	}
	e.srv.stops.completeAsync(id, op)
	if !e.srv.noEnvRun(id) {
		t.Fatal("a completed stop ended no_env, which is sticky")
	}
	if _, ok := e.srv.store.Snapshot().NoEnvRuns[id]; !ok {
		t.Fatal("a completed stop dropped the no_env marker")
	}
}

// Admission (not only the stream watch) refuses once the team turned the proxy off.
func TestR31AIProxyAdmissionRefusesWhenProxyOff(t *testing.T) {
	e, _, proxy := aiSetup(t)
	e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "sk-ant-REAL-KEY-123456"})
	id := e.createVM()
	tok := e.srv.aiToken(id, e.srv.store.Snapshot().AIVMs[id].Gen)
	if c := aiStatus(t, proxy.URL, tok); c != 200 {
		t.Fatalf("before proxy off: %d", c)
	}
	if st, _ := e.do(e.owner, "PUT", "/v1/ai/proxy", map[string]any{"enabled": false}); st != 200 {
		t.Fatalf("proxy off: %d", st)
	}
	// Same token generation: the VM's env was not re-issued, but the team
	// switched the proxy off, so a new request must not be admitted.
	if c := aiStatus(t, proxy.URL, tok); c != 403 {
		t.Errorf("request with proxy off = %d, want 403", c)
	}
	if st, _ := e.do(e.owner, "PUT", "/v1/ai/proxy", map[string]any{"enabled": true}); st != 200 {
		t.Fatalf("proxy on: %d", st)
	}
	tok = e.srv.aiToken(id, e.srv.store.Snapshot().AIVMs[id].Gen)
	if c := aiStatus(t, proxy.URL, tok); c != 200 {
		t.Errorf("after proxy back on: %d", c)
	}
}
