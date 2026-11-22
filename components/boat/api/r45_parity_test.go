package main

import (
	"net/http"
	"testing"
)

// r45: resume with a narrower environment cold-boots; "base" is a real
// environment name (id, then name, then the back-compat fallback); the per-sandbox
// env is stored and replayed on every later start; a self-token type change has no
// side effects.

func envVal(m map[string]any, k string) any {
	env, _ := m["env"].(map[string]any)
	return env[k]
}

// 1: a stopped VM ran with S1. Resuming with an environment that selects only S2
// cold-boots (fresh_boot); a superset does not.
func TestR45ResumeNarrowerEnvironmentBootsFresh(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/S1", map[string]any{"value": "one", "inject": true})
	e.do(e.owner, "PUT", "/v1/secrets/S2", map[string]any{"value": "two", "inject": false})
	e.saveEnv("narrow", map[string]string{"E": "n"}, []string{"S2"})
	e.saveEnv("wide", map[string]string{"E": "w"}, []string{"S1", "S2"})

	narrowVM := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+narrowVM+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+narrowVM+"/start", map[string]any{"environment": "narrow"}); st != 200 {
		t.Fatalf("narrow resume = %d %v", st, out)
	}
	if v := e.lastStart(); v["fresh_boot"] != true {
		t.Errorf("resume with an environment that drops S1 = %v, want fresh_boot", v)
	}
	if got := envVal(e.lastStart(), "S1"); got != nil {
		t.Errorf("narrow resume env carries S1 = %v", got)
	}

	wideVM := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+wideVM+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+wideVM+"/start", map[string]any{"environment": "wide"}); st != 200 {
		t.Fatalf("wide resume = %d %v", st, out)
	}
	if v := e.lastStart(); v["fresh_boot"] != nil {
		t.Errorf("resume with a superset = %v, want no fresh_boot", v)
	}
	if n := e.freshBootsOf(wideVM); n != 0 {
		t.Errorf("superset fresh boots = %d, want 0", n)
	}

	// Compat resume takes the same path.
	bx := e.newSandbox(nil)
	vm := e.nativeOf(bx)
	e.ok("POST", "/api/v1/sandboxes/"+bx+"/stop", nil, 202)
	e.ok("POST", "/api/v1/sandboxes/"+bx+"/resume", map[string]any{"environment": "narrow"}, 202)
	if n := e.freshBootsOf(vm); n != 1 {
		t.Errorf("compat narrow resume fresh boots = %d, want 1", n)
	}
}

// 2: "base" is a real name. id first, then exact name; "base" is the team default
// only when no environment is named "base"; any other unknown value is 404.
func TestR45EnvironmentRefResolution(t *testing.T) {
	e := setup(t)
	yes := true
	e.do(e.owner, "POST", "/v1/environments", map[string]any{"name": "prod", "env": map[string]string{"WHICH": "prod"}, "is_default": &yes})
	otherID := e.saveEnv("other", map[string]string{"WHICH": "other"}, nil)
	var prodID string
	for id, ev := range e.srv.store.Snapshot().Environments {
		if ev.Name == "prod" {
			prodID = id
		}
	}
	recorded := func(vm string) string { return e.srv.store.Snapshot().VMEnvs[vm] }

	// By id.
	st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "a", "size": "small", "environment": otherID})
	if st != 201 {
		t.Fatalf("create by id = %d %v", st, out)
	}
	if got := recorded(out["id"].(string)); got != otherID {
		t.Errorf("create by id recorded %q, want %q", got, otherID)
	}
	vm := out["id"].(string)
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{"environment": prodID}); st != 200 {
		t.Fatalf("start by id = %d %v", st, out)
	}
	if w := envVal(e.lastStart(), "WHICH"); w != "prod" {
		t.Errorf("start by id used %v, want prod", w)
	}
	st, out = e.do(e.owner, "POST", "/v1/vms/"+vm+"/fork", map[string]any{"environment": otherID})
	if st != 201 || recorded(out["id"].(string)) != otherID {
		t.Errorf("fork by id = %d %v, recorded %q", st, out, recorded(out["id"].(string)))
	}

	// Fallback: no environment is named "base", so "base" is the team default, on
	// every path.
	st, out = e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "b", "size": "small", "environment": "base"})
	if st != 201 || recorded(out["id"].(string)) != prodID {
		t.Fatalf("create base fallback = %d %v, recorded %q want %q", st, out, recorded(out["id"].(string)), prodID)
	}
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{"environment": "base"}); st != 200 {
		t.Fatalf("start base fallback = %d %v", st, out)
	}
	if w := envVal(e.lastStart(), "WHICH"); w != "prod" {
		t.Errorf("start base fallback used %v, want prod", w)
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/fork", map[string]any{"environment": "base"}); st != 201 || recorded(out["id"].(string)) != prodID {
		t.Errorf("fork base fallback = %d %v", st, out)
	}

	// Unknown values are 404 unknown_environment, before anything is touched.
	for name, call := range map[string]func() (int, map[string]any){
		"create": func() (int, map[string]any) {
			return e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "c", "size": "small", "environment": "nope"})
		},
		"start": func() (int, map[string]any) {
			return e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{"environment": "nope"})
		},
		"fork": func() (int, map[string]any) {
			return e.do(e.owner, "POST", "/v1/vms/"+vm+"/fork", map[string]any{"environment": "nope"})
		},
	} {
		st, out := call()
		errObj, _ := out["error"].(map[string]any)
		if st != http.StatusNotFound || errObj["code"] != "unknown_environment" {
			t.Errorf("%s unknown environment = %d %v, want 404 unknown_environment", name, st, out)
		}
	}
}

// 3: the create-time env is stored per VM and replayed on every later start;
// fork and clone inherit it unless the request gives its own; a start with env
// replaces it; deleting the VM drops it.
func TestR45PerSandboxEnvPersists(t *testing.T) {
	e := setup(t)
	e.saveEnv("prod", map[string]string{"WHICH": "prod", "TENANT_ID": "from-env"}, nil)
	st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "a", "size": "small", "environment": "prod", "env": map[string]string{"TENANT_ID": "t1"}})
	if st != 201 {
		t.Fatalf("create = %d %v", st, out)
	}
	vm := out["id"].(string)
	if got := e.srv.store.Snapshot().VMExtraEnv[vm]["TENANT_ID"]; got != "t1" {
		t.Fatalf("stored env = %v, want TENANT_ID=t1 (the caller's own, not the environment's)", e.srv.store.Snapshot().VMExtraEnv[vm])
	}
	if _, has := e.srv.store.Snapshot().VMExtraEnv[vm]["WHICH"]; has {
		t.Errorf("the environment's vars must not be stored per VM")
	}

	// Resume keeps it, over the environment's vars.
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{}); st != 200 {
		t.Fatalf("resume = %d %v", st, out)
	}
	if got := envVal(e.lastStart(), "TENANT_ID"); got != "t1" {
		t.Errorf("resume TENANT_ID = %v, want t1", got)
	}
	if got := envVal(e.lastStart(), "WHICH"); got != "prod" {
		t.Errorf("resume WHICH = %v, want prod", got)
	}

	// Fork inherits; fork with env replaces.
	st, out = e.do(e.owner, "POST", "/v1/vms/"+vm+"/fork", map[string]any{})
	if st != 201 {
		t.Fatalf("fork = %d %v", st, out)
	}
	child := out["id"].(string)
	if got := e.boatdEnvOf(child)["TENANT_ID"]; got != "t1" {
		t.Errorf("fork TENANT_ID = %v, want t1", got)
	}
	if got := e.srv.store.Snapshot().VMExtraEnv[child]["TENANT_ID"]; got != "t1" {
		t.Errorf("fork stored env = %v", e.srv.store.Snapshot().VMExtraEnv[child])
	}
	st, out = e.do(e.owner, "POST", "/v1/vms/"+vm+"/fork", map[string]any{"env": map[string]string{"TENANT_ID": "t2"}})
	if st != 201 {
		t.Fatalf("fork with env = %d %v", st, out)
	}
	child2 := out["id"].(string)
	if got := e.boatdEnvOf(child2)["TENANT_ID"]; got != "t2" {
		t.Errorf("fork with env TENANT_ID = %v, want t2", got)
	}
	if got := e.srv.store.Snapshot().VMExtraEnv[vm]["TENANT_ID"]; got != "t1" {
		t.Errorf("a fork with env changed the parent's stored env: %v", got)
	}

	// A start with env replaces the stored map.
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{"env": map[string]string{"TENANT_ID": "t3"}}); st != 200 {
		t.Fatalf("resume with env = %d %v", st, out)
	}
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{})
	if got := envVal(e.lastStart(), "TENANT_ID"); got != "t3" {
		t.Errorf("later resume TENANT_ID = %v, want t3", got)
	}

	// Clone: a create from a snapshot of the VM inherits; one with env replaces.
	sid := e.r39Snapshot(vm, "for-clone")
	st, out = e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "clone", "size": "small", "snapshot": sid})
	if st != 201 {
		t.Fatalf("clone = %d %v", st, out)
	}
	clone := out["id"].(string)
	if got := e.srv.store.Snapshot().VMExtraEnv[clone]["TENANT_ID"]; got != "t3" {
		t.Errorf("clone stored env = %v, want TENANT_ID=t3", e.srv.store.Snapshot().VMExtraEnv[clone])
	}
	if got := e.boatdEnvOf(clone)["TENANT_ID"]; got != "t3" {
		t.Errorf("clone TENANT_ID = %v, want t3", got)
	}
	st, out = e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "clone2", "size": "small", "snapshot": sid, "env": map[string]string{"TENANT_ID": "t4"}})
	if st != 201 {
		t.Fatalf("clone with env = %d %v", st, out)
	}
	if got := e.srv.store.Snapshot().VMExtraEnv[out["id"].(string)]["TENANT_ID"]; got != "t4" {
		t.Errorf("clone with env stored %v, want t4", got)
	}

	// Delete drops it.
	if st, out := e.do(e.owner, "DELETE", "/v1/vms/"+child, nil); st != 200 && st != 202 && st != 204 {
		t.Fatalf("delete = %d %v", st, out)
	}
	if _, ok := e.srv.store.Snapshot().VMExtraEnv[child]; ok {
		t.Errorf("a deleted VM's stored env must be dropped")
	}
}

// 4: a type change called with a VM's own token (which cannot start its VM) is
// refused before the VM is stopped or resized.
func TestR45SelfTokenTypeChangeHasNoSideEffects(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	bx := e.newSandbox(nil)
	vm := e.nativeOf(bx)
	tok := e.currentSelfToken(vm)
	resizes, size := e.resizeCount(), e.boatdSizeOf(vm)
	st, out, _ := e.compat(tok, "POST", "/api/v1/sandboxes/"+bx+"/resume", map[string]any{"type": "large"}, nil)
	if st != http.StatusForbidden {
		t.Fatalf("self-token type change = %d %v, want 403", st, out)
	}
	if e.boatdState(vm) != "running" || e.resizeCount() != resizes || e.boatdSizeOf(vm) != size {
		t.Errorf("a refused type change had side effects: state %s, resizes %d->%d, size %s->%s",
			e.boatdState(vm), resizes, e.resizeCount(), size, e.boatdSizeOf(vm))
	}
}

// A VM's own token cannot power-cycle its VM through compat recover: refused
// before the stop, so the VM keeps running.
func TestR45SelfTokenRecoverHasNoSideEffects(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	bx := e.newSandbox(nil)
	vm := e.nativeOf(bx)
	tok := e.currentSelfToken(vm)
	st, out, _ := e.compat(tok, "POST", "/api/v1/sandboxes/"+bx+"/recover", nil, nil)
	if st != http.StatusForbidden {
		t.Fatalf("self-token recover = %d %v, want 403", st, out)
	}
	if got := e.boatdState(vm); got != "running" {
		t.Errorf("a refused recover stopped the VM: state %s", got)
	}
}
