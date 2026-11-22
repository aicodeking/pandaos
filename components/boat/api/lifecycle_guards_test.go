package main

import "testing"

func TestLifecycleProtectedCloneRequiresSourceEvidence(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	sid := e.r39Snapshot(id, "ordinary")
	if err := e.srv.store.Update(func(st *State) error { delete(st.VMSecrets, snapSourceKey(sid)); return nil }); err != nil {
		t.Fatal(err)
	}
	st, out := e.createFrom(sid, map[string]any{"no_env": true})
	if st != 201 || e.freshCreatesOf(out["id"].(string)) != 1 {
		t.Fatalf("protected clone without source evidence must cold boot: %d %v", st, out)
	}
}

func TestLifecycleNarrowedStartFailureStaysStoppedAndRetryable(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/PROD", map[string]any{"value": "value", "inject": true})
	e.saveEnv("bare", map[string]string{}, []string{})
	id := e.createVM()
	e.do(e.owner, "POST", vmPath(id, "stop"), nil)
	e.boatd.mu.Lock()
	e.boatd.startEnvFail = true
	e.boatd.mu.Unlock()
	st, out := e.do(e.owner, "POST", vmPath(id, "start"), map[string]any{"environment": "bare"})
	if st != 502 || e.nativeVM(id)["state"] != "stopped" {
		t.Fatalf("failed narrowed start = %d %v; must stop", st, out)
	}
	if st, out := e.do(e.owner, "POST", vmPath(id, "commands"), map[string]any{"cmd": []string{"true"}}); st != 409 {
		t.Fatalf("command while pending = %d %v", st, out)
	}
	e.boatd.mu.Lock()
	e.boatd.startEnvFail = false
	e.boatd.mu.Unlock()
	if st, out := e.do(e.owner, "POST", vmPath(id, "start"), map[string]any{"environment": "bare"}); st != 200 {
		t.Fatalf("retry = %d %v", st, out)
	}
	if e.freshBootsOf(id) != 2 {
		t.Fatal("retry must discard memory again until the transition succeeds")
	}
}

func TestLifecycleProtectedShareAndImportKeepPolicy(t *testing.T) {
	e := setup(t)
	e.saveEnv("default", map[string]string{"DEFAULT_VAR": "set"}, []string{})
	e.do(e.owner, "PUT", "/v1/environments/default", map[string]any{"is_default": true})
	src := e.createVMWith(map[string]any{"no_env": true})
	sid := e.r39Snapshot(src, "protected")
	st, share := e.do(e.owner, "POST", vmPath(src, "snapshots", sid, "shares"), map[string]any{})
	if st != 201 {
		t.Fatalf("share = %d %v", st, share)
	}
	for _, compat := range []bool{false, true} {
		var id string
		if compat {
			id = e.nativeOf(e.newSandbox(map[string]any{"from": share["code"]}))
		} else {
			st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"from": share["code"]})
			if st != 201 {
				t.Fatalf("share create = %d %v", st, out)
			}
			id = out["id"].(string)
		}
		if !e.srv.noEnvRun(id) || e.guestVar(id, "DEFAULT_VAR") != nil || e.srv.store.Snapshot().VMEnvs[id] != "" {
			t.Fatal("share source policy must precede default environment selection")
		}
	}
	st, imp := e.do(e.owner, "POST", "/v1/snapshots/import-share", map[string]any{"code": share["code"]})
	if st != 201 {
		t.Fatalf("import = %d %v", st, imp)
	}
	isid := imp["id"].(string)
	back, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if len(back.Snapshot().VMSecrets[snapSourceKey(isid)]) != 2 {
		t.Fatal("import policy must be durable when the import returns")
	}
	st, clone := e.createFrom(isid, nil)
	if st != 201 || !e.srv.noEnvRun(clone["id"].(string)) {
		t.Fatalf("import clone = %d %v", st, clone)
	}
}

func TestLifecycleCopyLabelsRestoreForeignPolicy(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	src := e.createVM()
	sid := e.r39Snapshot(src, "source")
	_, share := e.do(e.owner, "POST", vmPath(src, "snapshots", sid, "shares"), map[string]any{})
	st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"from": share["code"]})
	if st != 201 {
		t.Fatalf("copy = %d %v", st, out)
	}
	id := out["id"].(string)
	st, fork := e.do(e.owner, "POST", vmPath(id, "fork"), map[string]any{})
	if st != 201 {
		t.Fatalf("fork = %d %v", st, fork)
	}
	child := fork["id"].(string)
	for _, vm := range []string{id, child} {
		labels := e.labelsOf(vm)
		if labels[foreignLabel] != "1" || labels[restrictedLabel] != "1" || labels[noSelfLabel] != "1" {
			t.Fatalf("creation provenance = %v", labels)
		}
		if err := e.srv.store.Update(func(st *State) error {
			delete(st.ForeignVMs, vm)
			delete(st.VMTokens, vm)
			delete(st.AIVMs, vm)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.srv.reconcileCopyCredentials(e.srv.baseCtx()); err != nil {
		t.Fatal(err)
	}
	for _, vm := range []string{id, child} {
		st := e.srv.store.Snapshot()
		tok := st.VMTokens[vm]
		if !st.ForeignVMs[vm] || tok == nil || !tok.NoSelfAPI || !tok.RestrictSelf {
			t.Fatal("adoption must restore copy policy before credentials")
		}
		if ai := st.AIVMs[vm]; ai == nil || !ai.Blocked {
			t.Fatal("adoption must restore the foreign AI restriction before credentials")
		}
	}
}

func TestLifecyclePendingIntentSurvivesFailedBoot(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	e.do(e.owner, "POST", vmPath(id, "stop"), nil)
	e.failNextStarts(1)
	if st, out := e.do(e.owner, "POST", vmPath(id, "start"), map[string]any{"no_env": true}); st != 502 {
		t.Fatalf("failed boot = %d %v", st, out)
	}
	back, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if pending := back.Snapshot().VMTransitions[id]; pending == nil || !pending.NoEnv {
		t.Fatal("failed boot must keep durable protected intent")
	}
	if st, out := e.do(e.owner, "POST", vmPath(id, "start"), nil); st != 200 {
		t.Fatalf("retry = %d %v", st, out)
	}
	if e.freshBootsOf(id) != 1 || !e.srv.noEnvRun(id) || e.srv.store.Snapshot().VMTransitions[id] != nil {
		t.Fatal("retry must complete a fresh protected boot before clearing intent")
	}
}

func TestLifecycleForeignProtectedTransitionNeedsEvidence(t *testing.T) {
	e := setup(t)
	src := e.createVM()
	sid := e.r39Snapshot(src, "ordinary")
	_, share := e.do(e.owner, "POST", vmPath(src, "snapshots", sid, "shares"), map[string]any{})
	if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"from": share["code"], "no_env": true}); st != 409 {
		t.Fatalf("unproven protected foreign clone = %d %v", st, out)
	}
	st, copy := e.do(e.owner, "POST", "/v1/vms", map[string]any{"from": share["code"]})
	if st != 201 {
		t.Fatalf("ordinary copy = %d %v", st, copy)
	}
	id := copy["id"].(string)
	e.do(e.owner, "POST", vmPath(id, "stop"), nil)
	if st, out := e.do(e.owner, "POST", vmPath(id, "start"), map[string]any{"no_env": true}); st != 409 {
		t.Fatalf("unproven foreign conversion = %d %v", st, out)
	}
	if st, out := e.do(e.owner, "POST", vmPath(id, "fork"), map[string]any{"no_env": true}); st != 409 {
		t.Fatalf("unproven protected foreign fork = %d %v", st, out)
	}
}

func TestLifecycleDisabledSelfPolicySurvivesMissingTokenRow(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	id := e.createVMWith(map[string]any{"self_api": false})
	if err := e.srv.store.Update(func(st *State) error { delete(st.VMTokens, id); return nil }); err != nil {
		t.Fatal(err)
	}
	e.stopStart(id, nil)
	if e.guestVar(id, "BOAT_API_KEY") != nil {
		t.Fatal("a lost token row must not enable the self API")
	}
	if tok := e.srv.store.Snapshot().VMTokens[id]; tok == nil || !tok.NoSelfAPI {
		t.Fatal("disabled policy was not rehydrated")
	}
}
