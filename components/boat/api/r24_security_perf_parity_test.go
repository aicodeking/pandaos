package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestR24CrossTeamSnapShareAuthorityAndExpiry(t *testing.T) {
	setupPair := func(t *testing.T, expiresInS int) (*env, string, string, string, string, string, string) {
		t.Helper()
		e := setup(t)
		recipientID, recipientKey := e.newTeam("beta")
		if st, out := e.do(recipientKey, "PUT", "/v1/teams/"+recipientID+"/share-allowlist", map[string]any{"teams": []string{e.teamID()}}); st != 200 {
			t.Fatalf("allowlist sender: %d %v", st, out)
		}

		// Add a second admin member on sender team so we can test admin key deletion, rotation,
		// expiry, user removal, and demotion from admin -> member.
		carolInitKey, carolUserID := e.addMember("admin")

		adminKey := carolInitKey
		if expiresInS > 0 {
			st, kOut := e.do(carolInitKey, "POST", "/v1/keys", map[string]any{"name": "expiring-admin", "expires_in_s": expiresInS})
			if st != 201 {
				t.Fatalf("create expiring admin key: %d %v", st, kOut)
			}
			adminKey = kOut["token"].(string)
		}

		vmID := e.createVM()
		sid := e.snapshotOf(vmID)
		return e, vmID, sid, adminKey, carolUserID, recipientID, recipientKey
	}

	assertShareGone := func(t *testing.T, e *env, senderTok, recipientKey, shareID string) {
		t.Helper()
		st, shared := e.do(recipientKey, "GET", "/v1/shared-snapshots", nil)
		if st != 200 {
			t.Fatalf("shared-snapshots: %d %v", st, shared)
		}
		if got := list(shared, "shares"); len(got) != 0 {
			t.Fatalf("revoked cross-team grant still visible in GET /v1/shared-snapshots: %v", got)
		}
		st, imp := e.do(recipientKey, "POST", "/v1/snapshots/import-share", map[string]any{"share_id": shareID, "name": "copied"})
		if st != 404 {
			t.Fatalf("POST /v1/snapshots/import-share after revocation: status=%d body=%v, want 404", st, imp)
		}
		st, vis := e.do(senderTok, "GET", "/v1/snapshots?visibility=shared", nil)
		if st != 200 {
			t.Fatalf("visibility=shared: %d %v", st, vis)
		}
		if got := list(vis, "snapshots"); len(got) != 0 {
			t.Fatalf("revoked cross-team grant still listed as active in visibility=shared: %v", got)
		}
	}

	for _, tc := range []string{"delete_key", "rotate_zero_overlap", "expire_key", "remove_user", "demote_to_member"} {
		t.Run(tc, func(t *testing.T) {
			expS := 0
			if tc == "expire_key" {
				expS = 120
			}
			e, vmID, sid, adminKey, carolUserID, recipientTeamID, recipientKey := setupPair(t, expS)
			// Request ttl_s=0 (or 3600 on expiring key) and verify key-expiry capping too.
			reqTTL := int64(0)
			if tc == "expire_key" {
				reqTTL = 3600
			}
			st, shOut := e.do(adminKey, "POST", vmPath(vmID, "snapshots", sid, "shares"), map[string]any{
				"team_id": recipientTeamID,
				"ttl_s":   reqTTL,
			})
			if st != 201 {
				t.Fatalf("createShare: %d %v", st, shOut)
			}
			shareID := shOut["id"].(string)
			keyID, _, _ := ParseToken(adminKey)

			stored := e.srv.store.Snapshot().SnapShares[shareID]
			ak := e.srv.store.Snapshot().Keys[keyID]
			if stored == nil || ak == nil || stored.KeyID != keyID || stored.MintedByKeyGen != ak.Gen {
				t.Fatalf("cross-team SnapShare missing KeyID/MintedByKeyGen: %+v (key=%+v)", stored, ak)
			}
			if tc == "expire_key" {
				if stored.Expires == nil || ak.Expires == nil || stored.Expires.After(*ak.Expires) {
					t.Fatalf("SnapShare.Expires (%v) was not capped by APIKey.Expires (%v)", stored.Expires, ak.Expires)
				}
			}

			// Sanity: before authority revocation, recipient sees the shared snapshot.
			if st, pre := e.do(recipientKey, "GET", "/v1/shared-snapshots", nil); st != 200 || len(list(pre, "shares")) != 1 {
				t.Fatalf("expected 1 visible shared snapshot before revocation: %d %v", st, pre)
			}

			switch tc {
			case "delete_key":
				if st, out := e.do(e.owner, "DELETE", "/v1/keys/"+keyID, nil); st != 200 {
					t.Fatalf("delete key: %d %v", st, out)
				}
			case "rotate_zero_overlap":
				if st, out := e.do(e.owner, "POST", "/v1/keys/"+keyID+"/rotate", map[string]any{"overlap_s": 0}); st != 200 {
					t.Fatalf("rotate key: %d %v", st, out)
				}
			case "expire_key":
				past := time.Now().UTC().Add(-time.Second)
				if err := e.srv.store.Update(func(st *State) error {
					st.Keys[keyID].Expires = &past
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			case "remove_user":
				if st, out := e.do(e.owner, "DELETE", "/v1/teams/"+e.teamID()+"/members/"+carolUserID, nil); st != 200 {
					t.Fatalf("remove member: %d %v", st, out)
				}
			case "demote_to_member":
				if err := e.srv.store.Update(func(st *State) error {
					tm := st.Teams[e.teamID()]
					for i := range tm.Members {
						if tm.Members[i].UserID == carolUserID {
							tm.Members[i].Role = "member"
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}

			assertShareGone(t, e, e.owner, recipientKey, shareID)
		})
	}

	// Also verify legacy/unowned sh.KeyID == "" cross-team grants cannot bypass grantFor.
	t.Run("empty_key_id_denied", func(t *testing.T) {
		e, vmID, sid, _, _, recipientTeamID, recipientKey := setupPair(t, 0)
		if err := e.srv.store.Update(func(st *State) error {
			st.SnapShares["unowned"] = &SnapShare{
				ID:         "unowned",
				TeamID:     e.teamID(),
				VMID:       vmID,
				SnapshotID: sid,
				Grantee:    recipientTeamID,
				Created:    time.Now().UTC(),
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if st, shared := e.do(recipientKey, "GET", "/v1/shared-snapshots", nil); st != 200 || len(list(shared, "shares")) != 0 {
			t.Fatalf("unowned cross-team SnapShare bypassed grantFor: %d %v", st, shared)
		}
		if st, _ := e.do(recipientKey, "POST", "/v1/snapshots/import-share", map[string]any{"share_id": "unowned"}); st != 404 {
			t.Fatalf("unowned cross-team SnapShare imported: %d", st)
		}
	})
}

func TestR24VMsWriteEscalationBlocked(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	writeOnly := e.newKey("vms:write")
	writeRead := e.newKey("vms:write", "vms:read")

	// 1. Explicit self_api: true without both vms:read and exec -> 403 before creating a VM.
	for _, tok := range []string{writeOnly, writeRead} {
		st, out := e.do(tok, "POST", "/v1/vms", map[string]any{"name": "esc-self", "self_api": true})
		if st != 403 || !strings.Contains(str(out["error"].(map[string]any), "message"), "self_api requires scopes vms:read and exec") {
			t.Fatalf("self_api:true with insufficient scopes: status=%d out=%v", st, out)
		}
	}
	if len(e.boatd.creates) != 0 {
		t.Fatalf("rejected self_api:true touched boatd: %d creates", len(e.boatd.creates))
	}

	// 2. Default self_api == nil with vms:write-only succeeds on both /v1/vms (201) and /api/v1/sandboxes (202)
	// without minting BOAT_API_KEY!
	st, vmOut := e.do(writeOnly, "POST", "/v1/vms", map[string]any{"name": "plain-write"})
	if st != 201 {
		t.Fatalf("plain vms:write createVM failed: %d %v", st, vmOut)
	}
	vm1 := vmOut["id"].(string)
	if tokRec := e.srv.store.Snapshot().VMTokens[vm1]; tokRec != nil && !tokRec.NoSelfAPI {
		t.Fatalf("vms:write createVM minted active self token: %+v", tokRec)
	}
	e.boatd.mu.Lock()
	vm1Env, _ := e.boatd.vms[vm1]["env"].(map[string]any)
	e.boatd.mu.Unlock()
	if vm1Env["BOAT_API_KEY"] != nil {
		t.Fatalf("vms:write createVM injected BOAT_API_KEY into guest env: %v", vm1Env)
	}
	forged := e.srv.selfToken(vm1, 1)
	if st, _ := e.do(forged, "GET", vmPath(vm1), nil); st != 401 {
		t.Fatalf("forged self token worked for vms:write-created VM: status=%d", st)
	}

	cst, cOut, _ := e.compat(writeOnly, "POST", "/api/v1/sandboxes", map[string]any{"name": "compat-write"}, nil)
	if cst != 202 {
		t.Fatalf("plain vms:write compatCreate failed: %d %v", cst, cOut)
	}
	targetBX := cOut["sandbox"].(map[string]any)["id"].(string)
	vm2 := e.nativeOf(targetBX)
	e.boatd.mu.Lock()
	vm2Env, _ := e.boatd.vms[vm2]["env"].(map[string]any)
	e.boatd.mu.Unlock()
	if vm2Env["BOAT_API_KEY"] != nil {
		t.Fatalf("vms:write compatCreate injected BOAT_API_KEY into guest env: %v", vm2Env)
	}

	// 3. Caller env overrides require scope exec across createVM, compatCreate, startVM, compatResume, forkVM, cloneVM, compatFork
	// before touching boatd or Store.
	targetVM := vm2
	e.ok("POST", "/api/v1/sandboxes/"+targetBX+"/stop", nil, 202)

	e.boatd.mu.Lock()
	createsBefore := len(e.boatd.creates)
	startsBefore := len(e.boatd.starts)
	forksBefore := len(e.boatd.forks)
	e.boatd.mu.Unlock()

	nativeEnvChecks := []struct {
		name, path string
	}{
		{"createVM", "/v1/vms"},
		{"startVM", vmPath(targetVM, "start")},
		{"forkVM", vmPath(targetVM, "fork")},
		{"cloneVM", vmPath(targetVM, "clone")},
	}
	for _, nc := range nativeEnvChecks {
		st, out := e.do(writeOnly, "POST", nc.path, map[string]any{"env": map[string]string{"LD_PRELOAD": "/tmp/evil.so"}})
		if st != 403 || !strings.Contains(str(out["error"].(map[string]any), "message"), "env requires scope exec") {
			t.Errorf("%s caller env with vms:write: status=%d out=%v", nc.name, st, out)
		}
	}
	compatEnvChecks := []struct {
		name, path string
	}{
		{"compatCreate", "/api/v1/sandboxes"},
		{"compatResume", "/api/v1/sandboxes/" + targetBX + "/resume"},
		{"compatFork", "/api/v1/sandboxes/" + targetBX + "/fork"},
	}
	for _, cc := range compatEnvChecks {
		hdr := map[string]string{"Idempotency-Key": "env-denied-" + cc.name}
		st, out, _ := e.compat(writeOnly, "POST", cc.path, map[string]any{"env": map[string]string{"PATH": "/tmp/evil"}}, hdr)
		if st != 403 || !strings.Contains(str(out, "message"), "env requires scope exec") {
			t.Errorf("%s caller env with vms:write: status=%d out=%v", cc.name, st, out)
		}
	}
	e.boatd.mu.Lock()
	if len(e.boatd.creates) != createsBefore || len(e.boatd.starts) != startsBefore || len(e.boatd.forks) != forksBefore {
		t.Fatalf("caller env denial touched boatd: creates %d->%d starts %d->%d forks %d->%d",
			createsBefore, len(e.boatd.creates), startsBefore, len(e.boatd.starts), forksBefore, len(e.boatd.forks))
	}
	e.boatd.mu.Unlock()

	// 4. Unbuilt Environment.SetupScript requires scope exec before allocating a VM on boatd,
	// whereas a built environment (from_build == true) succeeds for vms:write!
	st, envOut := e.do(e.owner, "POST", "/v1/environments", map[string]any{
		"name":         "unbuilt-script",
		"setup_script": "echo pwned > /tmp/pwn",
		"is_default":   true,
	})
	if st != 201 {
		t.Fatalf("create environment: %d %v", st, envOut)
	}
	envID := envOut["id"].(string)

	e.boatd.mu.Lock()
	createsBefore = len(e.boatd.creates)
	e.boatd.mu.Unlock()

	// Default unbuilt environment with SetupScript rejected on both /v1/vms and /api/v1/sandboxes!
	st, out := e.do(writeOnly, "POST", "/v1/vms", map[string]any{"name": "from-unbuilt-default"})
	if st != 403 || !strings.Contains(str(out["error"].(map[string]any), "message"), "environment setup_script requires scope exec") {
		t.Fatalf("unbuilt default setup_script on createVM: %d %v", st, out)
	}
	cst, cOut, _ = e.compat(writeOnly, "POST", "/api/v1/sandboxes", map[string]any{"name": "compat-unbuilt"}, nil)
	if cst != 403 || !strings.Contains(str(cOut, "message"), "environment setup_script requires scope exec") {
		t.Fatalf("unbuilt setup_script on compatCreate: %d %v", cst, cOut)
	}
	e.boatd.mu.Lock()
	if len(e.boatd.creates) != createsBefore {
		t.Fatalf("unbuilt setup_script allocated VM on boatd before rejecting: %d -> %d", createsBefore, len(e.boatd.creates))
	}
	e.boatd.mu.Unlock()

	// Build the environment; once built, setup_script does not run on create, so vms:write succeeds!
	if st, bOut := e.do(e.owner, "POST", "/v1/environments/"+envID+"/build", nil); st != 202 && st != 200 {
		t.Fatalf("build environment: %d %v", st, bOut)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ev := e.srv.store.Snapshot().Environments[envID]
		if ev != nil && ev.Build != nil && ev.Build.State == "ready" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, builtVM := e.do(writeOnly, "POST", "/v1/vms", map[string]any{"name": "from-built", "environment": "unbuilt-script"})
	if st != 201 || builtVM["from_build"] != true {
		t.Fatalf("built environment should succeed for vms:write: %d %v", st, builtVM)
	}
}

func TestR24IdempotencyBoundToAuthorityAndRouteState(t *testing.T) {
	createWithHostedPort := func(e *env, tok, idemKey string) (int, map[string]any) {
		st, out, _ := e.compat(tok, "POST", "/api/v1/sandboxes", map[string]any{
			"name":        "idem-box",
			"hostedPorts": []map[string]any{{"port": 8080}, {"port": 8081, "public": true}},
		}, map[string]string{"Idempotency-Key": idemKey})
		return st, out
	}

	portSig := func(t *testing.T, out map[string]any) string {
		t.Helper()
		hp, ok := out["hostedPorts"].([]any)
		if !ok || len(hp) == 0 {
			t.Fatalf("missing hostedPorts in response: %v", out)
		}
		rawURL, _ := hp[0].(map[string]any)["url"].(string)
		if rawURL == "" {
			t.Fatalf("missing url in hostedPorts[0]: %v", hp[0])
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatal(err)
		}
		return u.Query().Get("boat_sig")
	}

	for _, tc := range []string{
		"live_same_team_replay",
		"key_deleted",
		"key_rotated_zero_overlap",
		"key_expired",
		"sandbox_deleted",
		"sandbox_unshared",
		"port_tokens_revoked",
		"port_privacy_changed",
	} {
		t.Run(tc, func(t *testing.T) {
			e := setup(t)
			e.srv.cfg.VMDomain = "vm.boat.example"
			if len(e.srv.cfg.DesktopKey) == 0 {
				e.srv.cfg.DesktopKey = []byte("0123456789abcdef0123456789abcdef")
			}
			minterBody := map[string]any{"name": "idem-minter", "scopes": []string{"vms:read", "vms:write", "exec"}}
			if tc == "key_expired" {
				minterBody["expires_in_s"] = 120
			}
			st, kOut := e.do(e.owner, "POST", "/v1/keys", minterBody)
			if st != 201 {
				t.Fatalf("create minter key: %d %v", st, kOut)
			}
			minterTok := kOut["token"].(string)
			minterKeyID := kOut["id"].(string)

			idemKey := "idem-r24-" + tc
			st1, out1 := createWithHostedPort(e, minterTok, idemKey)
			if st1 != 202 {
				t.Fatalf("initial create: %d %v", st1, out1)
			}
			bx1 := out1["sandbox"].(map[string]any)["id"].(string)
			vm1 := e.nativeOf(bx1)
			sig1 := portSig(t, out1)

			if tc == "live_same_team_replay" {
				st2, out2 := createWithHostedPort(e, e.owner, idemKey)
				if st2 != 202 || out2["sandbox"].(map[string]any)["id"] != bx1 || portSig(t, out2) != sig1 {
					t.Fatalf("live same-team replay did not return cached response: %d %v", st2, out2)
				}
				return
			}

			switch tc {
			case "key_deleted":
				e.do(e.owner, "DELETE", "/v1/keys/"+minterKeyID, nil)
			case "key_rotated_zero_overlap":
				e.do(e.owner, "POST", "/v1/keys/"+minterKeyID+"/rotate", map[string]any{"overlap_s": 0})
			case "key_expired":
				past := time.Now().UTC().Add(-time.Second)
				if err := e.srv.store.Update(func(st *State) error {
					st.Keys[minterKeyID].Expires = &past
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			case "sandbox_deleted":
				if delSt, delOut, _ := e.compat(e.owner, "DELETE", "/api/v1/sandboxes/"+bx1, nil, map[string]string{"X-Ascii-Confirm-Delete": bx1}); delSt != 202 {
					t.Fatalf("delete sandbox: %d %v", delSt, delOut)
				}
			case "sandbox_unshared":
				if uSt, uOut, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx1+"/unshare", nil, nil); uSt != 501 || errCode(uOut) != "not_supported" {
					t.Fatalf("unshare sandbox: %d %v", uSt, uOut)
				}
				st2, out2 := createWithHostedPort(e, e.owner, idemKey)
				if st2 != 202 || out2["sandbox"].(map[string]any)["id"] != bx1 || portSig(t, out2) != sig1 {
					t.Fatalf("unsupported unshare invalidated replay: %d %v", st2, out2)
				}
				return
			case "port_tokens_revoked":
				e.do(e.owner, "POST", vmPath(vm1, "port-tokens", "revoke"), nil)
			case "port_privacy_changed":
				e.do(e.owner, "POST", vmPath(vm1, "ports"), map[string]any{"port": 8081, "auth": "private"})
			}

			st2, out2 := createWithHostedPort(e, e.owner, idemKey)
			if st2 != 202 {
				t.Fatalf("fresh execution after %s failed: %d %v", tc, st2, out2)
			}
			bx2 := out2["sandbox"].(map[string]any)["id"].(string)
			sig2 := portSig(t, out2)
			if bx2 == bx1 || sig2 == sig1 {
				t.Fatalf("stale Idempotency-Key entry replayed after %s: bx1=%s bx2=%s sig1=%s sig2=%s", tc, bx1, bx2, sig1, sig2)
			}
		})
	}
}

func TestR24StateCloneCoalescingPoolingAndIndexedJumpSSH(t *testing.T) {
	t.Run("exhaustive_state_clone", func(t *testing.T) {
		now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
		later := now.Add(time.Hour)
		exitCode := 7
		secSlice := []string{"S1", "S2"}
		nestedStr := "nested"
		nestedBool := true

		orig := &State{
			Users: map[string]*User{"u1": {ID: "u1", Name: "alice", Operator: true, Created: now}},
			Teams: map[string]*Team{"t1": {
				ID: "t1", Name: "acme", Members: []Member{{UserID: "u1", Role: "owner"}},
				Created: now, AcceptSharesFrom: []string{"t2"},
				SelfToken: SelfTokenPolicy{PublicPorts: true, SSH: true},
			}},
			Keys: map[string]*APIKey{"k1": {
				ID: "k1", UserID: "u1", TeamID: "t1", Name: "main", Scopes: []string{"admin", "exec"},
				Salt: "s", Hash: "h", Gen: 2, Created: now, PrevSalt: "ps", PrevHash: "ph",
				PrevUntil: &later, Rotated: &now, Expires: &later,
			}},
			Secrets:    map[string]*Secret{"t1/S1": {TeamID: "t1", Name: "S1", Sealed: "ct", Updated: now, Inject: true}},
			Webhooks:   map[string]*Webhook{"wh1": {ID: "wh1", TeamID: "t1", URL: "https://x.test", Events: []string{"*"}, SealedSecret: "whs", Created: now, PrevSealedSecret: "old", PrevUntil: &later, Name: "h", Format: "boatdev", Updated: now}},
			Deliveries: map[string][]Delivery{"wh1": {{ID: "d1", WebhookID: "wh1", EventID: "ev1", Event: "vm.started", Attempt: 1, Status: 200, OK: true, At: now}}},
			SSHGrants:  map[string]*SSHGrant{"g1": {TeamID: "t1", PublicKey: r11Pub, VMs: []string{"vm1"}, CleanupVMs: []string{"vm0"}, KeyID: "k1", CreatedByKeyGen: 2, UserID: "u1", Created: now}},
			VMSecrets:  map[string][]string{"vm1": {"S1"}},
			Commands:   map[string][]CommandRecord{"vm1": {{ID: "c1", Cmd: []string{"echo", "hi"}, Cwd: "/tmp", Kind: "command", KeyID: "k1", StartedAt: now, State: "exited", Exit: &exitCode, FinishedAt: &later, ProcessID: 1}}},
			Prompts:    map[string][]*PromptRun{"vm1": {{ID: "p1", VMID: "vm1", ConversationID: "conv_1", Agent: "claude", Model: "sonnet", Prompt: "hi", CmdID: "c1", State: "completed", Exit: &exitCode, Created: now, Finished: &later}}},
			Conversations: map[string][]*Conversation{"vm1": {{
				ID: "conv_1", VMID: "vm1", Agent: "claude", Model: "sonnet", Session: "sess-1", PromptCount: 1, LastPrompt: "hi", Created: now, Updated: later,
			}}},
			Environments: map[string]*Environment{"env1": {
				ID: "env1", TeamID: "t1", Name: "base", Image: "ubuntu-24.04", Size: "small", SetupScript: "true",
				Env: map[string]string{"K": "V"}, Secrets: &secSlice, IsDefault: true, Version: 3, Created: now, Updated: later,
				Build: &EnvBuild{State: "ready", Version: 3, VMID: "vm0", SnapshotID: "sn0", At: now},
			}},
			VMEnvs:    map[string]string{"vm1": "env1"},
			VMEnvPins: map[string]*EnvPin{"vm1": {Version: 1, Vars: map[string]string{"K": "v"}}},
			VMTransitions: map[string]*CredentialTransition{"vm1": {
				NoEnv: true, EnvID: "env1", Version: 2, Vars: map[string]string{"K": "next"},
				Selection: []string{"S1"}, KeepSelection: true,
			}},
			VMNames:              map[string]string{"vm1": "api worker"},
			VMEnvErrors:          map[string]string{"vm1": "env failed"},
			NoEnvRuns:            map[string]time.Time{"vm1": now},
			SnapShares:           map[string]*SnapShare{"sh1": {ID: "sh1", TeamID: "t1", VMID: "vm1", SnapshotID: "sn1", Grantee: "t2", Expires: &later, Uses: 1, Created: now, KeyID: "k1", MintedByKeyGen: 2}},
			AICreds:              map[string]*AICredential{"t1/anthropic": {TeamID: "t1", Provider: "anthropic", Sealed: "ais", Last4: "1234", Updated: now}},
			AIVMs:                map[string]*AIVM{"vm1": {TeamID: "t1", Gen: 4, Blocked: false}},
			AIProxyOff:           map[string]bool{"t1": true},
			AIUsage:              map[string]*AIUsageRow{"t1/vm1/anthropic/sonnet": {TeamID: "t1", VMID: "vm1", Provider: "anthropic", Model: "sonnet", Requests: 5, InputTokens: 10, OutputTokens: 20, Updated: now}},
			AIUsageDays:          map[string]*AIUsageRow{"t1/vm1/anthropic/sonnet/2026-01-02": {TeamID: "t1", VMID: "vm1", Provider: "anthropic", Model: "sonnet", Requests: 5, InputTokens: 10, OutputTokens: 20, Updated: now, Day: "2026-01-02"}},
			AIUsageDaysV1:        true,
			EnvErrorSpans:        map[string][]EnvErrorSpan{"vm1": {{Start: now, End: later}, {Start: later}}},
			EnvErrorCutoffs:      map[string]time.Time{"vm1": now},
			EnvErrorLegacyBefore: now,
			Telemetry:            map[string]*TeamTelemetry{"t1": {Endpoint: "https://otel.test", SealedHeaders: "sh", HeaderNames: []string{"X-Test"}, Updated: now}},
			ForeignVMs:           map[string]bool{"vm1": true},
			RouteGens:            map[string]int{"vm1": 3},
			PortMinters:          map[string][]string{"vm1": {"k1"}},
			PortMinterGens:       map[string][]string{"vm1": {"k1@2"}},
			DesktopVMs:           map[string]bool{"vm1": true},
			StopOps: map[string]map[string]any{"stop1": {
				"_team": "t1", "nested_map": map[string]any{"k": "v"}, "nested_slice": []any{"a", map[string]string{"m": "n"}},
				"str_ptr": &nestedStr, "bool_ptr": &nestedBool,
			}},
			StopLast:          map[string]string{"vm1": "stop1"},
			DeletionOps:       map[string]map[string]any{"del1": {"_team": "t1", "tags": []string{"x"}}},
			Idempotency:       map[string]*IdemRecord{"t1\x00ik1": {Hash: "hh", Status: 202, CType: "application/json", Body: []byte(`{"ok":true}`), At: now, KeyID: "k1", KeyGen: 2, TeamID: "t1", VMIDs: []string{"vm1"}, RouteGens: map[string]int{"vm1": 3}}},
			VMExtraEnv:        map[string]map[string]string{"vm1": {"TENANT_ID": "t1"}},
			VMTokens:          map[string]*VMToken{"vm1": {TeamID: "t1", Gen: 5, RestrictSelf: true}},
			CompatIDs:         map[string]*CompatID{"bx_23456789": {VM: "vm1", Team: "t1"}},
			CompatByVM:        map[string]string{"vm1": "bx_23456789"},
			CompatSetups:      map[string]*CompatSetup{"vm1": {Status: "done", At: now}},
			CommandSeq:        map[string]int{"vm1": 9},
			CopyCredentialsV1: true,
			Seq:               42,
		}

		// Reflection check: every exported field on State must be non-zero in orig so future fields cannot be missed!
		rv := reflect.ValueOf(orig).Elem()
		rt := rv.Type()
		for i := 0; i < rt.NumField(); i++ {
			sf := rt.Field(i)
			if !sf.IsExported() {
				continue
			}
			if rv.Field(i).IsZero() {
				t.Fatalf("State.%s is zero in exhaustive clone test; populate and mutate it!", sf.Name)
			}
		}

		beforeJSON, err := json.Marshal(orig)
		if err != nil {
			t.Fatal(err)
		}

		cl := orig.clone()

		// Mutate every nested structure in cl.
		cl.Users["u1"].Name = "mutated"
		cl.Teams["t1"].Members[0].Role = "member"
		cl.Teams["t1"].AcceptSharesFrom[0] = "other"
		cl.Keys["k1"].Scopes[0] = "vms:read"
		*cl.Keys["k1"].Expires = now.Add(99 * time.Hour)
		cl.Secrets["t1/S1"].Sealed = "changed"
		cl.Webhooks["wh1"].Events[0] = "vm.stopped"
		*cl.Webhooks["wh1"].PrevUntil = now
		cl.Deliveries["wh1"][0].Status = 500
		cl.SSHGrants["g1"].VMs[0] = "vm999"
		cl.SSHGrants["g1"].CleanupVMs[0] = "vm888"
		cl.VMSecrets["vm1"][0] = "OTHER"
		cl.Commands["vm1"][0].Cmd[0] = "rm"
		*cl.Commands["vm1"][0].Exit = 99
		*cl.Prompts["vm1"][0].Exit = 88
		cl.Conversations["vm1"][0].Session = "changed"
		cl.Environments["env1"].Env["K"] = "MUTATED"
		(*cl.Environments["env1"].Secrets)[0] = "MUT"
		cl.Environments["env1"].Build.State = "failed"
		cl.VMEnvs["vm1"] = "env2"
		cl.VMEnvPins["vm1"].Vars["K"] = "mutated"
		cl.VMTransitions["vm1"].Vars["K"] = "mutated"
		cl.VMTransitions["vm1"].Selection[0] = "OTHER"
		cl.VMTransitions["vm1"].Version = 999
		cl.VMTransitions["vm1"].EnvID = "env2"
		cl.VMTransitions["vm1"].NoEnv = false
		cl.VMTransitions["vm1"].KeepSelection = false
		cl.VMNames["vm1"] = "mutated"
		cl.VMExtraEnv["vm1"]["TENANT_ID"] = "mutated"
		cl.VMEnvErrors["vm1"] = "mutated"
		cl.NoEnvRuns["vm1"] = now.Add(time.Hour)
		*cl.SnapShares["sh1"].Expires = now
		cl.AICreds["t1/anthropic"].Last4 = "9999"
		cl.AIVMs["vm1"].Gen = 999
		cl.AIProxyOff["t1"] = false
		cl.AIUsage["t1/vm1/anthropic/sonnet"].Requests = 999
		cl.AIUsageDays["t1/vm1/anthropic/sonnet/2026-01-02"].Requests = 999
		cl.EnvErrorSpans["vm1"][0].End = now
		cl.EnvErrorCutoffs["vm1"] = later
		cl.EnvErrorLegacyBefore = later
		cl.Telemetry["t1"].HeaderNames[0] = "X-Mut"
		cl.ForeignVMs["vm1"] = false
		cl.RouteGens["vm1"] = 999
		cl.PortMinters["vm1"][0] = "k999"
		cl.PortMinterGens["vm1"][0] = "k999@1"
		cl.DesktopVMs["vm1"] = false
		cl.StopOps["stop1"]["nested_map"].(map[string]any)["k"] = "mutated"
		cl.StopOps["stop1"]["nested_slice"].([]any)[1].(map[string]string)["m"] = "mutated"
		*cl.StopOps["stop1"]["str_ptr"].(*string) = "mutated"
		*cl.StopOps["stop1"]["bool_ptr"].(*bool) = false
		cl.StopLast["vm1"] = "stop999"
		cl.DeletionOps["del1"]["tags"].([]string)[0] = "mutated"
		cl.Idempotency["t1\x00ik1"].Body[0] = 'X'
		cl.Idempotency["t1\x00ik1"].VMIDs[0] = "vm999"
		cl.Idempotency["t1\x00ik1"].RouteGens["vm1"] = 777
		cl.VMTokens["vm1"].Gen = 777
		cl.CompatIDs["bx_23456789"].VM = "vm999"
		cl.CompatByVM["vm1"] = "bx_99999999"
		cl.CompatSetups["vm1"].Status = "failed"
		cl.CommandSeq["vm1"] = 999
		cl.CopyCredentialsV1 = false

		afterJSON, err := json.Marshal(orig)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(beforeJSON, afterJSON) {
			t.Fatalf("mutating State.clone() corrupted orig!\nbefore: %s\nafter:  %s", beforeJSON, afterJSON)
		}
	})

	t.Run("coalesced_credential_rotations_and_pooling", func(t *testing.T) {
		for _, addr := range []string{"http://127.0.0.1:9999", "/tmp/boatd-test.sock"} {
			bc := newBoatd(addr)
			tr, ok := bc.hc.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("newBoatd(%q) transport is %T, want *http.Transport", addr, bc.hc.Transport)
			}
			if tr.MaxIdleConns != 128 || tr.MaxIdleConnsPerHost != 64 || tr.IdleConnTimeout != 90*time.Second {
				t.Fatalf("newBoatd(%q) pooling = (%d, %d, %v), want (128, 64, 90s)",
					addr, tr.MaxIdleConns, tr.MaxIdleConnsPerHost, tr.IdleConnTimeout)
			}
		}

		e := setup(t)
		e.enableSSH()
		e.srv.cfg.AIProxyURL = "https://ai.example"
		e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "sk-ant-test"})

		vm := e.createVM()
		e.boatd.mu.Lock()
		envOpsBefore := len(e.boatd.envOps)
		e.boatd.mu.Unlock()

		// Forking a VM with both SSH helper and AI proxy credentials must coalesce parent
		// credential rotation and perform at most ONE POST /v1/vms/{parent}/env call!
		st, child := e.do(e.owner, "POST", vmPath(vm, "fork"), map[string]any{"name": "child"})
		if st != 201 {
			t.Fatalf("forkVM: %d %v", st, child)
		}
		time.Sleep(50 * time.Millisecond) // catch any stray async pushSelf goroutine
		e.boatd.mu.Lock()
		parentEnvPushes := 0
		for _, op := range e.boatd.envOps[envOpsBefore:] {
			if op["vm"] == vm {
				parentEnvPushes++
			}
		}
		e.boatd.mu.Unlock()
		if parentEnvPushes != 1 {
			t.Fatalf("parent credential refresh pushed /env %d times on fork, want 1", parentEnvPushes)
		}

		// createShare with refreshAI == true must also perform at most ONE POST /v1/vms/{vm}/env call!
		st, snap := e.do(e.owner, "POST", vmPath(vm, "snapshots"), map[string]any{"name": "sh-snap"})
		if st != 201 {
			t.Fatalf("snapshot: %d %v", st, snap)
		}
		e.boatd.mu.Lock()
		envOpsBeforeShare := len(e.boatd.envOps)
		e.boatd.mu.Unlock()

		st, sh := e.do(e.owner, "POST", vmPath(vm, "snapshots", snap["id"].(string), "shares"), map[string]any{})
		if st != 201 {
			t.Fatalf("createShare: %d %v", st, sh)
		}
		time.Sleep(50 * time.Millisecond)
		e.boatd.mu.Lock()
		shareEnvPushes := 0
		for _, op := range e.boatd.envOps[envOpsBeforeShare:] {
			if op["vm"] == vm {
				shareEnvPushes++
			}
		}
		e.boatd.mu.Unlock()
		if shareEnvPushes != 1 {
			t.Fatalf("createShare pushed /env %d times, want 1", shareEnvPushes)
		}

		// Batch create (count > 1) provisions self and AI tokens across all VMs cleanly.
		st, batch := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "batch", "count": 3})
		if st != 201 || len(batch["vms"].([]any)) != 3 {
			t.Fatalf("batch createVMs: %d %v", st, batch)
		}
	})

	t.Run("indexed_o1_jump_ssh_lookup", func(t *testing.T) {
		e := setup(t)
		e.enableSSH()
		vm1 := e.createVM()
		vm2 := e.createVM()

		st, sshOut := e.do(e.owner, "POST", vmPath(vm1, "ssh"), map[string]any{"public_key": r11Pub})
		if st != 200 {
			t.Fatalf("sshVM: %d %v", st, sshOut)
		}

		var boatdCalls atomic.Int64
		var lastPath atomic.Value
		origTransport := e.srv.boatd.hc.Transport
		e.srv.boatd.hc.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			boatdCalls.Add(1)
			lastPath.Store(req.URL.RequestURI())
			return origTransport.RoundTrip(req)
		})

		// 1. Unknown public key miss: O(1) index miss with ZERO boatd HTTP calls!
		ungrantedPub := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILM+rvN+ot98qgEN796jTiQfZfG1KaT0PtFDJ/XFSqti ungranted"
		boatdCalls.Store(0)
		code, lines := e.jumpSSH(ungrantedPub)
		if code != 204 || len(lines) != 0 {
			t.Fatalf("unknown jump SSH key: code=%d lines=%v, want 204 empty", code, lines)
		}
		if got := boatdCalls.Load(); got != 0 {
			t.Fatalf("unknown jump SSH key made %d boatd calls, want 0", got)
		}

		// 2. Single-VM grant hit: fetches ONLY GET /v1/vms/{vm1} (1 boatd call, not full VM list).
		boatdCalls.Store(0)
		code, lines = e.jumpSSH(r11Pub)
		if code != 200 || len(lines) != 1 {
			t.Fatalf("granted jump SSH key: code=%d lines=%v, want 200 [1 line]", code, lines)
		}
		if got, p := boatdCalls.Load(), lastPath.Load(); got != 1 || p != "/v1/vms/"+vm1 {
			t.Fatalf("single-VM jump SSH lookup made %d calls (last=%v), want 1 call to /v1/vms/%s", got, p, vm1)
		}

		// 3. Large multi-VM grant hit (> 4 VMs on a team): promotes to 1 team-scoped view=ssh call!
		for _, extra := range []string{vm2, e.createVM(), e.createVM(), e.createVM()} {
			if st, out := e.do(e.owner, "POST", vmPath(extra, "ssh"), map[string]any{"public_key": r11Pub}); st != 200 {
				t.Fatalf("sshVM(%s): %d %v", extra, st, out)
			}
		}
		boatdCalls.Store(0)
		code, lines = e.jumpSSH(r11Pub)
		if code != 200 || len(lines) != 1 {
			t.Fatalf("large-grant jump SSH lookup: code=%d lines=%v", code, lines)
		}
		gotURI, _ := lastPath.Load().(string)
		if boatdCalls.Load() != 1 || !strings.Contains(gotURI, "team="+url.QueryEscape(e.teamID())) || !strings.Contains(gotURI, "view=ssh") {
			t.Fatalf("large-grant jump SSH lookup made %d calls (uri=%q), want 1 team-scoped view=ssh call", boatdCalls.Load(), gotURI)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestR24RunningStartResumeRetypeNullTTLAndUnshareParity(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	e.srv.cfg.VMDomain = "vm.boat.example"
	if len(e.srv.cfg.DesktopKey) == 0 {
		e.srv.cfg.DesktopKey = []byte("0123456789abcdef0123456789abcdef")
	}
	e.srv.cfg.AIProxyURL = "https://ai.example"
	e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "sk-ant-live"})

	bx := e.newSandbox(map[string]any{"ttlSeconds": 600, "type": "small"})
	vmID := e.nativeOf(bx)
	base := "/api/v1/sandboxes/" + bx

	selfBefore := e.selfTokenOf(vmID)
	aiGenBefore := e.srv.store.Snapshot().AIVMs[vmID].Gen
	aiTokBefore := e.srv.aiToken(vmID, aiGenBefore)

	e.boatd.mu.Lock()
	policyCallsBefore := e.boatd.sshPolicyCalls
	e.boatd.mu.Unlock()

	// 1. Native POST /v1/vms/{id}/start on an already-running VM preserves self & AI tokens,
	// skips redundant SSH policy provisioning, and pushes caller env via POST /v1/vms/{id}/env!
	st, startedOut := e.do(e.owner, "POST", vmPath(vmID, "start"), map[string]any{
		"env": map[string]string{"LIVE_OVERRIDE": "v1"},
	})
	if st != 200 {
		t.Fatalf("startVM on running VM: %d %v", st, startedOut)
	}
	if st, _ := e.do(selfBefore, "GET", vmPath(vmID), nil); st != 200 {
		t.Fatalf("startVM on running VM broke existing guest BOAT_API_KEY: status=%d", st)
	}
	if _, _, ok := e.srv.aiCheckToken(aiTokBefore); !ok {
		t.Fatal("startVM on running VM broke existing guest AI proxy token")
	}
	e.boatd.mu.Lock()
	policyCallsAfter := e.boatd.sshPolicyCalls
	guestEnvAfter, _ := e.boatd.vms[vmID]["env"].(map[string]any)
	e.boatd.mu.Unlock()
	if policyCallsAfter != policyCallsBefore {
		t.Fatalf("startVM on running VM re-provisioned current SSH policy: %d -> %d", policyCallsBefore, policyCallsAfter)
	}
	if guestEnvAfter["LIVE_OVERRIDE"] != "v1" || guestEnvAfter["BOAT_API_KEY"] != selfBefore {
		t.Fatalf("startVM on running VM did not push merged env with intact BOAT_API_KEY: %v", guestEnvAfter)
	}

	// Same for compatResume on an already-running VM (no size change): tokens stay valid!
	e.ok("POST", base+"/resume", map[string]any{"env": map[string]string{"LIVE_OVERRIDE": "v2"}}, 202)
	if st, _ := e.do(selfBefore, "GET", vmPath(vmID), nil); st != 200 {
		t.Fatalf("compatResume on running VM broke guest BOAT_API_KEY: status=%d", st)
	}
	if _, _, ok := e.srv.aiCheckToken(aiTokBefore); !ok {
		t.Fatal("compatResume on running VM broke guest AI proxy token")
	}

	// 2. compatResume with unknown type ("medium") fails 400 BEFORE stopping a running VM!
	e.boatd.mu.Lock()
	stopsBefore := len(e.boatd.stops)
	e.boatd.mu.Unlock()

	rawReq := httptest.NewRequest("POST", base+"/resume", strings.NewReader(`{"type":"medium"}`))
	rawReq.Header.Set("Authorization", "Bearer "+e.owner)
	rawReq.Header.Set("Content-Type", "application/json")
	rawRec := httptest.NewRecorder()
	e.srv.ServeHTTP(rawRec, rawReq)
	if rawRec.Code != 409 { // r46: a running sandbox is not retyped by resume
		t.Fatalf("running resume with type medium status=%d body=%s, want 409", rawRec.Code, rawRec.Body.String())
	}
	e.boatd.mu.Lock()
	stopsAfterBadType := len(e.boatd.stops)
	curState := e.boatd.vms[vmID]["state"]
	startsBeforeResize := len(e.boatd.starts)
	e.boatd.mu.Unlock()
	if stopsAfterBadType != stopsBefore || curState != "running" {
		t.Fatalf("unknown type stopped running VM: stops %d->%d state=%v", stopsBefore, stopsAfterBadType, curState)
	}

	// Valid running resize via compatResume stops once, resizes, starts ONCE (not twice!),
	// supersedes outstanding stop operations, and accepts ttlSeconds: null!
	stopOp := e.srv.stops.begin(e.teamID(), vmID)
	e.srv.stops.fail(vmID, stopOp, "prior stop failure")

	e.do(e.owner, "POST", "/v1/vms/"+vmID+"/stop", nil) // r46: the type changes on a stopped sandbox
	e.boatd.mu.Lock()
	startsBeforeResize = len(e.boatd.starts)
	e.boatd.mu.Unlock()
	resOut := e.ok("POST", base+"/resume", map[string]any{"type": "large", "ttlSeconds": nil}, 202)
	resSB := resOut["sandbox"].(map[string]any)
	if resOut["type"] != "sandbox.resuming" || resSB["type"] != "large" || resSB["ttlSeconds"] != nil || resSB["archiveAfter"] != nil {
		t.Fatalf("compatResume resize + null TTL unexpected response: %v", resOut)
	}
	if stopRec, _ := resSB["stop"].(map[string]any); stopRec == nil || stopRec["status"] != "superseded" {
		t.Fatalf("compatResume after running resize did not supersede prior stop op: %v", resSB["stop"])
	}
	e.boatd.mu.Lock()
	startsDelta := len(e.boatd.starts) - startsBeforeResize
	e.boatd.mu.Unlock()
	if startsDelta != 1 {
		t.Fatalf("compatResume running resize called boatd start %d times, want 1", startsDelta)
	}

	// 3. compatFork with ttlSeconds: null disables auto-stop on child.
	forkOut := e.ok("POST", base+"/fork", map[string]any{"name": "null-ttl-fork", "ttlSeconds": nil}, 202)
	forkSB := forkOut["sandbox"].(map[string]any)
	if forkSB["ttlSeconds"] != nil || forkSB["archiveAfter"] != nil {
		t.Fatalf("compatFork ttlSeconds:null unexpected sandbox: %v", forkSB)
	}

	// 4. Creator-only privacy cannot be provided by team-scoped sandboxes.
	e.ok("POST", base+"/host", map[string]any{"port": 9090, "public": true}, 200)
	unshSt, unsh, _ := e.compat(e.owner, "POST", base+"/unshare", nil, nil)
	if unshSt != 501 || errCode(unsh) != "not_supported" || unsh["ok"] != false {
		t.Fatalf("compatUnshare status=%d body=%v, want 501 not_supported", unshSt, unsh)
	}
}
