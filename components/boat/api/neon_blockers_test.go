package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNeonPerfCompatHotPathsDoNotWaitForFsync verifies that compatStop (success
// and failure), deletions.add, setCompatSetup, and recordDisplayName update
// in-memory state immediately via Store.Apply without stalling on fsync.
func TestNeonPerfCompatHotPathsDoNotWaitForFsync(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(map[string]any{"name": "hot-path"})
	vmID := e.nativeOf(bx)
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}

	const fsyncDelay = 600 * time.Millisecond
	const budget = 250 * time.Millisecond
	slowFsync(t, fsyncDelay)

	// 1. recordDisplayName (PATCH /v1/vms/{id}) does not wait for fsync.
	t0 := time.Now()
	st, out := e.do(e.owner, "PATCH", "/v1/vms/"+vmID, map[string]any{"name": "Display Worker"})
	if took := time.Since(t0); st != 200 || out["name"] != "Display Worker" || took > budget {
		t.Fatalf("PATCH display name: status=%d name=%v took=%v (budget %v)", st, out["name"], took, budget)
	}
	if got := e.srv.store.Snapshot().VMNames[vmID]; got != "Display Worker" {
		t.Fatalf("VMNames[%s] = %q, want Display Worker", vmID, got)
	}

	// 2. compatStop (success) does not wait for fsync.
	t0 = time.Now()
	st, out, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/stop", nil, nil)
	if took := time.Since(t0); st != 202 || took > budget {
		t.Fatalf("compatStop success: status=%d took=%v (budget %v) out=%v", st, took, budget, out)
	}
	stopOp, _ := out["operation"].(map[string]any)
	if stopOp == nil || stopOp["status"] != "completed" {
		t.Fatalf("compatStop op = %v, want status=completed", stopOp)
	}

	// 3. compatStop (failing upstream) uses failAsync and does not wait for fsync.
	restore := r26Proxy(e, func(r *http.Request) bool {
		return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop")
	})
	t0 = time.Now()
	st, _, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/stop", nil, nil)
	restore()
	if took := time.Since(t0); st != http.StatusBadGateway || took > budget {
		t.Fatalf("compatStop failure: status=%d took=%v (budget %v)", st, took, budget)
	}
	if last, _ := e.srv.stops.last(vmID).(map[string]any); last == nil || last["status"] != "failing" {
		t.Fatalf("compatStop failing last = %v, want status=failing", last)
	}

	// 4. setCompatSetup (create with files) does not wait for fsync.
	t0 = time.Now()
	st, out, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{
		"name":  "setup-fast",
		"files": []map[string]any{{"path": "hello.txt", "content": "hi"}},
	}, nil)
	if took := time.Since(t0); st != 202 || took > budget {
		t.Fatalf("compat create with files: status=%d took=%v (budget %v) out=%v", st, took, budget, out)
	}
	setupBX := out["sandbox"].(map[string]any)["id"].(string)
	setupVM := e.nativeOf(setupBX)
	if cs := e.srv.store.Snapshot().CompatSetups[setupVM]; cs == nil || cs.Status != "done" {
		t.Fatalf("CompatSetups[%s] = %+v, want done", setupVM, cs)
	}

	// 5. deletions.add does not wait for fsync and is immediately visible.
	delOp := newDeletionOp("sandbox", bx)
	t0 = time.Now()
	e.srv.deletions.add(e.teamID(), delOp)
	if took := time.Since(t0); took > budget {
		t.Fatalf("deletions.add took %v (budget %v)", took, budget)
	}
	if got := e.srv.deletions.get(e.teamID(), delOp["id"].(string)); got == nil || got["targetId"] != bx {
		t.Fatalf("deletions.get = %v, want targetId=%s", got, bx)
	}
}

// TestNeonStoreApplyNotStalledBehindFlusherWhenUpdateWaits verifies that while
// flusher is mid-fsync (holding s.wmu), a queued Store.Update waits outside
// s.mu so non-blocking Store.Apply and Snapshot complete immediately, with
// linearizable final state and zero races.
func TestNeonStoreApplyNotStalledBehindFlusherWhenUpdateWaits(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Clean Flush should do zero writes.
	before := st.writes.Load()
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := st.writes.Load(); got != before {
		t.Fatalf("clean Flush incremented writes: %d -> %d", before, got)
	}

	const fsyncDelay = 450 * time.Millisecond
	slowFsync(t, fsyncDelay)

	// Trigger a background flush that will hold s.wmu for ~2*fsyncDelay.
	if err := st.Apply(func(s *State) error { s.VMEnvs["vm-bg"] = "bg"; return nil }); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !st.writing.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !st.writing.Load() {
		t.Fatal("flusher did not enter writeAtomic")
	}

	// Queue a synchronous Update while flusher is mid-fsync.
	updateDone := make(chan error, 1)
	go func() {
		updateDone <- st.Update(func(s *State) error {
			s.VMEnvs["vm-sync"] = "sync"
			return nil
		})
	}()
	time.Sleep(25 * time.Millisecond) // let Update block on s.wmu before s.mu

	// Non-blocking Apply and Snapshot must not stall behind the flusher's fsync.
	t0 := time.Now()
	if err := st.Apply(func(s *State) error { s.VMEnvs["vm-fast"] = "fast"; return nil }); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(t0); took > 200*time.Millisecond {
		t.Fatalf("Store.Apply stalled %v behind flusher+queued Update (want < 200ms)", took)
	}
	if snap := st.Snapshot(); snap.VMEnvs["vm-fast"] != "fast" {
		t.Fatalf("Snapshot missing fast Apply: %v", snap.VMEnvs)
	}

	if err := <-updateDone; err != nil {
		t.Fatalf("queued Update failed: %v", err)
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}

	re, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	got := re.Snapshot().VMEnvs
	for k, want := range map[string]string{"vm-bg": "bg", "vm-fast": "fast", "vm-sync": "sync"} {
		if got[k] != want {
			t.Errorf("persisted VMEnvs[%q] = %q, want %q", k, got[k], want)
		}
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Flush(); !errors.Is(err, errStoreClosed) {
		t.Fatalf("Flush after Close = %v, want errStoreClosed", err)
	}
}

// TestNeonSecurityCreateShareFailsClosedWithAIAndSelfOrSSH verifies that
// createShare fails closed (rolls back st.SnapShares and returns 502
// credential_push_failed) whenever selfGen > 0 or SSHPolicyHelperFile is set,
// EVEN WHEN refreshAI is true.
func TestNeonSecurityCreateShareFailsClosedWithAIAndSelfOrSSH(t *testing.T) {
	cases := []struct {
		name      string
		enableSSH bool
		dropSelf  bool
	}{
		{name: "ai_and_ssh_helper", enableSSH: true, dropSelf: true},
		{name: "ai_and_self_token", enableSSH: false, dropSelf: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			e.srv.cfg.VMAPIURL = "https://api.example.test"
			e.srv.cfg.AIProxyURL = "https://ai.example.test"
			if tc.enableSSH {
				e.enableSSH()
			}
			if st, out := e.do(e.owner, "PUT", "/v1/ai/credentials/openai", map[string]any{"value": "sk-synthetic-test-key"}); st != 200 {
				t.Fatalf("ai cred: %d %v", st, out)
			}
			vmID := e.createVM()
			sid := e.snapshotOf(vmID)
			path := "/v1/vms/" + vmID + "/snapshots/" + sid + "/shares"

			if e.srv.store.Snapshot().AIVMs[vmID] == nil {
				t.Fatalf("expected AIVMs[%s] to be present", vmID)
			}
			if tc.dropSelf {
				if err := e.srv.store.Update(func(st *State) error { delete(st.VMTokens, vmID); return nil }); err != nil {
					t.Fatal(err)
				}
			} else if e.srv.store.Snapshot().VMTokens[vmID] == nil {
				t.Fatalf("expected VMTokens[%s] to be present", vmID)
			}

			restore := r26Proxy(e, func(r *http.Request) bool {
				return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/vms/"+vmID+"/env")
			})
			st, out := e.do(e.owner, "POST", path, map[string]any{})
			restore()

			if st != http.StatusBadGateway || nativeErrCode(out) != "credential_push_failed" {
				t.Fatalf("createShare with failing push = %d %v, want 502 credential_push_failed", st, out)
			}
			if n := len(e.srv.store.Snapshot().SnapShares); n != 0 {
				t.Fatalf("failed push left %d SnapShares, want 0 (rolled back)", n)
			}

			// When push succeeds, share creation returns 201.
			st, out = e.do(e.owner, "POST", path, map[string]any{})
			if st != http.StatusCreated {
				t.Fatalf("createShare after push recovery = %d %v, want 201", st, out)
			}
		})
	}
}

// TestNeonSecurityCompatFileUploadWatchAliveRevocation verifies that revoking
// the key that minted a compat upload URL cancels an in-flight putVMFile call
// via watchAlive.
func TestNeonSecurityCompatFileUploadWatchAliveRevocation(t *testing.T) {
	e := setup(t)
	e.srv.streamRecheck = 50 * time.Millisecond
	bx := e.newSandbox(nil)
	vmID := e.nativeOf(bx)

	st, k := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "upload-minter"})
	if st != 201 {
		t.Fatalf("create key: %d %v", st, k)
	}
	code, up, _ := e.compat(k["token"].(string), "POST", "/api/v1/sandboxes/"+bx+"/files/upload-url",
		map[string]any{"path": "secret.txt", "ttlSeconds": 600}, nil)
	if code != 200 {
		t.Fatalf("upload-url: %d %v", code, up)
	}

	enteredPut := make(chan struct{})
	putCanceled := make(chan struct{})
	var once sync.Once
	orig := e.srv.boatd.hc.Transport
	e.srv.boatd.hc.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/v1/vms/"+vmID+"/files") {
			once.Do(func() { close(enteredPut) })
			<-r.Context().Done()
			close(putCanceled)
			return nil, r.Context().Err()
		}
		if orig == nil {
			return http.DefaultTransport.RoundTrip(r)
		}
		return orig.RoundTrip(r)
	})
	t.Cleanup(func() { e.srv.boatd.hc.Transport = orig })

	uploadStatus := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPut, up["url"].(string), strings.NewReader("payload"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			uploadStatus <- 0
			return
		}
		resp.Body.Close()
		uploadStatus <- resp.StatusCode
	}()

	select {
	case <-enteredPut:
	case <-time.After(3 * time.Second):
		t.Fatal("upload did not reach putVMFile")
	}

	// While key is alive, upload must remain open.
	select {
	case <-putCanceled:
		t.Fatal("putVMFile canceled before key revocation")
	case <-time.After(150 * time.Millisecond):
	}

	// Revoke the minting key: watchAlive must cancel putVMFile promptly.
	if st, out := e.do(e.owner, "DELETE", "/v1/keys/"+k["id"].(string), nil); st != 200 {
		t.Fatalf("revoke key: %d %v", st, out)
	}
	select {
	case <-putCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("putVMFile was not canceled after minting key revocation")
	}
	select {
	case status := <-uploadStatus:
		if status == 200 {
			t.Fatalf("revoked upload returned 200, want failure")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upload HTTP request did not complete after cancellation")
	}
}

// TestNeonSecurityMarkEnvNeverLandedFailsClosed verifies that when
// markEnvNeverLanded cannot durably persist the mark, it returns an error,
// still sets the mark in memory, and failClosedCreate/failClosedFork refuse
// to keep an unpersisted-mark stopped VM.
func TestNeonSecurityMarkEnvNeverLandedFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only directory")
	}
	e := setup(t)
	vmID := e.createVM()
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.dir, 0o700) })

	if err := e.srv.markEnvNeverLanded(vmID); err == nil {
		t.Fatal("markEnvNeverLanded succeeded on read-only store directory")
	}
	if !e.srv.envNeverLanded(vmID) {
		t.Fatal("markEnvNeverLanded did not apply in-memory fallback mark")
	}

	// failClosedCreate must not return closedStopped when mark persistence fails.
	req := httptest.NewRequest(http.MethodPost, "/v1/vms", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, &reqInfo{id: "req_test"}))
	res := e.srv.failClosedCreate(req, vmID, VM{"id": vmID, "state": "running"})
	if res.kind == closedStopped {
		t.Fatalf("failClosedCreate returned closedStopped despite failed mark persistence; want closedStuck or closedDeleted")
	}
	if got := e.boatdState(vmID); got != "stopped" && got != "" {
		t.Fatalf("VM state after failClosedCreate = %q, want stopped or deleted", got)
	}

	// failClosedFork must also refuse to keep an unpersisted-mark stopped child.
	w := httptest.NewRecorder()
	e.srv.failClosedFork(w, req, vmID, runMark{}, VM{"id": vmID, "state": "running"}, "env push failed", func() {})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("failClosedFork with unwritable store = %d %s, want 500 cleanup_failed", w.Code, w.Body.String())
	}
}

// TestNeonParityCompatCatalogMaxTTL verifies GET /api/v1/api-keys advertises
// maxTtl: "365d" matching parseKeyTTL.
func TestNeonParityCompatCatalogMaxTTL(t *testing.T) {
	e := setup(t)
	list := e.ok("GET", "/api/v1/api-keys", nil, 200)
	cat, _ := list["catalog"].(map[string]any)
	if cat["maxTtl"] != "365d" {
		t.Fatalf("catalog.maxTtl = %v, want 365d", cat["maxTtl"])
	}
	if cat["defaultTtl"] != "never" {
		t.Fatalf("catalog.defaultTtl = %v, want never", cat["defaultTtl"])
	}
	if st, out, _ := e.compat(e.owner, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "max", "preset": "read-only", "ttl": "365d"}, nil); st != 201 {
		t.Fatalf("365d ttl create: %d %v", st, out)
	}
	if st, _, _ := e.compat(e.owner, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "over", "preset": "read-only", "ttl": "366d"}, nil); st != 400 {
		t.Fatalf("366d ttl create = %d, want 400", st)
	}
}

// TestNeonParityCompatCreateSkipsSetupOnEnvError verifies that compat create
// (single and batch) does not write files or run setupScript on VMs whose env
// failed (env_error != "", state == "error").
func TestNeonParityCompatCreateSkipsSetupOnEnvError(t *testing.T) {
	e := setup(t)
	srcVM := e.createVM()
	e.r39Snapshot(srcVM, "env-err-snap")

	e.setEnvFailures(true, false)
	defer e.setEnvFailures(false, false)

	// Single create with files + setupScript.
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{
		"name":        "err-single",
		"files":       []map[string]any{{"path": "should-not-exist.txt", "content": "nope"}},
		"setupScript": "echo should-not-run",
	}, nil)
	if st != 202 {
		t.Fatalf("single compat create: %d %v", st, out)
	}
	if out["status"] != "provisioning" {
		t.Fatalf("status = %v, want provisioning", out["status"])
	}
	sb := out["sandbox"].(map[string]any)
	if sb["state"] != "error" || sb["error"] == nil {
		t.Fatalf("sandbox = %v, want state=error", sb)
	}
	singleVM := e.nativeOf(sb["id"].(string))

	// Batch create with files + setupScript.
	st, bout, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{
		"name":        "err-batch",
		"count":       2,
		"from":        "env-err-snap",
		"files":       []map[string]any{{"path": "batch-no.txt", "content": "nope"}},
		"setupScript": "echo batch-no",
	}, nil)
	if st != 202 {
		t.Fatalf("batch compat create: %d %v", st, bout)
	}
	if bout["status"] != "provisioning" {
		t.Fatalf("batch status = %v, want provisioning", bout["status"])
	}

	// Give any erroneously launched background goroutines a brief moment.
	time.Sleep(50 * time.Millisecond)

	var ids []string
	ids = append(ids, singleVM)
	for _, raw := range bout["sandboxes"].([]any) {
		item := raw.(map[string]any)
		if item["state"] != "error" {
			t.Fatalf("batch item state = %v, want error", item["state"])
		}
		ids = append(ids, e.nativeOf(item["id"].(string)))
	}

	snap := e.srv.store.Snapshot()
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	for _, id := range ids {
		if snap.CompatSetups[id] != nil {
			t.Errorf("CompatSetups[%s] = %+v, want nil on env_error", id, snap.CompatSetups[id])
		}
		for k := range e.boatd.files {
			if strings.HasPrefix(k, id+":") && (strings.HasSuffix(k, "should-not-exist.txt") || strings.HasSuffix(k, "batch-no.txt")) {
				t.Errorf("compatSetupVM wrote file %s on errored VM", k)
			}
		}
		for _, c := range e.boatd.cmds {
			if c.vm == id && len(c.argv) >= 3 && c.argv[0] == "bash" && c.argv[1] == "-lc" {
				t.Errorf("compatSetupVM executed detached setupScript on errored VM %s: %v", id, c.argv)
			}
		}
	}
	for _, ex := range e.boatd.execs {
		if cmd, _ := ex["cmd"].([]any); len(cmd) >= 3 && cmd[0] == "bash" && cmd[1] == "-lc" {
			t.Errorf("compatSetupVM ran setupScript exec on errored VM: %v", ex)
		}
	}
}

// TestNeonParityCompatDeleteSnapshotMatchesNativeAndUUID verifies that
// DELETE /api/v1/snapshots/{sid} matches both native snap_... IDs and
// compat snapshotUUID(...) IDs.
func TestNeonParityCompatDeleteSnapshotMatchesNativeAndUUID(t *testing.T) {
	e := setup(t)
	vmID := e.createVM()
	nativeID1 := e.snapshotOf(vmID)
	nativeID2 := e.snapshotOf(vmID)

	// 1. Delete by native snapshot id (e.g. snap_...).
	st, out, _ := e.compat(e.owner, "DELETE", "/api/v1/snapshots/"+nativeID1, nil,
		map[string]string{"X-Ascii-Confirm-Delete": nativeID1})
	if st != http.StatusAccepted || out["type"] != "snapshot.deleting" {
		t.Fatalf("delete by native id %s = %d %v, want 202 snapshot.deleting", nativeID1, st, out)
	}
	if op := out["operation"].(map[string]any); op["targetId"] != nativeID1 {
		t.Fatalf("native delete targetId = %v, want %s", op["targetId"], nativeID1)
	}

	// 2. Delete by compat snapshotUUID(...).
	uuid2 := snapshotUUID(e.teamID(), nativeID2)
	st, out, _ = e.compat(e.owner, "DELETE", "/api/v1/snapshots/"+uuid2, nil,
		map[string]string{"X-Ascii-Confirm-Delete": uuid2})
	if st != http.StatusAccepted || out["type"] != "snapshot.deleting" {
		t.Fatalf("delete by uuid %s = %d %v, want 202 snapshot.deleting", uuid2, st, out)
	}
	if op := out["operation"].(map[string]any); op["targetId"] != uuid2 {
		t.Fatalf("uuid delete targetId = %v, want %s", op["targetId"], uuid2)
	}

	e.boatd.mu.Lock()
	_, exists1 := e.boatd.snaps[nativeID1]
	_, exists2 := e.boatd.snaps[nativeID2]
	e.boatd.mu.Unlock()
	if exists1 || exists2 {
		t.Fatalf("snapshots still present after delete: exists1=%v exists2=%v", exists1, exists2)
	}
}
