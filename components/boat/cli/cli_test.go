package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

var vm1 = obj{"id": "vm1", "name": "dev", "size": "default", "state": "running", "image": "ubuntu-24.04", "team": "team_1"}

// fakeAPI serves deterministic responses and logs each request.
func fakeAPI(t *testing.T, log *bytes.Buffer) *httptest.Server {
	mux := http.NewServeMux()
	j := func(code int, v any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(v)
		}
	}
	mux.HandleFunc("GET /v1/me", j(200, obj{"user": obj{"id": "usr_1", "name": "alice"}, "team": obj{"id": "team_1", "name": "acme"}, "role": "owner", "scopes": []string{"vms:read", "vms:write", "exec", "admin"}, "key_id": "k1"}))
	mux.HandleFunc("GET /v1/vms", j(200, obj{"vms": []obj{vm1, {"id": "vm2", "name": "ci", "size": "large", "state": "stopped", "image": "ubuntu-24.04"}}}))
	mux.HandleFunc("GET /v1/vms/vm1", j(200, vm1))
	mux.HandleFunc("GET /v1/vms/{id}", j(404, obj{"error": obj{"code": "not_found", "message": "vm not found", "request_id": "req_x"}}))
	mux.HandleFunc("POST /v1/vms", j(201, obj{"id": "vm3", "name": "new", "size": "small", "state": "creating", "image": "ubuntu-24.04"}))
	mux.HandleFunc("POST /v1/vms/vm1/stop", j(200, obj{"id": "vm1", "name": "dev", "size": "default", "state": "stopped", "image": "ubuntu-24.04"}))
	mux.HandleFunc("POST /v1/vms/vm1/fork", j(201, obj{"id": "vm4", "name": "dev-2", "size": "default", "state": "forking", "image": "ubuntu-24.04"}))
	mux.HandleFunc("DELETE /v1/vms/vm1", j(200, obj{"ok": true}))
	stream := func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		enc.Encode(obj{"stream": "stdout", "data_b64": base64.StdEncoding.EncodeToString([]byte("hi\n"))})
		enc.Encode(obj{"stream": "stderr", "data_b64": base64.StdEncoding.EncodeToString([]byte("warn\n"))})
		enc.Encode(obj{"exit": 3})
	}
	mux.HandleFunc("POST /v1/vms/vm1/exec", stream)
	mux.HandleFunc("POST /v1/vms/vm1/prompt", stream)
	mux.HandleFunc("POST /v1/vms/vm1/ssh", j(200, obj{"host": "10.0.0.5", "port": 22, "user": "ubuntu", "jump": "boat@gw.example:2222"}))
	mux.HandleFunc("GET /v1/vms/vm1/desktop", j(200, obj{"url": "https://boat.example/desktop/vm1?exp=1&sig=ab", "expires_at": "2026-01-01T00:00:00Z"}))
	mux.HandleFunc("GET /v1/vms/vm1/ports", j(200, obj{"ports": []obj{{"guest_port": 3000, "host_port": 20001, "hostname": "3000-vm1"}}}))
	mux.HandleFunc("POST /v1/vms/vm1/ports", j(201, obj{"guest_port": 8080, "hostname": "8080-vm1"}))
	mux.HandleFunc("GET /v1/vms/vm1/files", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "remote-data") })
	mux.HandleFunc("PUT /v1/vms/vm1/files", j(200, obj{"ok": true, "path": "/tmp/x", "bytes": 5}))
	mux.HandleFunc("GET /v1/secrets", j(200, obj{"secrets": []obj{{"name": "ANTHROPIC_API_KEY", "updated": "2026-01-01T00:00:00Z"}}}))
	mux.HandleFunc("PUT /v1/secrets/{name}", j(200, obj{"name": "OPENAI_API_KEY", "updated": "2026-01-01T00:00:00Z"}))
	mux.HandleFunc("POST /v1/keys", j(201, obj{"id": "k2", "name": "ci", "scopes": []string{"vms:read"}, "token": "boat_x_y"}))
	mux.HandleFunc("GET /v1/teams", j(200, obj{"teams": []obj{{"id": "team_1", "name": "acme", "role": "owner"}}}))
	mux.HandleFunc("GET /v1/webhooks", j(200, obj{"webhooks": []obj{{"id": "wh_1", "url": "https://h.example", "events": []string{"vm.*"}}}}))
	mux.HandleFunc("GET /v1/usage", j(200, obj{"team": "team_1", "vm_seconds": obj{"default": 42}, "starts": 2}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		fmt.Fprintf(log, "%s %s %s\n", r.Method, r.URL.RequestURI(), bytes.TrimSpace(b))
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGolden(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519.pub"), []byte("ssh-ed25519 AAAAC3Nza me@laptop\n"), 0o600)
	local := filepath.Join(home, "up.txt")
	os.WriteFile(local, []byte("hello"), 0o600)
	cases := []struct {
		name string
		args []string
	}{
		{"list_json", []string{"list", "--json"}},
		{"list_text", []string{"list"}},
		{"new_json", []string{"new", "--name", "new", "--size", "small", "--ttl", "2h", "--json"}},
		{"new_snapshot_json", []string{"new", "--snapshot", "snap-1", "--idle-stop", "never", "--json"}},
		{"stop_by_name_json", []string{"stop", "dev", "--json"}},
		{"fork_json", []string{"fork", "vm1", "--name", "dev-2", "--json"}},
		{"rm_json", []string{"--json", "rm", "vm1"}},
		{"exec_json", []string{"exec", "--json", "vm1", "--", "ls", "--json"}},
		{"exec_text", []string{"exec", "vm1", "--", "ls"}},
		{"prompt_json", []string{"prompt", "vm1", "fix the tests", "--agent", "codex", "--json"}},
		{"ssh_json", []string{"ssh", "vm1", "--json"}},
		{"desktop_json", []string{"desktop", "vm1", "--json"}},
		{"ports_ls_json", []string{"ports", "ls", "vm1", "--json"}},
		{"ports_add_json", []string{"ports", "add", "vm1", "8080", "--json"}},
		{"scp_up_json", []string{"scp", "--json", local, "vm1:/tmp/x"}},
		{"secrets_ls_json", []string{"secrets", "ls", "--json"}},
		{"secrets_set_json", []string{"secrets", "set", "OPENAI_API_KEY", "sk-1", "--json"}},
		{"keys_create_json", []string{"keys", "create", "--name", "ci", "--scopes", "vms:read", "--json"}},
		{"teams_json", []string{"teams", "--json"}},
		{"webhooks_json", []string{"webhooks", "ls", "--json"}},
		{"usage_json", []string{"usage", "--json"}},
		{"whoami_json", []string{"whoami", "--json"}},
		{"error_json", []string{"stop", "nope", "--json"}},
		{"usage_error", []string{"bogus"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var reqs bytes.Buffer
			srv := fakeAPI(t, &reqs)
			var out, errw bytes.Buffer
			a := &app{out: &out, errw: &errw, in: strings.NewReader(""),
				getenv: func(k string) string {
					return map[string]string{"HOME": home, "BOAT_CONFIG": filepath.Join(home, "cfg.json"), "BOAT_URL": srv.URL, "BOAT_API_KEY": "boat_test"}[k]
				},
				execProg: func(argv []string) error { return fmt.Errorf("unexpected exec %v", argv) },
				open:     func(string) error { return nil }}
			code := a.run(c.args)
			got := fmt.Sprintf("$ boat %s\nexit %d\n--- stdout\n%s--- stderr\n%s--- requests\n%s",
				strings.Join(c.args, " "), code, out.String(), errw.String(), reqs.String())
			got = strings.ReplaceAll(got, home, "$HOME")
			got = regexp.MustCompile(`(?s)--- stderr\nboat - persistent.*?\n--- requests`).ReplaceAllString(got, "--- stderr\n<usage>\n--- requests")
			path := filepath.Join("testdata", c.name+".golden")
			if *update {
				os.WriteFile(path, []byte(got), 0o644)
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run go test -update)", err)
			}
			if got != string(want) {
				t.Errorf("mismatch for %s\n--- got\n%s\n--- want\n%s", c.name, got, want)
			}
		})
	}
}

// Every --json stdout line must be a standalone JSON object (JSONL contract).
func TestJSONLContract(t *testing.T) {
	files, _ := filepath.Glob("testdata/*_json.golden")
	if len(files) == 0 {
		t.Fatal("no golden files")
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		stdout := strings.SplitN(strings.SplitN(string(b), "--- stdout\n", 2)[1], "--- stderr\n", 2)[0]
		for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
			if line == "" {
				continue
			}
			var v map[string]any
			if err := json.Unmarshal([]byte(line), &v); err != nil {
				t.Errorf("%s: not a JSON object line: %q", f, line)
			}
		}
	}
}

func TestLoginSavesConfig(t *testing.T) {
	var reqs bytes.Buffer
	srv := fakeAPI(t, &reqs)
	cfg := filepath.Join(t.TempDir(), "boat", "config.json")
	var out bytes.Buffer
	env := map[string]string{"BOAT_CONFIG": cfg}
	a := &app{out: &out, errw: io.Discard, in: strings.NewReader(""), getenv: func(k string) string { return env[k] }}
	if code := a.run([]string{"login", "--url", srv.URL + "/", "--key", "boat_k", "--json"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	st, err := os.Stat(cfg)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config perms: %v %v", st, err)
	}
	c, _ := loadConfig(func(k string) string { return env[k] })
	if c.URL != srv.URL || c.Key != "boat_k" {
		t.Errorf("config %+v", c)
	}
}

func TestSSHExecArgv(t *testing.T) {
	var reqs bytes.Buffer
	srv := fakeAPI(t, &reqs)
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519.pub"), []byte("ssh-ed25519 AAAA k\n"), 0o600)
	var argv []string
	a := &app{out: io.Discard, errw: io.Discard, getenv: func(k string) string {
		return map[string]string{"HOME": home, "BOAT_CONFIG": "/nonexistent", "BOAT_URL": srv.URL, "BOAT_API_KEY": "k"}[k]
	}, execProg: func(v []string) error { argv = v; return nil }}
	if code := a.run([]string{"ssh", "dev", "--", "uptime"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := "ssh -p 22 -o StrictHostKeyChecking=accept-new -o ProxyCommand=ssh -W %h:%p -p 2222 boat@gw.example ubuntu@10.0.0.5 uptime"
	if strings.Join(argv, " ") != want {
		t.Errorf("argv %q", strings.Join(argv, " "))
	}
}
