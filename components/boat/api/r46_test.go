package main

import (
	"net/http"
	"slices"
	"testing"
)

// r46: no_env is sticky (boat.dev's), a no_env start or fork never replays the
// stored per-sandbox env, a fork without a ttl gets one hour, and a compat resume
// of a running sandbox that carries environment, noEnv or type is 409.

func (e *env) lastForkTTL() (any, bool) {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	if len(e.boatd.forks) == 0 {
		return nil, false
	}
	v, ok := e.boatd.forks[len(e.boatd.forks)-1]["ttl_s"]
	return v, ok
}

func (e *env) startCount() int {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	return len(e.boatd.starts)
}

func (e *env) vmCount() int {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	return len(e.boatd.vms)
}

func r46Code(out map[string]any) string {
	if m, ok := out["error"].(map[string]any); ok {
		c, _ := m["code"].(string)
		return c
	}
	return ""
}

// A stop, a plain start, a type change and a recover keep no_env, push no team
// secrets and (sticky) need no fresh boot; the mark survives a restart of the server.
func TestR46NoEnvStickyAcrossStopStartRecoverAndRestart(t *testing.T) {
	e := setup(t)
	id := e.noEnvVM()
	fresh := e.freshBootsOf(id)
	for i := 0; i < 2; i++ {
		if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil); st != 200 {
			t.Fatalf("stop = %d %v", st, out)
		}
		if !e.noEnvRunOf(id) {
			t.Fatal("a stop ended no_env")
		}
		if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{}); st != 200 {
			t.Fatalf("plain start = %d %v", st, out)
		}
		if got, _ := e.lastStart()["env"].(map[string]any); got["TEAM_SEC"] != nil {
			t.Fatalf("a plain start of a no_env VM pushed team secrets: %v", got)
		}
		if !e.noEnvRunOf(id) {
			t.Fatal("a plain start ended no_env")
		}
	}
	if e.freshBootsOf(id) != fresh {
		t.Fatal("a plain start of a no_env VM cold-booted: its memory holds no team secrets")
	}
	bx := id
	e.ok("POST", "/api/v1/sandboxes/"+bx+"/recover", nil, 200)
	if got, _ := e.lastStart()["env"].(map[string]any); got["TEAM_SEC"] != nil || !e.noEnvRunOf(id) {
		t.Fatalf("recover: env %v, no_env %v", got, e.noEnvRunOf(id))
	}
	if st, _ := e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "hi"}); st != 200 {
		t.Fatalf("prompt = %d", st)
	}
	if v := e.lastExecEnv(); v["TEAM_SEC"] != nil {
		t.Fatalf("prompt env = %v, want no team secrets", v)
	}
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	back, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if _, ok := back.Snapshot().NoEnvRuns[id]; !ok {
		t.Fatal("the no_env mark did not survive a restart")
	}
}

// A start that names an environment, or sends no_env:false, on a no_env VM is a
// 400 no_env_sticky before anything happens.
func TestR46StickyNoEnvRefusesEnvironmentAndNoEnvFalse(t *testing.T) {
	e := setup(t)
	e.saveEnv("prod", map[string]string{"E1": "v1"}, nil)
	id := e.noEnvVM()
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	starts := e.startCount()
	for name, body := range map[string]map[string]any{
		"environment":       {"environment": "prod"},
		"unknown":           {"environment": "nope"},
		"no_env false":      {"no_env": false},
		"environment+false": {"environment": "prod", "no_env": false},
	} {
		st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", body)
		if st != 400 || r46Code(out) != "no_env_sticky" {
			t.Errorf("%s: start = %d %v, want 400 no_env_sticky", name, st, out)
		}
	}
	if e.startCount() != starts || e.boatdState(id) != "stopped" {
		t.Fatalf("a refused start reached boatd (starts %d -> %d, state %s)", starts, e.startCount(), e.boatdState(id))
	}
	// no_env:true and a plain start stay allowed.
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("no_env:true start = %d %v", st, out)
	}
	// A normal VM takes an environment as before.
	n := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+n+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+n+"/start", map[string]any{"environment": "prod", "no_env": false}); st != 200 {
		t.Fatalf("a normal VM's start with an environment = %d %v", st, out)
	}
	if e.noEnvRunOf(n) {
		t.Fatal("an environment start marked a normal VM no_env")
	}
}

// Fork, clone and create-from-snapshot of a no_env VM are no_env; an environment
// or no_env:false on them is 400 no_env_sticky and copies nothing.
func TestR46ForkAndCreateFromSnapshotInheritNoEnv(t *testing.T) {
	e := setup(t)
	e.saveEnv("prod", map[string]string{"E1": "v1"}, nil)
	parent := e.noEnvVM()
	e.do(e.owner, "POST", "/v1/vms/"+parent+"/stop", nil) // a stopped parent forks no_env too
	vms := e.vmCount()
	for name, body := range map[string]map[string]any{
		"no_env false": {"name": "k", "no_env": false},
		"environment":  {"name": "k", "environment": "prod"},
	} {
		for _, path := range []string{"/fork", "/clone"} {
			st, out := e.do(e.owner, "POST", "/v1/vms/"+parent+path, body)
			if st != 400 || r46Code(out) != "no_env_sticky" {
				t.Errorf("%s %s = %d %v, want 400 no_env_sticky", name, path, st, out)
			}
		}
	}
	if e.vmCount() != vms {
		t.Fatal("a refused fork copied the VM")
	}
	for _, path := range []string{"/fork", "/clone"} {
		st, kid := e.do(e.owner, "POST", "/v1/vms/"+parent+path, map[string]any{"name": "kid" + path[1:2]})
		if st != 201 {
			t.Fatalf("%s = %d %v", path, st, kid)
		}
		cid := kid["id"].(string)
		if !e.noEnvRunOf(cid) || e.vmEnv(cid)["TEAM_SEC"] != nil {
			t.Fatalf("%s child: no_env %v, env %v", path, e.noEnvRunOf(cid), e.vmEnv(cid))
		}
		// The child stays no_env across stop and a plain start.
		e.do(e.owner, "POST", "/v1/vms/"+cid+"/stop", nil)
		e.do(e.owner, "POST", "/v1/vms/"+cid+"/start", map[string]any{})
		if !e.noEnvRunOf(cid) {
			t.Fatalf("%s child lost no_env", path)
		}
	}

	// Create from a snapshot of the no_env VM: no_env, no team secrets, no default
	// environment; naming one is refused.
	e.do(e.owner, "POST", "/v1/vms/"+parent+"/start", map[string]any{})
	sid := e.r39Snapshot(parent, "no-env-snap")
	if rec := e.snapRecord(sid); !slices.Contains(rec, snapNoEnv) {
		t.Fatalf("record = %v, want @no_env", rec)
	}
	if st, out := e.createFrom(sid, map[string]any{"environment": "prod"}); st != 400 || r46Code(out) != "no_env_sticky" {
		t.Fatalf("create from a no_env snapshot with an environment = %d %v, want 400 no_env_sticky", st, out)
	}
	st, out := e.createFrom(sid, nil)
	if st != 201 {
		t.Fatalf("create from the snapshot = %d %v", st, out)
	}
	cid := out["id"].(string)
	if !e.noEnvRunOf(cid) || e.vmEnv(cid)["TEAM_SEC"] != nil {
		t.Fatalf("created from a no_env snapshot: no_env %v, env %v", e.noEnvRunOf(cid), e.vmEnv(cid))
	}

	// A snapshot of a normal VM is not inherited.
	n := e.createVM()
	nsid := e.r39Snapshot(n, "normal-snap")
	st, out = e.createFrom(nsid, nil)
	if st != 201 || e.noEnvRunOf(out["id"].(string)) {
		t.Fatalf("create from a normal snapshot = %d %v, want a normal VM", st, out)
	}
}

// A no_env create is sticky from the start.
func TestR46NoEnvCreateIsSticky(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "ne", "no_env": true})
	if st != 201 {
		t.Fatalf("create = %d %v", st, out)
	}
	id := out["id"].(string)
	if !e.noEnvRunOf(id) {
		t.Fatal("a no_env create is not marked")
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{}); st != 200 {
		t.Fatalf("start = %d %v", st, out)
	}
	if got, _ := e.lastStart()["env"].(map[string]any); got["TEAM_SEC"] != nil || !e.noEnvRunOf(id) {
		t.Fatalf("plain start of a no_env create: env %v, no_env %v", got, e.noEnvRunOf(id))
	}
}

// The per-sandbox env stored for a VM is not replayed to a no_env start or fork;
// only an env on that request is used, and it is not stored.
func TestR46NoEnvStartAndForkDoNotReplayStoredEnv(t *testing.T) {
	e := setup(t)
	st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "w", "env": map[string]string{"STORED": "old"}})
	if st != 201 {
		t.Fatalf("create = %d %v", st, out)
	}
	id := out["id"].(string)
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{})
	if got, _ := e.lastStart()["env"].(map[string]any); got["STORED"] != "old" {
		t.Fatalf("control: a normal start replays the stored env, got %v", got)
	}
	// A no_env fork takes none of it; its own env is used and not stored.
	st, kid := e.do(e.owner, "POST", "/v1/vms/"+id+"/fork", map[string]any{"name": "k1", "no_env": true, "env": map[string]string{"ONLY": "1"}})
	if st != 201 {
		t.Fatalf("no_env fork = %d %v", st, kid)
	}
	k1 := kid["id"].(string)
	if v := e.vmEnv(k1); v["STORED"] != nil || v["ONLY"] != "1" {
		t.Fatalf("no_env fork env = %v, want ONLY and no STORED", v)
	}
	if _, ok := e.srv.store.Snapshot().VMExtraEnv[k1]; ok {
		t.Fatal("a no_env fork stored its env")
	}
	st, kid = e.do(e.owner, "POST", "/v1/vms/"+id+"/fork", map[string]any{"name": "k2", "no_env": true})
	if st != 201 {
		t.Fatalf("no_env fork = %d %v", st, kid)
	}
	if v := e.vmEnv(kid["id"].(string)); v["STORED"] != nil {
		t.Fatalf("a no_env fork replayed the parent's stored env: %v", v)
	}
	// A no_env start (cold, then running) likewise.
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true, "env": map[string]string{"RUN": "1"}}); st != 200 {
		t.Fatalf("no_env start = %d %v", st, out)
	}
	got, _ := e.lastStart()["env"].(map[string]any)
	if got["STORED"] != nil || got["RUN"] != "1" {
		t.Fatalf("no_env start env = %v, want RUN and no STORED", got)
	}
	if stored := e.srv.store.Snapshot().VMExtraEnv[id]; stored["STORED"] != "old" || stored["RUN"] != "" {
		t.Fatalf("stored env after a no_env start = %v, want it unchanged", stored)
	}
	// Later plain starts of the (now sticky) VM do not replay it either.
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{})
	if got, _ := e.lastStart()["env"].(map[string]any); got["STORED"] != nil {
		t.Fatalf("a plain start of a no_env VM replayed the stored env: %v", got)
	}
}

// A fork or clone without a ttl gets one hour, whatever the parent's auto-stop;
// an explicit value (0 included) is kept.
func TestR46ForkWithoutTTLGetsOneHour(t *testing.T) {
	e := setup(t)
	st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "p"}) // no ttl: auto-stop off
	if st != 201 {
		t.Fatalf("create = %d %v", st, out)
	}
	parent := out["id"].(string)
	for _, path := range []string{"/fork", "/clone"} {
		st, kid := e.do(e.owner, "POST", "/v1/vms/"+parent+path, map[string]any{"name": "a" + path[1:2]})
		if st != 201 {
			t.Fatalf("%s = %d %v", path, st, kid)
		}
		if v, ok := e.lastForkTTL(); !ok || v != float64(3600) {
			t.Fatalf("%s without ttl_s sent ttl_s = %v (%v), want 3600", path, v, ok)
		}
		if ttl := e.boatd.vms[kid["id"].(string)]["ttl_s"]; ttl != float64(3600) {
			t.Fatalf("%s child ttl = %v, want 3600", path, ttl)
		}
	}
	for _, c := range []struct {
		body map[string]any
		want float64
	}{{map[string]any{"name": "z", "ttl_s": 0}, 0}, {map[string]any{"name": "y", "ttl_s": 120}, 120}} {
		if st, kid := e.do(e.owner, "POST", "/v1/vms/"+parent+"/fork", c.body); st != 201 {
			t.Fatalf("fork %v = %d %v", c.body, st, kid)
		}
		if v, ok := e.lastForkTTL(); !ok || v != c.want {
			t.Fatalf("fork %v sent ttl_s = %v (%v), want %v", c.body, v, ok, c.want)
		}
	}
}

func TestR46CompatForkWithoutTTLGetsOneHour(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil) // no ttl
	base := "/api/v1/sandboxes/" + bx
	out := e.ok("POST", base+"/fork", map[string]any{"name": "c1"}, 202)
	if v, ok := e.lastForkTTL(); !ok || v != float64(3600) {
		t.Fatalf("compat fork without ttlSeconds sent ttl_s = %v (%v), want 3600", v, ok)
	}
	if sb := out["sandbox"].(map[string]any); sb["ttlSeconds"] != float64(3600) {
		t.Fatalf("child ttlSeconds = %v, want 3600", sb["ttlSeconds"])
	}
	for name, ttl := range map[string]any{"null": nil} {
		out := e.ok("POST", base+"/fork", map[string]any{"name": "n-" + name, "ttlSeconds": ttl}, 202)
		if v, ok := e.lastForkTTL(); !ok || v != float64(0) {
			t.Fatalf("compat fork ttlSeconds %s sent ttl_s = %v (%v), want 0", name, v, ok)
		}
		if sb := out["sandbox"].(map[string]any); sb["ttlSeconds"] != nil {
			t.Fatalf("ttlSeconds %s: child ttlSeconds = %v, want none", name, sb["ttlSeconds"])
		}
	}
	e.ok("POST", base+"/fork", map[string]any{"name": "c2", "ttlSeconds": 90}, 202)
	if v, _ := e.lastForkTTL(); v != float64(90) {
		t.Fatalf("explicit ttlSeconds sent ttl_s = %v, want 90", v)
	}
}

// A compat resume of a running sandbox that carries environment, noEnv or type is
// 409 sandbox_running_stop_first with no side effects; a plain resume is as before.
func TestR46CompatResumeOfRunningSandboxIsRefusedWithChanges(t *testing.T) {
	e := setup(t)
	e.saveEnv("prod", map[string]string{"E1": "v1"}, nil)
	bx := e.newSandbox(nil)
	id := e.nativeOf(bx)
	base := "/api/v1/sandboxes/" + bx
	starts, stops, resizes := e.startCount(), len(e.boatd.stops), e.resizeCount()
	for name, body := range map[string]map[string]any{
		"environment": {"environment": "prod"},
		"noEnv":       {"noEnv": true},
		"type":        {"type": "large"},
		"all":         {"environment": "prod", "type": "large", "env": map[string]string{"A": "1"}},
	} {
		st, out, _ := e.compat(e.owner, "POST", base+"/resume", body, nil)
		if st != http.StatusConflict || errCode(out) != "sandbox_running_stop_first" {
			t.Errorf("%s: resume = %d %v, want 409 sandbox_running_stop_first", name, st, out)
		}
	}
	e.boatd.mu.Lock()
	nstops := len(e.boatd.stops)
	e.boatd.mu.Unlock()
	if e.startCount() != starts || nstops != stops || e.resizeCount() != resizes || e.boatdState(id) != "running" || e.noEnvRunOf(id) {
		t.Fatalf("a refused resume had side effects (starts %d->%d, stops %d->%d, resizes %d->%d, state %s)",
			starts, e.startCount(), stops, nstops, resizes, e.resizeCount(), e.boatdState(id))
	}
	// A plain resume of a running sandbox (what a client does to extend or retry) is accepted.
	e.ok("POST", base+"/resume", map[string]any{}, 202)
}
