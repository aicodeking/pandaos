package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// compat sends a boat.dev-style request with extra headers.
func (e *env) compat(token, method, path string, body any, hdr map[string]string) (int, map[string]any, http.Header) {
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
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 && raw[0] == '{' && !strings.Contains(resp.Header.Get("Content-Type"), "ndjson") {
		json.Unmarshal(raw, &out)
	} else {
		out = map[string]any{"_raw": string(raw)}
	}
	return resp.StatusCode, out, resp.Header
}

func errCode(out map[string]any) string {
	c, _ := out["code"].(string)
	return c
}

// The quickstart of docs.boat.dev against this server: create, get, command, stop.
func TestCompatQuickstart(t *testing.T) {
	e := setup(t)
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"ttlSeconds": 3600, "type": "small", "name": "q"}, nil)
	sb, _ := out["sandbox"].(map[string]any)
	if st != 202 || out["ok"] != true || out["type"] != "sandbox.created" || sb["id"] == nil || sb["type"] != "small" {
		t.Fatalf("create: %d %v", st, out)
	}
	id := sb["id"].(string)
	e.boatd.mu.Lock()
	if c := e.boatd.creates[len(e.boatd.creates)-1]; c["ttl_s"] != float64(3600) || c["size"] != "small" {
		t.Errorf("native create body: %v", c)
	}
	e.boatd.mu.Unlock()
	st, out, _ = e.compat(e.owner, "GET", "/api/v1/sandboxes/"+id, nil, nil)
	if st != 200 || out["type"] != "sandbox.info" || out["sandbox"].(map[string]any)["state"] != "running" {
		t.Errorf("get: %d %v", st, out)
	}
	st, out, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/commands", map[string]any{"command": "echo hi && uname -a"}, nil)
	if st != 200 || out["type"] != "command.finished" || out["exitCode"] != float64(0) || out["success"] != true || out["stdout"] != "hello\n" {
		t.Errorf("command: %d %v", st, out)
	}
	e.boatd.mu.Lock()
	ex := e.boatd.execs[len(e.boatd.execs)-1]
	e.boatd.mu.Unlock()
	if cmd := ex["cmd"].([]any); len(cmd) != 3 || cmd[0] != "bash" || cmd[2] != "echo hi && uname -a" || ex["cwd"] != "/home/dev" || ex["timeout_s"] != float64(30) {
		t.Errorf("native exec: %v", ex)
	}
	st, out, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/stop", nil, nil)
	if st != 200 || out["status"] != "archived" || out["sandbox"].(map[string]any)["state"] != "archived" {
		t.Errorf("stop: %d %v", st, out)
	}
	st, out, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"ttlSeconds": 7200, "failFast": true}, nil)
	if st != 202 || out["type"] != "sandbox.resuming" || out["status"] != "running" {
		t.Errorf("resume: %d %v", st, out)
	}
	st, out, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/stop", map[string]any{"force": true}, nil)
	if st != 200 || out["forced"] != true {
		t.Errorf("force stop: %d %v", st, out)
	}
	e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/resume", nil, nil)
	st, out, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/fork", map[string]any{"name": "kid", "ttlSeconds": 7200}, nil)
	if st != 202 || out["type"] != "sandbox.forking" || out["id"] == id {
		t.Errorf("fork: %d %v", st, out)
	}
}

func TestCompatStreamFilesAndErrors(t *testing.T) {
	e := setup(t)
	_, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", nil, nil)
	id := out["sandbox"].(map[string]any)["id"].(string)

	// Streamed command: started, stdout, exit.
	_, out, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/commands", map[string]any{"command": "echo hi", "stream": true}, nil)
	lines := strings.Split(strings.TrimSpace(out["_raw"].(string)), "\n")
	var first, last map[string]any
	json.Unmarshal([]byte(lines[0]), &first)
	json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	if first["type"] != "started" || last["type"] != "exit" || last["exitCode"] != float64(0) || !strings.Contains(out["_raw"].(string), `"type":"stdout"`) {
		t.Errorf("stream: %s", out["_raw"])
	}

	// Files: relative and /home/user paths land in the guest's home.
	st, out, _ := e.compat(e.owner, "PUT", "/api/v1/sandboxes/"+id+"/files", map[string]any{"path": "a.txt", "content": "hey"}, nil)
	if st != 200 || out["type"] != "file.written" || out["path"] != "/home/dev/a.txt" || out["size"] != float64(3) {
		t.Errorf("write: %d %v", st, out)
	}
	e.compat(e.owner, "PUT", "/api/v1/sandboxes/"+id+"/files", map[string]any{"path": "/home/user/b.bin",
		"content": base64.StdEncoding.EncodeToString([]byte{0, 1, 2}), "encoding": "base64"}, nil)
	st, out, _ = e.compat(e.owner, "GET", "/api/v1/sandboxes/"+id+"/files?path=b.bin&encoding=base64", nil, nil)
	if st != 200 || out["type"] != "file.read" || out["content"] != "AAEC" || out["size"] != float64(3) {
		t.Errorf("read: %d %v", st, out)
	}
	st, out, _ = e.compat(e.owner, "GET", "/api/v1/sandboxes/"+id+"/files?path=/home/user/a.txt", nil, nil)
	if st != 200 || out["content"] != "hey" || out["encoding"] != "utf8" {
		t.Errorf("read utf8: %d %v", st, out)
	}
	big := strings.Repeat("x", compatMaxFile+1)
	if st, out, _ := e.compat(e.owner, "PUT", "/api/v1/sandboxes/"+id+"/files", map[string]any{"path": "big", "content": big}, nil); st != 413 || errCode(out) != "file_too_large" {
		t.Errorf("big file: %d %v", st, errCode(out))
	}

	// boat.dev's error envelope, for every kind of failure.
	for _, c := range []struct {
		token, method, path string
		body                any
		status              int
		code                string
	}{
		{"", "GET", "/api/v1/sandboxes", nil, 401, "unauthorized"},
		{e.owner, "GET", "/api/v1/sandboxes/nope", nil, 404, "not_found"},
		{e.owner, "GET", "/api/v1/sandboxes?bogus=1", nil, 400, "invalid_query"},
		{e.owner, "POST", "/api/v1/sandboxes", map[string]any{"unknownField": 1}, 400, "invalid_json"},
		{e.owner, "POST", "/api/v1/sandboxes", map[string]any{"setupScript": "x"}, 400, "invalid_request"},
		{e.owner, "POST", "/api/v1/sandboxes", map[string]any{"ttlSeconds": 0}, 400, "invalid_request"},
		{e.owner, "POST", "/api/v1/sandboxes", map[string]any{"from": "no-such-snapshot"}, 404, "snapshot_not_found"},
		{e.owner, "POST", "/api/v1/sandboxes/" + id + "/commands", map[string]any{"command": "x", "timeoutSeconds": 601}, 400, "invalid_timeout"},
		{e.owner, "POST", "/api/v1/sandboxes/" + id + "/commands", map[string]any{"command": "x", "detached": true}, 400, "invalid_request"},
	} {
		st, out, _ := e.compat(c.token, c.method, c.path, c.body, nil)
		errObj, _ := out["error"].(map[string]any)
		rid, _ := out["requestId"].(string)
		if st != c.status || out["ok"] != false || out["type"] != "sandbox.error" || errCode(out) != c.code ||
			errObj["code"] != c.code || errObj["status"] != float64(c.status) || !strings.HasPrefix(rid, "req_") {
			t.Errorf("%s %s: %d %v", c.method, c.path, st, out)
		}
	}
	// A full host: 503 with retryAfterSeconds.
	e.boatd.mu.Lock()
	e.boatd.refuse = "host_capacity"
	e.boatd.mu.Unlock()
	if st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", nil, nil); st != 503 || errCode(out) != "out_of_capacity" || out["retryAfterSeconds"] != float64(10) {
		t.Errorf("full host: %d %v", st, out)
	}
}

func TestCompatListPagination(t *testing.T) {
	e := setup(t)
	for _, n := range []string{"a1", "a2", "a3", "b1", "b2"} {
		e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": n, "labels": map[string]string{"grp": n[:1]}}, nil)
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		path := "/api/v1/sandboxes?limit=2&sort=asc"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		st, out, _ := e.compat(e.owner, "GET", path, nil, nil)
		if st != 200 || out["type"] != "sandbox.list" {
			t.Fatalf("list: %d %v", st, out)
		}
		pages++
		for _, x := range out["sandboxes"].([]any) {
			id := x.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("%s twice", id)
			}
			seen[id] = true
		}
		pi := out["pageInfo"].(map[string]any)
		if pi["hasMore"] != true {
			break
		}
		cursor = pi["nextCursor"].(string)
	}
	if len(seen) != 5 || pages != 3 {
		t.Errorf("paged %d sandboxes in %d pages", len(seen), pages)
	}
	_, out, _ := e.compat(e.owner, "GET", "/api/v1/sandboxes?namePrefix=a&label=grp=a", nil, nil)
	if n := len(out["sandboxes"].([]any)); n != 3 {
		t.Errorf("filters: %d", n)
	}
	_, out, _ = e.compat(e.owner, "GET", "/api/v1/sandboxes?name=b2&state=running", nil, nil)
	if n := len(out["sandboxes"].([]any)); n != 1 {
		t.Errorf("name+state: %d", n)
	}
	// Another team sees nothing.
	other := otherTeamKey(e)
	_, out, _ = e.compat(other, "GET", "/api/v1/sandboxes", nil, nil)
	if n := len(out["sandboxes"].([]any)); n != 0 {
		t.Errorf("other team sees %d", n)
	}
}

func TestCompatDeleteAndLimits(t *testing.T) {
	e := setup(t)
	_, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", nil, nil)
	id := out["sandbox"].(map[string]any)["id"].(string)
	st, out, _ := e.compat(e.owner, "GET", "/api/v1/limits", nil, nil)
	if st != 200 || out["type"] != "limits.info" || out["activeSandboxes"] != float64(1) || out["canStart"] != true {
		t.Errorf("limits: %d %v", st, out)
	}
	if st, out, _ := e.compat(e.owner, "DELETE", "/api/v1/sandboxes/"+id, nil, nil); st != 409 || errCode(out) != "confirmation_required" {
		t.Errorf("delete without confirmation: %d %v", st, out)
	}
	st, out, _ = e.compat(e.owner, "DELETE", "/api/v1/sandboxes/"+id, nil, map[string]string{"X-Ascii-Confirm-Delete": id})
	op, _ := out["operation"].(map[string]any)
	if st != 202 || out["type"] != "sandbox.deleting" || !strings.HasPrefix(op["id"].(string), "bdop_") || op["targetId"] != id {
		t.Fatalf("delete: %d %v", st, out)
	}
	st, out, _ = e.compat(e.owner, "GET", "/api/v1/deletion-operations/"+op["id"].(string), nil, nil)
	if st != 200 || out["operation"].(map[string]any)["status"] != "completed" {
		t.Errorf("deletion op: %d %v", st, out)
	}
	if st, _, _ := e.compat(otherTeamKey(e), "GET", "/api/v1/deletion-operations/"+op["id"].(string), nil, nil); st != 404 {
		t.Errorf("another team's deletion op: %d", st)
	}
	if st, _ := e.do(e.owner, "GET", "/v1/vms/"+id, nil); st != 404 {
		t.Errorf("deleted VM still there: %d", st)
	}
}

func TestCompatIdempotencyKey(t *testing.T) {
	e := setup(t)
	k := map[string]string{"Idempotency-Key": "abc"}
	st, a, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "x"}, k)
	st2, b, h := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "x"}, k)
	if st != 202 || st2 != 202 || a["sandbox"].(map[string]any)["id"] != b["sandbox"].(map[string]any)["id"] || h.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay: %d %d %v %v", st, st2, a, b)
	}
	e.boatd.mu.Lock()
	n := len(e.boatd.creates)
	e.boatd.mu.Unlock()
	if n != 1 {
		t.Errorf("%d creates for one key", n)
	}
	if st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "y"}, k); st != 409 || errCode(out) != "idempotency_key_reused" {
		t.Errorf("other body: %d %v", st, out)
	}
	// Per team: another team's same key is its own.
	if st, _, _ := e.compat(otherTeamKey(e), "POST", "/api/v1/sandboxes", map[string]any{"name": "x"}, k); st != 202 {
		t.Errorf("other team, same key: %d", st)
	}
	// A failed request frees its key.
	bad := map[string]string{"Idempotency-Key": "f"}
	e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"from": "nope"}, bad)
	if st, _, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"from": "nope"}, bad); st != 404 {
		t.Errorf("retry after a failure: %d", st)
	}
	// Fork honours it too.
	id := a["sandbox"].(map[string]any)["id"].(string)
	fk := map[string]string{"Idempotency-Key": "fork-1"}
	_, f1, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/fork", map[string]any{"name": "f"}, fk)
	_, f2, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/fork", map[string]any{"name": "f"}, fk)
	if f1["id"] == nil || f1["id"] != f2["id"] {
		t.Errorf("fork replay: %v %v", f1, f2)
	}
	if st, _, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", nil, map[string]string{"Idempotency-Key": strings.Repeat("k", 256)}); st != 400 {
		t.Errorf("long key: %d", st)
	}
	// In progress: a second request with the key while the first runs.
	if _, code := e.srv.idem.begin(e.teamID()+"\x00slow", "h"); code != "" {
		t.Fatal(code)
	}
	if _, code := e.srv.idem.begin(e.teamID()+"\x00slow", "h"); code != "idempotency_in_progress" {
		t.Errorf("in progress: %q", code)
	}
}

func TestCompatBatchAndVMToken(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"count": 3, "name": "w"}, nil)
	if l, _ := out["sandboxes"].([]any); st != 202 || len(l) != 3 {
		t.Fatalf("batch: %d %v", st, out)
	}
	id := out["sandboxes"].([]any)[0].(map[string]any)["id"].(string)
	other := out["sandboxes"].([]any)[1].(map[string]any)["id"].(string)
	tok, _ := lastEnvOp(e, id)["BOAT_API_KEY"].(string)
	if st, _, _ := e.compat(tok, "GET", "/api/v1/sandboxes/"+id, nil, nil); st != 200 {
		t.Errorf("VM token on itself through compat: %d", st)
	}
	for _, p := range []string{"/api/v1/sandboxes/" + other, "/api/v1/sandboxes", "/api/v1/limits"} {
		if st, _, _ := e.compat(tok, "GET", p, nil, nil); st != 404 {
			t.Errorf("VM token on %s: %d", p, st)
		}
	}
	if st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"count": 17}, nil); st != 400 {
		t.Errorf("count 17: %d %v", st, out)
	}
}

// otherTeamKey makes a second team with its own owner key.
func otherTeamKey(e *env) string {
	e.t.Helper()
	_, tok := e.newTeam("other" + randHex(3))
	return tok
}
