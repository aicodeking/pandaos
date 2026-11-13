package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// refAPI answers what boat.dev's CLI reference invocations touch, and records
// "METHOD /path?query body" for each request.
type refAPI struct {
	mu   sync.Mutex
	reqs []string
	tgz  []byte // served for a pack temp file (scp -r down)
}

func (api *refAPI) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	line := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		line += "?" + r.URL.RawQuery
	}
	if len(b) > 0 && len(b) < 4096 {
		line += " " + string(b)
	}
	api.mu.Lock()
	api.reqs = append(api.reqs, line)
	api.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	switch {
	case r.Method == "POST" && (strings.HasSuffix(path, "/exec") || strings.HasSuffix(path, "/prompt")):
		home := base64.StdEncoding.EncodeToString([]byte("/home/dev"))
		io.WriteString(w, `{"stream":"stdout","data_b64":"`+home+`"}`+"\n"+`{"exit":0}`+"\n")
	case r.Method == "GET" && path == "/v1/vms/vm1/files" && strings.Contains(r.URL.RawQuery, "boat-scp"):
		w.Write(api.tgz)
	case r.Method == "GET" && path == "/v1/vms/vm1/prompts":
		io.WriteString(w, `{"prompts":[{"id":"p1","state":"running","conversation_id":"conv_0123456789abcdef"}]}`)
	case r.Method == "POST" && strings.HasSuffix(path, "/fork"):
		io.WriteString(w, `{"id":"vm2","name":"fork","state":"running"}`)
	case r.Method == "GET" && strings.HasPrefix(path, "/v1/vms/") && strings.Count(path, "/") == 3:
		io.WriteString(w, `{"id":"vm1","name":"dev","state":"running"}`)
	case strings.Contains(path, "/accounts/"):
		io.WriteString(w, `{"accounts":[{"id":"anthropic-api-key","label":"…abcd","active":true}]}`)
	default:
		io.WriteString(w, `{"ok":true,"id":"vm1","name":"dev","state":"running","url":"https://8080-vm1.vm.example/","operation":{"id":"stop_1","status":"completed"}}`)
	}
}

func (api *refAPI) saw(want string) bool {
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, r := range api.reqs {
		if strings.Contains(r, want) {
			return true
		}
	}
	return false
}

func refTgz(t *testing.T) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{{"out/", ""}, {"out/a.txt", "A"}, {"out/sub/b.txt", "B"}} {
		h := &tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(f.name, "/") {
			h.Typeflag, h.Mode = tar.TypeDir, 0o755
		}
		tw.WriteHeader(h)
		tw.Write([]byte(f.body))
	}
	tw.WriteHeader(&tar.Header{Name: "out/../../evil", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// TestBoatDevReference runs boat.dev's CLI reference invocations
// (docs.boat.dev/cli-reference.md) against a stub: each either makes the
// native call it maps to, or fails with a reason (never "flag not defined").
func TestBoatDevReference(t *testing.T) {
	api := &refAPI{tgz: refTgz(t)}
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519.pub"), []byte("ssh-ed25519 AAAA test"), 0o644)
	local := filepath.Join(tmp, "session.env")
	os.WriteFile(local, []byte("K=V"), 0o644)
	setup := filepath.Join(tmp, "setup.sh")
	os.WriteFile(setup, []byte("echo hi"), 0o644)
	dir := filepath.Join(tmp, "my-project")
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "f.txt"), []byte("f"), 0o644)
	down := filepath.Join(tmp, "down")
	os.MkdirAll(down, 0o755)

	cases := []struct {
		args []string
		code int
		want string // a request (substring) the command must make, or "!text" in its output
	}{
		{[]string{"new", "--no-auto-stop"}, 0, `POST /v1/vms {"image":"","name":"","size":"","ttl_s":0}`},
		{[]string{"new", "--ttl", "600"}, 0, `"ttl_s":600`},
		{[]string{"new", "--ttl", "600", "--no-auto-stop"}, 2, "!conflict"},
		{[]string{"new", "--env", "KEY=VALUE", "--env", "OTHER=V"}, 0, `"env":{"KEY":"VALUE","OTHER":"V"}`},
		{[]string{"new", "--environment", "users"}, 0, `"environment":"users"`},
		{[]string{"new", "--no-snapshots"}, 1, "!--no-snapshots is not supported"},
		{[]string{"new", "--no-monitoring"}, 1, "!not supported"},
		{[]string{"new", "--personal", "--fail-fast"}, 0, `"fail_fast":true`},
		{[]string{"new", "--visibility", "private"}, 1, "!--visibility private is not supported"},
		{[]string{"new", "--host", "3000", "--host", "8080:public"}, 0, `POST /v1/vms/vm1/ports {"auth":"public","port":8080}`},
		{[]string{"new", "--file", "workspace/.env=" + local}, 0, "PUT /v1/vms/vm1/files?path=%2Fhome%2Fdev%2Fworkspace%2F.env"},
		{[]string{"new", "--setup-file", setup}, 0, `POST /v1/vms/vm1/commands {"cmd":["bash","-lc","echo hi"]}`},
		{[]string{"new", "--name", "api worker"}, 0, `"name":"api worker"`},
		{[]string{"new", "extra", "args"}, 2, "!unexpected arguments"},
		{[]string{"fork", "--ttl", "600", "vm1"}, 0, `PATCH /v1/vms/vm2 {"ttl_s":600}`},
		{[]string{"fork", "--no-auto-stop", "vm1"}, 0, `PATCH /v1/vms/vm2 {"ttl_s":0}`},
		{[]string{"fork", "vm1"}, 0, "POST /v1/vms/vm1/fork"},
		{[]string{"fork", "-e", "TENANT_ID=acme", "vm1"}, 0, `"env":{"TENANT_ID":"acme"}`},
		{[]string{"fork", "--fail-fast", "vm1"}, 0, `"fail_fast":true`},
		{[]string{"fork", "--type", "small", "vm1"}, 1, "!--type is not supported"},
		{[]string{"fork", "--no-env", "vm1"}, 1, "!--no-env is not supported"},
		{[]string{"resume", "--ttl", "7200", "vm1"}, 0, `PATCH /v1/vms/vm1 {"ttl_s":7200}`},
		{[]string{"resume", "--no-auto-stop", "vm1"}, 0, `PATCH /v1/vms/vm1 {"ttl_s":0}`},
		{[]string{"resume", "-e", "KEY=VALUE", "vm1"}, 0, `POST /v1/vms/vm1/start {"env":{"KEY":"VALUE"}}`},
		{[]string{"resume", "--no-wait", "vm1"}, 0, "POST /v1/vms/vm1/start"},
		{[]string{"resume", "--no-env", "vm1"}, 1, "!--no-env is not supported"},
		{[]string{"extend", "vm1", "--no-auto-stop"}, 0, `PATCH /v1/vms/vm1 {"ttl_s":0}`},
		{[]string{"stop", "vm1", "--status", "stop_1"}, 0, "GET /api/v1/sandboxes/vm1/stops/stop_1"},
		{[]string{"stop", "vm1", "--yes"}, 0, "POST /v1/vms/vm1/stop"},
		{[]string{"exec", "vm1", "--status", "p1"}, 0, "GET /v1/vms/vm1/commands/p1"},
		{[]string{"exec", "vm1", "--kill", "p1"}, 0, "DELETE /v1/vms/vm1/commands/p1"},
		{[]string{"exec", "vm1", "--stream", "--timeout", "600", "npm test"}, 0, `"cmd":["bash","-lc","npm test"]`},
		{[]string{"history", "vm1", "--mode", "commands"}, 0, "GET /v1/vms/vm1/command-history"},
		{[]string{"history", "vm1", "c1", "--stderr"}, 1, "!--stderr is not supported"},
		{[]string{"host", "vm1", "3000", "--title", "Login preview"}, 0, `POST /v1/vms/vm1/ports {"auth":"private","port":3000}`},
		{[]string{"host", "vm1", "3000", "--public"}, 0, `"auth":"public"`},
		{[]string{"host", "vm1", "5173", "--localhost"}, 1, "!--localhost is not supported"},
		{[]string{"limits", "--all"}, 0, "GET /api/v1/limits"},
		{[]string{"api-key", "list", "--all"}, 0, "GET /v1/keys"},
		{[]string{"api-key", "usage", "k1", "--verbose"}, 0, "GET /v1/keys/k1/usage"},
		{[]string{"events", "vm1", "--convo", "current"}, 0, "GET /api/v1/sandboxes/vm1/events?conversation=01234567-89ab-cdef-8000-000000000000"},
		{[]string{"steer", "vm1", "--convo", "current", "Focus"}, 0, `POST /v1/vms/vm1/prompts/p1/steer {"prompt":"Focus"}`},
		{[]string{"interrupt", "vm1"}, 0, "POST /v1/vms/vm1/prompts/p1/interrupt"},
		{[]string{"interrupt", "vm1", "--convo", "conv_ffffffffffffffff"}, 1, "!no_running_turn"},
		{[]string{"convos", "vm1"}, 0, "GET /v1/vms/vm1/conversations"},
		{[]string{"prompt", "vm1", "--provider", "codex", "--reasoning-effort", "xhigh", "--fast", "Refactor"}, 0, "POST /v1/vms/vm1/prompt"},
		{[]string{"prompt", "vm1", "--attach", local, "Why?"}, 0, `PUT /v1/vms/vm1/files?path=%2Fhome%2Fdev%2Fattachments%2Fsession.env`},
		{[]string{"snapshot", "latest", "vm1"}, 0, "GET /api/v1/sandboxes/vm1/snapshots/latest"},
		{[]string{"snapshot", "tree", "s1"}, 0, "GET /api/v1/snapshots/s1/tree"},
		{[]string{"snapshot", "pull", "s1", "-o", down, "--path", "/x"}, 1, "!--path is not supported"},
		{[]string{"snapshot", "delete", "s1", "s2", "--yes"}, 0, "DELETE /api/v1/snapshots/s2"},
		{[]string{"snapshot", "delete", "--all", "vm1"}, 0, "DELETE /api/v1/sandboxes/vm1/snapshots"},
		{[]string{"snapshot", "share", "web-stack"}, 0, "POST /api/v1/saved-snapshots/web-stack/shares"},
		{[]string{"snapshot", "shares", "web-stack"}, 0, "GET /api/v1/saved-snapshots/web-stack/shares"},
		{[]string{"snapshot", "unshare", "web-stack", "shr_1"}, 0, "DELETE /api/v1/saved-snapshots/web-stack/shares/shr_1"},
		{[]string{"snapshot", "vm1", "web-stack"}, 0, `POST /api/v1/saved-snapshots {"name":"web-stack","sandboxId":"vm1"}`},
		{[]string{"snapshots", "vm1", "--limit", "50"}, 0, "GET /v1/vms/vm1/snapshots"},
		{[]string{"snapshots", "--all"}, 0, "GET /v1/snapshots"},
		{[]string{"ai", "profiles"}, 0, "GET /api/v1/credential-profiles"},
		{[]string{"ai", "profile", "default", "work"}, 0, `PUT /api/v1/credential-profiles/work {"isDefault":true}`},
		{[]string{"ai", "accounts", "anthropic-api-key"}, 0, "GET /api/v1/credential-profiles/default/accounts/anthropic-api-key"},
		{[]string{"ai", "switch", "anthropic-api-key", "1"}, 0, "PUT /api/v1/credential-profiles/default/accounts/anthropic-api-key/anthropic-api-key/active"},
		{[]string{"ai", "usage", "vm1", "--days", "7"}, 0, "GET /v1/vms/vm1/ai-usage"},
		{[]string{"env", "list"}, 0, "GET /v1/environments"},
		{[]string{"env", "info", "base"}, 0, "GET /v1/environments/base"},
		{[]string{"env", "rename", "staging", "prod"}, 0, `PUT /api/v1/environments/staging {"name":"prod"}`},
		{[]string{"env", "set-var", "base", "STRIPE_KEY=sk_1"}, 0, `PUT /api/v1/environments/base/vars/STRIPE_KEY {"value":"sk_1"}`},
		{[]string{"env", "rm-var", "base", "STRIPE_KEY"}, 0, "DELETE /api/v1/environments/base/vars/STRIPE_KEY"},
		{[]string{"env", "add-repo", "base", "octocat/hello-world", "--branch", "develop"}, 0, "POST /api/v1/environments/base/repos"},
		{[]string{"env", "set-file", "base", "backend/.env", "--from", local}, 0, "PUT /api/v1/environments/base/secret-files"},
		{[]string{"env", "upgrade", "base"}, 0, "POST /api/v1/environments/base/upgrade"},
		{[]string{"env", "set", "base", "--secrets", "false"}, 0, `PUT /api/v1/environments/base {"passSecrets":false}`},
		{[]string{"env", "set", "base", "--github", "true"}, 1, "!--github is not supported"},
		{[]string{"env", "list", "--org", "acme"}, 1, "!organizations"},
		{[]string{"status"}, 0, "GET /v1/me"},
		{[]string{"status", "--check"}, 0, "GET /v1/me"},
		{[]string{"--org", "acme", "new"}, 1, "!organizations are a boat.dev cloud feature"},
		{[]string{"--org", "personal", "limits"}, 0, "GET /api/v1/limits"},
		{[]string{"billing", "portal"}, 1, "!not supported on self-hosted"},
		{[]string{"login", "--google"}, 1, "!no browser or email sign-in"},
		{[]string{"browser", "vm1", "--profile", "/data/chrome-profile"}, 1, "!--profile is not supported"},
		{[]string{"ssh", "-t", "vm1", "--", "tmux", "attach"}, 0, "!-t"},
		{[]string{"scp", "-r", dir, "vm1:dir"}, 0, "PUT /v1/vms/vm1/files?path=%2Ftmp%2F.boat-push-"},
		{[]string{"scp", "--recursive", dir, "vm1:/home/user/"}, 0, `"my-project","/tmp/.boat-push-`},
		{[]string{"scp", dir, "vm1:dir"}, 2, "!is a directory (use -r)"},
		{[]string{"scp", local, "vm1:file"}, 0, "PUT /v1/vms/vm1/files?path=%2Fhome%2Fdev%2Ffile"},
		{[]string{"scp", "-r", "vm1:out", down}, 0, "GET /v1/vms/vm1/files?path=%2Ftmp%2F.boat-scp-"},
		{[]string{"webhook", "create", "https://example.com/h", "--name", "prod", "--event", "ready"}, 0, `"events":["sandbox.ready"]`},
	}
	for _, c := range cases {
		api.mu.Lock()
		api.reqs = nil
		api.mu.Unlock()
		env := map[string]string{"BOAT_URL": srv.URL, "BOAT_API_KEY": "boat_k", "BOAT_CONFIG": filepath.Join(tmp, "c.json"), "HOME": home}
		a, out, errw := acctApp(env, "")
		var argv []string
		a.execProg = func(v []string) error { argv = v; return nil }
		code := a.run(c.args)
		text := out.String() + errw.String() + strings.Join(argv, " ")
		ok := code == c.code
		if w, isText := strings.CutPrefix(c.want, "!"); isText {
			ok = ok && strings.Contains(text, w)
		} else {
			ok = ok && api.saw(c.want)
		}
		if !ok {
			t.Errorf("boat %s: exit %d (want %d), want %q\noutput: %s\nrequests: %q", strings.Join(c.args, " "), code, c.code, c.want, text, api.reqs)
		}
	}
	// scp -r down: the tree arrived, the escaping entry did not.
	if b, err := os.ReadFile(filepath.Join(down, "out", "sub", "b.txt")); err != nil || string(b) != "B" {
		t.Errorf("scp -r down: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "evil")); err == nil {
		t.Error("an archive entry escaped the target")
	}
}

// A usage error names what was wrong before the usage text.
func TestUsageErrorSaysWhy(t *testing.T) {
	env := map[string]string{"BOAT_URL": "http://unused.invalid", "BOAT_API_KEY": "boat_k", "BOAT_CONFIG": filepath.Join(t.TempDir(), "c.json")}
	a, _, errw := acctApp(env, "")
	if code := a.run([]string{"extend", "vm1"}); code != 2 {
		t.Fatalf("exit %d", code)
	}
	first, _, _ := strings.Cut(errw.String(), "\n")
	if !strings.Contains(first, "give one of --hours N") || !strings.Contains(errw.String(), "boat extend") {
		t.Errorf("stderr: %q", errw.String())
	}
	var j bytes.Buffer
	a, _, errw = acctApp(env, "")
	a.out = &j
	a.run([]string{"--json", "extend", "vm1"})
	var e struct{ Error struct{ Message string } }
	json.Unmarshal(errw.Bytes(), &e)
	if !strings.Contains(e.Error.Message, "give one of") {
		t.Errorf("json error: %q", errw.String())
	}
}
