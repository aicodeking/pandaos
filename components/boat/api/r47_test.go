package main

import (
	"slices"
	"testing"
	"time"
)

// r47: a no_env create never takes secrets, @no_env is recorded only for a run
// that pushed none, a native create without ttl_s gets one hour, and compat
// environment versions report the real sandboxCount.

func (e *env) lastCreateTTL() any {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	return e.boatd.creates[len(e.boatd.creates)-1]["ttl_s"]
}

func (e *env) memberKey(role string) string {
	tok, _ := e.addMember(role)
	return tok
}

// no_env with a non-empty secrets list is 400 no_env_sticky before any VM exists;
// absent or [] secrets are fine. A member key gets the same answer.
func TestR47NoEnvCreateRefusesSecrets(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	for name, key := range map[string]string{"owner": e.owner, "member": e.memberKey("member")} {
		n := e.vmCount()
		st, out := e.do(key, "POST", "/v1/vms", map[string]any{"no_env": true, "secrets": []string{"TEAM_SEC"}})
		if st != 400 || r46Code(out) != errNoEnvSticky {
			t.Fatalf("%s: no_env + secrets = %d %v, want 400 no_env_sticky", name, st, out)
		}
		if e.vmCount() != n {
			t.Fatalf("%s: the refused create booted a VM", name)
		}
		for _, body := range []map[string]any{{"no_env": true}, {"no_env": true, "secrets": []string{}}} {
			if st, out := e.do(key, "POST", "/v1/vms", body); st != 201 {
				t.Fatalf("%s: %v = %d %v, want 201", name, body, st, out)
			}
		}
	}
}

// A create from a no_env snapshot is no_env: naming secrets is refused too, and
// an empty selection still works.
func TestR47CreateFromNoEnvSnapshotRefusesSecrets(t *testing.T) {
	e := setup(t)
	id := e.noEnvVM()
	sid := e.r39Snapshot(id, "bare")
	if rec := e.snapRecord(sid); !slices.Contains(rec, snapNoEnv) {
		t.Fatalf("record of a no_env run = %v, want @no_env", rec)
	}
	member := e.memberKey("member")
	n := e.vmCount()
	for name, key := range map[string]string{"owner": e.owner, "member": member} {
		st, out := e.do(key, "POST", "/v1/vms", map[string]any{"snapshot": sid, "secrets": []string{"TEAM_SEC"}})
		if st != 400 || r46Code(out) != errNoEnvSticky {
			t.Fatalf("%s: create from a no_env snapshot with secrets = %d %v, want 400 no_env_sticky", name, st, out)
		}
	}
	if e.vmCount() != n {
		t.Fatal("a refused create booted a VM")
	}
	for _, body := range []map[string]any{{}, {"secrets": []string{}}} {
		st, out := e.createFrom(sid, body)
		if st != 201 {
			t.Fatalf("create from a no_env snapshot %v = %d %v", body, st, out)
		}
		if !e.noEnvRunOf(out["id"].(string)) {
			t.Fatal("a create from a no_env snapshot is not no_env")
		}
	}
}

func (e *env) markNoEnv(id string) {
	e.t.Helper()
	if err := e.srv.store.Update(func(st *State) error {
		st.NoEnvRuns[id] = time.Now()
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
}

// @no_env is recorded only when the run's pushed names are empty: a marked run
// that held secrets is recorded by its names, one with unknown names not at all.
func TestR47SnapshotRecordNeverClaimsNoEnvForASecretRun(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	run := e.createVM() // its run holds TEAM_SEC
	e.markNoEnv(run)    // marked, though its recorded names are not empty
	sid := e.r39Snapshot(run, "marked")
	rec := e.snapRecord(sid)
	if slices.Contains(rec, snapNoEnv) || !slices.Contains(rec, "TEAM_SEC") {
		t.Fatalf("record = %v, want TEAM_SEC and no @no_env", rec)
	}
	// Unknown names (none recorded): no @no_env either.
	unknown := e.createVM()
	e.markNoEnv(unknown)
	if err := e.srv.store.Update(func(st *State) error {
		delete(st.VMSecrets, runNamesKey(unknown))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rec := e.snapRecord(e.r39Snapshot(unknown, "unknown")); slices.Contains(rec, snapNoEnv) {
		t.Fatalf("record of a run with unknown names = %v, want no @no_env", rec)
	}
	// A genuine no_env run still records @no_env.
	if rec := e.snapRecord(e.r39Snapshot(e.noEnvVM(), "bare")); !slices.Contains(rec, snapNoEnv) {
		t.Fatalf("record of a no_env run = %v, want @no_env", rec)
	}
}

// A share to another team carries @no_env only for a genuine record; a record
// that claims @no_env next to names is not copied at all.
func TestR47CrossTeamShareRecord(t *testing.T) {
	e := setup(t)
	_, other := e.newTeam("other")
	share := func(vm, sid string) string {
		st, sh := e.do(e.owner, "POST", "/v1/vms/"+vm+"/snapshots/"+sid+"/shares", map[string]any{})
		if st != 201 {
			t.Fatalf("share = %d %v", st, sh)
		}
		st, imp := e.do(other, "POST", "/v1/snapshots/import-share", map[string]any{"code": sh["code"], "name": "imp"})
		if st != 201 {
			t.Fatalf("import = %d %v", st, imp)
		}
		return imp["id"].(string)
	}
	clean := e.noEnvVM()
	sid := e.r39Snapshot(clean, "bare")
	if rec := e.snapRecord(share(clean, sid)); !slices.Contains(rec, snapNoEnv) {
		t.Fatalf("cross-team copy of a genuine no_env record = %v, want @no_env", rec)
	}
	fvm := e.createVM()
	forged := e.r39Snapshot(fvm, "forged")
	if err := e.srv.store.Update(func(st *State) error {
		st.VMSecrets[snapSourceKey(forged)] = []string{snapSourceVM + "x", snapNoEnv, "TEAM_SEC"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rec := e.snapRecord(share(fvm, forged)); len(rec) != 0 {
		t.Fatalf("copy of a record that claims @no_env with names = %v, want none", rec)
	}
}

// An omitted ttl_s on native create is one hour; an explicit 0 is no auto-stop.
func TestR47CreateDefaultTTL(t *testing.T) {
	e := setup(t)
	if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "a"}); st != 201 {
		t.Fatalf("create = %d %v", st, out)
	}
	if got := e.lastCreateTTL(); got != float64(3600) {
		t.Fatalf("omitted ttl_s sent %v to boatd, want 3600", got)
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "b", "ttl_s": 0}); st != 201 {
		t.Fatalf("create = %d %v", st, out)
	}
	if got := e.lastCreateTTL(); got != float64(0) {
		t.Fatalf("explicit ttl_s 0 sent %v, want 0", got)
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "c", "ttl_s": 120}); st != 201 {
		t.Fatalf("create = %d %v", st, out)
	}
	if got := e.lastCreateTTL(); got != float64(120) {
		t.Fatalf("ttl_s 120 sent %v", got)
	}
	if st, _ := e.do(e.owner, "POST", "/v1/vms", map[string]any{"ttl_s": -1}); st != 400 {
		t.Fatalf("negative ttl_s = %d, want 400", st)
	}
	// Batch and create-from-snapshot take the same default.
	if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "d", "count": 2}); st != 201 {
		t.Fatalf("batch = %d %v", st, out)
	}
	if got := e.lastCreateTTL(); got != float64(3600) {
		t.Fatalf("batch omitted ttl_s sent %v, want 3600", got)
	}
	sid := e.r39Snapshot(e.createVM(), "s")
	if st, out := e.createFrom(sid, nil); st != 201 {
		t.Fatalf("from snapshot = %d %v", st, out)
	}
	if got := e.lastCreateTTL(); got != float64(3600) {
		t.Fatalf("snapshot create omitted ttl_s sent %v, want 3600", got)
	}
	if st, out := e.createFrom(sid, map[string]any{"ttl_s": 0}); st != 201 {
		t.Fatalf("from snapshot = %d %v", st, out)
	}
	if got := e.lastCreateTTL(); got != float64(0) {
		t.Fatalf("snapshot create ttl_s 0 sent %v, want 0", got)
	}
}

// Compat environment versions report how many team VMs are on the environment.
func TestR47CompatEnvironmentSandboxCount(t *testing.T) {
	e := setup(t)
	count := func() float64 {
		envs := e.ok("GET", "/api/v1/environments", nil, 200)["environments"].([]any)
		if len(envs) != 1 {
			t.Fatalf("environments = %v", envs)
		}
		vs := envs[0].(map[string]any)["versions"].([]any)
		return vs[0].(map[string]any)["sandboxCount"].(float64)
	}
	st, env := e.do(e.owner, "POST", "/v1/environments", map[string]any{"name": "web"})
	if st != 201 {
		t.Fatalf("create environment = %d %v", st, env)
	}
	if got := count(); got != 0 {
		t.Fatalf("sandboxCount = %v, want 0", got)
	}
	a := e.createVMWith(map[string]any{"environment": "web"})
	e.createVMWith(map[string]any{"environment": "web"})
	e.createVM() // the default environment is none: not counted
	if got := count(); got != 2 {
		t.Fatalf("sandboxCount = %v, want 2", got)
	}
	e.do(e.owner, "DELETE", "/v1/vms/"+a, nil)
	if got := count(); got != 1 {
		t.Fatalf("sandboxCount after a delete = %v, want 1", got)
	}
}
