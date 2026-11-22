package main

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// r37: a no_env cold start of a VM whose previous run had secrets boots fresh and
// fails closed when its env does not land; the run-end order is by sequence, not
// by the wall clock; the lazy mark delete is compare-and-delete; a compat read in
// the pre-start window says provisioning.

func (e *env) freshBootsOf(id string) int {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	n := 0
	for _, v := range e.boatd.freshBoots {
		if v == id {
			n++
		}
	}
	return n
}

func countStopOps(st *State) int {
	n := 0
	for _, op := range st.StopOps {
		if !isMarkRecord(op) {
			n++
		}
	}
	return n
}

func (e *env) boatdState(id string) string {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	s, _ := e.boatd.vms[id]["state"].(string)
	return s
}

// secretVM is a stopped VM whose last run was a normal one (it had TEAM_SEC).
func (e *env) secretVM() string {
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	id := e.createVM()
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil); st != 200 {
		e.t.Fatalf("stop = %d %v", st, out)
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{}); st != 200 {
		e.t.Fatalf("start = %d %v", st, out)
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil); st != 200 {
		e.t.Fatalf("stop = %d %v", st, out)
	}
	return id
}

// 1a: a no_env start after a run that had secrets cold-boots (fresh_boot); one
// after a no_env run, and a normal start, do not.
func TestR37NoEnvColdStartAfterSecretRunBootsFresh(t *testing.T) {
	e := setup(t)
	id := e.secretVM()
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("no_env start = %d %v", st, out)
	}
	if n := e.freshBootsOf(id); n != 1 {
		t.Fatalf("fresh boots after a secret run = %d, want 1", n)
	}
	if v := e.lastStart(); v["fresh_boot"] != true {
		t.Fatalf("start body = %v, want fresh_boot", v)
	}
	// The previous run was no_env: its memory holds no secrets, resume as usual.
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("second no_env start = %d %v", st, out)
	}
	if n := e.freshBootsOf(id); n != 1 {
		t.Fatalf("fresh boots after a no_env run = %d, want still 1", n)
	}
	// A normal start never asks for one.
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{})
	if v := e.lastStart(); v["fresh_boot"] != nil || e.freshBootsOf(id) != 1 {
		t.Fatalf("normal start = %v, fresh boots %d", v, e.freshBootsOf(id))
	}
}

// 1b/1c: the env push of a no_env cold start does not land: the VM is stopped
// (forced, no stop operation), the failure is recorded, the answer is 502.
func TestR37NoEnvStartEnvFailureFailsClosed(t *testing.T) {
	e := setup(t)
	id := e.secretVM()
	e.setEnvFailures(false, true)
	stopsBefore := countStopOps(e.srv.store.Snapshot())
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true})
	if st != 502 || nativeErrCode(out) != "env_start_failed" {
		t.Fatalf("no_env start with failing env = %d %v, want 502 env_start_failed", st, out)
	}
	if vm, _ := out["vm"].(map[string]any); vm["state"] != "stopped" || vm["id"] != id {
		t.Fatalf("env_start_failed body vm = %v, want the stopped VM", out["vm"])
	}
	if s := e.boatdState(id); s != "stopped" {
		t.Fatalf("VM state = %s, want stopped", s)
	}
	e.boatd.mu.Lock()
	last := e.boatd.stops[len(e.boatd.stops)-1]
	e.boatd.mu.Unlock()
	if last["force"] != true {
		t.Fatalf("last stop = %v, want force", last)
	}
	snap := e.srv.store.Snapshot()
	if snap.VMEnvErrors[id] == "" {
		t.Error("the env failure was not recorded")
	}
	if n := countStopOps(snap); n != stopsBefore {
		t.Errorf("stop operations = %d, want %d: the fail-closed stop must not record one", n, stopsBefore)
	}
	// Not running: no exec, native or SSH, can source a stale /etc/boat/env.
	if st, vm := e.do(e.owner, "GET", "/v1/vms/"+id, nil); st != 200 || vm["state"] == "running" {
		t.Fatalf("VM after the failed start = %d %v, want not running", st, vm["state"])
	}
	// Not left marked: the previous run had secrets, so the next no_env start boots fresh again.
	if _, on := snap.NoEnvRuns[id]; on {
		t.Error("the failed no_env start left its mark")
	}
	e.setEnvFailures(false, false)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("retry = %d %v", st, out)
	}
	if n := e.freshBootsOf(id); n != 2 {
		t.Fatalf("fresh boots = %d, want 2 (the failed run and its retry)", n)
	}
	if !e.noEnvRunOf(id) {
		t.Fatal("retry is not a no_env run")
	}
}

// 1b: when the fail-closed stop itself fails the VM may be running: the no_env
// mark stays and the env error is recorded (the VM reads agent_ready=false).
func TestR37NoEnvStartFailedStopKeepsMark(t *testing.T) {
	e := setup(t)
	id := e.secretVM()
	e.setEnvFailures(false, true)
	e.boatd.mu.Lock()
	e.boatd.failStop = true
	e.boatd.mu.Unlock()
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true})
	if st != 502 {
		t.Fatalf("start = %d %v, want 502", st, out)
	}
	if !strings.Contains(fmt.Sprint(out), "could not be stopped") {
		t.Errorf("answer does not say the stop failed: %v", out)
	}
	if _, on := e.srv.store.Snapshot().NoEnvRuns[id]; !on {
		t.Error("mark dropped although the VM may be running")
	}
	if e.srv.store.Snapshot().VMEnvErrors[id] == "" {
		t.Error("env failure not recorded")
	}
	if e.srv.envPushPending(id) {
		t.Error("the gate leaked")
	}
}

// 1b/2: boatd's start call errors after the VM booted: a cold no_env start is
// stopped, not left running with an unknown env.
func TestR37NoEnvColdStartBoatdErrorAfterBootStopsVM(t *testing.T) {
	e := setup(t)
	id := e.secretVM()
	e.boatd.mu.Lock()
	e.boatd.failStartsAfterBoot = 1
	e.boatd.mu.Unlock()
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st < 500 {
		t.Fatalf("start = %d %v, want an upstream error", st, out)
	}
	if s := e.boatdState(id); s != "stopped" {
		t.Fatalf("VM state = %s, want stopped", s)
	}
	if _, on := e.srv.store.Snapshot().NoEnvRuns[id]; on {
		t.Error("mark left although the previous run had secrets")
	}
	if e.srv.envPushPending(id) {
		t.Error("the gate leaked")
	}
}

// 2: a plain start of a running no_env VM errors while a stop plus a read raced
// it (boatd restores the VM): the run stays marked.
func TestR37ErroringStartReMarksRunningNoEnvRun(t *testing.T) {
	e := setup(t)
	id := e.noEnvVM()
	var hit atomic.Bool
	setStartHooks(t, nil, func(s *server, vm string) {
		if vm != id || !hit.CompareAndSwap(false, true) {
			return
		}
		if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil); st != 200 {
			t.Errorf("stop = %d %v", st, out)
		}
		e.do(e.owner, "GET", "/v1/vms", nil)
	})
	e.boatd.mu.Lock()
	e.boatd.failStartsAfterBoot = 1
	e.boatd.mu.Unlock()
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{}); st < 500 {
		t.Fatalf("start = %d %v, want an upstream error", st, out)
	}
	if !hit.Load() {
		t.Fatal("hook never ran")
	}
	if s := e.boatdState(id); s != "running" {
		t.Fatalf("VM state = %s, want running (a running no_env run is re-marked, not stopped)", s)
	}
	if !e.noEnvRunOf(id) {
		t.Fatal("the erroring start dropped the no_env mark")
	}
	if st, _ := e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "hi"}); st != 200 {
		t.Fatalf("prompt = %d", st)
	}
	if v := e.lastExecEnv(); v["TEAM_SEC"] != nil {
		t.Fatalf("prompt env = %v, want no team secret", v)
	}
}

// 3, 4 (the order of a stop and the mark by sequence, the lazy mark delete): gone
// with r46. no_env is sticky, so a stop never ends it (see r46_test.go).

// 5: a compat read in the pre-start window of a resume says provisioning, though
// boatd still reads the VM as stopped.
func TestR37CompatProvisioningInPreStartWindow(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	id := e.nativeOf(bx)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil); st != 200 {
		t.Fatalf("stop = %d %v", st, out)
	}
	var seen, listed atomic.Value
	setStartHooks(t, nil, func(s *server, vm string) {
		if vm != id {
			return
		}
		if st, out, _ := e.compat(e.owner, "GET", "/api/v1/sandboxes/"+bx, nil, nil); st == 200 {
			sb, _ := out["sandbox"].(map[string]any)
			seen.Store(sb["state"])
		}
		if st, out, _ := e.compat(e.owner, "GET", "/api/v1/sandboxes", nil, nil); st == 200 {
			for _, v := range out["sandboxes"].([]any) {
				if sb := v.(map[string]any); sb["id"] == bx {
					listed.Store(sb["state"])
				}
			}
		}
	})
	if st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/resume", map[string]any{}, nil); st >= 300 {
		t.Fatalf("resume = %d %v", st, out)
	}
	if seen.Load() != "provisioning" || listed.Load() != "provisioning" {
		t.Fatalf("pre-start window: get %v, list %v, want provisioning", seen.Load(), listed.Load())
	}
	if sb := e.ok("GET", "/api/v1/sandboxes/"+bx, nil, 200)["sandbox"].(map[string]any); sb["state"] == "provisioning" {
		t.Fatalf("still provisioning after the start settled: %v", sb["state"])
	}
}

// A pending credential transition whose stop failed refuses commands and prompts
// without falsely reporting an in-flight env push (env_pending=false, compat error).
func TestR37PendingTransitionRefusesPromptsWithoutEnvPending(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	id := e.nativeOf(bx)
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	e.setEnvFailures(false, true)
	e.boatd.mu.Lock()
	e.boatd.failStop = true
	e.boatd.mu.Unlock()
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != 502 {
		t.Fatalf("start = %d %v, want 502", st, out)
	}
	_, vm := e.do(e.owner, "GET", "/v1/vms/"+id, nil)
	if vm["env_pending"] != false || vm["agent_ready"] != false {
		t.Fatalf("GET after failed stop = %v, want env_pending=false agent_ready=false", vm)
	}
	if sb := e.ok("GET", "/api/v1/sandboxes/"+bx, nil, 200)["sandbox"].(map[string]any); sb["state"] != "error" {
		t.Fatalf("compat state after failed stop = %v, want error", sb["state"])
	}
	for _, route := range []struct {
		path string
		body map[string]any
	}{
		{"/v1/vms/" + id + "/commands", map[string]any{"cmd": []string{"true"}}},
		{"/v1/vms/" + id + "/prompts", map[string]any{"prompt": "hi"}},
		{"/v1/vms/" + id + "/prompt", map[string]any{"prompt": "hi"}},
	} {
		st, out := e.do(e.owner, "POST", route.path, route.body)
		if st != 409 || nativeErrCode(out) != "sandbox_not_ready" {
			t.Fatalf("POST %s = %d %v, want 409 sandbox_not_ready", route.path, st, out)
		}
	}
}
