package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestR16FailedCopyCannotUseParentCredentials(t *testing.T) {
	for _, action := range []string{"fork", "snapshots"} {
		t.Run(action, func(t *testing.T) {
			e := setup(t)
			e.srv.cfg.VMAPIURL, e.srv.cfg.AIProxyURL = "https://boat.example", "https://ai.example"
			e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "synthetic-provider-key"})
			vm := e.createVM()
			self := e.selfTokenOf(vm)
			ai := e.srv.aiToken(vm, e.srv.store.Snapshot().AIVMs[vm].Gen)
			e.boatd.mu.Lock()
			e.boatd.cleanupErr = true
			e.boatd.afterCopy = func() { e.srv.store.Close() }
			e.boatd.mu.Unlock()
			status, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/"+action, nil)
			if status != 500 || out["id"] != nil {
				t.Fatalf("failed copy returned: %d %v", status, out)
			}
			execStatus, _ := e.raw(self, "POST", "/v1/vms/"+vm+"/exec", map[string]any{"cmd": []string{"true"}})
			_, _, aiAlive := e.srv.aiCheckToken(ai)
			_, vms := e.do(e.owner, "GET", "/v1/vms", nil)
			_, snaps := e.do(e.owner, "GET", "/v1/snapshots", nil)
			e.boatd.mu.Lock()
			defer e.boatd.mu.Unlock()
			for id, child := range e.boatd.vms {
				if id != vm && child["state"] == "running" {
					t.Errorf("failed copy runs with parent self exec=%d AI valid=%v", execStatus, aiAlive)
				}
			}
			if len(list(vms, "vms")) != 1 || len(list(snaps, "snapshots")) != 0 {
				t.Errorf("failed copy is public with parent self exec=%d AI valid=%v: %v %v", execStatus, aiAlive, vms, snaps)
			}
		})
	}
}

func TestR16LateMintCannotResurrectCopiedCredentials(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL, e.srv.cfg.AIProxyURL = "https://boat.example", "https://ai.example"
	e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "synthetic-provider-key"})
	vm, team := e.createVM(), e.teamID()
	self := e.selfTokenOf(vm)
	ai := e.srv.aiToken(vm, e.srv.store.Snapshot().AIVMs[vm].Gen)
	if status, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/snapshots", nil); status != 201 {
		t.Fatalf("snapshot: %d %v", status, out)
	}
	if status, out := e.do(e.owner, "DELETE", "/v1/vms/"+vm, nil); status != 200 {
		t.Fatalf("delete: %d %v", status, out)
	}
	// Model an already-authorized request reaching issuance after deletion won.
	e.srv.aiEnv(vm, team, true)
	e.srv.guestEnv(vm, team, nil)
	_, _, aiAlive := e.srv.aiCheckToken(ai)
	if aiAlive || e.srv.authenticateSelf(self) != nil {
		t.Fatalf("copied credentials resurrected: AI=%v self=%v", aiAlive, e.srv.authenticateSelf(self) != nil)
	}
	if _, err := e.srv.bumpSelf(vm, team); err == nil {
		t.Error("retired self identity issued again")
	}
	if _, err := e.srv.aiEnv(vm, team, false); err == nil {
		t.Error("retired AI identity issued again")
	}
	if err := e.srv.recordVM(vm, team, []string{}, true, false); err == nil {
		t.Error("foreign registration revived a retired identity")
	}
	if keyAliveIn(e.srv.store.Snapshot(), "vm_"+vm, team, "", time.Now()) {
		t.Error("retired VM's durable grant survived")
	}
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	back, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	reloaded := &server{store: back, keys: e.srv.keys}
	if _, _, ok := reloaded.aiCheckToken(ai); ok || reloaded.authenticateSelf(self) != nil {
		t.Error("restart revived retired credentials")
	}
}

func TestR16RegistryAndAIRevocationFailuresStayPending(t *testing.T) {
	for _, failure := range []string{"registry", "ai"} {
		t.Run(failure, func(t *testing.T) {
			e := setup(t)
			e.srv.cfg.AIProxyURL = "https://ai.example" // no self token: AI failure is independent
			e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "synthetic-provider-key"})
			vm := e.createVM()
			if failure == "registry" {
				if err := e.srv.store.Update(func(st *State) error { st.ForeignVMs[vm] = true; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			e.boatd.mu.Lock()
			e.boatd.cleanupErr, e.boatd.afterCopy = true, func() { e.srv.store.Close() }
			e.boatd.mu.Unlock()
			status, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/fork", nil)
			errObj, _ := out["error"].(map[string]any)
			if status != 500 || errObj["code"] != "cleanup_failed" {
				t.Fatalf("failure: %d %v", status, out)
			}
			e.boatd.mu.Lock()
			defer e.boatd.mu.Unlock()
			for id, v := range e.boatd.vms {
				if id != vm && (v["pending_copy"] != true || v["state"] == "running") {
					t.Errorf("failed %s activated child: %v", failure, v)
				}
			}
		})
	}
}

func TestR16ForeignRegistrationPreservesGeneration(t *testing.T) {
	e := setup(t)
	vm, team := e.createVM(), e.teamID()
	if err := e.srv.store.Update(func(st *State) error { st.AIVMs[vm] = &AIVM{TeamID: team, Gen: 17}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.recordVM(vm, team, []string{}, true, false); err != nil {
		t.Fatal(err)
	}
	if got := e.srv.store.Snapshot().AIVMs[vm]; got.Gen != 17 || !got.Blocked {
		t.Fatalf("foreign registration reset identity: %+v", got)
	}
	if _, err := e.srv.aiBumpVM("missing-team-vm", "missing-team", true); err == nil {
		t.Error("issued for missing team")
	}
	if _, err := e.srv.bumpSelf("missing-team-vm", "missing-team"); err == nil {
		t.Error("issued self token for missing team")
	}
}

func TestR16DeletePersistenceFailurePrecedesDeletion(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	vm := e.createVM()
	e.srv.store.Close()
	if status, _ := e.do(e.owner, "DELETE", "/v1/vms/"+vm, nil); status != 500 {
		t.Fatalf("delete without durable retirement: %d", status)
	}
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	if e.boatd.vms[vm] == nil {
		t.Fatal("VM deleted before retirement was durable")
	}
}

func TestR16RollbackRetirementPrecedesDeletion(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	vm := e.createVM()
	e.srv.store.Close()
	if err := e.srv.discardCopyVM(vm); err == nil {
		t.Error("rollback hid retirement persistence failure")
	}
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	if e.boatd.vms[vm] == nil {
		t.Error("rollback deleted an issued identity before durable retirement")
	}
}

func TestR16RefusedCleanupKeepsCopyUnbooted(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	vm := e.createVM()
	e.boatd.mu.Lock()
	e.boatd.cleanupErr = true
	e.boatd.afterCopy = func() {
		if err := e.srv.retireVM(vm); err != nil {
			t.Error(err)
		}
	}
	e.boatd.mu.Unlock()
	status, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/fork", nil)
	errObj, _ := out["error"].(map[string]any)
	if status != 500 || errObj["code"] != "cleanup_failed" {
		t.Fatalf("cleanup refusal: %d %v", status, out)
	}
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	for id, child := range e.boatd.vms {
		if id != vm {
			if child["pending_copy"] != true || child["state"] == "running" {
				t.Error("refused cleanup released child")
			}
			if row := e.srv.store.Snapshot().VMTokens[id]; row == nil || !row.Retired {
				t.Error("cleanup did not retire before attempting deletion")
			}
		}
	}
}

type copyRoundTrip func(*http.Request) (*http.Response, error)

func (f copyRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestR16LostCopyResponsesRemainSafe(t *testing.T) {
	for _, lost := range []string{"prepare", "release"} {
		t.Run(lost, func(t *testing.T) {
			e := setup(t)
			e.srv.cfg.VMAPIURL, e.srv.cfg.AIProxyURL = "https://boat.example", "https://ai.example"
			e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "synthetic-provider-key"})
			vm := e.createVM()
			self := e.selfTokenOf(vm)
			ai := e.srv.aiToken(vm, e.srv.store.Snapshot().AIVMs[vm].Gen)
			e.srv.boatd.hc.Transport = copyRoundTrip(func(r *http.Request) (*http.Response, error) {
				resp, err := http.DefaultTransport.RoundTrip(r)
				if err == nil && r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/"+lost) && resp.StatusCode/100 == 2 {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					return nil, errors.New("lost copy response")
				}
				return resp, err
			})
			if status, _ := e.do(e.owner, "POST", "/v1/vms/"+vm+"/fork", nil); status/100 == 2 {
				t.Fatal("lost response returned success")
			}
			e.boatd.mu.Lock()
			for id, child := range e.boatd.vms {
				if id != vm && lost == "prepare" && (child["pending_copy"] != true || child["state"] == "running") {
					t.Errorf("uncertain prepare escaped: %v", child)
				}
			}
			e.boatd.mu.Unlock()
			if lost == "release" {
				if status, _ := e.raw(self, "POST", "/v1/vms/"+vm+"/exec", map[string]any{"cmd": []string{"true"}}); status != 401 {
					t.Errorf("released copy retained self capability: %d", status)
				}
				if _, _, ok := e.srv.aiCheckToken(ai); ok {
					t.Error("released copy retained AI capability")
				}
			}
		})
	}
}

func TestR16OldBoatdRefusesBeforeCopying(t *testing.T) {
	e := setup(t)
	vm := e.createVM()
	e.boatd.noPrepare = true
	for _, action := range []string{"fork", "snapshots"} {
		if status, _ := e.do(e.owner, "POST", "/v1/vms/"+vm+"/"+action, nil); status/100 == 2 {
			t.Errorf("old boatd accepted %s", action)
		}
	}
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	if len(e.boatd.vms) != 1 || len(e.boatd.snaps) != 0 || len(e.boatd.forks) != 0 {
		t.Fatal("prepare fallback allocated a copy")
	}
}

func TestR16ConcurrentCopiesRevokeCapturedCapabilities(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL, e.srv.cfg.AIProxyURL = "https://boat.example", "https://ai.example"
	e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "synthetic-provider-key"})
	vm, team := e.createVM(), e.teamID()
	_, base := e.do(e.owner, "POST", "/v1/vms/"+vm+"/snapshots", nil)
	captured, continueFirst := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	e.boatd.mu.Lock()
	e.boatd.afterCopy = func() {
		if first.CompareAndSwap(false, true) {
			close(captured)
			<-continueFirst
		}
	}
	e.boatd.beforeRelease = func(kind, id string) {
		vars := e.boatd.copyEnvs[id]
		if tok, _ := vars["BOAT_API_KEY"].(string); tok != "" && e.srv.authenticateSelf(tok) != nil {
			t.Errorf("%s release has a live captured self token", kind)
		}
		if tok, _ := vars["ANTHROPIC_API_KEY"].(string); tok != "" {
			if _, _, ok := e.srv.aiCheckToken(tok); ok {
				t.Errorf("%s release has a live captured AI token", kind)
			}
		}
	}
	e.boatd.mu.Unlock()
	done := make(chan int, 4)
	go func() { status, _ := e.do(e.owner, "POST", "/v1/vms/"+vm+"/fork", nil); done <- status }()
	select {
	case <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("first capture did not arrive")
	}
	// Publish newly issued credentials while the first copy is already immutable.
	oldGen := e.srv.store.Snapshot().VMTokens[vm].Gen
	gen, err := e.srv.bumpSelf(vm, team)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.aiEnv(vm, team, true); err != nil {
		t.Fatal(err)
	}
	e.srv.pushSelf(context.Background(), vm, gen)
	for _, action := range []string{"fork", "snapshots", "snapshots/" + base["id"].(string) + "/shares"} {
		go func() { status, _ := e.do(e.owner, "POST", "/v1/vms/"+vm+"/"+action, nil); done <- status }()
	}
	for i := 0; i < 3; i++ {
		select {
		case status := <-done:
			if status != 201 {
				t.Errorf("concurrent copy/share: %d", status)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent copy stalled")
		}
	}
	e.srv.pushSelf(context.Background(), vm, oldGen) // a delayed refresh from before capture
	close(continueFirst)
	select {
	case status := <-done:
		if status != 201 {
			t.Errorf("first copy: %d", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first copy stalled")
	}
	e.boatd.mu.Lock()
	var tokens []map[string]any
	for _, vars := range e.boatd.copyEnvs {
		tokens = append(tokens, vars)
	}
	e.boatd.mu.Unlock()
	for _, vars := range tokens {
		if tok, _ := vars["BOAT_API_KEY"].(string); tok != "" {
			if status, _ := e.raw(tok, "POST", "/v1/vms/"+vm+"/exec", map[string]any{"cmd": []string{"true"}}); status != 401 {
				t.Errorf("captured self token still executes: %d", status)
			}
		}
		if tok, _ := vars["ANTHROPIC_API_KEY"].(string); tok != "" {
			if _, _, ok := e.srv.aiCheckToken(tok); ok {
				t.Error("captured AI token still authenticates")
			}
		}
	}
}

func TestR16StartupRevocationIsDurableAndOnce(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL, e.srv.cfg.AIProxyURL = "https://boat.example", "https://ai.example"
	e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "synthetic-provider-key"})
	vm := e.createVM()
	self := e.selfTokenOf(vm)
	ai := e.srv.aiToken(vm, e.srv.store.Snapshot().AIVMs[vm].Gen)
	if err := e.srv.reconcileCopyCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	gen := e.srv.store.Snapshot().VMTokens[vm].Gen
	if err := e.srv.reconcileCopyCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.srv.store.Snapshot().VMTokens[vm].Gen != gen {
		t.Error("migration repeated")
	}
	currentSelf := e.srv.selfToken(vm, gen)
	currentAI := e.srv.aiToken(vm, e.srv.store.Snapshot().AIVMs[vm].Gen)
	if err := e.srv.boatd.call(context.Background(), "POST", vmPath(vm, "delete"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.reconcileCopyCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.srv.authenticateSelf(currentSelf) != nil {
		t.Error("missed outside-API deletion retained self credential")
	}
	if _, _, ok := e.srv.aiCheckToken(currentAI); ok {
		t.Error("missed outside-API deletion retained AI credential")
	}
	back, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	reloaded := &server{store: back, keys: e.srv.keys}
	if !back.Snapshot().CopyCredentialsV1 || reloaded.authenticateSelf(self) != nil {
		t.Error("startup self revocation was not durable")
	}
	if _, _, ok := reloaded.aiCheckToken(ai); ok {
		t.Error("startup AI revocation was not durable")
	}
	if reloaded.authenticateSelf(currentSelf) != nil {
		t.Error("retirement reconciliation was not durable")
	}
	// A prior rename may be visible after a directory-sync error without being durable yet.
	e.srv.store.Close()
	if err := e.srv.reconcileCopyCredentials(context.Background()); err == nil {
		t.Error("already-migrated startup skipped its durability barrier")
	}
}

func TestR16CanceledSnapshotAndFailedStartupStayClosed(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	vm := e.createVM()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.boatd.afterCopy = cancel
	if _, err := e.srv.captureSnapshot(ctx, vm, e.teamID(), ""); err == nil {
		t.Fatal("canceled snapshot succeeded")
	}
	e.boatd.mu.Lock()
	for _, sn := range e.boatd.snaps {
		if sn["pending_copy"] != true {
			t.Error("canceled snapshot published")
		}
	}
	e.boatd.mu.Unlock()
	e.srv.store.Close()
	if err := e.srv.reconcileCopyCredentials(context.Background()); err == nil {
		t.Fatal("startup succeeded without durable migration")
	}
}

func TestR16DelayedRefreshKeepsWorkingSourceCredentials(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL, e.srv.cfg.AIProxyURL = "https://boat.example", "https://ai.example"
	e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "synthetic-provider-key"})
	vm, team := e.createVM(), e.teamID()
	blocked, release, newSent := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	e.srv.boatd.hc.Transport = copyRoundTrip(func(r *http.Request) (*http.Response, error) {
		isEnv := r.Method == "POST" && r.URL.Path == vmPath(vm, "env")
		old := isEnv && first.CompareAndSwap(false, true)
		if old {
			close(blocked)
			<-release
		}
		resp, err := http.DefaultTransport.RoundTrip(r)
		if isEnv && !old {
			close(newSent)
		}
		return resp, err
	})
	done := make(chan error, 2)
	go func() { done <- e.srv.refreshCredentials(context.Background(), vm) }()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("old refresh did not block")
	}
	if _, err := e.srv.bumpSelf(vm, team); err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.aiBumpVM(vm, team, true); err != nil {
		t.Fatal(err)
	}
	go func() { done <- e.srv.refreshCredentials(context.Background(), vm) }()
	select {
	case <-newSent:
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("refresh stalled")
		}
	}
	vars := lastEnvOp(e, vm)
	self, _ := vars["BOAT_API_KEY"].(string)
	ai, _ := vars["ANTHROPIC_API_KEY"].(string)
	if e.srv.authenticateSelf(self) == nil {
		t.Error("delayed refresh overwrote the source's working self token")
	}
	if _, _, ok := e.srv.aiCheckToken(ai); !ok {
		t.Error("delayed refresh overwrote the source's working AI token")
	}
}

func TestR16MigrationCannotReuseLostLegacyGenerations(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL, e.srv.cfg.AIProxyURL = "https://boat.example", "https://ai.example"
	e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "synthetic-provider-key"})
	vm, team := e.createVM(), e.teamID()
	oldAI := e.srv.aiToken(vm, e.srv.store.Snapshot().AIVMs[vm].Gen)
	oldFutureSelf := e.srv.selfToken(vm, e.srv.store.Snapshot().VMTokens[vm].Gen+1)
	oldFutureKey := selfKeyID(vm, e.srv.store.Snapshot().VMTokens[vm].Gen+1)
	// Pre-upgrade first AI issuance used Apply; its record could be absent after a crash.
	if err := e.srv.store.Update(func(st *State) error { delete(st.AIVMs, vm); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.reconcileCopyCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.aiEnv(vm, team, true); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := e.srv.aiCheckToken(oldAI); ok {
		t.Error("upgrade reused a captured legacy AI token's lost generation")
	}
	if e.srv.authenticateSelf(oldFutureSelf) != nil {
		t.Error("upgrade activated a captured legacy self generation")
	}
	current := e.srv.store.Snapshot().VMTokens[vm].Gen
	principal := e.srv.authenticateSelf(e.srv.selfToken(vm, current))
	if principal == nil || !keyAliveIn(e.srv.store.Snapshot(), principal.Key.ID, team, "exec", time.Now()) {
		t.Error("upgraded self minter could not create working generation-bound links")
	}
	if keyAliveIn(e.srv.store.Snapshot(), oldFutureKey, team, "exec", time.Now()) {
		t.Error("upgrade revived legacy generation-bound links")
	}
}

func TestR16RefreshPreservesRealBoatdGuestEnvironment(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL, e.srv.cfg.AIProxyURL = "https://boat.example", "https://ai.example"
	e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "synthetic-provider-key"})
	status, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"env": map[string]string{"KEEP_ME": "user value"}})
	if status != 201 {
		t.Fatal(status, out)
	}
	vm := out["id"].(string)
	// The fake now follows real boatd's metadata contract too.
	var metadata VM
	if err := e.srv.boatd.call(context.Background(), "GET", vmPath(vm), nil, &metadata); err != nil {
		t.Fatal(err)
	}
	if _, leaked := metadata["env"]; leaked {
		t.Fatal("fake metadata still exposes guest env")
	}
	if status, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/snapshots", nil); status != 201 {
		t.Fatal(status, out)
	}
	vars := lastEnvOp(e, vm)
	self, _ := vars["BOAT_API_KEY"].(string)
	if vars["KEEP_ME"] != "user value" || vars["BOAT_ID"] != vm || e.srv.authenticateSelf(self) == nil {
		t.Error("refresh discarded guest env or failed to deliver a working self token when boatd metadata omitted env")
	}
	if status, _ := e.raw(e.owner, "GET", vmPath(vm, "env"), nil); status != 404 {
		t.Fatal("internal env getter was exposed publicly")
	}
}

func TestR16FailedEnvGetterDoesNotWrite(t *testing.T) {
	for _, bad := range []struct {
		name   string
		status int
		body   string
	}{
		{"unsupported", 502, `{"error":"guest agent: unknown op get_env"}`},
		{"missing-vars", 200, `{"ok":true}`},
	} {
		t.Run(bad.name, func(t *testing.T) {
			e := setup(t)
			e.srv.cfg.VMAPIURL = "https://boat.example"
			vm := e.createVM()
			e.boatd.mu.Lock()
			before := len(e.boatd.envOps)
			e.boatd.mu.Unlock()
			e.srv.boatd.hc.Transport = copyRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" && r.URL.Path == vmPath(vm, "env") {
					return &http.Response{StatusCode: bad.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(bad.body)), Request: r}, nil
				}
				return http.DefaultTransport.RoundTrip(r)
			})
			if err := e.srv.refreshCredentials(context.Background(), vm); err == nil {
				t.Error("unreadable getter succeeded")
			}
			e.boatd.mu.Lock()
			defer e.boatd.mu.Unlock()
			if len(e.boatd.envOps) != before {
				t.Error("failed getter overwrote guest env")
			}
		})
	}
}

func TestR16RefreshPreservesUserCredentials(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL, e.srv.cfg.AIProxyURL = "https://boat.example", "https://ai.example"
	for _, provider := range []string{"anthropic", "openai"} {
		e.do(e.owner, "PUT", "/v1/ai/credentials/"+provider, map[string]any{"value": "synthetic-provider-key"})
	}
	status, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"env": map[string]string{"BOAT_API_KEY": "user-owned-key", "CUSTOM": "it's quoted\nsecond line"}})
	if status != 201 {
		t.Fatal(status, out)
	}
	vm := out["id"].(string)
	var guest struct {
		Vars map[string]string `json:"vars"`
	}
	if err := e.srv.boatd.call(context.Background(), "GET", vmPath(vm, "env"), nil, &guest); err != nil {
		t.Fatal(err)
	}
	oldAI := guest.Vars["OPENAI_API_KEY"]
	guest.Vars["ANTHROPIC_API_KEY"], guest.Vars["ANTHROPIC_BASE_URL"] = "user-provider-key", "https://user.example"
	if err := e.srv.boatd.call(context.Background(), "POST", vmPath(vm, "env"), map[string]any{"vars": guest.Vars}, nil); err != nil {
		t.Fatal(err)
	}
	if status, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/snapshots", nil); status != 201 {
		t.Fatal(status, out)
	}
	vars := lastEnvOp(e, vm)
	if vars["BOAT_API_KEY"] != "user-owned-key" || vars["CUSTOM"] != "it's quoted\nsecond line" || vars["BOAT_ID"] != vm || vars["ANTHROPIC_API_KEY"] != "user-provider-key" || vars["ANTHROPIC_BASE_URL"] != "https://user.example" {
		t.Error("refresh changed user-provided credentials or custom vars")
	}
	current, _ := vars["OPENAI_API_KEY"].(string)
	if _, _, ok := e.srv.aiCheckToken(current); !ok || current == oldAI {
		t.Error("managed AI token did not refresh")
	}
}

func TestR16FirstAITokenSurvivesCrashBeforeNextCopy(t *testing.T) {
	e := setup(t)
	e.srv.cfg.AIProxyURL = "https://ai.example"
	if err := e.srv.reconcileCopyCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	vm, team := e.createVM(), e.teamID()
	e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "synthetic-provider-key"})
	oldStore := e.srv.store
	oldStore.wmu.Lock() // prevent the flusher from disguising a non-durable return
	held := true
	unlock := func() {
		if held {
			oldStore.wmu.Unlock()
			held = false
		}
	}
	defer unlock()
	type result struct {
		env map[string]string
		err error
	}
	issued := make(chan result, 1)
	started := make(chan struct{})
	go func() { close(started); env, err := e.srv.aiEnvNewVM(vm, team); issued <- result{env, err} }()
	<-started
	var r result
	early := false
	select {
	case r = <-issued:
		early = true
		t.Error("first managed AI token returned before its row could be persisted")
	case <-time.After(100 * time.Millisecond):
		unlock()
		select {
		case r = <-issued:
		case <-time.After(5 * time.Second):
			t.Fatal("durable first issuance stalled")
		}
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	data, err := os.ReadFile(oldStore.path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted State
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if row := persisted.AIVMs[vm]; row == nil || e.srv.aiToken(vm, row.Gen) != r.env["ANTHROPIC_API_KEY"] {
		t.Error("published AI token has no matching durable generation")
	}
	vars := map[string]string{"BOAT_ID": vm}
	for k, v := range r.env {
		vars[k] = v
	}
	if err := e.srv.boatd.call(context.Background(), "POST", vmPath(vm, "env"), map[string]any{"vars": vars}, nil); err != nil {
		t.Fatal(err)
	}
	// Simulate abrupt process loss, not Close (which would flush the missing row).
	if !early {
		oldStore.wmu.Lock()
		held = true
	}
	oldStore.closed = true
	unlock()
	reopened, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reloaded := &server{store: reopened, keys: e.srv.keys, boatd: e.srv.boatd, cfg: e.srv.cfg}
	if err := reloaded.reconcileCopyCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := reloaded.aiCheckToken(r.env["ANTHROPIC_API_KEY"]); !ok {
		t.Error("durable guest token did not authenticate after reload")
	}
	snap, err := reloaded.captureSnapshot(context.Background(), vm, team, "")
	if err != nil {
		t.Fatal(err)
	}
	e.boatd.mu.Lock()
	captured := e.boatd.copyEnvs[snap["id"].(string)]["ANTHROPIC_API_KEY"]
	e.boatd.mu.Unlock()
	if captured != r.env["ANTHROPIC_API_KEY"] {
		t.Fatal("copy did not capture the crash-surviving guest token")
	}
	if _, _, ok := reloaded.aiCheckToken(captured.(string)); ok {
		t.Error("released copy retained an authenticating source AI token")
	}
	if _, err := reloaded.aiEnv(vm, team, true); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := reloaded.aiCheckToken(captured.(string)); ok {
		t.Error("released copy's first-generation AI token became valid after crash and reissuance")
	}
}
