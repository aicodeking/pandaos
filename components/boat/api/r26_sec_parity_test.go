package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// r26LastCmdEnv is the env of the newest detached command in the fake guest.
func r26LastCmdEnv(e *env) map[string]any {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	best := 0
	var out map[string]any
	for id, c := range e.boatd.cmds {
		var n int
		fmt.Sscanf(id, "c_%d", &n)
		if n > best {
			best, out = n, c.env
		}
	}
	return out
}

// r26Proxy puts a proxy between boat-api and the fake boatd that answers 502 to
// every request fail() selects. The returned func restores the direct link.
func r26Proxy(e *env, fail func(*http.Request) bool) func() {
	e.t.Helper()
	target, err := url.Parse(e.srv.boatd.base)
	if err != nil {
		e.t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail(r) {
			http.Error(w, `{"error":"injected"}`, http.StatusBadGateway)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	e.t.Cleanup(ps.Close)
	orig := e.srv.boatd
	e.srv.boatd = newBoatd(ps.URL)
	return func() { e.srv.boatd = orig }
}

// 1+2: a sandbox created with noEnv holds no team secrets, at create and in the
// agent turns that follow; a no_env resume does the same for its run only.
func TestR26NoEnvCreateAndPromptsHoldNoTeamSecrets(t *testing.T) {
	e := setup(t)
	if st, out := e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "top-secret", "inject": true}); st != 200 && st != 201 {
		t.Fatalf("put secret: %d %v", st, out)
	}

	// Control: a plain sandbox still gets the secret (the fix is not "inject nothing").
	plain := e.nativeOf(e.newSandbox(nil))
	e.boatd.mu.Lock()
	plainEnv, _ := e.boatd.vms[plain]["env"].(map[string]any)
	e.boatd.mu.Unlock()
	if plainEnv["TEAM_SEC"] != "top-secret" {
		t.Fatalf("control sandbox lost its team secret: %v", plainEnv)
	}

	bx := e.newSandbox(map[string]any{"noEnv": true})
	vm := e.nativeOf(bx)
	e.boatd.mu.Lock()
	vmEnv, _ := e.boatd.vms[vm]["env"].(map[string]any)
	e.boatd.mu.Unlock()
	if _, leaked := vmEnv["TEAM_SEC"]; leaked {
		t.Fatalf("noEnv create injected a team secret: %v", vmEnv)
	}
	if sel, ok := e.srv.store.Snapshot().VMSecrets[vm]; !ok || len(sel) != 0 {
		t.Fatalf("noEnv VM selection = %v (recorded %v), want recorded empty", sel, ok)
	}

	// An agent turn derives its env from the VM's recorded selection: no secret.
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/prompts", map[string]any{"prompt": "go", "new": true}); st != 202 {
		t.Fatalf("prompt: %d %v", st, out)
	}
	if env := r26LastCmdEnv(e); env["TEAM_SEC"] != nil {
		t.Errorf("prompt on a noEnv VM loaded team secrets: %v", env)
	}

	// The plain sandbox's turns do carry it.
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+plain+"/prompts", map[string]any{"prompt": "go", "new": true}); st != 202 {
		t.Fatalf("plain prompt: %d %v", st, out)
	}
	if env := r26LastCmdEnv(e); env["TEAM_SEC"] != "top-secret" {
		t.Errorf("prompt on a plain VM lost its secret: %v", env)
	}
}

// 2 (resume): the no_env marker covers prompts after a no_env resume, and (r46:
// sticky) a later plain resume stays no_env.
func TestR26NoEnvResumeTurnsHoldNoTeamSecrets(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "top-secret", "inject": true})
	bx := e.newSandbox(nil)
	vm := e.nativeOf(bx)
	path := "/api/v1/sandboxes/" + bx
	prompt := func() map[string]any {
		t.Helper()
		if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/prompts", map[string]any{"prompt": "go", "new": true}); st != 202 {
			t.Fatalf("prompt: %d %v", st, out)
		}
		return r26LastCmdEnv(e)
	}
	e.ok("POST", path+"/stop", nil, 202)
	e.ok("POST", path+"/resume", map[string]any{"noEnv": true}, 202)
	if env := prompt(); env["TEAM_SEC"] != nil {
		t.Errorf("turn in a no_env run loaded team secrets: %v", env)
	}
	e.ok("POST", path+"/stop", nil, 202)
	e.ok("POST", path+"/resume", nil, 202)
	if env := prompt(); env["TEAM_SEC"] != nil {
		t.Errorf("turn after a plain resume of a no_env sandbox got team secrets: %v", env)
	}
}

// 3: sharing a snapshot of a VM with SSH policy but no token yet initializes
// the token and pushes the rotated credentials before answering; a failed push
// fails the request and leaves no share.
func TestR26ShareInitializesTokenAndPushesSynchronously(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	id := e.createVM()
	sid := e.snapshotOf(id)
	path := "/v1/vms/" + id + "/snapshots/" + sid + "/shares"
	if err := e.srv.store.Update(func(st *State) error { delete(st.VMTokens, id); return nil }); err != nil {
		t.Fatal(err)
	}

	// Push failure: the guest env cannot be written, so the share must not exist.
	restore := r26Proxy(e, func(r *http.Request) bool {
		return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/vms/"+id+"/env")
	})
	st, out := e.do(e.owner, "POST", path, map[string]any{})
	restore()
	if st != http.StatusBadGateway || nativeErrCode(out) != "credential_push_failed" {
		t.Fatalf("share with failing push: %d %v, want 502 credential_push_failed", st, out)
	}
	if n := len(e.srv.store.Snapshot().SnapShares); n != 0 {
		t.Errorf("failed push left %d shares", n)
	}

	// Success: token initialized, and the push happened before the 201 returned.
	if err := e.srv.store.Update(func(st *State) error { delete(st.VMTokens, id); return nil }); err != nil {
		t.Fatal(err)
	}
	st, out = e.do(e.owner, "POST", path, map[string]any{})
	if st != 201 {
		t.Fatalf("share: %d %v", st, out)
	}
	tok := e.srv.store.Snapshot().VMTokens[id]
	if tok == nil || tok.Gen < 1 || tok.TeamID != e.teamID() {
		t.Fatalf("share left VM token %+v, want an initialized one", tok)
	}
	e.boatd.mu.Lock()
	cfg := e.boatd.files[id+":/etc/boat/ssh-auth.json"]
	e.boatd.mu.Unlock()
	if len(cfg) == 0 {
		t.Error("rotated SSH state was not on the VM when the share answered")
	}
}

// 4: webhook update/rotate and the secrets update re-check the caller's
// authority inside the store transaction: a key revoked after route
// authorization changes nothing.
func TestR26CompatAdminWritesRevalidateInsideTransaction(t *testing.T) {
	e := setup(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(ts.Close)
	cr := e.ok("POST", "/api/v1/webhooks", map[string]any{"name": "h", "url": ts.URL, "events": []string{"sandbox.ready"}}, 201)
	hid := cr["webhook"].(map[string]any)["id"].(string)

	cases := []struct {
		name, method, path string
		body               any
	}{
		{"hook update", "PATCH", "/api/v1/webhooks/" + hid, map[string]any{"name": "renamed"}},
		{"secrets update", "POST", "/api/v1/secrets", map[string]any{"envContents": "A_SECRET=1\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key := e.newKey("admin", "vms:read", "vms:write")
			status, out, _ := r21PausedWrite(t, e, key, c.method, c.path, c.body, func() {
				r21ChangeAuthority(t, e, key, "delete-key")
			})
			if status != http.StatusUnauthorized {
				t.Fatalf("%s by a revoked key: %d %v, want 401", c.name, status, out)
			}
		})
	}
	t.Run("hook rotate", func(t *testing.T) {
		// Rotate reads no body, so there is no pause point: run the handler with
		// the principal authenticated before the key was revoked.
		key := e.newKey("admin", "vms:read", "vms:write")
		p := e.srv.authenticate("Bearer " + key)
		if p == nil {
			t.Fatal("key does not authenticate")
		}
		r21ChangeAuthority(t, e, key, "delete-key")
		req := httptest.NewRequest("POST", "/api/v1/webhooks/"+hid+"/rotate", nil)
		req.SetPathValue("id", hid)
		req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, &reqInfo{id: "r26-rotate"}))
		w := httptest.NewRecorder()
		e.srv.compatHookRotate(w, req, p)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("rotate by a revoked key: %d %s, want 401", w.Code, w.Body)
		}
	})
	h := e.srv.store.Snapshot().Webhooks[hid]
	if h.Name != "h" || h.PrevSealedSecret != "" {
		t.Errorf("revoked key changed the hook: %+v", h)
	}
	if e.srv.store.Snapshot().Secrets[secretKey(e.teamID(), "A_SECRET")] != nil {
		t.Error("revoked key wrote a secret")
	}
	// The owner's own writes still work.
	e.ok("PATCH", "/api/v1/webhooks/"+hid, map[string]any{"name": "renamed"}, 200)
	e.ok("POST", "/api/v1/webhooks/"+hid+"/rotate", nil, 200)
	e.ok("POST", "/api/v1/secrets", map[string]any{"envContents": "A_SECRET=1\n"}, 200)
}

// 5: a resume that fails to start the VM leaves the stop operation as it was;
// the one that succeeds supersedes it.
func TestR26FailedResumeKeepsStopOperation(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	vm := e.nativeOf(bx)
	path := "/api/v1/sandboxes/" + bx
	e.ok("POST", path+"/stop", nil, 202)
	status := func() any {
		st := e.srv.store.Snapshot()
		return st.StopOps[st.StopLast[vm]]["status"]
	}
	before := status()
	if before == "superseded" || before == nil {
		t.Fatalf("stop op status before resume = %v", before)
	}
	restore := r26Proxy(e, func(r *http.Request) bool {
		return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/vms/"+vm+"/start")
	})
	st, out, _ := e.compat(e.owner, "POST", path+"/resume", map[string]any{}, nil)
	restore()
	if st < 500 {
		t.Fatalf("resume with a failing start: %d %v, want an upstream error", st, out)
	}
	if got := status(); got == "superseded" {
		t.Errorf("failed resume superseded the stop op (was %v)", before)
	}
	e.ok("POST", path+"/resume", nil, 202)
	if got := status(); got != "superseded" {
		t.Errorf("successful resume left stop op %v, want superseded", got)
	}
}

// 6: limits counts a "ready" sandbox as active and lists the state as active.
func TestR26LimitsCountsReadyAsActive(t *testing.T) {
	e := setup(t)
	vm := e.nativeOf(e.newSandbox(nil))
	e.boatd.mu.Lock()
	e.boatd.vms[vm]["state"] = "ready"
	e.boatd.mu.Unlock()
	_, out, _ := e.compat(e.owner, "GET", "/api/v1/limits", nil, nil)
	if out["activeSandboxes"] != float64(1) {
		t.Errorf("ready sandbox not counted as active: %v", out)
	}
	states, _ := out["activeStates"].([]any)
	if !slices.Contains(states, any("ready")) {
		t.Errorf("activeStates lacks ready: %v", states)
	}
	e.boatd.mu.Lock()
	e.boatd.vms[vm]["state"] = "stopped"
	e.boatd.mu.Unlock()
	if _, out, _ = e.compat(e.owner, "GET", "/api/v1/limits", nil, nil); out["activeSandboxes"] != float64(0) {
		t.Errorf("stopped sandbox counted as active: %v", out)
	}
}

// 7: /v1/usage honors from and to; bad input is a 400 that never reaches boatd.
func TestR26UsageHonorsFromAndTo(t *testing.T) {
	e := setup(t)
	var calls []string
	bd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.RawQuery)
		team := r.URL.Query().Get("team")
		a, b := 100.0, 5.0 // usage since 2026-01-01
		if r.URL.Query().Get("since") == "2026-02-01T00:00:00Z" {
			a, b = 30, 2
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"since": r.URL.Query().Get("since"), "until": "now",
			"vms":       []any{map[string]any{"id": "vm1", "team": team, "size": "default", "vm_seconds": a}},
			"snapshots": []any{map[string]any{"id": "s1", "team": team, "snapshot_days": b}},
			"teams":     []any{map[string]any{"team": team, "vm_seconds": a, "vm_seconds_by_size": map[string]any{"default": a}, "snapshot_days": b}},
		})
	}))
	t.Cleanup(bd.Close)
	e.srv.boatd = newBoatd(bd.URL)

	for _, q := range []string{"from=yesterday", "since=nope", "to=nope", "from=2026-02-01T00:00:00Z&to=2026-01-01T00:00:00Z", "from=2026-01-01T00:00:00Z&to=2026-01-01T00:00:00Z"} {
		calls = nil
		if st, out := e.do(e.owner, "GET", "/v1/usage?"+q, nil); st != 400 {
			t.Errorf("usage?%s: %d %v, want 400", q, st, out)
		}
		if len(calls) != 0 {
			t.Errorf("usage?%s reached boatd: %v", q, calls)
		}
	}

	st, out := e.do(e.owner, "GET", "/v1/usage?from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z", nil)
	if st != 200 {
		t.Fatalf("windowed usage: %d %v", st, out)
	}
	vms := out["vms"].([]any)[0].(map[string]any)
	snaps := out["snapshots"].([]any)[0].(map[string]any)
	team := out["teams"].([]any)[0].(map[string]any)
	if vms["vm_seconds"] != 70.0 || snaps["snapshot_days"] != 3.0 || team["vm_seconds"] != 70.0 || team["snapshot_days"] != 3.0 ||
		team["vm_seconds_by_size"].(map[string]any)["default"] != 70.0 {
		t.Errorf("window [Jan 1, Feb 1] = %v", out)
	}
	if !strings.HasPrefix(fmt.Sprint(out["until"]), "2026-02-01T00:00:00") {
		t.Errorf("until = %v, want the requested to", out["until"])
	}

	// A `to` in the future is "until now": a single boatd call, report unchanged.
	calls = nil
	future := url.QueryEscape(time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	st, out = e.do(e.owner, "GET", "/v1/usage?from=2026-01-01T00:00:00Z&to="+future, nil)
	if st != 200 || len(calls) != 1 || out["vms"].([]any)[0].(map[string]any)["vm_seconds"] != 100.0 {
		t.Errorf("future to: %d calls=%v out=%v", st, calls, out)
	}

	// Without to, behavior is unchanged: since and from still alias.
	calls = nil
	if st, _ := e.do(e.owner, "GET", "/v1/usage?since=2026-01-01T00:00:00Z", nil); st != 200 || len(calls) != 1 {
		t.Errorf("since only: %d %v", st, calls)
	}
}

// 8: the spec documents the clone route and the exec requirement for public ports.
func TestR26OpenAPIDocumentsCloneAndPublicPortScope(t *testing.T) {
	raw, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			Summary string `json:"summary"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if _, ok := spec.Paths["/v1/vms/{id}/clone"]["post"]; !ok {
		t.Error("openapi.json lacks POST /v1/vms/{id}/clone")
	}
	sum := spec.Paths["/v1/vms/{id}/ports"]["post"].Summary
	if !strings.Contains(sum, "scope exec") {
		t.Errorf("POST /v1/vms/{id}/ports summary does not say public ports need exec: %q", sum)
	}
}

// nativeErrCode reads the code of a native {"error": {"code": ...}} answer.
func nativeErrCode(out map[string]any) string {
	m, _ := out["error"].(map[string]any)
	c, _ := m["code"].(string)
	return c
}
