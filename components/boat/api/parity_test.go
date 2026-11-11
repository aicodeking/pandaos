package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func list(m map[string]any, k string) []any {
	v, _ := m[k].([]any)
	return v
}

func TestDetachedCommands(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	base := "/v1/vms/" + id + "/commands"
	for _, bad := range []map[string]any{{}, {"cmd": []string{""}}, {"cmd": []string{"x"}, "cwd": "rel"}, {"cmd": []string{"x"}, "env": map[string]string{"a b": "c"}}} {
		if st, _ := e.do(e.owner, "POST", base, bad); st != 400 {
			t.Errorf("bad start %v: %d", bad, st)
		}
	}
	st, c := e.do(e.owner, "POST", base, map[string]any{"cmd": []string{"sleep", "100"}, "cwd": "/tmp", "env": map[string]string{"A": "b"}})
	if st != 201 || c["state"] != "running" {
		t.Fatalf("start: %d %v", st, c)
	}
	cid := c["id"].(string)
	if _, l := e.do(e.owner, "GET", base, nil); len(list(l, "commands")) != 1 {
		t.Errorf("list: %v", l)
	}
	if st, s := e.do(e.owner, "GET", base+"/"+cid, nil); st != 200 || s["state"] != "running" {
		t.Errorf("status: %d %v", st, s)
	}
	_, logs := e.do(e.owner, "GET", base+"/"+cid+"/logs?offset=0", nil)
	if b, _ := base64.StdEncoding.DecodeString(logs["data_b64"].(string)); string(b) != "started\n" || logs["running"] != true {
		t.Errorf("logs: %v", logs)
	}
	if st, _ := e.do(e.owner, "GET", base+"/"+cid+"/logs?offset=-1", nil); st != 400 {
		t.Errorf("bad offset: %d", st)
	}
	if st, _ := e.do(e.owner, "POST", base+"/"+cid+"/signal", map[string]any{"signal": "SEGV"}); st != 400 {
		t.Errorf("bad signal: %d", st)
	}
	// follow streams output, then ends when the command does
	done := make(chan string)
	go func() {
		_, b := e.raw(e.owner, "GET", base+"/"+cid+"/logs?follow=1", nil)
		done <- string(b)
	}()
	time.Sleep(100 * time.Millisecond)
	if st, s := e.do(e.owner, "POST", base+"/"+cid+"/signal", map[string]any{"signal": "SIGINT"}); st != 200 || s["signal"] != "INT" {
		t.Errorf("signal: %d %v", st, s)
	}
	select {
	case out := <-done:
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 2 || !strings.Contains(lines[0], "data_b64") || !strings.Contains(lines[1], `"exit"`) {
			t.Errorf("follow: %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not end after the command ended")
	}
	if _, s := e.do(e.owner, "GET", base+"/"+cid, nil); s["state"] != "signaled" {
		t.Errorf("after signal: %v", s)
	}
	_, c2 := e.do(e.owner, "POST", base, map[string]any{"cmd": []string{"sleep", "1"}})
	if st, _ := e.do(e.owner, "DELETE", base+"/"+c2["id"].(string), nil); st != 200 {
		t.Errorf("stop: %d", st)
	}

	// history is boat-api's own: it also answers for a stopped VM
	e.do(e.owner, "GET", "/v1/vms/"+id+"/command-history", nil) // refreshes from the guest
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	_, h := e.do(e.owner, "GET", "/v1/vms/"+id+"/command-history", nil)
	if len(list(h, "commands")) != 2 {
		t.Fatalf("history: %v", h)
	}
	if rec := list(h, "commands")[0].(map[string]any); rec["id"] != cid || rec["kind"] != "command" || rec["state"] != "signaled" {
		t.Errorf("history record: %v", rec)
	}
	if _, ok := list(h, "commands")[0].(map[string]any)["env"]; ok {
		t.Error("history must not keep env")
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil)

	_, other := e.newTeam("other")
	for _, try := range []struct{ m, p string }{{"GET", base}, {"GET", base + "/" + cid}, {"GET", base + "/" + cid + "/logs"}, {"DELETE", base + "/" + cid}, {"GET", "/v1/vms/" + id + "/command-history"}} {
		if st, _ := e.do(other, try.m, try.p, nil); st != 404 {
			t.Errorf("cross-team %s %s: %d", try.m, try.p, st)
		}
	}
	ro := e.newKey("vms:read")
	if st, _ := e.do(ro, "POST", base, map[string]any{"cmd": []string{"id"}}); st != 403 {
		t.Errorf("read-only key started a command: %d", st)
	}
	if st, p := e.do(e.owner, "GET", "/v1/vms/"+id+"/processes", nil); st != 200 || len(list(p, "processes")) != 1 {
		t.Errorf("processes: %d %v", st, p)
	}
}

func TestFileOps(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	f := "/v1/vms/" + id + "/files"
	e.boatd.mu.Lock()
	e.boatd.files[id+":/home/dev/a.txt"] = []byte("hello")
	e.boatd.mu.Unlock()
	st, l := e.do(e.owner, "GET", f+"/list?path=/home/dev", nil)
	if st != 200 || len(list(l, "entries")) != 1 {
		t.Fatalf("list: %d %v", st, l)
	}
	if st, _ := e.do(e.owner, "GET", f+"/list?path=rel", nil); st != 400 {
		t.Errorf("relative list: %d", st)
	}
	if st, m := e.do(e.owner, "POST", f+"/move", map[string]any{"from": "/home/dev/a.txt", "to": "/home/dev/b.txt"}); st != 200 || m["to"] != "/home/dev/b.txt" {
		t.Errorf("move: %d %v", st, m)
	}
	if st, _ := e.do(e.owner, "POST", f+"/move", map[string]any{"from": "rel", "to": "/x"}); st != 400 {
		t.Errorf("move relative: %d", st)
	}
	if st, _ := e.do(e.owner, "POST", f+"/move", map[string]any{"from": "/nope", "to": "/x"}); st != 404 {
		t.Errorf("move missing: %d", st)
	}
	if st, s := e.do(e.owner, "GET", f+"/search?path=/home&q=b.txt", nil); st != 200 || len(list(s, "matches")) != 1 {
		t.Errorf("search: %d %v", st, s)
	}
	if st, _ := e.do(e.owner, "GET", f+"/search?path=/home", nil); st != 400 {
		t.Errorf("search without q: %d", st)
	}
	if st, i := e.do(e.owner, "GET", f+"/info?path=/home/dev/b.txt", nil); st != 200 || i["type"] != "file" || i["size"] != float64(5) {
		t.Errorf("info: %d %v", st, i)
	}
	if st, _ := e.do(e.owner, "POST", f+"/folder", map[string]any{"path": "/home/dev/new"}); st != 200 {
		t.Errorf("folder: %d", st)
	}
	if st, _ := e.do(e.owner, "POST", f+"/permissions", map[string]any{"path": "/home/dev/b.txt", "mode": "600"}); st != 200 {
		t.Errorf("chmod: %d", st)
	}
	if st, _ := e.do(e.owner, "POST", f+"/permissions", map[string]any{"path": "/x", "mode": "u+x;rm"}); st != 400 {
		t.Errorf("bad mode: %d", st)
	}
	if st, _ := e.do(e.owner, "DELETE", f+"?path=/", nil); st != 400 {
		t.Errorf("delete /: %d", st)
	}
	if st, _ := e.do(e.owner, "DELETE", f+"?path=/home/dev/b.txt&recursive=1", nil); st != 200 {
		t.Errorf("delete: %d", st)
	}
	if _, ok := e.boatd.files[id+":/home/dev/b.txt"]; ok {
		t.Error("file not deleted")
	}
	_, other := e.newTeam("other")
	if st, _ := e.do(other, "DELETE", f+"?path=/etc/passwd", nil); st != 404 {
		t.Errorf("cross-team delete: %d", st)
	}
}

func lastCmdArgv(e *env) string {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	best := 0
	var out string
	for id, c := range e.boatd.cmds {
		var n int
		fmt.Sscanf(id, "c_%d", &n)
		if n > best {
			best, out = n, fmt.Sprint(c.argv...)
		}
	}
	return out
}

func TestAsyncPromptsClaude(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	p := "/v1/vms/" + id + "/prompts"
	if st, _ := e.do(e.owner, "POST", p, map[string]any{"prompt": " "}); st != 400 {
		t.Errorf("empty: %d", st)
	}
	if st, _ := e.do(e.owner, "POST", p, map[string]any{"prompt": "x", "agent": "gpt"}); st != 400 {
		t.Errorf("agent: %d", st)
	}
	st, run := e.do(e.owner, "POST", p, map[string]any{"prompt": "sleep-forever fix the bug"})
	if st != 202 || run["conversation_id"] == "" || run["state"] != "running" {
		t.Fatalf("start: %d %v", st, run)
	}
	argv := lastCmdArgv(e)
	if !strings.Contains(argv, "claude") || !strings.Contains(argv, "--session-id") {
		t.Errorf("first turn: %s", argv)
	}
	pid, conv := run["id"].(string), run["conversation_id"].(string)
	// the same conversation cannot run two turns at once
	if st, _ := e.do(e.owner, "POST", p, map[string]any{"prompt": "more"}); st != 409 {
		t.Errorf("second turn while running: %d", st)
	}
	// a new conversation can
	st, other := e.do(e.owner, "POST", p, map[string]any{"prompt": "sleep-forever other", "new": true})
	if st != 202 || other["conversation_id"] == conv {
		t.Fatalf("new: %d %v", st, other)
	}
	if st, g := e.do(e.owner, "GET", p+"/"+pid, nil); st != 200 || g["state"] != "running" {
		t.Errorf("status: %d %v", st, g)
	}
	if st, _ := e.do(e.owner, "GET", p+"/"+pid+"/logs", nil); st != 200 {
		t.Errorf("logs: %d", st)
	}

	// steer: interrupt + resume in the same session
	st, next := e.do(e.owner, "POST", p+"/"+pid+"/steer", map[string]any{"prompt": "skip the tests"})
	if st != 202 || next["conversation_id"] != conv || next["id"] == pid {
		t.Fatalf("steer: %d %v", st, next)
	}
	if argv := lastCmdArgv(e); !strings.Contains(argv, "--resume") || !strings.Contains(argv, "skip the tests") {
		t.Errorf("steer turn: %s", argv)
	}
	if _, g := e.do(e.owner, "GET", p+"/"+pid, nil); g["state"] != "steered" || g["steered_by"] != next["id"] {
		t.Errorf("steered turn: %v", g)
	}
	// the steered turn (stub: finishes at once) is done, so steering it is refused
	if st, _ := e.do(e.owner, "POST", p+"/"+next["id"].(string)+"/steer", map[string]any{"prompt": "x"}); st != 409 {
		t.Errorf("steer finished: %d", st)
	}

	// interrupt the other conversation
	if st, i := e.do(e.owner, "POST", p+"/"+other["id"].(string)+"/interrupt", nil); st != 200 || i["state"] != "interrupted" {
		t.Errorf("interrupt: %d %v", st, i)
	}
	if st, _ := e.do(e.owner, "POST", p+"/"+other["id"].(string)+"/interrupt", nil); st != 409 {
		t.Errorf("interrupt twice: %d", st)
	}

	// conversations list and resume by id
	_, cs := e.do(e.owner, "GET", "/v1/vms/"+id+"/conversations", nil)
	if len(list(cs, "conversations")) != 2 {
		t.Fatalf("conversations: %v", cs)
	}
	st, res := e.do(e.owner, "POST", p, map[string]any{"prompt": "continue", "conversation_id": conv})
	if st != 202 || res["conversation_id"] != conv {
		t.Fatalf("resume: %d %v", st, res)
	}
	if argv := lastCmdArgv(e); !strings.Contains(argv, "--resume") {
		t.Errorf("resume turn: %s", argv)
	}
	if st, _ := e.do(e.owner, "POST", p, map[string]any{"prompt": "x", "conversation_id": "conv_0000000000000000"}); st != 404 {
		t.Errorf("unknown conversation: %d", st)
	}
	_, l := e.do(e.owner, "GET", p, nil)
	if len(list(l, "prompts")) != 4 {
		t.Errorf("prompts: %v", l)
	}
	// a fork keeps the memory: same native session, its own thread
	_, ch := e.do(e.owner, "POST", "/v1/vms/"+id+"/fork", nil)
	_, cc := e.do(e.owner, "GET", "/v1/vms/"+ch["id"].(string)+"/conversations", nil)
	if len(list(cc, "conversations")) != 2 {
		t.Errorf("fork conversations: %v", cc)
	}
}

func TestAsyncPromptNativeSessions(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	p := "/v1/vms/" + id + "/prompts"
	for agent, sess := range map[string]string{"codex": "th_abc", "opencode": "ses_abc"} {
		_, run := e.do(e.owner, "POST", p, map[string]any{"prompt": "go", "agent": agent, "new": true})
		first := lastCmdArgv(e)
		if strings.Contains(first, sess) {
			t.Errorf("%s first turn already has a session: %s", agent, first)
		}
		if _, g := e.do(e.owner, "GET", p+"/"+run["id"].(string), nil); g["state"] != "completed" {
			t.Errorf("%s state: %v", agent, g)
		}
		st, _ := e.do(e.owner, "POST", p, map[string]any{"prompt": "again", "conversation_id": run["conversation_id"]})
		if argv := lastCmdArgv(e); st != 202 || !strings.Contains(argv, sess) {
			t.Errorf("%s resume: %d %s", agent, st, argv)
		}
	}
	_, f := e.do(e.owner, "POST", "/v1/vms/"+id+"/commands", map[string]any{"cmd": []string{"false"}})
	_, g := e.do(e.owner, "GET", "/v1/vms/"+id+"/commands/"+f["id"].(string), nil)
	if g["exit"] != float64(1) || g["state"] != "exited" {
		t.Errorf("failed command: %v", g)
	}
}

func TestEnvironments(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/A", map[string]any{"value": "sa"})
	e.do(e.owner, "PUT", "/v1/secrets/B", map[string]any{"value": "sb"})
	for _, bad := range []map[string]any{{}, {"name": "Bad Name"}, {"name": "x", "image": "nope"}, {"name": "x", "secrets": []string{"NOPE"}}, {"name": "x", "env": map[string]string{"a b": "c"}}} {
		if st, _ := e.do(e.owner, "POST", "/v1/environments", bad); st != 400 {
			t.Errorf("bad env %v: %d", bad, st)
		}
	}
	st, env := e.do(e.owner, "POST", "/v1/environments", map[string]any{"name": "base", "size": "large",
		"setup_script": "npm ci", "env": map[string]string{"FOO": "bar"}, "secrets": []string{"A"}})
	if st != 201 || env["version"] != float64(1) {
		t.Fatalf("create: %d %v", st, env)
	}
	eid := env["id"].(string)
	if st, _ := e.do(e.owner, "POST", "/v1/environments", map[string]any{"name": "base"}); st != 409 {
		t.Errorf("duplicate: %d", st)
	}
	if st, _ := e.do(e.owner, "POST", "/v1/vms", map[string]any{"environment": "typo"}); st != 400 {
		t.Errorf("unknown env must fail before create: %d", st)
	}
	if st, _ := e.do(e.owner, "POST", "/v1/vms", map[string]any{"environment": "base", "no_env": true}); st != 400 {
		t.Errorf("environment + no_env: %d", st)
	}

	st, vm := e.do(e.owner, "POST", "/v1/vms", map[string]any{"environment": "base"})
	if st != 201 || vm["environment"] != "base" || vm["size"] != "large" || vm["setup_cmd_id"] == nil {
		t.Fatalf("vm from env: %d %v", st, vm)
	}
	id := vm["id"].(string)
	got := envOf(e.boatd.vms[id])
	if got["FOO"] != "bar" || got["A"] != "sa" || got["B"] != nil {
		t.Errorf("env vars: %v", got)
	}
	if argv := lastCmdArgv(e); !strings.Contains(argv, "npm ci") || !strings.Contains(argv, "set -e") {
		t.Errorf("setup: %s", argv)
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil) // start keeps the selection
	if env := envOf(e.boatd.starts[len(e.boatd.starts)-1]); env["B"] != nil {
		t.Errorf("start of env VM leaks an unselected secret: %v", env)
	}

	// build, then new VMs start from the snapshot with no setup
	if st, b := e.do(e.owner, "POST", "/v1/environments/"+eid+"/build", nil); st != 202 || b["build"].(map[string]any)["state"] != "building" {
		t.Fatalf("build: %d %v", st, b)
	}
	waitFor(t, "build", func() bool {
		x := e.srv.store.Snapshot().Environments[eid]
		return x.Build != nil && x.Build.State != "building"
	})
	b := e.srv.store.Snapshot().Environments[eid].Build
	if b.State != "ready" || b.SnapshotID == "" {
		t.Fatalf("build result: %+v", b)
	}
	e.boatd.mu.Lock()
	builderState := e.boatd.vms[b.VMID]["state"]
	e.boatd.mu.Unlock()
	if builderState != "stopped" {
		t.Errorf("builder must stay stopped, got %v", builderState)
	}
	cmds := len(e.boatd.cmds)
	st, fast := e.do(e.owner, "POST", "/v1/vms", map[string]any{"environment": "base"})
	if st != 201 || fast["from_build"] != true || fast["setup_cmd_id"] != nil || fast["from_snapshot"] != b.SnapshotID || len(e.boatd.cmds) != cmds {
		t.Errorf("fast start: %d %v", st, fast)
	}

	// changing the script makes the build stale: back to running the setup
	st, up := e.do(e.owner, "PUT", "/v1/environments/"+eid, map[string]any{"setup_script": "make", "is_default": true})
	if st != 200 || up["version"] != float64(2) {
		t.Fatalf("update: %d %v", st, up)
	}
	st, stale := e.do(e.owner, "POST", "/v1/vms", map[string]any{}) // default env now
	if st != 201 || stale["from_build"] != false || stale["setup_cmd_id"] == nil || stale["environment"] != "base" {
		t.Errorf("stale build / default env: %d %v", st, stale)
	}
	if st, plain := e.do(e.owner, "POST", "/v1/vms", map[string]any{"no_env": true}); st != 201 || plain["environment"] != nil {
		t.Errorf("no_env: %d %v", st, plain)
	}

	// a failing setup is reported, the builder removed, the old snapshot kept
	e.do(e.owner, "PUT", "/v1/environments/"+eid, map[string]any{"setup_script": "exit 3"})
	e.do(e.owner, "POST", "/v1/environments/"+eid+"/build", nil)
	waitFor(t, "failed build", func() bool { return e.srv.store.Snapshot().Environments[eid].Build.State != "building" })
	fb := e.srv.store.Snapshot().Environments[eid].Build
	if fb.State != "failed" || !strings.Contains(fb.Error, "exited 3") || fb.SnapshotID != b.SnapshotID {
		t.Errorf("failed build: %+v", fb)
	}
	if st, g := e.do(e.owner, "GET", "/v1/environments/base", nil); st != 200 || g["id"] != eid { // by name too
		t.Errorf("get by name: %d %v", st, g)
	}
	if _, l := e.do(e.owner, "GET", "/v1/environments", nil); len(list(l, "environments")) != 1 {
		t.Errorf("list: %v", l)
	}
	_, other := e.newTeam("other")
	if st, _ := e.do(other, "GET", "/v1/environments/"+eid, nil); st != 404 {
		t.Errorf("cross-team env: %d", st)
	}
	if st, _ := e.do(other, "POST", "/v1/vms", map[string]any{"environment": "base"}); st != 400 {
		t.Errorf("cross-team env by name: %d", st)
	}
	if st, _ := e.do(e.owner, "DELETE", "/v1/environments/"+eid, nil); st != 200 {
		t.Errorf("delete: %d", st)
	}
	e.boatd.mu.Lock()
	_, kept := e.boatd.vms[b.VMID]
	e.boatd.mu.Unlock()
	if kept {
		t.Error("env delete left the builder VM")
	}
}

func TestResize(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/resize", map[string]any{}); st != 400 {
		t.Errorf("no size: %d", st)
	}
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/resize", map[string]any{"size": "large"}); st != 409 {
		t.Errorf("running VM: %d", st)
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/resize", map[string]any{"size": "large"}); st != 409 {
		t.Errorf("stop snapshot without discard_memory: %d", st)
	}
	if st, vm := e.do(e.owner, "POST", "/v1/vms/"+id+"/resize", map[string]any{"size": "large", "discard_memory": true, "disk_gib": 60}); st != 200 || vm["size"] != "large" {
		t.Errorf("resize: %d %v", st, vm)
	}
	_, other := e.newTeam("other")
	if st, _ := e.do(other, "POST", "/v1/vms/"+id+"/resize", map[string]any{"size": "small"}); st != 404 {
		t.Errorf("cross-team: %d", st)
	}
	if st, u := e.do(e.owner, "GET", "/v1/vms/"+id+"/usage", nil); st != 200 || u["id"] != id {
		t.Errorf("usage: %d %v", st, u)
	}
}

func TestKeyRotation(t *testing.T) {
	e := setup(t)
	old := e.newKey("vms:read")
	kid, _, _ := ParseToken(old)
	if st, _ := e.do(e.owner, "POST", "/v1/keys/"+kid+"/rotate", map[string]any{"overlap_s": -1}); st != 400 {
		t.Errorf("negative overlap: %d", st)
	}
	st, k := e.do(e.owner, "POST", "/v1/keys/"+kid+"/rotate", map[string]any{"overlap_s": 3600})
	if st != 200 || k["id"] != kid || k["prev_until"] == nil {
		t.Fatalf("rotate: %d %v", st, k)
	}
	fresh := k["token"].(string)
	if fresh == old {
		t.Fatal("rotation kept the secret")
	}
	if st, _ := e.do(old, "GET", "/v1/me", nil); st != 200 {
		t.Errorf("old secret during overlap: %d", st)
	}
	if st, _ := e.do(fresh, "GET", "/v1/me", nil); st != 200 {
		t.Errorf("new secret: %d", st)
	}
	if st, _ := e.do(fresh, "POST", "/v1/vms", map[string]any{}); st != 403 {
		t.Errorf("rotation must keep the scopes: %d", st)
	}
	// past the window the old secret is dead
	past := time.Now().Add(-time.Second)
	e.srv.store.Update(func(st *State) error { st.Keys[kid].PrevUntil = &past; return nil })
	if st, _ := e.do(old, "GET", "/v1/me", nil); st != 401 {
		t.Errorf("old secret after overlap: %d", st)
	}
	// overlap 0 revokes at once
	_, k2 := e.do(e.owner, "POST", "/v1/keys/"+kid+"/rotate", map[string]any{"overlap_s": 0})
	if st, _ := e.do(fresh, "GET", "/v1/me", nil); st != 401 {
		t.Errorf("overlap 0: %d", st)
	}
	if st, _ := e.do(k2["token"].(string), "GET", "/v1/me", nil); st != 200 {
		t.Errorf("new after overlap 0: %d", st)
	}
	_, other := e.newTeam("other")
	if st, _ := e.do(other, "POST", "/v1/keys/"+kid+"/rotate", nil); st != 404 {
		t.Errorf("cross-team rotate: %d", st)
	}
	if st, u := e.do(k2["token"].(string), "GET", "/v1/keys/"+kid+"/usage", nil); st != 200 || u["requests"].(float64) < 1 {
		t.Errorf("key usage: %d %v", st, u)
	}
	raw, _ := os.ReadFile(filepath.Join(e.dir, "state.json"))
	if strings.Contains(string(raw), strings.Split(k2["token"].(string), "_")[2]) {
		t.Error("state.json holds the raw new secret")
	}
}

func TestWebhookRotationAndUpdate(t *testing.T) {
	e := setup(t)
	target := &hookTarget{}
	ts := httptest.NewServer(target)
	defer ts.Close()
	_, h := e.do(e.owner, "POST", "/v1/webhooks", map[string]any{"url": ts.URL})
	id, oldSecret := h["id"].(string), h["secret"].(string)
	st, r := e.do(e.owner, "POST", "/v1/webhooks/"+id+"/rotate", map[string]any{"overlap_s": 3600})
	if st != 200 || r["secret"] == oldSecret || r["prev_until"] == nil {
		t.Fatalf("rotate: %d %v", st, r)
	}
	newSecret := r["secret"].(string)
	e.do(e.owner, "POST", "/v1/webhooks/"+id+"/test", nil)
	waitFor(t, "delivery", func() bool { return target.count() == 1 })
	sig := target.got[0].Header.Get("X-Boat-Signature")
	var ts0 int64
	fmt.Sscanf(sig, "t=%d,", &ts0)
	if sig != SignWebhook([]byte(newSecret), ts0, []byte(target.body[0]), []byte(oldSecret)) {
		t.Errorf("during overlap both secrets must sign: %s", sig)
	}
	past := time.Now().Add(-time.Second)
	e.srv.store.Update(func(st *State) error { st.Webhooks[id].PrevUntil = &past; return nil })
	e.do(e.owner, "POST", "/v1/webhooks/"+id+"/test", nil)
	waitFor(t, "second delivery", func() bool { return target.count() == 2 })
	if strings.Count(target.got[1].Header.Get("X-Boat-Signature"), "v1=") != 1 {
		t.Errorf("after overlap only the new secret signs: %s", target.got[1].Header.Get("X-Boat-Signature"))
	}
	if st, g := e.do(e.owner, "GET", "/v1/webhooks/"+id, nil); st != 200 || g["secret"] != nil {
		t.Errorf("get: %d %v", st, g)
	}
	if st, u := e.do(e.owner, "PATCH", "/v1/webhooks/"+id, map[string]any{"events": []string{"vm.*"}}); st != 200 || len(list(u, "events")) != 1 {
		t.Errorf("patch: %d %v", st, u)
	}
	if st, _ := e.do(e.owner, "PATCH", "/v1/webhooks/"+id, map[string]any{"url": "ftp://x"}); st != 400 {
		t.Errorf("patch bad url: %d", st)
	}
}

// N7: a retry waiting out its backoff must not hold a delivery slot, and one
// team's stuck endpoint must not use up another team's.
func TestWebhookSlotsReleasedWhileSleeping(t *testing.T) {
	e := setup(t)
	var ok atomic.Int32
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ok.Add(1); w.WriteHeader(204) }))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	d := e.srv.hooks
	d.sem = make(chan struct{}, 1) // one global slot
	d.backoff = []time.Duration{time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, hb := e.do(e.owner, "POST", "/v1/webhooks", map[string]any{"url": bad.URL})
	_, hg := e.do(e.owner, "POST", "/v1/webhooks", map[string]any{"url": good.URL})
	snap := e.srv.store.Snapshot()
	ev := Event{ID: "e1", Type: "x", TeamID: "t"}
	var wg sync.WaitGroup
	deliver := func(h Webhook) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.deliver(ctx, h, ev)
		}()
	}
	deliver(*snap.Webhooks[hb["id"].(string)])
	waitFor(t, "first attempt failed", func() bool { return len(e.srv.store.Snapshot().Deliveries[hb["id"].(string)]) == 1 })
	// the bad delivery now sleeps an hour; the only slot must be free
	deliver(*snap.Webhooks[hg["id"].(string)])
	waitFor(t, "second delivery with the slot", func() bool { return ok.Load() == 1 })
	cancel() // wake the sleeper; both must finish before the temp dir goes
	wg.Wait()
}

func TestWebhookPerTeamLimits(t *testing.T) {
	e := setup(t)
	var inflight, peak atomic.Int32
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inflight.Add(-1)
		w.WriteHeader(204)
	}))
	defer ts.Close()
	d := e.srv.hooks
	d.teamInflight, d.teamPending = 2, 4
	_, h := e.do(e.owner, "POST", "/v1/webhooks", map[string]any{"url": ts.URL})
	id := h["id"].(string)
	hook := *e.srv.store.Snapshot().Webhooks[id]
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.deliver(context.Background(), hook, Event{ID: "e", Type: "x", TeamID: hook.TeamID})
		}()
	}
	waitFor(t, "over-limit deliveries dropped", func() bool {
		dropped := 0
		for _, x := range e.srv.store.Snapshot().Deliveries[id] {
			if strings.HasPrefix(x.Error, "dropped") {
				dropped++
			}
		}
		return dropped == 2
	})
	waitFor(t, "two attempts in flight", func() bool { return peak.Load() >= 2 })
	time.Sleep(50 * time.Millisecond) // room for a third to break the limit
	if peak.Load() != 2 {
		t.Errorf("in-flight per team: %d, want 2", peak.Load())
	}
	close(release)
	wg.Wait()
}

func (e *env) snapshotOf(id string) string {
	_, sn := e.do(e.owner, "POST", "/v1/vms/"+id+"/snapshots", map[string]any{"name": "s"})
	return sn["id"].(string)
}

func TestSnapshotDownloadImportShare(t *testing.T) {
	e := setup(t)
	e.srv.cfg.PublicURL = "https://api.boat.example"
	id := e.createVM()
	sid := e.snapshotOf(id)
	base := "/v1/vms/" + id + "/snapshots/" + sid
	st, b := e.raw(e.owner, "GET", base+"/download", nil)
	if st != 200 || string(b) != "ARCHIVE:"+sid {
		t.Fatalf("download: %d %q", st, b)
	}
	if st, _ := e.raw(e.owner, "GET", "/v1/vms/"+id+"/snapshots/nope/download", nil); st != 404 {
		t.Errorf("other snapshot: %d", st)
	}
	tid, other := e.newTeam("other")
	if st, _ := e.raw(other, "GET", base+"/download", nil); st != 404 {
		t.Errorf("cross-team download: %d", st)
	}

	// import: raw body into the caller's team
	req, _ := http.NewRequest("POST", e.api.URL+"/v1/snapshots/import?name=imp", strings.NewReader("ARCHIVE:"+sid))
	req.Header.Set("Authorization", "Bearer "+other)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var imp map[string]any
	json.NewDecoder(resp.Body).Decode(&imp)
	resp.Body.Close()
	if resp.StatusCode != 201 || imp["team"] != tid {
		t.Fatalf("import: %d %v", resp.StatusCode, imp)
	}
	if st, _ := e.do(other, "POST", "/v1/vms", map[string]any{"snapshot": imp["id"]}); st != 201 {
		t.Errorf("create from imported: %d", st)
	}

	// grant another team
	if st, _ := e.do(e.owner, "POST", base+"/shares", map[string]any{"team_id": "team_nope"}); st != 404 {
		t.Errorf("share to unknown team: %d", st)
	}
	if st, _ := e.do(e.owner, "POST", base+"/shares", map[string]any{"ttl_s": -1}); st != 400 {
		t.Errorf("bad ttl: %d", st)
	}
	st, g := e.do(e.owner, "POST", base+"/shares", map[string]any{"team_id": tid})
	if st != 201 || g["grantee"] != tid || g["code"] != nil {
		t.Fatalf("grant: %d %v", st, g)
	}
	gid := g["id"].(string)
	_, third := e.newTeam("third")
	if st, _ := e.do(third, "POST", "/v1/snapshots/import-share", map[string]any{"share_id": gid}); st != 404 {
		t.Errorf("grant taken by a third team: %d", st)
	}
	if _, l := e.do(other, "GET", "/v1/shared-snapshots", nil); len(list(l, "shares")) != 1 {
		t.Errorf("shared with me: %v", l)
	}
	if _, l := e.do(third, "GET", "/v1/shared-snapshots", nil); len(list(l, "shares")) != 0 {
		t.Errorf("third team sees the share: %v", l)
	}
	if _, l := e.do(other, "GET", "/v1/snapshots", nil); len(list(l, "snapshots")) != 1 {
		t.Errorf("team snapshots: %v", l)
	}
	if st, _ := e.do(third, "DELETE", "/v1/snapshots/"+imp["id"].(string), nil); st != 404 {
		t.Errorf("cross-team snapshot delete: %d", st)
	}
	if st, _ := e.do(other, "DELETE", "/v1/snapshots/"+imp["id"].(string), nil); st != 200 {
		t.Errorf("delete imported snapshot: %d", st)
	}
	st, cp := e.do(other, "POST", "/v1/snapshots/import-share", map[string]any{"share_id": gid, "name": "copy"})
	if st != 201 || cp["team"] != tid || cp["name"] != "copy" {
		t.Fatalf("take grant: %d %v", st, cp)
	}

	// signed link
	st, ln := e.do(e.owner, "POST", base+"/shares", map[string]any{"ttl_s": 600})
	if st != 201 || !strings.HasPrefix(ln["code"].(string), "bsh_") || !strings.HasPrefix(ln["url"].(string), "https://api.boat.example/v1/snapshot-links/") {
		t.Fatalf("link: %d %v", st, ln)
	}
	code := ln["code"].(string)
	e.audit.Reset()
	if st, b := e.raw("", "GET", "/v1/snapshot-links/"+code+"/download", nil); st != 200 || string(b) != "ARCHIVE:"+sid {
		t.Errorf("link download: %d %q", st, b)
	}
	if strings.Contains(e.audit.String(), code) || !strings.Contains(e.audit.String(), "REDACTED") {
		t.Errorf("audit must log the download without the code: %s", e.audit.String())
	}
	if st, _ := e.raw("", "GET", "/v1/snapshot-links/"+code+"x/download", nil); st != 404 {
		t.Errorf("wrong code: %d", st)
	}
	st, fromCode := e.do(third, "POST", "/v1/vms", map[string]any{"from": code})
	if st != 201 || fromCode["team"] != e.teamOf(third) {
		t.Errorf("create from code: %d %v", st, fromCode)
	}
	if st, _ := e.do(third, "POST", "/v1/snapshots/import-share", map[string]any{"code": code}); st != 201 {
		t.Errorf("import by code: %d", st)
	}
	if st, _ := e.do(third, "POST", "/v1/snapshots/import-share", map[string]any{}); st != 400 {
		t.Errorf("neither id nor code: %d", st)
	}
	_, sl := e.do(e.owner, "GET", base+"/shares", nil)
	if len(list(sl, "shares")) != 2 {
		t.Fatalf("shares: %v", sl)
	}
	for _, x := range list(sl, "shares") {
		if x.(map[string]any)["code"] != nil {
			t.Error("share list leaks the code")
		}
	}
	raw, _ := os.ReadFile(filepath.Join(e.dir, "state.json"))
	if strings.Contains(string(raw), strings.Split(code, "_")[2]) {
		t.Error("state.json holds the raw share secret")
	}
	// expiry and revoke
	past := time.Now().Add(-time.Second)
	e.srv.store.Update(func(st *State) error { st.SnapShares[ln["id"].(string)].Expires = &past; return nil })
	if st, _ := e.raw("", "GET", "/v1/snapshot-links/"+code+"/download", nil); st != 404 {
		t.Errorf("expired link: %d", st)
	}
	if st, _ := e.do(other, "DELETE", base+"/shares/"+gid, nil); st != 404 {
		t.Errorf("grantee revoked: %d", st)
	}
	if st, _ := e.do(e.owner, "DELETE", base+"/shares/"+gid, nil); st != 200 {
		t.Errorf("revoke: %d", st)
	}
	if st, _ := e.do(other, "POST", "/v1/snapshots/import-share", map[string]any{"share_id": gid}); st != 404 {
		t.Errorf("revoked grant: %d", st)
	}
	// a read-only key may not share
	ro := e.newKey("vms:read")
	if st, _ := e.do(ro, "POST", base+"/shares", map[string]any{}); st != 403 {
		t.Errorf("read-only share: %d", st)
	}
}

func (e *env) teamOf(tok string) string {
	_, me := e.do(tok, "GET", "/v1/me", nil)
	return me["team"].(map[string]any)["id"].(string)
}

// aiEnvSetup is a stack with the AI proxy on, a fake Anthropic and OpenAI.
type aiFake struct {
	mu      sync.Mutex
	headers []http.Header
	paths   []string
	bodies  []string
	reply   func(w http.ResponseWriter, r *http.Request)
}

func (f *aiFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.headers = append(f.headers, r.Header.Clone())
	f.paths = append(f.paths, r.URL.RequestURI())
	f.bodies = append(f.bodies, string(b))
	reply := f.reply
	f.mu.Unlock()
	reply(w, r)
}

func aiSetup(t *testing.T) (*env, *aiFake, *httptest.Server) {
	e := setup(t)
	up := &aiFake{reply: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "tracking=1")
		io.WriteString(w, `{"model":"claude-x","usage":{"input_tokens":10,"output_tokens":5}}`)
	}}
	ups := httptest.NewServer(up)
	t.Cleanup(ups.Close)
	proxy := httptest.NewServer(e.srv.aiHandler())
	t.Cleanup(proxy.Close)
	e.srv.cfg.AIProxyURL = proxy.URL
	e.srv.cfg.AIUpstreams = map[string]string{"anthropic": ups.URL, "openai": ups.URL}
	return e, up, proxy
}

func aiCall(t *testing.T, base, path, hdr, tok string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+path, strings.NewReader(`{"messages":[]}`))
	if hdr == "x-api-key" {
		req.Header.Set("X-Api-Key", tok)
	} else {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Cookie", "secret=1")
	req.Header.Set("X-Forwarded-For", "10.200.0.5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestAIProxy(t *testing.T) {
	e, up, proxy := aiSetup(t)
	const realKey = "sk-ant-REAL-KEY-123456"
	if st, _ := e.do(e.owner, "PUT", "/v1/ai/credentials/nope", map[string]any{"value": realKey}); st != 400 {
		t.Errorf("unknown provider: %d", st)
	}
	if st, _ := e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "x"}); st != 400 {
		t.Errorf("short key: %d", st)
	}
	st, c := e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": realKey})
	if st != 200 || c["last4"] != "3456" {
		t.Fatalf("set: %d %v", st, c)
	}
	_, l := e.raw(e.owner, "GET", "/v1/ai/credentials", nil)
	if strings.Contains(string(l), realKey) || !strings.Contains(string(l), "anthropic") {
		t.Errorf("list: %s", l)
	}
	raw, _ := os.ReadFile(filepath.Join(e.dir, "state.json"))
	if strings.Contains(string(raw), realKey) {
		t.Error("state.json holds the real key")
	}

	// the VM's env holds a token and the proxy URL, never the key
	id := e.createVM()
	if len(e.boatd.envOps) != 1 {
		t.Fatalf("env not pushed after create: %v", e.boatd.envOps)
	}
	vars := e.boatd.envOps[0]["vars"].(map[string]any)
	tok, _ := vars["ANTHROPIC_API_KEY"].(string)
	if !strings.HasPrefix(tok, "boatvm_"+id+"_") || vars["ANTHROPIC_BASE_URL"] != proxy.URL+"/anthropic" {
		t.Fatalf("vm env: %v", vars)
	}
	everything, _ := json.Marshal(e.boatd.envOps)
	if strings.Contains(string(everything), realKey) {
		t.Fatal("real key reached the VM")
	}

	// prompt goes through the same token; sync and async
	e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "hi"})
	last := e.boatd.execs[len(e.boatd.execs)-1]
	if envOf(last)["ANTHROPIC_API_KEY"] != tok {
		t.Errorf("sync prompt env: %v", envOf(last))
	}
	st, run := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": "hi"})
	if st != 202 {
		t.Fatalf("async: %d %v", st, run)
	}
	cmdEnv := e.boatd.cmds[run["cmd_id"].(string)].env
	if cmdEnv["ANTHROPIC_API_KEY"] != tok || strings.Contains(fmt.Sprint(cmdEnv), realKey) {
		t.Errorf("async prompt env: %v", cmdEnv)
	}

	// the proxy swaps the token for the real key
	resp, body := aiCall(t, proxy.URL, "/anthropic/v1/messages?beta=1", "x-api-key", tok)
	if resp.StatusCode != 200 || !strings.Contains(body, "claude-x") {
		t.Fatalf("proxied: %d %s", resp.StatusCode, body)
	}
	h := up.headers[0]
	if h.Get("X-Api-Key") != realKey || h.Get("Authorization") != "" || h.Get("Cookie") != "" || h.Get("X-Forwarded-For") != "" {
		t.Errorf("upstream headers: %v", h)
	}
	if h.Get("Anthropic-Version") == "" || up.paths[0] != "/v1/messages?beta=1" || up.bodies[0] != `{"messages":[]}` {
		t.Errorf("request not forwarded: %v %v %v", h, up.paths[0], up.bodies[0])
	}
	if resp.Header.Get("Set-Cookie") != "" {
		t.Error("upstream Set-Cookie reached the VM")
	}

	// bad tokens, a real-looking key, a token for another provider's missing key
	for _, bad := range []string{"", "nope", realKey, "boatvm_" + id + "_" + strings.Repeat("0", 64)} {
		if resp, _ := aiCall(t, proxy.URL, "/anthropic/v1/messages", "x-api-key", bad); resp.StatusCode != 401 {
			t.Errorf("token %q: %d", bad, resp.StatusCode)
		}
	}
	if resp, _ := aiCall(t, proxy.URL, "/openai/v1/chat/completions", "bearer", tok); resp.StatusCode != 404 {
		t.Errorf("no openai credential: %d", resp.StatusCode)
	}
	if resp, _ := aiCall(t, proxy.URL, "/evil.example/x", "bearer", tok); resp.StatusCode != 404 {
		t.Errorf("unknown provider path: %d", resp.StatusCode)
	}

	// the proxy listener serves nothing of the public API
	if resp, _ := aiCall(t, proxy.URL, "/v1/me", "bearer", e.owner); resp.StatusCode == 200 {
		t.Error("public API reachable through the proxy port")
	}

	// usage is metered per VM
	st, u := e.do(e.owner, "GET", "/v1/vms/"+id+"/ai-usage", nil)
	totals := u["totals"].(map[string]any)
	if st != 200 || totals["input_tokens"] != float64(10) || totals["output_tokens"] != float64(5) || totals["requests"] != float64(1) {
		t.Errorf("vm usage: %d %v", st, u)
	}
	if _, tu := e.do(e.owner, "GET", "/v1/ai/usage", nil); list(tu, "usage")[0].(map[string]any)["model"] != "claude-x" {
		t.Errorf("team usage: %v", tu)
	}

	// block and unblock, at once
	e.do(e.owner, "PUT", "/v1/vms/"+id+"/ai-access", map[string]any{"enabled": false})
	if resp, _ := aiCall(t, proxy.URL, "/anthropic/v1/messages", "x-api-key", tok); resp.StatusCode != 403 {
		t.Errorf("blocked: %d", resp.StatusCode)
	}
	e.do(e.owner, "PUT", "/v1/vms/"+id+"/ai-access", map[string]any{"enabled": true})
	if resp, _ := aiCall(t, proxy.URL, "/anthropic/v1/messages", "x-api-key", tok); resp.StatusCode != 200 {
		t.Errorf("unblocked: %d", resp.StatusCode)
	}

	// a stop kills the token (the event covers TTL and idle stops as well)
	e.srv.hooks.handleRaw(context.Background(), "vm.state", `{"type":"vm.state","vm_id":"`+id+`","team":"`+e.teamID()+`","state":"stopped"}`)
	if resp, _ := aiCall(t, proxy.URL, "/anthropic/v1/messages", "x-api-key", tok); resp.StatusCode != 401 {
		t.Errorf("token after stop: %d", resp.StatusCode)
	}
	// start gives a new one, which works
	e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil)
	newTok, _ := envOf(e.boatd.starts[len(e.boatd.starts)-1])["ANTHROPIC_API_KEY"].(string)
	if newTok == tok || !strings.HasPrefix(newTok, "boatvm_") {
		t.Fatalf("start token: %q", newTok)
	}
	if resp, _ := aiCall(t, proxy.URL, "/anthropic/v1/messages", "x-api-key", newTok); resp.StatusCode != 200 {
		t.Errorf("new token: %d", resp.StatusCode)
	}

	// a fork gets its own token, pushed to its env
	before := len(e.boatd.envOps)
	_, ch := e.do(e.owner, "POST", "/v1/vms/"+id+"/fork", nil)
	if len(e.boatd.envOps) != before+1 {
		t.Fatal("fork did not get its own env")
	}
	forkTok := e.boatd.envOps[before]["vars"].(map[string]any)["ANTHROPIC_API_KEY"].(string)
	if forkTok == newTok || !strings.Contains(forkTok, ch["id"].(string)) {
		t.Errorf("fork token: %s", forkTok)
	}

	// another team's VM cannot use this team's key; it has no credential
	_, other := e.newTeam("other")
	st, ov := e.do(other, "POST", "/v1/vms", map[string]any{})
	if st != 201 {
		t.Fatal(st)
	}
	if _, ok := envOf(e.boatd.vms[ov["id"].(string)])["ANTHROPIC_API_KEY"]; ok {
		t.Error("another team's VM got the credential")
	}
	if st, _ := e.do(other, "PUT", "/v1/vms/"+id+"/ai-access", map[string]any{"enabled": false}); st != 404 {
		t.Errorf("cross-team block: %d", st)
	}
	if st, _ := e.do(other, "GET", "/v1/vms/"+id+"/ai-usage", nil); st != 404 {
		t.Errorf("cross-team usage: %d", st)
	}

	// proxy off: the operator's explicit choice to inject raw keys
	if st, p := e.do(e.owner, "GET", "/v1/ai/proxy", nil); st != 200 || p["enabled"] != true || p["available"] != true {
		t.Errorf("proxy state: %d %v", st, p)
	}
	e.do(e.owner, "PUT", "/v1/ai/proxy", map[string]any{"enabled": false})
	e.do(e.owner, "POST", "/v1/vms", map[string]any{})
	if v := e.boatd.envOps[len(e.boatd.envOps)-1]["vars"].(map[string]any); v["ANTHROPIC_API_KEY"] != realKey {
		t.Errorf("proxy off env: %v", v)
	}
	if st, _ := e.do(e.owner, "DELETE", "/v1/ai/credentials/anthropic", nil); st != 200 {
		t.Errorf("delete credential: %d", st)
	}
}

func TestUsageTap(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-y\",\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":40}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":70}}\n\ndata: [DONE]\n"
	for _, chunk := range []int{1, 7, len(sse)} { // token counts must not depend on read sizes
		tap := &usageTap{}
		for i := 0; i < len(sse); i += chunk {
			tap.Write([]byte(sse[i:min(i+chunk, len(sse))]))
		}
		if in, out, model := tap.result(); in != 100 || out != 70 || model != "claude-y" {
			t.Errorf("chunk %d: in=%d out=%d model=%s", chunk, in, out, model)
		}
	}
	oai := &usageTap{}
	oai.Write([]byte(`{"model":"gpt-x","usage":{"prompt_tokens":3,"completion_tokens":4}}`))
	if in, out, model := oai.result(); in != 3 || out != 4 || model != "gpt-x" {
		t.Errorf("openai: %d %d %s", in, out, model)
	}
	resp := &usageTap{}
	resp.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5\",\"usage\":{\"input_tokens\":8,\"output_tokens\":9}}}\n"))
	if in, out, _ := resp.result(); in != 8 || out != 9 {
		t.Errorf("responses api: %d %d", in, out)
	}
	junk := &usageTap{}
	junk.Write([]byte("<html>not json"))
	if in, out, _ := junk.result(); in != 0 || out != 0 {
		t.Errorf("junk: %d %d", in, out)
	}
}

// collector records OTLP posts.
type collector struct {
	mu     sync.Mutex
	posts  map[string][]string
	header http.Header
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.posts == nil {
		c.posts = map[string][]string{}
	}
	c.posts[r.URL.Path] = append(c.posts[r.URL.Path], string(b))
	c.header = r.Header.Clone()
}

func (c *collector) got(path string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.posts[path], "\n")
}

func TestOpenTelemetry(t *testing.T) {
	e := setup(t)
	col := &collector{}
	ts := httptest.NewServer(col)
	defer ts.Close()
	otel := newOTel(ts.URL, map[string]string{"X-Vendor": "v"})
	e.srv.otel = otel

	trace := strings.Repeat("a", 32)
	req, _ := http.NewRequest("GET", e.api.URL+"/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+e.owner)
	req.Header.Set("Traceparent", "00-"+trace+"-"+strings.Repeat("b", 16)+"-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	id := e.createVM()
	e.srv.hooks.handleRaw(context.Background(), "vm.state", `{"type":"vm.state","vm_id":"`+id+`","team":"`+e.teamID()+`","state":"running"}`)
	// A span is queued after its response is written, so the client can get ahead of it.
	waitFor(t, "two spans and a log queued", func() bool { return len(otel.spans) >= 2 && len(otel.logs) >= 1 })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { otel.run(ctx); close(done) }()
	// run only flushes what it has taken off the queues when ctx ends (see its select).
	waitFor(t, "queues drained", func() bool { return len(otel.spans) == 0 && len(otel.logs) == 0 })
	cancel()
	<-done // run flushes on the way out

	traces := col.got("/v1/traces")
	var doc struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID, SpanID, ParentSpanID, Name string
					StartTimeUnixNano                   string
				}
			}
		}
	}
	first := strings.SplitN(traces, "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &doc); err != nil || len(doc.ResourceSpans) == 0 {
		t.Fatalf("traces: %v %s", err, traces)
	}
	found := false
	for _, sp := range doc.ResourceSpans[0].ScopeSpans[0].Spans {
		if sp.Name == "GET /v1/me" && sp.TraceID == trace && sp.ParentSpanID == strings.Repeat("b", 16) && len(sp.SpanID) == 16 && sp.StartTimeUnixNano != "" {
			found = true
		}
	}
	if !found {
		t.Errorf("span with continued trace missing: %s", traces)
	}
	if !strings.Contains(traces, `"POST /v1/vms"`) || strings.Contains(traces, e.owner) {
		t.Errorf("traces content: %s", traces)
	}
	logs := col.got("/v1/logs")
	if !strings.Contains(logs, `vm.state`) || !strings.Contains(logs, id) {
		t.Errorf("lifecycle logs: %s", logs)
	}
	if col.header.Get("X-Vendor") != "v" || col.header.Get("Content-Type") != "application/json" {
		t.Errorf("headers: %v", col.header)
	}
	if a, b := traceIDs("garbage"); len(a) != 32 || b != "" {
		t.Errorf("new trace: %q %q", a, b)
	}
}

func TestTeamTelemetry(t *testing.T) {
	e := setup(t)
	col := &collector{}
	ts := httptest.NewServer(col)
	defer ts.Close()
	if st, g := e.do(e.owner, "GET", "/v1/telemetry", nil); st != 200 || g["configured"] != false {
		t.Errorf("unset: %d %v", st, g)
	}
	if st, _ := e.do(e.owner, "PUT", "/v1/telemetry", map[string]any{"endpoint": "ftp://x"}); st != 400 {
		t.Errorf("bad endpoint: %d", st)
	}
	st, v := e.do(e.owner, "PUT", "/v1/telemetry", map[string]any{"endpoint": ts.URL + "/v1/logs", "headers": map[string]string{"X-Key": "vendor-secret"}})
	if st != 200 || v["endpoint"] != ts.URL || list(v, "header_names")[0] != "X-Key" {
		t.Fatalf("set: %d %v", st, v)
	}
	if _, g := e.raw(e.owner, "GET", "/v1/telemetry", nil); strings.Contains(string(g), "vendor-secret") {
		t.Error("read returns the header value")
	}
	raw, _ := os.ReadFile(filepath.Join(e.dir, "state.json"))
	if strings.Contains(string(raw), "vendor-secret") {
		t.Error("state.json holds the header value")
	}
	team := e.teamID()
	e.srv.emitVMEvent(context.Background(), "vm.state", team, "vm9", "running")
	e.srv.emitVMEvent(context.Background(), "vm.state", "someone-else", "vm8", "running")
	waitFor(t, "team log export", func() bool { return strings.Contains(col.got("/v1/logs"), "vm9") })
	if strings.Contains(col.got("/v1/logs"), "vm8") {
		t.Error("another team's event was exported")
	}
	col.mu.Lock()
	if col.header.Get("X-Key") != "vendor-secret" {
		t.Errorf("header not sent: %v", col.header)
	}
	col.mu.Unlock()
	if st, _ := e.do(e.owner, "DELETE", "/v1/telemetry", nil); st != 200 {
		t.Errorf("delete: %d", st)
	}
}

func TestEventsStreamTeamFiltered(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.api.URL+"/v1/events?after=3", nil)
	req.Header.Set("Authorization", "Bearer "+e.owner)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events: %d %v", resp.StatusCode, resp.Header)
	}
	e.boatd.events <- `{"type":"vm.state"}`
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "vm.state") {
		t.Errorf("event: %q", buf[:n])
	}
	cancel() // end the stream so its exchange is recorded before the test ends
	resp.Body.Close()
	if st, _ := e.do(e.owner, "GET", "/v1/events?after=x", nil); st != 400 {
		t.Errorf("bad after: %d", st)
	}
}

// The older routes that no other test drove to a 2xx (the drift check needs each).
func TestRemainingRoutes(t *testing.T) {
	e := setup(t)
	tid := e.teamID()
	if _, l := e.do(e.owner, "GET", "/v1/teams", nil); len(list(l, "teams")) != 1 {
		t.Errorf("teams: %v", l)
	}
	if st, g := e.do(e.owner, "GET", "/v1/teams/"+tid, nil); st != 200 || len(list(g, "members")) != 1 {
		t.Errorf("team: %d %v", st, g)
	}
	_, m := e.do(e.owner, "POST", "/v1/teams/"+tid+"/members", map[string]any{"name": "bob"})
	uid := m["user"].(map[string]any)["id"].(string)
	if st, _ := e.do(e.owner, "DELETE", "/v1/teams/"+tid+"/members/"+uid, nil); st != 200 {
		t.Errorf("remove member: %d", st)
	}
	id := e.createVM()
	if st, vm := e.do(e.owner, "GET", "/v1/vms/"+id, nil); st != 200 || vm["id"] != id {
		t.Errorf("get vm: %d %v", st, vm)
	}
	sid := e.snapshotOf(id)
	if _, l := e.do(e.owner, "GET", "/v1/vms/"+id+"/snapshots", nil); len(list(l, "snapshots")) != 1 {
		t.Errorf("snapshots: %v", l)
	}
	if st, _ := e.do(e.owner, "DELETE", "/v1/vms/"+id+"/snapshots/"+sid, nil); st != 200 {
		t.Errorf("delete snapshot: %d", st)
	}
	_, h := e.do(e.owner, "POST", "/v1/webhooks", map[string]any{"url": "https://h.example/x"})
	if st, _ := e.do(e.owner, "DELETE", "/v1/webhooks/"+h["id"].(string), nil); st != 200 {
		t.Errorf("delete webhook: %d", st)
	}
}

// The proxy forwards only each provider's inference endpoints (aiAllowed): a
// VM token must not reach a provider's file, fine-tuning, batch, admin or
// organization APIs with the team's real key.
func TestAIProxyAllowlist(t *testing.T) {
	e, up, proxy := aiSetup(t)
	e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "sk-ant-REAL-KEY-123456"})
	e.do(e.owner, "PUT", "/v1/ai/credentials/openai", map[string]any{"value": "sk-REAL-OPENAI-KEY-1234"})
	id := e.createVM()
	vars := e.boatd.envOps[len(e.boatd.envOps)-1]["vars"].(map[string]any)
	ant, oai := vars["ANTHROPIC_API_KEY"].(string), vars["OPENAI_API_KEY"].(string)
	if ant == "" || oai == "" {
		t.Fatalf("tokens for %s: %v", id, vars)
	}
	call := func(method, path, tok string) int {
		req, _ := http.NewRequest(method, proxy.URL+path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Api-Key", tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct{ method, path, tok string }{
		{"POST", "/anthropic/v1/messages", ant},
		{"POST", "/anthropic/v1/messages/count_tokens", ant},
		{"GET", "/anthropic/v1/models", ant},
		{"GET", "/anthropic/v1/models/claude-x", ant},
		{"POST", "/openai/v1/chat/completions", oai},
		{"POST", "/openai/v1/responses", oai},
		{"GET", "/openai/v1/responses/resp_123", oai},
		{"POST", "/openai/v1/completions", oai},
		{"GET", "/openai/v1/models", oai},
	} {
		if st := call(c.method, c.path, c.tok); st != 200 {
			t.Errorf("%s %s: %d, want it forwarded", c.method, c.path, st)
		}
	}
	n := len(up.paths)
	for _, c := range []struct{ method, path, tok string }{
		{"GET", "/anthropic/v1/organizations/me", ant},
		{"POST", "/anthropic/v1/messages/batches", ant},
		{"GET", "/anthropic/v1/files", ant},
		{"POST", "/openai/v1/files", oai},
		{"POST", "/openai/v1/fine_tuning/jobs", oai},
		{"GET", "/openai/v1/organization/api_keys", oai},
		{"POST", "/openai/v1/batches", oai},
		{"DELETE", "/openai/v1/models/ft-mine", oai},
		{"POST", "/openai/v1/chat/completions/../../files", oai},
		{"POST", "/openai/v1/chat/completions/%2e%2e/%2e%2e/files", oai},
		{"GET", "/openai/", oai},
	} {
		if st := call(c.method, c.path, c.tok); st != 403 && st != 404 && st != 405 {
			t.Errorf("%s %s: %d, want it refused", c.method, c.path, st)
		}
	}
	if len(up.paths) != n {
		t.Errorf("refused requests reached the provider: %v", up.paths[n:])
	}
}
