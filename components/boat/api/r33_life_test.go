package main

import (
	"net/http"
	"testing"
)

func (e *env) failNextStarts(n int) {
	e.boatd.mu.Lock()
	e.boatd.failStarts = n
	e.boatd.mu.Unlock()
}

func (e *env) vmEnv(id string) map[string]any {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	m, _ := e.boatd.vms[id]["env"].(map[string]any)
	return m
}

func (e *env) noEnvRunOf(id string) bool {
	e.t.Helper()
	st, out := e.do(e.owner, "GET", "/v1/vms/"+id, nil)
	if st != 200 {
		e.t.Fatalf("GET %s = %d %v", id, st, out)
	}
	v, _ := out["no_env_run"].(bool)
	return v
}

// noEnvVM is a running VM started with no_env, with a team secret to leak.
func (e *env) noEnvVM() string {
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	id := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != 200 {
		e.t.Fatalf("no_env start = %d %v", st, out)
	}
	return id
}

// 1: a successful retype of a running no_env run restarts it no_env: the caller
// did not send noEnv, yet no team secret reaches the guest (caller env stays).
func TestR33RetypeSuccessKeepsNoEnv(t *testing.T) {
	// r46: the type changes while the sandbox is stopped (a running one answers 409);
	// no_env is sticky, so the start that follows stays no_env without noEnv sent.
	e := setup(t)
	id := e.noEnvVM()
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/resume",
		map[string]any{"type": "large", "env": map[string]string{"NEW": "1"}}, nil)
	if st != http.StatusAccepted {
		t.Fatalf("resume --type = %d %v", st, out)
	}
	start := e.lastStart()
	envm, _ := start["env"].(map[string]any)
	if envm["TEAM_SEC"] != nil || envm["NEW"] != "1" {
		t.Fatalf("restart env = %v, want no team secrets and the caller's NEW", envm)
	}
	if e.boatd.vms[id]["size"] != "large" {
		t.Fatalf("size = %v, want large", e.boatd.vms[id]["size"])
	}
	if !e.noEnvRunOf(id) {
		t.Fatal("retype ended the no_env mark")
	}
	if st, _ := e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "hi"}); st != 200 {
		t.Fatalf("prompt = %d", st)
	}
	if v := e.lastExecEnv(); v["TEAM_SEC"] != nil {
		t.Fatalf("prompt env after retype = %v, want no team secrets", v)
	}
	// Control: a normal sandbox's retype brings the secrets.
	id2 := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+id2+"/stop", nil)
	e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id2+"/resume", map[string]any{"type": "small"}, nil)
	envm, _ = e.lastStart()["env"].(map[string]any)
	if envm["TEAM_SEC"] != "s3" || e.noEnvRunOf(id2) {
		t.Fatalf("normal sandbox retype env = %v, want team secrets", envm)
	}
}

// 2: a start that fails after a successful resize leaves the VM running as it
// was (no_env, previous ttl; the new size stays) and answers the start's error.
func TestR33FailedStartAfterResizeRestoresRun(t *testing.T) {
	// r46: a resume with a type on a running sandbox is refused (409) before the
	// stop, so there is no failed start to restore from; the no_env mark and ttl stay.
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	_, vm := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "n", "size": "small", "ttl_s": 4321})
	id := vm["id"].(string)
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true, "ttl_s": 4321})
	e.failNextStarts(1)
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/resume",
		map[string]any{"type": "large", "env": map[string]string{"NEW": "1"}}, nil)
	if st != http.StatusConflict {
		t.Fatalf("resume with a type on a running sandbox = %d %v, want 409", st, out)
	}
	e.boatd.mu.Lock()
	state, size := e.boatd.vms[id]["state"], e.boatd.vms[id]["size"]
	e.boatd.mu.Unlock()
	if state != "running" || size != "small" {
		t.Fatalf("VM = %v/%v, want running/small", state, size)
	}
	if !e.noEnvRunOf(id) {
		t.Fatal("the refused resume ended the no_env mark")
	}
}

// 3: a fork of a no_env run is a no_env child (no team secrets, marked), unless
// the caller sends no_env:false.
func TestR33ForkOfNoEnvRunIsNoEnv(t *testing.T) {
	e := setup(t)
	parent := e.noEnvVM()
	st, kid := e.do(e.owner, "POST", "/v1/vms/"+parent+"/fork", map[string]any{"name": "kid"})
	if st != 201 {
		t.Fatalf("fork = %d %v", st, kid)
	}
	cid := kid["id"].(string)
	if v := e.vmEnv(cid); v["TEAM_SEC"] != nil {
		t.Fatalf("child env = %v, want no team secrets", v)
	}
	if !e.noEnvRunOf(cid) {
		t.Fatal("child of a no_env run is not a no_env run")
	}
	if st, _ := e.raw(e.owner, "POST", "/v1/vms/"+cid+"/prompt", map[string]any{"prompt": "hi"}); st != 200 {
		t.Fatalf("child prompt = %d", st)
	}
	if v := e.lastExecEnv(); v["TEAM_SEC"] != nil {
		t.Fatalf("child prompt env = %v, want no team secrets", v)
	}
	// The compat fork (with a type change on top) keeps it too.
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+parent+"/fork", map[string]any{"name": "kid2", "type": "large"}, nil)
	if st != http.StatusAccepted {
		t.Fatalf("compat fork --type = %d %v", st, out)
	}
	c2 := out["id"].(string)
	if v := e.vmEnv(c2); v["TEAM_SEC"] != nil {
		t.Fatalf("retyped child env = %v, want no team secrets", v)
	}
	envm, _ := e.lastStart()["env"].(map[string]any)
	if envm["TEAM_SEC"] != nil {
		t.Fatalf("retyped child restart env = %v, want no team secrets", envm)
	}
	if !e.noEnvRunOf(c2) {
		t.Fatal("retyped child is not a no_env run")
	}
	// r46: a no_env parent's fork is always no_env: no_env:false is a 400 no_env_sticky.
	st, kid = e.do(e.owner, "POST", "/v1/vms/"+parent+"/fork", map[string]any{"name": "kid3", "no_env": false})
	if st != 400 || kid["error"].(map[string]any)["code"] != "no_env_sticky" {
		t.Fatalf("fork no_env:false of a no_env parent = %d %v, want 400 no_env_sticky", st, kid)
	}
	// Control: a normal parent forks a normal child.
	normal := e.createVM()
	_, k := e.do(e.owner, "POST", "/v1/vms/"+normal+"/fork", map[string]any{"name": "kid4"})
	if v := e.vmEnv(k["id"].(string)); v["TEAM_SEC"] != "s3" || e.noEnvRunOf(k["id"].(string)) {
		t.Fatalf("normal child env = %v, want team secrets, not no_env", v)
	}
}

// 4: a stop that completes while a no_env cold start is still inside boatd does
// not end the run the start then begins.
func TestR33StopRacingNoEnvColdStartKeepsRun(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	id := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	g := e.holdBoatd(false)
	done := make(chan int, 1)
	go func() {
		st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true})
		done <- st
	}()
	<-g.hit
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil); st != 200 {
		t.Fatalf("racing stop = %d", st)
	}
	e.do(e.owner, "GET", "/v1/vms/"+id, nil) // the read that lazily ends a marked run
	close(g.release)
	if st := <-done; st != 200 {
		t.Fatalf("held start = %d", st)
	}
	if !e.noEnvRunOf(id) {
		t.Fatal("a stop racing the cold start ended the no_env run")
	}
	if st, _ := e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "hi"}); st != 200 {
		t.Fatalf("prompt = %d", st)
	}
	if v := e.lastExecEnv(); v["TEAM_SEC"] != nil {
		t.Fatalf("prompt env = %v, want no team secrets", v)
	}
	// r46: a stop after the start does not end no_env: it is sticky.
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if !e.noEnvRunOf(id) {
		t.Fatal("a later stop ended the no_env mark")
	}
}
