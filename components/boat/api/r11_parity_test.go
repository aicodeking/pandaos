package main

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

func (e *env) nativeOf(bx string) string {
	e.t.Helper()
	c := e.srv.store.Snapshot().CompatIDs[bx]
	if c == nil {
		e.t.Fatalf("no vm for %s", bx)
	}
	return c.VM
}

func (e *env) waitSetup(bx string, want string) map[string]any {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sb := e.ok("GET", "/api/v1/sandboxes/"+bx, nil, 200)["sandbox"].(map[string]any)
		if sb["setupStatus"] == want || time.Now().After(deadline) {
			return sb
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCompatCreateFilesAndSetup(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(map[string]any{
		"files": []map[string]any{
			{"path": "workspace/.env", "content": "A=1\n"},
			{"path": "/home/user/b.bin", "content": base64.StdEncoding.EncodeToString([]byte{0, 1, 2}), "encoding": "base64"},
			{"path": "/tmp/boot.txt", "content": "x"},
		},
		"setupScript": "echo hi",
	})
	vm := e.nativeOf(bx)
	e.boatd.mu.Lock()
	got := map[string]string{}
	for _, p := range []string{"/home/dev/workspace/.env", "/home/dev/b.bin", "/tmp/boot.txt"} {
		got[p] = string(e.boatd.files[vm+":"+p])
	}
	e.boatd.mu.Unlock()
	if got["/home/dev/workspace/.env"] != "A=1\n" || got["/home/dev/b.bin"] != "\x00\x01\x02" || got["/tmp/boot.txt"] != "x" {
		t.Errorf("files: %q", got)
	}
	if sb := e.waitSetup(bx, "done"); sb["setupStatus"] != "done" || sb["setupError"] != nil {
		t.Errorf("setup: %v %v", sb["setupStatus"], sb["setupError"])
	}
	// a failing script: create succeeds, setupStatus failed with the exit code
	bx2 := e.newSandbox(map[string]any{"setupScript": "exit 3"})
	if sb := e.waitSetup(bx2, "failed"); sb["setupStatus"] != "failed" || !strings.Contains(fmt.Sprint(sb["setupError"]), "exit 3") {
		t.Errorf("failed setup: %v %v", sb["setupStatus"], sb["setupError"])
	}
	for _, c := range []struct {
		body map[string]any
		code string
	}{
		{map[string]any{"files": []map[string]any{{"path": "/etc/passwd", "content": "x"}}}, "invalid_files"},
		{map[string]any{"files": []map[string]any{{"path": "../../etc/x", "content": "x"}}}, "invalid_files"},
		{map[string]any{"files": []map[string]any{{"path": "a", "content": "x"}, {"path": "/home/user/a", "content": "y"}}}, "invalid_files"},
		{map[string]any{"files": []map[string]any{{"path": "a", "content": "!!", "encoding": "base64"}}}, "invalid_files"},
		{map[string]any{"setupScript": strings.Repeat("x", 64<<10+1)}, "invalid_setup_script"},
	} {
		st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", c.body, nil)
		if st != 400 || out["error"].(map[string]any)["code"] != c.code {
			t.Errorf("%v: %d %v", c.body["files"], st, out)
		}
	}
}

func TestCompatForkResumeEnvType(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	fork := e.ok("POST", "/api/v1/sandboxes/"+bx+"/fork", map[string]any{"env": map[string]string{"FOO": "bar"}, "type": "large"}, 202)
	child := e.nativeOf(fork["sandbox"].(map[string]any)["id"].(string))
	if v := lastEnvOp(e, child); v["FOO"] != "bar" {
		t.Errorf("fork env: %v", v)
	}
	e.boatd.mu.Lock()
	size := e.boatd.vms[child]["size"]
	e.boatd.mu.Unlock()
	if size != "large" {
		t.Errorf("fork type: size %v", size)
	}
	// labels replace the parent's; its reserved boat.* labels carry over.
	e.boatd.mu.Lock()
	e.boatd.vms[e.nativeOf(bx)]["labels"] = map[string]any{"old": "1", secretsLabel: "S"}
	e.boatd.mu.Unlock()
	fork = e.ok("POST", "/api/v1/sandboxes/"+bx+"/fork", map[string]any{"labels": map[string]string{"a": "b"}}, 202)
	child = e.nativeOf(fork["sandbox"].(map[string]any)["id"].(string))
	e.boatd.mu.Lock()
	labels := fmt.Sprint(e.boatd.vms[child]["labels"])
	e.boatd.mu.Unlock()
	if labels != "map[a:b boat.secrets:S]" {
		t.Errorf("fork labels: %s", labels)
	}
	for _, bad := range []map[string]string{{secretsLabel: "x"}, {"bad key": "x"}} {
		if st, _, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/fork", map[string]any{"labels": bad}, nil); st != 400 {
			t.Errorf("fork labels %v: %d", bad, st)
		}
	}
	e.ok("POST", "/api/v1/sandboxes/"+bx+"/stop", map[string]any{}, 202)
	e.ok("POST", "/api/v1/sandboxes/"+bx+"/resume", map[string]any{"env": map[string]string{"RUN": "2"}, "type": "small"}, 202)
	vm := e.nativeOf(bx)
	e.boatd.mu.Lock()
	size = e.boatd.vms[vm]["size"]
	e.boatd.mu.Unlock()
	if size != "small" {
		t.Errorf("resume type: size %v", size)
	}
}

func TestRateLimitHeaders(t *testing.T) {
	e := setup(t)
	e.srv.limiter = newLimiter(1, 5)
	e.srv.teamLim = newLimiter(10, 3)
	st, _, h := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{}, nil)
	if st != 202 || h.Get("RateLimit-Policy") != `"key";q=5;w=5, "team";q=3;w=1` || !strings.HasPrefix(h.Get("RateLimit"), `"team";r=`) {
		t.Fatalf("create: %d policy %q limit %q", st, h.Get("RateLimit-Policy"), h.Get("RateLimit"))
	}
	if _, _, h := e.compat(e.owner, "GET", "/api/v1/sandboxes", nil, nil); h.Get("RateLimit") != "" {
		t.Error("list carries RateLimit (boat.dev declares it on create/fork/resume and 429)")
	}
	for i := 0; i < 10; i++ {
		st, _, h = e.compat(e.owner, "GET", "/api/v1/limits", nil, nil)
		if st == 429 {
			break
		}
	}
	if st != 429 || h.Get("RateLimit") == "" || h.Get("Retry-After") == "" {
		t.Errorf("429: %d %q %q", st, h.Get("RateLimit"), h.Get("Retry-After"))
	}
}
