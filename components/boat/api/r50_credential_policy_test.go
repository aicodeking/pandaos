package main

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestR50ProtectedLineageAndStickySnapshotPolicy(t *testing.T) {
	for _, bornProtected := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-conversion", true: "protected-create"}[bornProtected], func(t *testing.T) {
			e := setup(t)
			var parent string
			if bornProtected {
				parent = e.createVMWith(map[string]any{"no_env": true})
			} else {
				parent = e.createVM()
				e.stopStart(parent, map[string]any{"no_env": true})
			}
			for generation := 0; generation < 2; generation++ {
				st, out := e.do(e.owner, "POST", vmPath(parent, "fork"), map[string]any{})
				if st != 201 {
					t.Fatalf("fork = %d %v", st, out)
				}
				child := out["id"].(string)
				wantFresh := 0
				if !bornProtected {
					wantFresh = 1
				}
				if e.freshReleasesOf(child) != wantFresh {
					t.Fatalf("fresh fork = %d, want %d", e.freshReleasesOf(child), wantFresh)
				}
				names, known := runNamesOf(e.srv.store.Snapshot(), child)
				if !known || len(names) != 0 || !e.srv.noEnvRun(child) {
					t.Fatalf("protected child run = %v/%v", names, known)
				}
				sid := e.r39Snapshot(child, "protected")
				if !e.srv.snapshotSourceNoEnv(sid) || e.srv.snapshotSourceProtected(sid) != bornProtected {
					t.Fatal("sticky policy must be independent of RAM eligibility")
				}
				st, clone := e.createFrom(sid, nil)
				if st != 201 || !e.srv.noEnvRun(clone["id"].(string)) || e.freshCreatesOf(clone["id"].(string)) != wantFresh {
					t.Fatalf("protected clone = %d %v", st, clone)
				}
				parent = child
			}
		})
	}
}

func TestR50ProtectedRunNeedsPositiveInLockEvidence(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	// Missing policy is conservative for injection but cannot certify RAM.
	if noEnv, parentNoEnv := e.srv.forkNoEnv(nil, id, VM{"id": id}); noEnv || parentNoEnv {
		t.Fatal("absence is not a protected run record")
	}
	if e.srv.snapshotRunNoEnv(id, nil) {
		t.Fatal("missing VM is not protected RAM evidence")
	}
	protected := e.createVMWith(map[string]any{"no_env": true})
	e.boatd.mu.Lock()
	vm := fakeVMMetadata(e.boatd.vms[protected])
	e.boatd.mu.Unlock()
	if !e.srv.snapshotRunNoEnv(protected, vm) {
		t.Fatal("protected creation must be eligible")
	}
	unknownVM := VM{"id": protected, "labels": vm["labels"]}
	if noEnv, _ := e.srv.forkNoEnv(nil, protected, unknownVM); !noEnv {
		t.Fatal("missing status must preserve sticky policy")
	}
	if e.srv.snapshotRunNoEnv(protected, unknownVM) {
		t.Fatal("unavailable status cannot certify RAM")
	}
	if err := e.srv.store.Update(func(st *State) error { delete(st.VMSecrets, runNamesKey(protected)); return nil }); err != nil {
		t.Fatal(err)
	}
	if e.srv.snapshotRunNoEnv(protected, vm) {
		t.Fatal("unknown run names cannot certify RAM")
	}
}

func TestR50NamedEnvironmentPlainRemovalColdBoots(t *testing.T) {
	for _, fork := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "fork"}[fork], func(t *testing.T) {
			e := setup(t)
			e.saveEnv("web", map[string]string{"OLD": "v1"}, []string{})
			id := e.createVMWith(map[string]any{"environment": "web"})
			e.do(e.owner, "PUT", "/v1/environments/web", map[string]any{"env": map[string]string{}})
			path := vmPath(id, "fork")
			wantStatus := 201
			if !fork {
				e.do(e.owner, "POST", vmPath(id, "stop"), nil)
				path = vmPath(id, "start")
				wantStatus = 200
			}
			st, out := e.do(e.owner, "POST", path, map[string]any{"environment": "web"})
			if st != wantStatus {
				t.Fatalf("apply = %d %v", st, out)
			}
			result := out["id"].(string)
			fresh := e.freshBootsOf(result)
			if fork {
				fresh = e.freshReleasesOf(result)
			}
			if fresh != 1 || e.guestVar(result, "OLD") != nil {
				t.Fatal("plain variable removal must discard inherited memory")
			}
		})
	}
}

func TestR50DeletedEnvironmentKeepsPinnedResumeVars(t *testing.T) {
	e := setup(t)
	e.saveEnv("retained", map[string]string{}, []string{})
	if code, out := e.do(e.owner, "PUT", "/v1/environments/retained", map[string]any{"is_default": true}); code != 200 {
		t.Fatalf("retained default = %d %v", code, out)
	}
	eid := e.saveEnv("web", map[string]string{"PINNED": "v1"}, []string{})
	id := e.createVMWith(map[string]any{"environment": "web"})
	st, out, _ := e.compat(e.owner, "DELETE", "/api/v1/environments/"+envUUIDOf(t, e, "web"), nil, nil)
	if st != 200 {
		t.Fatalf("delete = %d %v", st, out)
	}
	if e.srv.store.Snapshot().Environments[eid] != nil {
		t.Fatal("environment remains selectable")
	}
	e.stopStart(id, nil)
	if e.guestVar(id, "PINNED") != "v1" {
		t.Fatal("plain resume must retain the deleted environment's pin")
	}
	if st, _ := e.do(e.owner, "POST", vmPath(id, "fork"), map[string]any{"environment": "web"}); st != 404 {
		t.Fatalf("deleted named environment = %d", st)
	}
}

func TestR50CredentialRefreshSerializesWithUpgrade(t *testing.T) {
	e := setup(t)
	e.saveEnv("web", map[string]string{"FOO": "v1"}, []string{})
	id := e.createVMWith(map[string]any{"environment": "web"})
	e.putEnvVar("web", "FOO", "v2")
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var once sync.Once
	e.srv.boatd.hc.Transport = copyRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" && r.URL.Path == vmPath(id, "env") {
			once.Do(func() { close(entered); <-release })
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- e.srv.refreshCredentials(context.Background(), id) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not reach push")
	}
	upgradeDone := make(chan int, 1)
	go func() { st, _ := e.do(e.owner, "POST", "/v1/environments/web/upgrade", nil); upgradeDone <- st }()
	close(release)
	select {
	case err := <-refreshDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh stalled")
	}
	select {
	case st := <-upgradeDone:
		if st != 200 {
			t.Fatal(st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upgrade stalled")
	}
	if e.guestVar(id, "FOO") != "v2" || e.nativeVM(id)["environment_version"] != float64(2) {
		t.Fatal("refresh must not replay an older environment after upgrade")
	}
}

func TestR50ReconcileRechecksTransitionAfterLifecycleLock(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	if err := e.srv.beginCredentialTransition(id, &CredentialTransition{NoEnv: true, Selection: []string{}}); err != nil {
		t.Fatal(err)
	}
	unlock := startLocks.lock(id)
	var releaseOnce sync.Once
	releaseLock := func() { releaseOnce.Do(unlock) }
	defer releaseLock()
	done := make(chan error, 1)
	go func() { done <- e.srv.reconcileCopyCredentials(context.Background()) }()
	waitR50LifecycleWaiter(t, id)
	if err := e.srv.finishCredentialTransition(id); err != nil {
		releaseLock()
		t.Fatal(err)
	}
	releaseLock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reconcile stalled")
	}
	if e.nativeVM(id)["state"] != "running" {
		t.Fatal("a completed transition must not be stopped from the stale list")
	}
}

func waitR50LifecycleWaiter(t *testing.T, id string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		startLocks.mu.Lock()
		lock := startLocks.m[id]
		waiting := lock != nil && lock.refs >= 2
		startLocks.mu.Unlock()
		if waiting {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("lifecycle operation did not wait for its lock")
		}
	}
}

func TestR50ReconcileMissingTransitionVM(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	id := e.createVM()
	if err := e.srv.beginCredentialTransition(id, &CredentialTransition{NoEnv: true, Selection: []string{}}); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.boatd.call(context.Background(), "POST", vmPath(id, "delete"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.reconcileCopyCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tok := e.srv.store.Snapshot().VMTokens[id]; tok == nil || !tok.Retired {
		t.Fatal("missing VM credentials must retire")
	}
}

func TestR50ProtectedSnapshotRequiresConsistentRecords(t *testing.T) {
	e := setup(t)
	for _, tc := range []struct {
		name      string
		rec, cert []string
		want      bool
	}{
		{"missing source", nil, []string{snapSourceVM + "source"}, false},
		{"missing certificate", []string{snapSourceVM + "source", snapNoEnv}, nil, false},
		{"matching", []string{snapSourceVM + "source", snapNoEnv}, []string{snapSourceVM + "source"}, true},
		{"mismatch", []string{snapSourceVM + "source", snapNoEnv}, []string{snapSourceVM + "other"}, false},
		{"mixed names", []string{snapSourceVM + "source", snapNoEnv, "PLAIN"}, []string{snapSourceVM + "source"}, false},
		{"import", []string{snapSourceVM, snapNoEnv}, []string{snapSourceVM}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := e.srv.store.Update(func(st *State) error {
				st.VMSecrets[snapSourceKey("record")] = tc.rec
				st.VMSecrets[snapProtectedKey("record")] = tc.cert
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if got := e.srv.snapshotSourceProtected("record"); got != tc.want {
				t.Fatalf("eligibility = %v, want %v", got, tc.want)
			}
		})
	}
}
