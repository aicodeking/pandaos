package main

import (
	"testing"
	"time"
)

// r32: prompt/history bookkeeping is lazy (no fsync wait), compat prompt refuses
// while the environment is in error, compat unshare is unsupported.

func r32Timed(t *testing.T, name string, fn func()) {
	t.Helper()
	t0 := time.Now()
	fn()
	if took := time.Since(t0); took > r31Budget {
		t.Errorf("%s took %v under %v fsync (budget %v): it waits for the disk", name, took, r31FsyncDelay, r31Budget)
	}
}

// POST /prompts records the run lazily and shows it at once.
func TestR32PromptPostDoesNotWaitForFsync(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	slowFsync(t, r31FsyncDelay)
	var st int
	var run map[string]any
	r32Timed(t, "POST prompts", func() {
		st, run = e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": "go", "new": true})
	})
	if st != 202 {
		t.Fatalf("prompt: %d %v", st, run)
	}
	snap := e.srv.store.Snapshot()
	if ps := snap.Prompts[id]; len(ps) != 1 || ps[0].ID != run["id"] {
		t.Errorf("prompts = %+v, want the new run", ps)
	}
	if cs := snap.Conversations[id]; len(cs) != 1 || cs[0].PromptCount != 1 {
		t.Errorf("conversations = %+v, want one with one prompt", cs)
	}
}

// GET /prompts after the guest finished a turn updates it without an fsync wait.
func TestR32ListPromptsRefreshDoesNotWaitForFsync(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	_, run := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": "go", "new": true})
	if run["state"] != "running" {
		t.Fatalf("setup run = %v", run)
	}
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	slowFsync(t, r31FsyncDelay)
	var st int
	var out map[string]any
	r32Timed(t, "GET prompts", func() { st, out = e.do(e.owner, "GET", "/v1/vms/"+id+"/prompts", nil) })
	if st != 200 {
		t.Fatalf("list: %d %v", st, out)
	}
	if ps := e.srv.store.Snapshot().Prompts[id]; len(ps) != 1 || ps[0].State != "completed" || ps[0].Finished == nil {
		t.Errorf("prompt after refresh = %+v, want completed", ps)
	}
	if l, _ := out["prompts"].([]any); len(l) != 1 || l[0].(map[string]any)["state"] != "completed" {
		t.Errorf("response prompts = %v, want completed", out["prompts"])
	}
}

// GET /command-history after a command ended refreshes without an fsync wait.
func TestR32CommandHistoryRefreshDoesNotWaitForFsync(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	st, cmd := e.do(e.owner, "POST", "/v1/vms/"+id+"/commands", map[string]any{"cmd": []string{"sleep", "60"}})
	if st != 201 {
		t.Fatalf("command: %d %v", st, cmd)
	}
	e.boatd.mu.Lock()
	for _, c := range e.boatd.cmds {
		c.running, c.exit = false, float64(0)
	}
	e.boatd.mu.Unlock()
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	slowFsync(t, r31FsyncDelay)
	var out map[string]any
	r32Timed(t, "GET command-history", func() { st, out = e.do(e.owner, "GET", "/v1/vms/"+id+"/command-history", nil) })
	if st != 200 {
		t.Fatalf("history: %d %v", st, out)
	}
	recs := e.srv.store.Snapshot().Commands[id]
	if len(recs) != 1 || recs[0].State == "running" || recs[0].FinishedAt == nil {
		t.Errorf("history after refresh = %+v, want the command finished", recs)
	}
	if l, _ := out["commands"].([]any); len(l) != 1 || l[0].(map[string]any)["state"] == "running" {
		t.Errorf("response commands = %v, want finished", out["commands"])
	}
}

// Compat prompt refuses with 409 sandbox_not_ready while the environment is in
// error, starts nothing, and works again after the retry.
func TestR32CompatPromptRefusesWhileEnvError(t *testing.T) {
	e := setup(t)
	e.setEnvFailures(true, false)
	bx := e.newSandbox(nil)
	id := e.nativeOf(bx)
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/prompt", map[string]any{"prompt": "go"}, nil)
	if st != 409 || errCode(out) != "sandbox_not_ready" {
		t.Fatalf("compat prompt = %d %v, want 409 sandbox_not_ready", st, out)
	}
	if ps := e.srv.store.Snapshot().Prompts[id]; len(ps) != 0 {
		t.Fatalf("a refused prompt was recorded: %+v", ps)
	}
	e.setEnvFailures(false, false)
	e.ok("POST", "/api/v1/sandboxes/"+bx+"/resume", nil, 202)
	if st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/prompt", map[string]any{"prompt": "go"}, nil); st != 202 {
		t.Fatalf("compat prompt after the retry = %d %v", st, out)
	}
}

// Unsupported compat unshare changes no route tokens, regardless of exec scope.
func TestR32CompatUnshareDoesNotRevoke(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	id := e.nativeOf(bx)
	before := e.srv.store.Snapshot().RouteGens[id]
	noExec := e.newKey("vms:write")
	for _, token := range []string{noExec, e.newKey("vms:write", "exec")} {
		if st, out, _ := e.compat(token, "POST", "/api/v1/sandboxes/"+bx+"/unshare", nil, nil); st != 501 || errCode(out) != "not_supported" {
			t.Fatalf("unshare = %d %v, want 501 not_supported", st, out)
		}
		if got := e.srv.store.Snapshot().RouteGens[id]; got != before {
			t.Fatalf("unsupported unshare changed generation %d -> %d", before, got)
		}
	}
	if st, _ := e.do(noExec, "POST", "/v1/vms/"+id+"/port-tokens/revoke", nil); st != 403 {
		t.Fatalf("native revoke without exec = %d, want 403", st)
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/port-tokens/revoke", nil); st != 200 {
		t.Fatalf("native revoke = %d %v", st, out)
	}
	if got := e.srv.store.Snapshot().RouteGens[id]; got != before+1 {
		t.Fatalf("native revoke generation = %d, want %d", got, before+1)
	}
}
