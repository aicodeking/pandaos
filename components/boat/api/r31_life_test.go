package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// r31Gate holds the fake boatd's env pushes / starts until released.
type r31Gate struct {
	hit     chan string // a held request announces its VM here
	release chan struct{}
	failEnv atomic.Bool // held env pushes then answer 502
}

// holdBoatd makes the fake boatd hold every env push (envs) or start (!envs)
// until gate.release is closed.
func (e *env) holdBoatd(envs bool) *r31Gate {
	g := &r31Gate{hit: make(chan string, 8), release: make(chan struct{})}
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	if envs {
		e.boatd.envHook = func(id string) bool {
			g.hit <- id
			<-g.release
			return g.failEnv.Load()
		}
	} else {
		e.boatd.startHook = func(id string) {
			g.hit <- id
			<-g.release
		}
	}
	return g
}

// 1: a start with env (or no_env) on a healthy running VM whose push fails is a
// 502 and nothing else: the guest kept its previous env file, so no env_error, no
// unbilled span, no gated commands, and the no_env mark is as it was.
func TestR31HealthyRunningVMEnvUpdateFailureChangesNothing(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	id := e.nativeOf(bx)
	// r38: no_env on a running VM that is not a no_env run is refused before any push.
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != http.StatusConflict || nativeErrCode(out) != "stop_required" {
		t.Fatalf("no_env on a running secrets VM = %d %v, want 409 stop_required", st, out)
	}
	e.setEnvFailures(true, false)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"env": map[string]string{"A": "1"}}); st != http.StatusBadGateway || nativeErrCode(out) != "env_update_failed" {
		t.Fatalf("env: start = %d %v, want 502 env_update_failed", st, out)
	}
	_, vm := e.do(e.owner, "GET", "/v1/vms/"+id, nil)
	if vm["env_error"] != nil || vm["agent_ready"] == false {
		t.Fatalf("GET after failed updates = %v, want no env_error", vm)
	}
	if sp := e.spans(id); len(sp) != 0 {
		t.Fatalf("env-error spans = %v, want none (the VM is billed as before)", sp)
	}
	if _, ok := e.srv.store.Snapshot().NoEnvRuns[id]; ok {
		t.Fatal("a failed no_env update left the no_env mark")
	}
	sb := e.ok("GET", "/api/v1/sandboxes/"+bx, nil, 200)["sandbox"].(map[string]any)
	if sb["state"] == "error" {
		t.Fatalf("compat state = %v, want not error", sb)
	}
	if st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/commands", map[string]any{"command": "echo hi"}, nil); st != 200 {
		t.Fatalf("compat command = %d %v, want it not gated", st, out)
	}

	// An existing mark is restored as it was, not dropped.
	e.setEnvFailures(false, false)
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true})
	before, ok := e.srv.store.Snapshot().NoEnvRuns[id]
	if !ok {
		t.Fatal("no_env cold start left no mark")
	}
	e.setEnvFailures(true, false)
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != http.StatusBadGateway {
		t.Fatalf("failing no_env update = %d", st)
	}
	if after := e.srv.store.Snapshot().NoEnvRuns[id]; !after.Equal(before) {
		t.Fatalf("mark %v -> %v, want unchanged", before, after)
	}
	// A successful no_env update on a healthy VM marks it.
	e.setEnvFailures(false, false)
	id2 := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+id2+"/stop", nil) // r38: a no_env run needs a cold start
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id2+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("no_env update = %d", st)
	}
	if _, ok := e.srv.store.Snapshot().NoEnvRuns[id2]; !ok {
		t.Fatal("a successful no_env update did not mark the run")
	}
}

// 1 (control): a VM already in env error stays in it when a push fails again.
func TestR31FailedRetryKeepsEnvError(t *testing.T) {
	e := setup(t)
	e.setEnvFailures(true, false)
	id := e.nativeOf(e.newSandbox(nil))
	if _, vm := e.do(e.owner, "GET", "/v1/vms/"+id, nil); vm["env_error"] == nil {
		t.Fatalf("setup: no env_error: %v", vm)
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"env": map[string]string{"A": "1"}}); st != http.StatusBadGateway {
		t.Fatalf("failing update = %d %v", st, out)
	}
	if _, vm := e.do(e.owner, "GET", "/v1/vms/"+id, nil); vm["env_error"] == nil {
		t.Fatalf("env_error lost: %v", vm)
	}
	if sp := e.spans(id); len(sp) != 1 || !sp[0].End.IsZero() {
		t.Fatalf("spans = %v, want the one open span", sp)
	}
}

// 4: a start carrying env on an already-running no_env VM pushes no team secrets
// and keeps the mark.
func TestR31RunningNoEnvVMUpdateHoldsNoTeamSecrets(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	id := e.createVM()
	// Control: a normal running VM's update carries the team secrets.
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"env": map[string]string{"X": "1"}}); st != 200 {
		t.Fatalf("control update = %d", st)
	}
	if v := e.lastEnvVars(); v["X"] != "1" || v["TEAM_SEC"] != "s3" {
		t.Fatalf("control env = %v, want X and TEAM_SEC", v)
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("no_env cold start = %d", st)
	}
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"env": map[string]string{"X": "2"}}); st != 200 {
		t.Fatalf("update of the no_env run = %d", st)
	}
	v := e.lastEnvVars()
	if v["X"] != "2" || v["BOAT_ID"] != id || v["TEAM_SEC"] != nil {
		t.Fatalf("no_env run update env = %v, want X, BOAT_ID, no team secrets", v)
	}
	if _, ok := e.srv.store.Snapshot().NoEnvRuns[id]; !ok {
		t.Fatal("the update dropped the no_env mark")
	}
}

// 5: a normal start clears the no_env mark only if it cold-started the VM. One
// that waits behind a no_env cold start finds the VM running and leaves the mark.
func TestR31NormalStartBehindNoEnvStartKeepsMark(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	g := e.holdBoatd(false)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true})
	}()
	select {
	case <-g.hit:
	case <-time.After(5 * time.Second):
		t.Fatal("no_env start never reached boatd")
	}
	go func() {
		defer wg.Done()
		e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil)
	}()
	time.Sleep(150 * time.Millisecond) // the normal start waits for the lock
	close(g.release)
	wg.Wait()
	if _, ok := e.srv.store.Snapshot().NoEnvRuns[id]; !ok {
		t.Fatal("a normal start that did not boot the VM cleared the no_env run's mark")
	}
}

// 3 (superseded by r32): /limits counts a running env-error sandbox too, as
// boatd's admission does; only the stopped one is free.
func TestR31LimitsCountEnvErrorSandboxes(t *testing.T) {
	e := setup(t)
	good := e.newSandbox(nil)
	e.setEnvFailures(true, false)
	e.newSandbox(nil) // running, env error: state "error"
	e.setEnvFailures(false, false)
	stopped := e.nativeOf(e.newSandbox(nil))
	e.do(e.owner, "POST", "/v1/vms/"+stopped+"/stop", nil)
	lim := e.ok("GET", "/api/v1/limits", nil, 200)
	if n, _ := lim["activeSandboxes"].(float64); n != 2 {
		t.Fatalf("limits = %v, want 2 active: %s and the env-error one hold slots", lim, good)
	}
}

// 2: sandbox.ready is sent once the env push succeeded, not when boatd says the
// VM runs; a failed push is sandbox.error and no ready.
type r31Hook struct {
	mu   sync.Mutex
	got  []map[string]any
	srv  *httptest.Server
	cncl context.CancelFunc
}

func (e *env) r31Hook(events ...string) *r31Hook {
	h := &r31Hook{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		h.mu.Lock()
		h.got = append(h.got, m)
		h.mu.Unlock()
		w.WriteHeader(204)
	}))
	e.t.Cleanup(h.srv.Close)
	e.ok("POST", "/api/v1/webhooks", map[string]any{"name": "h", "url": h.srv.URL, "events": events}, 201)
	ctx, cancel := context.WithCancel(context.Background())
	h.cncl = cancel
	e.t.Cleanup(cancel)
	go e.srv.hooks.watch(ctx)
	return h
}

func (h *r31Hook) types() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, m := range h.got {
		out = append(out, m["type"].(string))
	}
	return out
}

func (h *r31Hook) last() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.got[len(h.got)-1]
}

// runningEvent sends boatd's vm.state running event for the only VM and waits
// until the dispatcher has handled it (held, or delivered).
func (e *env) runningEvent(id string) {
	e.boatd.events <- `{"type":"vm.state","vm_id":"` + id + `","team":"` + e.teamID() + `","state":"running","prev":"creating"}`
}

func (e *env) gateHolds(id string) bool {
	g := &e.srv.hooks.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held[id] != nil
}

func TestR31ReadyWaitsForEnvPush(t *testing.T) {
	e := setup(t)
	h := e.r31Hook("sandbox.ready", "sandbox.error")
	g := e.holdBoatd(true)
	done := make(chan map[string]any, 1)
	go func() {
		_, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "w"})
		done <- out
	}()
	select {
	case <-g.hit:
	case <-time.After(5 * time.Second):
		t.Fatal("env push never started")
	}
	id := "vm1"
	e.runningEvent(id) // boatd: running, before the env landed
	waitFor(t, "running event held", func() bool { return e.gateHolds(id) })
	time.Sleep(100 * time.Millisecond)
	if got := h.types(); len(got) != 0 {
		t.Fatalf("delivered %v before the env push finished", got)
	}
	close(g.release)
	<-done
	waitFor(t, "sandbox.ready", func() bool { return len(h.types()) == 1 })
	if h.types()[0] != "sandbox.ready" {
		t.Fatalf("got %v, want sandbox.ready", h.types())
	}
	sb := h.last()["data"].(map[string]any)["sandbox"].(map[string]any)
	if sb["id"] != e.srv.compatID(e.teamID(), id) {
		t.Fatalf("sandbox id = %v", sb)
	}
}

func TestR31FailedEnvPushIsSandboxErrorNotReady(t *testing.T) {
	e := setup(t)
	h := e.r31Hook("sandbox.ready", "sandbox.error")
	g := e.holdBoatd(true)
	g.failEnv.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "w"})
	}()
	<-g.hit
	e.runningEvent("vm1")
	waitFor(t, "running event held", func() bool { return e.gateHolds("vm1") })
	close(g.release)
	<-done
	waitFor(t, "sandbox.error", func() bool { return len(h.types()) >= 1 })
	time.Sleep(100 * time.Millisecond)
	if got := h.types(); len(got) != 1 || got[0] != "sandbox.error" {
		t.Fatalf("got %v, want exactly sandbox.error", got)
	}
	d := h.last()["data"].(map[string]any)
	if d["error"] != envErrorMessage || d["state"] != "error" || d["sandbox"].(map[string]any)["id"] != e.srv.compatID(e.teamID(), "vm1") {
		t.Fatalf("sandbox.error data = %v", d)
	}
	// A running event that arrives after the failure is not a ready either.
	e.runningEvent("vm1")
	time.Sleep(150 * time.Millisecond)
	if got := h.types(); len(got) != 1 {
		t.Fatalf("late running event delivered %v", got)
	}
}

// A running event with no env push in flight is sandbox.ready at once.
func TestR31ReadyWithoutPendingPushIsImmediate(t *testing.T) {
	e := setup(t)
	h := e.r31Hook("sandbox.ready")
	id := e.createVM()
	e.runningEvent(id)
	waitFor(t, "sandbox.ready", func() bool { return len(h.types()) == 1 })
}

// A retry that clears an env error is a sandbox.ready; a failed update of a
// healthy running VM sends nothing.
func TestR31RetryRecoveryReadyAndHealthyFailureSilent(t *testing.T) {
	e := setup(t)
	h := e.r31Hook("sandbox.ready", "sandbox.error")
	healthy := e.createVM()
	e.setEnvFailures(true, false)
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+healthy+"/start", map[string]any{"env": map[string]string{"A": "1"}}); st != http.StatusBadGateway {
		t.Fatalf("healthy failing update = %d", st)
	}
	broken := e.nativeOf(e.newSandbox(nil)) // env push fails: running with env error
	waitFor(t, "sandbox.error for the failed create", func() bool { return len(h.types()) == 1 })
	e.setEnvFailures(false, false)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+broken+"/start", nil); st != 200 || out["env_error"] != nil {
		t.Fatalf("retry = %d %v", st, out)
	}
	waitFor(t, "recovery ready", func() bool { return len(h.types()) == 2 })
	if got := h.types(); got[0] != "sandbox.error" || got[1] != "sandbox.ready" {
		t.Fatalf("events %v, want error then ready", got)
	}
}

// 6: a usage report looks up the VMs with an open span with one boatd list call,
// not a GET per VM.
func TestR31UsageReportListsVMsOnce(t *testing.T) {
	e := setup(t)
	now := time.Now()
	var lists, gets atomic.Int32
	bd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/usage":
			team := r.URL.Query().Get("team")
			rows := []any{}
			for _, id := range []string{"vmA", "vmB", "vmC"} {
				rows = append(rows, map[string]any{"id": id, "team": team, "size": "small", "vm_seconds": 7200.0})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"vms": rows,
				"teams": []any{map[string]any{"team": team, "vm_seconds": 21600.0, "vm_seconds_by_size": map[string]any{"small": 21600.0}}}})
		case r.URL.Path == "/v1/vms":
			lists.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"vms": []any{
				map[string]any{"id": "vmA", "state": "running"},
				map[string]any{"id": "vmB", "state": "stopped", "updated_at": now.Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)},
			}})
		default:
			gets.Add(1)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(bd.Close)
	e.srv.boatd = newBoatd(bd.URL)
	for _, id := range []string{"vmA", "vmB", "vmC"} {
		seedSpans(e, id, EnvErrorSpan{Start: now.Add(-3 * time.Hour)})
	}
	_, out := e.do(e.owner, "GET", "/v1/usage?since="+url_(now.Add(-4*time.Hour)), nil)
	if lists.Load() != 1 || gets.Load() != 0 {
		t.Fatalf("boatd calls: %d lists, %d gets; want 1 and 0", lists.Load(), gets.Load())
	}
	secs := func(i int) float64 { return out["vms"].([]any)[i].(map[string]any)["vm_seconds"].(float64) }
	// vmA runs: its 3h span to now (clamped to its 2h); vmB stopped 2h ago: 1h of span; vmC unknown: to now.
	if a, b, c := secs(0), secs(1), secs(2); a != 0 || b < 3600-30 || b > 3600+30 || c != 0 {
		t.Fatalf("vm_seconds = %v %v %v", a, b, c)
	}
}
