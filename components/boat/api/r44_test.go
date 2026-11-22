package main

import (
	"net/http"
	"testing"
)

// r44: the no_env self-token restriction bound to the token generation, a named
// environment never changing a foreign VM's secret selection, one resolver for
// the environment name "base", and resume/fork --type with an environment on a
// no_env run.

func (e *env) markForeign(vm string) {
	e.t.Helper()
	err := e.srv.store.Update(func(st *State) error {
		st.ForeignVMs[vm] = true
		st.VMSecrets[vm] = []string{} // as create records for a foreign disk
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) boatdSizeOf(vm string) string {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	s, _ := e.boatd.vms[vm]["size"].(string)
	return s
}

func (e *env) resizeCount() int {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	return len(e.boatd.resizes)
}

// 1: a no_env run's token exfiltrated, then used to stop the VM: every snapshot
// and history read after the stop is still refused, until a start mints a new one.
func TestR44SelfStopKeepsNoEnvTokenRestricted(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "top-secret", "inject": true})
	bx := e.newSandbox(nil)
	vm := e.nativeOf(bx)
	sid := e.r39Snapshot(vm, "secret-run")
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("no_env start = %d %v", st, out)
	}
	tok := e.currentSelfToken(vm) // exfiltrated from the no_env run
	native := "/v1/vms/" + vm
	compat := "/api/v1/sandboxes/" + bx
	denied := func(step string) {
		t.Helper()
		for _, c := range []struct{ method, path string }{
			{"GET", native + "/snapshots"},
			{"POST", native + "/snapshots"},
			{"GET", native + "/snapshots/" + sid + "/download"},
			{"DELETE", native + "/snapshots/" + sid},
			{"GET", native + "/commands"},
			{"GET", native + "/command-history"},
			{"GET", native + "/prompts"},
			{"GET", native + "/conversations"},
			{"GET", compat + "/snapshots"},
			{"GET", compat + "/snapshots/latest"},
			{"GET", compat + "/command-history"},
			{"GET", compat + "/events"},
		} {
			st, out, _ := e.compat(tok, c.method, c.path, map[string]any{}, map[string]string{"X-Ascii-Confirm-Delete": bx})
			if st != http.StatusForbidden && st != http.StatusUnauthorized {
				t.Errorf("%s: %s %s with the no_env token = %d %v, want 403 or 401", step, c.method, c.path, st, out)
			}
		}
	}
	denied("while the run is no_env")
	if st, out := e.do(tok, "POST", native+"/stop", nil); st != 200 {
		t.Fatalf("self stop = %d %v", st, out) // the token may stop its own VM
	}
	if !e.srv.noEnvRun(vm) {
		t.Fatalf("a completed stop must not end no_env (sticky)")
	}
	denied("after the self stop")
	// The owner still reads them.
	if st, out := e.do(e.owner, "GET", native+"/snapshots", nil); st != 200 {
		t.Errorf("owner snapshot list = %d %v", st, out)
	}
	// A plain start mints a new generation: the old token is dead (401), and the
	// new token is still restricted (the VM is no_env for good).
	if st, out := e.do(e.owner, "POST", native+"/start", map[string]any{}); st != 200 {
		t.Fatalf("plain start = %d %v", st, out)
	}
	if st, _ := e.do(tok, "GET", native+"/snapshots", nil); st != http.StatusUnauthorized && st != http.StatusForbidden {
		t.Errorf("old token after a new generation = %d, want 401/403", st)
	}
	if st, out := e.do(e.currentSelfToken(vm), "GET", native+"/snapshots", nil); st != http.StatusForbidden {
		t.Errorf("new generation's token = %d %v, want 403 (sticky no_env)", st, out)
	}
}

// 1: a failed no_env push on a running normal VM puts the token flag back too.
func TestR44TokenFlagRestoredWithRunMark(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	vm := e.createVM()
	before := e.srv.readRunMark(vm)
	if err := e.srv.markNoEnvRun(vm, true); err != nil {
		t.Fatal(err)
	}
	if !e.srv.tokenNoEnvGen(vm) {
		t.Fatalf("a no_env mark must bind the token generation")
	}
	e.srv.restoreRunMark(vm, before)
	if e.srv.tokenNoEnvGen(vm) || e.srv.noEnvRun(vm) {
		t.Errorf("restoring the previous mark must restore the token flag")
	}
}

// 2: a foreign VM started or forked with an environment gets the environment's
// vars, never its secret selection, and keeps the selection it has.
func TestR44NativeForeignStartAndForkEnvironmentVarsOnly(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/S1", map[string]any{"value": "one", "inject": true})
	e.do(e.owner, "PUT", "/v1/secrets/S2", map[string]any{"value": "two", "inject": false})
	e.saveEnv("prod", map[string]string{"E1": "v1"}, []string{"S2", defaultSelection})
	vm := e.createVM()
	e.markForeign(vm)
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{"environment": "prod"}); st != 200 {
		t.Fatalf("start = %d %v", st, out)
	}
	got, _ := e.lastStart()["env"].(map[string]any)
	if got["E1"] != "v1" || got["S1"] != nil || got["S2"] != nil {
		t.Fatalf("foreign start env = %v, want E1 only", got)
	}
	st := e.srv.store.Snapshot()
	if sel, ok := st.VMSecrets[vm]; !ok || len(sel) != 0 {
		t.Errorf("foreign VM selection = %v (%v), want it kept empty", sel, ok)
	}
	// The next plain start keeps the vars and still no secrets.
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{})
	got, _ = e.lastStart()["env"].(map[string]any)
	if got["E1"] != "v1" || got["S1"] != nil || got["S2"] != nil {
		t.Errorf("later foreign start env = %v, want E1 only", got)
	}

	// Fork of a foreign parent.
	st2, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/fork", map[string]any{"environment": "prod"})
	if st2 != 201 {
		t.Fatalf("fork = %d %v", st2, out)
	}
	child := out["id"].(string)
	snap := e.srv.store.Snapshot()
	if sel := snap.VMSecrets[child]; len(sel) != 0 {
		t.Errorf("foreign child selection = %v, want none", sel)
	}
	if !snap.ForeignVMs[child] {
		t.Errorf("a child of a foreign VM is foreign")
	}
	for _, rel := range e.boatd.releaseBodies {
		if env, _ := rel["env"].(map[string]any); env != nil {
			if env["S1"] != nil || env["S2"] != nil {
				t.Errorf("release env %v carries team secrets", env)
			}
		}
	}
	if env := e.boatdEnvOf(child); env["S1"] != nil || env["S2"] != nil {
		t.Errorf("child env %v carries team secrets", env)
	}
}

// 2: compat resume and fork of a foreign VM take the environment's vars only.
func TestR44CompatForeignResumeAndForkEnvironmentVarsOnly(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/S1", map[string]any{"value": "one", "inject": true})
	e.do(e.owner, "PUT", "/v1/secrets/S2", map[string]any{"value": "two", "inject": false})
	e.saveEnv("prod", map[string]string{"E1": "v1"}, []string{"S2", defaultSelection})
	bx := e.newSandbox(nil)
	vm := e.nativeOf(bx)
	e.markForeign(vm)
	base := "/api/v1/sandboxes/" + bx
	e.ok("POST", base+"/stop", nil, 202)
	e.ok("POST", base+"/resume", map[string]any{"environment": "prod"}, 202)
	got, _ := e.lastStart()["env"].(map[string]any)
	if got["E1"] != "v1" || got["S1"] != nil || got["S2"] != nil {
		t.Fatalf("compat foreign resume env = %v, want E1 only", got)
	}
	if sel := e.srv.store.Snapshot().VMSecrets[vm]; len(sel) != 0 {
		t.Errorf("selection after foreign resume = %v", sel)
	}
	out := e.ok("POST", base+"/fork", map[string]any{"environment": "prod"}, 202)
	child := e.nativeOf(out["sandbox"].(map[string]any)["id"].(string))
	if sel := e.srv.store.Snapshot().VMSecrets[child]; len(sel) != 0 {
		t.Errorf("compat foreign fork child selection = %v", sel)
	}
	if env := e.boatdEnvOf(child); env["S1"] != nil || env["S2"] != nil {
		t.Errorf("compat foreign child env %v carries team secrets", env)
	}
}

// 3: "base" is a real name on create, start, fork and clone, native and compat:
// with a default "prod" and another environment literally named "base",
// environment:"base" selects the literal one (the fallback to the team default
// only applies when none is named "base": see r45_parity_test.go).
func TestR44BaseEnvironmentOneResolver(t *testing.T) {
	e := setup(t)
	yes := true
	e.do(e.owner, "POST", "/v1/environments", map[string]any{"name": "prod", "env": map[string]string{"WHICH": "prod"}, "is_default": &yes})
	e.saveEnv("base", map[string]string{"WHICH": "literal-base"}, nil)
	e.saveEnv("other", map[string]string{"WHICH": "other"}, nil)
	var prodID, baseID string
	for id, ev := range e.srv.store.Snapshot().Environments {
		switch ev.Name {
		case "prod":
			prodID = id
		case "base":
			baseID = id
		}
	}
	recorded := func(vm string) string { return e.srv.store.Snapshot().VMEnvs[vm] }
	which := func(m map[string]any) any {
		env, _ := m["env"].(map[string]any)
		return env["WHICH"]
	}

	// Native create.
	st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "a", "size": "small", "environment": "base"})
	if st != 201 {
		t.Fatalf("native create base = %d %v", st, out)
	}
	if got := recorded(out["id"].(string)); got != baseID {
		t.Errorf("native create environment base recorded %q, want the literal base %q", got, baseID)
	}
	st, out = e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "b", "size": "small", "environment": "other"})
	if st != 201 {
		t.Fatalf("native create other")
	}
	if got := recorded(out["id"].(string)); got == prodID || got == "" {
		t.Errorf("native create environment other recorded %q", got)
	}
	// Compat create.
	bx := e.newSandbox(map[string]any{"environment": "base"})
	vm := e.nativeOf(bx)
	if got := recorded(vm); got != baseID {
		t.Errorf("compat create environment base recorded %q, want literal base %q", got, baseID)
	}

	// Start and resume.
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{"environment": "base"}); st != 200 {
		t.Fatalf("start base = %d %v", st, out)
	}
	if w := which(e.lastStart()); w != "literal-base" {
		t.Errorf("native start base used %v, want literal-base", w)
	}
	base := "/api/v1/sandboxes/" + bx
	e.ok("POST", base+"/stop", nil, 202)
	e.ok("POST", base+"/resume", map[string]any{"environment": "base"}, 202)
	if w := which(e.lastStart()); w != "literal-base" {
		t.Errorf("compat resume base used %v, want literal-base", w)
	}
	// Fork: the child records the default environment.
	prod := baseID
	st, out = e.do(e.owner, "POST", "/v1/vms/"+vm+"/fork", map[string]any{"environment": "base"})
	if st != 201 {
		t.Fatalf("fork base = %d %v", st, out)
	}
	if got := e.srv.store.Snapshot().VMEnvs[out["id"].(string)]; got != prod || got == "" {
		t.Errorf("native fork base recorded %q, want the literal base %q", got, prod)
	}
	cout := e.ok("POST", base+"/fork", map[string]any{"environment": "base"}, 202)
	child := e.nativeOf(cout["sandbox"].(map[string]any)["id"].(string))
	if got := e.srv.store.Snapshot().VMEnvs[child]; got != prod {
		t.Errorf("compat fork base recorded %q, want the literal base %q", got, prod)
	}
	// Clone: a compat create from a sandbox.
	sid := e.snapshotOf(vm)
	clone := e.newSandbox(map[string]any{"from": sid, "environment": "base"})
	if got := recorded(e.nativeOf(clone)); got != baseID {
		t.Errorf("compat clone environment base recorded %q, want literal base %q", got, baseID)
	}
	// An unknown name is still refused, and "base" on a team with no default is no
	// environment, not an error.
	if st, _ := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "c", "size": "small", "environment": "nope"}); st != 404 {
		t.Errorf("native create unknown environment = %d, want 404", st)
	}
	e2 := setup(t)
	if st, out := e2.do(e2.owner, "POST", "/v1/vms", map[string]any{"name": "d", "size": "small", "environment": "base"}); st != 201 {
		t.Errorf("native create base without a default = %d %v, want 201", st, out)
	}
}

// 4 (r46: sticky): resume or fork with an environment of a no_env sandbox is refused
// up front, 400 no_env_sticky; type changes keep no_env; a running sandbox is
// never stopped by a resume with a type.
func TestR44CompatResumeTypeWithEnvironmentOnNoEnvRun(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/S1", map[string]any{"value": "one", "inject": true})
	e.saveEnv("prod", map[string]string{"E1": "v1"}, nil)
	bx := e.newSandbox(nil)
	vm := e.nativeOf(bx)
	base := "/api/v1/sandboxes/" + bx
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("no_env start = %d %v", st, out)
	}
	if !e.srv.noEnvRun(vm) {
		t.Fatal("setup: the VM must be no_env")
	}
	resizes, size := e.resizeCount(), e.boatdSizeOf(vm)
	for name, c := range map[string]struct {
		body map[string]any
		want int
		code string
	}{
		"noEnv with environment": {map[string]any{"environment": "prod", "noEnv": true, "type": "large"}, 400, ""},
		"unknown environment":    {map[string]any{"environment": "nope", "type": "large"}, 404, ""},
		"environment (sticky)":   {map[string]any{"environment": "prod", "type": "large"}, 400, "no_env_sticky"},
		"type while running":     {map[string]any{"type": "large"}, 409, "sandbox_running_stop_first"},
	} {
		st, out, _ := e.compat(e.owner, "POST", base+"/resume", c.body, nil)
		if st != c.want || (c.code != "" && errCode(out) != c.code) {
			t.Errorf("%s = %d %v, want %d %s", name, st, out, c.want, c.code)
		}
		if e.resizeCount() != resizes || e.boatdSizeOf(vm) != size || e.boatdState(vm) != "running" || !e.srv.noEnvRun(vm) {
			t.Fatalf("%s: a refused resume had side effects (resizes %d, size %s, state %s)", name, e.resizeCount(), e.boatdSizeOf(vm), e.boatdState(vm))
		}
	}
	// Stopped: the type changes and the start stays no_env (no team secrets, no environment).
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	e.ok("POST", base+"/resume", map[string]any{"type": "small"}, 202)
	if !e.srv.noEnvRun(vm) {
		t.Errorf("a type change must keep no_env")
	}
	if got, _ := e.lastStart()["env"].(map[string]any); got["E1"] != nil || got["S1"] != nil {
		t.Errorf("start env = %v, want neither the environment nor team secrets", got)
	}
}

// 4: compat fork --type with an environment from a no_env parent.
func TestR44CompatForkTypeWithEnvironmentOfNoEnvParent(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/S1", map[string]any{"value": "one", "inject": true})
	e.saveEnv("prod", map[string]string{"E1": "v1"}, nil)
	bx := e.newSandbox(nil)
	vm := e.nativeOf(bx)
	base := "/api/v1/sandboxes/" + bx
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{"no_env": true})
	vms := e.boatdVMCount()
	resizes := e.resizeCount()

	for name, body := range map[string]map[string]any{
		"environment":        {"environment": "prod", "type": "large"},
		"environment noEnv":  {"environment": "prod", "noEnv": true, "type": "large"},
		"environment noEnv0": {"environment": "prod", "noEnv": false, "type": "large"},
		"noEnv false":        {"noEnv": false, "type": "large"},
	} {
		if st, out, _ := e.compat(e.owner, "POST", base+"/fork", body, nil); st != 400 {
			t.Errorf("fork %s of a no_env parent = %d %v, want 400", name, st, out)
		}
	}
	if e.boatdVMCount() != vms || e.resizeCount() != resizes {
		t.Fatalf("a refused fork copied or resized something")
	}
	// A plain fork with a type: the child is no_env and keeps it through the type change.
	out := e.ok("POST", base+"/fork", map[string]any{"type": "large"}, 202)
	child := e.nativeOf(out["sandbox"].(map[string]any)["id"].(string))
	if !e.srv.noEnvRun(child) {
		t.Errorf("a fork of a no_env parent is not no_env")
	}
	if e.boatdSizeOf(child) != "large" {
		t.Errorf("child size = %s, want large", e.boatdSizeOf(child))
	}
	if env := e.boatdEnvOf(child); env["E1"] != nil || env["S1"] != nil {
		t.Errorf("child env = %v, want no environment and no team secrets", env)
	}
}
