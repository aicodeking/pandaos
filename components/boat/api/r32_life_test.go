package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// r32: legacy prompt honours no_env, retype rollback keeps the run's no_env and
// ttl, compat is provisioning while the env push is in flight, /limits counts
// env-error sandboxes.

func (e *env) lastExecEnv() map[string]any {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	if len(e.boatd.execs) == 0 {
		return nil
	}
	v, _ := e.boatd.execs[len(e.boatd.execs)-1]["env"].(map[string]any)
	return v
}

func (e *env) lastStart() map[string]any {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	if len(e.boatd.starts) == 0 {
		return nil
	}
	return e.boatd.starts[len(e.boatd.starts)-1]
}

func (e *env) setResizeTimeout(on bool) {
	e.boatd.mu.Lock()
	e.boatd.resizeTimeout = on
	e.boatd.mu.Unlock()
}

// 1: the legacy POST /v1/vms/{id}/prompt of a no_env run carries no team secrets.
func TestR32LegacyPromptOfNoEnvRunHoldsNoTeamSecrets(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	id := e.createVM()
	// Control: a normal run's legacy prompt carries them.
	if st, _ := e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "hi"}); st != 200 {
		t.Fatalf("control prompt = %d", st)
	}
	if v := e.lastExecEnv(); v["TEAM_SEC"] != "s3" {
		t.Fatalf("control env = %v, want TEAM_SEC", v)
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("no_env start = %d", st)
	}
	if st, _ := e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "hi"}); st != 200 {
		t.Fatalf("no_env prompt = %d", st)
	}
	if v := e.lastExecEnv(); v["TEAM_SEC"] != nil {
		t.Fatalf("no_env run's legacy prompt env = %v, want no team secrets", v)
	}
}

// 2: a retype whose resize fails after the stop restarts the VM as it was: a
// no_env run stays no_env (no team secrets, mark kept), the ttl is kept, and the
// request's env is not applied.
func TestR32RetypeRollbackKeepsNoEnvAndTTL(t *testing.T) {
	// r46: a running sandbox is not stopped and resized by a resume with a type (409,
	// no side effects), so there is no run to roll back; the no_env mark is sticky
	// and stays, and a type change on the stopped sandbox keeps it no_env.
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	st, vm := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "n", "size": "small", "ttl_s": 4321})
	if st != 201 {
		t.Fatalf("create = %d %v", st, vm)
	}
	id := vm["id"].(string)
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true, "ttl_s": 4321}); st != 200 {
		t.Fatalf("no_env start = %d", st)
	}
	e.boatd.mu.Lock()
	startsBefore := len(e.boatd.starts)
	e.boatd.mu.Unlock()
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/resume",
		map[string]any{"type": "large", "env": map[string]string{"NEW": "1"}}, nil)
	if st != http.StatusConflict || errCode(out) != "sandbox_running_stop_first" {
		t.Fatalf("resume with a type on a running sandbox = %d %v, want 409 sandbox_running_stop_first", st, out)
	}
	e.boatd.mu.Lock()
	state, size, starts := e.boatd.vms[id]["state"], e.boatd.vms[id]["size"], len(e.boatd.starts)
	e.boatd.mu.Unlock()
	if state != "running" || size != "small" || starts != startsBefore {
		t.Fatalf("the refused resume touched the VM: %v/%v, starts %d->%d", state, size, startsBefore, starts)
	}
	if _, ok := e.srv.store.Snapshot().NoEnvRuns[id]; !ok {
		t.Fatal("the refused resume cleared the no_env mark")
	}
}

// 2: an unknown size in boatd's catalogue is a 400 that never stops the VM.
func TestR32RetypeUnknownSizeDoesNotTouchVM(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"type": "gigantic"}, nil)
	if st != 409 { // r46: a running sandbox is not retyped by resume, so its type is not even judged
		t.Fatalf("resume unknown type = %d %v, want 409", st, out)
	}
	e.boatd.mu.Lock()
	if e.boatd.vms[id]["state"] != "running" || len(e.boatd.stops) != 0 || len(e.boatd.starts) != 0 {
		t.Fatalf("VM touched: state %v stops %v starts %v", e.boatd.vms[id]["state"], e.boatd.stops, e.boatd.starts)
	}
	e.boatd.mu.Unlock()
	// Stopped, the unknown type is the 400 it was.
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"type": "gigantic"}, nil); st != 400 {
		t.Fatalf("stopped resume unknown type = %d %v, want 400", st, out)
	}
}

// 3: while a create's env push is in flight the compat sandbox is provisioning
// (GET and list) and commands are refused with 409 sandbox_not_ready.
func TestR32CompatProvisioningWhileEnvPushPending(t *testing.T) {
	e := setup(t)
	g := e.holdBoatd(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "w"})
	}()
	var id string
	select {
	case id = <-g.hit:
	case <-time.After(5 * time.Second):
		t.Fatal("env push never reached boatd")
	}
	sb := e.ok("GET", "/api/v1/sandboxes/"+id, nil, 200)["sandbox"].(map[string]any)
	if sb["state"] != "provisioning" {
		t.Fatalf("GET during the push = %v, want provisioning", sb["state"])
	}
	list := e.ok("GET", "/api/v1/sandboxes", nil, 200)["sandboxes"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["state"] != "provisioning" {
		t.Fatalf("list during the push = %v, want provisioning", list)
	}
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/commands", map[string]any{"command": "echo hi"}, nil)
	if st != http.StatusConflict || !strings.Contains(errCode(out), "sandbox_not_ready") {
		t.Fatalf("command during the push = %d %v, want 409 sandbox_not_ready", st, out)
	}
	close(g.release)
	<-done
	sb = e.ok("GET", "/api/v1/sandboxes/"+id, nil, 200)["sandbox"].(map[string]any)
	if sb["state"] != "ready" {
		t.Fatalf("GET after the push = %v, want ready", sb["state"])
	}
	if st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/commands", map[string]any{"command": "echo hi"}, nil); st != 200 {
		t.Fatalf("command after the push = %d %v", st, out)
	}
	if e.srv.envPushPending(id) {
		t.Fatal("pending mark leaked")
	}
}

// 4: /limits counts an env-error sandbox: boatd's admission counts every running
// VM, so it holds its slot until stopped or deleted.
func TestR32LimitsCountEnvErrorUntilStopped(t *testing.T) {
	e := setup(t)
	e.setEnvFailures(true, false)
	bad := e.nativeOf(e.newSandbox(nil))
	e.setEnvFailures(false, false)
	if sb := e.ok("GET", "/api/v1/sandboxes/"+bad, nil, 200)["sandbox"].(map[string]any); sb["state"] != "error" {
		t.Fatalf("setup: state %v, want error", sb["state"])
	}
	if lim := e.ok("GET", "/api/v1/limits", nil, 200); lim["activeSandboxes"] != float64(1) {
		t.Fatalf("limits = %v, want the env-error sandbox counted", lim)
	}
	e.do(e.owner, "POST", "/v1/vms/"+bad+"/stop", nil)
	if lim := e.ok("GET", "/api/v1/limits", nil, 200); lim["activeSandboxes"] != float64(0) {
		t.Fatalf("limits after stop = %v, want 0", lim)
	}
}
