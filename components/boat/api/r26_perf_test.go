package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// r26Delay shortens the lazy idempotency persist window for one test and
// restores it afterwards.
func r26Delay(t *testing.T, d time.Duration) {
	t.Helper()
	old := idemPersistDelay
	idemPersistDelay = d
	t.Cleanup(func() { idemPersistDelay = old })
}

func (e *env) doIdem(token, key, method, path string, body any) (int, map[string]any, http.Header) {
	e.t.Helper()
	return e.compat(token, method, path, body, map[string]string{"Idempotency-Key": key})
}

// hasIdemKey: the stored key is team\x00[scope\x00]key.
func hasIdemKey(st *State, key string) bool {
	for k := range st.Idempotency {
		if strings.HasSuffix(k, "\x00"+key) {
			return true
		}
	}
	return false
}

// waitWrites polls until the store has done at least want writes since base.
func waitWrites(t *testing.T, s *Store, base uint64, want uint64) uint64 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.writes.Load() - base; got >= want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s.writes.Load() - base
}

// A create with an Idempotency-Key must not wait for the idempotency record to
// reach the disk, replays at once, and the record is persisted lazily in one
// coalesced write. Before R26 the record was a synchronous Store.Update
// (writeAtomic + fsync) before the response flushed.
func TestR26IdempotentCreateDoesNotWaitForFsyncAndReplays(t *testing.T) {
	e := setup(t)
	r26Delay(t, 300*time.Millisecond)
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	slowFsync(t, 400*time.Millisecond)
	base := e.srv.store.writes.Load()

	t0 := time.Now()
	st, out, _ := e.doIdem(e.owner, "k-create", "POST", "/v1/vms", map[string]any{"name": "idem-a"})
	took := time.Since(t0)
	if st != 201 {
		t.Fatalf("create: %d %v", st, out)
	}
	if took > 300*time.Millisecond {
		t.Fatalf("idempotent create took %v under 400ms fsync: the response waited for the record's disk write", took)
	}
	if got := e.srv.store.writes.Load() - base; got != 0 {
		t.Fatalf("sync store writes on idempotent create = %d, want 0 (record persists lazily)", got)
	}
	// Replay is served from memory before anything is on disk.
	st2, out2, hdr := e.doIdem(e.owner, "k-create", "POST", "/v1/vms", map[string]any{"name": "idem-a"})
	if st2 != 201 || hdr.Get("Idempotent-Replayed") != "true" || out2["id"] != out["id"] {
		t.Fatalf("replay: %d %v %v", st2, hdr, out2)
	}
	// The lazy write lands within the window: exactly one.
	if got := waitWrites(t, e.srv.store, base, 1); got != 1 {
		t.Fatalf("lazy writes after create = %d, want 1", got)
	}
	time.Sleep(500 * time.Millisecond)
	if got := e.srv.store.writes.Load() - base; got != 1 {
		t.Fatalf("total writes for one idempotent create = %d, want 1", got)
	}
}

// Fork with a key used to write twice synchronously (fork's own record, then
// the idempotency record); it is now one sync write plus one lazy write.
func TestR26IdempotentForkWriteCount(t *testing.T) {
	e := setup(t)
	vm := e.createVM()
	r26Delay(t, 300*time.Millisecond)
	time.Sleep(400 * time.Millisecond) // let create's own background flush settle
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	base := e.srv.store.writes.Load()
	st, out, _ := e.doIdem(e.owner, "k-fork", "POST", "/v1/vms/"+vm+"/fork", map[string]any{"name": "idem-fork"})
	if st != 201 {
		t.Fatalf("fork: %d %v", st, out)
	}
	syncWrites := e.srv.store.writes.Load() - base
	if syncWrites > 1 {
		t.Fatalf("sync store writes on idempotent fork = %d, want <= 1", syncWrites)
	}
	waitWrites(t, e.srv.store, base, 1)
	time.Sleep(600 * time.Millisecond)
	if total := e.srv.store.writes.Load() - base; total > 2 {
		t.Fatalf("total writes for one idempotent fork = %d, want <= 2 (1 sync + 1 lazy)", total)
	}
	if st, _, hdr := e.doIdem(e.owner, "k-fork", "POST", "/v1/vms/"+vm+"/fork", map[string]any{"name": "idem-fork"}); st != 201 || hdr.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("fork replay: %d %v", st, hdr)
	}
}

// Distinct-key concurrent creates must not serialize on fsync.
func TestR26ConcurrentDistinctKeyCreatesDoNotSerializeOnFsync(t *testing.T) {
	e := setup(t)
	r26Delay(t, 300*time.Millisecond)
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	const n = 8
	const fsync = 250 * time.Millisecond
	slowFsync(t, fsync)
	base := e.srv.store.writes.Load()
	var wg sync.WaitGroup
	errs := make(chan string, n)
	t0 := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, out, _ := e.doIdem(e.owner, fmt.Sprintf("k-%d", i), "POST", "/v1/vms", map[string]any{"name": fmt.Sprintf("c%d", i)})
			if st != 201 {
				errs <- fmt.Sprintf("create %d: %d %v", i, st, out)
			}
		}(i)
	}
	wg.Wait()
	took := time.Since(t0)
	close(errs)
	for m := range errs {
		t.Fatal(m)
	}
	// Serialized, each create paid at least one 250ms fsync under the idem
	// mutex/store lock (n * fsync = 2s); now none does.
	if took > 3*fsync {
		t.Fatalf("%d concurrent distinct-key creates took %v (fsync %v): they serialize on the record write", n, took, fsync)
	}
	if got := e.srv.store.writes.Load() - base; got != 0 {
		t.Fatalf("sync writes during concurrent creates = %d, want 0", got)
	}
	waitWrites(t, e.srv.store, base, 1)
	time.Sleep(1200 * time.Millisecond)
	if total := e.srv.store.writes.Load() - base; total > 2 {
		t.Fatalf("%d records took %d writes, want them coalesced (<= 2)", n, total)
	}
	snap := e.srv.store.Snapshot()
	for i := 0; i < n; i++ {
		if !hasIdemKey(snap, fmt.Sprintf("k-%d", i)) {
			t.Fatalf("record k-%d missing from the store", i)
		}
	}
}

// Closing the store persists records still inside the lazy window.
func TestR26LazyIdemRecordSurvivesGracefulClose(t *testing.T) {
	e := setup(t)
	r26Delay(t, time.Hour)
	st, out, _ := e.doIdem(e.owner, "k-close", "POST", "/v1/vms", map[string]any{"name": "close"})
	if st != 201 {
		t.Fatalf("create: %d %v", st, out)
	}
	if err := e.srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	re, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	if !hasIdemKey(re.Snapshot(), "k-close") {
		t.Fatal("idempotency record lost across a graceful Close")
	}
}

// deleteVM: one durable write (the credential retirement that must precede the
// delete) plus one background write; no host-wide VM listing and no SSH policy
// re-attestation. Before R26: retireVM + forgetVM + syncJumpKeys Updates, a GET
// /v1/vms scan and a re-attest of every running VM.
func TestR26DeleteVMOneDurableWriteNoHostScan(t *testing.T) {
	e := setup(t)
	vm := e.createVM()
	other := e.createVM()
	// An owned grant on both VMs, and one on the deleted VM only.
	if err := e.srv.store.Update(func(st *State) error {
		st.SSHGrants["g-both"] = &SSHGrant{TeamID: e.teamID(), PublicKey: "ssh-ed25519 AAAA", VMs: []string{vm, other}, KeyID: "key1"}
		st.SSHGrants["g-only"] = &SSHGrant{TeamID: e.teamID(), PublicKey: "ssh-ed25519 BBBB", VMs: []string{vm}, KeyID: "key2"}
		st.SSHGrants["g-legacy"] = &SSHGrant{TeamID: e.teamID(), PublicKey: "ssh-ed25519 CCCC", VMs: []string{vm}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var reqs []string
	orig := e.srv.boatd.hc.Transport
	e.srv.boatd.hc.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		reqs = append(reqs, req.Method+" "+req.URL.Path)
		mu.Unlock()
		return orig.RoundTrip(req)
	})
	t.Cleanup(func() { e.srv.boatd.hc.Transport = orig })
	time.Sleep(100 * time.Millisecond)
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	base := e.srv.store.writes.Load()
	baseSeq := e.srv.store.Snapshot().seq

	if st, out := e.do(e.owner, "DELETE", "/v1/vms/"+vm, nil); st != 200 {
		t.Fatalf("delete: %d %v", st, out)
	}
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := e.srv.store.writes.Load() - base; got > 2 {
		t.Fatalf("deleteVM store writes = %d, want <= 2 (1 durable retire + 1 coalesced background)", got)
	}
	if got := e.srv.store.Snapshot().seq - baseSeq; got != 2 {
		t.Fatalf("deleteVM store transactions = %d, want 2 (retire, forget)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, r := range reqs {
		if r == "GET /v1/vms" || strings.HasSuffix(r, "/ssh-policy") || strings.HasSuffix(r, "/ssh-policy-batch") {
			t.Fatalf("deleteVM made a host scan / re-attest request: %v", reqs)
		}
	}
	st := e.srv.store.Snapshot()
	if st.SSHGrants["g-only"] != nil {
		t.Fatal("grant for the deleted VM alone was not removed")
	}
	if g := st.SSHGrants["g-both"]; g == nil || len(g.VMs) != 1 || g.VMs[0] != other {
		t.Fatalf("shared grant = %+v, want only %s", g, other)
	}
	if st.SSHGrants["g-legacy"] == nil {
		t.Fatal("unowned historical grant must be kept")
	}
	if tok := st.VMTokens[vm]; tok != nil && !tok.Retired {
		t.Fatal("deleted VM's token not retired")
	}
}

// createVM pushes the env to the guest exactly once (set_env over vsock is a
// round trip each), and it carries the per-VM extras.
func TestR26CreateSendsEnvOnce(t *testing.T) {
	e := setup(t)
	if st, out := e.do(e.owner, "PUT", "/v1/secrets/TOKEN", map[string]any{"value": "s3cret", "inject": true}); st/100 != 2 {
		t.Fatalf("secret: %d %v", st, out)
	}
	e.boatd.mu.Lock()
	e.boatd.envOps = nil
	e.boatd.mu.Unlock()
	st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "once", "env": map[string]any{"EXTRA": "1"}})
	if st != 201 {
		t.Fatalf("create: %d %v", st, out)
	}
	id := out["id"].(string)
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	last := e.boatd.creates[len(e.boatd.creates)-1]
	if env, _ := last["env"].(map[string]any); len(env) != 0 {
		t.Fatalf("boot request still carries env %v: it would be sent a second time", env)
	}
	if len(e.boatd.envOps) != 1 {
		t.Fatalf("create sent set_env %d times: %v, want 1", len(e.boatd.envOps), e.boatd.envOps)
	}
	vars, _ := e.boatd.envOps[0]["vars"].(map[string]any)
	if e.boatd.envOps[0]["vm"] != id || vars["BOAT_ID"] != id || vars["EXTRA"] != "1" || vars["TOKEN"] != "s3cret" {
		t.Fatalf("env op = %v, want BOAT_ID, EXTRA and TOKEN for %s", e.boatd.envOps[0], id)
	}
}

// Secrets selection: absent injects the @default (Inject-marked) secrets, an
// empty non-nil list injects none, a named list injects exactly those.
func TestR26SecretsNilVsEmptySelection(t *testing.T) {
	e := setup(t)
	for name, inject := range map[string]bool{"DEF": true, "NOTDEF": false} {
		if st, out := e.do(e.owner, "PUT", "/v1/secrets/"+name, map[string]any{"value": "v-" + name, "inject": inject}); st/100 != 2 {
			t.Fatalf("secret %s: %d %v", name, st, out)
		}
	}
	envOf := func(body map[string]any) map[string]any {
		t.Helper()
		e.boatd.mu.Lock()
		e.boatd.envOps = nil
		e.boatd.mu.Unlock()
		st, out := e.do(e.owner, "POST", "/v1/vms", body)
		if st != 201 {
			t.Fatalf("create %v: %d %v", body, st, out)
		}
		e.boatd.mu.Lock()
		defer e.boatd.mu.Unlock()
		if len(e.boatd.envOps) != 1 {
			t.Fatalf("create %v sent %d env ops, want 1", body, len(e.boatd.envOps))
		}
		vars, _ := e.boatd.envOps[0]["vars"].(map[string]any)
		return vars
	}
	if v := envOf(map[string]any{"name": "nil-sel"}); v["DEF"] != "v-DEF" || v["NOTDEF"] != nil {
		t.Fatalf("absent secrets: env = %v, want @default (DEF) only", v)
	}
	if v := envOf(map[string]any{"name": "empty-sel", "secrets": []string{}}); v["DEF"] != nil || v["NOTDEF"] != nil {
		t.Fatalf("empty secrets: env = %v, want no team secrets", v)
	}
	if v := envOf(map[string]any{"name": "named-sel", "secrets": []string{"NOTDEF"}}); v["NOTDEF"] != "v-NOTDEF" || v["DEF"] != nil {
		t.Fatalf("named secrets: env = %v, want NOTDEF only", v)
	}
}

// A failed env push (secrets, BOAT_ID/token) must be visible on the create
// response, not reported as a ready VM that silently lacks its environment.
func TestR26CreateReportsFailedEnvPush(t *testing.T) {
	e := setup(t)
	e.boatd.mu.Lock()
	e.boatd.envFail = true
	e.boatd.mu.Unlock()
	st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "noenv-push", "env": map[string]any{"EXTRA": "1"}})
	if st != 201 {
		t.Fatalf("create: %d %v", st, out)
	}
	if out["agent_ready"] != false || out["env_error"] == nil {
		t.Fatalf("create with failed env push = %v, want agent_ready=false and env_error", out)
	}
}
