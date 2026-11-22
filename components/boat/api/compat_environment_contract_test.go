package main

import (
	"testing"
)

// Contract: docs.boat.dev/openapi/boat-v1.yaml, deleteEnvironment and Sandbox.
func TestCompatEnvironmentDeletionGuards(t *testing.T) {
	t.Run("last is team scoped", func(t *testing.T) {
		e := setup(t)
		id := e.saveEnv("only", map[string]string{}, nil)
		other := otherTeamKey(e)
		if status, out := e.do(other, "POST", "/v1/environments", map[string]any{"name": "other"}); status != 201 {
			t.Fatalf("other environment = %d %v", status, out)
		}
		if status, out, _ := e.compat(e.owner, "DELETE", "/api/v1/environments/"+envUUID(id, 0), nil, nil); status != 409 || errCode(out) != "conflict" {
			t.Fatalf("last environment delete = %d %v", status, out)
		}
		if e.srv.store.Snapshot().Environments[id] == nil {
			t.Fatal("rejected deletion removed the environment")
		}
		// The native API intentionally permits deleting the last environment.
		if status, out := e.do(e.owner, "DELETE", "/v1/environments/"+id, nil); status != 200 {
			t.Fatalf("native delete = %d %v", status, out)
		}
	})
	t.Run("default", func(t *testing.T) {
		e := setup(t)
		id := e.saveEnv("default", map[string]string{}, nil)
		e.saveEnv("spare", map[string]string{}, nil)
		if status, out := e.do(e.owner, "PUT", "/v1/environments/"+id, map[string]any{"is_default": true}); status != 200 {
			t.Fatalf("set default = %d %v", status, out)
		}
		if status, out, _ := e.compat(e.owner, "DELETE", "/api/v1/environments/"+envUUID(id, 0), nil, nil); status != 409 || errCode(out) != "conflict" {
			t.Fatalf("default delete = %d %v", status, out)
		}
	})
}

func TestCompatEnvironmentConcurrentDeleteKeepsOne(t *testing.T) {
	e := setup(t)
	ids := []string{e.saveEnv("one", map[string]string{}, nil), e.saveEnv("two", map[string]string{}, nil)}
	start := make(chan struct{})
	results := make(chan int, 2)
	for _, id := range ids {
		go func(id string) {
			<-start
			status, _, _ := e.compat(e.owner, "DELETE", "/api/v1/environments/"+envUUID(id, 0), nil, nil)
			results <- status
		}(id)
	}
	close(start)
	a, b := <-results, <-results
	if !((a == 200 && b == 409) || (a == 409 && b == 200)) || len(e.srv.store.Snapshot().Environments) != 1 {
		t.Fatalf("concurrent deletes = %d, %d; environments = %d", a, b, len(e.srv.store.Snapshot().Environments))
	}
}

func TestCompatSandboxEnvironmentMetadata(t *testing.T) {
	e := setup(t)
	eid := e.saveEnv("web", map[string]string{"PINNED": "v1"}, []string{})
	e.saveEnv("spare", map[string]string{}, []string{})
	if status, out := e.do(e.owner, "PUT", "/v1/environments/"+eid, map[string]any{"is_default": true}); status != 200 {
		t.Fatalf("set default = %d %v", status, out)
	}
	check := func(sandbox map[string]any, name any, version any) {
		t.Helper()
		if sandbox["environment"] != name || sandbox["environmentVersion"] != version {
			t.Errorf("environment metadata = %v / %v, want %v / %v", sandbox["environment"], sandbox["environmentVersion"], name, version)
		}
		if _, ok := sandbox["environmentVersion"]; !ok {
			t.Error("missing nullable environmentVersion")
		}
		if _, ok := sandbox["access"]; ok {
			t.Error("access asserted without known creator identity")
		}
	}
	created := e.ok("POST", "/api/v1/sandboxes", nil, 202)["sandbox"].(map[string]any)
	check(created, "web", float64(1))
	bx := created["id"].(string)
	id := e.nativeOf(bx)
	if status, out := e.do(e.owner, "PUT", "/api/v1/environments/"+envUUID(eid, 0), map[string]any{"name": "renamed", "envContents": "PINNED=v2\n"}); status != 200 {
		t.Fatalf("rename and edit = %d %v", status, out)
	}
	check(e.ok("GET", "/api/v1/sandboxes/"+bx, nil, 200)["sandbox"].(map[string]any), "renamed", float64(1))
	for _, row := range e.ok("GET", "/api/v1/sandboxes", nil, 200)["sandboxes"].([]any) {
		check(row.(map[string]any), "renamed", float64(1))
	}
	named := e.ok("POST", "/api/v1/sandboxes", map[string]any{"environment": "renamed"}, 202)["sandbox"].(map[string]any)
	check(named, "renamed", float64(2))
	// Legacy unpinned VMs replay the current environment, so report its version.
	if err := e.srv.store.Update(func(st *State) error { delete(st.VMEnvPins, id); return nil }); err != nil {
		t.Fatal(err)
	}
	check(e.ok("GET", "/api/v1/sandboxes/"+bx, nil, 200)["sandbox"].(map[string]any), "renamed", float64(2))
	if err := e.srv.store.Update(func(st *State) error {
		st.VMEnvPins[id] = &EnvPin{Version: 1, Vars: map[string]string{"PINNED": "v1"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A supported non-default deletion hides the association but keeps the pin.
	if status, out := e.do(e.owner, "PUT", "/api/v1/environments/"+envUUID(eid, 0), map[string]any{"isDefault": false}); status != 200 {
		t.Fatalf("unset default = %d %v", status, out)
	}
	if status, out := e.do(e.owner, "DELETE", "/api/v1/environments/"+envUUID(eid, 0), nil); status != 200 {
		t.Fatalf("delete = %d %v", status, out)
	}
	check(e.ok("GET", "/api/v1/sandboxes/"+bx, nil, 200)["sandbox"].(map[string]any), nil, nil)
	e.stopStart(id, nil)
	if e.guestVar(id, "PINNED") != "v1" {
		t.Fatal("deletion lost pinned replay vars")
	}
	noenv := e.ok("POST", "/api/v1/sandboxes", map[string]any{"noEnv": true}, 202)["sandbox"].(map[string]any)
	check(noenv, nil, nil)
}
