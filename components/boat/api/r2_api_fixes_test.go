package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 1a. Environment-wide upgrade skips & prunes 404 VMEnvs instead of aborting.
func TestR2EnvWideUpgradeSkipsAndPrunes404VMEnvs(t *testing.T) {
	e := setup(t)
	e.saveEnv("web", map[string]string{"FOO": "v1"}, []string{})
	liveVM := e.createVMWith(map[string]any{"environment": "web"})
	envID := e.srv.store.Snapshot().VMEnvs[liveVM]
	if envID == "" {
		t.Fatal("missing envID")
	}
	// Inject a stale VMEnvs/VMEnvPins entry for a VM missing from boatd.
	if err := e.srv.store.Update(func(st *State) error {
		st.VMEnvs["vm-stale-404"] = envID
		st.VMEnvPins["vm-stale-404"] = &EnvPin{Version: 1, Vars: map[string]string{"FOO": "v1"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.putEnvVar("web", "FOO", "v2")

	// Explicit agentIds targeting the missing VM must still return 404.
	if st, _ := e.do(e.owner, "POST", "/v1/environments/web/upgrade", map[string]any{"agentIds": []string{"vm-stale-404"}}); st != 404 {
		t.Fatalf("explicit stale agentId = %d, want 404", st)
	}
	// Environment-wide upgrade must succeed, upgrade liveVM, and prune vm-stale-404.
	st, out := e.do(e.owner, "POST", "/v1/environments/web/upgrade", nil)
	if st != 200 || out["upgraded_live"] != float64(1) || out["failed"] != float64(0) {
		t.Fatalf("env-wide upgrade = %d %v", st, out)
	}
	if e.guestVar(liveVM, "FOO") != "v2" {
		t.Fatalf("liveVM FOO = %v, want v2", e.guestVar(liveVM, "FOO"))
	}
	snap := e.srv.store.Snapshot()
	if snap.VMEnvs["vm-stale-404"] != "" || snap.VMEnvPins["vm-stale-404"] != nil {
		t.Fatalf("stale VMEnvs/VMEnvPins not pruned: env=%q pin=%v", snap.VMEnvs["vm-stale-404"], snap.VMEnvPins["vm-stale-404"])
	}
}

// 1b. Failed !freshBoot fork release cleans up childID and removes orphan VMEnvs/VMEnvPins.
func TestR2ForkReleaseFailureCleansOrphanVMEnvs(t *testing.T) {
	e := setup(t)
	e.saveEnv("web", map[string]string{"FOO": "v1"}, []string{})
	parent := e.createVMWith(map[string]any{"environment": "web"})
	envID := e.srv.store.Snapshot().VMEnvs[parent]

	e.boatd.mu.Lock()
	e.boatd.failRelease = 1
	e.boatd.mu.Unlock()

	st, out := e.do(e.owner, "POST", vmPath(parent, "fork"), map[string]any{"name": "broken-child"})
	if st != 502 {
		t.Fatalf("failed fork release = %d %v, want 502", st, out)
	}
	// No orphan VMEnvs/VMEnvPins or boatd VM left behind.
	snap := e.srv.store.Snapshot()
	count := 0
	for id, eid := range snap.VMEnvs {
		if eid == envID {
			count++
			if id != parent {
				t.Errorf("orphan VMEnvs entry left behind: %s", id)
			}
		}
	}
	if count != 1 {
		t.Fatalf("VMEnvs count for env = %d, want 1", count)
	}
	for id := range snap.VMEnvPins {
		if id != parent {
			t.Errorf("orphan VMEnvPins entry left behind: %s", id)
		}
	}
	if n := e.boatdVMCount(); n != 1 {
		t.Fatalf("boatd VM count = %d, want 1 (unreleased child should be discarded)", n)
	}
	// Subsequent env-wide upgrade succeeds cleanly.
	e.putEnvVar("web", "FOO", "v2")
	st, out = e.do(e.owner, "POST", "/v1/environments/web/upgrade", nil)
	if st != 200 || out["upgraded_live"] != float64(1) || out["failed"] != float64(0) {
		t.Fatalf("upgrade after failed fork = %d %v", st, out)
	}
}

// 1c. Startup reconcile prunes dead VMEnvs and VMEnvPins so versions[].sandboxCount is accurate.
func TestR2ReconcilePrunesDeadVMEnvsAndPins(t *testing.T) {
	e := setup(t)
	e.saveEnv("web", map[string]string{"FOO": "v1"}, []string{})
	liveVM := e.createVMWith(map[string]any{"environment": "web"})
	envID := e.srv.store.Snapshot().VMEnvs[liveVM]
	if err := e.srv.store.Update(func(st *State) error {
		st.VMEnvs["vm-dead-1"] = envID
		st.VMEnvPins["vm-dead-1"] = &EnvPin{Version: 1, Vars: map[string]string{"FOO": "v1"}}
		st.VMEnvPins["vm-dead-pin-only"] = &EnvPin{Version: 1, Vars: map[string]string{"FOO": "v1"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.reconcileCopyCredentials(context.Background()); err != nil {
		t.Fatalf("reconcileCopyCredentials: %v", err)
	}
	snap := e.srv.store.Snapshot()
	if snap.VMEnvs["vm-dead-1"] != "" || snap.VMEnvPins["vm-dead-1"] != nil || snap.VMEnvPins["vm-dead-pin-only"] != nil {
		t.Fatalf("dead VMEnvs/VMEnvPins survived reconcile: envs=%v pins=%v", snap.VMEnvs, snap.VMEnvPins)
	}
	if snap.VMEnvs[liveVM] != envID || snap.VMEnvPins[liveVM] == nil {
		t.Fatalf("live VM lost env/pin during reconcile")
	}
	// Compat environment view reports sandboxCount == 1 (not inflated by dead entries).
	versions := envVersions(snap, snap.Environments[envID])
	if len(versions) != 1 || versions[0].(map[string]any)["sandboxCount"] != 1 {
		t.Fatalf("versions = %v, want single version with sandboxCount=1", versions)
	}
}

// 2a. Guest agent user/state OpErrors map to 400/409 instead of 502 Bad Gateway.
func TestR2GuestOpErrorsMapTo4xx(t *testing.T) {
	e := setup(t)
	vm := e.createVM()

	// Command already exited -> DELETE /v1/vms/{id}/commands/{cid} returns 409 conflict.
	st, started := e.do(e.owner, "POST", vmPath(vm, "commands"), map[string]any{"cmd": []string{"echo", "done"}})
	if st != 201 {
		t.Fatalf("start cmd = %d %v", st, started)
	}
	cid := started["id"].(string)
	if st, out := e.do(e.owner, "DELETE", vmPath(vm, "commands", cid), nil); st != 409 || nativeErrCode(out) != "conflict" {
		t.Fatalf("stop exited command = %d %v, want 409 conflict", st, out)
	}

	// Refusing to delete protected system dir -> 400 bad_request.
	if st, out := e.do(e.owner, "DELETE", vmPath(vm, "files")+"?path=/etc", nil); st != 400 || nativeErrCode(out) != "bad_request" {
		t.Fatalf("delete /etc = %d %v, want 400 bad_request", st, out)
	}

	// Non-empty directory delete without recursive -> 409 conflict.
	e.boatd.mu.Lock()
	e.boatd.files[vm+":/work/sub/a.txt"] = []byte("hello")
	e.boatd.files[vm+":/work/dest.txt"] = []byte("existing")
	e.boatd.mu.Unlock()
	if st, out := e.do(e.owner, "DELETE", vmPath(vm, "files")+"?path=/work/sub", nil); st != 409 || nativeErrCode(out) != "conflict" {
		t.Fatalf("delete non-empty dir = %d %v, want 409 conflict", st, out)
	}

	// Move over existing destination without overwrite -> 409 conflict.
	if st, out := e.do(e.owner, "POST", vmPath(vm, "files", "move"), map[string]any{"from": "/work/sub/a.txt", "to": "/work/dest.txt"}); st != 409 || nativeErrCode(out) != "conflict" {
		t.Fatalf("move onto existing = %d %v, want 409 conflict", st, out)
	}

	// Additional real guest OpError / OSError messages via hook.
	cases := []struct {
		msg      string
		wantCode int
	}{
		{"guest agent: [Errno 39] Directory not empty: '/work/d'", 409},
		{"guest agent: /work/x already exists", 409},
		{"guest agent: [Errno 20] Not a directory: '/work/f/x'", 400},
		{"guest agent: [Errno 21] Is a directory: '/work/d'", 400},
		{"guest agent: /work/d is a directory; delete it first", 409},
		{"guest agent: unexpected internal panic", 502},
	}
	for _, tc := range cases {
		e.boatd.mu.Lock()
		e.boatd.agentOpHook = func(op string, in map[string]any) (int, map[string]any, bool) {
			return 502, map[string]any{"error": tc.msg}, true
		}
		e.boatd.mu.Unlock()
		st, out := e.do(e.owner, "GET", vmPath(vm, "files", "list")+"?path=/work", nil)
		if st != tc.wantCode {
			t.Errorf("%q -> HTTP %d (%v), want %d", tc.msg, st, out, tc.wantCode)
		}
	}
	e.boatd.mu.Lock()
	e.boatd.agentOpHook = nil
	e.boatd.mu.Unlock()
}

// 2b. compatStopCommand is idempotent when the command already exited.
func TestR2CompatStopCommandIdempotent(t *testing.T) {
	e := setup(t)
	sb := e.newSandbox(nil)
	base := "/api/v1/sandboxes/" + sb

	// Running detached command: first stop signals it, second stop is idempotent.
	started := e.ok("POST", base+"/commands", map[string]any{"command": "sleep 60", "detached": true}, 200)
	pid := itoa(int(started["processId"].(float64)))
	first := e.ok("DELETE", base+"/commands/"+pid, nil, 200)
	if first["running"] != false || first["signaled"] != true {
		t.Fatalf("first stop = %v, want running=false signaled=true", first)
	}
	second := e.ok("DELETE", base+"/commands/"+pid, nil, 200)
	if second["running"] != false || second["signaled"] != false {
		t.Fatalf("second stop = %v, want running=false signaled=false", second)
	}

	// Already-exited detached command returns 200 running=false signaled=false.
	fast := e.ok("POST", base+"/commands", map[string]any{"command": "echo hi", "detached": true}, 200)
	fastPID := itoa(int(fast["processId"].(float64)))
	stopped := e.ok("DELETE", base+"/commands/"+fastPID, nil, 200)
	if stopped["running"] != false || stopped["signaled"] != false {
		t.Fatalf("stop already-exited = %v, want running=false signaled=false", stopped)
	}
}

// 3. compatResume rolls size back to prevSize when startVM fails after resizing a stopped sandbox.
func TestR2CompatResumeRollbackSizeOnStartFailure(t *testing.T) {
	e := setup(t)
	vm := e.createVMWith(map[string]any{"size": "small"})
	e.do(e.owner, "POST", vmPath(vm, "stop"), nil)

	e.boatd.mu.Lock()
	e.boatd.failStarts = 1
	e.boatd.mu.Unlock()

	sbID := e.srv.compatID(e.teamID(), vm)
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+sbID+"/resume", map[string]any{"type": "large"}, nil)
	if st != 502 {
		t.Fatalf("resume with failing start = %d %v, want 502", st, out)
	}
	if got := str(e.nativeVM(vm), "size"); got != "small" {
		t.Fatalf("size after failed resume = %q, want rolled back to small", got)
	}

	// Also verify rollback when fail-closed noEnv start fails (env_start_failed).
	e.setEnvFailures(false, true)
	st, out, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes/"+sbID+"/resume", map[string]any{"type": "large", "noEnv": true}, nil)
	if st != 502 || out["code"] != "env_start_failed" {
		t.Fatalf("resume with env_start_failed = %d %v, want 502 env_start_failed", st, out)
	}
	if got := str(e.nativeVM(vm), "size"); got != "small" {
		t.Fatalf("size after fail-closed env_start_failed = %q, want rolled back to small", got)
	}
}

// 4. compat envContents (parseDotenv / dotenv) round-trip quoting & CR/LF/NUL rejection.
func TestR2CompatEnvContentsQuotingAndValidation(t *testing.T) {
	in := map[string]string{
		"OPTS":   `--tag="prod"`,
		"QUOTED": `"hello"`,
		"SINGLE": `'world'`,
		"SPACED": "  padded  ",
		"SIMPLE": "bar",
		"EMPTY":  "",
	}
	encoded := dotenv(in)
	parsed, msg := parseDotenv(encoded)
	if msg != "" {
		t.Fatalf("parseDotenv error: %s (encoded=%q)", msg, encoded)
	}
	for k, want := range in {
		if parsed[k] != want {
			t.Errorf("round-trip %s: got %q, want %q (encoded=%q)", k, parsed[k], want, encoded)
		}
	}
	// Reject NUL and bare CR in parseDotenv.
	if _, msg := parseDotenv("A=foo\x00bar\n"); msg == "" {
		t.Error("expected NUL rejection in parseDotenv")
	}
	if _, msg := parseDotenv("A=foo\rbar\n"); msg == "" {
		t.Error("expected bare CR rejection in parseDotenv")
	}

	// End-to-end compat API round-trip and CR/LF/NUL rejection on PUT /vars/{key}.
	e := setup(t)
	e.saveEnv("rt", map[string]string{}, []string{})
	envID := envUUIDOf(t, e, "rt")

	for _, bad := range []string{"a\nb", "a\rb", "a\x00b"} {
		if st, _, _ := e.compat(e.owner, "PUT", "/api/v1/environments/"+envID+"/vars/BAD", map[string]any{"value": bad}, nil); st != 400 {
			t.Errorf("PUT var with %q = %d, want 400", bad, st)
		}
	}

	st, _, _ := e.compat(e.owner, "PUT", "/api/v1/environments/"+envID+"/vars/OPTS", map[string]any{"value": `--tag="prod"`}, nil)
	if st != 200 {
		t.Fatalf("PUT OPTS = %d", st)
	}
	envs := e.ok("GET", "/api/v1/environments", nil, 200)["environments"].([]any)
	var envContents string
	for _, raw := range envs {
		m := raw.(map[string]any)
		if m["id"] == envID {
			envContents, _ = m["envContents"].(string)
		}
	}
	if !strings.Contains(envContents, `OPTS=--tag="prod"`) {
		t.Fatalf("envContents = %q, want OPTS=--tag=\"prod\"", envContents)
	}
	st, upd, _ := e.compat(e.owner, "PUT", "/api/v1/environments/"+envID, map[string]any{"envContents": envContents}, nil)
	if st != 200 {
		t.Fatalf("PUT env round-trip = %d %v", st, upd)
	}
	if got := e.srv.store.Snapshot().Environments[e.envIDByName("rt")].Env["OPTS"]; got != `--tag="prod"` {
		t.Fatalf("stored OPTS after round-trip = %q, want --tag=\"prod\"", got)
	}
}

func (e *env) envIDByName(name string) string {
	for id, ev := range e.srv.store.Snapshot().Environments {
		if ev.Name == name {
			return id
		}
	}
	return ""
}

// 5a. CoW store shares immutable entries across snapshots while preserving strict snapshot isolation.
func TestR2StoreCoWPreservesSnapshotIsolation(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	if err := store.Update(func(st *State) error {
		st.Webhooks["wh1"] = &Webhook{ID: "wh1", TeamID: "t1"}
		st.Deliveries["wh1"] = []Delivery{{ID: "d1", WebhookID: "wh1", Status: 200, OK: true, At: now}}
		st.Idempotency["t1\x00k1"] = &IdemRecord{Hash: "h1", Status: 200, Body: []byte(`{"ok":true}`), At: now}
		st.StopOps["s1"] = map[string]any{"id": "s1", "status": "pending", "_team": "t1", "_vm": "vm1"}
		st.StopLast["vm1"] = "s1"
		st.DeletionOps["del1"] = map[string]any{"id": "del1", "_team": "t1"}
		st.AIUsage["t1/vm1/openai/m"] = &AIUsageRow{TeamID: "t1", VMID: "vm1", Provider: "openai", Model: "m", Requests: 1, Updated: now}
		st.AIUsageDays["t1/vm1/openai/m/2026-11-18"] = &AIUsageRow{TeamID: "t1", VMID: "vm1", Provider: "openai", Model: "m", Day: "2026-11-18", Requests: 1, Updated: now}
		st.VMTokens["vm1"] = &VMToken{TeamID: "t1", Gen: 1}
		st.VMTokens["vm2"] = &VMToken{TeamID: "t1", Gen: 2}
		st.Commands["vm1"] = []CommandRecord{{ID: "c1", Cmd: []string{"echo", "hi"}, State: "exited"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	snap1 := store.Snapshot()
	// Unrelated mutation must share immutable pointers across snapshots.
	if err := store.Apply(func(st *State) error {
		st.VMNames["vm1"] = "renamed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snap2 := store.Snapshot()
	if snap2.Idempotency["t1\x00k1"] != snap1.Idempotency["t1\x00k1"] {
		t.Error("unrelated Apply cloned immutable Idempotency record")
	}
	if snap2.AIUsage["t1/vm1/openai/m"] != snap1.AIUsage["t1/vm1/openai/m"] {
		t.Error("unrelated Apply cloned immutable AIUsage row")
	}
	if snap2.VMTokens["vm2"] != snap1.VMTokens["vm2"] {
		t.Error("unrelated Apply cloned immutable VMToken")
	}

	// Mutating AIUsage, VMTokens, StopOps, and Deliveries must NOT mutate snap1 (even concurrently).
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = snap1.AIUsage["t1/vm1/openai/m"].Requests
			_ = snap1.VMTokens["vm1"].Gen
			_ = snap1.StopOps["s1"]["status"]
			_ = len(snap1.Deliveries["wh1"])
		}
	}()
	stops := &stopOps{store: store}
	stops.complete("vm1", map[string]any{"id": "s1"})
	if err := store.Apply(func(st *State) error {
		addAIUsage(st.AIUsage, "t1/vm1/openai/m", &AIUsageRow{TeamID: "t1", VMID: "vm1", Provider: "openai", Model: "m", Requests: 5}, "", now.Add(time.Second))
		st.cowVMToken("vm1").Gen = 99
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	if snap1.AIUsage["t1/vm1/openai/m"].Requests != 1 {
		t.Errorf("snap1 AIUsage corrupted: %d", snap1.AIUsage["t1/vm1/openai/m"].Requests)
	}
	if snap1.VMTokens["vm1"].Gen != 1 {
		t.Errorf("snap1 VMToken corrupted: %d", snap1.VMTokens["vm1"].Gen)
	}
	if snap1.StopOps["s1"]["status"] != "pending" {
		t.Errorf("snap1 StopOps corrupted: %v", snap1.StopOps["s1"]["status"])
	}
}

// 5b. dispatcher.record uses ApplyLazy and does not block on synchronous fsync.
func TestR2WebhookDeliveryRecordDoesNotSyncFsync(t *testing.T) {
	e := setup(t)
	st, created := e.do(e.owner, "POST", "/v1/webhooks", map[string]any{"url": "https://example.invalid/hook", "events": []string{"vm.started"}})
	if st != 201 {
		t.Fatalf("create hook = %d %v", st, created)
	}
	hookID := created["id"].(string)

	prevDelay := syncDelay.Swap(int64(250 * time.Millisecond))
	t.Cleanup(func() { syncDelay.Store(prevDelay) })

	start := time.Now()
	e.srv.hooks.record(Delivery{ID: "dlv_fast", WebhookID: hookID, EventID: "ev_1", Event: "vm.started", Attempt: 1, Status: 200, OK: true, At: time.Now().UTC()})
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Fatalf("dispatcher.record blocked %v on fsync (expected lazy non-blocking write)", elapsed)
	}
	if got := e.srv.store.Snapshot().Deliveries[hookID]; len(got) != 1 || got[0].ID != "dlv_fast" {
		t.Fatalf("Deliveries snapshot = %+v, want dlv_fast visible immediately", got)
	}
}

// 5c. guardBody caches verified VM on r.Context() so handlers do not issue duplicate GET /v1/vms/{id}.
func TestR2BodyGuardCachesVMNoDuplicateGet(t *testing.T) {
	e := setup(t)
	vm := e.createVM()
	sbID := e.srv.compatID(e.teamID(), vm)

	resetGets := func() {
		e.boatd.mu.Lock()
		e.boatd.vmGets[vm] = 0
		e.boatd.mu.Unlock()
	}
	gets := func() int {
		e.boatd.mu.Lock()
		defer e.boatd.mu.Unlock()
		return e.boatd.vmGets[vm]
	}

	// Native POST /v1/vms/{id}/commands
	resetGets()
	if st, out := e.do(e.owner, "POST", vmPath(vm, "commands"), map[string]any{"cmd": []string{"echo", "ok"}}); st != 201 {
		t.Fatalf("native start cmd = %d %v", st, out)
	}
	if n := gets(); n != 1 {
		t.Errorf("POST /v1/vms/{id}/commands issued %d GETs to boatd, want 1", n)
	}

	// Compat POST /api/v1/sandboxes/{id}/commands (detached)
	resetGets()
	started := e.ok("POST", "/api/v1/sandboxes/"+sbID+"/commands", map[string]any{"command": "sleep 60", "detached": true}, 200)
	if n := gets(); n != 1 {
		t.Errorf("POST /api/v1/sandboxes/{id}/commands (detached) issued %d GETs to boatd, want 1", n)
	}

	// Compat DELETE /api/v1/sandboxes/{id}/commands/{pid}
	resetGets()
	pid := itoa(int(started["processId"].(float64)))
	e.ok("DELETE", "/api/v1/sandboxes/"+sbID+"/commands/"+pid, nil, 200)
	if n := gets(); n != 1 {
		t.Errorf("DELETE /api/v1/sandboxes/{id}/commands/{pid} issued %d GETs to boatd, want 1", n)
	}

	// Compat PUT /api/v1/sandboxes/{id}/files
	resetGets()
	e.ok("PUT", "/api/v1/sandboxes/"+sbID+"/files", map[string]any{"path": "hello.txt", "content": "hi"}, 200)
	if n := gets(); n != 1 {
		t.Errorf("PUT /api/v1/sandboxes/{id}/files issued %d GETs to boatd, want 1", n)
	}
}

// 6a. importSnapshot persists snapPolicyKey when boatd reports no_env:true or label boat.no_env=1.
func TestR2ImportSnapshotPreservesNoEnv(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TOP_SECRET", map[string]any{"value": "shh", "inject": true})

	// Case 1: export & re-import a snapshot of a no_env VM (boatd returns no_env: true).
	noEnvVM := e.createVMWith(map[string]any{"no_env": true})
	st, snap := e.do(e.owner, "POST", vmPath(noEnvVM, "snapshots"), map[string]any{"name": "clean"})
	if st != 201 {
		t.Fatalf("create snapshot = %d %v", st, snap)
	}
	sid := snap["id"].(string)
	req, _ := http.NewRequest("GET", e.api.URL+vmPath(noEnvVM, "snapshots", sid, "download"), nil)
	req.Header.Set("Authorization", "Bearer "+e.owner)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("download snapshot = %v %v", resp, err)
	}
	archive, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	impReq, _ := http.NewRequest("POST", e.api.URL+"/v1/snapshots/import?name=reimported", bytes.NewReader(archive))
	impReq.Header.Set("Authorization", "Bearer "+e.owner)
	impResp, err := http.DefaultClient.Do(impReq)
	if err != nil || impResp.StatusCode != 201 {
		t.Fatalf("import snapshot = %v %v", impResp, err)
	}
	var imported map[string]any
	json.NewDecoder(impResp.Body).Decode(&imported)
	impResp.Body.Close()
	impID := imported["id"].(string)
	if !e.srv.snapshotSourceNoEnv(impID) {
		t.Fatalf("imported snapshot %s lost sticky no_env policy", impID)
	}
	// Verify sticky policy persisted durably on disk.
	back, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if !snapshotSourceNoEnvIn(back.Snapshot(), impID) {
		t.Fatalf("imported snapshot %s sticky no_env policy not durable on disk", impID)
	}
	// Creating a VM from the trusted re-imported protected no_env snapshot cannot
	// escape sticky no_env with secrets or an environment, and succeeds as a
	// protected no_env VM (no 409 unsafe_source).
	if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"snapshot": impID, "secrets": []string{"TOP_SECRET"}}); st != 400 || nativeErrCode(out) != "no_env_sticky" {
		t.Fatalf("create from imported no_env snapshot with secrets = %d %v, want 400 no_env_sticky", st, out)
	}
	if !e.srv.snapshotSourceProtected(impID) {
		t.Fatalf("trusted re-imported protected snapshot %s lost protected lineage", impID)
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"snapshot": impID}); st != 201 {
		t.Fatalf("create from trusted re-imported protected no_env snapshot = %d %v, want 201", st, out)
	}

	// Case 2: untrusted import where boatd returns labels["boat.no_env"] = "1"
	// (no verified protected lineage) keeps sticky no_env and fails closed 409.
	impReq2, _ := http.NewRequest("POST", e.api.URL+"/v1/snapshots/import?name=labeled", strings.NewReader("ARCHIVE:ext:label_no_env"))
	impReq2.Header.Set("Authorization", "Bearer "+e.owner)
	impResp2, err := http.DefaultClient.Do(impReq2)
	if err != nil || impResp2.StatusCode != 201 {
		t.Fatalf("import labeled snapshot = %v %v", impResp2, err)
	}
	var imported2 map[string]any
	json.NewDecoder(impResp2.Body).Decode(&imported2)
	impResp2.Body.Close()
	impID2 := imported2["id"].(string)
	if !e.srv.snapshotSourceNoEnv(impID2) || e.srv.snapshotSourceProtected(impID2) {
		t.Fatalf("imported labeled no_env snapshot %s policy=%v protected=%v", impID2, e.srv.snapshotSourceNoEnv(impID2), e.srv.snapshotSourceProtected(impID2))
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"snapshot": impID2}); st != 409 || nativeErrCode(out) != "unsafe_source" {
		t.Fatalf("create from untrusted imported no_env snapshot = %d %v, want 409 unsafe_source", st, out)
	}
}

// 6b. Team-scoped GET /v1/teams and GET /v1/teams/{id} isolation and non-admin redaction.
func TestR2TeamScopedTeamsIsolationAndRedaction(t *testing.T) {
	e := setup(t)
	teamA := e.teamID()
	st, tb := e.do(e.owner, "POST", "/v1/teams", map[string]any{"name": "team-b"})
	if st != 201 {
		t.Fatalf("create team-b = %d %v", st, tb)
	}
	teamB, _ := tb["id"].(string)
	if teamB == "" {
		if m, ok := tb["team"].(map[string]any); ok {
			teamB, _ = m["id"].(string)
		}
	}
	if teamB == "" {
		t.Fatalf("unexpected createTeam response: %v", tb)
	}

	// Configure accept_shares_from & self_token on teamA so we can verify redaction.
	if st, out := e.do(e.owner, "PUT", "/v1/teams/"+teamA+"/share-allowlist", map[string]any{"teams": []string{teamB}}); st != 200 {
		t.Fatalf("allowlist = %d %v", st, out)
	}
	if st, out := e.do(e.owner, "PUT", "/v1/teams/"+teamA+"/self-token", map[string]any{"public_ports": true}); st != 200 {
		t.Fatalf("self-token = %d %v", st, out)
	}

	// Add bob as admin of teamA and also member of teamB.
	st, m := e.do(e.owner, "POST", "/v1/teams/"+teamA+"/members", map[string]any{"name": "bob", "role": "admin"})
	if st != 201 {
		t.Fatalf("add bob = %d %v", st, m)
	}
	bobUID := m["user"].(map[string]any)["id"].(string)
	bobAdminKey := m["key"].(map[string]any)["token"].(string)
	if err := e.srv.store.Update(func(st *State) error {
		st.Teams[teamB].Members = append(st.Teams[teamB].Members, Member{UserID: bobUID, Role: "member"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Mint a non-admin key for bob in teamA.
	st, kOut := e.do(bobAdminKey, "POST", "/v1/keys", map[string]any{"name": "bob-ro", "scopes": []string{"vms:read"}})
	if st != 201 {
		t.Fatalf("mint bob-ro = %d %v", st, kOut)
	}
	bobROKey := kOut["token"].(string)
	// Mint a non-admin key for the operator in teamA.
	opROKey := e.newKey("vms:read")

	// Bob's admin key (non-operator) is scoped to teamA only, sees full teamA fields.
	st, l := e.do(bobAdminKey, "GET", "/v1/teams", nil)
	if st != 200 || len(list(l, "teams")) != 1 || list(l, "teams")[0].(map[string]any)["id"] != teamA {
		t.Fatalf("bob admin listTeams = %d %v, want only teamA", st, l)
	}
	if st, _ := e.do(bobAdminKey, "GET", "/v1/teams/"+teamB, nil); st != 404 {
		t.Fatalf("bob admin getTeam(teamB) = %d, want 404", st)
	}
	if st, g := e.do(bobAdminKey, "GET", "/v1/teams/"+teamA, nil); st != 200 || g["members"] == nil || g["accept_shares_from"] == nil || g["self_token"] == nil {
		t.Fatalf("bob admin getTeam(teamA) = %d %v, want full admin view", st, g)
	}

	// Non-admin keys (both regular user and operator without admin scope) see only own team and redacted details.
	for name, tok := range map[string]string{"bob-ro": bobROKey, "op-ro": opROKey} {
		st, l := e.do(tok, "GET", "/v1/teams", nil)
		if st != 200 || len(list(l, "teams")) != 1 || list(l, "teams")[0].(map[string]any)["id"] != teamA {
			t.Errorf("%s listTeams = %d %v, want only teamA", name, st, l)
		}
		if st, _ := e.do(tok, "GET", "/v1/teams/"+teamB, nil); st != 404 {
			t.Errorf("%s getTeam(teamB) = %d, want 404", name, st)
		}
		st, g := e.do(tok, "GET", "/v1/teams/"+teamA, nil)
		if st != 200 {
			t.Fatalf("%s getTeam(teamA) = %d %v", name, st, g)
		}
		if _, has := g["members"]; has {
			t.Errorf("%s getTeam leaked members: %v", name, g)
		}
		if _, has := g["accept_shares_from"]; has {
			t.Errorf("%s getTeam leaked accept_shares_from: %v", name, g)
		}
		if _, has := g["self_token"]; has {
			t.Errorf("%s getTeam leaked self_token: %v", name, g)
		}
	}

	// Operator admin still sees both teams and full details.
	st, l = e.do(e.owner, "GET", "/v1/teams", nil)
	if st != 200 || len(list(l, "teams")) != 2 {
		t.Fatalf("operator admin listTeams = %d %v, want 2 teams", st, l)
	}
	if st, g := e.do(e.owner, "GET", "/v1/teams/"+teamB, nil); st != 200 || g["members"] == nil {
		t.Fatalf("operator admin getTeam(teamB) = %d %v", st, g)
	}
}

// 6c. AI proxy enforces minimum-progress stall guard & size cap on non-usage-injected streaming bodies.
func TestR2AIProxyStreamingBodyStallGuard(t *testing.T) {
	oldWin, oldDeadline, oldMin := aiProgressWindow, aiBodyDeadline, aiMinProgress
	aiProgressWindow = 120 * time.Millisecond
	aiBodyDeadline = 400 * time.Millisecond
	aiMinProgress = 64 << 10
	t.Cleanup(func() {
		aiProgressWindow, aiBodyDeadline, aiMinProgress = oldWin, oldDeadline, oldMin
	})

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	defer upstream.Close()

	e := setup(t)
	e.srv.cfg.AIUpstreams = map[string]string{"anthropic": upstream.URL}
	proxy := httptest.NewServer(e.srv.aiHandler())
	defer proxy.Close()
	e.srv.cfg.AIProxyURL = proxy.URL

	if st, out := e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "sk-ant-test"}); st != 200 {
		t.Fatalf("put ai cred = %d %v", st, out)
	}
	vm := e.createVM()
	tok := e.srv.aiToken(vm, e.srv.store.Snapshot().AIVMs[vm].Gen)

	// Case 1: Declared Content-Length > aiMaxBody rejected up front with 413 without hitting upstream.
	rec := httptest.NewRecorder()
	rr := httptest.NewRequest("POST", "/anthropic/v1/messages", http.NoBody)
	rr.ContentLength = aiMaxBody + 1
	rr.Header.Set("X-Api-Key", tok)
	e.srv.aiHandler().ServeHTTP(rec, rr)
	if rec.Code != 413 {
		t.Fatalf("oversized anthropic Content-Length = %d %s, want 413", rec.Code, rec.Body.String())
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("oversized request hit upstream %d times, want 0", upstreamHits.Load())
	}

	// Case 2: Chunked streaming request to /anthropic/v1/messages that stalls mid-body returns 408.
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(`{"model":"claude","messages":[`))
		// Stall longer than aiProgressWindow without sending aiMinProgress bytes.
		time.Sleep(600 * time.Millisecond)
		_ = pw.Close()
	}()
	req, _ := http.NewRequest("POST", proxy.URL+"/anthropic/v1/messages", pr)
	req.Header.Set("X-Api-Key", tok)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("stalled request client err: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 408 {
		t.Fatalf("stalled streaming anthropic body = %d %s, want 408", resp.StatusCode, body)
	}
}
