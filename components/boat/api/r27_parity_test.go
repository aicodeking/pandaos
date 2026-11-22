package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func (e *env) setEnvFail(v bool) {
	e.boatd.mu.Lock()
	e.boatd.envFail = v
	e.boatd.mu.Unlock()
}

// A compat create whose env push failed must not claim a ready sandbox.
func TestR27CompatCreateReflectsEnvError(t *testing.T) {
	e := setup(t)
	e.setEnvFail(true)
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"env": map[string]any{"EXTRA": "1"}}, nil)
	if st != 202 {
		t.Fatalf("create: %d %v", st, out)
	}
	sb, _ := out["sandbox"].(map[string]any)
	if out["status"] != "provisioning" || sb["state"] != "error" || sb["error"] != "could not set the VM environment" {
		t.Fatalf("create with failed env push = %v, want status provisioning, state error, error set", out)
	}
	e.setEnvFail(false)
	st, out, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"env": map[string]any{"EXTRA": "1"}}, nil)
	sb, _ = out["sandbox"].(map[string]any)
	if st != 202 || out["status"] != "ready" || sb["state"] != "ready" || sb["error"] != nil {
		t.Fatalf("healthy create = %d %v", st, out)
	}
}

func TestR27CompatCreateCountReflectsEnvError(t *testing.T) {
	e := setup(t)
	src := e.createVM()
	e.r39Snapshot(src, "batch-src") // count above 1 clones a snapshot (r43)
	e.setEnvFail(true)
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"count": 2, "from": "batch-src", "env": map[string]any{"EXTRA": "1"}}, nil)
	if st != 202 || out["status"] != "provisioning" {
		t.Fatalf("batch create: %d %v", st, out)
	}
	for _, x := range out["sandboxes"].([]any) {
		if sb := x.(map[string]any); sb["state"] != "error" || sb["error"] == nil {
			t.Fatalf("batch sandbox = %v", sb)
		}
	}
}

// Starting a running VM with env whose push fails answers 502, not 200.
func TestR27StartRunningEnvPushFailure(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	e.setEnvFail(true)
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"env": map[string]any{"A": "b"}})
	if eo, _ := out["error"].(map[string]any); st != http.StatusBadGateway || eo["code"] != "env_update_failed" {
		t.Fatalf("native start = %d %v, want 502 env_update_failed", st, out)
	}
	bx := e.newSandbox(nil)
	st, out, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/resume", map[string]any{"env": map[string]any{"A": "b"}}, nil)
	if st != http.StatusBadGateway {
		t.Fatalf("compat resume = %d %v, want 502", st, out)
	}
	e.setEnvFail(false)
	st, out = e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"env": map[string]any{"A": "b"}})
	if st != 200 {
		t.Fatalf("healthy start = %d %v", st, out)
	}
}

func TestR27SymlinkType(t *testing.T) {
	for _, typ := range []string{"symlink", "link"} {
		got := fileEntry("/home/user", map[string]any{"name": "l", "type": typ})
		if got["isSymlink"] != true || got["isDir"] != false {
			t.Errorf("fileEntry type %q = %v, want isSymlink", typ, got)
		}
	}
	if got := fileEntry("/h", map[string]any{"name": "f", "type": "file"}); got["isSymlink"] != false {
		t.Errorf("file entry = %v", got)
	}
	for in, want := range map[string]string{"symbolic link": "symlink", "directory": "dir", "regular file": "file"} {
		if got := statFileType(in); got != want {
			t.Errorf("statFileType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestR27UsageDateOnly(t *testing.T) {
	e := setup(t)
	st, out := e.do(e.owner, "GET", "/v1/usage?from="+url.QueryEscape("2026-01-02"), nil)
	if st != 200 || out["since"] != "2026-01-02T00:00:00Z" {
		t.Fatalf("date-only from = %d %v", st, out)
	}
	st, out = e.do(e.owner, "GET", "/v1/usage?since=2026-01-02T03:04:05Z", nil)
	if st != 200 || out["since"] != "2026-01-02T03:04:05Z" {
		t.Fatalf("RFC 3339 since = %d %v", st, out)
	}
	st, out = e.do(e.owner, "GET", "/v1/usage?from=2026-01-02&to=2026-01-03", nil)
	if st != 200 || out["until"] == nil {
		t.Fatalf("date-only window = %d %v", st, out)
	}
	st, out = e.do(e.owner, "GET", "/v1/usage?from=2026-01-03&to=2026-01-02", nil)
	if st != 400 {
		t.Fatalf("reversed window = %d %v", st, out)
	}
	st, out = e.do(e.owner, "GET", "/v1/usage?from=yesterday", nil)
	if eo, _ := out["error"].(map[string]any); st != 400 || !strings.Contains(eo["message"].(string), "YYYY-MM-DD") {
		t.Fatalf("bad from = %d %v", st, out)
	}
}
