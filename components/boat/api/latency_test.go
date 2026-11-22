package main

import (
	"bytes"
	"testing"
	"time"
)

// slowFsync makes every fsync take d, like a disk busy flushing a snapshot.
func slowFsync(t *testing.T, d time.Duration) {
	t.Helper()
	syncDelay.Store(int64(d))
	t.Cleanup(func() { syncDelay.Store(0) })
}

// apiOverheadBudget: boat-api's own share of create, fork, stop and start.
const apiOverheadBudget = 100 * time.Millisecond

// TestLifecycleDoesNotWaitForFsync fails when create, new-from-snapshot, fork,
// stop or start wait for a state.json fsync again: boatbench 41652606509
// measured 1.9 s of API time on new-from-snapshot from that one write.
func TestLifecycleDoesNotWaitForFsync(t *testing.T) {
	e := setup(t)
	if st, out := e.do(e.owner, "PUT", "/v1/secrets/TOKEN", map[string]any{"value": "s3cret"}); st/100 != 2 {
		t.Fatalf("secret: %d %v", st, out)
	}
	// A wait on the disk costs at least the full fsync delay; keep that far
	// above the budget so ordinary CPU contention (e.g. -race on a loaded
	// host) never looks like an fsync wait, while any real wait still fails.
	const fsyncDelay = 1500 * time.Millisecond
	const budget = fsyncDelay / 3
	slowFsync(t, fsyncDelay)
	timed := func(name, method, path string, body any, want int) map[string]any {
		t.Helper()
		t0 := time.Now()
		st, out := e.do(e.owner, method, path, body)
		took := time.Since(t0)
		if st != want {
			t.Fatalf("%s: %d %v", name, st, out)
		}
		if took > budget {
			t.Errorf("%s took %v through the API with an instant boatd and %v fsync (budget %v): something waits for the disk", name, took, fsyncDelay, budget)
		}
		return out
	}
	vm := timed("create", "POST", "/v1/vms", map[string]any{"name": "a", "secrets": []string{}, "no_env": true}, 201)["id"].(string)
	timed("create with a selection", "POST", "/v1/vms", map[string]any{"name": "b", "secrets": []string{"TOKEN"}}, 201)
	timed("create, all secrets", "POST", "/v1/vms", map[string]any{"name": "c"}, 201)
	e.boatd.mu.Lock()
	e.boatd.snaps["snap1"] = map[string]any{"id": "snap1", "team": e.teamID(), "vm_id": vm}
	e.boatd.mu.Unlock()
	timed("new-from-snapshot", "POST", "/v1/vms", map[string]any{"name": "d", "snapshot": "snap1", "secrets": []string{}, "no_env": true}, 201)
	timed("fork", "POST", "/v1/vms/"+vm+"/fork", map[string]any{"name": "f"}, 201)
	timed("stop", "POST", "/v1/vms/"+vm+"/stop", nil, 200)
	timed("start", "POST", "/v1/vms/"+vm+"/start", nil, 200)
}

// TestLifecycleWithProductionFlagsDoesNotWaitForFsync enables PublicURL,
// VMAPIURL, AIProxyURL (with a team AI credential), and SSHPolicyHelperFile so
// brand-new single and batch creates (native and compat without Idempotency-Key)
// plus fork/stop/start when no live self/AI credential requires durable copy
// revocation do not block on fsync.
func TestLifecycleWithProductionFlagsDoesNotWaitForFsync(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	e.srv.cfg.PublicURL = "https://api.example.test"
	e.srv.cfg.VMAPIURL = "https://api.example.test"
	e.srv.cfg.AIProxyURL = "https://ai.example.test"
	if st, out := e.do(e.owner, "PUT", "/v1/secrets/TOKEN", map[string]any{"value": "s3cret"}); st/100 != 2 {
		t.Fatalf("secret: %d %v", st, out)
	}
	if st, out := e.do(e.owner, "PUT", "/v1/ai/credentials/openai", map[string]any{"value": "sk-synthetic-prod-key"}); st != 200 {
		t.Fatalf("ai cred: %d %v", st, out)
	}
	_, noSelfTok := e.newTeam("prod-noself")
	slowFsync(t, 300*time.Millisecond)
	timedAs := func(tok, name, method, path string, body any, want int) map[string]any {
		t.Helper()
		t0 := time.Now()
		st, out := e.do(tok, method, path, body)
		took := time.Since(t0)
		if st != want {
			t.Fatalf("%s: %d %v", name, st, out)
		}
		if took > 200*time.Millisecond {
			t.Errorf("%s took %v under 300ms fsync (production flags): something waited for disk sync", name, took)
		}
		return out
	}
	vm := timedAs(e.owner, "prod create", "POST", "/v1/vms", map[string]any{"name": "prod-a"}, 201)["id"].(string)
	batch := timedAs(e.owner, "prod batch create", "POST", "/v1/vms", map[string]any{"name": "prod-batch", "count": 4}, 201)
	if vms, _ := batch["vms"].([]any); len(vms) != 4 {
		t.Fatalf("batch vms: %v", batch)
	}
	timedAs(e.owner, "prod stop", "POST", "/v1/vms/"+vm+"/stop", nil, 200)
	timedAs(e.owner, "prod start", "POST", "/v1/vms/"+vm+"/start", nil, 200)

	// Create, stop, and start on a self_api:false VM avoid fsync; with SSH
	// enabled, fork durably rotates the parent's SSH lookup token in 1 writeAtomic
	// (~600ms under 300ms syncDelay, never 2+ >=1200ms), and without SSH it avoids fsync too.
	nsVM := timedAs(noSelfTok, "prod noself create", "POST", "/v1/vms", map[string]any{"name": "ns-a", "self_api": false}, 201)["id"].(string)
	if err := e.srv.store.Flush(); err != nil {
		t.Fatalf("flush prior background Apply state before timing noself fork: %v", err)
	}
	writesBeforeForkSSH := e.srv.store.writes.Load()
	tForkSSH := time.Now()
	stForkSSH, outForkSSH := e.do(noSelfTok, "POST", "/v1/vms/"+nsVM+"/fork", map[string]any{"name": "ns-fork"})
	if took, w := time.Since(tForkSSH), e.srv.store.writes.Load()-writesBeforeForkSSH; stForkSSH != 201 || w != 1 || took > 950*time.Millisecond {
		t.Errorf("prod noself fork (with SSH) status=%d writes=%d took=%v out=%v (want 1 writeAtomic)", stForkSSH, w, took, outForkSSH)
	}
	timedAs(noSelfTok, "prod noself stop", "POST", "/v1/vms/"+nsVM+"/stop", nil, 200)
	timedAs(noSelfTok, "prod noself start", "POST", "/v1/vms/"+nsVM+"/start", nil, 200)
	savedSSHHelper := e.srv.cfg.SSHPolicyHelperFile
	e.srv.cfg.SSHPolicyHelperFile = ""
	nsNoSSH := timedAs(noSelfTok, "prod noself-nossh create", "POST", "/v1/vms", map[string]any{"name": "ns-nossh", "self_api": false}, 201)["id"].(string)
	timedAs(noSelfTok, "prod noself-nossh fork", "POST", "/v1/vms/"+nsNoSSH+"/fork", map[string]any{"name": "ns-nossh-fork"}, 201)
	e.srv.cfg.SSHPolicyHelperFile = savedSSHHelper

	t0 := time.Now()
	st, cOut, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "compat-prod"}, nil)
	if took := time.Since(t0); st != 202 || took > 200*time.Millisecond {
		t.Errorf("compat create status=%d took=%v out=%v", st, took, cOut)
	}
	cID := cOut["sandbox"].(map[string]any)["id"].(string)
	cVM := e.srv.store.Snapshot().CompatIDs[cID].VM
	timedAs(e.owner, "native stop before compat resume", "POST", "/v1/vms/"+cVM+"/stop", nil, 200)
	t0 = time.Now()
	st, crOut, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+cID+"/resume", nil, nil)
	if took := time.Since(t0); st != 202 || took > 200*time.Millisecond {
		t.Errorf("compat resume status=%d took=%v out=%v", st, took, crOut)
	}

	e.r39Snapshot(e.createVM(), "cb-src") // count above 1 clones a snapshot (r43)
	t0 = time.Now()
	st, cbOut, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "cb", "count": 4, "from": "cb-src"}, nil)
	if took := time.Since(t0); st != 202 || took > 200*time.Millisecond {
		t.Errorf("compat batch create status=%d took=%v out=%v", st, took, cbOut)
	}
}

func TestCompatStopSingleWriteAtomic(t *testing.T) {
	e := setup(t)
	cID := e.newSandbox(map[string]any{"name": "single-sync-stop"})
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	// Each writeAtomic performs 2 syncFile calls (temp file + parent dir).
	// Under 200ms syncDelay, 1 writeAtomic takes ~400ms whereas 2 writeAtomics take >=800ms.
	slowFsync(t, 200*time.Millisecond)
	t0 := time.Now()
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+cID+"/stop", nil, nil)
	took := time.Since(t0)
	if st != 202 {
		t.Fatalf("compat stop: %d %v", st, out)
	}
	if took > 650*time.Millisecond {
		t.Fatalf("compat stop took %v under 200ms syncDelay (want 1 writeAtomic ~400ms, not 2 >=800ms)", took)
	}
}

// TestSelectionSurvivesLostStoreWrite: the store write after create no longer
// waits for the disk, so a crash can lose it. The selection label on the boatd
// record must still keep a start from injecting every team secret.
func TestSelectionSurvivesLostStoreWrite(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TOKEN", map[string]any{"value": "s3cret"})
	e.do(e.owner, "PUT", "/v1/secrets/OTHER", map[string]any{"value": "o"})
	_, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "a", "secrets": []string{"OTHER"}})
	vm := out["id"].(string)
	_, out = e.do(e.owner, "POST", "/v1/vms/"+vm+"/fork", map[string]any{"name": "f"})
	child := out["id"].(string)
	_, out = e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "n", "secrets": []string{}})
	none := out["id"].(string)
	// The crash: the API comes back without the records.
	e.srv.store.Update(func(st *State) error { st.VMSecrets = map[string][]string{}; return nil })
	for id, want := range map[string]string{vm: "o", child: "o", none: ""} {
		e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
		if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil); st != 200 {
			t.Fatalf("start %s: %d %v", id, st, out)
		}
		e.boatd.mu.Lock()
		env, _ := e.boatd.starts[len(e.boatd.starts)-1]["env"].(map[string]any)
		e.boatd.mu.Unlock()
		if _, leaked := env["TOKEN"]; leaked {
			t.Errorf("%s: start injected TOKEN, outside its selection: %v", id, env)
		}
		if got, _ := env["OTHER"].(string); got != want {
			t.Errorf("%s: OTHER = %q, want %q", id, got, want)
		}
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"labels": map[string]string{secretsLabel: ""}}); st != 400 {
		t.Errorf("a caller set the reserved selection label: %d %v", st, out)
	}
}

// TestApplyFlushes: what Apply leaves in memory reaches the disk.
func TestApplyFlushes(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.Apply(func(st *State) error { st.VMEnvs["vm1"] = "env1"; return nil })
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Snapshot().VMEnvs["vm1"] != "env1" {
		t.Fatal("Apply's change is not on disk after Flush")
	}
	// An older state never overwrites a newer one.
	s.Update(func(st *State) error { st.VMEnvs["vm2"] = "env2"; return nil })
	old := *s.Snapshot()
	old.seq--
	s.persist(&old)
	s3, _ := OpenStore(dir)
	if s3.Snapshot().VMEnvs["vm2"] != "env2" {
		t.Fatal("a stale state overwrote a newer one")
	}
}

// BenchmarkCreate: boat-api's time per create with an instant boatd.
func BenchmarkCreate(b *testing.B) {
	t := &testing.T{}
	e := setup(t)
	e.t = t
	for i := 0; i < b.N; i++ {
		e.do(e.owner, "POST", "/v1/vms", map[string]any{"secrets": []string{}, "no_env": true})
	}
}

// TestCloseKeepsAdminInit is boat-real's 401 (boat-e2e 42628046892): boat-api
// runs with the state it loaded, admin init adds the operator key behind it,
// then the service restarts. The old process's Close must not write its
// unchanged state back over admin init's file.
func TestCloseKeepsAdminInit(t *testing.T) {
	dir := t.TempDir()
	kr, err := NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	running, err := OpenSealedStore(dir, kr) // the server, before admin init
	if err != nil {
		t.Fatal(err)
	}
	initStore, err := OpenSealedStore(dir, kr) // boat-api admin init, another process
	if err != nil {
		t.Fatal(err)
	}
	k, err := bootstrap(initStore, kr, "me", "ops")
	if err != nil {
		t.Fatal(err)
	}
	initStore.Close()
	if err := running.Close(); err != nil { // SIGTERM of the old server
		t.Fatal(err)
	}
	after, err := OpenSealedStore(dir, kr) // the restarted server
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { after.Close() })
	id, _, _ := ParseToken(k.Token)
	if after.Snapshot().Keys[id] == nil {
		t.Fatal("the old server's shutdown wrote its state over admin init's key")
	}
}
