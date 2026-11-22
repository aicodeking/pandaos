package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// r34: the legacy /prompt honours the not-ready gate, recover and any managed
// restart keep a no_env run, an in-flight start never lets a no_env run end,
// env_pending is exposed, and forgetVM drops the credential push lock.

// sawTeamSecret reports whether any exec or command boatd saw carried TEAM_SEC.
func (e *env) sawTeamSecret() bool {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	for _, x := range e.boatd.execs {
		if m, _ := x["env"].(map[string]any); m["TEAM_SEC"] != nil {
			return true
		}
	}
	for _, c := range e.boatd.cmds {
		if c.env["TEAM_SEC"] != nil {
			return true
		}
	}
	return false
}

// 1: the legacy POST /v1/vms/{id}/prompt refuses (409 sandbox_not_ready) a VM
// whose environment never reached the guest, like /prompts.
func TestR34LegacyPromptRefusedWhileNotReady(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	e.setEnvFailures(true, false)
	id := e.nativeOf(e.newSandbox(nil))
	e.setEnvFailures(false, false)
	for _, path := range []string{"/prompt", "/prompts"} {
		st, out := e.do(e.owner, "POST", "/v1/vms/"+id+path, map[string]any{"prompt": "hi"})
		if st != http.StatusConflict || nativeErrCode(out) != "sandbox_not_ready" {
			t.Fatalf("POST %s of an env-error VM = %d %v, want 409 sandbox_not_ready", path, st, out)
		}
	}
	if e.sawTeamSecret() {
		t.Fatal("a refused prompt still ran with team secrets")
	}
	// Control: a healthy VM's legacy prompt still runs.
	ok := e.createVM()
	if st, _ := e.raw(e.owner, "POST", "/v1/vms/"+ok+"/prompt", map[string]any{"prompt": "hi"}); st != 200 {
		t.Fatalf("healthy legacy prompt = %d", st)
	}
}

// 2: compat recover (force stop + start) of a no_env run restarts it no_env: no
// team secrets reach the guest and the run stays marked.
func TestR34RecoverKeepsNoEnvRun(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	id := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("no_env start = %d", st)
	}
	if st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/recover", nil, nil); st != 200 {
		t.Fatalf("recover = %d %v", st, out)
	}
	if env, _ := e.lastStart()["env"].(map[string]any); env["TEAM_SEC"] != nil {
		t.Fatalf("recover's start env = %v, want no team secrets", env)
	}
	st, vm := e.do(e.owner, "GET", "/v1/vms/"+id, nil)
	if st != 200 || vm["no_env_run"] != true {
		t.Fatalf("GET after recover = %d %v, want no_env_run=true", st, vm)
	}
	if st, _ := e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "hi"}); st != 200 {
		t.Fatalf("prompt after recover = %d", st)
	}
	if v := e.lastExecEnv(); v["TEAM_SEC"] != nil {
		t.Fatalf("prompt after recover env = %v, want no team secrets", v)
	}
	// Control: a normal run's recover restarts it with its secrets, unmarked.
	id2 := e.createVM()
	if st, _, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id2+"/recover", nil, nil); st != 200 {
		t.Fatalf("control recover = %d", st)
	}
	if env, _ := e.lastStart()["env"].(map[string]any); env["TEAM_SEC"] != "s3" {
		t.Fatalf("control recover env = %v, want team secrets", env)
	}
}

// 2 (structural): a start marked as a managed restart keeps a still-marked no_env
// run no_env even when the restarting path's body says nothing.
func TestR34ManagedRestartKeepsNoEnvStructurally(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	id := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true})
	p := e.srv.authenticate("Bearer " + e.owner)
	if p == nil {
		t.Fatal("owner not authenticated")
	}
	req := func(managed bool) *http.Request {
		r := httptest.NewRequest("POST", "/", nil)
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, &reqInfo{id: "r34"}))
		if managed {
			r = withManagedRestart(r)
		}
		return r
	}
	// Managed stop (no stop op recorded), then a bodyless managed start.
	if res := e.srv.callNative(withManagedStop(req(false)), p, "POST /v1/vms/{id}/stop", e.srv.stopVM, id, map[string]any{"force": true}); !res.ok() {
		t.Fatalf("stop: %s", res.body.String())
	}
	if res := e.srv.callNative(req(true), p, "POST /v1/vms/{id}/start", e.srv.startVM, id, nil); !res.ok() {
		t.Fatalf("start: %s", res.body.String())
	}
	if env, _ := e.lastStart()["env"].(map[string]any); env["TEAM_SEC"] != nil {
		t.Fatalf("managed restart env = %v, want no team secrets", env)
	}
	if _, ok := e.srv.store.Snapshot().NoEnvRuns[id]; !ok {
		t.Fatal("managed restart dropped the no_env mark")
	}
}

// 3: a stop that completes while a no_env cold start is in flight does not end
// the run: prompts are refused until the start settles, nothing runs with team
// secrets, and the run is still no_env afterwards.
func TestR34InFlightNoEnvStartKeepsMark(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	id := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	g := e.holdBoatd(false)
	var once sync.Once
	releaseStart := func() { once.Do(func() { close(g.release) }) }
	t.Cleanup(releaseStart)
	done := make(chan int, 1)
	go func() {
		st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true})
		done <- st
	}()
	select {
	case <-g.hit:
	case <-time.After(5 * time.Second):
		t.Fatal("start never reached boatd")
	}
	if !e.srv.envPushPending(id) {
		t.Fatal("a cold start in flight does not hold the not-ready gate")
	}
	// A stop completes meanwhile, and boatd has the VM running by now.
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil); st != 200 {
		t.Fatalf("stop = %d", st)
	}
	e.boatd.mu.Lock()
	e.boatd.vms[id]["state"] = "running"
	e.boatd.vms[id]["agent_ready"] = true
	e.boatd.mu.Unlock()
	if !e.srv.noEnvRun(id) {
		t.Fatal("a stop completing during the start ended the no_env run")
	}
	if _, ok := e.srv.store.Snapshot().NoEnvRuns[id]; !ok {
		t.Fatal("the no_env mark was dropped during the start")
	}
	_, vm := e.do(e.owner, "GET", "/v1/vms/"+id, nil)
	if vm["env_pending"] != true {
		t.Fatalf("GET during the start = %v, want env_pending=true", vm)
	}
	for _, path := range []string{"/prompt", "/prompts"} {
		st, out := e.do(e.owner, "POST", "/v1/vms/"+id+path, map[string]any{"prompt": "hi"})
		if st != http.StatusConflict || nativeErrCode(out) != "sandbox_not_ready" {
			t.Fatalf("POST %s during the start = %d %v, want 409 sandbox_not_ready", path, st, out)
		}
	}
	releaseStart()
	select {
	case st := <-done:
		if st != 200 {
			t.Fatalf("start = %d", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start never finished")
	}
	if e.sawTeamSecret() {
		t.Fatal("something ran with team secrets during the no_env start window")
	}
	_, vm = e.do(e.owner, "GET", "/v1/vms/"+id, nil)
	if vm["no_env_run"] != true || vm["env_pending"] != false {
		t.Fatalf("GET after the start = %v, want no_env_run=true env_pending=false", vm)
	}
	if st, _ := e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "hi"}); st != 200 {
		t.Fatalf("prompt after the start = %d", st)
	}
	if v := e.lastExecEnv(); v["TEAM_SEC"] != nil {
		t.Fatalf("prompt env = %v, want no team secrets", v)
	}
	if e.srv.envPushPending(id) {
		t.Fatal("pending mark leaked")
	}
}

// 3: every cold start holds the gate, even one whose env is empty; a stale mark
// of an earlier run is still honored (fail closed) while the start is in flight.
func TestR34ColdStartHoldsGateWithoutEnv(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	g := e.holdBoatd(false)
	t.Cleanup(func() {
		select {
		case <-g.release:
		default:
			close(g.release)
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil)
	}()
	select {
	case <-g.hit:
	case <-time.After(5 * time.Second):
		t.Fatal("start never reached boatd")
	}
	if !e.srv.envPushPending(id) {
		t.Fatal("a cold start with no env does not hold the gate")
	}
	close(g.release)
	<-done
	if e.srv.envPushPending(id) {
		t.Fatal("gate leaked after the start")
	}
}

// 4: env_pending is on VM bodies: false at rest.
func TestR34EnvPendingFalseAtRest(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	st, vm := e.do(e.owner, "GET", "/v1/vms/"+id, nil)
	if st != 200 || vm["env_pending"] != false {
		t.Fatalf("GET = %d %v, want env_pending=false", st, vm)
	}
	st, out := e.do(e.owner, "GET", "/v1/vms", nil)
	list, _ := out["vms"].([]any)
	if st != 200 || len(list) == 0 || list[0].(map[string]any)["env_pending"] != false {
		t.Fatalf("list = %d %v, want env_pending=false", st, out)
	}
}

// 6: deleting a VM drops its credential push lock.
func TestR34ForgetVMDropsCredentialPushLock(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	e.srv.store.credentialPushes.Store(id, new(struct{}))
	e.srv.forgetVM(id)
	if _, ok := e.srv.store.credentialPushes.Load(id); ok {
		t.Fatal("credentialPushes kept the deleted VM's entry")
	}
}
