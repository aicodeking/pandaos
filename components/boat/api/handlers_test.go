package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type env struct {
	t     *testing.T
	api   *httptest.Server
	srv   *server
	boatd *fakeBoatd
	owner string // owner token
	dir   string
	audit *bytes.Buffer
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() }) // before TempDir's removal: no late background write
	kr, err := NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	k, err := bootstrap(store, kr, "alice", "acme")
	if err != nil {
		t.Fatal(err)
	}
	fb, bsrv := newFakeBoatd(t)
	audit := &bytes.Buffer{}
	cfg := Config{Images: []string{"ubuntu-24.04"}, DesktopURL: "https://boat.example/desktop",
		DesktopKey: []byte("desktop-key"), DesktopTTL: time.Minute, AllowPrivateHook: true}
	s := newServer(store, kr, newBoatd(bsrv.URL), cfg, newLimiter(0, 0), &auditLog{w: audit})
	s.hooks.backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	api := httptest.NewServer(recorder(s))
	t.Cleanup(api.Close)
	return &env{t: t, api: api, srv: s, boatd: fb, owner: k.Token, dir: dir, audit: audit}
}

func (e *env) do(token, method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	status, b := e.raw(token, method, path, body)
	var out map[string]any
	if len(b) > 0 && b[0] == '{' {
		if err := json.Unmarshal(b, &out); err != nil {
			e.t.Fatalf("%s %s: bad json %q", method, path, b)
		}
	}
	return status, out
}

func (e *env) raw(token, method, path string, body any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.api.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (e *env) newKey(scopes ...string) string {
	body := map[string]any{"name": "k"}
	if len(scopes) > 0 {
		body["scopes"] = scopes
	}
	st, out := e.do(e.owner, "POST", "/v1/keys", body)
	if st != 201 {
		e.t.Fatalf("create key: %d %v", st, out)
	}
	return out["token"].(string)
}

func (e *env) createVM() string {
	st, vm := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "dev", "size": "small"})
	if st != 201 {
		e.t.Fatalf("create vm: %d %v", st, vm)
	}
	return vm["id"].(string)
}

func TestAuth(t *testing.T) {
	e := setup(t)
	id, secret, _ := ParseToken(e.owner)
	for _, tok := range []string{"", "nope", "boat_" + id + "_" + strings.Repeat("0", 64), "boat_" + strings.Repeat("a", 16) + "_" + secret} {
		if st, _ := e.do(tok, "GET", "/v1/me", nil); st != 401 {
			t.Errorf("token %q: got %d, want 401", tok, st)
		}
	}
	st, me := e.do(e.owner, "GET", "/v1/me", nil)
	if st != 200 || me["role"] != "owner" {
		t.Fatalf("me: %d %v", st, me)
	}
	if !strings.Contains(e.audit.String(), `"status":401`) {
		t.Error("401s should be audited")
	}
	raw, _ := os.ReadFile(filepath.Join(e.dir, "state.json"))
	if bytes.Contains(raw, []byte(secret)) {
		t.Error("state.json contains the raw key secret")
	}
}

func TestParseToken(t *testing.T) {
	_, _, tok := NewToken()
	if _, _, ok := ParseToken(tok); !ok {
		t.Fatal("fresh token should parse")
	}
	for _, bad := range []string{"boat_", "boat_abc_def", strings.Replace(tok, "boat_", "boot_", 1), tok + "x", strings.ToUpper(tok[:6]) + tok[6:]} {
		if _, _, ok := ParseToken(bad); ok {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestScopes(t *testing.T) {
	e := setup(t)
	ro := e.newKey("vms:read")
	if st, _ := e.do(ro, "GET", "/v1/vms", nil); st != 200 {
		t.Errorf("read key list: %d", st)
	}
	if st, _ := e.do(ro, "POST", "/v1/vms", map[string]any{}); st != 403 {
		t.Errorf("read key create: %d, want 403", st)
	}
	if st, _ := e.do(ro, "POST", "/v1/keys", map[string]any{"scopes": []string{"admin"}}); st != 403 {
		t.Errorf("escalation via key mint: %d, want 403", st)
	}
	if st, out := e.do(ro, "POST", "/v1/keys", map[string]any{}); st != 201 || len(out["scopes"].([]any)) != 1 {
		t.Errorf("default scopes should inherit the minting key's: %d %v", st, out)
	}
	if st, _ := e.do(ro, "PUT", "/v1/secrets/X", map[string]any{"value": "v"}); st != 403 {
		t.Errorf("read key set secret: %d", st)
	}
	// a member cannot hold admin even if requested at invite time
	st, out := e.do(e.owner, "POST", "/v1/teams/"+e.teamID()+"/members", map[string]any{"name": "bob", "role": "member"})
	if st != 201 {
		t.Fatalf("add member: %d %v", st, out)
	}
	bob := out["key"].(map[string]any)["token"].(string)
	if st, _ := e.do(bob, "GET", "/v1/webhooks", nil); st != 403 {
		t.Errorf("member webhooks: %d, want 403", st)
	}
	if st, _ := e.do(bob, "POST", "/v1/vms", map[string]any{}); st != 201 {
		t.Errorf("member create vm: %d", st)
	}
	if !slices.Equal(effectiveScopes([]string{"admin", "exec"}, "member"), []string{"exec"}) {
		t.Error("role must cap key scopes")
	}
}

func (e *env) teamID() string {
	_, me := e.do(e.owner, "GET", "/v1/me", nil)
	return me["team"].(map[string]any)["id"].(string)
}

func TestRateLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLimiter(1, 2)
	l.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if ok, _ := l.allow("k"); !ok {
			t.Fatal("burst should allow")
		}
	}
	if ok, wait := l.allow("k"); ok || wait <= 0 {
		t.Fatal("third request should be limited")
	}
	if ok, _ := l.allow("other"); !ok {
		t.Fatal("limits are per key")
	}
	now = now.Add(time.Second)
	if ok, _ := l.allow("k"); !ok {
		t.Fatal("refill after 1s")
	}
}

func TestRateLimitHTTP(t *testing.T) {
	e := setup(t)
	e.srv.limiter = newLimiter(0.001, 1)
	e.do(e.owner, "GET", "/v1/me", nil)
	if st, _ := e.do(e.owner, "GET", "/v1/me", nil); st != 429 {
		t.Fatalf("got %d, want 429", st)
	}
}

func TestSecretsEncryption(t *testing.T) {
	kr, _ := NewKeyring(bytes.Repeat([]byte{1}, 32))
	sealed, err := kr.Seal([]byte("hunter2"), "t/A")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := kr.Open(sealed, "t/A"); err != nil || string(v) != "hunter2" {
		t.Fatalf("roundtrip: %q %v", v, err)
	}
	if _, err := kr.Open(sealed, "t/B"); err == nil {
		t.Error("ciphertext must be bound to team/name")
	}
	other, _ := NewKeyring(bytes.Repeat([]byte{2}, 32))
	if _, err := other.Open(sealed, "t/A"); err == nil {
		t.Error("wrong master key must fail")
	}
}

func TestSecretsAPIAndInjection(t *testing.T) {
	e := setup(t)
	if st, _ := e.do(e.owner, "PUT", "/v1/secrets/ANTHROPIC_API_KEY", map[string]any{"value": "sk-secret-1"}); st != 200 {
		t.Fatalf("put: %d", st)
	}
	if st, _ := e.do(e.owner, "PUT", "/v1/secrets/bad-name", map[string]any{"value": "x"}); st != 400 {
		t.Errorf("bad name: %d", st)
	}
	_, list := e.raw(e.owner, "GET", "/v1/secrets", nil)
	if bytes.Contains(list, []byte("sk-secret-1")) || !bytes.Contains(list, []byte("ANTHROPIC_API_KEY")) {
		t.Errorf("list must show names only: %s", list)
	}
	raw, _ := os.ReadFile(filepath.Join(e.dir, "state.json"))
	if bytes.Contains(raw, []byte("sk-secret-1")) {
		t.Error("secret stored in plaintext")
	}
	st, vm := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "dev"})
	if st != 201 {
		t.Fatalf("create: %d %v", st, vm)
	}
	if _, leaked := vm["env"]; leaked {
		t.Error("vm response leaks env")
	}
	id := vm["id"].(string)
	if got := e.boatd.vms[id]["env"].(map[string]any)["ANTHROPIC_API_KEY"]; got != "sk-secret-1" {
		t.Errorf("secret not injected at create: %v", got)
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil)
	if got := e.boatd.starts[0]["env"].(map[string]any)["ANTHROPIC_API_KEY"]; got != "sk-secret-1" {
		t.Errorf("secret not injected at start: %v", got)
	}
	if st, _ := e.do(e.owner, "DELETE", "/v1/secrets/ANTHROPIC_API_KEY", nil); st != 200 {
		t.Errorf("delete: %d", st)
	}
}

func TestVMLifecycleAndIsolation(t *testing.T) {
	e := setup(t)
	if st, _ := e.do(e.owner, "POST", "/v1/vms", map[string]any{"size": "huge"}); st != 400 {
		t.Errorf("bad size: %d", st)
	}
	if st, _ := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "Bad Name"}); st != 400 {
		t.Errorf("bad name: %d", st)
	}
	id := e.createVM()
	st, list := e.do(e.owner, "GET", "/v1/vms", nil)
	if st != 200 || len(list["vms"].([]any)) != 1 {
		t.Fatalf("list: %d %v", st, list)
	}
	if st, vm := e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil); st != 200 || vm["state"] != "stopped" {
		t.Errorf("stop: %d %v", st, vm)
	}
	st, fork := e.do(e.owner, "POST", "/v1/vms/"+id+"/fork", map[string]any{"name": "dev-2"})
	if st != 201 || fork["name"] != "dev-2" {
		t.Errorf("fork: %d %v", st, fork)
	}

	// another team's owner cannot see or touch the VM
	_, out := e.do(e.owner, "POST", "/v1/teams", map[string]any{"name": "other"})
	otherTeam := out["id"].(string)
	_, k := e.do(e.owner, "POST", "/v1/keys", map[string]any{"team_id": otherTeam})
	other := k["token"].(string)
	for _, p := range []string{"/v1/vms/" + id, "/v1/vms/" + id + "/ports"} {
		if st, _ := e.do(other, "GET", p, nil); st != 404 {
			t.Errorf("cross-team GET %s: %d", p, st)
		}
	}
	if st, _ := e.do(other, "POST", "/v1/vms/"+id+"/exec", map[string]any{"cmd": []string{"id"}}); st != 404 {
		t.Errorf("cross-team exec: %d", st)
	}
	if _, l := e.do(other, "GET", "/v1/vms", nil); len(l["vms"].([]any)) != 0 {
		t.Error("cross-team list leaked")
	}
	if st, _ := e.do(e.owner, "DELETE", "/v1/vms/"+id, nil); st != 200 {
		t.Errorf("delete: %d", st)
	}
	if st, _ := e.do(e.owner, "GET", "/v1/vms/"+id, nil); st != 404 {
		t.Errorf("deleted vm: %d", st)
	}
}

func TestExecAndPrompt(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/exec", map[string]any{}); st != 400 {
		t.Errorf("empty cmd: %d", st)
	}
	st, b := e.raw(e.owner, "POST", "/v1/vms/"+id+"/exec", map[string]any{"cmd": []string{"echo", "hello"}})
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if st != 200 || len(lines) != 2 || !strings.Contains(lines[1], `"exit":0`) {
		t.Fatalf("exec stream: %d %q", st, b)
	}
	e.do(e.owner, "PUT", "/v1/secrets/OPENAI_API_KEY", map[string]any{"value": "sk-o"})
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "hi", "agent": "gpt"}); st != 400 {
		t.Errorf("unknown agent: %d", st)
	}
	st, _ = e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "fix the bug", "agent": "codex"})
	if st != 200 {
		t.Fatalf("prompt: %d", st)
	}
	last := e.boatd.execs[len(e.boatd.execs)-1]
	cmd := last["cmd"].([]any)
	if cmd[0] != "codex" || cmd[len(cmd)-1] != "fix the bug" || cmd[len(cmd)-2] != "--" {
		t.Errorf("prompt cmd: %v", cmd)
	}
	if last["env"].(map[string]any)["OPENAI_API_KEY"] != "sk-o" {
		t.Error("prompt must receive team secrets")
	}
}

func TestFilesSSHDesktop(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	req, _ := http.NewRequest("PUT", e.api.URL+"/v1/vms/"+id+"/files?path=/tmp/a.txt", strings.NewReader("data"))
	req.Header.Set("Authorization", "Bearer "+e.owner)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("put file: %d", resp.StatusCode)
	}
	if st, b := e.raw(e.owner, "GET", "/v1/vms/"+id+"/files?path=/tmp/a.txt", nil); st != 200 || string(b) != "data" {
		t.Errorf("get file: %d %q", st, b)
	}
	if st, _ := e.raw(e.owner, "GET", "/v1/vms/"+id+"/files?path=rel", nil); st != 400 {
		t.Errorf("relative path: %d", st)
	}

	pub := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl me@laptop"
	for i := 0; i < 2; i++ {
		st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/ssh", map[string]any{"public_key": pub})
		if st != 200 || out["host"] != "10.0.0.5" || out["user"] != "dev" {
			t.Fatalf("ssh: %d %v", st, out)
		}
	}
	ak := string(e.boatd.files[id+":/home/dev/.ssh/authorized_keys"])
	if strings.Count(ak, "ssh-ed25519") != 1 {
		t.Errorf("authorized_keys should hold the key once: %q", ak)
	}
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/ssh", map[string]any{"public_key": "ssh-ed25519 AAAA\ncommand=evil"}); st != 400 {
		t.Errorf("multi-line key: %d", st)
	}

	st, d := e.do(e.owner, "GET", "/v1/vms/"+id+"/desktop", nil)
	if st != 200 || !strings.HasPrefix(d["url"].(string), "https://boat.example/desktop/"+id+"?exp=") {
		t.Fatalf("desktop: %d %v", st, d)
	}
	if len(e.boatd.desk) != 1 || e.boatd.desk[0]["action"] != "start" || e.boatd.desk[0]["token"] != SignGuestDesktop([]byte("desktop-key"), id) || len(SignGuestDesktop([]byte("k"), id)) != 64 {
		t.Errorf("guest desktop not started with token: %v", e.boatd.desk)
	}
	if SignDesktop([]byte("k"), "vm1", 100) == SignDesktop([]byte("k"), "vm1", 101) {
		t.Error("signature must cover exp")
	}
}

func TestPortsSnapshotsUsageImages(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/ports", map[string]any{"port": 70000}); st != 400 {
		t.Errorf("bad port: %d", st)
	}
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/ports", map[string]any{"port": 3000}); st != 201 {
		t.Errorf("add port: %d", st)
	}
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/snapshots", map[string]any{"name": "s"}); st != 201 {
		t.Errorf("snapshot: %d", st)
	}
	st, u := e.do(e.owner, "GET", "/v1/usage?from=2026-01-01T00:00:00Z", nil)
	teams, _ := u["teams"].([]any)
	if st != 200 || len(teams) != 1 || teams[0].(map[string]any)["team"] != e.teamID() || u["since"] != "2026-01-01T00:00:00Z" {
		t.Errorf("usage must be team-scoped and pass since: %d %v", st, u)
	}
	if st, im := e.do(e.owner, "GET", "/v1/images", nil); st != 200 || len(im["images"].([]any)) != 1 {
		t.Errorf("images: %d %v", st, im)
	}
}

func TestKeysAndTeams(t *testing.T) {
	e := setup(t)
	k := e.newKey("vms:read")
	_, list := e.do(e.owner, "GET", "/v1/keys", nil)
	if n := len(list["keys"].([]any)); n != 2 {
		t.Errorf("keys: %d", n)
	}
	if bytes.Contains(must(e.raw(e.owner, "GET", "/v1/keys", nil)), []byte(`"hash"`)) {
		t.Error("key list leaks hashes")
	}
	id, _, _ := ParseToken(k)
	if st, _ := e.do(e.owner, "DELETE", "/v1/keys/"+id, nil); st != 200 {
		t.Errorf("delete key: %d", st)
	}
	if st, _ := e.do(k, "GET", "/v1/me", nil); st != 401 {
		t.Errorf("revoked key: %d", st)
	}
	tid := e.teamID()
	_, me := e.do(e.owner, "GET", "/v1/me", nil)
	uid := me["user"].(map[string]any)["id"].(string)
	if st, _ := e.do(e.owner, "DELETE", "/v1/teams/"+tid+"/members/"+uid, nil); st != 409 {
		t.Errorf("removing last owner: %d", st)
	}
}

func must(_ int, b []byte) []byte { return b }

// TestOpenAPIMatchesRoutes diffs the registered route table against
// openapi.json in both directions, so either side drifting fails CI.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	var spec struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(openapiSpec, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.OpenAPI != "3.1.0" {
		t.Errorf("openapi version %q", spec.OpenAPI)
	}
	e := setup(t)
	registered := map[string]bool{}
	for _, pat := range e.srv.routeTable {
		registered[pat] = true
	}
	documented := map[string]bool{}
	for path, ops := range spec.Paths {
		for m := range ops {
			if m == "parameters" {
				continue
			}
			documented[strings.ToUpper(m)+" "+path] = true
		}
	}
	for pat := range registered {
		if !documented[pat] {
			t.Errorf("route %q is registered but missing from openapi.json", pat)
		}
	}
	for pat := range documented {
		if !registered[pat] {
			t.Errorf("openapi.json documents %q but no such route is registered", pat)
		}
	}
	if len(registered) < 40 {
		t.Errorf("route table looks incomplete: %d routes", len(registered))
	}
	if st, _ := e.raw("", "GET", "/v1/openapi.json", nil); st != 200 {
		t.Errorf("spec endpoint: %d", st)
	}
}

func TestRequestID(t *testing.T) {
	e := setup(t)
	resp, _ := http.Get(e.api.URL + "/healthz")
	resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("X-Request-Id"), "req_") {
		t.Error("missing request id")
	}
	e.do(e.owner, "POST", "/v1/vms", map[string]any{})
	var entry auditEntry
	lines := strings.Split(strings.TrimSpace(e.audit.String()), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil || entry.Method != "POST" || entry.KeyID == "" || entry.RequestID == "" {
		t.Errorf("audit entry: %+v %v", entry, err)
	}
}

func TestJumpKeys(t *testing.T) {
	e := setup(t)
	jf := filepath.Join(t.TempDir(), "jump_authorized_keys")
	e.srv.cfg.JumpKeysFile = jf
	id := e.createVM()
	id2 := e.createVM()
	pub := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl me@laptop"
	for _, v := range []string{id, id2, id} {
		if st, out := e.do(e.owner, "POST", "/v1/vms/"+v+"/ssh", map[string]any{"public_key": pub}); st != 200 {
			t.Fatalf("ssh: %d %v", st, out)
		}
	}
	e.boatd.mu.Lock()
	e.boatd.vms[id2]["ip"] = "10.200.0.9"
	e.boatd.mu.Unlock()
	e.srv.syncJumpKeys(context.Background())
	want := `restrict,port-forwarding,permitopen="10.0.0.5:22",permitopen="10.200.0.9:22" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl` + "\n"
	if b, _ := os.ReadFile(jf); string(b) != want {
		t.Fatalf("jump keys:\n%s\nwant\n%s", b, want)
	}
	e.do(e.owner, "DELETE", "/v1/vms/"+id2, nil)
	if b, _ := os.ReadFile(jf); strings.Contains(string(b), "10.200.0.9") {
		t.Errorf("deleted VM still permitted: %s", b)
	}
	// a VM that moved to another team (ip reuse) is dropped
	e.boatd.mu.Lock()
	e.boatd.vms[id]["team"] = "someone-else"
	e.boatd.mu.Unlock()
	e.srv.syncJumpKeys(context.Background())
	if b, _ := os.ReadFile(jf); len(b) != 0 {
		t.Errorf("foreign VM still permitted: %s", b)
	}
	if n := len(e.srv.store.Snapshot().SSHGrants); n != 0 {
		t.Errorf("stale grants not pruned: %d", n)
	}
}
