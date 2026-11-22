package main

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// r36: a start takes the not-ready gate before it reads the VM or the run's
// mark; a start that races a stop re-marks (also when the stop completes after
// the start returned); a prompt re-checks the gate and the mark right before its
// exec; env_pending is state-free; a prompt refresh reads running turns in
// parallel.

func setStartHooks(t *testing.T, pre, post func(s *server, id string)) {
	t.Helper()
	startPreGateHook, startPostReadHook = pre, post
	t.Cleanup(func() { startPreGateHook, startPostReadHook = nil, nil })
}

func setPromptExecHook(t *testing.T, f func(s *server, id string)) {
	t.Helper()
	promptExecHook = f
	t.Cleanup(func() { promptExecHook = nil })
}

// 1: the reviewer's sequence. A plain start {} of a running no_env VM; a stop
// plus reads land after the start has read the VM and the mark; boatd restores
// the VM. The run stays no_env and the next prompt carries no team secret.
func TestR36StopAndReadAfterStartReadKeepNoEnvRun(t *testing.T) {
	e := setup(t)
	id := e.noEnvVM()
	var hit atomic.Bool
	setStartHooks(t, nil, func(s *server, vm string) {
		if vm != id || !hit.CompareAndSwap(false, true) {
			return
		}
		if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil); st != 200 {
			t.Errorf("stop = %d %v", st, out)
		}
		for i := 0; i < 2; i++ {
			e.do(e.owner, "GET", "/v1/vms", nil) // used to drop the mark
		}
	})
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{}); st != 200 {
		t.Fatalf("start = %d %v", st, out)
	}
	if !hit.Load() {
		t.Fatal("the hook never ran")
	}
	if !e.noEnvRunOf(id) {
		t.Fatal("no_env_run = false after the racing stop")
	}
	if st, _ := e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "hi"}); st != 200 {
		t.Fatalf("prompt = %d", st)
	}
	if v := e.lastExecEnv(); v["TEAM_SEC"] != nil {
		t.Fatalf("prompt env = %v, want no team secret", v)
	}
	if e.srv.envPushPending(id) {
		t.Fatal("the gate leaked")
	}
}

// 1b: the start returns before the racing stop's handler wrote "completed": a
// stop requested before boatd answered does not end the run, whenever it lands.
func TestR36StopCompletingAfterStartReturnedKeepsNoEnvRun(t *testing.T) {
	e := setup(t)
	id := e.noEnvVM()
	var op map[string]any
	setStartHooks(t, nil, func(s *server, vm string) {
		if vm == id && op == nil {
			op = s.stops.begin("team", id) // requested now, completed later
		}
	})
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{}); st != 200 {
		t.Fatalf("start = %d %v", st, out)
	}
	if op == nil {
		t.Fatal("the hook never ran")
	}
	e.srv.stops.complete(id, op) // the stop handler finishes after the start answered
	if !e.noEnvRunOf(id) {
		t.Fatal("a stop requested before the start returned ended the run")
	}
	if st, _ := e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "hi"}); st != 200 {
		t.Fatalf("prompt = %d", st)
	}
	if v := e.lastExecEnv(); v["TEAM_SEC"] != nil {
		t.Fatalf("prompt env = %v, want no team secret", v)
	}
	// r46: a later stop does not end no_env either (sticky).
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if !e.noEnvRunOf(id) {
		t.Fatal("a later stop ended the no_env mark")
	}
}

// 1c: the gate is first: the start holds it while it reads the VM, and a stop
// that completed before the start looked ended the run (a new plain run).
func TestR36GateIsTakenBeforeTheReads(t *testing.T) {
	e := setup(t)
	id := e.noEnvVM()
	var preGate, postRead atomic.Bool
	setStartHooks(t, func(s *server, vm string) {
		if vm == id {
			preGate.Store(s.envPushPending(id))
		}
	}, func(s *server, vm string) {
		if vm == id {
			postRead.Store(s.envPushPending(id))
		}
	})
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{}); st != 200 {
		t.Fatalf("start = %d %v", st, out)
	}
	if preGate.Load() || !postRead.Load() {
		t.Fatalf("gate before the hook = %v, after the reads = %v; want false, true", preGate.Load(), postRead.Load())
	}
	// A stop plus a read before the start looked: the VM is stopped, the run ended.
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	setStartHooks(t, func(s *server, vm string) {
		e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
		e.do(e.owner, "GET", "/v1/vms", nil)
	}, nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{}); st != 200 {
		t.Fatalf("start = %d %v", st, out)
	}
	if !e.noEnvRunOf(id) { // r46: sticky, whatever order the stop and the start came in
		t.Fatal("a plain start of a stopped no_env VM dropped the mark")
	}
}

// 2: a prompt that passed refuseNotReady and chose its secrets, then a cold
// start took the gate or marked the run no_env: it is refused before the exec.
func TestR36PromptRechecksBeforeExec(t *testing.T) {
	for name, path := range map[string]string{"prompts": "/prompts", "legacy": "/prompt"} {
		for hook, f := range map[string]func(s *server, id string){
			"gate": func(s *server, id string) { s.markEnvPush(id) },
			"mark": func(s *server, id string) { _ = s.markNoEnvRun(id, true) },
		} {
			t.Run(name+"/"+hook, func(t *testing.T) {
				e := setup(t)
				e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
				id := e.createVM()
				setPromptExecHook(t, func(s *server, vm string) {
					if vm == id {
						f(s, vm)
					}
				})
				st, b := e.raw(e.owner, "POST", "/v1/vms/"+id+path, map[string]any{"prompt": "hi"})
				if st != 409 || !strings.Contains(string(b), "sandbox_not_ready") {
					t.Fatalf("prompt = %d %s, want 409 sandbox_not_ready", st, b)
				}
				if e.sawTeamSecret() {
					t.Fatal("a team secret reached the guest")
				}
			})
		}
	}
	// Control: no race, the secret is carried.
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	id := e.createVM()
	if st, b := e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": "hi"}); st != 202 {
		t.Fatalf("control = %d %s", st, b)
	}
	if !e.sawTeamSecret() {
		t.Fatal("control carried no secret")
	}
}

// 3: env_pending follows the gate in any state.
func TestR36EnvPendingInAnyState(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	release := e.srv.markEnvPush(id)
	_, vm := e.do(e.owner, "GET", "/v1/vms/"+id, nil)
	if vm["state"] == "running" || vm["env_pending"] != true {
		t.Fatalf("stopped VM with the gate = state %v env_pending %v, want not running and true", vm["state"], vm["env_pending"])
	}
	release()
	_, vm = e.do(e.owner, "GET", "/v1/vms/"+id, nil)
	if vm["env_pending"] != false {
		t.Fatalf("env_pending = %v after release, want false", vm["env_pending"])
	}
}

// 4: a refresh reads running turns at most refreshParallel at a time, and every
// turn's result is applied.
func TestR36RefreshPromptsRunsInParallel(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	const n = 20
	for i := 0; i < n; i++ {
		if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": fmt.Sprintf("turn-%02d", i), "agent": "codex", "new": true}); st != 202 {
			t.Fatalf("prompt %d: %d %v", i, st, out)
		}
	}
	e.settle()
	// Put every turn back to running with no session, as a fresh page would find it.
	if err := e.srv.store.Apply(func(st *State) error {
		for _, run := range st.Prompts[id] {
			run.State, run.Exit, run.Finished = "running", nil, nil
		}
		for _, c := range st.Conversations[id] {
			c.Session = ""
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var inflight, peak atomic.Int32
	e.boatd.mu.Lock()
	e.boatd.logsHook = func() {
		cur := inflight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inflight.Add(-1)
	}
	e.boatd.mu.Unlock()
	st, out := e.do(e.owner, "GET", "/v1/vms/"+id+"/prompts", nil)
	if st != 200 {
		t.Fatalf("list = %d %v", st, out)
	}
	list, _ := out["prompts"].([]any)
	if len(list) != n {
		t.Fatalf("prompts = %d, want %d", len(list), n)
	}
	for i, x := range list {
		run := x.(map[string]any)
		if run["state"] != "completed" {
			t.Fatalf("turn %d = %v, want completed", i, run["state"])
		}
		if want := fmt.Sprintf("turn-%02d", i); !strings.Contains(fmt.Sprint(run["prompt"]), want) {
			t.Fatalf("turn %d prompt = %v, want %s (order kept)", i, run["prompt"], want)
		}
	}
	if p := peak.Load(); p < 2 || p > refreshParallel {
		t.Fatalf("peak concurrent guest reads = %d, want 2..%d", p, refreshParallel)
	}
}
