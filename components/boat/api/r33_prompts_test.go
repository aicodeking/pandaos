package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// r33: native prompts are refused (409 sandbox_not_ready) while the VM's
// environment is not applied; prompt refresh is TTL-cached and singleflight;
// poll loops back off.

// countGuestOps puts a counting proxy between boat-api and the fake boatd and
// returns the number of guest agent calls per op.
func countGuestOps(t *testing.T, e *env) func(op string) int {
	t.Helper()
	target, err := url.Parse(e.srv.boatd.base)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	counts := map[string]int{}
	rp := httputil.NewSingleHostReverseProxy(target)
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/agent") {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			for _, op := range []string{"cmd_status", "cmd_logs", "cmd_start", "cmd_signal"} {
				if bytes.Contains(b, []byte(`"`+op+`"`)) {
					mu.Lock()
					counts[op]++
					mu.Unlock()
				}
			}
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(px.Close)
	e.srv.boatd = newBoatd(px.URL)
	return func(op string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[op]
	}
}

func (e *env) finishAllCmds() {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	for _, c := range e.boatd.cmds {
		c.running, c.exit = false, float64(0)
	}
}

func promptState(t *testing.T, e *env, id, pid string) string {
	t.Helper()
	st, out := e.do(e.owner, "GET", "/v1/vms/"+id+"/prompts/"+pid, nil)
	if st != 200 {
		t.Fatalf("get prompt: %d %v", st, out)
	}
	return out["state"].(string)
}

// An env error: native prompt and steer refuse, nothing is started or
// recorded, native exec (the repair channel) still runs; a good retry reopens.
func TestR33NativePromptRefusedWhileEnvError(t *testing.T) {
	e := setup(t)
	e.setEnvFailures(true, false)
	id := e.nativeOf(e.newSandbox(nil))
	for _, path := range []string{"/v1/vms/" + id + "/prompts"} {
		st, out := e.do(e.owner, "POST", path, map[string]any{"prompt": "go", "new": true})
		if st != 409 || nativeErrCode(out) != "sandbox_not_ready" {
			t.Fatalf("native prompt = %d %v, want 409 sandbox_not_ready", st, out)
		}
	}
	e.boatd.mu.Lock()
	started := len(e.boatd.cmds)
	e.boatd.mu.Unlock()
	if ps := e.srv.store.Snapshot().Prompts[id]; len(ps) != 0 || started != 0 {
		t.Fatalf("refused prompt left %d records, %d guest commands", len(ps), started)
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/commands", map[string]any{"cmd": []string{"echo", "fix"}}); st != 201 {
		t.Fatalf("native exec during env error = %d %v, want it allowed", st, out)
	}
	e.setEnvFailures(false, false)
	e.ok("POST", "/api/v1/sandboxes/"+id+"/resume", nil, 202)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": "go", "new": true}); st != 202 {
		t.Fatalf("native prompt after the retry = %d %v", st, out)
	}
}

// A steer is refused too, before the running turn is interrupted.
func TestR33NativeSteerRefusedWhileEnvError(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	_, run := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": "sleep-forever", "new": true})
	pid := run["id"].(string)
	e.srv.store.Update(func(st *State) error { st.VMEnvErrors[id] = "boom"; return nil })
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts/"+pid+"/steer", map[string]any{"prompt": "other"})
	if st != 409 || nativeErrCode(out) != "sandbox_not_ready" {
		t.Fatalf("steer = %d %v, want 409 sandbox_not_ready", st, out)
	}
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	for _, c := range e.boatd.cmds {
		if !c.running || c.signal != nil {
			t.Fatalf("the running turn was interrupted by a refused steer: %+v", c)
		}
	}
}

// A pending env push refuses native prompts the same way.
func TestR33NativePromptRefusedWhileEnvPushPending(t *testing.T) {
	e := setup(t)
	g := e.holdBoatd(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "w"})
	}()
	var id string
	select {
	case id = <-g.hit:
	case <-time.After(5 * time.Second):
		t.Fatal("env push never reached boatd")
	}
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": "go", "new": true})
	if st != 409 || nativeErrCode(out) != "sandbox_not_ready" {
		t.Errorf("native prompt during the push = %d %v, want 409 sandbox_not_ready", st, out)
	}
	close(g.release)
	<-done
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": "go", "new": true}); st != 202 {
		t.Fatalf("native prompt after the push = %d %v", st, out)
	}
}

// Rapid reads share one refresh: guest status calls stay at one per running turn
// for the whole burst, and a read after the TTL sees the turn's end.
func TestR33RapidGETsReuseRefresh(t *testing.T) {
	e := setup(t)
	count := countGuestOps(t, e)
	id := e.createVM()
	_, run := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": "sleep-forever", "new": true})
	pid := run["id"].(string)
	time.Sleep(promptRefreshTTL + 50*time.Millisecond) // the POST's own refresh window is over
	before := count("cmd_status")
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if st, out := e.do(e.owner, "GET", "/v1/vms/"+id+"/prompts", nil); st != 200 {
				t.Errorf("list: %d %v", st, out)
			}
		}()
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		e.do(e.owner, "GET", "/v1/vms/"+id+"/prompts/"+pid, nil)
	}
	if got := count("cmd_status") - before; got > 2 {
		t.Fatalf("%d GETs made %d guest status calls, want at most 2", 2*n, got)
	}
	e.finishAllCmds()
	time.Sleep(promptRefreshTTL + 50*time.Millisecond)
	if s := promptState(t, e, id, pid); s != "completed" {
		t.Fatalf("state after the TTL = %s, want completed", s)
	}
}

// A POST that follows a turn's end never trips over the cache: the finished
// turn no longer blocks the conversation, and a read right after the POST sees
// the new turn.
func TestR33PostSeesFreshState(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	_, first := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": "sleep-forever", "new": true})
	conv := first["conversation_id"].(string)
	e.do(e.owner, "GET", "/v1/vms/"+id+"/prompts", nil) // opens the TTL window with the turn running
	e.finishAllCmds()
	st, second := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": "next", "conversation_id": conv})
	if st != 202 {
		t.Fatalf("POST right after the turn ended = %d %v, want 202 (not 'a turn is running')", st, second)
	}
	st, out := e.do(e.owner, "GET", "/v1/vms/"+id+"/prompts", nil)
	list, _ := out["prompts"].([]any)
	if st != 200 || len(list) != 2 {
		t.Fatalf("list after POST = %d %v, want both turns", st, out)
	}
	if list[0].(map[string]any)["state"] != "completed" {
		t.Errorf("first turn = %v, want completed", list[0])
	}
}

// A forced caller waits for the refresh in flight and then does its own;
// concurrent plain callers share one.
func TestR33SingleflightAndForce(t *testing.T) {
	ps := &promptRefreshSet{m: map[promptRefreshKey]*promptRefresh{}}
	s := &server{}
	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/", nil)
	var calls atomic.Int32
	gate := make(chan struct{})
	fn := func(*http.Request, string) {
		calls.Add(1)
		<-gate
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ps.run(s, r, "vm", refreshTTL, fn) }()
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("8 concurrent refreshes ran fn %d times, want 1", n)
	}
	ps.run(s, r, "vm", refreshTTL, fn)
	if calls.Load() != 1 {
		t.Fatal("a refresh inside the TTL ran again")
	}
	ps.run(s, r, "vm", refreshForce, fn)
	if calls.Load() != 2 {
		t.Fatal("a forced refresh was skipped")
	}
	ps.forget(s, "vm")
	ps.run(s, r, "vm", refreshTTL, fn)
	if calls.Load() != 3 {
		t.Fatal("a refresh after forget was skipped")
	}
}

// Join mode (compat reads) shares a refresh in flight but never reuses a
// finished one: a lone caller always sees the guest's current state.
func TestR33JoinModeNeverServesStale(t *testing.T) {
	ps := &promptRefreshSet{m: map[promptRefreshKey]*promptRefresh{}}
	s := &server{}
	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/", nil)
	var calls atomic.Int32
	fn := func(*http.Request, string) { calls.Add(1) }
	for i := 1; i <= 3; i++ {
		ps.run(s, r, "vm", refreshJoin, fn)
		if int(calls.Load()) != i {
			t.Fatalf("sequential join #%d ran fn %d times total, want %d", i, calls.Load(), i)
		}
	}
}

func TestR33BackoffDoublesAndStaysBounded(t *testing.T) {
	b := newBackoff(10*time.Millisecond, 40*time.Millisecond, 200*time.Millisecond)
	var got []time.Duration
	for i := 0; i < 4; i++ {
		got = append(got, b.next)
		b.next = min(b.next*2, b.max)
	}
	if got[0] != 10*time.Millisecond || got[1] != 20*time.Millisecond || got[2] != 40*time.Millisecond || got[3] != 40*time.Millisecond {
		t.Fatalf("steps = %v, want 10,20,40,40ms", got)
	}
	b = newBackoff(10*time.Millisecond, 40*time.Millisecond, 100*time.Millisecond)
	t0 := time.Now()
	n := 0
	for b.sleep(context.Background()) {
		n++
	}
	if took := time.Since(t0); took < 90*time.Millisecond || took > 400*time.Millisecond || n < 3 {
		t.Fatalf("loop ran %d steps in %v, want the 100ms total", n, took)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if newBackoff(time.Second, time.Second, time.Minute).sleep(ctx) {
		t.Fatal("sleep ignored a cancelled context")
	}
}
