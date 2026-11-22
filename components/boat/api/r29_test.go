package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func (e *env) setReleaseEnvFail(on bool) {
	e.boatd.mu.Lock()
	e.boatd.releaseEnvFail = on
	e.boatd.mu.Unlock()
}

// A fork or clone whose release-time set_env failed is running but not ready:
// the body says so, the failure is persisted like create's, and the compat
// sandbox is in state "error".
func TestR29ForkEnvFailure(t *testing.T) {
	for _, route := range []string{"fork", "clone"} {
		t.Run(route, func(t *testing.T) {
			e := setup(t)
			parent := e.createVM()
			e.setReleaseEnvFail(true)
			st, child := e.do(e.owner, "POST", "/v1/vms/"+parent+"/"+route, map[string]any{"name": "kid"})
			if st != 201 {
				t.Fatalf("%s: %d %v", route, st, child)
			}
			if child["agent_ready"] != false || child["env_error"] == nil {
				t.Fatalf("%s body = %v, want agent_ready=false with env_error", route, child)
			}
			cid, _ := child["id"].(string)
			if e.srv.store.Snapshot().VMEnvErrors[cid] == "" {
				t.Fatal("fork env failure not persisted")
			}
			_, got := e.do(e.owner, "GET", "/v1/vms/"+cid, nil)
			if got["agent_ready"] != false || got["env_error"] == nil {
				t.Fatalf("GET after %s = %v", route, got)
			}
			// A fork whose env was applied stays clean.
			e.setReleaseEnvFail(false)
			st, ok := e.do(e.owner, "POST", "/v1/vms/"+parent+"/"+route, map[string]any{"name": "good"})
			if st != 201 || ok["env_error"] != nil || ok["agent_ready"] == false {
				t.Fatalf("clean %s = %d %v", route, st, ok)
			}
		})
	}
}

func TestR29CompatForkEnvFailure(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	e.setReleaseEnvFail(true)
	out := e.ok("POST", "/api/v1/sandboxes/"+bx+"/fork", map[string]any{"name": "kid"}, 202)
	sb, _ := out["sandbox"].(map[string]any)
	if sb["state"] != "error" || sb["error"] == nil || out["status"] != "error" {
		t.Fatalf("compat fork = %v, want state/status error", out)
	}
	got := e.ok("GET", "/api/v1/sandboxes/"+sb["id"].(string), nil, 200)["sandbox"].(map[string]any)
	if got["state"] != "error" {
		t.Fatalf("compat get of the fork = %v", got)
	}
}

// Starting an already-running VM must not hide a persisted env failure: while
// the push keeps failing the answer still carries it, on the native route and
// through compat resume (r30: a working push is retried and clears it).
func TestR29StartRunningKeepsEnvError(t *testing.T) {
	e := setup(t)
	e.setEnvFailures(true, false)
	bx := e.newSandbox(map[string]any{"env": map[string]string{"A": "1"}})
	id := e.nativeOf(bx)
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil)
	if st != 200 || out["agent_ready"] != false || out["env_error"] == nil {
		t.Fatalf("native start of a running VM = %d %v", st, out)
	}
	cst, cout, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/resume", nil, nil)
	sb, _ := cout["sandbox"].(map[string]any)
	if cst != 202 || sb["state"] != "error" {
		t.Fatalf("compat resume = %d %v", cst, cout)
	}
}

// A date-only `to` is the end of that UTC day (the next midnight), so
// from==to is one whole day.
func TestR29UsageDateOnlyToEndsDay(t *testing.T) {
	e := setup(t)
	st, out := e.do(e.owner, "GET", "/v1/usage?from=2026-01-02&to=2026-01-02", nil)
	if st != 200 || out["since"] != "2026-01-02T00:00:00Z" || out["until"] != "2026-01-03T00:00:00Z" {
		t.Fatalf("one-day window = %d %v", st, out)
	}
	st, out = e.do(e.owner, "GET", "/v1/usage?from=2026-01-02&to=2026-01-04", nil)
	if st != 200 || out["until"] != "2026-01-05T00:00:00Z" {
		t.Fatalf("multi-day window = %d %v", st, out)
	}
	// An RFC 3339 `to` is exact and exclusive of nothing extra.
	st, out = e.do(e.owner, "GET", "/v1/usage?from=2026-01-02&to=2026-01-02T12:00:00Z", nil)
	if st != 200 || out["until"] != "2026-01-02T12:00:00Z" {
		t.Fatalf("RFC 3339 to = %d %v", st, out)
	}
	if st, out = e.do(e.owner, "GET", "/v1/usage?from=2026-01-03&to=2026-01-02", nil); st != 400 {
		t.Fatalf("reversed window = %d %v", st, out)
	}
}

// Minting a file URL rechecks authority from the snapshot: it must not queue
// behind the store's writer mutex, which Update holds across its fsync.
func TestR29MintDoesNotWaitForStoreWriter(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	e.ok("PUT", "/api/v1/sandboxes/"+bx+"/files", map[string]any{"path": "a.txt", "content": "x"}, 200)
	e.srv.store.mu.Lock() // an Update stuck in its fsync
	locked := true
	unlock := func() {
		if locked {
			locked = false
			e.srv.store.mu.Unlock()
		}
	}
	defer unlock()
	done := make(chan int, 1)
	go func() {
		st, _ := e.do(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/files/download-url", map[string]any{"path": "a.txt"})
		done <- st
	}()
	select {
	case st := <-done:
		if st != 200 && st != 201 {
			t.Fatalf("mint = %d", st)
		}
	case <-time.After(3 * time.Second):
		unlock()
		<-done
		t.Fatal("mint blocked behind the store writer mutex")
	}
}

// An open AI proxy stream ends when its token generation moves on.
func TestR29AIStreamEndsWhenTokenRotates(t *testing.T) {
	e, up, proxy := aiSetup(t)
	e.srv.streamRecheck = 50 * time.Millisecond
	if st, c := e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "sk-ant-REAL-KEY-123456"}); st != 200 {
		t.Fatalf("credential: %d %v", st, c)
	}
	id := e.createVM()
	up.mu.Lock()
	up.reply = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for i := 0; ; i++ {
			if _, err := fmt.Fprintf(w, "data: {\"n\":%d}\n\n", i); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	up.mu.Unlock()
	tok := e.srv.aiToken(id, e.srv.store.Snapshot().AIVMs[id].Gen)
	req, _ := http.NewRequest("POST", proxy.URL+"/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
	req.Header.Set("X-Api-Key", tok)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stream: %d", resp.StatusCode)
	}
	ended := make(chan struct{})
	go func() {
		io.Copy(io.Discard, resp.Body)
		close(ended)
	}()
	expectOpen(t, "AI stream before rotation", ended, 400*time.Millisecond)
	if _, err := e.srv.aiBumpVM(id, e.srv.store.Snapshot().AIVMs[id].TeamID, true); err != nil {
		t.Fatal(err)
	}
	expectEnds(t, "AI stream after the token rotated", ended, 3*time.Second)
}

// A stream also ends when the VM is retired.
func TestR29AIStreamEndsWhenVMRetired(t *testing.T) {
	e, up, proxy := aiSetup(t)
	e.srv.streamRecheck = 50 * time.Millisecond
	e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "sk-ant-REAL-KEY-123456"})
	id := e.createVM()
	up.mu.Lock()
	up.reply = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		for {
			if _, err := io.WriteString(w, "data: x\n\n"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	up.mu.Unlock()
	tok := e.srv.aiToken(id, e.srv.store.Snapshot().AIVMs[id].Gen)
	req, _ := http.NewRequest("POST", proxy.URL+"/anthropic/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("X-Api-Key", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	ended := make(chan struct{})
	go func() {
		io.Copy(io.Discard, resp.Body)
		close(ended)
	}()
	expectOpen(t, "AI stream before retirement", ended, 300*time.Millisecond)
	if err := e.srv.store.Update(func(st *State) error { st.cowAIVM(id).Retired = true; return nil }); err != nil {
		t.Fatal(err)
	}
	expectEnds(t, "AI stream after retirement", ended, 3*time.Second)
}

var _ = httptest.NewRecorder
