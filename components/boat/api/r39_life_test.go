package main

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// r39: a create from a snapshot with a narrower secret selection than the run
// it was taken from boots fresh (fresh_boot) and fails closed; the fork reads
// its parent's run under the parent's start lock; a failed-closed compat fork
// carries the child's sandbox; sandbox.error is published from the VM in hand;
// the 502 vm is the VM boatd reports after the stop.

func (e *env) freshCreatesOf(id string) int {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	n := 0
	for _, v := range e.boatd.freshCreates {
		if v == id {
			n++
		}
	}
	return n
}

func (e *env) lastCreate() map[string]any {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	return e.boatd.creates[len(e.boatd.creates)-1]
}

// r39Snapshot snapshots a VM (native) and returns the snapshot id.
func (e *env) r39Snapshot(vm, name string) string {
	e.t.Helper()
	st, out := e.do(e.owner, "POST", vmPath(vm, "snapshots"), map[string]any{"name": name})
	if st != 201 {
		e.t.Fatalf("snapshot of %s = %d %v", vm, st, out)
	}
	return out["id"].(string)
}

// createFrom creates a VM from snapshot sid with the given extra body fields.
func (e *env) createFrom(sid string, extra map[string]any) (int, map[string]any) {
	body := map[string]any{"name": "from-snap", "snapshot": sid}
	for k, v := range extra {
		body[k] = v
	}
	return e.do(e.owner, "POST", "/v1/vms", body)
}

// 1: a snapshot of a run with secrets, created from with a narrower selection,
// asks boatd for a fresh boot; the same selection, or a wider one, does not.
func TestR39CreateFromSnapshotNarrowerSelectionBootsFresh(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	src := e.createVM() // the default selection: TEAM_SEC in its env
	sid := e.r39Snapshot(src, "with-secrets")

	st, out := e.createFrom(sid, map[string]any{"secrets": []string{}})
	if st != 201 {
		t.Fatalf("narrower create = %d %v", st, out)
	}
	kid := out["id"].(string)
	if e.freshCreatesOf(kid) != 1 || e.lastCreate()["fresh_boot"] != true {
		t.Fatalf("narrower create: fresh creates %d, body %v, want a fresh_boot", e.freshCreatesOf(kid), e.lastCreate())
	}
	// The env is pushed even though it is empty of secrets: BOAT_ID is in it.
	e.boatd.mu.Lock()
	vars, _ := e.boatd.vms[kid]["env"].(map[string]any)
	e.boatd.mu.Unlock()
	if vars["BOAT_ID"] != kid || vars["TEAM_SEC"] != nil {
		t.Fatalf("env of the fresh-boot VM = %v, want BOAT_ID and no TEAM_SEC", vars)
	}

	// A named selection is narrower than the defaults it was taken under.
	if st, out := e.createFrom(sid, map[string]any{"secrets": []string{"TEAM_SEC"}}); st != 201 || e.freshCreatesOf(out["id"].(string)) != 1 {
		t.Fatalf("named-only create = %d %v, want fresh_boot", st, out)
	}

	// The same selection (the default) resumes the memory: no fresh_boot.
	st, out = e.createFrom(sid, nil)
	if st != 201 {
		t.Fatalf("same-selection create = %d %v", st, out)
	}
	if e.freshCreatesOf(out["id"].(string)) != 0 || e.lastCreate()["fresh_boot"] != nil {
		t.Fatalf("same selection sent fresh_boot: %v", e.lastCreate())
	}

	// A snapshot of a run with no secrets can be created from with any selection.
	bare := e.createVMWith(map[string]any{"secrets": []string{}})
	bsid := e.r39Snapshot(bare, "bare")
	if st, out := e.createFrom(bsid, nil); st != 201 || e.freshCreatesOf(out["id"].(string)) != 0 {
		t.Fatalf("create from a secret-free snapshot = %d %v, want no fresh_boot", st, out)
	}
}

func (e *env) createVMWith(extra map[string]any) string {
	e.t.Helper()
	body := map[string]any{"name": "dev", "size": "small"}
	for k, v := range extra {
		body[k] = v
	}
	st, vm := e.do(e.owner, "POST", "/v1/vms", body)
	if st != 201 {
		e.t.Fatalf("create vm: %d %v", st, vm)
	}
	return vm["id"].(string)
}

// 1: a snapshot with no record (an import, a legacy snapshot) is judged against
// the team's inject defaults; a converted no_env snapshot retains sticky policy.
func TestR39UnknownSourceAndNoEnvSource(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	src := e.createVM()
	sid := e.r39Snapshot(src, "legacy")
	e.srv.store.Update(func(st *State) error { delete(st.VMSecrets, snapSourceKey(sid)); return nil })
	if st, out := e.createFrom(sid, map[string]any{"secrets": []string{}}); st != 201 || e.freshCreatesOf(out["id"].(string)) != 1 {
		t.Fatalf("unknown source, narrower = %d %v, want fresh_boot", st, out)
	}
	if st, out := e.createFrom(sid, nil); st != 201 || e.freshCreatesOf(out["id"].(string)) != 0 {
		t.Fatalf("unknown source, defaults = %d %v, want no fresh_boot", st, out)
	}
	// A converted no_env disk keeps its policy but cannot certify copied memory.
	ne := e.noEnvVM()
	nsid := e.r39Snapshot(ne, "clean")
	if rec := e.srv.store.Snapshot().VMSecrets[snapSourceKey(nsid)]; len(rec) != 2 || rec[1] != snapNoEnv {
		t.Fatalf("no_env snapshot record = %v", rec)
	}
	if !e.srv.snapshotSourceNoEnv(nsid) || e.srv.snapshotSourceProtected(nsid) {
		t.Fatal("converted snapshot must retain sticky no_env policy without a protected-memory certificate")
	}
	st, out := e.createFrom(nsid, map[string]any{"secrets": []string{}})
	if st != 201 {
		t.Fatalf("create from a converted no_env snapshot = %d %v", st, out)
	}
	kid := out["id"].(string)
	if !e.srv.noEnvRun(kid) || e.freshCreatesOf(kid) != 1 || e.lastCreate()["fresh_boot"] != true {
		t.Fatalf("converted snapshot clone = %v, want sticky no_env and fresh_boot", out)
	}
}

// 1: the same through compat: a saved snapshot of a secrets sandbox, created
// from with noEnv, boots fresh; without noEnv it does not.
func TestR39CompatFromNoEnvBootsFresh(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	src := e.newSandbox(nil)
	e.ok("POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": src, "name": "golden"}, 202)

	kid := e.nativeOf(e.newSandbox(map[string]any{"from": "golden", "noEnv": true}))
	if e.freshCreatesOf(kid) != 1 || e.lastCreate()["fresh_boot"] != true {
		t.Fatalf("compat from+noEnv: fresh creates %d, body %v", e.freshCreatesOf(kid), e.lastCreate())
	}
	kid = e.nativeOf(e.newSandbox(map[string]any{"from": "golden"}))
	if e.freshCreatesOf(kid) != 0 || e.lastCreate()["fresh_boot"] != nil {
		t.Fatalf("compat from: sent fresh_boot: %v", e.lastCreate())
	}
}

// 1: a fresh-boot create whose env push fails is stopped (never left running with
// the snapshot's /etc/boat/env): 502 env_start_failed with the stopped VM, not
// the one the create saw. Unstoppable, it is deleted.
func TestR39FreshCreateFailsClosed(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	sid := e.r39Snapshot(e.createVM(), "src")
	h := e.r31Hook("sandbox.error")

	e.setEnvFailures(true, false)
	st, out := e.createFrom(sid, map[string]any{"secrets": []string{}, "ttl_s": 3600})
	if st != http.StatusBadGateway || nativeErrCode(out) != "env_start_failed" {
		t.Fatalf("create = %d %v, want 502 env_start_failed", st, out)
	}
	vm, _ := out["vm"].(map[string]any)
	id, _ := vm["id"].(string)
	if id == "" || vm["state"] != "stopped" || vm["stopped_by"] != "boatd" || vm["expires_at"] != nil || vm["env_error"] != nil {
		t.Fatalf("vm = %v, want the VM boatd reports after the stop (no expires_at, no env_error)", vm)
	}
	if s := e.boatdState(id); s != "stopped" {
		t.Fatalf("VM = %s, want stopped", s)
	}
	// sandbox.error came from the VM in hand, not from a read of the stopped VM.
	waitFor(t, "sandbox.error", func() bool { return len(h.types()) >= 1 })
	if sb := h.last()["data"].(map[string]any)["sandbox"].(map[string]any); sb["id"] != e.srv.compatID(e.teamID(), id) {
		t.Fatalf("sandbox.error = %v", h.last())
	}

	// A VM that cannot be stopped is deleted.
	e.boatd.mu.Lock()
	e.boatd.failStop = true
	e.boatd.mu.Unlock()
	before := e.boatdVMCount()
	st, out = e.createFrom(sid, map[string]any{"secrets": []string{}})
	if st != http.StatusBadGateway || nativeErrCode(out) != "env_start_failed" || out["vm"] != nil {
		t.Fatalf("unstoppable create = %d %v, want 502 env_start_failed and no vm", st, out)
	}
	if n := e.boatdVMCount(); n != before {
		t.Fatalf("boatd has %d VMs, want %d (the failed one deleted)", n, before)
	}
}

// 1: a compat create that fails closed carries the stopped sandbox.
func TestR39CompatCreateFailClosedCarriesSandbox(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	src := e.newSandbox(nil)
	e.ok("POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": src, "name": "golden"}, 202)
	e.setEnvFailures(true, false)
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"from": "golden", "noEnv": true}, nil)
	sb, _ := out["sandbox"].(map[string]any)
	if st != http.StatusBadGateway || out["code"] != "env_start_failed" || sb == nil || sb["state"] != "archived" || !reBX.MatchString(sb["id"].(string)) {
		t.Fatalf("compat create = %d %v, want 502 env_start_failed with the archived sandbox", st, out)
	}
}

// 2: a fork waits for a start of its parent: the run mark it reads and the
// memory it copies are the parent's after that start.
func TestR39ForkTakesParentStartLock(t *testing.T) {
	e := setup(t)
	parent := e.runningSecretVM()
	unlock := startLocks.lock(parent)
	var wg sync.WaitGroup
	var st int
	var out map[string]any
	wg.Add(1)
	go func() {
		defer wg.Done()
		st, out = e.do(e.owner, "POST", "/v1/vms/"+parent+"/fork", map[string]any{"name": "kid", "no_env": true})
	}()
	time.Sleep(300 * time.Millisecond)
	e.boatd.mu.Lock()
	n := len(e.boatd.forks)
	e.boatd.mu.Unlock()
	if n != 0 {
		unlock()
		wg.Wait()
		t.Fatalf("the fork reached boatd (%d prepares) while the parent's start lock was held", n)
	}
	unlock()
	wg.Wait()
	if st != 201 || e.freshReleasesOf(out["id"].(string)) != 1 {
		t.Fatalf("fork after the start = %d %v, want 201 with a fresh-boot release", st, out)
	}
}

// 3, 4, 5: a fork that fails closed answers, through compat, with the child's
// sandbox; sandbox.error is published although the child is stopped by then; the
// vm of the native answer is the child as boatd reports it after the stop.
func TestR39ForkFailClosedSandboxHookAndPostStopVM(t *testing.T) {
	e := setup(t)
	h := e.r31Hook("sandbox.error")
	parent := e.runningSecretVM()
	e.setReleaseEnvFail(true)

	st, out := e.do(e.owner, "POST", "/v1/vms/"+parent+"/fork", map[string]any{"name": "kid", "no_env": true, "ttl_s": 3600})
	if st != http.StatusBadGateway || nativeErrCode(out) != "env_start_failed" {
		t.Fatalf("native fork = %d %v, want 502 env_start_failed", st, out)
	}
	vm, _ := out["vm"].(map[string]any)
	if vm["state"] != "stopped" || vm["stopped_by"] != "boatd" || vm["expires_at"] != nil || vm["env_error"] != nil {
		t.Fatalf("native vm = %v, want the post-stop VM", vm)
	}
	waitFor(t, "sandbox.error of the fork", func() bool { return len(h.types()) >= 1 })
	if d := h.last()["data"].(map[string]any); d["state"] != "error" || d["sandbox"].(map[string]any)["id"] != e.srv.compatID(e.teamID(), vm["id"].(string)) {
		t.Fatalf("sandbox.error data = %v", d)
	}

	st, cout, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+e.srv.compatID(e.teamID(), parent)+"/fork", map[string]any{"name": "kid2", "noEnv": true}, nil)
	sb, _ := cout["sandbox"].(map[string]any)
	if st != http.StatusBadGateway || cout["code"] != "env_start_failed" || sb == nil || sb["state"] != "archived" {
		t.Fatalf("compat fork = %d %v, want 502 env_start_failed with the archived child as sandbox", st, cout)
	}
	if id, _ := sb["id"].(string); !reBX.MatchString(id) || e.srv.store.Snapshot().CompatIDs[id] == nil {
		t.Fatalf("compat sandbox id = %v, want the child's bx id", sb["id"])
	}
}

// 5: a fail-closed start answers with the VM as boatd reports it after the stop;
// sandbox.error is published for it.
func TestR39StartFailClosedPostStopVMAndHook(t *testing.T) {
	e := setup(t)
	h := e.r31Hook("sandbox.error")
	id := e.secretVM()
	e.setEnvFailures(false, true)
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true, "ttl_s": 3600})
	if st != http.StatusBadGateway || nativeErrCode(out) != "env_start_failed" {
		t.Fatalf("start = %d %v, want 502 env_start_failed", st, out)
	}
	vm, _ := out["vm"].(map[string]any)
	if vm["state"] != "stopped" || vm["stopped_by"] != "boatd" || vm["expires_at"] != nil || vm["env_error"] != nil || vm["error"] != nil {
		t.Fatalf("vm = %v, want the post-stop VM: no expires_at, no env_error", vm)
	}
	waitFor(t, "sandbox.error", func() bool { return len(h.types()) >= 1 })
	time.Sleep(100 * time.Millisecond)
	if got := h.types(); len(got) != 1 || got[0] != "sandbox.error" {
		t.Fatalf("webhook events = %v, want exactly one sandbox.error", got)
	}
	if d := h.last()["data"].(map[string]any); d["sandbox"].(map[string]any)["id"] != e.srv.compatID(e.teamID(), id) || d["error"] != envErrorMessage {
		t.Fatalf("sandbox.error data = %v", d)
	}
}

// The snapshot's record goes with the snapshot and with its VM.
func TestR39SnapshotRecordIsForgotten(t *testing.T) {
	e := setup(t)
	vm := e.createVM()
	a, b := e.r39Snapshot(vm, "a"), e.r39Snapshot(vm, "b")
	has := func(sid string) bool { _, ok := e.srv.store.Snapshot().VMSecrets[snapSourceKey(sid)]; return ok }
	if !has(a) || !has(b) {
		t.Fatal("snapshots have no recorded selection")
	}
	if st, out := e.do(e.owner, "DELETE", "/v1/snapshots/"+a, nil); st != 200 {
		t.Fatalf("delete snapshot = %d %v", st, out)
	}
	if has(a) || !has(b) {
		t.Fatalf("after deleting a: has(a)=%v has(b)=%v", has(a), has(b))
	}
	if st, out := e.do(e.owner, "DELETE", "/v1/vms/"+vm, nil); st != 200 {
		t.Fatalf("delete vm = %d %v", st, out)
	}
	waitFor(t, "the VM's snapshot records dropped", func() bool { return !has(b) })
}
