package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// r45: a no_env run's self-token is limited to an allowlist and to the commands
// of its own run (the processId counter is guessable); a restricted self-token
// cannot change its VM's auto-stop, resize or delete it; keyAliveIn honours
// NoSelfAPI for vm_ keys.

// noEnvRunWithEarlierCommands creates a VM whose earlier (secret) run started
// two detached commands, then starts a no_env run. It returns the VM's ids, the
// run's self-token and the processId of a command the token itself started.
func (e *env) noEnvRunWithEarlierCommands() (bx, vm, tok string, earlier int, own float64) {
	e.t.Helper()
	e.srv.cfg.VMAPIURL = "https://boat.example"
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "top-secret", "inject": true})
	bx = e.newSandbox(nil)
	vm = e.nativeOf(bx)
	base := "/api/v1/sandboxes/" + bx
	e.ok("POST", base+"/commands", map[string]any{"command": "echo TEAM_SEC=top-secret", "detached": true}, 200)
	e.ok("POST", base+"/commands", map[string]any{"command": "sleep-forever", "detached": true}, 200)
	earlier = 2
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	time.Sleep(2 * time.Millisecond) // the run's mark is later than the earlier records
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{"no_env": true}); st != 200 {
		e.t.Fatalf("no_env start = %d %v", st, out)
	}
	tok = e.currentSelfToken(vm)
	st, out, _ := e.compat(tok, "POST", base+"/commands", map[string]any{"command": "echo mine", "detached": true}, nil)
	if st != 200 {
		e.t.Fatalf("the no_env token's own detached command = %d %v", st, out)
	}
	own, _ = out["processId"].(float64)
	return
}

func TestR45NoEnvSelfTokenCannotReadEarlierRunCommands(t *testing.T) {
	e := setup(t)
	bx, vm, tok, earlier, own := e.noEnvRunWithEarlierCommands()
	base := "/api/v1/sandboxes/" + bx
	if int(own) <= earlier {
		t.Fatalf("own processId %v must follow the earlier %d", own, earlier)
	}
	refused := func(st int) bool { return st == http.StatusForbidden || st == http.StatusNotFound }
	for pid := 1; pid <= earlier+3; pid++ {
		if float64(pid) == own {
			continue
		}
		for _, c := range []struct{ method, suffix string }{{"GET", ""}, {"GET", "/stream"}, {"DELETE", ""}} {
			path := fmt.Sprintf("%s/commands/%d%s", base, pid, c.suffix)
			st, body := e.raw(tok, c.method, path, nil)
			if !refused(st) || strings.Contains(string(body), "top-secret") || strings.Contains(string(body), "sleep-forever") {
				t.Errorf("%s %s with a no_env token = %d %s, want 403/404 and no command text", c.method, path, st, body)
			}
		}
	}
	// The earlier records of the native routes, by command id.
	for _, rec := range e.srv.store.Snapshot().Commands[vm] {
		if rec.ProcessID > earlier {
			continue
		}
		for _, c := range []struct{ method, suffix string }{{"GET", ""}, {"GET", "/logs"}, {"DELETE", ""}, {"POST", "/signal"}} {
			st, body := e.raw(tok, c.method, "/v1/vms/"+vm+"/commands/"+rec.ID+c.suffix, map[string]any{})
			if st != http.StatusForbidden {
				t.Errorf("native %s command %s%s of an earlier run = %d %s, want 403", c.method, rec.ID, c.suffix, st, body)
			}
		}
		// By command id on the compat route too.
		if st, _ := e.raw(tok, "GET", base+"/commands/"+rec.ID, nil); st != http.StatusForbidden {
			t.Errorf("compat status by commandId of an earlier run = %d, want 403", st)
		}
	}
	// The earlier commands were not stopped by those DELETEs.
	for _, rec := range e.srv.store.Snapshot().Commands[vm] {
		if rec.ProcessID == earlier && rec.State != "running" {
			t.Errorf("earlier command %d is %q: the refused DELETE still stopped it", rec.ProcessID, rec.State)
		}
	}
	// The token's own command still works: status, stream, stop.
	mine := fmt.Sprintf("%s/commands/%d", base, int(own))
	if st, body := e.raw(tok, "GET", mine, nil); st != 200 || !strings.Contains(string(body), "echo mine") {
		t.Errorf("own command status = %d %s", st, body)
	}
	if st, body := e.raw(tok, "GET", mine+"/stream", nil); st != 200 || !strings.Contains(string(body), `"exit"`) {
		t.Errorf("own command stream = %d %s", st, body)
	}
	if st, body := e.raw(tok, "DELETE", mine, nil); st != 200 {
		t.Errorf("own command stop = %d %s", st, body)
	}
	for _, rec := range e.srv.store.Snapshot().Commands[vm] {
		if int(own) == rec.ProcessID {
			if st, body := e.raw(tok, "GET", "/v1/vms/"+vm+"/commands/"+rec.ID, nil); st != 200 {
				t.Errorf("native own command status = %d %s", st, body)
			}
			if st, body := e.raw(tok, "GET", "/v1/vms/"+vm+"/commands/"+rec.ID+"/logs", nil); st != 200 {
				t.Errorf("native own command logs = %d %s", st, body)
			}
		}
	}
	// The owner's key still reads the earlier commands.
	if st, body := e.raw(e.owner, "GET", base+"/commands/1", nil); st != 200 {
		t.Errorf("owner reads an earlier command = %d %s", st, body)
	}
}

// Everything off the allowlist is 403 for a no_env token, native and compat,
// and a compat handler that goes through callNative is held to the same list.
func TestR45NoEnvSelfTokenAllowlist(t *testing.T) {
	e := setup(t)
	bx, vm, tok, _, _ := e.noEnvRunWithEarlierCommands()
	native, compat := "/v1/vms/"+vm, "/api/v1/sandboxes/"+bx
	for _, c := range []struct{ method, path string }{
		{"GET", native + "/usage"},
		{"GET", native + "/processes"},
		{"GET", native + "/ports"},
		{"POST", native + "/ports"},
		{"POST", native + "/prompt"},
		{"POST", native + "/prompts"},
		{"POST", native + "/fork"},
		{"POST", native + "/start"},
		{"POST", native + "/ssh"},
		{"GET", native + "/ssh"},
		{"POST", native + "/port-tokens/revoke"},
		{"GET", compat + "/usage"},
		{"GET", compat + "/processes"},
		{"POST", compat + "/prompt"},
		{"POST", compat + "/fork"},
		{"POST", compat + "/resume"},
		{"POST", compat + "/host"},
		{"POST", compat + "/sshkey"},
		{"POST", compat + "/share"},
		{"GET", compat + "/artifacts"},
		{"GET", "/api/v1/sandboxes"},
	} {
		st, body := e.raw(tok, c.method, c.path, map[string]any{})
		if st != http.StatusForbidden && st != http.StatusNotFound {
			t.Errorf("%s %s with a no_env token = %d %s, want 403/404", c.method, c.path, st, body)
		}
	}
	for _, c := range []struct{ method, path string }{
		{"GET", native},
		{"GET", compat},
		{"GET", native + "/files/list?path=/"},
		{"GET", compat + "/files/list?path=/"},
		{"GET", native + "/ai-usage"},
	} {
		if st, body := e.raw(tok, c.method, c.path, nil); st != 200 {
			t.Errorf("allowed %s %s = %d %s", c.method, c.path, st, body)
		}
	}
	if st, body := e.raw(tok, "POST", compat+"/files/folder", map[string]any{"path": "/tmp/r45"}); st != 200 {
		t.Errorf("compat files/folder (callNative) = %d %s", st, body)
	}
	if st, body := e.raw(tok, "POST", native+"/exec", map[string]any{"cmd": []string{"true"}}); st != 200 {
		t.Errorf("exec = %d %s", st, body)
	}
	if st, body := e.raw(tok, "POST", compat+"/stop", map[string]any{}); st != 200 && st != 202 {
		t.Errorf("stopping itself = %d %s", st, body)
	}
}

// A compat handler is held to the allowlist by callNative itself: the native
// history route is refused for a no_env token even when called through it.
func TestR45CallNativeRefusesNoEnvSelfToken(t *testing.T) {
	e := setup(t)
	_, vm, tok, _, _ := e.noEnvRunWithEarlierCommands()
	p := e.srv.authenticate("Bearer " + tok)
	if p == nil || p.VMID != vm {
		t.Fatalf("self-token principal = %+v", p)
	}
	r, _ := http.NewRequest("GET", "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, &reqInfo{id: "r45"}))
	for _, pattern := range []string{"GET /v1/vms/{id}/command-history", "GET /v1/vms/{id}/snapshots", "GET /v1/vms/{id}/processes"} {
		res := e.srv.callNative(r, p, pattern, func(w http.ResponseWriter, r *http.Request, p *principal) {
			t.Errorf("%s ran for a no_env self-token", pattern)
		}, vm, nil)
		if res.status != http.StatusForbidden {
			t.Errorf("callNative %s = %d, want 403", pattern, res.status)
		}
	}
}

// A no_env or RestrictSelf self-token cannot change its VM's auto-stop, resize
// it or delete it; an ordinary self-token can still do each.
func TestR45RestrictedSelfTokenLifecycle(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	bx := e.newSandbox(nil)
	vm := e.nativeOf(bx)
	native, compat := "/v1/vms/"+vm, "/api/v1/sandboxes/"+bx
	tok := e.currentSelfToken(vm)

	// Control: an ordinary self-token may.
	if st, out := e.do(tok, "PATCH", native, map[string]any{"ttl_s": 3600}); st != 200 {
		t.Fatalf("ordinary self-token PATCH ttl_s = %d %v", st, out)
	}
	if st, out := e.do(tok, "PATCH", native, map[string]any{"name": "renamed"}); st != 200 {
		t.Errorf("ordinary self-token rename = %d %v", st, out)
	}

	check := func(label string) {
		t.Helper()
		for _, c := range []struct {
			method, path string
			body         any
		}{
			{"PATCH", native, map[string]any{"ttl_s": 0}},
			{"PATCH", native, map[string]any{"idle_stop_s": -1}},
			{"PATCH", native, map[string]any{"extend_s": 60}},
			{"POST", native + "/resize", map[string]any{"size": "large"}},
			{"DELETE", native, nil},
			{"PATCH", compat, map[string]any{"ttlSeconds": 100}},
			{"DELETE", compat, nil},
		} {
			st, body := e.raw(tok, c.method, c.path, c.body)
			if st != http.StatusForbidden {
				t.Errorf("%s: %s %s = %d %s, want 403", label, c.method, c.path, st, body)
			}
		}
		if st, out := e.do(e.owner, "GET", native, nil); st != 200 || out["state"] == "deleted" {
			t.Errorf("%s: the VM must still be there: %d %v", label, st, out)
		}
	}

	// RestrictSelf.
	if err := e.srv.store.Update(func(st *State) error { st.cowVMToken(vm).RestrictSelf = true; return nil }); err != nil {
		t.Fatal(err)
	}
	check("RestrictSelf")
	// A restricted token's rename is still allowed.
	if st, out := e.do(tok, "PATCH", native, map[string]any{"name": "again"}); st != 200 {
		t.Errorf("restricted self-token rename = %d %v", st, out)
	}
	if err := e.srv.store.Update(func(st *State) error { st.cowVMToken(vm).RestrictSelf = false; return nil }); err != nil {
		t.Fatal(err)
	}

	// no_env.
	e.do(e.owner, "POST", native+"/stop", nil)
	if st, out := e.do(e.owner, "POST", native+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("no_env start = %d %v", st, out)
	}
	tok = e.currentSelfToken(vm)
	check("no_env")
}

// keyAliveIn: a vm_ key stands only while its token carries the self API.
func TestR45KeyAliveInHonoursNoSelfAPI(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	vm := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{})
	st := e.srv.store.Snapshot()
	tok := st.VMTokens[vm]
	if tok == nil {
		t.Fatal("no VM token")
	}
	team := tok.TeamID
	key := fmt.Sprintf("vm_%s@%d", vm, tok.Gen)
	if !keyAliveIn(st, key, team, "exec", time.Now()) {
		t.Fatal("control: a live vm_ key must stand")
	}
	if err := e.srv.store.Update(func(s *State) error { s.cowVMToken(vm).NoSelfAPI = true; return nil }); err != nil {
		t.Fatal(err)
	}
	st = e.srv.store.Snapshot()
	for _, k := range []string{key, "vm_" + vm} {
		if keyAliveIn(st, k, team, "exec", time.Now()) || keyAliveIn(st, k, team, "", time.Now()) {
			t.Errorf("keyAliveIn(%s) stands for a NoSelfAPI token", k)
		}
	}
}
