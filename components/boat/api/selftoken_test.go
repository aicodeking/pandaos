package main

import (
	"strings"
	"testing"
)

// withoutGuestIdentity drops what every VM gets (BOAT_ID, BOAT_URL, BOAT_API_KEY).
func withoutGuestIdentity(env map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range env {
		if k != "BOAT_ID" && k != "BOAT_URL" && k != "BOAT_API_KEY" {
			out[k] = v
		}
	}
	return out
}

func envOpsHave(ops []map[string]any, name string) bool {
	for _, op := range ops {
		if vars, _ := op["vars"].(map[string]any); vars[name] != nil {
			return true
		}
	}
	return false
}

// lastEnvOp is the env boatd last wrote into vm.
func lastEnvOp(e *env, vm string) map[string]any {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	for i := len(e.boatd.envOps) - 1; i >= 0; i-- {
		if op := e.boatd.envOps[i]; op["vm"] == vm {
			vars, _ := op["vars"].(map[string]any)
			return vars
		}
	}
	return nil
}

func TestGuestTokenScopedToItsVM(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example/"
	a := e.createVM()
	b := e.createVM()
	env := lastEnvOp(e, a)
	tok, _ := env["BOAT_API_KEY"].(string)
	if env["BOAT_ID"] != a || env["BOAT_URL"] != "https://boat.example" || !strings.HasPrefix(tok, selfTokenPrefix) {
		t.Fatalf("guest env of %s: %v", a, env)
	}
	// Itself: read, exec, files, stop and start.
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/vms/" + a}, {"POST", "/v1/vms/" + a + "/exec"}, {"GET", "/v1/vms/" + a + "/ports"},
	} {
		var body any
		if c.method == "POST" {
			body = map[string]any{"cmd": []string{"true"}}
		}
		if st, _ := e.raw(tok, c.method, c.path, body); st/100 != 2 {
			t.Errorf("own VM %s %s: %d", c.method, c.path, st)
		}
	}
	// Anything else: another VM, the list, creating, keys, secrets, team admin.
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/vms/" + b}, {"POST", "/v1/vms/" + b + "/exec"}, {"POST", "/v1/vms/" + b + "/stop"},
		{"DELETE", "/v1/vms/" + b}, {"GET", "/v1/vms/" + b + "/files?path=/etc/hostname"},
		{"GET", "/v1/vms"}, {"POST", "/v1/vms"}, {"GET", "/v1/keys"}, {"POST", "/v1/keys"},
		{"GET", "/v1/secrets"}, {"PUT", "/v1/secrets/X"}, {"GET", "/v1/snapshots"}, {"GET", "/v1/webhooks"},
		{"GET", "/v1/usage"}, {"GET", "/v1/environments"},
	} {
		var body any
		if c.method != "GET" && c.method != "DELETE" {
			body = map[string]any{"cmd": []string{"true"}, "value": "v", "name": "x"}
		}
		if st, _ := e.raw(tok, c.method, c.path, body); st != 404 {
			t.Errorf("VM token on %s %s: %d, want 404", c.method, c.path, st)
		}
	}
	if st, _ := e.do(e.owner, "GET", "/v1/vms/"+b, nil); st != 200 {
		t.Fatalf("b must still exist: %d", st)
	}
	// A forged or altered token is not a key at all.
	forged := strings.Replace(tok, "_"+a, "_"+b, 1)
	if st, _ := e.raw(forged, "GET", "/v1/vms/"+b, nil); st != 401 {
		t.Errorf("token with another VM's id: %d, want 401", st)
	}
	// A new run gets a new token and the old one dies.
	e.do(e.owner, "POST", "/v1/vms/"+a+"/stop", nil)
	e.do(e.owner, "POST", "/v1/vms/"+a+"/start", nil)
	e.boatd.mu.Lock()
	started, _ := e.boatd.starts[len(e.boatd.starts)-1]["env"].(map[string]any)
	e.boatd.mu.Unlock()
	tok2, _ := started["BOAT_API_KEY"].(string)
	if tok2 == "" || tok2 == tok {
		t.Fatalf("start must give a new token: %v", started)
	}
	if st, _ := e.raw(tok, "GET", "/v1/vms/"+a, nil); st != 401 {
		t.Errorf("token of the previous run: %d, want 401", st)
	}
	if st, _ := e.raw(tok2, "GET", "/v1/vms/"+a, nil); st != 200 {
		t.Errorf("token of this run: %d", st)
	}
	// A fork's disk holds the parent's token: it must not reach the parent any more.
	_, child := e.do(e.owner, "POST", "/v1/vms/"+a+"/fork", map[string]any{"name": "kid"})
	if st, _ := e.raw(tok2, "GET", "/v1/vms/"+a, nil); st != 401 {
		t.Errorf("parent token after fork: %d, want 401", st)
	}
	ctok, _ := lastEnvOp(e, child["id"].(string))["BOAT_API_KEY"].(string)
	if st, _ := e.raw(ctok, "GET", "/v1/vms/"+a, nil); st != 404 {
		t.Errorf("child token on the parent: %d, want 404", st)
	}
	if st, _ := e.raw(ctok, "GET", "/v1/vms/"+child["id"].(string), nil); st != 200 {
		t.Errorf("child token on itself: %d", st)
	}
	// Deleting the VM kills its token.
	e.do(e.owner, "DELETE", "/v1/vms/"+child["id"].(string), nil)
	if st, _ := e.raw(ctok, "GET", "/v1/vms/"+child["id"].(string), nil); st != 401 {
		t.Errorf("token of a deleted VM: %d, want 401", st)
	}
	// A team's own BOAT_API_KEY secret wins over the VM token.
	e.do(e.owner, "PUT", "/v1/secrets/BOAT_API_KEY", map[string]any{"value": "team-key", "inject": true})
	c := e.createVM()
	if got := lastEnvOp(e, c)["BOAT_API_KEY"]; got != nil && got != "team-key" {
		t.Errorf("team key replaced: %v", got)
	}
}

func TestGuestEnvWithoutURL(t *testing.T) {
	e := setup(t)
	a := e.createVM()
	env := lastEnvOp(e, a)
	if env["BOAT_ID"] != a || env["BOAT_API_KEY"] != nil || env["BOAT_URL"] != nil {
		t.Errorf("no -vm-api-url/-public-url: want only BOAT_ID, got %v", env)
	}
}
