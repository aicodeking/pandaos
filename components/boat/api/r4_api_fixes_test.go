package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- A: Idempotency cache route-gen hoisting + reverse index ----

func BenchmarkIdemFinish(b *testing.B) {
	dir := b.TempDir()
	st, err := OpenStore(filepath.Join(dir, "state.json"))
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC()
	const numVMs = 200
	const numRecords = 8000

	teamID := "team_bench"
	keyID := "key_bench"
	if err := st.Update(func(s *State) error {
		s.Users["u1"] = &User{ID: "u1", Name: "bench"}
		s.Teams[teamID] = &Team{ID: teamID, Name: "bench", Members: []Member{{UserID: "u1", Role: "owner"}}}
		s.Keys[keyID] = &APIKey{ID: keyID, TeamID: teamID, UserID: "u1", Scopes: allScopes, Gen: 1}
		for i := 0; i < numVMs; i++ {
			vmID := fmt.Sprintf("vm-%04d", i)
			s.RouteGens[vmID] = 1
			s.PortMinters[vmID] = []string{keyID}
			s.CompatByVM[vmID] = fmt.Sprintf("bx_%08d", i)
			s.VMTokens[vmID] = &VMToken{TeamID: teamID, Gen: 1}
			s.AIVMs[vmID] = &AIVM{TeamID: teamID, Gen: 1}
		}
		for i := 0; i < numRecords; i++ {
			vmID := fmt.Sprintf("vm-%04d", i%numVMs)
			tPrefix := fmt.Sprintf("team_%02d", i/800)
			if i/800 == 0 {
				tPrefix = teamID
			}
			k := fmt.Sprintf("%s\x00rec-%05d", tPrefix, i)
			s.Idempotency[k] = &IdemRecord{
				Hash:      "h",
				Status:    202,
				CType:     "application/json",
				Body:      []byte(`{"ok":true}`),
				At:        now.Add(-time.Minute),
				KeyID:     keyID,
				KeyGen:    1,
				TeamID:    teamID,
				VMIDs:     []string{vmID},
				RouteGens: map[string]int{vmID: 1},
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}

	c := &idemCache{store: st, now: func() time.Time { return now }}
	p := &principal{
		User:   &User{ID: "u1"},
		Team:   &Team{ID: teamID},
		Key:    &APIKey{ID: keyID, TeamID: teamID, UserID: "u1", Gen: 1},
		KeyGen: 1,
	}
	body := []byte(`{"ok":true,"type":"sandbox.created"}`)
	vmIDs := []string{"vm-0000"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k := fmt.Sprintf("team_bench_hot\x00bench-%d", i%100)
		c.mu.Lock()
		if c.pending == nil {
			c.pending = map[string]string{}
		}
		c.pending[k] = "hash"
		c.mu.Unlock()
		c.finishWithMeta(k, 202, "application/json", body, p, vmIDs)
	}
}

func TestR4IdemFinishConstantRouteGenSnapshots(t *testing.T) {
	orig := idemRouteGensSnapshot
	defer func() { idemRouteGensSnapshot = orig }()

	var calls atomic.Int64
	idemRouteGensSnapshot = func(st *State, now time.Time) map[string]int {
		calls.Add(1)
		return orig(st, now)
	}

	for _, count := range []int{10, 500, maxIdemPerTeam} {
		t.Run(fmt.Sprintf("records=%d", count), func(t *testing.T) {
			e := setup(t)
			now := time.Now().UTC()
			e.srv.idem.now = func() time.Time { return now }
			teamID := e.teamID()
			p := e.srv.authenticate("Bearer " + e.owner)

			if err := e.srv.store.Update(func(st *State) error {
				for i := 0; i < 50; i++ {
					vmID := fmt.Sprintf("vm-%03d", i)
					st.RouteGens[vmID] = 1
					st.PortMinters[vmID] = []string{p.Key.ID}
					st.CompatByVM[vmID] = fmt.Sprintf("bx_%08d", i)
				}
				for i := 0; i < count; i++ {
					vmID := fmt.Sprintf("vm-%03d", i%50)
					k := fmt.Sprintf("%s\x00seed-%04d", teamID, i)
					st.Idempotency[k] = &IdemRecord{
						Hash:      "h",
						Status:    202,
						CType:     "application/json",
						Body:      []byte(`{"ok":true}`),
						At:        now.Add(-time.Duration(count-i) * time.Second),
						KeyID:     p.Key.ID,
						KeyGen:    p.KeyGen,
						TeamID:    teamID,
						VMIDs:     []string{vmID},
						RouteGens: map[string]int{vmID: 1},
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			calls.Store(0)
			k := teamID + "\x00fresh-key"
			if rec, code := e.srv.idem.begin(k, "fresh-hash"); rec != nil || code != "" {
				t.Fatalf("begin: rec=%v code=%q", rec, code)
			}
			e.srv.idem.finishWithMeta(k, 202, "application/json", []byte(`{"ok":true}`), p, []string{"vm-000"})
			if got := calls.Load(); got > 1 {
				t.Fatalf("bumpedRouteGensSnapshot called %d times for %d entries, want <= 1", got, count)
			}
		})
	}
}

func TestR4IdemReverseIndexForgetVM(t *testing.T) {
	e := setup(t)
	k1 := map[string]string{"Idempotency-Key": "vm-one"}
	k2 := map[string]string{"Idempotency-Key": "vm-two"}
	st1, a, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "one"}, k1)
	st2, b, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "two"}, k2)
	if st1 != 202 || st2 != 202 {
		t.Fatalf("creates: %d %d", st1, st2)
	}
	bx1 := a["sandbox"].(map[string]any)["id"].(string)
	bx2 := b["sandbox"].(map[string]any)["id"].(string)
	vm1 := e.nativeOf(bx1)
	vm2 := e.nativeOf(bx2)

	// Delete vm1: its idempotency record is dropped via reverse index, vm2's stays.
	if st, out, _ := e.compat(e.owner, "DELETE", "/api/v1/sandboxes/"+bx1, nil, map[string]string{"X-Ascii-Confirm-Delete": bx1}); st != 202 {
		t.Fatalf("delete bx1: %d %v", st, out)
	}
	snap := e.srv.store.Snapshot()
	if snap.Idempotency[e.teamID()+"\x00vm-one"] != nil {
		t.Fatalf("forgetVM(%s) left idempotency entry behind", vm1)
	}
	if snap.Idempotency[e.teamID()+"\x00vm-two"] == nil {
		t.Fatalf("forgetVM(%s) evicted unrelated vm %s idempotency entry", vm1, vm2)
	}
	// Replaying vm-two still hits cache; replaying vm-one creates a fresh sandbox.
	st2r, b2, hdr2 := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "two"}, k2)
	if st2r != 202 || hdr2.Get("Idempotent-Replayed") != "true" || b2["sandbox"].(map[string]any)["id"] != bx2 {
		t.Fatalf("vm-two replay failed: %d %v", st2r, b2)
	}
	st1r, a2, hdr1 := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "one"}, k1)
	if st1r != 202 || hdr1.Get("Idempotent-Replayed") == "true" || a2["sandbox"].(map[string]any)["id"] == bx1 {
		t.Fatalf("vm-one replayed deleted sandbox: %d %v", st1r, a2)
	}
}

// ---- B: AIVMs shallow clone + cowAIVM ----

func BenchmarkCloneForWriteAIVMs(b *testing.B) {
	for _, tombstones := range []int{0, maxRetiredVMTombstones} {
		b.Run(fmt.Sprintf("tombstones=%d", tombstones), func(b *testing.B) {
			st := &State{}
			st.init()
			for i := 0; i < tombstones; i++ {
				id := fmt.Sprintf("vm-retired-%05d", i)
				st.VMTokens[id] = &VMToken{TeamID: "t1", Gen: 1, Retired: true}
				st.AIVMs[id] = &AIVM{TeamID: "t1", Gen: 1, Retired: true, Blocked: true}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = st.cloneForWrite()
			}
		})
	}
}

func TestR4AIVMsShallowCloneAndCoWIsolation(t *testing.T) {
	e := setup(t)
	team := e.teamID()
	if err := e.srv.store.Update(func(st *State) error {
		st.AIVMs["vm-shared"] = &AIVM{TeamID: team, Gen: 2, Blocked: false}
		st.AIVMs["vm-tomb"] = &AIVM{TeamID: team, Gen: 1, Retired: true, Blocked: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snap1 := e.srv.store.Snapshot()
	sharedBefore := snap1.AIVMs["vm-shared"]
	tombBefore := snap1.AIVMs["vm-tomb"]

	// Unrelated Update must share both *AIVM pointers (no deep clone).
	if err := e.srv.store.Update(func(st *State) error {
		st.VMNames["unrelated"] = "x"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snap2 := e.srv.store.Snapshot()
	if snap2.AIVMs["vm-shared"] != sharedBefore || snap2.AIVMs["vm-tomb"] != tombBefore {
		t.Fatal("unrelated Update deep-cloned immutable AIVMs entries")
	}

	// Mutating vm-shared via aiBumpVM and retireCredentials must CoW-copy vm-shared
	// without mutating snap1 or cloning untouched vm-tomb.
	if _, err := e.srv.aiBumpVM("vm-shared", team, false); err != nil {
		t.Fatal(err)
	}
	snap3 := e.srv.store.Snapshot()
	if snap1.AIVMs["vm-shared"].Gen != 2 {
		t.Fatalf("snap1 mutated in place: Gen=%d, want 2", snap1.AIVMs["vm-shared"].Gen)
	}
	if snap3.AIVMs["vm-shared"].Gen != 3 || snap3.AIVMs["vm-shared"] == sharedBefore {
		t.Fatalf("snap3 did not CoW bump Gen: %+v", snap3.AIVMs["vm-shared"])
	}
	if snap3.AIVMs["vm-tomb"] != tombBefore {
		t.Fatal("mutating vm-shared cloned untouched vm-tomb")
	}

	if err := e.srv.retireVM("vm-shared"); err != nil {
		t.Fatal(err)
	}
	snap4 := e.srv.store.Snapshot()
	if snap3.AIVMs["vm-shared"].Retired {
		t.Fatal("retireVM mutated snap3 AIVM in place")
	}
	if !snap4.AIVMs["vm-shared"].Retired || !snap4.AIVMs["vm-shared"].Blocked {
		t.Fatalf("snap4 vm-shared not retired: %+v", snap4.AIVMs["vm-shared"])
	}
	if snap4.AIVMs["vm-tomb"] != tombBefore {
		t.Fatal("retireVM cloned untouched vm-tomb")
	}
}

// ---- C: Jump-key sync stopped-VM deferral + scoped re-attestation ----

func TestR4JumpSyncSkipsStoppedVMAndStopsExpiryPolling(t *testing.T) {
	e := setup(t)
	_, vm := e.jumpSetup()
	e.grantSSH(e.owner, vm)
	e.assertSSH(vm, r11Pub, 200)

	// Stop the VM before revoking its grant.
	if st, out := e.do(e.owner, "POST", vmPath(vm, "stop"), nil); st != 200 {
		t.Fatalf("stop: %d %v", st, out)
	}

	var vmListCalls, sshPolicyCalls, sshViewCalls atomic.Int32
	orig := e.srv.boatd.hc.Transport
	if orig == nil {
		orig = http.DefaultTransport
	}
	e.srv.boatd.hc.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" && r.URL.Path == "/v1/vms" {
			vmListCalls.Add(1)
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/ssh-policy") {
			sshPolicyCalls.Add(1)
		}
		if r.Method == "GET" && r.URL.Path == vmPath(vm) && r.URL.Query().Get("view") == "ssh" {
			sshViewCalls.Add(1)
		}
		return orig.RoundTrip(r)
	})

	// Revoke SSH grant while VM is stopped: must NOT call prepareSSHPolicy on stopped VM.
	if st, out := e.do(e.owner, "DELETE", vmPath(vm, "ssh", url.PathEscape(sshFingerprint(r11Pub))), nil); st != 200 {
		t.Fatalf("revoke: %d %v", st, out)
	}
	if sshPolicyCalls.Load() != 0 || sshViewCalls.Load() != 0 {
		t.Fatalf("stopped VM revocation called prepareSSHPolicy: policy=%d view=%d", sshPolicyCalls.Load(), sshViewCalls.Load())
	}

	// Run jumpExpiryLoop across multiple ticks: stopped VM is deferred, so 0 GET /v1/vms calls.
	vmListCalls.Store(0)
	ctx, cancel := context.WithCancel(context.Background())
	go e.srv.jumpExpiryLoop(ctx, 10*time.Millisecond)
	time.Sleep(80 * time.Millisecond)
	cancel()
	if got := vmListCalls.Load(); got != 0 {
		t.Fatalf("jumpExpiryLoop polled boatd %d times while only stopped VM sat in CleanupVMs, want 0", got)
	}

	// Start the stopped VM again: it must come up with the revoked SSH state (204)
	// and clear the deferred marker so cleanup can drain.
	if st, out := e.do(e.owner, "POST", vmPath(vm, "start"), nil); st != 200 {
		t.Fatalf("start: %d %v", st, out)
	}
	if code, _ := e.lookupSSH(vm, r11Pub); code != 204 {
		t.Fatalf("started VM still authorized revoked key: %d, want 204", code)
	}
	if err := e.srv.syncJumpKeysForVM(context.Background(), vm); err != nil {
		t.Fatalf("sync after start: %v", err)
	}
	if len(e.srv.store.Snapshot().SSHGrants) != 0 {
		t.Fatalf("cleanup debt not drained after VM started: %v", e.srv.store.Snapshot().SSHGrants)
	}
}

func TestR4JumpSyncScopedReattestation(t *testing.T) {
	e := setup(t)
	e.enableSSH()

	// Team A: owner + member + 2 running VMs; Team B: other team + 1 running VM.
	vmA1 := e.createVM()
	vmA2 := e.createVM()
	_, tokB := e.newTeam("team-b")
	stB, outB := e.do(tokB, "POST", "/v1/vms", map[string]any{"name": "b1"})
	if stB != 201 {
		t.Fatalf("create vmB1: %d %v", stB, outB)
	}
	vmB1 := outB["id"].(string)

	// Track POST /ssh-policy per VM.
	var mu sync.Mutex
	policyHits := map[string]int{}
	orig := e.srv.boatd.hc.Transport
	if orig == nil {
		orig = http.DefaultTransport
	}
	e.srv.boatd.hc.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/ssh-policy") {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if len(parts) == 4 {
				mu.Lock()
				policyHits[parts[2]]++
				mu.Unlock()
			}
		}
		return orig.RoundTrip(r)
	})
	resetHits := func() {
		// Force ssh_policy_ready=false on all 3 VMs so any re-attestation would issue POST /ssh-policy.
		for _, id := range []string{vmA1, vmA2, vmB1} {
			e.requireSSHInstall(id)
		}
		mu.Lock()
		policyHits = map[string]int{}
		mu.Unlock()
	}
	hits := func(id string) int {
		mu.Lock()
		defer mu.Unlock()
		return policyHits[id]
	}

	// 1. Single-VM grant revoke on vmA1: re-attests ONLY vmA1 (not vmA2, not vmB1).
	e.grantSSH(e.owner, vmA1)
	resetHits()
	if st, out := e.do(e.owner, "DELETE", vmPath(vmA1, "ssh", url.PathEscape(sshFingerprint(r11Pub))), nil); st != 200 {
		t.Fatalf("revoke vmA1: %d %v", st, out)
	}
	if hits(vmA1) != 1 || hits(vmA2) != 0 || hits(vmB1) != 0 {
		t.Fatalf("single-VM revoke policy hits: vmA1=%d vmA2=%d vmB1=%d, want 1/0/0", hits(vmA1), hits(vmA2), hits(vmB1))
	}
	if c, _ := e.lookupSSH(vmA1, r11Pub); c != 204 {
		t.Fatalf("revoked key still allowed on vmA1: %d", c)
	}

	// 2. Key delete in Team A: re-attests Team A's VMs, NEVER Team B's vmB1.
	stK, kOut := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "del-key", "scopes": []string{"vms:read", "exec"}})
	if stK != 201 {
		t.Fatalf("create key: %d %v", stK, kOut)
	}
	e.grantSSH(kOut["token"].(string), vmA1)
	resetHits()
	if st, out := e.do(e.owner, "DELETE", "/v1/keys/"+kOut["id"].(string), nil); st != 200 {
		t.Fatalf("delete key: %d %v", st, out)
	}
	if hits(vmA1) == 0 || hits(vmA2) == 0 || hits(vmB1) != 0 {
		t.Fatalf("deleteKey policy hits: vmA1=%d vmA2=%d vmB1=%d, want >0/>0/0", hits(vmA1), hits(vmA2), hits(vmB1))
	}
	if c, _ := e.lookupSSH(vmA1, r11Pub); c != 204 {
		t.Fatalf("deleted key's grant still allowed: %d", c)
	}

	// 3. Key rotate (overlap_s=0) in Team A: re-attests Team A's VMs, NEVER Team B's vmB1.
	stK2, kOut2 := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "rot-key", "scopes": []string{"vms:read", "exec"}})
	if stK2 != 201 {
		t.Fatalf("create key2: %d %v", stK2, kOut2)
	}
	e.grantSSH(kOut2["token"].(string), vmA1)
	resetHits()
	if st, out := e.do(e.owner, "POST", "/v1/keys/"+kOut2["id"].(string)+"/rotate", map[string]any{"overlap_s": 0}); st != 200 {
		t.Fatalf("rotate key: %d %v", st, out)
	}
	if hits(vmA1) == 0 || hits(vmA2) == 0 || hits(vmB1) != 0 {
		t.Fatalf("rotateKey policy hits: vmA1=%d vmA2=%d vmB1=%d, want >0/>0/0", hits(vmA1), hits(vmA2), hits(vmB1))
	}
	if c, _ := e.lookupSSH(vmA1, r11Pub); c != 204 {
		t.Fatalf("rotated key's grant still allowed: %d", c)
	}

	// 4. Member removal in Team A: re-attests Team A's VMs, NEVER Team B's vmB1.
	memTok, memUID := e.addMember("member")
	e.grantSSH(memTok, vmA2)
	resetHits()
	if st, out := e.do(e.owner, "DELETE", "/v1/teams/"+e.teamID()+"/members/"+memUID, nil); st != 200 {
		t.Fatalf("remove member: %d %v", st, out)
	}
	if hits(vmA1) == 0 || hits(vmA2) == 0 || hits(vmB1) != 0 {
		t.Fatalf("removeMember policy hits: vmA1=%d vmA2=%d vmB1=%d, want >0/>0/0", hits(vmA1), hits(vmA2), hits(vmB1))
	}
	if c, _ := e.lookupSSH(vmA2, r11Pub); c != 204 {
		t.Fatalf("removed member's grant still allowed: %d", c)
	}
}

// ---- D: Detached command start single RTT + attach backoff ----

func TestR4DetachedStartSingleRTTAndAttachBackoff(t *testing.T) {
	e := setup(t)
	count := countGuestOps(t, e)
	bx := e.newSandbox(nil)
	vmID := e.nativeOf(bx)

	// Modern guest returns pid in cmd_start -> 1 cmd_start, 0 cmd_status.
	s0, st0 := count("cmd_start"), count("cmd_status")
	started := e.ok("POST", "/api/v1/sandboxes/"+bx+"/commands", map[string]any{"command": "echo fast", "detached": true}, 200)
	if started["pid"] != float64(42) || started["processId"] != float64(1) {
		t.Fatalf("detached start: %v", started)
	}
	if count("cmd_start")-s0 != 1 || count("cmd_status")-st0 != 0 {
		t.Fatalf("detached start RPCs: cmd_start=%d cmd_status=%d, want 1/0", count("cmd_start")-s0, count("cmd_status")-st0)
	}
	if len(e.srv.store.Snapshot().Commands[vmID]) != 1 {
		t.Fatalf("command history not recorded: %v", e.srv.store.Snapshot().Commands[vmID])
	}

	// Legacy guest omits pid in cmd_start -> falls back to 1 cmd_status.
	e.boatd.mu.Lock()
	e.boatd.legacyCmdStart = true
	e.boatd.mu.Unlock()
	s1, st1 := count("cmd_start"), count("cmd_status")
	legacy := e.ok("POST", "/api/v1/sandboxes/"+bx+"/commands", map[string]any{"command": "echo legacy", "detached": true}, 200)
	if legacy["pid"] != float64(42) {
		t.Fatalf("legacy detached start pid: %v", legacy)
	}
	if count("cmd_start")-s1 != 1 || count("cmd_status")-st1 != 1 {
		t.Fatalf("legacy detached start RPCs: cmd_start=%d cmd_status=%d, want 1/1", count("cmd_start")-s1, count("cmd_status")-st1)
	}
	e.boatd.mu.Lock()
	e.boatd.legacyCmdStart = false
	e.boatd.mu.Unlock()

	// Attach backoff: a command that finishes after 25ms finishes well before the 250ms cap.
	slow := e.ok("POST", "/api/v1/sandboxes/"+bx+"/commands", map[string]any{"command": "sleep-forever", "detached": true}, 200)
	time.AfterFunc(25*time.Millisecond, func() { e.finishAllCmds() })
	t0 := time.Now()
	fr := e.attachFrames("/api/v1/sandboxes/" + bx + "/commands/" + jsonNum(slow["processId"]) + "/stream")
	elapsed := time.Since(t0)
	if fr[len(fr)-1]["type"] != "exit" {
		t.Fatalf("attach frames: %v", fr)
	}
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("attach took %v for a 25ms command; expected short initial backoff (<200ms)", elapsed)
	}
}

// ---- E: Real-boatd-shaped 4xx guest error mapping ----

func TestR4GuestErrorMappingNativeAndCompat(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	id := e.nativeOf(bx)

	// Seed a file and a directory (by placing a child under /home/dev/dir/).
	if st, out := e.do(e.owner, "PUT", vmPath(id, "files")+"?path=/home/dev/src.txt", "hello"); st != 200 {
		t.Fatalf("put src: %d %v", st, out)
	}
	if st, out := e.do(e.owner, "PUT", vmPath(id, "files")+"?path=/home/dev/dir/child.txt", "x"); st != 200 {
		t.Fatalf("put child: %d %v", st, out)
	}

	checkNoPrefix := func(t *testing.T, msg string) {
		t.Helper()
		if strings.Contains(msg, guestPrefix) {
			t.Errorf("client-visible message leaked %q: %q", guestPrefix, msg)
		}
	}

	// 1. vmOK + guest 404 (not_found): preserves guest message, never bare "not found" or "vm not found".
	st, out := e.do(e.owner, "GET", vmPath(id, "commands", "c_missing"), nil)
	er, _ := out["error"].(map[string]any)
	msg, _ := er["message"].(string)
	if st != 404 || er["code"] != "not_found" || msg == "not found" || msg == "vm not found" || !strings.Contains(msg, "no such command") {
		t.Errorf("native vmOK+guest404: %d %v", st, out)
	}
	checkNoPrefix(t, msg)

	st, out = e.do(e.owner, "POST", vmPath(id, "files", "move"), map[string]any{"from": "/home/dev/missing.txt", "to": "/home/dev/dst.txt"})
	er, _ = out["error"].(map[string]any)
	msg, _ = er["message"].(string)
	if st != 404 || er["code"] != "not_found" || !strings.Contains(msg, "no such file") {
		t.Errorf("native file_move 404: %d %v", st, out)
	}
	checkNoPrefix(t, msg)

	st, cout, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/files/move", map[string]any{"source": "missing.txt", "destination": "dst.txt"}, nil)
	cmsg, _ := cout["message"].(string)
	if st != 404 || cout["ok"] != false || errCode(cout) != "not_found" || !strings.Contains(cmsg, "no such file") {
		t.Errorf("compat file_move 404: %d %v", st, cout)
	}
	checkNoPrefix(t, cmsg)

	// 2. spawn_failed (missing binary & missing cwd) -> 400 bad_request.
	for _, body := range []map[string]any{
		{"cmd": []string{"no-such-bin"}},
		{"cmd": []string{"echo", "hi"}, "cwd": "/no-such-dir"},
	} {
		st, out = e.do(e.owner, "POST", vmPath(id, "commands"), body)
		er, _ = out["error"].(map[string]any)
		msg, _ = er["message"].(string)
		if st != 400 || er["code"] != "bad_request" || msg == "" {
			t.Errorf("native spawn_failed %v: %d %v", body, st, out)
		}
		checkNoPrefix(t, msg)
	}

	// 3. permission_denied -> 403 permission_denied (native & compat).
	st, out = e.do(e.owner, "POST", vmPath(id, "commands"), map[string]any{"cmd": []string{"permission-denied"}})
	er, _ = out["error"].(map[string]any)
	msg, _ = er["message"].(string)
	if st != 403 || er["code"] != "permission_denied" || !strings.Contains(msg, "permission denied") {
		t.Errorf("native permission_denied: %d %v", st, out)
	}
	checkNoPrefix(t, msg)

	st, cout, _ = e.compat(e.owner, "DELETE", "/api/v1/sandboxes/"+bx+"/files?path=/root/denied/x", nil, nil)
	cmsg, _ = cout["message"].(string)
	if st != 403 || cout["ok"] != false || errCode(cout) != "permission_denied" || !strings.Contains(cmsg, "permission denied") {
		t.Errorf("compat permission_denied: %d %v", st, cout)
	}
	checkNoPrefix(t, cmsg)

	// 4. file_move onto a directory (is_a_directory) -> 409 conflict with message preserved.
	st, out = e.do(e.owner, "POST", vmPath(id, "files", "move"), map[string]any{"from": "/home/dev/src.txt", "to": "/home/dev/dir"})
	er, _ = out["error"].(map[string]any)
	msg, _ = er["message"].(string)
	if st != 409 || er["code"] != "conflict" || !strings.Contains(msg, "is a directory") {
		t.Errorf("native file_move is_a_directory: %d %v", st, out)
	}
	checkNoPrefix(t, msg)

	st, cout, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/files/move", map[string]any{"source": "src.txt", "destination": "dir"}, nil)
	cmsg, _ = cout["message"].(string)
	if st != 409 || cout["ok"] != false || errCode(cout) != "conflict" || !strings.Contains(cmsg, "is a directory") {
		t.Errorf("compat file_move is_a_directory: %d %v", st, cout)
	}
	checkNoPrefix(t, cmsg)

	// 5. invalid_user, invalid_argument (unknown op), and unsupported via agentOpHook.
	for _, tc := range []struct {
		name       string
		status     int
		errMsg     string
		code       string
		wantStatus int
		wantCode   string
	}{
		{"invalid_user", 400, "guest agent: no such user: ghost", "invalid_user", 400, "bad_request"},
		{"invalid_argument_unknown_op", 400, "guest agent: unknown op: bogus", "invalid_argument", 400, "bad_request"},
		{"unsupported", 422, "guest agent: desktop is not installed in this image (slim variant)", "unsupported", 422, "not_supported"},
		{"unsupported_legacy_400", 400, "guest agent: desktop is not installed in this image (slim variant)", "unsupported", 422, "not_supported"},
	} {
		e.boatd.mu.Lock()
		e.boatd.agentOpHook = func(op string, _ map[string]any) (int, map[string]any, bool) {
			if op == "cmd_start" {
				return tc.status, map[string]any{"error": tc.errMsg, "code": tc.code}, true
			}
			return 0, nil, false
		}
		e.boatd.mu.Unlock()
		st, out = e.do(e.owner, "POST", vmPath(id, "commands"), map[string]any{"cmd": []string{"true"}})
		er, _ = out["error"].(map[string]any)
		msg, _ = er["message"].(string)
		if st != tc.wantStatus || er["code"] != tc.wantCode || msg != strings.TrimPrefix(tc.errMsg, guestPrefix) {
			t.Errorf("%s native: %d %v", tc.name, st, out)
		}
		checkNoPrefix(t, msg)
		st, cout, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/commands", map[string]any{"command": "true", "detached": true}, nil)
		cmsg, _ = cout["message"].(string)
		if st != tc.wantStatus || cout["ok"] != false || errCode(cout) != tc.wantCode || cmsg != strings.TrimPrefix(tc.errMsg, guestPrefix) {
			t.Errorf("%s compat: %d %v", tc.name, st, cout)
		}
		checkNoPrefix(t, cmsg)
	}
	e.boatd.mu.Lock()
	e.boatd.agentOpHook = nil
	e.boatd.mu.Unlock()
}

// ---- F: compatSyncExec CompatError + ExecEvent.code OpenAPI ----

func TestR4CompatSyncExecErrorAndExecEventCodeOpenAPI(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	id := e.nativeOf(bx)

	cases := []struct {
		name       string
		frame      map[string]any
		wantStatus int
		wantCode   string
	}{
		{"spawn_failed", map[string]any{"error": "guest agent: exec failed: no such file", "code": "spawn_failed"}, 400, "bad_request"},
		{"invalid_user", map[string]any{"error": "guest agent: no such user: ghost", "code": "invalid_user"}, 400, "bad_request"},
		{"not_found", map[string]any{"error": "guest agent: missing path", "code": "not_found"}, 400, "bad_request"},
		{"invalid_argument", map[string]any{"error": "guest agent: bad argv", "code": "invalid_argument"}, 400, "bad_request"},
		{"invalid_argument_unknown_op", map[string]any{"error": "guest agent: unknown op: bogus", "code": "invalid_argument"}, 400, "bad_request"},
		{"unsupported", map[string]any{"error": "guest agent: unsupported op", "code": "unsupported"}, 422, "not_supported"},
		{"permission_denied", map[string]any{"error": "guest agent: permission denied", "code": "permission_denied"}, 403, "permission_denied"},
		{"stream_break_no_code", map[string]any{"error": "guest agent: vsock reset"}, 502, "http_502"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e.boatd.mu.Lock()
			e.boatd.execErr = tc.frame
			e.boatd.mu.Unlock()
			defer func() {
				e.boatd.mu.Lock()
				e.boatd.execErr = nil
				e.boatd.mu.Unlock()
			}()

			// Synchronous compat command must answer CompatError (never 200 with "error").
			st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/commands", map[string]any{"command": "echo x"}, nil)
			if st != tc.wantStatus || out["ok"] != false || out["type"] != "sandbox.error" || errCode(out) != tc.wantCode {
				t.Fatalf("compat sync exec %s: status=%d out=%v, want %d %s", tc.name, st, out, tc.wantStatus, tc.wantCode)
			}
			if msg, _ := out["message"].(string); strings.Contains(msg, guestPrefix) {
				t.Errorf("compat sync exec leaked prefix: %q", msg)
			}

			// Streamed compat command keeps a schema-valid terminal error frame.
			stS, outS, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/commands", map[string]any{"command": "echo x", "stream": true}, nil)
			if stS != 200 {
				t.Fatalf("compat stream exec: %d %v", stS, outS)
			}
			raw, _ := outS["_raw"].(string)
			if strings.Contains(raw, guestPrefix) {
				t.Errorf("compat stream exec leaked prefix: %q", raw)
			}

			// Native exec relays {"error", "code"} and validates against ExecEvent in openapi.json.
			req, _ := http.NewRequest("POST", e.api.URL+vmPath(id, "exec"), strings.NewReader(`{"cmd":["echo","x"]}`))
			req.Header.Set("Authorization", "Bearer "+e.owner)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("native exec status=%d body=%s", resp.StatusCode, body)
			}
			var ev map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(body), &ev); err != nil {
				t.Fatalf("native exec frame %q: %v", body, err)
			}
			if tc.frame["code"] != nil && ev["code"] != tc.frame["code"] {
				t.Fatalf("native exec frame code=%v, want %v", ev["code"], tc.frame["code"])
			}
		})
	}
}

// ---- G: cwd unification across native exec, prompt, commands, prompts ----

func TestR4CwdUnification(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	id := e.nativeOf(bx)

	goodCwds := []string{"", "/tmp", "~", "~/work", "subdir", "a/b/c"}
	badCwds := []string{"bad\x00cwd", "bad\ncwd", "bad\rcwd", "bad\tcwd", "bad\x1bcwd", "bad\x7fcwd"}

	for _, cwd := range goodCwds {
		// Native exec
		req, _ := http.NewRequest("POST", e.api.URL+vmPath(id, "exec"), strings.NewReader(fmt.Sprintf(`{"cmd":["echo","ok"],"cwd":%q}`, cwd)))
		req.Header.Set("Authorization", "Bearer "+e.owner)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("exec cwd %q = %d, want 200", cwd, resp.StatusCode)
		}
		// Native prompt (sync)
		reqP, _ := http.NewRequest("POST", e.api.URL+vmPath(id, "prompt"), strings.NewReader(fmt.Sprintf(`{"prompt":"hi","cwd":%q}`, cwd)))
		reqP.Header.Set("Authorization", "Bearer "+e.owner)
		reqP.Header.Set("Content-Type", "application/json")
		respP, err := http.DefaultClient.Do(reqP)
		if err != nil {
			t.Fatal(err)
		}
		respP.Body.Close()
		if respP.StatusCode != 200 {
			t.Errorf("prompt cwd %q = %d, want 200", cwd, respP.StatusCode)
		}
		// Native detached command
		if st, out := e.do(e.owner, "POST", vmPath(id, "commands"), map[string]any{"cmd": []string{"echo", "ok"}, "cwd": cwd}); st != 201 {
			t.Errorf("commands cwd %q = %d %v, want 201", cwd, st, out)
		}
		// Native async prompt
		if st, out := e.do(e.owner, "POST", vmPath(id, "prompts"), map[string]any{"prompt": "hi", "cwd": cwd, "new": true}); st != 202 {
			t.Errorf("prompts cwd %q = %d %v, want 202", cwd, st, out)
		}
	}

	for _, cwd := range badCwds {
		if st, _ := e.do(e.owner, "POST", vmPath(id, "exec"), map[string]any{"cmd": []string{"echo", "x"}, "cwd": cwd}); st != 400 {
			t.Errorf("exec bad cwd %q = %d, want 400", cwd, st)
		}
		if st, _ := e.do(e.owner, "POST", vmPath(id, "prompt"), map[string]any{"prompt": "hi", "cwd": cwd}); st != 400 {
			t.Errorf("prompt bad cwd %q = %d, want 400", cwd, st)
		}
		if st, _ := e.do(e.owner, "POST", vmPath(id, "commands"), map[string]any{"cmd": []string{"echo", "x"}, "cwd": cwd}); st != 400 {
			t.Errorf("commands bad cwd %q = %d, want 400", cwd, st)
		}
		if st, _ := e.do(e.owner, "POST", vmPath(id, "prompts"), map[string]any{"prompt": "hi", "cwd": cwd, "new": true}); st != 400 {
			t.Errorf("prompts bad cwd %q = %d, want 400", cwd, st)
		}
		if st, _, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/commands", map[string]any{"command": "echo x", "cwd": cwd}, nil); st != 400 {
			t.Errorf("compat commands bad cwd %q = %d, want 400", cwd, st)
		}
	}
}

// ---- H: Protected snapshot export/import ----

func TestR4ProtectedSnapshotExportImport(t *testing.T) {
	e := setup(t)
	if st, out := e.do(e.owner, "PUT", "/v1/secrets/TOP_SECRET", map[string]any{"value": "shh", "inject": true}); st != 200 {
		t.Fatalf("put secret: %d %v", st, out)
	}

	// 1. Trusted protected no_env export -> import -> create succeeds (no 409), keeps no_env, refuses secrets.
	protVM := e.createVMWith(map[string]any{"no_env": true})
	st, snap := e.do(e.owner, "POST", vmPath(protVM, "snapshots"), map[string]any{"name": "prot-snap"})
	if st != 201 {
		t.Fatalf("snapshot: %d %v", st, snap)
	}
	sid := snap["id"].(string)
	if !e.srv.snapshotSourceProtected(sid) {
		t.Fatalf("source snapshot %s not protected", sid)
	}

	dlReq, _ := http.NewRequest("GET", e.api.URL+vmPath(protVM, "snapshots", sid, "download"), nil)
	dlReq.Header.Set("Authorization", "Bearer "+e.owner)
	dlResp, err := http.DefaultClient.Do(dlReq)
	if err != nil || dlResp.StatusCode != 200 {
		t.Fatalf("download protected snapshot: %v %v", dlResp, err)
	}
	archive, _ := io.ReadAll(dlResp.Body)
	dlResp.Body.Close()

	e.boatd.mu.Lock()
	gotProt := len(e.boatd.downloadProtected) > 0 && e.boatd.downloadProtected[len(e.boatd.downloadProtected)-1]
	e.boatd.mu.Unlock()
	if !gotProt {
		t.Fatal("export of protected snapshot did not pass protected=1 to boatd")
	}

	impReq, _ := http.NewRequest("POST", e.api.URL+"/v1/snapshots/import?name=prot-reimp", bytes.NewReader(archive))
	impReq.Header.Set("Authorization", "Bearer "+e.owner)
	impResp, err := http.DefaultClient.Do(impReq)
	if err != nil || impResp.StatusCode != 201 {
		t.Fatalf("import protected snapshot: %v %v", impResp, err)
	}
	var imported map[string]any
	json.NewDecoder(impResp.Body).Decode(&imported)
	impResp.Body.Close()
	impID := imported["id"].(string)

	if !e.srv.snapshotSourceProtected(impID) || !e.srv.snapshotSourceNoEnv(impID) {
		t.Fatalf("re-imported protected snapshot %s missing lineage: protected=%v noEnv=%v",
			impID, e.srv.snapshotSourceProtected(impID), e.srv.snapshotSourceNoEnv(impID))
	}
	// Durable on disk.
	back, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if !snapshotSourceProtectedIn(back.Snapshot(), impID) || !snapshotSourceNoEnvIn(back.Snapshot(), impID) {
		t.Fatalf("re-imported protected snapshot %s lineage not durable on disk", impID)
	}
	// Secrets and environment are still refused (sticky no_env).
	if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"snapshot": impID, "secrets": []string{"TOP_SECRET"}}); st != 400 || nativeErrCode(out) != "no_env_sticky" {
		t.Fatalf("secrets on trusted protected import = %d %v, want 400 no_env_sticky", st, out)
	}
	// Creating a VM from the trusted re-imported protected snapshot succeeds 201 and keeps no_env!
	stC, created := e.do(e.owner, "POST", "/v1/vms", map[string]any{"snapshot": impID})
	if stC != 201 {
		t.Fatalf("create from trusted protected import = %d %v, want 201", stC, created)
	}
	newVM := created["id"].(string)
	if !e.srv.noEnvRun(newVM) {
		t.Fatalf("created VM %s from trusted protected import lost sticky no_env", newVM)
	}
	if e.guestVar(newVM, "TOP_SECRET") != nil {
		t.Fatalf("created VM %s received team secret!", newVM)
	}

	// 2. Non-protected snapshot export must NOT pass protected=1.
	normalVM := e.createVM()
	stN, snapN := e.do(e.owner, "POST", vmPath(normalVM, "snapshots"), map[string]any{"name": "normal-snap"})
	if stN != 201 {
		t.Fatalf("normal snapshot: %d %v", stN, snapN)
	}
	dlReqN, _ := http.NewRequest("GET", e.api.URL+vmPath(normalVM, "snapshots", snapN["id"].(string), "download"), nil)
	dlReqN.Header.Set("Authorization", "Bearer "+e.owner)
	dlRespN, err := http.DefaultClient.Do(dlReqN)
	if err != nil || dlRespN.StatusCode != 200 {
		t.Fatalf("download normal snapshot: %v %v", dlRespN, err)
	}
	dlRespN.Body.Close()
	e.boatd.mu.Lock()
	gotNormalProt := len(e.boatd.downloadProtected) > 0 && e.boatd.downloadProtected[len(e.boatd.downloadProtected)-1]
	e.boatd.mu.Unlock()
	if gotNormalProt {
		t.Fatal("export of non-protected snapshot erroneously passed protected=1")
	}

	// 3. Untrusted import (no protected:true from boatd) NEVER records protected and still fails 409 unsafe_source.
	untrustedReq, _ := http.NewRequest("POST", e.api.URL+"/v1/snapshots/import?name=untrusted", strings.NewReader("ARCHIVE:no_env"))
	untrustedReq.Header.Set("Authorization", "Bearer "+e.owner)
	untrustedResp, err := http.DefaultClient.Do(untrustedReq)
	if err != nil || untrustedResp.StatusCode != 201 {
		t.Fatalf("untrusted import: %v %v", untrustedResp, err)
	}
	var untrusted map[string]any
	json.NewDecoder(untrustedResp.Body).Decode(&untrusted)
	untrustedResp.Body.Close()
	uID := untrusted["id"].(string)
	if e.srv.snapshotSourceProtected(uID) {
		t.Fatalf("untrusted import %s unexpectedly recorded protected lineage", uID)
	}
	if !e.srv.snapshotSourceNoEnv(uID) {
		t.Fatalf("untrusted import %s lost sticky no_env policy", uID)
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"snapshot": uID}); st != 409 || nativeErrCode(out) != "unsafe_source" {
		t.Fatalf("create from untrusted import = %d %v, want 409 unsafe_source", st, out)
	}
}
