package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// acctAPI records each request body by "METHOD path".
func acctAPI(t *testing.T) (*httptest.Server, map[string]string) {
	got := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got[r.Method+" "+r.URL.Path] = string(b)
		if r.Header.Get("Authorization") != "Bearer boat_k" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"code":"unauthorized","message":"bad key"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/me":
			io.WriteString(w, `{"team":{"id":"team_1","name":"acme"},"role":"owner"}`)
		case "GET /v1/vms/vm1", "PATCH /v1/vms/vm1":
			io.WriteString(w, `{"id":"vm1","name":"api worker","state":"running"}`)
		case "POST /api/v1/api-keys/scoped":
			w.WriteHeader(201)
			io.WriteString(w, `{"ok":true,"type":"api_key.created","apiKey":{"id":"k2","name":"ci"},"secret":"boat_k2_s"}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"code":"not_found","message":"no"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func acctApp(env map[string]string, in string) (*app, *bytes.Buffer, *bytes.Buffer) {
	var out, errw bytes.Buffer
	return &app{out: &out, errw: &errw, in: strings.NewReader(in), getenv: func(k string) string { return env[k] }}, &out, &errw
}

// Council round 3: the owner pasted boat-api admin init's whole JSON as the
// key and got a 401. login takes the bare token or that JSON, as --key, as
// boat.dev's positional [key], or with --key-stdin.
func TestLoginAcceptsTokenOrInitJSON(t *testing.T) {
	srv, _ := acctAPI(t)
	initJSON := `{"id":"k","user_id":"usr_1","team_id":"team_1","name":"initial","scopes":[],"created":"2026-01-01T00:00:00Z","token":"boat_k"}`
	for name, c := range map[string]struct {
		args []string
		in   string
	}{
		"json flag":  {[]string{"login", "--url", srv.URL, "--key", initJSON}, ""},
		"positional": {[]string{"login", "--url", srv.URL, "boat_k"}, ""},
		"stdin json": {[]string{"login", "--url", srv.URL, "--key-stdin"}, initJSON + "\n"},
		"spaces":     {[]string{"login", "--url", srv.URL, "--key", "  boat_k\n"}, ""},
	} {
		env := map[string]string{"BOAT_CONFIG": filepath.Join(t.TempDir(), "c.json")}
		a, _, errw := acctApp(env, c.in)
		if code := a.run(c.args); code != 0 {
			t.Errorf("%s: exit %d %s", name, code, errw)
			continue
		}
		if cfg, _ := loadConfig(a.getenv); cfg.Key != "boat_k" {
			t.Errorf("%s: saved key %q", name, cfg.Key)
		}
	}
	env := map[string]string{"BOAT_CONFIG": filepath.Join(t.TempDir(), "c.json")}
	a, _, _ := acctApp(env, "")
	if code := a.run([]string{"login", "--url", srv.URL, "--key", "boat_k", "boat_k"}); code != 2 {
		t.Errorf("--key and a positional key: exit %d", code)
	}
}

func TestLogoutClearsToken(t *testing.T) {
	env := map[string]string{"BOAT_CONFIG": filepath.Join(t.TempDir(), "c.json")}
	saveConfig(func(k string) string { return env[k] }, Config{URL: "https://api.example", Key: "boat_k"})
	a, out, _ := acctApp(env, "")
	if code := a.run([]string{"logout"}); code != 0 {
		t.Fatalf("logout: %d", code)
	}
	if cfg, _ := loadConfig(a.getenv); cfg.Key != "" || cfg.URL != "https://api.example" {
		t.Errorf("after logout: %+v", cfg)
	}
	if !strings.Contains(out.String(), "Logged out") {
		t.Errorf("output: %s", out)
	}
	if code := a.run([]string{"logout"}); code != 0 { // twice is fine
		t.Errorf("second logout: %d", code)
	}
}

func TestRenameAndAPIKeyPreset(t *testing.T) {
	srv, got := acctAPI(t)
	env := map[string]string{"BOAT_URL": srv.URL, "BOAT_API_KEY": "boat_k", "BOAT_CONFIG": filepath.Join(t.TempDir(), "c.json")}
	a, out, errw := acctApp(env, "")
	if code := a.run([]string{"rename", "vm1", "api worker", "--json"}); code != 0 {
		t.Fatalf("rename: %d %s", code, errw)
	}
	var body map[string]any
	json.Unmarshal([]byte(got["PATCH /v1/vms/vm1"]), &body)
	if body["name"] != "api worker" || len(body) != 1 {
		t.Errorf("rename body: %v", got["PATCH /v1/vms/vm1"])
	}
	if code := a.run([]string{"rename", "vm1"}); code != 2 {
		t.Errorf("rename without a name: %d", code)
	}
	out.Reset()
	if code := a.run([]string{"api-key", "create", "ci", "--preset", "ci", "--ttl", "never"}); code != 0 {
		t.Fatalf("api-key create: %d %s", code, errw)
	}
	body = nil
	json.Unmarshal([]byte(got["POST /api/v1/api-keys/scoped"]), &body)
	if body["name"] != "ci" || body["preset"] != "ci" || body["ttl"] != "never" {
		t.Errorf("api-key body: %v", got["POST /api/v1/api-keys/scoped"])
	}
	if !strings.Contains(out.String(), "boat_k2_s") {
		t.Errorf("secret not printed: %s", out)
	}
	if code := a.run([]string{"api-key", "create", "x", "--preset", "ci", "--actions", "exec"}); code != 2 {
		t.Errorf("--preset with --actions: %d", code)
	}
}

func TestCompletionsAndSelfUpdate(t *testing.T) {
	for _, sh := range []string{"bash", "zsh", "fish", "powershell"} {
		a, out, _ := acctApp(map[string]string{}, "")
		if code := a.run([]string{"completions", sh}); code != 0 || !strings.Contains(out.String(), "rename") {
			t.Errorf("%s: %d %s", sh, code, out)
		}
	}
	a, _, _ := acctApp(map[string]string{}, "")
	if code := a.run([]string{"completions", "tcsh"}); code != 2 {
		t.Errorf("unknown shell: %d", code)
	}
	// self-update is upgrade: same release lookup (here: none, so it fails the same way).
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	env := map[string]string{"BOAT_RELEASE_BASE": srv.URL}
	a1, _, e1 := acctApp(env, "")
	a2, _, e2 := acctApp(env, "")
	self := filepath.Join(t.TempDir(), "boat")
	a1.exe = func() (string, error) { return self, nil }
	a2.exe = a1.exe
	c1, c2 := a1.run([]string{"upgrade"}), a2.run([]string{"self-update"})
	if c1 != c2 || c1 == 0 || e1.String() != e2.String() {
		t.Errorf("self-update differs from upgrade: %d %q vs %d %q", c2, e2, c1, e1)
	}
}
