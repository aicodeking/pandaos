package main

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// r40: a fork decides no_env from the parent as it is after any start that held
// the lock; a snapshot is recorded under the start lock, from the VM's state read
// there; an environment builder holds only its environment's selection and is
// admin-only; unknown snapshots resume only for a create that covers every team
// secret; a VM whose env never landed fails closed until one push does; the
// fail-closed answers report env_pending=false.

func setForkPreLockHook(t *testing.T, f func(s *server, parent string)) {
	t.Helper()
	forkPreLockHook = f
	t.Cleanup(func() { forkPreLockHook = nil })
}

func setSnapshotPostPrepareHook(t *testing.T, f func(s *server, vm string)) {
	t.Helper()
	snapshotPostPrepareHook = f
	t.Cleanup(func() { snapshotPostPrepareHook = nil })
}

// snapRecord is the secret-selection record of snapshot sid ("" = none).
func (e *env) snapRecord(sid string) []string {
	return e.srv.store.Snapshot().VMSecrets[snapSourceKey(sid)]
}

func (e *env) boatdEnvOf(id string) map[string]any {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	m, _ := e.boatd.vms[id]["env"].(map[string]any)
	return m
}

// 1: a fork that read its parent stopped, then waited for a no_env cold start of
// it, forks the now-running no_env run: a no_env child, no team secrets.
func TestR40ForkRereadsParentUnderStartLock(t *testing.T) {
	e := setup(t)
	id := e.secretVM() // stopped; its last run had TEAM_SEC
	entered := make(chan struct{}, 1)
	setForkPreLockHook(t, func(_ *server, parent string) {
		if parent == id {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
	})
	var (
		once   atomic.Bool
		wg     sync.WaitGroup
		forkSt int
		kid    map[string]any
	)
	setStartHooks(t, nil, func(_ *server, vm string) {
		if vm != id || !once.CompareAndSwap(false, true) {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			forkSt, kid = e.do(e.owner, "POST", "/v1/vms/"+id+"/fork", map[string]any{"name": "kid"})
		}()
		select {
		case <-entered: // the fork read the parent (stopped) and now waits for the lock
		case <-time.After(5 * time.Second):
			t.Error("the fork never reached the parent's start lock")
		}
	})
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("no_env start = %d %v", st, out)
	}
	wg.Wait()
	if forkSt != 201 {
		t.Fatalf("fork = %d %v", forkSt, kid)
	}
	cid := kid["id"].(string)
	if !e.noEnvRunOf(cid) {
		t.Errorf("the child of a no_env run is not a no_env run: %v", kid)
	}
	if v := e.boatdEnvOf(cid); v["TEAM_SEC"] != nil {
		t.Errorf("child env = %v, want no TEAM_SEC", v)
	}
	if sel, ok := e.srv.store.Snapshot().VMSecrets[cid]; !ok || len(sel) != 0 {
		t.Errorf("child selection = %v (recorded %v), want none", sel, ok)
	}
	// The converted parent has sticky no_env policy but no protected lineage.
	if n := e.freshReleasesOf(cid); n != 1 {
		t.Errorf("fresh releases = %d, want 1 (converted parent memory is not certified)", n)
	}
	e.boatd.mu.Lock()
	labels, _ := e.boatd.vms[cid]["labels"].(map[string]any)
	e.boatd.mu.Unlock()
	if _, has := labels[secretsLabel]; has && labels[secretsLabel] != "" {
		t.Errorf("child labels = %v, want no secret selection", labels)
	}
}

// 2: a snapshot of a stopped secrets-run VM racing a no_env cold start is not
// recorded @no_env: the start waits for the snapshot's record.
func TestR40SnapshotHoldsStartLockAcrossPrepareAndRecord(t *testing.T) {
	e := setup(t)
	id := e.secretVM()
	var once atomic.Bool
	var wg sync.WaitGroup
	startsAtHook, startsAfter := -1, -1
	var startSt int
	setSnapshotPostPrepareHook(t, func(_ *server, vm string) {
		if vm != id || !once.CompareAndSwap(false, true) {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			startSt, _ = e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true})
		}()
		time.Sleep(300 * time.Millisecond)
		e.boatd.mu.Lock()
		startsAtHook = len(e.boatd.starts)
		e.boatd.mu.Unlock()
		if _, on := e.srv.store.Snapshot().NoEnvRuns[id]; on {
			t.Error("the no_env start marked its run while the snapshot was between prepare and record")
		}
	})
	e.boatd.mu.Lock()
	before := len(e.boatd.starts)
	e.boatd.mu.Unlock()
	sid := e.r39Snapshot(id, "racy")
	wg.Wait()
	e.boatd.mu.Lock()
	startsAfter = len(e.boatd.starts)
	e.boatd.mu.Unlock()
	if startsAtHook != before || startsAfter != before+1 || startSt != 200 {
		t.Fatalf("starts before/at the hook/after = %d/%d/%d (start %d), want the start to wait for the snapshot", before, startsAtHook, startsAfter, startSt)
	}
	rec := e.snapRecord(sid)
	if slices.Contains(rec, snapNoEnv) || !slices.Contains(rec, "TEAM_SEC") {
		t.Fatalf("record = %v, want the run's selection (TEAM_SEC), not @no_env", rec)
	}
	// So a create with no secrets cold-boots instead of resuming its memory.
	if st, out := e.createFrom(sid, map[string]any{"secrets": []string{}}); st != 201 || e.freshCreatesOf(out["id"].(string)) != 1 {
		t.Fatalf("create from the raced snapshot = %d %v, want a fresh boot", st, out)
	}
}

// 2: a snapshot is @no_env when the VM is no_env (sticky, r46: running or stopped,
// whatever run it holds), and never before: a VM that was a secret run keeps the
// record of the selection until a no_env start marks it.
func TestR40SnapshotNoEnvOnlyForTheRunItHolds(t *testing.T) {
	e := setup(t)
	other := e.secretVM()
	if rec := e.snapRecord(e.r39Snapshot(other, "before-mark")); slices.Contains(rec, snapNoEnv) || !slices.Contains(rec, "TEAM_SEC") {
		t.Fatalf("secret run: record = %v, want TEAM_SEC", rec)
	}
	id := e.noEnvVM() // running, no_env
	if rec := e.snapRecord(e.r39Snapshot(id, "running-no-env")); !slices.Contains(rec, snapNoEnv) {
		t.Fatalf("running no_env run: record = %v, want @no_env", rec)
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil); st != 200 {
		t.Fatalf("stop = %d %v", st, out)
	}
	if rec := e.snapRecord(e.r39Snapshot(id, "stopped-no-env")); !slices.Contains(rec, snapNoEnv) {
		t.Fatalf("stopped no_env run: record = %v, want @no_env", rec)
	}
	// A plain start stays no_env: the next snapshot is @no_env too.
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{}); st != 200 {
		t.Fatalf("start = %d %v", st, out)
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if rec := e.snapRecord(e.r39Snapshot(id, "after-plain-start")); !slices.Contains(rec, snapNoEnv) {
		t.Fatalf("after a plain start: record = %v, want @no_env", rec)
	}
}

// 3: an environment's builder holds the environment's selection, durably; only an
// admin key may start, fork or exec it; the build snapshot records the selection.
func TestR40EnvBuilderSelectionAndAdminOnly(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/A", map[string]any{"value": "sa", "inject": true})
	e.do(e.owner, "PUT", "/v1/secrets/B", map[string]any{"value": "sb", "inject": true})
	st, envOut := e.do(e.owner, "POST", "/v1/environments", map[string]any{"name": "base", "secrets": []string{"A"}})
	if st != 201 {
		t.Fatalf("create environment: %d %v", st, envOut)
	}
	eid := envOut["id"].(string)
	if st, out := e.do(e.owner, "POST", "/v1/environments/"+eid+"/build", nil); st != 202 {
		t.Fatalf("build: %d %v", st, out)
	}
	waitFor(t, "build", func() bool {
		x := e.srv.store.Snapshot().Environments[eid]
		return x.Build != nil && x.Build.State != "building"
	})
	b := e.srv.store.Snapshot().Environments[eid].Build
	if b.State != "ready" || b.VMID == "" {
		t.Fatalf("build = %+v", b)
	}
	builder := b.VMID
	snap := e.srv.store.Snapshot()
	if sel := snap.VMSecrets[builder]; !slices.Equal(sel, []string{"A"}) {
		t.Fatalf("builder selection = %v, want [A]", sel)
	}
	e.boatd.mu.Lock()
	labels, _ := e.boatd.vms[builder]["labels"].(map[string]any)
	e.boatd.mu.Unlock()
	if labels[secretsLabel] != "A" || labels[builderLabel] != eid {
		t.Fatalf("builder labels = %v, want boat.secrets=A and boat.env", labels)
	}
	if rec := e.snapRecord(b.SnapshotID); !slices.Equal(rec, []string{snapSourceVM + builder, "A"}) {
		t.Fatalf("build snapshot record = %v, want [vm=%s A]", rec, builder)
	}

	member := e.newKey("vms:read", "vms:write", "exec")
	for _, c := range []struct{ path, body string }{
		{"/start", "start"}, {"/fork", "fork"}, {"/exec", "exec"},
	} {
		var body any = map[string]any{}
		if c.body == "exec" {
			body = map[string]any{"cmd": []string{"true"}}
		}
		if st, out := e.do(member, "POST", "/v1/vms/"+builder+c.path, body); st != http.StatusForbidden || nativeErrCode(out) != "forbidden" {
			t.Errorf("non-admin %s of a builder = %d %v, want 403", c.body, st, out)
		}
	}
	if n := e.boatdState(builder); n != "stopped" {
		t.Errorf("builder = %s after the refused starts, want stopped", n)
	}

	// An admin start pushes the builder's selection, not every team secret.
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+builder+"/start", map[string]any{}); st != 200 {
		t.Fatalf("admin start = %d %v", st, out)
	}
	v := e.lastStart()
	envm, _ := v["env"].(map[string]any)
	if envm["A"] != "sa" || envm["B"] != nil {
		t.Fatalf("admin start env = %v, want A only", envm)
	}
	// A builder from before the selection was recorded falls back to the environment's.
	if err := e.srv.store.Update(func(st *State) error { delete(st.VMSecrets, builder); return nil }); err != nil {
		t.Fatal(err)
	}
	e.boatd.mu.Lock()
	delete(e.boatd.vms[builder]["labels"].(map[string]any), secretsLabel)
	delete(e.boatd.vms[builder]["labels"].(map[string]any), selectionMark)
	e.boatd.mu.Unlock()
	e.do(e.owner, "POST", "/v1/vms/"+builder+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+builder+"/start", map[string]any{}); st != 200 {
		t.Fatalf("admin start of a legacy builder = %d %v", st, out)
	}
	if envm, _ := e.lastStart()["env"].(map[string]any); envm["A"] != "sa" || envm["B"] != nil {
		t.Fatalf("legacy builder start env = %v, want A only", envm)
	}
}

// 4: a snapshot of unknown source resumes only for a create that covers every
// team secret; the record holds concrete names, so changing the defaults later
// does not widen it.
func TestR40UnknownSourceNeedsEverySecretToResume(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	e.do(e.owner, "PUT", "/v1/secrets/OTHER", map[string]any{"value": "o", "inject": false})
	sid := e.r39Snapshot(e.createVM(), "src")
	if err := e.srv.store.Update(func(st *State) error { delete(st.VMSecrets, snapSourceKey(sid)); return nil }); err != nil {
		t.Fatal(err)
	}
	// Covers the defaults only: not everything the unknown run could have held.
	if st, out := e.createFrom(sid, map[string]any{"secrets": []string{"TEAM_SEC"}}); st != 201 || e.freshCreatesOf(out["id"].(string)) != 1 {
		t.Fatalf("defaults-only create from an unknown snapshot = %d %v, want a fresh boot", st, out)
	}
	if st, out := e.createFrom(sid, nil); st != 201 || e.freshCreatesOf(out["id"].(string)) != 1 {
		t.Fatalf("default create from an unknown snapshot = %d %v, want a fresh boot", st, out)
	}
	if st, out := e.createFrom(sid, map[string]any{"secrets": []string{"TEAM_SEC", "OTHER"}}); st != 201 || e.freshCreatesOf(out["id"].(string)) != 0 {
		t.Fatalf("every-secret create from an unknown snapshot = %d %v, want to resume", st, out)
	}
}

func TestR40RecordExpandsDefaultsToNames(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	sid := e.r39Snapshot(e.createVM(), "src") // the default selection
	if rec := e.snapRecord(sid); !slices.Contains(rec, "TEAM_SEC") {
		t.Fatalf("record = %v, want the concrete name TEAM_SEC", rec)
	}
	// The team stops injecting it: a create asking for the defaults no longer covers
	// what the snapshot's memory holds.
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": false})
	if st, out := e.createFrom(sid, nil); st != 201 || e.freshCreatesOf(out["id"].(string)) != 1 {
		t.Fatalf("create after the defaults shrank = %d %v, want a fresh boot", st, out)
	}
	// A VM with no selection at all (every secret) is recorded by the names it has now.
	legacy := e.createVM()
	if err := e.srv.store.Update(func(st *State) error {
		delete(st.VMSecrets, legacy)
		delete(st.VMSecrets, runNamesKey(legacy)) // from before run names were kept too
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.boatd.mu.Lock()
	delete(e.boatd.vms[legacy]["labels"].(map[string]any), secretsLabel)
	delete(e.boatd.vms[legacy]["labels"].(map[string]any), selectionMark)
	e.boatd.mu.Unlock()
	// r42: with no run names its snapshot is of unknown source (no record) ...
	if rec := e.snapRecord(e.r39Snapshot(legacy, "legacy-unknown")); len(rec) != 0 {
		t.Fatalf("legacy record without run names = %v, want none", rec)
	}
	// ... and once a start pushes every team secret, the names it pushed.
	e.do(e.owner, "POST", "/v1/vms/"+legacy+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+legacy+"/start", map[string]any{}); st != 200 {
		t.Fatalf("legacy start = %d %v", st, out)
	}
	if rec := e.snapRecord(e.r39Snapshot(legacy, "legacy")); !slices.Contains(rec, "TEAM_SEC") || slices.Contains(rec, snapAll) {
		t.Fatalf("legacy record = %v, want concrete names", rec)
	}
}

// 4: a share import carries the sender's record to the new snapshot.
func TestR40ShareImportCopiesTheRecord(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	src := e.createVMWith(map[string]any{"secrets": []string{}}) // no team secrets
	sid := e.r39Snapshot(src, "bare")
	srcRec := slices.Clone(e.snapRecord(sid))
	st, sh := e.do(e.owner, "POST", "/v1/vms/"+src+"/snapshots/"+sid+"/shares", map[string]any{})
	if st != 201 {
		t.Fatalf("share = %d %v", st, sh)
	}
	st, imp := e.do(e.owner, "POST", "/v1/snapshots/import-share", map[string]any{"code": sh["code"], "name": "imported"})
	if st != 201 {
		t.Fatalf("import = %d %v", st, imp)
	}
	isid := imp["id"].(string)
	rec := e.snapRecord(isid)
	if len(rec) == 0 || !slices.Equal(rec[1:], srcRec[1:]) {
		t.Fatalf("imported record = %v, want the sender's %v", rec, srcRec)
	}
	if strings.HasPrefix(rec[0], snapSourceVM+src) {
		t.Errorf("imported record names the sender's VM %q: deleting it would drop the copy", rec[0])
	}
	// The sender's run held no team secrets (its selection was empty): the import resumes.
	if st, out := e.createFrom(isid, map[string]any{"secrets": []string{}}); st != 201 || e.freshCreatesOf(out["id"].(string)) != 0 {
		t.Fatalf("create from the import = %d %v, want to resume", st, out)
	}
	// A secret-run import keeps its names: a create without them cold-boots.
	run := e.createVM()
	sid2 := e.r39Snapshot(run, "secret")
	st, sh2 := e.do(e.owner, "POST", "/v1/vms/"+run+"/snapshots/"+sid2+"/shares", map[string]any{})
	if st != 201 {
		t.Fatalf("share 2 = %d %v", st, sh2)
	}
	_, imp2 := e.do(e.owner, "POST", "/v1/snapshots/import-share", map[string]any{"code": sh2["code"], "name": "imported2"})
	if rec := e.snapRecord(imp2["id"].(string)); !slices.Contains(rec, "TEAM_SEC") {
		t.Fatalf("imported secret-run record = %v, want TEAM_SEC", rec)
	}
	// Deleting the sender's VM does not drop the copy; deleting the import does.
	e.do(e.owner, "DELETE", "/v1/vms/"+run, nil)
	if rec := e.snapRecord(imp2["id"].(string)); len(rec) == 0 {
		t.Error("the copy was dropped with the sender's VM")
	}
	e.do(e.owner, "DELETE", "/v1/snapshots/"+imp2["id"].(string), nil)
	if rec := e.snapRecord(imp2["id"].(string)); len(rec) != 0 {
		t.Errorf("record of a deleted import = %v, want none", rec)
	}
}

// 5: a create that failed closed leaves a VM whose next plain start fails closed
// too, until one env push lands.
func TestR40EnvNeverLandedFailsNextStartClosed(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	sid := e.r39Snapshot(e.createVM(), "src")
	e.setEnvFailures(true, false)
	st, out := e.createFrom(sid, map[string]any{"secrets": []string{}})
	if st != http.StatusBadGateway || nativeErrCode(out) != "env_start_failed" {
		t.Fatalf("create = %d %v, want 502 env_start_failed", st, out)
	}
	id := out["vm"].(map[string]any)["id"].(string)
	if !e.srv.envNeverLanded(id) {
		t.Fatal("the fail-closed create left no env-never-landed mark")
	}

	// A plain start whose env does not land: stopped again, 502.
	e.setEnvFailures(false, true)
	st, out = e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{})
	if st != http.StatusBadGateway || nativeErrCode(out) != "env_start_failed" {
		t.Fatalf("plain start = %d %v, want 502 env_start_failed", st, out)
	}
	if vm, _ := out["vm"].(map[string]any); vm["state"] != "stopped" || vm["id"] != id {
		t.Fatalf("vm = %v, want the stopped VM", out["vm"])
	}
	if s := e.boatdState(id); s != "stopped" {
		t.Fatalf("VM = %s, want stopped", s)
	}
	if !e.srv.envNeverLanded(id) {
		t.Fatal("the mark was cleared by a start whose env did not land")
	}

	// One that lands clears it.
	e.setEnvFailures(false, false)
	if st, out = e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{}); st != 200 {
		t.Fatalf("start = %d %v, want 200", st, out)
	}
	if e.srv.envNeverLanded(id) {
		t.Fatal("the mark survived a start whose env landed")
	}
	// Later starts are ordinary again: a failed push leaves the VM running with its error.
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	e.setEnvFailures(false, true)
	if st, out = e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{}); st != 200 || out["env_error"] == nil {
		t.Fatalf("ordinary start with a failing env = %d %v, want 200 with env_error", st, out)
	}
	if e.srv.envPushPending(id) {
		t.Error("the gate leaked")
	}
}

// 5: the same for a fork that failed closed, and the mark goes with a deleted VM.
func TestR40ForkFailClosedMarksChildAndDeleteDropsIt(t *testing.T) {
	e := setup(t)
	parent := e.runningSecretVM()
	e.setReleaseEnvFail(true)
	st, out := e.do(e.owner, "POST", "/v1/vms/"+parent+"/fork", map[string]any{"name": "kid", "no_env": true})
	if st != http.StatusBadGateway || nativeErrCode(out) != "env_start_failed" {
		t.Fatalf("fork = %d %v, want 502 env_start_failed", st, out)
	}
	vm, _ := out["vm"].(map[string]any)
	child := vm["id"].(string)
	if !e.srv.envNeverLanded(child) {
		t.Fatal("the fail-closed fork left no env-never-landed mark")
	}
	// 6: the answer is not provisioning any more.
	if vm["env_pending"] != false {
		t.Errorf("fail-closed fork answer env_pending = %v, want false", vm["env_pending"])
	}
	if st, out := e.do(e.owner, "DELETE", "/v1/vms/"+child, nil); st != 200 {
		t.Fatalf("delete = %d %v", st, out)
	}
	if e.srv.envNeverLanded(child) {
		t.Error("the mark outlived its VM")
	}
}

// 6: a fail-closed create answers with env_pending=false.
func TestR40CreateFailClosedAnswerIsNotPending(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	sid := e.r39Snapshot(e.createVM(), "src")
	e.setEnvFailures(true, false)
	st, out := e.createFrom(sid, map[string]any{"secrets": []string{}})
	if st != http.StatusBadGateway || nativeErrCode(out) != "env_start_failed" {
		t.Fatalf("create = %d %v", st, out)
	}
	vm, _ := out["vm"].(map[string]any)
	if vm["env_pending"] != false {
		t.Fatalf("fail-closed create answer env_pending = %v, want false: %v", vm["env_pending"], vm)
	}
	// A batch: every stopped VM in vms says the same.
	st, out = e.createFrom(sid, map[string]any{"secrets": []string{}, "count": 2})
	if st != http.StatusBadGateway {
		t.Fatalf("batch create = %d %v", st, out)
	}
	list, _ := out["vms"].([]any)
	if len(list) != 2 {
		t.Fatalf("batch vms = %v", out["vms"])
	}
	for _, x := range list {
		if x.(map[string]any)["env_pending"] != false {
			t.Errorf("batch member = %v, want env_pending=false", x)
		}
	}
}
