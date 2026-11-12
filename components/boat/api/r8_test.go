package main

import (
	"testing"
)

// Native /v1 routes (and so the native CLI) accept a boat.dev bx_ id; another
// team's bx_ id is still 404.
func TestNativeRoutesResolveBxIDs(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	st, vm := e.do(e.owner, "GET", "/v1/vms/"+bx, nil)
	if st != 200 || vm["id"] == bx || vm["id"] == nil {
		t.Fatalf("GET /v1/vms/%s: %d %v", bx, st, vm)
	}
	if st, _ := e.do(otherTeamKey(e), "GET", "/v1/vms/"+bx, nil); st != 404 {
		t.Errorf("other team: %d, want 404", st)
	}
	if st, _ := e.do(e.owner, "GET", "/v1/vms/bx_22222222", nil); st != 404 {
		t.Errorf("unknown bx_: %d, want 404", st)
	}
}

func TestCompatUnshareNotSupported(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/unshare", nil, nil)
	if st != 501 || errCode(out) != "not_supported" {
		t.Errorf("unshare: %d %v", st, out)
	}
	if st, _, _ := e.compat(otherTeamKey(e), "POST", "/api/v1/sandboxes/"+bx+"/unshare", nil, nil); st != 404 {
		t.Errorf("foreign unshare: %d", st)
	}
}

func TestCompatProviderModels(t *testing.T) {
	e := setup(t)
	pm, err := parseProviderModels("claude-code=sonnet,opus; codex=gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	e.srv.cfg.ProviderModels = pm
	st, out, _ := e.compat(e.owner, "GET", "/api/v1/provider-models", nil, nil)
	if st != 200 || out["type"] != "provider_models.list" {
		t.Fatalf("provider-models: %d %v", st, out)
	}
	got := map[string]int{}
	for _, p := range out["providers"].([]any) {
		p := p.(map[string]any)
		got[p["provider"].(string)] = len(p["models"].([]any))
	}
	if got["claude-code"] != 2 || got["codex"] != 1 || got["prime-agent"] != 0 || len(got) != len(agents) {
		t.Errorf("providers: %v", got)
	}
	for _, bad := range []string{"nope=x", "claude", "claude=bad model!"} {
		if _, err := parseProviderModels(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}
