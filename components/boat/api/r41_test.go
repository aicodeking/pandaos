package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// r41: the environment-builder gate on every guest and lifecycle operation, the
// env-never-landed mark following the disk, resume_on_boot refused from tenants,
// a stale no_env mark dropped by a normal cold start, the snapshot capture's
// lock scope, the conversation cap and the AI body budget.

// builderVM is an ordinary VM relabelled as the builder of an environment, with
// its compat id.
func (e *env) builderVM() (vm, bx string) {
	bx = e.newSandbox(map[string]any{"name": "builder"})
	vm = e.nativeOf(bx)
	e.boatd.mu.Lock()
	e.boatd.vms[vm]["labels"] = map[string]any{builderLabel: "env_x"}
	e.boatd.mu.Unlock()
	return vm, bx
}

func isBuilderRefusal(out map[string]any) bool {
	msg, _ := out["message"].(string)
	if e, ok := out["error"].(map[string]any); ok {
		msg, _ = e["message"].(string)
	}
	return strings.Contains(msg, "environment builder")
}

// 1: no route that resolves a builder VM answers a non-admin key but the metadata
// reads; an admin key still works.
func TestR41BuilderGateCoversEveryRoute(t *testing.T) {
	e := setup(t)
	vm, bx := e.builderVM()
	member := e.newKey("vms:read", "vms:write", "exec")
	// Routes that are not the builder gate's: another scope, a guest-only route,
	// or one that answers before it resolves the VM (the port host is not set up).
	other := map[string]bool{
		"POST /v1/vms/{id}/ssh/authorize":                    true,
		"GET /v1/vms/{id}/ports/{port}/url":                  true,
		"PUT /v1/vms/{id}/ai-access":                         true,
		"PUT /api/v1/sandboxes/{id}/ai-access":               true,
		"POST /v1/vms/{id}/snapshots/{sid}/shares":           true,
		"GET /v1/vms/{id}/snapshots/{sid}/shares":            true,
		"DELETE /v1/vms/{id}/snapshots/{sid}/shares/{share}": true,
	}
	probed := 0
	for _, pat := range e.srv.routeTable {
		m, path, _ := strings.Cut(pat, " ")
		if !strings.Contains(path, "{id}") || !(strings.HasPrefix(path, "/v1/vms/") || strings.HasPrefix(path, "/api/v1/sandboxes/")) {
			continue
		}
		id, hdr := vm, map[string]string{}
		if strings.HasPrefix(path, "/api/") {
			id = bx
			hdr["X-Ascii-Confirm-Delete"] = bx
		}
		p := strings.ReplaceAll(path, "{id}", id)
		for _, k := range []string{"pid", "cid", "sid", "port", "fingerprint", "share", "stopid"} {
			p = strings.ReplaceAll(p, "{"+k+"}", "1")
		}
		// Valid enough to pass each handler's own validation before the VM lookup.
		p += "?path=/x&pattern=x&query=x&q=x&limit=10"
		var body any
		if m != "GET" && m != "DELETE" {
			body = map[string]any{"path": "/x", "cmd": []string{"true"}, "command": "true", "prompt": "hi", "name": "n"}
		}
		st, out, _ := e.compat(member, m, p, body, hdr)
		switch {
		case other[pat]:
			continue
		case builderReadRoutes[pat]:
			if st == http.StatusForbidden && isBuilderRefusal(out) {
				t.Errorf("%s: refused for a metadata read (%d %v)", pat, st, out)
			}
		default:
			probed++
			if st != http.StatusForbidden || !isBuilderRefusal(out) {
				t.Errorf("%s on a builder as a non-admin key = %d %v, want the builder 403", pat, st, out)
			}
		}
	}
	if probed < 60 {
		t.Fatalf("probed only %d routes", probed)
	}
	// Nothing reached the guest or boatd.
	e.boatd.mu.Lock()
	n := len(e.boatd.execs) + len(e.boatd.starts)
	e.boatd.mu.Unlock()
	if n != 0 {
		t.Errorf("%d guest operations reached boatd for a non-admin key", n)
	}
	// The admin key is not refused, native and compat.
	if st, out := e.raw(e.owner, "POST", "/v1/vms/"+vm+"/exec", map[string]any{"cmd": []string{"true"}}); st == http.StatusForbidden {
		t.Errorf("admin exec on a builder = %d %s", st, out)
	}
	if st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/commands", map[string]any{"command": "true"}, nil); st == http.StatusForbidden {
		t.Errorf("admin compat command on a builder = %d %v", st, out)
	}
	if st, out := e.do(e.owner, "PATCH", "/v1/vms/"+vm, map[string]any{"name": "renamed"}); st != 200 {
		t.Errorf("admin patch of a builder = %d %v", st, out)
	}
	// The metadata reads work for a non-admin key.
	if st, out := e.do(member, "GET", "/v1/vms/"+vm, nil); st != 200 {
		t.Errorf("non-admin GET of a builder = %d %v", st, out)
	}
}

// 1: the representative operations, one by one.
func TestR41BuilderRepresentativeOperationsRefused(t *testing.T) {
	e := setup(t)
	vm, bx := e.builderVM()
	member := e.newKey("vms:read", "vms:write", "exec")
	cases := []struct {
		name, method, path string
		body               any
	}{
		{"compat commands", "POST", "/api/v1/sandboxes/" + bx + "/commands", map[string]any{"command": "id"}},
		{"compat prompt", "POST", "/api/v1/sandboxes/" + bx + "/prompt", map[string]any{"prompt": "hi"}},
		{"compat file read", "GET", "/api/v1/sandboxes/" + bx + "/files?path=/etc/boat/env", nil},
		{"compat file write", "PUT", "/api/v1/sandboxes/" + bx + "/files?path=/x", map[string]any{"content": "x"}},
		{"native commands", "POST", "/v1/vms/" + vm + "/commands", map[string]any{"cmd": []string{"id"}}},
		{"native prompts", "POST", "/v1/vms/" + vm + "/prompts", map[string]any{"prompt": "hi", "new": true}},
		{"legacy prompt", "POST", "/v1/vms/" + vm + "/prompt", map[string]any{"prompt": "hi"}},
		{"files", "GET", "/v1/vms/" + vm + "/files?path=/etc/boat/env", nil},
		{"ssh", "POST", "/v1/vms/" + vm + "/ssh", map[string]any{}},
		{"desktop", "GET", "/v1/vms/" + vm + "/desktop", nil},
		{"snapshot create", "POST", "/v1/vms/" + vm + "/snapshots", map[string]any{"name": "s"}},
		{"snapshot download", "GET", "/v1/vms/" + vm + "/snapshots/snap1/download", nil},
		{"delete", "DELETE", "/v1/vms/" + vm, nil},
	}
	for _, c := range cases {
		st, out, _ := e.compat(member, c.method, c.path, c.body, map[string]string{"X-Ascii-Confirm-Delete": bx})
		if st != http.StatusForbidden || !isBuilderRefusal(out) {
			t.Errorf("%s = %d %v, want the builder 403", c.name, st, out)
		}
	}
	if len(e.boatd.snaps) != 0 {
		t.Errorf("a snapshot of the builder was taken: %v", e.boatd.snaps)
	}
	if st := e.boatdState(vm); st == "" {
		t.Errorf("the builder was deleted")
	}
}

// 2: a fork of a VM whose env never landed is released like a fresh boot that must
// fail closed, and the child inherits the mark until a push lands on it.
func TestR41ForkOfEnvNeverParentFailsClosed(t *testing.T) {
	e := setup(t)
	parent := e.secretVM()
	e.srv.markEnvNeverLanded(parent)

	e.setReleaseEnvFail(true)
	st, out := e.do(e.owner, "POST", "/v1/vms/"+parent+"/fork", map[string]any{"name": "kid"})
	if st != http.StatusBadGateway || nativeErrCode(out) != "env_start_failed" {
		t.Fatalf("fork of an env-never parent with a failing push = %d %v, want 502 env_start_failed", st, out)
	}
	kid, _ := out["vm"].(map[string]any)
	kidID, _ := kid["id"].(string)
	if kidID == "" {
		t.Fatalf("no child in %v", out)
	}
	if !e.srv.envNeverLanded(kidID) {
		t.Errorf("the failed child did not inherit the env-never mark")
	}
	if e.freshReleasesOf(kidID) != 1 || e.boatdState(kidID) != "stopped" {
		t.Errorf("child: fresh releases %d, state %s, want 1 and stopped", e.freshReleasesOf(kidID), e.boatdState(kidID))
	}
	if !e.srv.envNeverLanded(parent) {
		t.Errorf("the parent lost its mark")
	}

	// A push that lands gives the child's disk a new env: the mark goes, the
	// parent's stays.
	e.setReleaseEnvFail(false)
	st, out = e.do(e.owner, "POST", "/v1/vms/"+parent+"/fork", map[string]any{"name": "kid2"})
	if st != 201 {
		t.Fatalf("fork with a landing push = %d %v", st, out)
	}
	id2 := out["id"].(string)
	if e.freshReleasesOf(id2) != 1 {
		t.Errorf("fork of an env-never parent: %d fresh releases, want 1", e.freshReleasesOf(id2))
	}
	if e.srv.envNeverLanded(id2) {
		t.Errorf("the mark survived a push that landed")
	}
	if !e.srv.envNeverLanded(parent) {
		t.Errorf("the parent lost its mark")
	}
}

// 2: a snapshot of a VM whose env never landed holds another run's env file: it is
// recorded as unknown, so a narrower create boots fresh.
func TestR41SnapshotOfEnvNeverVMIsUnknownSource(t *testing.T) {
	e := setup(t)
	vm := e.secretVM()
	if rec := e.snapRecord(e.r39Snapshot(vm, "known")); len(rec) == 0 {
		t.Fatalf("control: a snapshot of a VM with a landed env has no record")
	}
	e.srv.markEnvNeverLanded(vm)
	sid := e.r39Snapshot(vm, "unknown")
	if rec := e.snapRecord(sid); len(rec) != 0 {
		t.Fatalf("snapshot of an env-never VM recorded %v, want no record", rec)
	}
	st, out := e.createFrom(sid, map[string]any{"secrets": []string{}})
	if st != 201 || e.freshCreatesOf(out["id"].(string)) != 1 {
		t.Fatalf("create from it = %d %v, want a fresh boot", st, out)
	}
}

// 3: resume_on_boot is the host's.
func TestR41PatchRefusesResumeOnBoot(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	for _, body := range []map[string]any{
		{"resume_on_boot": true},
		{"resume_on_boot": false},
		{"name": "x", "resume_on_boot": true},
	} {
		st, out := e.do(e.owner, "PATCH", "/v1/vms/"+id, body)
		if st != 400 || nativeErrCode(out) != "invalid_request" {
			t.Errorf("PATCH %v = %d %v, want 400 invalid_request", body, st, out)
		}
		if msg := fmt.Sprint(out["error"]); !strings.Contains(msg, "resume_on_boot is managed by the host") {
			t.Errorf("PATCH %v message = %s", body, msg)
		}
	}
	if _, out := e.do(e.owner, "GET", "/v1/vms/"+id, nil); out["name"] == "x" {
		t.Errorf("a refused PATCH renamed the VM")
	}
	if st, out := e.do(e.owner, "PATCH", "/v1/vms/"+id, map[string]any{"name": "ok"}); st != 200 {
		t.Errorf("a plain PATCH = %d %v", st, out)
	}
}

// 4 (r46: sticky): a plain cold start of a no_env VM is a no_env start, so a start
// that errored leaves the mark as it was: the VM stays no_env.
func TestR41NormalColdStartDropsStaleNoEnvMark(t *testing.T) {
	e := setup(t)
	id := e.noEnvVM()
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil); st != 200 {
		t.Fatalf("stop = %d %v", st, out)
	}
	if _, ok := e.srv.store.Snapshot().NoEnvRuns[id]; !ok {
		t.Fatal("setup: no mark")
	}
	e.failNextStarts(1)
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{})
	if st < 500 {
		t.Fatalf("start with a failing boatd = %d %v, want 5xx", st, out)
	}
	if _, ok := e.srv.store.Snapshot().NoEnvRuns[id]; !ok {
		t.Errorf("a failed plain cold start of a no_env VM dropped its mark")
	}
}

// 4 (r46): a no_env mark that cannot be written stops the start that would have set
// it with a 500, and boatd is not called (fail closed: the VM must not run unmarked).
func TestR41NormalColdStartFailsWhenMarkCannotBeDropped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only directory")
	}
	e := setup(t)
	id := e.secretVM() // stopped, not marked
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	e.boatd.mu.Lock()
	before := len(e.boatd.starts)
	e.boatd.mu.Unlock()
	setStartHooks(t, nil, func(_ *server, vm string) {
		if vm == id {
			os.Chmod(e.dir, 0o500) // the next state write fails
		}
	})
	t.Cleanup(func() { os.Chmod(e.dir, 0o700) })
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true})
	os.Chmod(e.dir, 0o700)
	if st != 500 {
		t.Fatalf("no_env start whose mark cannot be written = %d %v, want 500", st, out)
	}
	e.boatd.mu.Lock()
	after := len(e.boatd.starts)
	e.boatd.mu.Unlock()
	if after != before {
		t.Errorf("boatd was asked to start the VM (%d -> %d starts) without its mark", before, after)
	}
	if e.noEnvRunOf(id) {
		t.Error("the VM is marked although the mark could not be written")
	}
}

// 5: the revoke of a snapshot capture (a synchronous state write) does not hold
// the VM's start lock: a start of the VM is not blocked by a slow fsync.
func TestR41SnapshotRevokeDoesNotBlockStart(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "http://api.test" // VMs get a self token: the revoke writes the store
	id := e.createVM()
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	const fsyncDelay = 1500 * time.Millisecond
	slowFsync(t, fsyncDelay)
	var wg sync.WaitGroup
	wg.Add(1)
	var snapSt int
	t0 := time.Now()
	go func() {
		defer wg.Done()
		snapSt, _ = e.do(e.owner, "POST", "/v1/vms/"+id+"/snapshots", map[string]any{"name": "s"})
	}()
	time.Sleep(300 * time.Millisecond) // the capture is in its revoke now
	t1 := time.Now()
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{}); st != 200 {
		t.Fatalf("start = %d %v", st, out)
	}
	took := time.Since(t1)
	wg.Wait()
	if snapSt != 201 {
		t.Fatalf("snapshot = %d", snapSt)
	}
	if time.Since(t0) < fsyncDelay {
		t.Skip("the capture finished before the fsync delay: the revoke wrote nothing durable")
	}
	if took > fsyncDelay/2 {
		t.Errorf("a start took %v during a snapshot's revoke (fsync %v): it waited for the capture's lock", took, fsyncDelay)
	}
}

// 6: conversations per VM are capped at 200; the least recently updated goes.
func TestR41ConversationsCapped(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	base := time.Now().UTC().Add(-time.Hour)
	if err := e.srv.store.Update(func(st *State) error {
		for i := 0; i < maxConvsPerVM; i++ {
			st.Conversations[id] = append(st.Conversations[id], &Conversation{
				ID: fmt.Sprintf("conv-%03d", i), VMID: id, Agent: "claude", Created: base, Updated: base.Add(time.Duration(i) * time.Second)})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, run := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": "go", "new": true})
	if st != 202 {
		t.Fatalf("prompt = %d %v", st, run)
	}
	convs := e.srv.store.Snapshot().Conversations[id]
	if len(convs) != maxConvsPerVM {
		t.Fatalf("%d conversations, want %d", len(convs), maxConvsPerVM)
	}
	for _, c := range convs {
		if c.ID == "conv-000" {
			t.Errorf("the least recently updated conversation was kept")
		}
	}
	if convs[len(convs)-1].ID != run["conversation_id"] && convs[len(convs)-1].PromptCount != 1 {
		t.Errorf("the new conversation is not the newest: %+v", convs[len(convs)-1])
	}
}

func TestR41CapConversationsKeepsOrderAndInput(t *testing.T) {
	now := time.Now()
	in := []*Conversation{{ID: "a", Updated: now.Add(2 * time.Second)}, {ID: "b", Updated: now}, {ID: "c", Updated: now.Add(time.Second)}}
	out := capConversations(in, 2)
	if len(out) != 2 || out[0].ID != "a" || out[1].ID != "c" {
		t.Errorf("capped = %v, want a, c", out)
	}
	if len(in) != 3 || in[1].ID != "b" {
		t.Errorf("the input was modified: %v", in)
	}
}

// 8: the per-team AI body budget is a quarter of the global one.
func TestR41AIBodyBudgetPerTeamIsAQuarter(t *testing.T) {
	if aiBudgetPerTeam != 32<<20 || aiBudgetGlobal != 128<<20 || aiBudgetPerTeam != aiBudgetGlobal/4 {
		t.Errorf("budgets: team %d MiB, global %d MiB, want 32 and 128", aiBudgetPerTeam>>20, aiBudgetGlobal>>20)
	}
	if aiBudgetPerVM < aiMaxBody || aiBudgetPerTeam < aiBudgetPerVM {
		t.Errorf("a VM budget of %d must hold one maximum body (%d) within the team's %d", aiBudgetPerVM, aiMaxBody, aiBudgetPerTeam)
	}
}
