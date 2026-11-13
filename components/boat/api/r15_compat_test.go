package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestR15IdempotencyPurgesBeforeAdmission(t *testing.T) {
	e := setup(t)
	now := time.Now().UTC()
	e.srv.idem.now = func() time.Time { return now }
	if err := e.srv.store.Update(func(st *State) error {
		for i := 0; i < maxIdemEntries; i++ {
			st.Idempotency[fmt.Sprint(i)] = &IdemRecord{Hash: "old", At: now.Add(-idemTTL - time.Second)}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rec, code := e.srv.idem.begin("new", "h"); rec != nil || code != "" {
		t.Fatalf("expired full cache blocked admission: %v %q", rec, code)
	}
	if len(e.srv.store.Snapshot().Idempotency) != 0 {
		t.Error("expired entries were not purged")
	}
	if _, code := e.srv.idem.begin("new", "h"); code != "idempotency_in_progress" {
		t.Errorf("pending claim lost: %q", code)
	}
	if _, code := e.srv.idem.begin("new", "changed"); code != "idempotency_key_reused" {
		t.Errorf("pending body mismatch lost: %q", code)
	}
}

func TestR15IdempotencyKeepsLiveAndPendingEntries(t *testing.T) {
	e := setup(t)
	now := time.Now().UTC()
	e.srv.idem.now = func() time.Time { return now }
	if err := e.srv.store.Update(func(st *State) error {
		for i := 0; i < maxIdemEntries-1; i++ {
			st.Idempotency[fmt.Sprint(i)] = &IdemRecord{Hash: "live", At: now.Add(-idemTTL)}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, code := e.srv.idem.begin("pending", "h"); code != "" {
		t.Fatal(code)
	}
	if _, code := e.srv.idem.begin("overflow", "h"); code != "rate_limited" {
		t.Errorf("capacity ignored a pending entry: %q", code)
	}
	if rec, code := e.srv.idem.begin("0", "live"); rec == nil || code != "" {
		t.Errorf("live replay lost at capacity: %v %q", rec, code)
	}
}

func TestR15CompatForkRejectsTypeWithoutLeakingChild(t *testing.T) {
	for _, typ := range []string{"../../invalid", "missing-size"} {
		t.Run(typ, func(t *testing.T) {
			e := setup(t)
			bx := e.newSandbox(nil)
			parent := e.nativeOf(bx)
			st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/fork", map[string]any{"type": typ}, nil)
			if st != 400 {
				t.Fatalf("invalid type: %d %v", st, out)
			}
			e.boatd.mu.Lock()
			defer e.boatd.mu.Unlock()
			if len(e.boatd.vms) != 1 || e.boatd.vms[parent] == nil || e.boatd.vms[parent]["state"] != "running" {
				t.Errorf("rejected fork left child or changed parent: %v", e.boatd.vms)
			}
			if strings.Contains(typ, "/") && len(e.boatd.forks) != 0 {
				t.Error("syntactically invalid type reached fork")
			}
			if len(e.srv.store.Snapshot().VMTokens) > 1 || len(e.srv.store.Snapshot().CompatIDs) != 1 {
				t.Error("rejected fork left child API state")
			}
		})
	}
}

func TestR15CompatForkReportsRollbackFailure(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	parent := e.nativeOf(bx)
	e.boatd.mu.Lock()
	e.boatd.cleanupErr = true
	e.boatd.mu.Unlock()
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/fork", map[string]any{"type": "missing-size"}, nil)
	if st != 500 || errCode(out) != "cleanup_failed" {
		t.Fatalf("rollback failure hidden: %d %v", st, out)
	}
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	if e.boatd.vms[parent]["state"] != "running" {
		t.Error("rollback touched parent")
	}
}
