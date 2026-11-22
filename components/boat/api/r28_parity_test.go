package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func (e *env) setEnvFailures(push, start bool) {
	e.boatd.mu.Lock()
	e.boatd.envFail, e.boatd.startEnvFail = push, start
	e.boatd.mu.Unlock()
}

// A failed env push is remembered per VM: GET and list say agent_ready=false with
// env_error, the compat sandbox is in state "error"; a later good push, or the
// VM's deletion, clears it.
func TestR28EnvPushFailurePersists(t *testing.T) {
	e := setup(t)
	e.setEnvFailures(true, false)
	bx := e.newSandbox(map[string]any{"env": map[string]string{"A": "1"}})
	id := e.nativeOf(bx)
	sb := e.ok("GET", "/api/v1/sandboxes/"+bx, nil, 200)["sandbox"].(map[string]any)
	if sb["state"] != "error" || sb["error"] == nil {
		t.Fatalf("compat get = %v, want state error with error", sb)
	}
	list := e.ok("GET", "/api/v1/sandboxes", nil, 200)["sandboxes"].([]any)
	if l := list[0].(map[string]any); l["state"] != "error" || l["error"] == nil {
		t.Fatalf("compat list = %v", l)
	}
	st, vm := e.do(e.owner, "GET", "/v1/vms/"+id, nil)
	if st != 200 || vm["agent_ready"] != false || vm["env_error"] == nil {
		t.Fatalf("native get = %d %v", st, vm)
	}
	_, lst := e.do(e.owner, "GET", "/v1/vms", nil)
	if v := lst["vms"].([]any)[0].(map[string]any); v["agent_ready"] != false || v["env_error"] == nil {
		t.Fatalf("native list = %v", v)
	}
	// Persisted in the store, so a restart keeps it.
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	st2, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Snapshot().VMEnvErrors[id] == "" {
		t.Error("env error not persisted")
	}
	st2.Close()

	// A later successful push clears it.
	e.setEnvFailures(false, false)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"env": map[string]string{"B": "2"}}); st != 200 {
		t.Fatalf("start running with env: %d %v", st, out)
	}
	_, vm = e.do(e.owner, "GET", "/v1/vms/"+id, nil)
	if vm["env_error"] != nil {
		t.Fatalf("env_error survived a good push: %v", vm)
	}
	if sb := e.ok("GET", "/api/v1/sandboxes/"+bx, nil, 200)["sandbox"].(map[string]any); sb["state"] == "error" {
		t.Fatalf("compat still error: %v", sb)
	}
	// A failed update of a healthy running VM changes nothing (r31): the guest kept
	// its env, so there is no env error to remember.
	e.setEnvFailures(true, false)
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"env": map[string]string{"C": "3"}}); st != http.StatusBadGateway {
		t.Fatalf("failed update of a healthy VM = %d, want 502", st)
	}
	if e.srv.store.Snapshot().VMEnvErrors[id] != "" {
		t.Fatal("failed update of a healthy running VM was recorded as an env error")
	}
	// Deletion drops a record made by a failed create push.
	id = e.nativeOf(e.newSandbox(nil))
	if e.srv.store.Snapshot().VMEnvErrors[id] == "" {
		t.Fatal("failed create push not recorded")
	}
	if st, _ := e.do(e.owner, "DELETE", "/v1/vms/"+id, nil); st/100 != 2 {
		t.Fatalf("delete: %d", st)
	}
	waitFor(t, "env error dropped", func() bool { return e.srv.store.Snapshot().VMEnvErrors[id] == "" })
}

// A cold start whose boot-time set_env failed is not a success.
func TestR28ColdStartEnvFailure(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s"})
	id := e.createVM()
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil); st != 200 {
		t.Fatalf("stop: %d", st)
	}
	e.setEnvFailures(false, true)
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil)
	if st != 200 || out["agent_ready"] != false || out["env_error"] == nil {
		t.Fatalf("native cold start = %d %v, want 200 with agent_ready=false and env_error (r30)", st, out)
	}
	_, vm := e.do(e.owner, "GET", "/v1/vms/"+id, nil)
	if vm["agent_ready"] != false || vm["env_error"] == nil {
		t.Fatalf("after failed start: %v", vm)
	}
	// A clean cold start clears it.
	e.setEnvFailures(false, false)
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil); st != 200 {
		t.Fatalf("good cold start: %d %v", st, out)
	}
	_, vm = e.do(e.owner, "GET", "/v1/vms/"+id, nil)
	if vm["env_error"] != nil {
		t.Fatalf("env_error after good start: %v", vm)
	}
}

func TestR28CompatResumeEnvFailure(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	base := "/api/v1/sandboxes/" + bx
	e.ok("POST", base+"/stop", nil, 202)
	e.setEnvFailures(false, true)
	st, out, _ := e.compat(e.owner, "POST", base+"/resume", nil, nil)
	if sbx, _ := out["sandbox"].(map[string]any); st != 202 || sbx["state"] != "error" || sbx["error"] == nil {
		t.Fatalf("resume with failed env = %d %v, want 202 with the sandbox in state error (r30)", st, out)
	}
	sb := e.ok("GET", base, nil, 200)["sandbox"].(map[string]any)
	if sb["state"] != "error" || sb["error"] == nil {
		t.Fatalf("sandbox after failed resume = %v", sb)
	}
	e.setEnvFailures(false, false)
	e.ok("POST", base+"/stop", nil, 202)
	e.ok("POST", base+"/resume", nil, 202)
	if sb := e.ok("GET", base, nil, 200)["sandbox"].(map[string]any); sb["state"] == "error" {
		t.Fatalf("sandbox after good resume = %v", sb)
	}
}

// Native usage keeps exact window bounds; compat billing is explicitly unsupported.
func TestR28CompatUsageWindow(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	id := e.nativeOf(bx)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	e.boatd.mu.Lock()
	e.boatd.usageWindowFn = func(q url.Values) map[string]any {
		until := time.Now().UTC()
		if v := q.Get("until"); v != "" {
			until, _ = time.Parse(time.RFC3339Nano, v)
		}
		since, _ := time.Parse(time.RFC3339Nano, q.Get("since"))
		return map[string]any{"exact": true, "since": since, "until": until, "intervals": []any{
			map[string]any{"kind": "vm", "id": id, "team": q.Get("team"), "size": "default", "start": t0, "end": t0.Add(1000 * time.Second)},
		}}
	}
	e.boatd.mu.Unlock()
	since, until := t0.Add(100*time.Second), t0.Add(300*time.Second)
	path := fmt.Sprintf("/api/v1/sandboxes/%s/usage?since=%d&until=%s", bx, since.Unix(), until.Format(time.RFC3339))
	if st, out, _ := e.compat(e.owner, "GET", path, nil, nil); st != 501 || errCode(out) != "not_supported" {
		t.Fatalf("compat usage = %d %v, want 501 not_supported", st, out)
	}
	native := "/v1/vms/" + id + "/usage"
	st, nat := e.do(e.owner, "GET", fmt.Sprintf("%s?since=%d&until=%d", native, since.Unix(), until.Unix()), nil)
	if st != 200 || nat["vm_seconds"] != float64(200) {
		t.Fatalf("native usage = %d %v", st, nat)
	}
	for field, want := range map[string]time.Time{"since": since, "until": until} {
		if got, err := time.Parse(time.RFC3339Nano, nat[field].(string)); err != nil || !got.Equal(want) {
			t.Errorf("%s = %v, want %v", field, nat[field], want)
		}
	}
	if st, out := e.do(e.owner, "GET", fmt.Sprintf("%s?since=%d", native, since.Unix()), nil); st != 200 || out["vm_seconds"] != float64(900) {
		t.Errorf("native since-only usage = %d %v, want 900 seconds", st, out)
	}
	for _, q := range []string{"since=yesterday", "until=x", "since=-5", fmt.Sprintf("since=%d&until=%d", until.Unix(), since.Unix())} {
		if st, out := e.do(e.owner, "GET", native+"?"+q, nil); st != 400 {
			t.Errorf("native usage?%s = %d %v, want 400", q, st, out)
		}
	}
}

// no_env on a native start holds for prompts, across a restart, until a normal start.
func TestR28NativeNoEnvPersists(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "top-secret"})
	_, vm := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "dev", "secrets": []string{"TEAM_SEC"}})
	id := vm["id"].(string)
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("start no_env: %d %v", st, out)
	}
	prompt := func(base string, tok string, h *env) map[string]any {
		t.Helper()
		st, run := h.do(tok, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": "hi"})
		if st != 202 {
			t.Fatalf("prompt: %d %v", st, run)
		}
		e.boatd.mu.Lock()
		defer e.boatd.mu.Unlock()
		return e.boatd.cmds[run["cmd_id"].(string)].env
	}
	if env := prompt("", e.owner, e); env["TEAM_SEC"] != nil {
		t.Fatalf("no_env prompt got team secrets: %v", env)
	}
	// A restarted server on the same store still honors it.
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	st2, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	s2 := newServer(st2, e.srv.keys, e.srv.boatd, e.srv.cfg, newLimiter(0, 0), &auditLog{w: &bytes.Buffer{}})
	api2 := httptest.NewServer(recorder(s2))
	defer api2.Close()
	e2 := &env{t: t, api: api2, srv: s2, boatd: e.boatd, owner: e.owner, dir: e.dir}
	if env := prompt("", e.owner, e2); env["TEAM_SEC"] != nil {
		t.Fatalf("restarted server leaked team secrets: %v", env)
	}
	// r46: no_env is sticky: a stop and a plain start keep it, no team secrets.
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil)
	if env := prompt("", e.owner, e); env["TEAM_SEC"] != nil {
		t.Fatalf("a plain start of a no_env VM restored secrets: %v", env)
	}
	if _, on := e.srv.store.Snapshot().NoEnvRuns[id]; !on {
		t.Error("no_env marker did not survive a plain start")
	}
}
