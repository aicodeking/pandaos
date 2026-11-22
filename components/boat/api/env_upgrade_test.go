package main

import "testing"

func TestEnvUpgradeTargetsAndAppliesLive(t *testing.T) {
	e := setup(t)
	e.saveEnv("web", map[string]string{"FOO": "v1"}, []string{})
	a := e.createVMWith(map[string]any{"environment": "web", "env": map[string]string{"CUSTOM": "mine"}})
	b := e.createVMWith(map[string]any{"environment": "web"})
	e.putEnvVar("web", "FOO", "v2")
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/environments/"+envUUIDOf(t, e, "web")+"/upgrade", map[string]any{"agentIds": []string{e.srv.compatID(e.teamID(), a)}}, nil)
	if st != 200 || out["upgradedLive"] != float64(1) || out["failed"] != float64(0) {
		t.Fatalf("upgrade = %d %v", st, out)
	}
	if e.guestVar(a, "FOO") != "v2" || e.guestVar(a, "CUSTOM") != "mine" || e.guestVar(b, "FOO") != "v1" {
		t.Fatal("targeted live upgrade must apply vars, preserve custom vars, and leave other sandboxes pinned")
	}
	if e.nativeVM(b)["environment_version"] != float64(1) {
		t.Fatal("untargeted sandbox moved")
	}
}

func TestEnvUpgradeStoppedSelectionAndLiveRemoval(t *testing.T) {
	e := setup(t)
	for _, name := range []string{"PROD", "DEV"} {
		e.do(e.owner, "PUT", "/v1/secrets/"+name, map[string]any{"value": name, "inject": true})
	}
	e.saveEnv("web", map[string]string{}, []string{"PROD", "DEV"})
	stopped := e.createVMWith(map[string]any{"environment": "web"})
	live := e.createVMWith(map[string]any{"environment": "web"})
	e.do(e.owner, "POST", vmPath(stopped, "stop"), nil)
	if st, out := e.do(e.owner, "PUT", "/v1/environments/web", map[string]any{"secrets": []string{"DEV"}}); st != 200 {
		t.Fatalf("edit = %d %v", st, out)
	}
	st, out := e.do(e.owner, "POST", "/v1/environments/web/upgrade", nil)
	if st != 200 || out["upgraded_stopped"] != float64(1) || out["failed"] != float64(1) || out["upgraded_live"] != float64(0) {
		t.Fatalf("upgrade = %d %v", st, out)
	}
	if e.nativeVM(live)["environment_version"] != float64(1) {
		t.Fatal("uncertified live removal moved the pin")
	}
	if st, out := e.do(e.owner, "POST", vmPath(stopped, "start"), nil); st != 200 {
		t.Fatalf("resume = %d %v", st, out)
	}
	if e.freshBootsOf(stopped) != 1 || e.guestVar(stopped, "PROD") != nil || e.guestVar(stopped, "DEV") != "DEV" {
		t.Fatal("stopped upgrade must apply the effective selection on a fresh boot")
	}
}

func TestEnvUpgradeValidatesAllTargetsBeforeApplying(t *testing.T) {
	e := setup(t)
	e.saveEnv("web", map[string]string{"FOO": "v1"}, []string{})
	e.saveEnv("other", map[string]string{}, []string{})
	a := e.createVMWith(map[string]any{"environment": "web"})
	b := e.createVMWith(map[string]any{"environment": "other"})
	e.putEnvVar("web", "FOO", "v2")
	for _, ids := range [][]string{{a, b}, {a, "missing"}} {
		st, out := e.do(e.owner, "POST", "/v1/environments/web/upgrade", map[string]any{"agentIds": ids})
		if st != 404 || e.nativeVM(a)["environment_version"] != float64(1) || e.guestVar(a, "FOO") != "v1" {
			t.Fatalf("invalid target set = %d %v", st, out)
		}
	}
	st, out := e.do(e.owner, "POST", "/v1/environments/web/upgrade", map[string]any{"agentIds": []string{}})
	if st != 200 || out["upgraded"] != float64(0) {
		t.Fatalf("empty set = %d %v", st, out)
	}
	if st, _ := e.do(e.newKey("vms:write"), "POST", "/v1/environments/web/upgrade", map[string]any{"agentIds": []string{a}}); st != 403 {
		t.Fatalf("non-admin = %d", st)
	}
}

func TestEnvUpgradeFailedGuestDoesNotMovePin(t *testing.T) {
	e := setup(t)
	e.saveEnv("web", map[string]string{"FOO": "v1"}, []string{})
	id := e.createVMWith(map[string]any{"environment": "web"})
	e.putEnvVar("web", "FOO", "v2")
	e.boatd.mu.Lock()
	e.boatd.envFail = true
	e.boatd.mu.Unlock()
	st, out := e.do(e.owner, "POST", "/v1/environments/web/upgrade", nil)
	if st != 200 || out["failed"] != float64(1) || out["upgraded"] != float64(0) || e.nativeVM(id)["environment_version"] != float64(1) {
		t.Fatalf("failed upgrade = %d %v", st, out)
	}
}

func TestEnvUpgradeLiveLegacyWithoutPinFails(t *testing.T) {
	e := setup(t)
	e.saveEnv("web", map[string]string{"FOO": "v1"}, []string{})
	id := e.createVMWith(map[string]any{"environment": "web"})
	if err := e.srv.store.Update(func(st *State) error { delete(st.VMEnvPins, id); return nil }); err != nil {
		t.Fatal(err)
	}
	e.putEnvVar("web", "FOO", "v2")
	st, out := e.do(e.owner, "POST", "/v1/environments/web/upgrade", nil)
	if st != 200 || out["failed"] != float64(1) || out["upgraded"] != float64(0) || e.srv.store.Snapshot().VMEnvPins[id] != nil || e.guestVar(id, "FOO") != "v1" {
		t.Fatalf("uncertified live legacy upgrade = %d %v", st, out)
	}
}

func TestEnvUpgradeNullAndEmptyTargets(t *testing.T) {
	e := setup(t)
	e.saveEnv("web", map[string]string{"FOO": "v1"}, []string{})
	id := e.createVMWith(map[string]any{"environment": "web"})
	e.putEnvVar("web", "FOO", "v2")
	for _, path := range []string{"/v1/environments/web/upgrade", "/api/v1/environments/" + envUUIDOf(t, e, "web") + "/upgrade"} {
		st, out := e.do(e.owner, "POST", path, map[string]any{"agentIds": nil})
		if st != 400 || e.guestVar(id, "FOO") != "v1" {
			t.Fatalf("null targets = %d %v", st, out)
		}
		st, out = e.do(e.owner, "POST", path, map[string]any{"agentIds": []string{}})
		if st != 200 || out["upgraded"] != float64(0) || e.guestVar(id, "FOO") != "v1" {
			t.Fatalf("empty targets = %d %v", st, out)
		}
	}
	if st, out := e.do(e.owner, "POST", "/v1/environments/web/upgrade", map[string]any{}); st != 200 || out["upgraded_live"] != float64(1) {
		t.Fatalf("absent targets = %d %v", st, out)
	}
}

func TestEnvUpgradeCurrentCustomSelectionIsSkipped(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/CUSTOM", map[string]any{"value": "value", "inject": true})
	e.saveEnv("web", map[string]string{"FOO": "v1"}, []string{})
	id := e.createVMWith(map[string]any{"environment": "web", "secrets": []string{"CUSTOM"}})
	st, out := e.do(e.owner, "POST", "/v1/environments/web/upgrade", nil)
	if st != 200 || out["upgraded"] != float64(0) || out["failed"] != float64(0) || e.guestVar(id, "CUSTOM") != "value" {
		t.Fatalf("current custom selection = %d %v", st, out)
	}
}

func TestEnvUpgradeAmbiguousPushKeepsPossibleRunNames(t *testing.T) {
	e := setup(t)
	e.saveEnv("web", map[string]string{}, []string{})
	id := e.createVMWith(map[string]any{"environment": "web"})
	e.do(e.owner, "PUT", "/v1/secrets/ADDED", map[string]any{"value": "value"})
	e.do(e.owner, "PUT", "/v1/environments/web", map[string]any{"secrets": []string{"ADDED"}})
	e.boatd.mu.Lock()
	e.boatd.envFail = true
	e.boatd.mu.Unlock()
	st, out := e.do(e.owner, "POST", "/v1/environments/web/upgrade", nil)
	names, known := runNamesOf(e.srv.store.Snapshot(), id)
	if st != 200 || out["failed"] != float64(1) || !known || len(names) != 1 || names[0] != "ADDED" {
		t.Fatalf("possible delivery = %d %v, names %v/%v", st, out, names, known)
	}
	back, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	names, known = runNamesOf(back.Snapshot(), id)
	if !known || len(names) != 1 || names[0] != "ADDED" {
		t.Fatal("possible delivery names must be durable before push")
	}
}

func TestEnvUpgradePlainRemovalUsesEffectiveVars(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "removed", true: "override"}[custom], func(t *testing.T) {
			e := setup(t)
			e.saveEnv("web", map[string]string{"OLD": "v1"}, []string{})
			body := map[string]any{"environment": "web"}
			if custom {
				body["env"] = map[string]string{"OLD": "custom"}
			}
			id := e.createVMWith(body)
			e.do(e.owner, "PUT", "/v1/environments/web", map[string]any{"env": map[string]string{}})
			st, out := e.do(e.owner, "POST", "/v1/environments/web/upgrade", nil)
			if st != 200 {
				t.Fatalf("upgrade = %d %v", st, out)
			}
			if custom {
				if out["upgraded_live"] != float64(1) || e.guestVar(id, "OLD") != "custom" {
					t.Fatal(out)
				}
			} else if out["failed"] != float64(1) {
				t.Fatal(out)
			}
		})
	}
}
