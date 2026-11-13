package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestR15CreateSetupRequiresExecBeforeSideEffects(t *testing.T) {
	for _, body := range []map[string]any{
		{"setupScript": "echo $PRIVATE_SECRET"},
		{"files": []map[string]string{{"path": "nested/start.sh", "content": "echo $PRIVATE_SECRET"}}},
		{"count": 2, "setupScript": "true"},
	} {
		t.Run("denied", func(t *testing.T) {
			e := setup(t)
			if st, out := e.do(e.owner, "PUT", "/v1/secrets/PRIVATE_SECRET", map[string]any{"value": "secret", "inject": true}); st != 200 {
				t.Fatalf("secret: %d %v", st, out)
			}
			tok := e.newKey("vms:write")
			st, out, _ := e.compat(tok, "POST", "/api/v1/sandboxes", body, nil)
			if st != 403 || errCode(out) != "forbidden" {
				t.Errorf("create without exec: %d %v", st, out)
			}
			e.boatd.mu.Lock()
			defer e.boatd.mu.Unlock()
			if len(e.boatd.creates) != 0 || len(e.boatd.execs) != 0 || len(e.boatd.files) != 0 || len(e.boatd.envOps) != 0 {
				t.Error("denied create reached boatd or injected secrets")
			}
			if len(e.srv.store.Snapshot().VMTokens) != 0 || len(e.srv.store.Snapshot().CompatSetups) != 0 {
				t.Error("denied create left VM state")
			}
		})
	}
	e := setup(t)
	tok := e.newKey("vms:write")
	if st, out, _ := e.compat(tok, "POST", "/api/v1/sandboxes", nil, nil); st != 202 {
		t.Fatalf("plain create must still work: %d %v", st, out)
	}
}

func TestR15IdempotencyRechecksCreateAuthorization(t *testing.T) {
	for _, body := range []map[string]any{
		{"hostedPorts": []map[string]int{{"port": 8080}}},
		{"setupScript": "true"},
		{"files": []map[string]string{{"path": "boot.txt", "content": "x"}}},
	} {
		t.Run("replay", func(t *testing.T) {
			e := setup(t)
			e.srv.cfg.VMDomain = "vm.example"
			tok := e.newKey("vms:write", "exec")
			hdr := map[string]string{"Idempotency-Key": "privileged-create"}
			st, first, _ := e.compat(tok, "POST", "/api/v1/sandboxes", body, hdr)
			if st != 202 {
				t.Fatalf("initial create: %d %v", st, first)
			}
			if body["setupScript"] != nil {
				e.waitSetup(first["sandbox"].(map[string]any)["id"].(string), "done")
			}
			if st, replay, h := e.compat(e.owner, "POST", "/api/v1/sandboxes", body, hdr); st != 202 || h.Get("Idempotent-Replayed") != "true" || replay["sandbox"].(map[string]any)["id"] != first["sandbox"].(map[string]any)["id"] {
				t.Fatalf("authorized same-team replay: %d %v", st, replay)
			}
			weak := e.newKey("vms:write")
			if st, out, h := e.compat(weak, "POST", "/api/v1/sandboxes", body, hdr); st != 403 || h.Get("Idempotent-Replayed") != "" || out["hostedPorts"] != nil {
				t.Errorf("exec-less replay: %d %v", st, out)
			}
			kid := keyID(e, tok)
			if err := e.srv.store.Update(func(st *State) error {
				st.Keys[kid].Scopes = []string{"vms:write"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if st, out, _ := e.compat(tok, "POST", "/api/v1/sandboxes", body, hdr); st != 403 {
				t.Errorf("replay after scope removal: %d %v", st, out)
			}
			other := otherTeamKey(e)
			if st, out, h := e.compat(other, "POST", "/api/v1/sandboxes", body, hdr); st != 202 || h.Get("Idempotent-Replayed") != "" || out["sandbox"].(map[string]any)["id"] == first["sandbox"].(map[string]any)["id"] {
				t.Errorf("team cache isolation: %d %v", st, out)
			}
		})
	}
}

func TestR15SelfGenerationFailsClosedOnPersistenceError(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	vm := e.createVM()
	old := e.selfTokenOf(vm)
	if err := e.srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.bumpSelf(vm, e.teamID()); err == nil {
		t.Error("token generation succeeded without durable persistence")
	}
	if err := e.srv.rotateSelf(vm); err == nil {
		t.Error("rotation swallowed the persistence failure")
	}
	if e.srv.authenticateSelf(old) == nil {
		t.Error("failed rotation changed the published token generation")
	}
}

func TestR15CopiesWithholdOnRotationPersistenceError(t *testing.T) {
	for _, action := range []string{"fork", "snapshots", "share"} {
		t.Run(action, func(t *testing.T) {
			e := setup(t)
			e.srv.cfg.VMAPIURL = "https://boat.example"
			vm := e.createVM()
			path := "/v1/vms/" + vm + "/" + action
			if action == "share" {
				st, sn := e.do(e.owner, "POST", "/v1/vms/"+vm+"/snapshots", nil)
				if st != 201 {
					t.Fatalf("snapshot: %d %v", st, sn)
				}
				path = "/v1/vms/" + vm + "/snapshots/" + sn["id"].(string) + "/shares"
				if err := e.srv.store.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				e.boatd.mu.Lock()
				e.boatd.afterCopy = func() { e.srv.store.Close() }
				e.boatd.mu.Unlock()
			}
			st, out := e.do(e.owner, "POST", path, nil)
			errObj, _ := out["error"].(map[string]any)
			wantCode, wantVMs := "store_error", 1
			if action == "fork" {
				wantCode, wantVMs = "cleanup_failed", 2
			} // retirement must persist before deletion too
			if st != 500 || out["id"] != nil || errObj["code"] != wantCode {
				t.Errorf("copy exposed without durable rotation: %d %v", st, out)
			}
			e.boatd.mu.Lock()
			defer e.boatd.mu.Unlock()
			if len(e.boatd.vms) != wantVMs || e.boatd.vms[vm] == nil {
				t.Error("failed fork lost the parent or leaked an unexpected VM")
			}
			for id, child := range e.boatd.vms {
				if id != vm && (child["pending_copy"] != true || child["state"] == "running") {
					t.Error("failed copy escaped its pending fence")
				}
			}
			wantSnaps := 0
			if action == "share" {
				wantSnaps = 1
			}
			if len(e.boatd.snaps) != wantSnaps || len(e.srv.store.Snapshot().SnapShares) != 0 {
				t.Error("failed rotation left a new snapshot or share, or removed the existing snapshot")
			}
		})
	}
}

func TestR15SelfRotationIsDurableBeforeReturn(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	vm := e.createVM()
	old := e.selfTokenOf(vm)
	if err := e.srv.rotateSelf(vm); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(e.srv.store.path)
	if err != nil {
		t.Fatal(err)
	}
	var disk State
	if err := json.Unmarshal(b, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.VMTokens[vm] == nil || disk.VMTokens[vm].Gen != e.srv.store.Snapshot().VMTokens[vm].Gen || e.srv.authenticateSelf(old) != nil {
		t.Error("rotation returned before its generation was durable")
	}
}

func TestR15CopyCleanupFailureIsReported(t *testing.T) {
	for _, action := range []string{"fork", "snapshots", "foreign-fork"} {
		t.Run(action, func(t *testing.T) {
			e := setup(t)
			e.srv.cfg.VMAPIURL = "https://boat.example"
			vm := e.createVM()
			if action == "foreign-fork" {
				if err := e.srv.store.Update(func(st *State) error {
					st.ForeignVMs[vm] = true
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				action = "fork"
			}
			e.boatd.mu.Lock()
			e.boatd.cleanupErr = true
			e.boatd.afterCopy = func() { e.srv.store.Close() }
			e.boatd.mu.Unlock()
			st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/"+action, nil)
			errObj, _ := out["error"].(map[string]any)
			if st != 500 || errObj["code"] != "cleanup_failed" || out["id"] != nil {
				t.Errorf("cleanup failure hidden or copy returned: %d %v", st, out)
			}
			e.boatd.mu.Lock()
			for id, child := range e.boatd.vms {
				if id != vm && (child["pending_copy"] != true || child["state"] == "running") {
					t.Errorf("failed cleanup left an active copy: %v", child)
				}
			}
			for _, snap := range e.boatd.snaps {
				if snap["pending_copy"] != true {
					t.Errorf("failed cleanup published snapshot: %v", snap)
				}
			}
			e.boatd.mu.Unlock()
		})
	}
}

func TestR15ReplayRechecksMembership(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMDomain = "vm.example"
	tok, uid := e.addMember("member")
	body := map[string]any{"hostedPorts": []map[string]int{{"port": 8080}}}
	hdr := map[string]string{"Idempotency-Key": "member-create"}
	if st, out, _ := e.compat(tok, "POST", "/api/v1/sandboxes", body, hdr); st != 202 {
		t.Fatalf("create: %d %v", st, out)
	}
	if st, out := e.do(e.owner, "DELETE", "/v1/teams/"+e.teamID()+"/members/"+uid, nil); st != 200 {
		t.Fatalf("remove member: %d %v", st, out)
	}
	if st, out, h := e.compat(tok, "POST", "/api/v1/sandboxes", body, hdr); st != 401 || h.Get("Idempotent-Replayed") != "" {
		t.Errorf("removed member received replay: %d %v", st, out)
	}
}
