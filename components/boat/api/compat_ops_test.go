package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

var reBX = regexp.MustCompile(`^bx_[23456789abcdefghjkmnpqrstuvwxyz]{8}$`)

// newSandbox creates one sandbox through /api/v1 and returns its bx_ id.
func (e *env) newSandbox(body map[string]any) string {
	e.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", body, nil)
	if st != 202 {
		e.t.Fatalf("create: %d %v", st, out)
	}
	return out["sandbox"].(map[string]any)["id"].(string)
}

func (e *env) ok(method, path string, body any, want int) map[string]any {
	e.t.Helper()
	st, out, _ := e.compat(e.owner, method, path, body, nil)
	if st != want || out["ok"] != true {
		e.t.Fatalf("%s %s: %d %v", method, path, st, out)
	}
	return out
}

// The official SDKs refuse a Sandbox whose id is not bx_ + 8 of boat.dev's
// alphabet. Ids are stable, survive a restart, never collide, and another
// team's id is 404 (and the native id still works).
func TestCompatSandboxIDs(t *testing.T) {
	e := setup(t)
	id := e.newSandbox(map[string]any{"name": "a"})
	if !reBX.MatchString(id) {
		t.Fatalf("id %q is not a boat.dev sandbox id", id)
	}
	vm := e.srv.store.Snapshot().CompatIDs[id].VM
	if got := e.ok("GET", "/api/v1/sandboxes/"+id, nil, 200)["sandbox"].(map[string]any)["id"]; got != id {
		t.Errorf("get: %v", got)
	}
	if got := e.ok("GET", "/api/v1/sandboxes/"+vm, nil, 200)["sandbox"].(map[string]any)["id"]; got != id {
		t.Errorf("native id: %v", got)
	}
	list := e.ok("GET", "/api/v1/sandboxes", nil, 200)["sandboxes"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != id {
		t.Errorf("list: %v", list)
	}
	// Persisted: a fresh store from the same directory knows it.
	st2, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if c := st2.Snapshot().CompatIDs[id]; c == nil || c.VM != vm {
		t.Errorf("not persisted: %v", c)
	}
	// Collisions are drawn again.
	draws := []string{id, id, "bx_22222222"}
	e.srv.drawCompatID = func() string { d := draws[0]; draws = draws[1:]; return d }
	id2 := e.newSandbox(map[string]any{"name": "b"})
	if id2 != "bx_22222222" {
		t.Errorf("collision not redrawn: %s", id2)
	}
	e.srv.drawCompatID = nil
	other := otherTeamKey(e)
	if st, out, _ := e.compat(other, "GET", "/api/v1/sandboxes/"+id, nil, nil); st != 404 || errCode(out) != "not_found" {
		t.Errorf("other team: %d %v", st, out)
	}
	if st, _, _ := e.compat(e.owner, "GET", "/api/v1/sandboxes/bx_33333333", nil, nil); st != 404 {
		t.Errorf("unknown id: %d", st)
	}
}

func TestCompatDeleteByBXID(t *testing.T) {
	e := setup(t)
	id := e.newSandbox(nil)
	st, out, _ := e.compat(e.owner, "DELETE", "/api/v1/sandboxes/"+id, nil, map[string]string{"X-Ascii-Confirm-Delete": id})
	if st != 202 || out["operation"].(map[string]any)["targetId"] != id {
		t.Fatalf("delete: %d %v", st, out)
	}
	if e.srv.store.Snapshot().CompatIDs[id] != nil {
		t.Error("bx_ id kept after delete")
	}
}

// PATCH renames and sets ttlSeconds (archiveAfter moves); native PATCH
// extend_s pushes the deadline; an old boatd gives 501.
func TestCompatUpdateAndExtend(t *testing.T) {
	e := setup(t)
	id := e.newSandbox(map[string]any{"name": "a", "ttlSeconds": 600})
	sb := e.ok("PATCH", "/api/v1/sandboxes/"+id, map[string]any{"name": "renamed", "ttlSeconds": 7200}, 200)["sandbox"].(map[string]any)
	after, err := time.Parse(time.RFC3339, sb["archiveAfter"].(string))
	if sb["name"] != "renamed" || sb["ttlSeconds"] != float64(7200) || err != nil || time.Until(after) < 7100*time.Second {
		t.Fatalf("patch: %v", sb)
	}
	vm := e.srv.store.Snapshot().CompatIDs[id].VM
	st, out := e.do(e.owner, "PATCH", "/v1/vms/"+vm, map[string]any{"extend_s": 3600})
	ext, _ := time.Parse(time.RFC3339, str(out, "expires_at"))
	if st != 200 || ext.Sub(after) < 3599*time.Second {
		t.Errorf("extend: %d %v", st, out)
	}
	if st, _ := e.do(e.owner, "PATCH", "/v1/vms/"+vm, map[string]any{"extend_s": 0}); st != 400 {
		t.Errorf("extend 0: %d", st)
	}
	e.boatd.noPatch = true
	if st, out := e.do(e.owner, "PATCH", "/v1/vms/"+vm, map[string]any{"extend_s": 60}); st != 501 || out["error"].(map[string]any)["code"] != "not_supported" {
		t.Errorf("old boatd: %d %v", st, out)
	}
	e.boatd.noPatch = false
	// A prompt gives the VM a fresh full TTL, as on boat.dev.
	e.boatd.mu.Lock()
	n := len(e.boatd.patches)
	e.boatd.mu.Unlock()
	e.ok("POST", "/api/v1/sandboxes/"+id+"/prompt", map[string]any{"provider": "claude", "prompt": "hello"}, 202)
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	if len(e.boatd.patches) != n+1 || e.boatd.patches[n]["ttl_s"] != float64(7200) {
		t.Errorf("prompt did not refresh the TTL: %v", e.boatd.patches)
	}
}

func TestCompatMeLimitsUsage(t *testing.T) {
	e := setup(t)
	id := e.newSandbox(map[string]any{"type": "small"})
	if u := e.ok("GET", "/api/v1/me", nil, 200)["user"].(map[string]any); u["id"] == "" {
		t.Errorf("me: %v", u)
	}
	if u := e.ok("GET", "/api/v1/sandboxes/"+id+"/usage", nil, 200); u["sandboxId"] != id || u["sandboxType"] != "small" {
		t.Errorf("usage: %v", u)
	}
}

func TestCompatPromptsFlow(t *testing.T) {
	e := setup(t)
	id := e.newSandbox(nil)
	base := "/api/v1/sandboxes/" + id
	q := e.ok("POST", base+"/prompt", map[string]any{"provider": "claude-code", "prompt": "sleep-forever fix it"}, 202)
	pid := q["promptId"].(string)
	conv, _ := q["conversationId"].(string)
	if q["id"] != id || q["status"] != "queued" || len(conv) != 36 {
		t.Fatalf("prompt: %v", q)
	}
	if st := e.ok("GET", base, nil, 200)["sandbox"].(map[string]any)["state"]; st != "running" {
		t.Errorf("state while an agent works: %v", st)
	}
	run := e.ok("GET", base+"/prompts/"+pid, nil, 200)["promptRun"].(map[string]any)
	if run["status"] != "running" || run["done"] != false || run["conversationId"] != conv || run["sandboxId"] != id {
		t.Errorf("prompt run: %v", run)
	}
	convs := e.ok("GET", base+"/conversations", nil, 200)["conversations"].([]any)
	if len(convs) != 1 || convs[0].(map[string]any)["id"] != conv || convs[0].(map[string]any)["running"] != true {
		t.Errorf("conversations: %v", convs)
	}
	st := e.ok("POST", base+"/steer", map[string]any{"message": "skip the tests", "conversation": conv}, 200)
	if st["conversationId"] != conv || st["promptId"] == pid || st["status"] != "steered" {
		t.Errorf("steer: %v", st)
	}
	// A second conversation, interrupted.
	q2 := e.ok("POST", base+"/prompt", map[string]any{"provider": "claude", "prompt": "sleep-forever other", "new": true}, 202)
	in := e.ok("POST", base+"/interrupt?conversation="+q2["conversationId"].(string), nil, 200)
	if in["conversationId"] != q2["conversationId"] {
		t.Errorf("interrupt: %v", in)
	}
	if st, out, _ := e.compat(e.owner, "POST", base+"/interrupt?conversation="+q2["conversationId"].(string), nil, nil); st != 409 {
		t.Errorf("interrupt twice: %d %v", st, out)
	}
	// Events: prompt and response events, in order, paged by the SDKs' cursor.
	ev := e.ok("GET", base+"/events?sort=asc&limit=2", nil, 200)
	list := ev["events"].([]any)
	if len(list) != 2 || ev["pageInfo"].(map[string]any)["hasMore"] != true || list[0].(map[string]any)["type"] != "prompt" {
		t.Fatalf("events: %v", ev)
	}
	last := list[1].(map[string]any)
	cur := base64.RawURLEncoding.EncodeToString([]byte(jsonNum(last["timestamp"]) + ":" + last["id"].(string)))
	rest := e.ok("GET", base+"/events?sort=asc&cursor="+cur, nil, 200)["events"].([]any)
	for _, x := range rest {
		if x.(map[string]any)["id"] == last["id"] || x.(map[string]any)["id"] == list[0].(map[string]any)["id"] {
			t.Errorf("cursor repeated an event: %v", x)
		}
	}
	if len(rest) == 0 {
		t.Error("no events after the cursor")
	}
	if r := e.ok("GET", base+"/events?type=response", nil, 200)["events"].([]any); len(r) == 0 || r[0].(map[string]any)["type"] != "response" {
		t.Errorf("type filter: %v", r)
	}
}

func jsonNum(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestCompatCommandsFlow(t *testing.T) {
	e := setup(t)
	id := e.newSandbox(nil)
	base := "/api/v1/sandboxes/" + id
	started := e.ok("POST", base+"/commands", map[string]any{"command": "sleep 100", "detached": true}, 200)
	if started["type"] != "command.started" || started["processId"] != float64(1) || started["pid"] != float64(42) {
		t.Fatalf("detached: %v", started)
	}
	pid := "1"
	if s := e.ok("GET", base+"/commands/"+pid, nil, 200); s["processId"] != float64(1) || s["command"] != "sleep 100" {
		t.Errorf("status: %v", s)
	}
	hist := e.ok("GET", base+"/command-history", nil, 200)["commands"].([]any)
	if len(hist) != 1 || hist[0].(map[string]any)["sandboxId"] != id || hist[0].(map[string]any)["mode"] != "detached" {
		t.Fatalf("history: %v", hist)
	}
	cid := hist[0].(map[string]any)["commandId"].(string)
	if c := e.ok("GET", base+"/command-history/"+cid, nil, 200)["command"].(map[string]any); c["processId"] != float64(1) {
		t.Errorf("info: %v", c)
	}
	if l := e.ok("GET", base+"/command-history/"+cid+"/logs", nil, 200); l["commandId"] != cid || l["stream"] != "stdout" {
		t.Errorf("logs: %v", l)
	}
	if s := e.ok("DELETE", base+"/commands/"+pid, nil, 200); s["signaled"] != true || s["command"].(map[string]any)["commandId"] != cid {
		t.Errorf("stop: %v", s)
	}
	if p := e.ok("GET", base+"/processes", nil, 200); p["total"] == nil {
		t.Errorf("processes: %v", p)
	}
	if st, _, _ := e.compat(e.owner, "GET", base+"/commands/99", nil, nil); st != 404 {
		t.Errorf("unknown process: %d", st)
	}
}

func TestCompatFilesFlow(t *testing.T) {
	e := setup(t)
	id := e.newSandbox(nil)
	base := "/api/v1/sandboxes/" + id
	e.ok("PUT", base+"/files", map[string]any{"path": "dir/a.txt", "content": "hi"}, 200)
	if f := e.ok("GET", base+"/files/list?path=dir", nil, 200)["files"].([]any); len(f) != 1 || f[0].(map[string]any)["path"] != "/home/dev/dir/a.txt" {
		t.Errorf("list: %v", f)
	}
	if s := e.ok("GET", base+"/files/search?pattern=*a.txt*", nil, 200)["files"].([]any); len(s) != 1 {
		t.Errorf("search: %v", s)
	}
	if m := e.ok("POST", base+"/files/move", map[string]any{"source": "dir/a.txt", "destination": "dir/b.txt"}, 200); m["destination"] != "/home/dev/dir/b.txt" {
		t.Errorf("move: %v", m)
	}
	e.ok("GET", base+"/files/info?path=dir/b.txt", nil, 200)
	e.ok("POST", base+"/files/folder", map[string]any{"path": "newdir", "mode": "755"}, 200)
	e.ok("POST", base+"/files/permissions", map[string]any{"path": "dir/b.txt", "mode": "600"}, 200)
	if d := e.ok("DELETE", base+"/files?path=dir/b.txt", nil, 200); d["path"] != "/home/dev/dir/b.txt" {
		t.Errorf("delete: %v", d)
	}
	if st, _, _ := e.compat(e.owner, "DELETE", base+"/files", nil, nil); st != 400 {
		t.Errorf("delete without path: %d", st)
	}
}

func TestCompatSavedSnapshots(t *testing.T) {
	e := setup(t)
	id := e.newSandbox(nil)
	s := e.ok("POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "golden"}, 202)["snapshot"].(map[string]any)
	if s["name"] != "golden" || s["sourceSandboxId"] != id {
		t.Fatalf("save: %v", s)
	}
	e.ok("POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "golden"}, 202) // replaces
	if l := e.ok("GET", "/api/v1/saved-snapshots", nil, 200)["snapshots"].([]any); len(l) != 1 {
		t.Errorf("list after a replace: %v", l)
	}
	e.ok("GET", "/api/v1/saved-snapshots/golden", nil, 200)
	kid := e.newSandbox(map[string]any{"from": "golden"})
	if !reBX.MatchString(kid) {
		t.Errorf("create from: %s", kid)
	}
	e.ok("DELETE", "/api/v1/saved-snapshots/golden", nil, 200)
	if st, _, _ := e.compat(e.owner, "GET", "/api/v1/saved-snapshots/golden", nil, nil); st != 404 {
		t.Errorf("after delete: %d", st)
	}
	other := otherTeamKey(e)
	if st, _, _ := e.compat(other, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "steal"}, nil); st != 404 {
		t.Errorf("other team saves my sandbox: %d", st)
	}
}

// Webhooks registered through /api/v1 get boat.dev's events, payload and
// signature; native webhooks keep theirs.
func TestCompatWebhooks(t *testing.T) {
	e := setup(t)
	target := &hookTarget{}
	ts := httptest.NewServer(target)
	defer ts.Close()
	cr := e.ok("POST", "/api/v1/webhooks", map[string]any{"name": "prod", "url": ts.URL, "events": []string{"sandbox.archived", "sandbox.ready"}}, 201)
	hook := cr["webhook"].(map[string]any)
	secret := cr["secret"].(string)
	if !regexp.MustCompile(`^wh_[a-f0-9]{24}$`).MatchString(hook["id"].(string)) || !regexp.MustCompile(`^whsec_[a-f0-9]{64}$`).MatchString(secret) {
		t.Fatalf("ids: %v", cr)
	}
	hid := hook["id"].(string)
	if l := e.ok("GET", "/api/v1/webhooks", nil, 200)["webhooks"].([]any); len(l) != 1 {
		t.Errorf("list: %v", l)
	}
	e.ok("GET", "/api/v1/webhooks/"+hid, nil, 200)
	if u := e.ok("PATCH", "/api/v1/webhooks/"+hid, map[string]any{"events": []string{"sandbox.archived"}}, 200)["webhook"].(map[string]any); len(u["events"].([]any)) != 1 {
		t.Errorf("update: %v", u)
	}
	if st, _, _ := e.compat(e.owner, "POST", "/api/v1/webhooks", map[string]any{"url": ts.URL, "events": []string{"vm.stopped"}}, nil); st != 400 {
		t.Errorf("native event name accepted: %d", st)
	}
	rot := e.ok("POST", "/api/v1/webhooks/"+hid+"/rotate", nil, 200)
	secret = rot["secret"].(string)

	id := e.newSandbox(map[string]any{"name": "hooked"})
	vm := e.srv.store.Snapshot().CompatIDs[id].VM
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.srv.hooks.watch(ctx)
	e.boatd.events <- `{"type":"vm.state","vm_id":"` + vm + `","team":"` + e.teamID() + `","state":"running","prev":"starting"}` // not subscribed
	e.boatd.events <- `{"type":"vm.state","vm_id":"` + vm + `","team":"` + e.teamID() + `","state":"stopped","prev":"stopping"}`
	waitFor(t, "delivery", func() bool { return target.count() >= 1 })
	time.Sleep(50 * time.Millisecond)
	target.mu.Lock()
	defer target.mu.Unlock()
	if len(target.got) != 1 {
		t.Fatalf("deliveries: %v", target.body)
	}
	req, body := target.got[0], target.body[0]
	var payload struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			Sandbox       map[string]any `json:"sandbox"`
			PreviousState string         `json:"previousState"`
			State         string         `json:"state"`
		} `json:"data"`
	}
	json.Unmarshal([]byte(body), &payload)
	if payload.Type != "sandbox.archived" || payload.Data.Sandbox["id"] != id || payload.Data.Sandbox["name"] != "hooked" ||
		payload.Data.State != "archived" || payload.Data.PreviousState != "archiving" || !strings.HasPrefix(payload.ID, "evt_") {
		t.Errorf("payload: %s", body)
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(req.Header.Get("X-Ascii-Delivery") + "." + req.Header.Get("X-Ascii-Timestamp") + "."))
	m.Write([]byte(body))
	if req.Header.Get("X-Ascii-Signature") != "v1="+hex.EncodeToString(m.Sum(nil)) || req.Header.Get("X-Ascii-Event") != "sandbox.archived" ||
		req.Header.Get("X-Ascii-Delivery") != payload.ID || req.Header.Get("X-Ascii-Attempt") != "1" {
		t.Errorf("headers: %v", req.Header)
	}
	if d := e.ok("DELETE", "/api/v1/webhooks/"+hid, nil, 200); d["id"] != hid {
		t.Errorf("delete: %v", d)
	}
}

func TestCompatKeysAndEnvironments(t *testing.T) {
	e := setup(t)
	list := e.ok("GET", "/api/v1/api-keys", nil, 200)
	if len(list["apiKeys"].([]any)) != 1 || list["catalog"].(map[string]any)["scopedCreationEnabled"] != true {
		t.Fatalf("keys: %v", list)
	}
	cr := e.ok("POST", "/api/v1/api-keys/scoped", map[string]any{"name": "ci", "preset": "read-only"}, 201)
	k := cr["apiKey"].(map[string]any)
	secret := cr["secret"].(string)
	if acts := k["scope"].(map[string]any)["actions"].([]any); len(acts) != 1 || acts[0] != "vms:read" || len(cr["apiKeys"].([]any)) != 2 {
		t.Fatalf("create: %v", cr)
	}
	if st, _, _ := e.compat(secret, "GET", "/api/v1/sandboxes", nil, nil); st != 200 {
		t.Errorf("the new key reads: %d", st)
	}
	if st, _, _ := e.compat(secret, "POST", "/api/v1/sandboxes", map[string]any{}, nil); st != 403 {
		t.Errorf("a read-only key creates: %d", st)
	}
	if st, _, _ := e.compat(e.owner, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "x", "sandboxIds": []string{"bx_22222222"}}, nil); st != 400 {
		t.Errorf("per-sandbox key accepted: %d", st)
	}
	kid := k["id"].(string)
	rot := e.ok("POST", "/api/v1/api-keys/"+kid+"/rotate", nil, 200)
	if rot["secret"] == secret {
		t.Error("rotation kept the secret")
	}
	if st, _, _ := e.compat(secret, "GET", "/api/v1/sandboxes", nil, nil); st != 401 {
		t.Errorf("old secret after rotation: %d", st)
	}
	if u := e.ok("GET", "/api/v1/api-keys/"+kid+"/usage", nil, 200); u["id"] != kid {
		t.Errorf("usage: %v", u)
	}
	if l := e.ok("DELETE", "/api/v1/api-keys/"+kid, nil, 200)["apiKeys"].([]any); len(l) != 1 {
		t.Errorf("revoke: %v", l)
	}

	envs := e.ok("POST", "/api/v1/environments", map[string]any{"name": "web"}, 200)["environments"].([]any)
	if len(envs) != 1 {
		t.Fatalf("create env: %v", envs)
	}
	env := envs[0].(map[string]any)
	id := env["id"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-8[0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Errorf("environment id %q is not a UUID", id)
	}
	st, out, _ := e.compat(e.owner, "PUT", "/api/v1/environments/"+id, map[string]any{"envContents": "A=1\n# c\nB=two\n", "passSecrets": false}, nil)
	upd, _ := out["environment"].(map[string]any)
	if st != 200 || out["success"] != true || upd["envContents"] != "A=1\nB=two\n" || upd["passSecrets"] != false || upd["id"] != id {
		t.Fatalf("update env: %d %v", st, out)
	}
	if got := e.ok("GET", "/api/v1/environments", nil, 200)["environments"].([]any); len(got) != 1 {
		t.Errorf("list envs: %v", got)
	}
	if st, out, _ := e.compat(e.owner, "DELETE", "/api/v1/environments/"+id, nil, nil); st != 200 || out["success"] != true {
		t.Errorf("delete env: %d %v", st, out)
	}
	other := otherTeamKey(e)
	e.ok("POST", "/api/v1/environments", map[string]any{"name": "mine"}, 200)
	if l := e.ok("GET", "/api/v1/environments", nil, 200)["environments"].([]any); len(l) != 1 {
		t.Fatal(l)
	}
	mine := e.ok("GET", "/api/v1/environments", nil, 200)["environments"].([]any)[0].(map[string]any)["id"].(string)
	if st, _, _ := e.compat(other, "DELETE", "/api/v1/environments/"+mine, nil, nil); st != 404 {
		t.Errorf("other team deletes my environment: %d", st)
	}
}
