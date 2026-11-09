package main

import (
	"testing"
)

// boatd's force stop, fail_fast and batch create (PROTOCOL.md), through the public routes.
func TestForceStopFailFastAndBatch(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	e.do(e.owner, "PUT", "/v1/secrets/TOKEN", map[string]any{"value": "s"})
	id := e.createVM()

	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", map[string]any{"force": true}); st != 200 || out["has_snapshot"] != false {
		t.Errorf("force stop: %d %v", st, out)
	}
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil); st != 200 {
		t.Errorf("plain stop without a body: %d", st)
	}
	e.boatd.mu.Lock()
	if len(e.boatd.stops) != 2 || e.boatd.stops[0]["force"] != true || e.boatd.stops[1]["force"] != nil {
		t.Errorf("stop bodies sent to boatd: %v", e.boatd.stops)
	}
	e.boatd.mu.Unlock()

	// fail_fast is forwarded on create, start and fork; boatd's refusal passes through.
	e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"fail_fast": true})
	e.do(e.owner, "POST", "/v1/vms/"+id+"/fork", map[string]any{"fail_fast": true})
	e.do(e.owner, "POST", "/v1/vms", map[string]any{"fail_fast": true})
	e.boatd.mu.Lock()
	if e.boatd.starts[len(e.boatd.starts)-1]["fail_fast"] != true || e.boatd.forks[0]["fail_fast"] != true ||
		e.boatd.creates[len(e.boatd.creates)-1]["fail_fast"] != true {
		t.Errorf("fail_fast not forwarded: start %v fork %v create %v", e.boatd.starts, e.boatd.forks, e.boatd.creates)
	}
	e.boatd.refuse = "no_ready_machine"
	e.boatd.mu.Unlock()
	for _, c := range []struct{ path string }{{"/v1/vms"}, {"/v1/vms/" + id + "/start"}, {"/v1/vms/" + id + "/fork"}} {
		st, out := e.do(e.owner, "POST", c.path, map[string]any{"fail_fast": true})
		if code, _ := out["error"].(map[string]any)["code"].(string); st != 503 || code != "no_ready_machine" {
			t.Errorf("%s on a full host: %d %v", c.path, st, out)
		}
	}
	e.boatd.mu.Lock()
	e.boatd.refuse = "host_capacity"
	e.boatd.mu.Unlock()
	if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{}); st != 503 || out["error"].(map[string]any)["code"] != "host_capacity" {
		t.Errorf("host_capacity: %d %v", st, out)
	}
	e.boatd.mu.Lock()
	e.boatd.refuse = ""
	e.boatd.mu.Unlock()

	// count: one request, every VM fully set up (selection, own token, env).
	st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "w", "count": 3, "secrets": []string{}})
	vms, _ := out["vms"].([]any)
	if st != 201 || len(vms) != 3 {
		t.Fatalf("batch: %d %v", st, out)
	}
	for i, x := range vms {
		vm := x.(map[string]any)
		vid := vm["id"].(string)
		if want := "w-" + string(rune('1'+i)); vm["name"] != want {
			t.Errorf("batch name %d: %v, want %s", i, vm["name"], want)
		}
		env := lastEnvOp(e, vid)
		if env["BOAT_ID"] != vid || env["BOAT_API_KEY"] == nil || env["TOKEN"] != nil {
			t.Errorf("batch VM %s env: %v", vid, env)
		}
		if tok, _ := env["BOAT_API_KEY"].(string); tok != "" {
			if st, _ := e.raw(tok, "GET", "/v1/vms/"+vid, nil); st != 200 {
				t.Errorf("batch VM %s token on itself: %d", vid, st)
			}
		}
	}
	e.boatd.mu.Lock()
	if last := e.boatd.creates[len(e.boatd.creates)-1]; last["count"] != float64(3) {
		t.Errorf("count not forwarded: %v", last)
	}
	e.boatd.mu.Unlock()
	for _, bad := range []any{0.5, -1, 17} {
		if st, _ := e.do(e.owner, "POST", "/v1/vms", map[string]any{"count": bad}); st != 400 {
			t.Errorf("count %v: %d, want 400", bad, st)
		}
	}
	// count 1 keeps the single-VM answer.
	if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"count": 1}); st != 201 || out["id"] == nil {
		t.Errorf("count 1: %d %v", st, out)
	}
}
