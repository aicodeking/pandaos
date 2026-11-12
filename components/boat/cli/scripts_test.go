package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// boat.dev's "In scripts and CI" snippet (docs.boat.dev, CLI reference), run by
// bash with jq against a stub API: login --key-stdin --json, new --json piped
// into jq select(.event == "ready"), exec, and delete --yes --json in a trap.
// BOAT_API_URL carries boat.dev's /api/v1 base. --json errors end with an
// {"event":"error"} line on stdout; prompt and events print JSON Lines.
func TestBoatDevScriptSnippet(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "boat")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	var mu sync.Mutex
	var reqs []string
	vm := obj{"id": "vm-9", "name": "ci", "size": "small", "state": "running", "image": "ubuntu-24.04"}
	mux := http.NewServeMux()
	j := func(code int, v any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(v)
		}
	}
	mux.HandleFunc("GET /v1/me", j(200, obj{"user": obj{"id": "usr_1"}, "team": obj{"id": "team_1", "name": "acme"}, "role": "owner"}))
	mux.HandleFunc("POST /v1/vms", j(201, vm))
	mux.HandleFunc("GET /v1/vms/vm-9", j(200, vm))
	mux.HandleFunc("DELETE /v1/vms/vm-9", j(200, obj{"ok": true}))
	mux.HandleFunc("POST /v1/vms/vm-9/stop", j(409, obj{"error": obj{"code": "conflict", "message": "busy", "status": 409}}))
	stream := func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		enc.Encode(obj{"stream": "stdout", "data_b64": base64.StdEncoding.EncodeToString([]byte("ok\n"))})
		enc.Encode(obj{"exit": 0})
	}
	mux.HandleFunc("POST /v1/vms/vm-9/exec", stream)
	mux.HandleFunc("POST /v1/vms/vm-9/prompt", stream)
	mux.HandleFunc("GET /api/v1/sandboxes/vm-9/events", j(200, obj{"ok": true, "events": []obj{
		{"id": "e1", "type": "prompt", "timestamp": 1, "data": obj{"prompt": "hi"}},
		{"id": "e2", "type": "response", "timestamp": 2, "data": obj{"text": "hello"}}}}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()

	script := `set -euo pipefail
boat login --key-stdin --json <<< "$BOAT_API_KEY" >"$OUT/login.json"
sandbox_id="$(boat new --json | jq -r 'select(.event == "ready") | .id')"
echo "$sandbox_id" >"$OUT/id"
trap 'boat delete "$sandbox_id" --yes --json >"$OUT/delete.json"' EXIT
boat exec "$sandbox_id" --timeout 600 "npm test" >"$OUT/exec.out"
boat prompt "$sandbox_id" hi --json >"$OUT/prompt.jsonl"
boat events "$sandbox_id" --json >"$OUT/events.jsonl"
rc=0; boat stop "$sandbox_id" --json >"$OUT/stop.jsonl" 2>/dev/null || rc=$?
echo "$rc" >"$OUT/stop.rc"
`
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "OUT="+dir,
		"BOAT_CONFIG="+filepath.Join(dir, "config.json"), "BOAT_API_URL="+srv.URL+"/api/v1", "BOAT_URL=", "BOAT_API_KEY=boat_k_secret")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script: %v\n%s", err, out)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	if id := read("id"); id != "vm-9" {
		t.Errorf("ready id: %q", id)
	}
	if out := read("exec.out"); out != "ok" {
		t.Errorf("exec: %q", out)
	}
	if d := read("delete.json"); !strings.Contains(d, `"id":"vm-9"`) {
		t.Errorf("delete --yes --json: %q", d)
	}
	for _, f := range []string{"prompt.jsonl", "events.jsonl", "stop.jsonl"} {
		sc := bufio.NewScanner(strings.NewReader(read(f)))
		n := 0
		for sc.Scan() {
			var v map[string]any
			if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
				t.Errorf("%s line %d is not JSON: %q", f, n, sc.Text())
			}
			n++
		}
		if f == "events.jsonl" && n != 2 {
			t.Errorf("events: %d lines, want one per event", n)
		}
	}
	if rc := read("stop.rc"); rc == "0" {
		t.Error("a refused request exited 0")
	}
	lines := strings.Split(read("stop.jsonl"), "\n")
	var last map[string]any
	json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	if last["event"] != "error" || last["code"] != "conflict" || last["status"] != 409.0 || last["message"] != "busy" || last["error"] == nil {
		t.Errorf("error line: %v", last)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, r := range reqs {
		if !strings.HasSuffix(r, "Bearer boat_k_secret") || strings.HasPrefix(strings.Fields(r)[1], "/api/v1/v1") {
			t.Errorf("request %q: wrong base or key", r)
		}
	}
}
