package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestPortsRevoke(t *testing.T) {
	var mu sync.Mutex
	var reqs []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"vms":[{"id":"vm1","name":"dev","state":"running"}]}`)
	})
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"vm1","name":"dev","state":"running"}`)
	})
	mux.HandleFunc("GET /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		io.WriteString(w, `{"error":{"code":"not_found","message":"vm not found"}}`)
	})
	gen := 0
	mux.HandleFunc("POST /v1/vms/vm1/port-tokens/revoke", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.Method+" "+r.URL.RequestURI())
		gen++
		g := gen
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(obj{"ok": true, "generation": g})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	env := map[string]string{
		"BOAT_URL":     srv.URL,
		"BOAT_API_KEY": "boat_k",
		"BOAT_CONFIG":  filepath.Join(t.TempDir(), "c.json"),
	}

	// Text output by VM ID.
	a, out, errw := acctApp(env, "")
	if code := a.run([]string{"ports", "revoke", "vm1"}); code != 0 {
		t.Fatalf("ports revoke text: exit %d err=%s", code, errw)
	}
	if !strings.Contains(out.String(), "revoked port tokens for vm1 (generation 1)") {
		t.Fatalf("ports revoke text output = %q", out.String())
	}

	// Resolve VM by unique name and --json output.
	a, out, errw = acctApp(env, "")
	if code := a.run([]string{"ports", "revoke", "dev", "--json"}); code != 0 {
		t.Fatalf("ports revoke --json: exit %d err=%s", code, errw)
	}
	var got obj
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &got); err != nil {
		t.Fatalf("ports revoke --json unmarshal %q: %v", out.String(), err)
	}
	if got["ok"] != true || got["generation"] != float64(2) {
		t.Fatalf("ports revoke --json = %#v", got)
	}

	// Usage errors (missing or extra args) make no revoke request.
	before := len(reqs)
	for _, bad := range [][]string{{"ports", "revoke"}, {"ports", "revoke", "vm1", "3000"}} {
		a, _, _ = acctApp(env, "")
		if code := a.run(bad); code != 2 {
			t.Errorf("%v: exit %d, want 2", bad, code)
		}
	}
	if len(reqs) != before {
		t.Errorf("usage errors made requests: %v", reqs[before:])
	}

	// Help output mentions revoke.
	a, out, _ = acctApp(env, "")
	if code := a.run([]string{"help", "ports"}); code != 0 || !strings.Contains(out.String(), "revoke") {
		t.Errorf("help ports: code=%d out=%q", code, out.String())
	}
}

func TestHistoryModeAndQuery(t *testing.T) {
	var mu sync.Mutex
	var gotQueries []url.Values
	var gotPaths []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"vms":[{"id":"vm1","name":"dev","state":"running"}]}`)
	})
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"vm1","name":"dev","state":"running"}`)
	})
	mux.HandleFunc("GET /v1/vms/bx_23456789", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"vm1","name":"dev","state":"running"}`)
	})
	mux.HandleFunc("GET /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		io.WriteString(w, `{"error":{"code":"not_found","message":"vm not found"}}`)
	})
	mux.HandleFunc("GET /api/v1/sandboxes/vm1/command-history", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPaths = append(gotPaths, r.URL.Path)
		gotQueries = append(gotQueries, r.URL.Query())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		cmds := []obj{
			{
				"commandId": "c_2",
				"sandboxId": "bx_23456789",
				"command":   "npm test",
				"mode":      "detached",
				"status":    "exited",
				"exitCode":  0,
				"startedAt": "2026-01-02T00:00:00Z",
			},
			{
				"commandId": "p_1",
				"sandboxId": "bx_23456789",
				"command":   "claude prompt",
				"mode":      "process",
				"status":    "running",
				"exitCode":  nil,
				"startedAt": "2026-01-01T00:00:00Z",
			},
		}
		var next any
		if r.URL.Query().Get("limit") == "1" {
			cmds = cmds[:1]
			next = "cur_next_1"
		}
		json.NewEncoder(w).Encode(obj{"ok": true, "type": "command.list", "commands": cmds, "nextCursor": next})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	env := map[string]string{
		"BOAT_URL":     srv.URL,
		"BOAT_API_KEY": "boat_k",
		"BOAT_CONFIG":  filepath.Join(t.TempDir(), "c.json"),
	}

	cases := []struct {
		name     string
		args     []string
		wantMode string
		wantQ    string
		wantLim  string
		wantCur  string
	}{
		{
			name:     "mode detached",
			args:     []string{"history", "vm1", "--mode", "detached"},
			wantMode: "detached",
			wantLim:  "20",
		},
		{
			name:     "mode process",
			args:     []string{"history", "vm1", "--mode", "process"},
			wantMode: "process",
			wantLim:  "20",
		},
		{
			name:     "comma-separated modes with spaces",
			args:     []string{"history", "bx_23456789", "--mode", "detached, process, sync"},
			wantMode: "detached,process,sync",
			wantLim:  "20",
		},
		{
			name:     "search query --q with VM name and cursor",
			args:     []string{"history", "dev", "--mode", "detached,process", "--q", "npm test", "--limit", "5", "--cursor", "cur_0"},
			wantMode: "detached,process",
			wantQ:    "npm test",
			wantLim:  "5",
			wantCur:  "cur_0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			gotQueries = nil
			gotPaths = nil
			mu.Unlock()
			a, out, errw := acctApp(env, "")
			if code := a.run(tc.args); code != 0 {
				t.Fatalf("exit %d err=%s", code, errw)
			}
			if len(gotQueries) != 1 || gotPaths[0] != "/api/v1/sandboxes/vm1/command-history" {
				t.Fatalf("paths=%v queries=%v", gotPaths, gotQueries)
			}
			q := gotQueries[0]
			if q.Get("mode") != tc.wantMode || q.Get("q") != tc.wantQ || q.Get("limit") != tc.wantLim || q.Get("cursor") != tc.wantCur {
				t.Fatalf("query = %v, want mode=%q q=%q limit=%q cursor=%q", q, tc.wantMode, tc.wantQ, tc.wantLim, tc.wantCur)
			}
			txt := out.String()
			for _, want := range []string{"ID", "MODE", "STATUS", "EXIT", "STARTED_AT", "COMMAND", "c_2", "detached", "exited", "npm test", "p_1", "process"} {
				if !strings.Contains(txt, want) {
					t.Errorf("human table missing %q:\n%s", want, txt)
				}
			}
		})
	}

	// Cursor hint on stderr when nextCursor is present (human mode).
	a, _, errw := acctApp(env, "")
	if code := a.run([]string{"history", "vm1", "--limit", "1"}); code != 0 {
		t.Fatalf("history --limit 1: exit %d err=%s", code, errw)
	}
	if !strings.Contains(errw.String(), "--cursor cur_next_1") {
		t.Errorf("expected next cursor hint on stderr, got %q", errw.String())
	}

	// JSON mode outputs clean JSONL of compat command entries.
	a, out, errw := acctApp(env, "")
	if code := a.run([]string{"history", "vm1", "--mode", "detached,process", "--q", "npm", "--json"}); code != 0 {
		t.Fatalf("history --json: exit %d err=%s", code, errw)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL lines, got %d: %q", len(lines), out.String())
	}
	var first, second obj
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal line 0: %v", err)
	}
	if first["id"] != "c_2" || first["commandId"] != "c_2" || first["mode"] != "detached" || first["command"] != "npm test" {
		t.Errorf("first json line = %#v", first)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("unmarshal line 1: %v", err)
	}
	if second["id"] != "p_1" || second["commandId"] != "p_1" || second["mode"] != "process" {
		t.Errorf("second json line = %#v", second)
	}

	// Invalid modes and limits fail locally with exit 2 and no compat call.
	mu.Lock()
	before := len(gotQueries)
	mu.Unlock()
	for _, bad := range [][]string{
		{"history", "vm1", "--mode", "bogus"},
		{"history", "vm1", "--mode", "detached,"},
		{"history", "vm1", "--limit", "0"},
		{"history", "vm1", "--limit", "101"},
	} {
		a, _, _ = acctApp(env, "")
		if code := a.run(bad); code != 2 {
			t.Errorf("%v: exit %d, want 2", bad, code)
		}
	}
	mu.Lock()
	after := len(gotQueries)
	mu.Unlock()
	if after != before {
		t.Errorf("invalid history flags made %d requests", after-before)
	}
}

func TestKeysCreateTTLAndAPIKeyHelp(t *testing.T) {
	var mu sync.Mutex
	var bodies []obj
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/keys", func(w http.ResponseWriter, r *http.Request) {
		var body obj
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		resp := obj{"id": "k_new", "name": body["name"], "token": "boat_tok_1"}
		if _, hasExp := body["expires_in_s"]; hasExp {
			resp["expires"] = "2026-04-01T00:00:00Z"
		}
		json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	env := map[string]string{
		"BOAT_URL":     srv.URL,
		"BOAT_API_KEY": "boat_k",
		"BOAT_CONFIG":  filepath.Join(t.TempDir(), "c.json"),
	}

	valid := []struct {
		ttl  string
		want int64 // 0 means expires_in_s must be omitted
	}{
		{"24h", 86400},
		{"7d", 7 * 86400},
		{"90d", 90 * 86400},
		{"365d", 365 * 86400},
		{"3600", 3600},
		{"1h30m", 5400},
		{"never", 0},
		{"", 0},
	}

	for _, tc := range valid {
		mu.Lock()
		bodies = nil
		mu.Unlock()
		args := []string{"keys", "create", "--name", "ci"}
		if tc.ttl != "" {
			args = append(args, "--ttl", tc.ttl)
		}
		a, out, errw := acctApp(env, "")
		if code := a.run(args); code != 0 {
			t.Fatalf("keys create --ttl %q: exit %d err=%s", tc.ttl, code, errw)
		}
		if len(bodies) != 1 {
			t.Fatalf("keys create --ttl %q: got %d requests", tc.ttl, len(bodies))
		}
		v, hasExp := bodies[0]["expires_in_s"]
		if tc.want == 0 {
			if hasExp {
				t.Errorf("keys create --ttl %q: unexpected expires_in_s=%v", tc.ttl, v)
			}
		} else {
			if !hasExp || v != float64(tc.want) {
				t.Errorf("keys create --ttl %q: expires_in_s=%v (present=%v), want %d", tc.ttl, v, hasExp, tc.want)
			}
			if !strings.Contains(errw.String(), "expires at 2026-04-01T00:00:00Z") {
				t.Errorf("keys create --ttl %q: expected expiry note on stderr, got %q", tc.ttl, errw.String())
			}
		}
		if !strings.Contains(out.String(), "boat_tok_1") {
			t.Errorf("keys create --ttl %q: token missing from stdout %q", tc.ttl, out.String())
		}
	}

	// JSON output with --ttl.
	mu.Lock()
	bodies = nil
	mu.Unlock()
	a, out, errw := acctApp(env, "")
	if code := a.run([]string{"keys", "create", "--name", "ci", "--ttl", "7d", "--json"}); code != 0 {
		t.Fatalf("keys create --ttl 7d --json: exit %d err=%s", code, errw)
	}
	var created obj
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &created); err != nil {
		t.Fatalf("unmarshal keys create --json: %v", err)
	}
	if created["token"] != "boat_tok_1" || created["expires"] != "2026-04-01T00:00:00Z" {
		t.Errorf("keys create --json = %#v", created)
	}

	// Invalid TTLs fail locally with exit 2 and no POST.
	mu.Lock()
	bodies = nil
	mu.Unlock()
	for _, bad := range []string{"366d", "8761h", "0", "-24h", "0d", "500ms", "bogus"} {
		a, _, _ := acctApp(env, "")
		if code := a.run([]string{"keys", "create", "--name", "ci", "--ttl", bad}); code != 2 {
			t.Errorf("keys create --ttl %q: exit %d, want 2", bad, code)
		}
	}
	if len(bodies) != 0 {
		t.Errorf("invalid --ttl made POST requests: %v", bodies)
	}

	// Help text for keys and api-key documents supported TTLs.
	a, out, _ = acctApp(env, "")
	if code := a.run([]string{"help", "keys"}); code != 0 || !strings.Contains(out.String(), "--ttl") {
		t.Errorf("help keys missing --ttl: code=%d out=%q", code, out.String())
	}
	a, out, _ = acctApp(env, "")
	if code := a.run([]string{"help", "api-key"}); code != 0 || !strings.Contains(out.String(), "--ttl 90d|24h|365d|never") {
		t.Errorf("help api-key missing --ttl: code=%d out=%q", code, out.String())
	}
}

func TestForkTypeValidatedBeforeFork(t *testing.T) {
	var mu sync.Mutex
	var reqs []string
	mux := http.NewServeMux()
	rec := func(r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		line := r.Method + " " + r.URL.RequestURI()
		if s := strings.TrimSpace(string(b)); s != "" {
			line += " " + s
		}
		mu.Lock()
		reqs = append(reqs, line)
		mu.Unlock()
	}
	reply := func(w http.ResponseWriter, code int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		reply(w, 200, `{"id":"vm1","name":"dev","size":"default","state":"running","image":"ubuntu-24.04"}`)
	})
	mux.HandleFunc("GET /v1/images", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		reply(w, 200, `{"images":[],"sizes":{"default":{},"large":{}}}`)
	})
	mux.HandleFunc("POST /v1/vms/vm1/fork", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		reply(w, 201, `{"id":"vm_child","name":"child","size":"default","state":"stopped"}`)
	})
	mux.HandleFunc("POST /v1/vms/vm_child/resize", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		reply(w, 200, `{"id":"vm_child","size":"large","state":"stopped"}`)
	})
	mux.HandleFunc("POST /v1/vms/vm_child/start", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		reply(w, 200, `{"id":"vm_child","size":"large","state":"running"}`)
	})
	mux.HandleFunc("DELETE /v1/vms/vm_child", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		reply(w, 200, `{"ok":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	run := func(args ...string) (int, string, string) {
		home := t.TempDir()
		var out, errw bytes.Buffer
		a := &app{
			out:  &out,
			errw: &errw,
			in:   strings.NewReader(""),
			getenv: func(k string) string {
				return map[string]string{
					"HOME":         home,
					"BOAT_CONFIG":  filepath.Join(home, "c"),
					"BOAT_URL":     srv.URL,
					"BOAT_API_KEY": "boat_k",
				}[k]
			},
		}
		return a.run(args), out.String(), errw.String()
	}

	// Invalid --type on fork and --size on clone must fail before POST /v1/vms/vm1/fork.
	for _, args := range [][]string{
		{"fork", "vm1", "--type", "nosuch"},
		{"clone", "vm1", "--size", "nosuch"},
	} {
		reqs = nil
		code, _, errStr := run(args...)
		if code == 0 || !strings.Contains(errStr, `unknown size "nosuch"`) {
			t.Fatalf("%v: code=%d err=%q", args, code, errStr)
		}
		if got, want := strings.Join(reqs, "\n"), "GET /v1/vms/vm1\nGET /v1/images"; got != want {
			t.Fatalf("%v must not call POST fork on invalid size:\ngot:\n%s\nwant:\n%s", args, got, want)
		}
	}

	// Valid --type proceeds through fork -> resize -> start.
	reqs = nil
	if code, _, errStr := run("fork", "vm1", "--type", "large"); code != 0 {
		t.Fatalf("fork --type large failed: code=%d err=%q", code, errStr)
	}
	want := strings.Join([]string{
		"GET /v1/vms/vm1",
		"GET /v1/images",
		`POST /v1/vms/vm1/fork {"name":""}`,
		`POST /v1/vms/vm_child/resize {"discard_memory":true,"size":"large"}`,
		"POST /v1/vms/vm_child/start",
	}, "\n")
	if got := strings.Join(reqs, "\n"); got != want {
		t.Fatalf("fork --type large requests:\ngot:\n%s\nwant:\n%s", got, want)
	}
}
