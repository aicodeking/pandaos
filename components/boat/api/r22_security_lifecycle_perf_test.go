package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestR22AdoptedAndResumeOnBootSSHReattestation(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	team := e.teamID()

	// Adopted running VM present on boatd before syncJumpKeys / event stream reconnect,
	// plus a foreign adopted VM whose RestrictSelf/NoSelfAPI must stay enforced.
	e.boatd.mu.Lock()
	e.boatd.vms["vm-adopted"] = VM{
		"id": "vm-adopted", "name": "adopted", "size": "default", "image": "ubuntu-24.04",
		"team": team, "state": "running", "ip": "10.0.0.41", "ports": []any{},
		"ssh_boot_epoch": "epoch-1", "ssh_policy_ready": false,
	}
	e.boatd.vms["vm-foreign-adopted"] = VM{
		"id": "vm-foreign-adopted", "name": "foreign", "size": "default", "image": "ubuntu-24.04",
		"team": team, "state": "running", "ip": "10.0.0.42", "ports": []any{},
		"ssh_boot_epoch": "epoch-f1", "ssh_policy_ready": false,
	}
	e.boatd.vms["vm-booting"] = VM{
		"id": "vm-booting", "name": "booting", "size": "default", "image": "ubuntu-24.04",
		"team": team, "state": "stopped", "ip": "10.0.0.43", "ports": []any{},
		"ssh_boot_epoch": "epoch-b1", "ssh_policy_ready": false,
	}
	e.boatd.mu.Unlock()
	if err := e.srv.store.Update(func(st *State) error {
		st.ForeignVMs["vm-foreign-adopted"] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// 1. syncJumpKeysLocked re-attests running adopted VMs and creates VMTokens.
	if err := e.srv.syncJumpKeysErr(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.boatd.mu.Lock()
	readyAdopted := e.boatd.vms["vm-adopted"]["ssh_policy_ready"] == true
	readyForeign := e.boatd.vms["vm-foreign-adopted"]["ssh_policy_ready"] == true
	e.boatd.mu.Unlock()
	if !readyAdopted || !readyForeign {
		t.Fatalf("adopted running VMs not attested on syncJumpKeys: adopted=%v foreign=%v", readyAdopted, readyForeign)
	}
	st := e.srv.store.Snapshot()
	if tok := st.VMTokens["vm-foreign-adopted"]; tok == nil || !tok.RestrictSelf || !tok.NoSelfAPI {
		t.Fatalf("foreign adopted VM token missing restrictions: %+v", tok)
	}

	// 2. Simulate boatd restart resetting ssh_policy_ready=false with a new ssh_boot_epoch,
	// then reconnect via s.hooks.consume.
	e.boatd.mu.Lock()
	e.boatd.vms["vm-adopted"]["ssh_policy_ready"] = false
	e.boatd.vms["vm-adopted"]["ssh_boot_epoch"] = "epoch-2"
	e.boatd.mu.Unlock()
	cctx, cancel := context.WithCancel(context.Background())
	go e.srv.hooks.consume(cctx)
	deadline := time.Now().Add(2 * time.Second)
	readyReconnect := false
	for time.Now().Before(deadline) {
		e.boatd.mu.Lock()
		readyReconnect = e.boatd.vms["vm-adopted"]["ssh_policy_ready"] == true
		e.boatd.mu.Unlock()
		if readyReconnect {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if !readyReconnect {
		t.Fatal("adopted VM not re-attested on boatd event-stream reconnect")
	}

	// 3. ResumeOnBoot VM finishes booting and emits vm.state == "running".
	e.boatd.mu.Lock()
	e.boatd.vms["vm-booting"]["state"] = "running"
	e.boatd.vms["vm-booting"]["ssh_boot_epoch"] = "epoch-b2"
	e.boatd.vms["vm-booting"]["ssh_policy_ready"] = false
	e.boatd.mu.Unlock()
	e.srv.hooks.handleRaw(context.Background(), "vm.state", fmt.Sprintf(`{"type":"vm.state","team":%q,"vm_id":"vm-booting","state":"running"}`, team))
	deadline = time.Now().Add(2 * time.Second)
	readyBooted := false
	for time.Now().Before(deadline) {
		e.boatd.mu.Lock()
		readyBooted = e.boatd.vms["vm-booting"]["ssh_policy_ready"] == true
		e.boatd.mu.Unlock()
		if readyBooted && e.srv.store.Snapshot().VMTokens["vm-booting"] != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !readyBooted || e.srv.store.Snapshot().VMTokens["vm-booting"] == nil {
		t.Fatal("ResumeOnBoot VM not attested upon vm.state=running")
	}
}

func TestR22EnvBuildProvisionsSSHPolicyBeforeSetupScript(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	team := e.teamID()

	var policyReadyDuringSetup atomic.Bool
	var tokenRestrictDuringSetup atomic.Bool
	var failSSHPolicy atomic.Bool
	origTransport := e.srv.boatd.hc.Transport
	if origTransport == nil {
		origTransport = http.DefaultTransport
	}
	e.srv.boatd.hc.Transport = r20SSHTransport(func(r *http.Request) (*http.Response, error) {
		if failSSHPolicy.Load() && r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/ssh-policy") {
			return nil, errors.New("guest helper timeout")
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/exec") {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			vmID := parts[2]
			e.boatd.mu.Lock()
			ready := e.boatd.vms[vmID]["ssh_policy_ready"] == true
			e.boatd.mu.Unlock()
			policyReadyDuringSetup.Store(ready)
			if tok := e.srv.store.Snapshot().VMTokens[vmID]; tok != nil && tok.RestrictSelf && tok.NoSelfAPI && !tok.Retired {
				tokenRestrictDuringSetup.Store(true)
			}
		}
		return origTransport.RoundTrip(r)
	})

	vmID, snapID, err := e.srv.envBuildSteps(context.Background(), Environment{
		ID: "env_1", Name: "node", Version: 1, Image: "ubuntu-24.04", Size: "small",
		SetupScript: "echo installing-deps",
	}, team)
	if err != nil || vmID == "" || snapID == "" {
		t.Fatalf("envBuildSteps: vm=%q snap=%q err=%v", vmID, snapID, err)
	}
	if !policyReadyDuringSetup.Load() || !tokenRestrictDuringSetup.Load() {
		t.Fatalf("builder VM was not unquarantined with restricted token before SetupScript: ready=%v token=%v",
			policyReadyDuringSetup.Load(), tokenRestrictDuringSetup.Load())
	}
	if tok := e.srv.store.Snapshot().VMTokens[vmID]; tok == nil || !tok.Retired {
		t.Fatalf("builder VM token was not retired on exit: %+v", tok)
	}

	// Failure in prepareSSHPolicy fails the build cleanly with "ssh_policy: ...".
	failSSHPolicy.Store(true)
	_, _, err = e.srv.envBuildSteps(context.Background(), Environment{
		ID: "env_2", Name: "broken", Version: 1, Image: "ubuntu-24.04", Size: "small",
		SetupScript: "echo never-runs",
	}, team)
	if err == nil || !strings.Contains(err.Error(), "ssh_policy:") {
		t.Fatalf("expected ssh_policy build failure, got %v", err)
	}
}

func TestR22KeyGenRotationRevokesAllDerivedGrantsAndCapsSnapshotDownload(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	e.srv.cfg.VMDomain = "vm.example"
	e.srv.cfg.PublicURL = e.api.URL
	e.srv.cfg.PortURLTTL = time.Hour

	vm := e.createVM()
	e.ok("PUT", "/api/v1/sandboxes/"+vm+"/files", map[string]any{"path": "secret.txt", "content": "top-secret"}, 200)
	if st, out := e.do(e.owner, "POST", vmPath(vm, "ports"), map[string]any{"port": 8080}); st != 201 {
		t.Fatalf("add port: %d %v", st, out)
	}
	stCode, snapOut := e.do(e.owner, "POST", vmPath(vm, "snapshots"), map[string]any{"name": "base"})
	if stCode != 201 {
		t.Fatalf("snapshot: %d %v", stCode, snapOut)
	}
	sid := snapOut["id"].(string)

	// Mint an admin key (which has admin, vms:read, vms:write, exec).
	keyTok := e.newKey("admin", "vms:read", "vms:write", "exec")
	keyID, _, _ := ParseToken(keyTok)

	routeGenOf := func() uint64 {
		_, rg := e.do("", "GET", "/internal/route-gens", nil)
		gens, _ := rg["gens"].(map[string]any)
		if f, ok := gens[vm].(float64); ok {
			return uint64(f)
		}
		return 0
	}

	mintAll := func(tok string) (dlURL, upURL string, portGen uint64, shareCode string) {
		t.Helper()
		s1, o1, _ := e.compat(tok, "POST", "/api/v1/sandboxes/"+vm+"/files/download-url", map[string]any{"path": "secret.txt", "ttlSeconds": 3600}, nil)
		if s1 != 200 {
			t.Fatalf("mint dl: %d %v", s1, o1)
		}
		s2, o2, _ := e.compat(tok, "POST", "/api/v1/sandboxes/"+vm+"/files/upload-url", map[string]any{"path": "up.txt", "ttlSeconds": 3600}, nil)
		if s2 != 200 {
			t.Fatalf("mint up: %d %v", s2, o2)
		}
		s3, o3 := e.do(tok, "GET", vmPath(vm, "ports", "8080", "url"), nil)
		if s3 != 200 || o3["url"] == nil {
			t.Fatalf("mint port url: %d %v", s3, o3)
		}
		s4, o4 := e.do(tok, "POST", vmPath(vm, "snapshots", sid, "shares"), map[string]any{"ttl_s": 3600})
		if s4 != 201 {
			t.Fatalf("mint share: %d %v", s4, o4)
		}
		e.grantSSH(tok, vm)
		return o1["url"].(string), o2["url"].(string), routeGenOf(), o4["code"].(string)
	}

	dl0, up0, portGen0, code0 := mintAll(keyTok)
	e.assertSSH(vm, r11Pub, 200)

	// Case A: overlap_s = 0 immediately invalidates file URLs, port route-gen, share code, and SSH grant.
	stRot, rotOut := e.do(e.owner, "POST", "/v1/keys/"+keyID+"/rotate", map[string]any{"overlap_s": 0})
	if stRot != 200 {
		t.Fatalf("rotate overlap=0: %d %v", stRot, rotOut)
	}
	keyTok1 := rotOut["token"].(string)

	if resp, err := http.Get(dl0); err != nil || resp.StatusCode != 404 {
		t.Fatalf("dl0 after overlap=0 rotate: %v %v", resp, err)
	}
	upReq, _ := http.NewRequest("PUT", up0, strings.NewReader("nope"))
	if resp, err := http.DefaultClient.Do(upReq); err != nil || resp.StatusCode != 404 {
		t.Fatalf("up0 after overlap=0 rotate: %v %v", resp, err)
	}
	if got := routeGenOf(); got <= portGen0 {
		t.Fatalf("route-gen did not advance on overlap=0 rotate: before=%d after=%d", portGen0, got)
	}
	if e.srv.shareByCode(code0) != nil {
		t.Fatal("share code remained valid after overlap=0 rotate")
	}
	e.assertSSH(vm, r11Pub, 204)
	if jCode, _ := e.jumpSSH(r11Pub); jCode != 204 {
		t.Fatalf("jump SSH remained valid after overlap=0 rotate: %d", jCode)
	}

	// Case B: overlap_s = 60 keeps Gen 1 grants valid during overlap, then automatically
	// invalidates all 4 when PrevUntil expires without another key rotation!
	dl1, up1, portGen1, code1 := mintAll(keyTok1)
	stRot2, rotOut2 := e.do(e.owner, "POST", "/v1/keys/"+keyID+"/rotate", map[string]any{"overlap_s": 60})
	if stRot2 != 200 {
		t.Fatalf("rotate overlap=60: %d %v", stRot2, rotOut2)
	}
	keyTok2 := rotOut2["token"].(string)
	if resp, err := http.Get(dl1); err != nil || resp.StatusCode != 200 {
		t.Fatalf("dl1 during overlap should stay 200: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
	if e.srv.shareByCode(code1) == nil {
		t.Fatal("code1 should stay valid during overlap")
	}
	e.assertSSH(vm, r11Pub, 200)

	// Advance PrevUntil into the past and verify automatic rejection across all 4!
	if err := e.srv.store.Update(func(st *State) error {
		past := time.Now().Add(-time.Second)
		st.Keys[keyID].PrevUntil = &past
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if resp, err := http.Get(dl1); err != nil || resp.StatusCode != 404 {
		t.Fatalf("dl1 after PrevUntil expiry: %v %v", resp, err)
	}
	upReq1, _ := http.NewRequest("PUT", up1, strings.NewReader("nope"))
	if resp, err := http.DefaultClient.Do(upReq1); err != nil || resp.StatusCode != 404 {
		t.Fatalf("up1 after PrevUntil expiry: %v %v", resp, err)
	}
	if got := routeGenOf(); got <= portGen1 {
		t.Fatalf("route-gen did not advance after PrevUntil expiry: before=%d after=%d", portGen1, got)
	}
	if e.srv.shareByCode(code1) != nil {
		t.Fatal("code1 remained valid after PrevUntil expiry")
	}
	e.assertSSH(vm, r11Pub, 204)

	// Case C: compat rotate endpoint (/api/v1/api-keys/{id}/rotate) also bumps Gen and revokes immediately.
	dl2, _, _, code2 := mintAll(keyTok2)
	cSt, _, _ := e.compat(e.owner, "POST", "/api/v1/api-keys/"+keyID+"/rotate", nil, nil)
	if cSt != 200 {
		t.Fatalf("compat rotate: %d", cSt)
	}
	if resp, err := http.Get(dl2); err != nil || resp.StatusCode != 404 {
		t.Fatalf("dl2 after compat rotate: %v %v", resp, err)
	}
	if e.srv.shareByCode(code2) != nil {
		t.Fatal("code2 remained valid after compat rotate")
	}

	// Case D: compatSnapshotDownload caps signed chunk URL expiry to key.Expires and revokes on key rotation.
	stShort, shortOut := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "short", "scopes": []string{"exec", "vms:read"}, "expires_in_s": 45})
	if stShort != 201 {
		t.Fatalf("short key: %d %v", stShort, shortOut)
	}
	shortTok := shortOut["token"].(string)
	shortID := shortOut["id"].(string)
	dlSt, dlOut, _ := e.compat(shortTok, "GET", "/api/v1/snapshots/"+sid+"/download", nil, nil)
	if dlSt != 200 {
		t.Fatalf("compat snapshot download: %d %v", dlSt, dlOut)
	}
	if ttl := dlOut["expiresInSeconds"].(float64); ttl > 46 || ttl < 1 {
		t.Fatalf("compatSnapshotDownload did not cap TTL to key expiry: %v", ttl)
	}
	chunkURL := dlOut["chunks"].([]any)[0].(map[string]any)["signedUrl"].(string)
	if resp, err := http.Get(chunkURL); err != nil || resp.StatusCode != 200 {
		t.Fatalf("chunkURL before rotate: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
	e.do(e.owner, "POST", "/v1/keys/"+shortID+"/rotate", map[string]any{"overlap_s": 0})
	if resp, err := http.Get(chunkURL); err != nil || resp.StatusCode != 404 {
		t.Fatalf("chunkURL after rotate: %v %v", resp, err)
	}

	// Case E: shareByCode & importShare require "admin" scope on the minting user/key.
	adminTok, adminUID := e.addMember("admin")
	sShare, shareOut := e.do(adminTok, "POST", vmPath(vm, "snapshots", sid, "shares"), map[string]any{"ttl_s": 3600})
	if sShare != 201 {
		t.Fatalf("admin member share: %d %v", sShare, shareOut)
	}
	memberCode := shareOut["code"].(string)
	if e.srv.shareByCode(memberCode) == nil {
		t.Fatal("admin share should be valid before demotion")
	}
	// Demote user to "member" (loses "admin" scope).
	if err := e.srv.store.Update(func(st *State) error {
		tm := st.Teams[e.teamID()]
		for i := range tm.Members {
			if tm.Members[i].UserID == adminUID {
				tm.Members[i].Role = "member"
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if e.srv.shareByCode(memberCode) != nil {
		t.Fatal("shareByCode accepted share after minting user lost admin role")
	}
	_, otherTok := e.newTeam("importer")
	if stImp, _ := e.do(otherTok, "POST", "/v1/snapshots/import-share", map[string]any{"code": memberCode, "name": "imp"}); stImp != 404 {
		t.Fatalf("importShare accepted demoted share: %d", stImp)
	}
}

func TestR22SelfForkForbiddenAndForeignVMSelfRestrictions(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	e.srv.cfg.VMAPIURL = "https://boat.example"
	e.srv.cfg.VMDomain = "vm.example"

	// Enable both PublicPorts and SSH on the team's SelfToken policy.
	if st, out := e.do(e.owner, "PUT", "/v1/teams/"+e.teamID()+"/self-token", map[string]any{"public_ports": true, "ssh": true}); st != 200 {
		t.Fatalf("self-token policy: %d %v", st, out)
	}

	vm := e.createVM()
	bx := e.srv.compatIDs(e.teamID(), []string{vm})[vm]
	selfTok := e.selfTokenOf(vm)

	// 1. Recursive fork via boatself_ is 403 on both native and compat endpoints.
	if st, out := e.do(selfTok, "POST", vmPath(vm, "fork"), map[string]any{"name": "child"}); st != 403 {
		t.Fatalf("native self-token fork=%d %v, want 403", st, out)
	}
	if st, out, _ := e.compat(selfTok, "POST", "/api/v1/sandboxes/"+bx+"/fork", map[string]any{"name": "child"}, nil); st != 403 {
		t.Fatalf("compat self-token fork=%d %v, want 403", st, out)
	}

	// 2. Import a foreign snapshot -> boot foreign VM -> BOAT_API_KEY omitted by default!
	impReq, _ := http.NewRequest("POST", e.api.URL+"/v1/snapshots/import?name=foreign-snap", strings.NewReader("ARCHIVE:foreign-snap"))
	impReq.Header.Set("Authorization", "Bearer "+e.owner)
	impReq.Header.Set("Content-Type", "application/octet-stream")
	impResp, err := http.DefaultClient.Do(impReq)
	if err != nil {
		t.Fatal(err)
	}
	var imported map[string]any
	json.NewDecoder(impResp.Body).Decode(&imported)
	impResp.Body.Close()
	if impResp.StatusCode != 201 {
		t.Fatalf("import snapshot: %d %v", impResp.StatusCode, imported)
	}
	foreignSnapID := imported["id"].(string)

	stF1, f1Out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "foreign-default", "snapshot": foreignSnapID})
	if stF1 != 201 {
		t.Fatalf("create foreign default: %d %v", stF1, f1Out)
	}
	fvmDefault := f1Out["id"].(string)
	e.boatd.mu.Lock()
	defEnv, _ := e.boatd.vms[fvmDefault]["env"].(map[string]any)
	e.boatd.mu.Unlock()
	if defEnv["BOAT_API_KEY"] != nil && defEnv["BOAT_API_KEY"] != "" {
		t.Fatalf("foreign snapshot VM injected BOAT_API_KEY by default: %v", defEnv)
	}

	// Compat foreign boot also omits BOAT_API_KEY by default.
	cF1St, cF1Out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "foreign-compat", "from": foreignSnapID}, nil)
	if cF1St != 202 {
		t.Fatalf("compat create foreign: %d %v", cF1St, cF1Out)
	}
	cFVM := e.nativeOf(cF1Out["sandbox"].(map[string]any)["id"].(string))
	e.boatd.mu.Lock()
	cDefEnv, _ := e.boatd.vms[cFVM]["env"].(map[string]any)
	e.boatd.mu.Unlock()
	if cDefEnv["BOAT_API_KEY"] != nil && cDefEnv["BOAT_API_KEY"] != "" {
		t.Fatalf("compat foreign snapshot VM injected BOAT_API_KEY by default: %v", cDefEnv)
	}

	// 3. Explicit self_api: true injects BOAT_API_KEY on foreign VM, BUT PublicPorts & SSH remain 403!
	stF2, f2Out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "foreign-optin", "snapshot": foreignSnapID, "self_api": true})
	if stF2 != 201 {
		t.Fatalf("create foreign self_api=true: %d %v", stF2, f2Out)
	}
	fvmOptIn := f2Out["id"].(string)
	e.boatd.mu.Lock()
	optEnv, _ := e.boatd.vms[fvmOptIn]["env"].(map[string]any)
	e.boatd.mu.Unlock()
	foreignSelfTok, _ := optEnv["BOAT_API_KEY"].(string)
	if !strings.HasPrefix(foreignSelfTok, "boatself_") {
		t.Fatalf("expected BOAT_API_KEY when self_api=true, got %v", optEnv)
	}
	if st, out := e.do(foreignSelfTok, "POST", vmPath(fvmOptIn, "ports"), map[string]any{"port": 8080, "public": true}); st != 403 {
		t.Fatalf("foreign VM self-token opened public port: %d %v", st, out)
	}
	if st, out := e.do(foreignSelfTok, "POST", vmPath(fvmOptIn, "ssh"), map[string]any{"public_key": r11Pub}); st != 403 {
		t.Fatalf("foreign VM self-token added SSH key: %d %v", st, out)
	}
}

type stalledReader struct {
	sent bool
	wait time.Duration
}

func (s *stalledReader) Read(p []byte) (int, error) {
	if !s.sent {
		s.sent = true
		return copy(p, "chunk1"), nil
	}
	time.Sleep(s.wait)
	return 0, io.EOF
}

func TestR22UploadDeadlinesPreCommitRevalidationAndDeleteTeamGuard(t *testing.T) {
	e := setup(t)
	vm := e.createVM()

	// 1. Stalled streaming upload hits UploadIdleTimeout -> 408 request_timeout.
	e.srv.cfg.UploadIdleTimeout = 40 * time.Millisecond
	req, _ := http.NewRequest("PUT", e.api.URL+vmPath(vm, "files")+"?path=/tmp/stall.bin", &stalledReader{wait: 200 * time.Millisecond})
	req.Header.Set("Authorization", "Bearer "+e.owner)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("stalled upload status=%d, want 408", resp.StatusCode)
	}
	e.srv.cfg.UploadIdleTimeout = 30 * time.Second

	// 2. Mid-upload key revocation during importSnapshot discards the imported boatd snapshot!
	impKey := e.newKey("vms:write", "vms:read")
	impKeyID, _, _ := ParseToken(impKey)
	pr, pw := io.Pipe()
	doneImp := make(chan int, 1)
	go func() {
		ireq, _ := http.NewRequest("POST", e.api.URL+"/v1/snapshots/import?name=revoked-mid-upload", pr)
		ireq.Header.Set("Authorization", "Bearer "+impKey)
		ireq.Header.Set("Content-Type", "application/octet-stream")
		iresp, ierr := http.DefaultClient.Do(ireq)
		if ierr != nil {
			doneImp <- 0
			return
		}
		iresp.Body.Close()
		doneImp <- iresp.StatusCode
	}()
	_, _ = pw.Write([]byte("ARCHIVE:part1"))
	e.do(e.owner, "DELETE", "/v1/keys/"+impKeyID, nil)
	_ = pw.Close()
	if got := <-doneImp; got != 401 {
		t.Fatalf("revoked mid-import status=%d, want 401", got)
	}
	e.boatd.mu.Lock()
	snapCount := len(e.boatd.snaps)
	e.boatd.mu.Unlock()
	if snapCount != 0 {
		t.Fatalf("revoked mid-import left %d snapshots on boatd", snapCount)
	}

	// 3. deleteTeam revalidates authority BEFORE deleting boatd snapshots.
	doomedTeam, doomedOwner := e.newTeam("doomed")
	stVM, dvm := e.do(doomedOwner, "POST", "/v1/vms", map[string]any{"name": "keep"})
	if stVM != 201 {
		t.Fatalf("doomed vm: %d %v", stVM, dvm)
	}
	dvmID := dvm["id"].(string)
	if stSn, snOut := e.do(doomedOwner, "POST", vmPath(dvmID, "snapshots"), map[string]any{"name": "precious"}); stSn != 201 {
		t.Fatalf("doomed snap: %d %v", stSn, snOut)
	}
	// Stop the VM so deleteTeam passes the live-VM check, then construct a stale principal whose key was rotated.
	e.do(doomedOwner, "DELETE", vmPath(dvmID), nil)
	// Put a standalone imported snapshot on doomedTeam so deleteTeam reaches deleteSnapshotsOf!
	impReq2, _ := http.NewRequest("POST", e.api.URL+"/v1/snapshots/import?name=doomed-snap", strings.NewReader("ARCHIVE:doomed"))
	impReq2.Header.Set("Authorization", "Bearer "+doomedOwner)
	impReq2.Header.Set("Content-Type", "application/octet-stream")
	impResp2, _ := http.DefaultClient.Do(impReq2)
	var impSnap2 map[string]any
	json.NewDecoder(impResp2.Body).Decode(&impSnap2)
	impResp2.Body.Close()
	doomedSnapID := impSnap2["id"].(string)

	opKey := e.newKey("admin")
	opKeyID, _, _ := ParseToken(opKey)
	staleP := e.srv.authenticate("Bearer " + opKey)
	e.do(e.owner, "POST", "/v1/keys/"+opKeyID+"/rotate", map[string]any{"overlap_s": 0})

	w := httptest.NewRecorder()
	delReq := httptest.NewRequest("DELETE", "/v1/teams/"+doomedTeam, nil)
	delReq.SetPathValue("id", doomedTeam)
	delReq = delReq.WithContext(context.WithValue(delReq.Context(), ctxKey{}, &reqInfo{id: "r22-del-team"}))
	e.srv.deleteTeam(w, delReq, staleP)
	if w.Code != 401 {
		t.Fatalf("deleteTeam with rotated principal=%d, want 401", w.Code)
	}
	e.boatd.mu.Lock()
	stillExists := e.boatd.snaps[doomedSnapID] != nil
	e.boatd.mu.Unlock()
	if !stillExists {
		t.Fatal("deleteTeam deleted boatd snapshots despite revoked principal!")
	}
}

func TestR22PerTeamStoreCapsAndBoundedOTelGoroutines(t *testing.T) {
	e := setup(t)
	teamA := e.teamID()
	teamB, tokB := e.newTeam("team-b")
	_ = tokB

	// 1. Idempotency, StopOps, and DeletionOps per-team caps (1024): pre-seed 1020 entries for teamA
	// and verify live operations past 1024 evict oldest teamA entries without touching teamB!
	slotB := teamB + "\x00keep-b"
	if replay, code := e.srv.idem.begin(slotB, "hash-b"); code != "" || replay != nil {
		t.Fatalf("teamB idem begin: %q %v", code, replay)
	}
	e.srv.idem.finish(slotB, 202, "application/json", []byte(`{"ok":true}`))
	opB := e.srv.stops.begin(teamB, "vm-b")
	e.srv.stops.complete("vm-b", opB)
	baseTime := time.Now().UTC().Add(-2 * time.Minute)
	e.srv.deletions.add(teamB, map[string]any{"id": "bdop_b0", "requestedAt": baseTime})

	const preSeed = 1020
	firstStopA := "stop_seed_0000"
	firstDelA := "bdop_a0"
	if err := e.srv.store.Update(func(st *State) error {
		for i := 0; i < preSeed; i++ {
			at := baseTime.Add(time.Duration(i) * time.Millisecond)
			st.Idempotency[fmt.Sprintf("%s\x00flood-%04d", teamA, i)] = &IdemRecord{
				Hash: "hash-a", Status: 202, CType: "application/json", Body: []byte(`{"ok":true}`), At: at,
			}
			stopID := fmt.Sprintf("stop_seed_%04d", i)
			vmID := fmt.Sprintf("vm-a-%d", i)
			st.StopOps[stopID] = map[string]any{
				"id": stopID, "status": "pending", "requestedAt": at, "_team": teamA, "_vm": vmID,
			}
			st.StopLast[vmID] = stopID
			delID := fmt.Sprintf("bdop_a%d", i)
			st.DeletionOps[delID] = map[string]any{
				"id": delID, "requestedAt": at, "_team": teamA,
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for i := preSeed; i < maxIdemPerTeam+10; i++ {
		slotA := fmt.Sprintf("%s\x00flood-%04d", teamA, i)
		if _, code := e.srv.idem.begin(slotA, "hash-a"); code != "" {
			t.Fatalf("teamA idem %d: %s", i, code)
		}
		e.srv.idem.finish(slotA, 202, "application/json", []byte(`{"ok":true}`))
	}
	for i := preSeed; i < maxStopOpsPerTeam+10; i++ {
		e.srv.stops.begin(teamA, fmt.Sprintf("vm-a-%d", i))
	}
	for i := preSeed; i < maxDeletionOpsPerTeam+10; i++ {
		e.srv.deletions.add(teamA, map[string]any{"id": fmt.Sprintf("bdop_a%d", i), "requestedAt": baseTime.Add(time.Duration(i) * time.Millisecond)})
	}

	if replay, code := e.srv.idem.begin(slotB, "hash-b"); code != "" || replay == nil {
		t.Fatalf("teamA flood evicted teamB idempotency record: code=%q replay=%v", code, replay)
	}
	st := e.srv.store.Snapshot()
	if st.Idempotency[teamA+"\x00flood-0000"] != nil {
		t.Fatal("oldest teamA idempotency entry was not evicted")
	}
	if st.StopOps[opB["id"].(string)] == nil {
		t.Fatal("teamA StopOps flood evicted teamB stop operation")
	}
	if st.StopOps[firstStopA] != nil {
		t.Fatal("oldest teamA StopOps entry was not evicted")
	}
	if st.DeletionOps["bdop_b0"] == nil {
		t.Fatal("teamA DeletionOps flood evicted teamB deletion operation")
	}
	if st.DeletionOps[firstDelA] != nil {
		t.Fatal("oldest teamA DeletionOps entry was not evicted")
	}

	// 4. Bounded per-team OTLP export goroutines (<= 8 concurrent per team).
	const otelTeamCap = 8
	var active, peak atomic.Int32
	unblock := make(chan struct{})
	otelSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := active.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		<-unblock
		active.Add(-1)
		w.WriteHeader(200)
	}))
	defer otelSrv.Close()
	e.srv.hooks.client = otelSrv.Client()
	if code, out := e.do(e.owner, "PUT", "/v1/telemetry", map[string]any{"endpoint": otelSrv.URL}); code != 200 {
		t.Fatalf("put telemetry: %d %v", code, out)
	}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e.srv.emitVMEvent(context.Background(), "vm.state", teamA, fmt.Sprintf("vm-%d", i), "running")
		}(i)
	}
	wg.Wait()
	deadline := time.Now().Add(500 * time.Millisecond)
	for active.Load() < otelTeamCap && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(25 * time.Millisecond)
	gotPeak := peak.Load()
	close(unblock)
	if gotPeak > otelTeamCap || gotPeak == 0 {
		t.Fatalf("OTLP concurrent goroutines peak=%d, want 1..%d", gotPeak, otelTeamCap)
	}
}

func TestR22ExecTTYCompatRootPathAndSnapshotsVisibilityFilter(t *testing.T) {
	e := setup(t)
	e.srv.cfg.SSHUser = "root"
	vm := e.createVM()
	bx := e.srv.compatIDs(e.teamID(), []string{vm})[vm]

	// 1. TTY: true passes through to boatd exec request.
	st, raw := e.raw(e.owner, "POST", vmPath(vm, "exec"), map[string]any{"cmd": []string{"echo", "tty-ok"}, "tty": true})
	if st != 200 || !bytes.Contains(raw, []byte(`"exit":0`)) {
		t.Fatalf("exec tty: %d %s", st, raw)
	}
	e.boatd.mu.Lock()
	lastExec := e.boatd.execs[len(e.boatd.execs)-1]
	e.boatd.mu.Unlock()
	if lastExec["tty"] != true {
		t.Fatalf("exec tty was not forwarded to boatd: %v", lastExec)
	}

	// 2. compatPath & homeDir resolve to /root (never /home/root) when SSHUser == "root".
	for in, want := range map[string]string{
		"":                 "/root",
		"~":                "/root",
		"~/work/a.txt":     "/root/work/a.txt",
		"rel/b.txt":        "/root/rel/b.txt",
		"/home/user":       "/root",
		"/home/user/c.txt": "/root/c.txt",
	} {
		if got := e.srv.compatPath(in); got != want {
			t.Errorf("compatPath(%q) with root = %q, want %q", in, got, want)
		}
	}
	if stW, wOut, _ := e.compat(e.owner, "PUT", "/api/v1/sandboxes/"+bx+"/files", map[string]any{"path": "hello.txt", "content": "root-home"}, nil); stW != 200 || wOut["path"] != "/root/hello.txt" {
		t.Fatalf("compat file write for root user: %d %v", stW, wOut)
	}

	// 3. GET /v1/snapshots?visibility=shared filters to shared snapshots while omitting visibility returns all.
	_, s1 := e.do(e.owner, "POST", vmPath(vm, "snapshots"), map[string]any{"name": "priv-snap"})
	_, s2 := e.do(e.owner, "POST", vmPath(vm, "snapshots"), map[string]any{"name": "shared-snap"})
	sidShared := s2["id"].(string)
	if stSh, shOut := e.do(e.owner, "POST", vmPath(vm, "snapshots", sidShared, "shares"), map[string]any{"ttl_s": 3600}); stSh != 201 {
		t.Fatalf("share snap: %d %v", stSh, shOut)
	}
	stAll, allOut := e.do(e.owner, "GET", "/v1/snapshots", nil)
	if stAll != 200 || len(allOut["snapshots"].([]any)) != 2 {
		t.Fatalf("GET /v1/snapshots all: %d %v (%v)", stAll, allOut, s1)
	}
	stShared, sharedOut := e.do(e.owner, "GET", "/v1/snapshots?visibility=shared", nil)
	snapsShared, _ := sharedOut["snapshots"].([]any)
	if stShared != 200 || len(snapsShared) != 1 || snapsShared[0].(map[string]any)["id"] != sidShared {
		t.Fatalf("GET /v1/snapshots?visibility=shared: %d %v", stShared, sharedOut)
	}
	if stBad, _ := e.do(e.owner, "GET", "/v1/snapshots?visibility=bogus", nil); stBad != 400 {
		t.Fatalf("GET /v1/snapshots?visibility=bogus=%d, want 400", stBad)
	}
}
