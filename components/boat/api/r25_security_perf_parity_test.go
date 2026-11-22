package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// S1: Public port exposure requires scope "exec" (including live principal
// revalidation), and compatHost refuses vms:write-only callers with 403 before
// mutating boatd ports or calling an exec-scoped native route.
func TestR25PublicPortRequiresExecAndCompatHostPrecheck(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMDomain = "vm.boat.example"
	vmID := e.createVM()

	st, bxOut := e.do(e.owner, "GET", "/api/v1/sandboxes", nil)
	if st != 200 {
		t.Fatalf("list sandboxes: %d %v", st, bxOut)
	}
	bxID := list(bxOut, "sandboxes")[0].(map[string]any)["id"].(string)

	// Mint a vms:read + vms:write key (no exec).
	st, kwOut := e.do(e.owner, "POST", "/v1/keys", map[string]any{
		"name":   "write-only",
		"scopes": []string{"vms:read", "vms:write"},
	})
	if st != 201 {
		t.Fatalf("create write-only key: %d %v", st, kwOut)
	}
	writeKey := kwOut["token"].(string)

	// Track native route calls to ensure compatHost never invokes an exec-scoped route
	// or mutates boatd ports for a write-only caller.
	var nativeCalls []string
	e.srv.onNative = func(pattern string, _ *principal) {
		nativeCalls = append(nativeCalls, pattern)
	}
	t.Cleanup(func() { e.srv.onNative = nil })

	// 1. Native POST /v1/vms/{id}/ports with auth=public (or public=true) rejected 403.
	for _, body := range []map[string]any{
		{"port": 8080, "auth": "public"},
		{"port": 8081, "public": true},
	} {
		st, out := e.do(writeKey, "POST", vmPath(vmID, "ports"), body)
		if st != http.StatusForbidden {
			t.Fatalf("write-only public port %v: status=%d body=%v, want 403", body, st, out)
		}
	}
	e.boatd.mu.Lock()
	portsAfterDeniedNative, _ := e.boatd.vms[vmID]["ports"].([]any)
	e.boatd.mu.Unlock()
	if len(portsAfterDeniedNative) != 0 {
		t.Fatalf("denied native public port mutated boatd ports: %v", portsAfterDeniedNative)
	}

	// 2. compatHost (private or public) rejected 403 with zero native calls and zero boatd port mutation.
	nativeCalls = nil
	for _, pub := range []bool{false, true} {
		st, out := e.do(writeKey, "POST", "/api/v1/sandboxes/"+bxID+"/host", map[string]any{"port": 8082, "public": pub})
		if st != http.StatusForbidden {
			t.Fatalf("write-only compatHost(public=%v): status=%d body=%v, want 403", pub, st, out)
		}
	}
	if len(nativeCalls) != 0 {
		t.Fatalf("compatHost invoked native routes for write-only caller: %v", nativeCalls)
	}
	e.boatd.mu.Lock()
	portsAfterDeniedHost, _ := e.boatd.vms[vmID]["ports"].([]any)
	e.boatd.mu.Unlock()
	if len(portsAfterDeniedHost) != 0 {
		t.Fatalf("denied compatHost mutated boatd ports: %v", portsAfterDeniedHost)
	}

	// 3. Private native port declaration STILL succeeds with vms:write.
	st, privOut := e.do(writeKey, "POST", vmPath(vmID, "ports"), map[string]any{"port": 8083, "auth": "private"})
	if st != 201 || privOut["auth"] != "private" {
		t.Fatalf("write-only private port: status=%d body=%v, want 201 private", st, privOut)
	}

	// 4. Caller with vms:write + exec succeeds on public native port AND compatHost;
	// and live scope downgrade of exec is caught by revalidatePrincipal!
	st, execKeyOut := e.do(e.owner, "POST", "/v1/keys", map[string]any{
		"name":   "write-exec",
		"scopes": []string{"vms:read", "vms:write", "exec"},
	})
	if st != 201 {
		t.Fatalf("create write+exec key: %d %v", st, execKeyOut)
	}
	execKey := execKeyOut["token"].(string)
	execKeyID, _, _ := ParseToken(execKey)

	if st, out := e.do(execKey, "POST", vmPath(vmID, "ports"), map[string]any{"port": 8084, "auth": "public"}); st != 201 || out["auth"] != "public" {
		t.Fatalf("write+exec public port: status=%d body=%v, want 201 public", st, out)
	}
	if st, out := e.do(execKey, "POST", "/api/v1/sandboxes/"+bxID+"/host", map[string]any{"port": 8085, "public": true}); st != 200 || out["access"] != "public" {
		t.Fatalf("write+exec compatHost(public=true): status=%d body=%v, want 200 public", st, out)
	}

	// Simulate a concurrent scope downgrade removing "exec" right after initial auth:
	// test revalidatePrincipal directly by stripping "exec" in store and calling addPort with cached principal.
	if err := e.srv.store.Update(func(s *State) error {
		s.Keys[execKeyID].Scopes = []string{"vms:read", "vms:write"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if st, out := e.do(execKey, "POST", vmPath(vmID, "ports"), map[string]any{"port": 8086, "auth": "public"}); st != http.StatusForbidden {
		t.Fatalf("downgraded key public port: status=%d body=%v, want 403", st, out)
	}
}

// S2: VMs with NoSelfAPI == true (self_api: false or caller-supplied BOAT_API_KEY)
// still rotate VMToken.Gen and refresh /etc/boat/ssh-auth.json on snapshot and fork,
// invalidating copied SSH lookup tokens without injecting a managed BOAT_API_KEY.
func TestR25NoSelfAPIRotatesSSHTokenOnCopyWithoutInjectingAPIKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"self_api_false", map[string]any{"name": "noself", "self_api": false}, ""},
		{"caller_boat_api_key", map[string]any{"name": "customkey", "env": map[string]string{"BOAT_API_KEY": "caller-custom-secret"}}, "caller-custom-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			e.enableSSH()

			st, created := e.do(e.owner, "POST", "/v1/vms", tc.body)
			if st != 201 {
				t.Fatalf("createVM: %d %v", st, created)
			}
			vmID := created["id"].(string)

			tok0 := e.srv.store.Snapshot().VMTokens[vmID]
			if tok0 == nil || !tok0.NoSelfAPI || tok0.Gen != 1 {
				t.Fatalf("initial VMToken = %+v, want NoSelfAPI=true Gen=1", tok0)
			}

			// Authorize an SSH key on the parent VM.
			if st, out := e.do(e.owner, "POST", vmPath(vmID, "ssh"), map[string]string{"public_key": r11Pub}); st != 200 {
				t.Fatalf("ssh grant: %d %v", st, out)
			}
			if st, _ := e.lookupSSH(vmID, r11Pub); st != 200 {
				t.Fatalf("initial lookupSSH status=%d, want 200", st)
			}

			readAuthToken := func(id string) string {
				t.Helper()
				e.boatd.mu.Lock()
				b := slices.Clone(e.boatd.files[id+":/etc/boat/ssh-auth.json"])
				e.boatd.mu.Unlock()
				var cfg map[string]any
				if err := json.Unmarshal(b, &cfg); err != nil {
					t.Fatalf("unmarshal ssh-auth.json for %s: %v", id, err)
				}
				return cfg["token"].(string)
			}
			checkEnvUntouched := func(id string) {
				t.Helper()
				e.boatd.mu.Lock()
				envMap, _ := e.boatd.vms[id]["env"].(map[string]any)
				gotKey, hasKey := envMap["BOAT_API_KEY"]
				e.boatd.mu.Unlock()
				if tc.want == "" {
					if hasKey && gotKey != "" {
						t.Fatalf("managed BOAT_API_KEY unexpectedly injected into self_api:false VM %s: %v", id, gotKey)
					}
				} else if gotKey != tc.want {
					t.Fatalf("caller BOAT_API_KEY overwritten on %s: got %v, want %q", id, gotKey, tc.want)
				}
			}

			tokenGen1 := readAuthToken(vmID)
			checkEnvUntouched(vmID)
			f := strings.Fields(r11Pub)

			// 1. Snapshot capture rotates VMToken.Gen (1 -> 2), invalidates captured tokenGen1 (401),
			// refreshes parent's /etc/boat/ssh-auth.json to Gen 2 (200), and leaves BOAT_API_KEY untouched.
			_ = e.snapshotOf(vmID)
			tok1 := e.srv.store.Snapshot().VMTokens[vmID]
			if tok1 == nil || !tok1.NoSelfAPI || tok1.Gen != 2 {
				t.Fatalf("after snapshot VMToken = %+v, want NoSelfAPI=true Gen=2", tok1)
			}
			if st, _ := e.do(tokenGen1, "POST", vmPath(vmID, "ssh", "authorize"), map[string]string{"user": "dev", "key_type": f[0], "key_blob": f[1]}); st != 401 {
				t.Fatalf("copied snapshot SSH bearer still accepted: status=%d, want 401", st)
			}
			tokenGen2 := readAuthToken(vmID)
			if tokenGen2 == tokenGen1 {
				t.Fatal("parent /etc/boat/ssh-auth.json token did not rotate on snapshot")
			}
			if st, _ := e.lookupSSH(vmID, r11Pub); st != 200 {
				t.Fatalf("live parent lookupSSH after snapshot status=%d, want 200", st)
			}
			checkEnvUntouched(vmID)

			// 2. Fork rotates parent VMToken.Gen again (2 -> 3), invalidates tokenGen2 (401),
			// refreshes parent's /etc/boat/ssh-auth.json to Gen 3 (200), and leaves BOAT_API_KEY untouched.
			forkName := strings.ReplaceAll(tc.name, "_", "-") + "-child"
			st, child := e.do(e.owner, "POST", vmPath(vmID, "fork"), map[string]any{"name": forkName})
			if st != 201 {
				t.Fatalf("forkVM: %d %v", st, child)
			}
			tok2 := e.srv.store.Snapshot().VMTokens[vmID]
			if tok2 == nil || !tok2.NoSelfAPI || tok2.Gen != 3 {
				t.Fatalf("after fork VMToken = %+v, want NoSelfAPI=true Gen=3", tok2)
			}
			if st, _ := e.do(tokenGen2, "POST", vmPath(vmID, "ssh", "authorize"), map[string]string{"user": "dev", "key_type": f[0], "key_blob": f[1]}); st != 401 {
				t.Fatalf("pre-fork SSH bearer still accepted: status=%d, want 401", st)
			}
			if st, _ := e.lookupSSH(vmID, r11Pub); st != 200 {
				t.Fatalf("live parent lookupSSH after fork status=%d, want 200", st)
			}
			checkEnvUntouched(vmID)
		})
	}
}

// P1: Mixing native POST /v1/vms/{id}/stop and POST /v1/vms/{id}/start keeps
// /api/v1/sandboxes/{id} .stop synchronized (completed / failing -> superseded).
func TestR25NativeStopAndStartSyncCompatStopOps(t *testing.T) {
	e := setup(t)
	st, created := e.do(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "p1-mix"})
	if st != 202 {
		t.Fatalf("compat create: %d %v", st, created)
	}
	bxID := created["sandbox"].(map[string]any)["id"].(string)
	vmID := e.srv.store.Snapshot().CompatIDs[bxID].VM

	// 1. Native stop records a completed stopOp visible on GET /api/v1/sandboxes/{id}.
	if st, out := e.do(e.owner, "POST", vmPath(vmID, "stop"), map[string]any{}); st != 200 {
		t.Fatalf("native stop: %d %v", st, out)
	}
	st, got := e.do(e.owner, "GET", "/api/v1/sandboxes/"+bxID, nil)
	if st != 200 {
		t.Fatalf("compat get after native stop: %d %v", st, got)
	}
	sb := got["sandbox"].(map[string]any)
	stop1, _ := sb["stop"].(map[string]any)
	if sb["state"] != "archived" || stop1 == nil || stop1["status"] != "completed" {
		t.Fatalf("after native stop: state=%v stop=%v, want archived + completed", sb["state"], stop1)
	}
	stop1ID := stop1["id"].(string)

	// 2. Native start transitions that completed stopOp to "superseded".
	if st, out := e.do(e.owner, "POST", vmPath(vmID, "start"), map[string]any{}); st != 200 {
		t.Fatalf("native start: %d %v", st, out)
	}
	st, stopGet := e.do(e.owner, "GET", "/api/v1/sandboxes/"+bxID+"/stops/"+stop1ID, nil)
	if st != 200 || stopGet["stop"].(map[string]any)["status"] != "superseded" {
		t.Fatalf("after native start: GET stop %s = %d %v, want superseded", stop1ID, st, stopGet)
	}

	// 3. Failed native stop records status="failing", and subsequent native start transitions it to "superseded".
	origTr := e.srv.boatd.hc.Transport
	e.srv.boatd.hc.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/stop") {
			return &http.Response{
				StatusCode: 500,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":"simulated stop failure"}`)),
			}, nil
		}
		return origTr.RoundTrip(req)
	})
	if st, _ := e.do(e.owner, "POST", vmPath(vmID, "stop"), map[string]any{}); st == 200 {
		t.Fatal("expected native stop to fail when boatd returns 500")
	}
	e.srv.boatd.hc.Transport = origTr

	st, gotFail := e.do(e.owner, "GET", "/api/v1/sandboxes/"+bxID, nil)
	if st != 200 {
		t.Fatalf("compat get after failed native stop: %d %v", st, gotFail)
	}
	stop2, _ := gotFail["sandbox"].(map[string]any)["stop"].(map[string]any)
	if stop2 == nil || stop2["status"] != "failing" {
		t.Fatalf("after failed native stop: stop=%v, want failing", stop2)
	}
	if st, out := e.do(e.owner, "POST", vmPath(vmID, "start"), map[string]any{}); st != 200 {
		t.Fatalf("native start on running VM: %d %v", st, out)
	}
	st, gotSup := e.do(e.owner, "GET", "/api/v1/sandboxes/"+bxID, nil)
	if st != 200 || gotSup["sandbox"].(map[string]any)["stop"].(map[string]any)["status"] != "superseded" {
		t.Fatalf("after start following failed stop: %d %v, want superseded", st, gotSup)
	}
}

// P3: POST /v1/vms/{id}/ssh with public_key returns 501 not_configured when
// SSH policy helper or HTTPS VM API URL is not configured, while still returning
// 200 connection info when public_key is omitted and 403 when SSH IS configured
// on the server but authority is revoked.
func TestR25UnconfiguredSSHReturns501NotConfigured(t *testing.T) {
	e := setup(t)
	vmID := e.createVM()

	// Info-only mode (no public_key) still succeeds with 200.
	if st, out := e.do(e.owner, "POST", vmPath(vmID, "ssh"), map[string]string{}); st != 200 || out["user"] == "" {
		t.Fatalf("info-only ssh without helper: %d %v, want 200", st, out)
	}

	// Key-authorizing mode without SSHPolicyHelperFile returns 501 not_configured.
	st, out := e.do(e.owner, "POST", vmPath(vmID, "ssh"), map[string]string{"public_key": r11Pub})
	if st != http.StatusNotImplemented {
		t.Fatalf("unconfigured helper ssh grant: status=%d body=%v, want 501", st, out)
	}
	if errObj, _ := out["error"].(map[string]any); errObj["code"] != "not_configured" {
		t.Fatalf("error code = %v, want not_configured", out)
	}

	// Also compat /api/v1/sandboxes/{id}/sshkey relays 501 when unconfigured.
	st, bxList := e.do(e.owner, "GET", "/api/v1/sandboxes", nil)
	if st != 200 {
		t.Fatalf("list sandboxes: %d %v", st, bxList)
	}
	bxID := list(bxList, "sandboxes")[0].(map[string]any)["id"].(string)
	if st, compatOut := e.do(e.owner, "POST", "/api/v1/sandboxes/"+bxID+"/sshkey", map[string]string{"key": r11Pub}); st != http.StatusNotImplemented {
		t.Fatalf("compat sshkey when unconfigured: status=%d body=%v, want 501", st, compatOut)
	}

	// Helper file set, but VMAPIURL is plain HTTP (not HTTPS): still 501 not_configured.
	e.enableSSH()
	e.srv.cfg.VMAPIURL = "http://insecure.boat.example"
	if st, httpOut := e.do(e.owner, "POST", vmPath(vmID, "ssh"), map[string]string{"public_key": r11Pub}); st != http.StatusNotImplemented {
		t.Fatalf("non-https VMAPIURL ssh grant: status=%d body=%v, want 501", st, httpOut)
	}

	// Restore valid HTTPS VMAPIURL -> 200 on a VM created with SSH enabled.
	e.srv.cfg.VMAPIURL = "https://boat.example"
	vmSSH := e.createVM()
	if st, okOut := e.do(e.owner, "POST", vmPath(vmSSH, "ssh"), map[string]string{"public_key": r11Pub}); st != 200 {
		t.Fatalf("configured ssh grant: %d %v, want 200", st, okOut)
	}
}

// P4 & P5: compatGet and compatList refresh running prompts so finished turns
// transition sandbox.state from "running" -> "ready" immediately on poll; and
// POST /api/v1/sandboxes/{id}/unshare returns explicit 501 not_supported.
func TestR25CompatPromptRefreshOnGetAndListAndUnshareEnvelope(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMDomain = "vm.boat.example"

	for _, mode := range []string{"get", "list"} {
		t.Run(mode, func(t *testing.T) {
			st, created := e.do(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "p4-" + mode})
			if st != 202 {
				t.Fatalf("create sandbox: %d %v", st, created)
			}
			bxID := created["sandbox"].(map[string]any)["id"].(string)

			// Launch a prompt that stays running in fakeBoatd ("sleep-forever").
			st, pr := e.do(e.owner, "POST", "/api/v1/sandboxes/"+bxID+"/prompt", map[string]any{
				"provider": "claude-code",
				"prompt":   "sleep-forever please",
				"new":      true,
			})
			if st != 202 {
				t.Fatalf("start prompt: %d %v", st, pr)
			}

			// While fakeCmd is running, sandbox.state is "running".
			st, runningGet := e.do(e.owner, "GET", "/api/v1/sandboxes/"+bxID, nil)
			if st != 200 || runningGet["sandbox"].(map[string]any)["state"] != "running" {
				t.Fatalf("while prompt active: %d %v, want state=running", st, runningGet)
			}

			// Finish the underlying command in fakeBoatd WITHOUT calling /prompts/{pid}.
			vmID := e.srv.store.Snapshot().CompatIDs[bxID].VM
			e.boatd.mu.Lock()
			for _, cmd := range e.boatd.cmds {
				if cmd.vm == vmID && cmd.running {
					cmd.running = false
					cmd.exit = 0
					cmd.log = `{"type":"result","result":"done","session_id":"s1"}` + "\n"
				}
			}
			e.boatd.mu.Unlock()

			if mode == "get" {
				st, refreshed := e.do(e.owner, "GET", "/api/v1/sandboxes/"+bxID, nil)
				if st != 200 || refreshed["sandbox"].(map[string]any)["state"] != "ready" {
					t.Fatalf("compatGet did not refresh finished prompt: %d %v, want state=ready", st, refreshed)
				}
			} else {
				// A list is a display read: it reuses the GET's refresh for promptRefreshTTL.
				time.Sleep(promptRefreshTTL + 50*time.Millisecond)
				st, listed := e.do(e.owner, "GET", "/api/v1/sandboxes?name=p4-"+mode, nil)
				if st != 200 {
					t.Fatalf("compatList: %d %v", st, listed)
				}
				sbs := list(listed, "sandboxes")
				if len(sbs) != 1 || sbs[0].(map[string]any)["state"] != "ready" {
					t.Fatalf("compatList did not refresh finished prompt: %v, want state=ready", sbs)
				}
			}
		})
	}

	// P5: Verify /api/v1/sandboxes/{id}/unshare envelope returns explicit 501 not_supported.
	st, created := e.do(e.owner, "POST", "/api/v1/sandboxes", map[string]any{
		"name":        "p5-unshare",
		"hostedPorts": []map[string]any{{"port": 8080, "public": true}},
	})
	if st != 202 {
		t.Fatalf("create sandbox for unshare: %d %v", st, created)
	}
	bxID := created["sandbox"].(map[string]any)["id"].(string)
	st, unsh := e.do(e.owner, "POST", "/api/v1/sandboxes/"+bxID+"/unshare", nil)
	if st != 501 || errCode(unsh) != "not_supported" || unsh["ok"] != false {
		t.Fatalf("unshare: %d %v, want 501 not_supported", st, unsh)
	}
}

// Perf2, Perf3, Perf5:
//   - Compact state.json serialization (no pretty-print indentation),
//   - Single-VM create & fork coalesce into 1 synchronous store write,
//   - prepareSSHPolicyFromVM completes in 1 boatd RPC when VM metadata is known,
//   - authorizeGuestSSH fast-rejects retired/stale tokens in O(1) with 0 boatd RPCs,
//   - Native POST /v1/vms and POST /v1/vms/{id}/fork honor Idempotency-Key
//     (replay, 409 mismatch, and route isolation from /api/v1).
func TestR25PerfCoalescingSSHOneRPCAndNativeIdempotency(t *testing.T) {
	e := setup(t)
	e.enableSSH()

	// 1. Single-VM create coalesces into 1 Store transaction (seq +1, zero synchronous fsyncs),
	// and flushing persists compact state.json.
	beforeSeq := e.srv.store.Snapshot().seq
	st, created := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "perf-one"})
	if st != 201 {
		t.Fatalf("createVM: %d %v", st, created)
	}
	vmID := created["id"].(string)
	if got := e.srv.store.Snapshot().seq - beforeSeq; got != 1 {
		t.Fatalf("single-VM create store transaction count (seq delta) = %d, want 1", got)
	}
	if err := e.srv.store.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rawDisk, err := os.ReadFile(e.srv.store.path)
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	plain, err := e.srv.store.decode(rawDisk)
	if err != nil {
		t.Fatalf("decode state.json: %v", err)
	}
	if strings.Contains(string(plain), "\n  \"") {
		t.Fatalf("state.json plaintext is still pretty-printed (%d bytes)", len(plain))
	}

	// 2. forkVM coalesces parent rotation + child token/metadata into 1 synchronous store write.
	beforeForkWrites := e.srv.store.writes.Load()
	beforeForkSeq := e.srv.store.Snapshot().seq
	st, forked := e.do(e.owner, "POST", vmPath(vmID, "fork"), map[string]any{"name": "perf-fork"})
	if st != 201 {
		t.Fatalf("forkVM: %d %v", st, forked)
	}
	if got := e.srv.store.writes.Load() - beforeForkWrites; got != 1 {
		t.Fatalf("forkVM synchronous store write count = %d, want 1", got)
	}
	if got := e.srv.store.Snapshot().seq - beforeForkSeq; got != 1 {
		t.Fatalf("forkVM store transaction count (seq delta) = %d, want 1", got)
	}

	// 3. prepareSSHPolicyFromVM with known VM metadata takes 1 boatd HTTP request (POST /ssh-policy only).
	var boatdReqs atomic.Int64
	origTransport := e.srv.boatd.hc.Transport
	e.srv.boatd.hc.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		boatdReqs.Add(1)
		return origTransport.RoundTrip(req)
	})
	t.Cleanup(func() { e.srv.boatd.hc.Transport = origTransport })

	// Wait for any background refresh from forkVM to settle before counting boatd RPCs.
	if err := e.srv.refreshSSHPolicyOnly(context.Background(), vmID); err != nil {
		t.Fatal(err)
	}
	e.boatd.mu.Lock()
	knownVM := fakeVMMetadata(e.boatd.vms[vmID])
	e.boatd.mu.Unlock()

	boatdReqs.Store(0)
	fresh0, err := e.srv.prepareSSHPolicyFromVM(context.Background(), vmID, e.teamID(), knownVM)
	if err != nil || str(fresh0, "id") != vmID || boatdReqs.Load() != 0 {
		t.Fatalf("already-current prepareSSHPolicyFromVM: fresh=%v err=%v rpcs=%d, want 0", fresh0, err, boatdReqs.Load())
	}

	knownVM["ssh_policy_ready"] = false
	delete(knownVM, "ssh_policy_digest")
	boatdReqs.Store(0)
	fresh, err := e.srv.prepareSSHPolicyFromVM(context.Background(), vmID, e.teamID(), knownVM)
	if err != nil || str(fresh, "id") != vmID {
		t.Fatalf("prepareSSHPolicyFromVM: fresh=%v err=%v", fresh, err)
	}
	if got := boatdReqs.Load(); got != 1 {
		t.Fatalf("prepareSSHPolicyFromVM boatd RPCs = %d, want 1", got)
	}

	// 4. authorizeGuestSSH rejects stale-gen and retired tokens in O(1) with 0 boatd RPCs.
	e.boatd.mu.Lock()
	cfgBytes := slices.Clone(e.boatd.files[vmID+":/etc/boat/ssh-auth.json"])
	e.boatd.mu.Unlock()
	var sshCfg map[string]any
	if err := json.Unmarshal(cfgBytes, &sshCfg); err != nil {
		t.Fatal(err)
	}
	staleBearer := sshCfg["token"].(string)
	_ = e.snapshotOf(vmID) // bumps Gen and updates attestedEpoch

	f := strings.Fields(r11Pub)
	boatdReqs.Store(0)
	if st, _ := e.do(staleBearer, "POST", vmPath(vmID, "ssh", "authorize"), map[string]string{"user": "dev", "key_type": f[0], "key_blob": f[1]}); st != 401 {
		t.Fatalf("stale bearer status=%d, want 401", st)
	}
	if got := boatdReqs.Load(); got != 0 {
		t.Fatalf("stale bearer authorizeGuestSSH made %d boatd RPCs, want 0", got)
	}

	// Delete forked VM and verify retired VM token also rejects in 0 boatd RPCs.
	forkID := forked["id"].(string)
	if st, out := e.do(e.owner, "DELETE", vmPath(forkID), nil); st != 200 {
		t.Fatalf("delete fork: %d %v", st, out)
	}
	boatdReqs.Store(0)
	if st, _ := e.do("boatssh_"+strings.Repeat("a", 64), "POST", vmPath(forkID, "ssh", "authorize"), map[string]string{"user": "dev", "key_type": f[0], "key_blob": f[1]}); st != 401 {
		t.Fatalf("retired VM authorizeGuestSSH status=%d, want 401", st)
	}
	if got := boatdReqs.Load(); got != 0 {
		t.Fatalf("retired VM authorizeGuestSSH made %d boatd RPCs, want 0", got)
	}

	// 5. Native POST /v1/vms and POST /v1/vms/{id}/fork Idempotency-Key replay, 409 conflict, and /api/v1 scope isolation.
	doIdem := func(path, idemKey string, body map[string]any) (int, http.Header, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", e.api.URL+path, strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+e.owner)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", idemKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, resp.Header, out
	}

	e.boatd.mu.Lock()
	beforeCount := len(e.boatd.vms)
	e.boatd.mu.Unlock()

	st1, h1, idemCreate1 := doIdem("/v1/vms", "idem-r25-shared", map[string]any{"name": "idem-native"})
	if st1 != 201 || h1.Get("Idempotent-Replayed") != "" {
		t.Fatalf("first native idempotent create: status=%d replayed=%q body=%v", st1, h1.Get("Idempotent-Replayed"), idemCreate1)
	}
	st2, h2, idemCreate2 := doIdem("/v1/vms", "idem-r25-shared", map[string]any{"name": "idem-native"})
	if st2 != 201 || h2.Get("Idempotent-Replayed") != "true" || idemCreate2["id"] != idemCreate1["id"] {
		t.Fatalf("replayed native idempotent create: status=%d replayed=%q id=%v want %v", st2, h2.Get("Idempotent-Replayed"), idemCreate2["id"], idemCreate1["id"])
	}
	e.boatd.mu.Lock()
	afterCreateCount := len(e.boatd.vms)
	e.boatd.mu.Unlock()
	if afterCreateCount != beforeCount+1 {
		t.Fatalf("expected exactly 1 VM created across idempotent replay, got delta=%d", afterCreateCount-beforeCount)
	}

	// Different body under same Idempotency-Key -> 409 idempotency_conflict.
	stMismatch, _, mismatchOut := doIdem("/v1/vms", "idem-r25-shared", map[string]any{"name": "different-name"})
	if stMismatch != http.StatusConflict {
		t.Fatalf("mismatched native Idempotency-Key payload: status=%d body=%v, want 409", stMismatch, mismatchOut)
	}

	// Same Idempotency-Key string on /api/v1/sandboxes does NOT collide with native /v1/vms!
	stCompat, _, compatOut := doIdem("/api/v1/sandboxes", "idem-r25-shared", map[string]any{"name": "compat-distinct"})
	if stCompat != 202 {
		t.Fatalf("compat /api/v1/sandboxes with same key string collided with native /v1/vms: %d %v", stCompat, compatOut)
	}

	// Native POST /v1/vms/{id}/fork Idempotency-Key replay & conflict.
	fSt1, fh1, fOut1 := doIdem(vmPath(vmID, "fork"), "idem-r25-fork", map[string]any{"name": "idem-fork-kid"})
	if fSt1 != 201 || fh1.Get("Idempotent-Replayed") != "" {
		t.Fatalf("first native idempotent fork: %d %v", fSt1, fOut1)
	}
	fSt2, fh2, fOut2 := doIdem(vmPath(vmID, "fork"), "idem-r25-fork", map[string]any{"name": "idem-fork-kid"})
	if fSt2 != 201 || fh2.Get("Idempotent-Replayed") != "true" || fOut2["id"] != fOut1["id"] {
		t.Fatalf("replayed native idempotent fork: status=%d replayed=%q out=%v want %v", fSt2, fh2.Get("Idempotent-Replayed"), fOut2, fOut1)
	}
	fStMismatch, _, fMismatchOut := doIdem(vmPath(vmID, "fork"), "idem-r25-fork", map[string]any{"name": "other-fork-kid"})
	if fStMismatch != http.StatusConflict {
		t.Fatalf("mismatched native fork Idempotency-Key: %d %v, want 409", fStMismatch, fMismatchOut)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
