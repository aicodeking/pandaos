package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// Keys may expire (boat.dev ttl); an expired key is 401.
func TestKeyTTL(t *testing.T) {
	e := setup(t)
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "t", "ttl": "1h", "preset": "read-only"}, nil)
	if st != 201 {
		t.Fatalf("create: %d %v", st, out)
	}
	ak := out["apiKey"].(map[string]any)
	if ak["expiresAt"] == nil || ak["expired"] != false {
		t.Errorf("apiKey: %v", ak)
	}
	tok := out["secret"].(string)
	if st, _ := e.do(tok, "GET", "/v1/me", nil); st != 200 {
		t.Fatalf("fresh key: %d", st)
	}
	id, _, _ := ParseToken(tok)
	e.srv.store.Update(func(s *State) error {
		past := time.Now().Add(-time.Second)
		s.Keys[id].Expires = &past
		return nil
	})
	if st, _ := e.do(tok, "GET", "/v1/me", nil); st != 401 {
		t.Errorf("expired key: %d, want 401", st)
	}
	for _, ttl := range []string{"366d", "0", "-1h", "soon"} {
		if st, out, _ := e.compat(e.owner, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "t", "ttl": ttl, "preset": "read-only"}, nil); st != 400 {
			t.Errorf("ttl %q: %d %v", ttl, st, out)
		}
	}
	for ttl, want := range map[string]int64{"never": 0, "": 0, "90d": 90 * 86400, "24h": 86400, "3600": 3600, "365d": maxKeyTTLS} {
		if got, err := parseKeyTTL(ttl); err != nil || got != want {
			t.Errorf("parseKeyTTL(%q) = %d, %v", ttl, got, err)
		}
	}
}

// linkStatus fetches a signed file URL (GET download, PUT upload) with no key.
func linkStatus(t *testing.T, e *env, method, u string) int {
	t.Helper()
	var body io.Reader
	if method == "PUT" {
		body = strings.NewReader("x")
	}
	req, _ := http.NewRequest(method, e.api.URL+u[strings.Index(u, "/api/v1/"):], body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// Links a member minted die with the member (removeMember) or the minting key
// (deleteKey): signed file URLs are refused, private port links are revoked
// (the VM's route generation is bumped, which the gateway enforces).
func TestLinksDieWithKeyAndMember(t *testing.T) {
	for _, how := range []string{"remove-member", "delete-key", "expire-key"} {
		t.Run(how, func(t *testing.T) {
			e := setup(t)
			e.srv.cfg.VMDomain = "vm.example"
			team := e.teamID()
			st, m := e.do(e.owner, "POST", "/v1/teams/"+team+"/members", map[string]any{"name": "bob", "role": "member"})
			if st != 201 {
				t.Fatalf("add member: %d %v", st, m)
			}
			bob := m["key"].(map[string]any)["token"].(string)
			bobKey, _, _ := ParseToken(bob)
			uid := m["key"].(map[string]any)["user_id"].(string)
			bx := e.newSandbox(nil)
			base := "/api/v1/sandboxes/" + bx
			e.ok("PUT", base+"/files", map[string]any{"path": "f.txt", "content": "secret"}, 200)
			mint := func(path string) string {
				st, out, _ := e.compat(bob, "POST", base+path, map[string]any{"path": "f.txt"}, nil)
				if st != 200 {
					t.Fatalf("mint %s: %d %v", path, st, out)
				}
				return out["url"].(string)
			}
			dl, ul := mint("/files/download-url"), mint("/files/upload-url")
			if linkStatus(t, e, "GET", dl) != 200 {
				t.Fatal("download link should work before revocation")
			}
			if st, _ := e.do(e.owner, "POST", "/v1/vms/"+bx+"/ports", map[string]any{"port": 8080}); st != 201 {
				t.Fatalf("add port: %d", st)
			}
			if st, out := e.do(bob, "GET", "/v1/vms/"+bx+"/ports/8080/url", nil); st != 200 || out["token"] == nil {
				t.Fatalf("port url: %d %v", st, out)
			}
			vm := e.srv.store.Snapshot().CompatIDs[bx].VM
			gen := e.srv.store.Snapshot().RouteGens[vm]
			switch how {
			case "remove-member":
				if st, out := e.do(e.owner, "DELETE", "/v1/teams/"+team+"/members/"+uid, nil); st != 200 {
					t.Fatalf("remove member: %d %v", st, out)
				}
			case "delete-key":
				if st, out := e.do(e.owner, "DELETE", "/v1/keys/"+bobKey, nil); st != 200 {
					t.Fatalf("delete key: %d %v", st, out)
				}
			case "expire-key":
				e.srv.store.Update(func(s *State) error {
					past := time.Now().Add(-time.Second)
					s.Keys[bobKey].Expires = &past
					return nil
				})
			}
			if st := linkStatus(t, e, "GET", dl); st != 404 {
				t.Errorf("download link after %s: %d, want 404", how, st)
			}
			if st := linkStatus(t, e, "PUT", ul); st != 404 {
				t.Errorf("upload link after %s: %d, want 404", how, st)
			}
			if how != "expire-key" && e.srv.store.Snapshot().RouteGens[vm] <= gen {
				t.Errorf("private port links survive %s: route generation still %d", how, gen)
			}
		})
	}
}

// A port link never outlives the key that minted it.
func TestPortLinkCappedAtKeyExpiry(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMDomain = "vm.example"
	e.srv.cfg.PortURLTTL = 24 * time.Hour
	st, out := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "short", "expires_in_s": 600})
	if st != 201 {
		t.Fatalf("key: %d %v", st, out)
	}
	tok := out["token"].(string)
	id := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+id+"/ports", map[string]any{"port": 8080})
	st, u := e.do(tok, "GET", "/v1/vms/"+id+"/ports/8080/url", nil)
	exp, _ := time.Parse(time.RFC3339, u["expires_at"].(string))
	if st != 200 || exp.After(time.Now().Add(11*time.Minute)) {
		t.Errorf("port link: %d expires %v, want within the key's 10 minutes", st, exp)
	}
}

// Idempotency-Key answers, deletion operations and stop operations survive a
// restart of boat-api (a second server on the same state directory).
func TestCompatOpsSurviveRestart(t *testing.T) {
	e := setup(t)
	idem := map[string]string{"Idempotency-Key": "restart-1"}
	st, created, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "a"}, idem)
	if st != 202 {
		t.Fatalf("create: %d %v", st, created)
	}
	bx := created["sandbox"].(map[string]any)["id"].(string)
	stop := e.ok("POST", "/api/v1/sandboxes/"+bx+"/stop", nil, 202)
	stopID := stop["sandbox"].(map[string]any)["stop"].(map[string]any)["id"].(string)
	gone := e.newSandbox(map[string]any{"name": "b"})
	_, del, _ := e.compat(e.owner, "DELETE", "/api/v1/sandboxes/"+gone, nil, map[string]string{"X-Ascii-Confirm-Delete": gone})
	opID := del["operation"].(map[string]any)["id"].(string)

	st2, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	s2 := newServer(st2, e.srv.keys, e.srv.boatd, e.srv.cfg, newLimiter(0, 0), &auditLog{w: io.Discard})
	api2 := httptest.NewServer(recorder(s2))
	defer api2.Close()
	e2 := *e
	e2.api, e2.srv = api2, s2

	st, again, hdr := e2.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "a"}, idem)
	if st != 202 || hdr.Get("Idempotent-Replayed") != "true" || again["sandbox"].(map[string]any)["id"] != bx {
		t.Errorf("idempotent replay after restart: %d %v", st, again)
	}
	if st, out, _ := e2.compat(e.owner, "GET", "/api/v1/deletion-operations/"+opID, nil, nil); st != 200 {
		t.Errorf("deletion op after restart: %d %v", st, out)
	}
	if st, out, _ := e2.compat(e.owner, "GET", "/api/v1/sandboxes/"+bx+"/stops/"+stopID, nil, nil); st != 200 || out["operation"] == nil && out["stop"] == nil && out["id"] == nil {
		t.Errorf("stop op after restart: %d %v", st, out)
	}
	if _, out, _ := e2.compat(e.owner, "GET", "/api/v1/sandboxes/"+bx, nil, nil); out["sandbox"].(map[string]any)["stop"] == nil {
		t.Errorf("Sandbox.stop forgotten after restart: %v", out)
	}
}
