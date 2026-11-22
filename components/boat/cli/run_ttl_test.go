package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// boat run creates the VM with boat.dev's 15-minute wall-clock auto-stop and,
// while the CLI is alive, keeps pushing it forward; the heartbeat ends with the job.
func TestRunHeartbeatExtendsTTLWhileAlive(t *testing.T) {
	old := runHeartbeatEvery
	runHeartbeatEvery = 20 * time.Millisecond
	t.Cleanup(func() { runHeartbeatEvery = old })

	api := newJobAPI(t)
	api.execLines = func(w http.ResponseWriter, r *http.Request, cmd []string) {
		deadline := time.Now().Add(5 * time.Second)
		for api.count("PATCH /v1/vms/vm-1") < 2 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		json.NewEncoder(w).Encode(obj{"exit": 0})
	}
	h := newHarness(t, api)
	if code := h.a.run([]string{"run", "--", "true"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	var create obj
	json.Unmarshal([]byte(api.lastBody("POST /v1/vms")), &create)
	if create["ttl_s"] != float64(900) || create["idle_stop_s"] != float64(runIdleStopS) {
		t.Errorf("create body %v, want ttl_s 900", create)
	}
	if n := api.count("PATCH /v1/vms/vm-1"); n < 2 {
		t.Fatalf("only %d heartbeat PATCHes: %v", n, api.reqs)
	}
	for _, b := range api.bodies["PATCH /v1/vms/vm-1"] {
		if b != `{"ttl_s":900}` {
			t.Errorf("heartbeat body %q", b)
		}
	}
	// None after the job: the last request is the DELETE, and no PATCH follows it.
	n := api.count("PATCH /v1/vms/vm-1")
	time.Sleep(100 * time.Millisecond)
	if api.count("PATCH /v1/vms/vm-1") != n || api.count("DELETE /v1/vms/vm-1") != 1 {
		t.Errorf("heartbeat outlived the job: %v", api.reqs)
	}
	last := api.reqs[len(api.reqs)-1]
	if !strings.HasPrefix(last, "DELETE ") {
		t.Errorf("last request %q, want the DELETE", last)
	}
}

func TestBareEnvNameOnForkAndResumeIsUsageError(t *testing.T) {
	for _, args := range [][]string{
		{"new", "-e", "base"}, {"new", "--env", "base"}, {"run", "-e", "base", "--", "true"},
		{"fork", "-e", "base", "vm1"}, {"fork", "--env", "base", "vm1"},
		{"resume", "-e", "base", "vm1"}, {"start", "--env", "base", "vm1"},
	} {
		api := newJobAPI(t)
		h := newHarness(t, api)
		if code := h.a.run(args); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if !strings.Contains(h.err.String(), "--environment base") {
			t.Errorf("%v: message %q does not point at --environment", args, h.err)
		}
		if api.count("POST /v1/vms/vm1/fork")+api.count("POST /v1/vms/vm1/start") != 0 {
			t.Errorf("%v: made a request: %v", args, api.reqs)
		}
	}
}
