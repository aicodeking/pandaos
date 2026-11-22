package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// r40: a retype whose no_env start failed closed but whose previous run was
// restored answers the live sandbox; a fork with type that deletes its child names
// no sandbox; the AI meter does not resurrect a deleted team's rows; AI request
// bodies are charged twice.

// failOnlyFirstStartEnv makes the next start's env push fail (a no_env cold start
// that fails closed) and every start after it succeed.
func (e *env) failOnlyFirstStartEnv() {
	n := 0
	e.boatd.mu.Lock()
	e.boatd.startEnvFail = true
	e.boatd.startHook = func(string) {
		e.boatd.mu.Lock()
		defer e.boatd.mu.Unlock()
		if n++; n >= 2 {
			e.boatd.startEnvFail = false
		}
	}
	e.boatd.mu.Unlock()
}

// 1: the restore start after a failed-closed retype succeeded: 502 env_start_failed
// with the live (ready) sandbox, not the stopped VM of the failed start, and a
// message saying the previous run is running again.
func TestR40RetypeRestoreAnswersLiveSandbox(t *testing.T) {
	// r46: noEnv/type on a running sandbox is a 409 before any stop; the run keeps
	// its secrets and is not marked.
	e := setup(t)
	id := e.runningSecretVM()
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/resume",
		map[string]any{"type": "large", "noEnv": true}, nil)
	if st != http.StatusConflict || out["code"] != "sandbox_running_stop_first" {
		t.Fatalf("resume = %d %v, want 409 sandbox_running_stop_first", st, out)
	}
	if s := e.boatdState(id); s != "running" {
		t.Fatalf("VM = %s, want running", s)
	}
	if e.noEnvRunOf(id) {
		t.Fatal("the refused resume marked the VM no_env")
	}
}

// 1b: if the restore fails too, the answer is unchanged: 500 restore_failed.
func TestR40RetypeRestoreFailedKeepsAnswer(t *testing.T) {
	// r46: with a type or noEnv a running sandbox answers 409, whatever boatd would do.
	e := setup(t)
	id := e.runningSecretVM()
	e.setEnvFailures(false, true)
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/resume",
		map[string]any{"type": "large", "noEnv": true}, nil)
	if st != http.StatusConflict {
		t.Fatalf("resume = %d %v, want 409", st, out)
	}
}

// 2: a fork with type whose retype failed deletes its child: the error names no
// sandbox or vm and says the VM was deleted.
func TestR40ForkTypeDeletedChildNamesNoSandbox(t *testing.T) {
	e := setup(t)
	parent := e.runningSecretVM()
	before := e.boatdVMCount()
	e.failNextStarts(1)
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+parent+"/fork",
		map[string]any{"type": "large", "name": "kid"}, nil)
	if st < 500 {
		t.Fatalf("fork = %d %v, want a failure", st, out)
	}
	if out["sandbox"] != nil || out["vm"] != nil || out["sandboxes"] != nil {
		t.Fatalf("answer names a VM that was deleted: %v", out)
	}
	if msg, _ := out["message"].(string); !strings.Contains(msg, "was deleted") {
		t.Fatalf("message = %q, want it to say the child was deleted", msg)
	}
	if n := e.boatdVMCount(); n != before {
		t.Fatalf("boatd VMs = %d, want %d: the child must be gone", n, before)
	}
}

// 2b: the child's no_env start failed closed and its restore worked: the answer is
// still env_start_failed, says the child was deleted (not that a run is running
// again) and names no sandbox.
func TestR40ForkTypeEnvStartFailedDeletesChild(t *testing.T) {
	e := setup(t)
	parent := e.runningSecretVM()
	before := e.boatdVMCount()
	e.failOnlyFirstStartEnv()
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+parent+"/fork",
		map[string]any{"type": "large", "noEnv": true, "name": "kid"}, nil)
	if st != http.StatusBadGateway || out["code"] != "env_start_failed" {
		t.Fatalf("fork = %d %v, want 502 env_start_failed", st, out)
	}
	if out["sandbox"] != nil || out["vm"] != nil {
		t.Fatalf("answer names a VM that was deleted: %v", out)
	}
	msg, _ := out["message"].(string)
	if !strings.Contains(msg, "was deleted") || strings.Contains(msg, "running again") {
		t.Fatalf("message = %q", msg)
	}
	if n := e.boatdVMCount(); n != before {
		t.Fatalf("boatd VMs = %d, want %d", n, before)
	}
}

// 2c: a child that cannot be deleted is left stopped and named.
func TestR40ForkTypeKeptChildStoppedAndNamed(t *testing.T) {
	e := setup(t)
	parent := e.runningSecretVM()
	e.failNextStarts(1)
	e.boatd.mu.Lock()
	e.boatd.cleanupErr = true
	e.boatd.mu.Unlock()
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+parent+"/fork",
		map[string]any{"type": "large", "name": "kid"}, nil)
	if st != 500 || out["code"] != "cleanup_failed" || out["sandbox"] != nil {
		t.Fatalf("fork = %d %v, want 500 cleanup_failed without sandbox", st, out)
	}
	msg, _ := out["message"].(string)
	var child string
	e.boatd.mu.Lock()
	for id := range e.boatd.vms {
		if id != parent && strings.Contains(msg, id) {
			child = id
		}
	}
	e.boatd.mu.Unlock()
	if child == "" || !strings.Contains(msg, "left stopped") {
		t.Fatalf("message = %q, want the child's id and that it was left stopped", msg)
	}
	if s := e.boatdState(child); s != "stopped" {
		t.Fatalf("kept child = %s, want stopped", s)
	}
}

// 3: a batch applied after its team was deleted adds no rows.
func TestR40MeterFlushSkipsDeletedTeam(t *testing.T) {
	e := setup(t)
	tid, _ := e.newTeam("gone")
	keep := e.teamID()
	if st, out := e.do(e.owner, "DELETE", "/v1/teams/"+tid, nil); st != 200 {
		t.Fatalf("delete team: %d %v", st, out)
	}
	now := time.Now().UTC()
	e.srv.aiMeter.addAt(now, tid, "vmx", "openai", "m", 3, 4) // a request that finished after the delete
	e.srv.aiMeter.addAt(now, keep, "vmk", "openai", "m", 5, 6)
	e.srv.aiMeter.flushFinal(e.srv)
	snap := e.srv.store.Snapshot()
	for _, m := range []map[string]*AIUsageRow{snap.AIUsage, snap.AIUsageDays} {
		for k, r := range m {
			if r.TeamID == tid {
				t.Errorf("a row of the deleted team was resurrected: %s", k)
			}
		}
	}
	if len(snap.AIUsage) != 1 || len(snap.AIUsageDays) != 1 {
		t.Fatalf("live team's rows = %d lifetime, %d per-day, want 1 each", len(snap.AIUsage), len(snap.AIUsageDays))
	}
}

// 4: the default body budgets account for the real memory of a buffered request
// (about 3x its bytes): at the global budget it stays under ~400 MiB, a team's
// share under the global one, and a VM can still hold a maximum body.
func TestR40AIBodyBudgetDefaultsAccountForMemory(t *testing.T) {
	const copies = 3 // what a request holds while it is patched and sent
	if got := copies * aiBudgetGlobal; got > 400<<20 {
		t.Fatalf("global budget %d MiB x %d copies = %d MiB of real memory, want at most 400", aiBudgetGlobal>>20, copies, got>>20)
	}
	if aiBudgetPerTeam > aiBudgetGlobal/2 {
		t.Fatalf("a team may take %d MiB of the %d MiB global budget: one team could take most of it", aiBudgetPerTeam>>20, aiBudgetGlobal>>20)
	}
	if aiBudgetPerVM < aiMaxBody || aiBudgetPerTeam < aiBudgetPerVM {
		t.Fatalf("budgets VM %d team %d: a VM must hold one maximum body (%d) and a team at least a VM's share", aiBudgetPerVM, aiBudgetPerTeam, aiMaxBody)
	}
	// The budget is on received bytes: a body is charged once its buffer capacity.
	l := newInflight(0)
	res := l.reserveTeam("vm1", "t1", 0, 0, 0)
	const n = 100 << 10
	r := httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("a", n)))
	got, err := aiReadBody(httptest.NewRecorder(), r, res)
	if err != nil || len(got) != n || res.got != n {
		t.Fatalf("read %d bytes, charged %d (%v), want %d each", len(got), res.got, err, n)
	}
	res.release()
}
