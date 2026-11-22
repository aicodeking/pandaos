package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// r35: a plain start of a running VM keeps a no_env run through a racing stop,
// the not-ready gate is state-free, the usage tap reads the tail of an oversized
// JSON body, a cold events page reads finished turns in parallel, and the event
// cache replaces a shorter entry.

// 1: the reviewer's interleaving. A no_env run; a plain start {} of the VM read
// as running is held in boatd; a stop completes; a native read in the window must
// not drop the mark; boatd then restores the VM. The run is still no_env and the
// next prompt carries no team secret.
func TestR35PlainStartOfRunningNoEnvVMKeepsMark(t *testing.T) {
	e := setup(t)
	id := e.noEnvVM()
	g := e.holdBoatd(false)
	var once sync.Once
	release := func() { once.Do(func() { close(g.release) }) }
	t.Cleanup(release)
	done := make(chan int, 1)
	go func() {
		st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{})
		done <- st
	}()
	select {
	case <-g.hit:
	case <-time.After(5 * time.Second):
		t.Fatal("the start never reached boatd")
	}
	if !e.srv.envPushPending(id) {
		t.Fatal("a start of a running VM does not hold the gate")
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil); st != 200 {
		t.Fatalf("stop = %d %v", st, out)
	}
	for i := 0; i < 2; i++ { // the window: any noEnvRun call used to drop the mark
		if st, out := e.do(e.owner, "GET", "/v1/vms", nil); st != 200 {
			t.Fatalf("list = %d %v", st, out)
		}
	}
	if _, ok := e.srv.store.Snapshot().NoEnvRuns[id]; !ok {
		t.Fatal("a read during the in-flight start dropped the no_env mark")
	}
	release()
	select {
	case st := <-done:
		if st != 200 {
			t.Fatalf("start = %d", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the start never finished")
	}
	if e.srv.envPushPending(id) {
		t.Fatal("the gate leaked")
	}
	if !e.noEnvRunOf(id) {
		t.Fatal("no_env_run = false after the plain start")
	}
	if st, _ := e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "hi"}); st != 200 {
		t.Fatalf("prompt after the start = %d", st)
	}
	if v := e.lastExecEnv(); v["TEAM_SEC"] != nil {
		t.Fatalf("prompt env = %v, want no team secret", v)
	}
	// r46: no_env is sticky: a stop after the start does not end it.
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if !e.noEnvRunOf(id) {
		t.Fatal("a stop after the start ended the no_env mark")
	}
}

// 1b: a plain start of a running VM whose run is normal stays normal, and a cold
// start without no_env still clears the mark.
func TestR35PlainStartDoesNotInventAMark(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{}); st != 200 {
		t.Fatalf("start = %d", st)
	}
	if e.noEnvRunOf(id) {
		t.Fatal("a plain start of a normal run marked it no_env")
	}
	nid := e.noEnvVM()
	e.do(e.owner, "POST", "/v1/vms/"+nid+"/stop", nil)
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+nid+"/start", map[string]any{}); st != 200 {
		t.Fatalf("cold start = %d", st)
	}
	if !e.noEnvRunOf(nid) { // r46: sticky
		t.Fatal("a cold plain start of a no_env VM dropped the mark")
	}
}

// 2: refuseNotReady refuses whenever an env push is in flight, in any state, and
// keeps the env_error rule.
func TestR35RefuseNotReadyIsStateFree(t *testing.T) {
	e := setup(t)
	call := func(vm VM) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", nil)
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, &reqInfo{id: "r35"}))
		if e.srv.refuseNotReady(w, r, vm) {
			return w.Code
		}
		return 0
	}
	vm := VM{"id": "vmX", "state": "stopped"}
	if got := call(vm); got != 0 {
		t.Fatalf("idle stopped VM refused: %d", got)
	}
	release := e.srv.markEnvPush("vmX")
	for _, state := range []string{"stopped", "running", "starting", ""} {
		if got := call(VM{"id": "vmX", "state": state}); got != http.StatusConflict {
			t.Fatalf("state %q during a start = %d, want 409", state, got)
		}
	}
	release()
	if got := call(vm); got != 0 {
		t.Fatalf("refused after the push settled: %d", got)
	}
	if err := e.srv.store.Update(func(st *State) error { st.VMEnvErrors["vmX"] = "env push failed"; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := call(VM{"id": "vmX", "state": "running"}); got != http.StatusConflict {
		t.Fatalf("env_error VM = %d, want 409", got)
	}
}

// 3: a JSON body over the 8 MiB cap is still metered: model from its head,
// usage from its tail; SSE is unchanged; the work stays linear.
func TestR35UsageTapOversizedJSON(t *testing.T) {
	pad := strings.Repeat("z", 9<<20)
	bodies := map[string]string{
		"compact": `{"model":"gpt-x","pad":"` + pad + `","usage":{"prompt_tokens":7,"completion_tokens":9}}`,
		"pretty": "{\n  \"id\": \"x\",\n  \"model\": \"claude-y\",\n  \"content\": [\n    {\n      \"text\": \"" + pad +
			"\"\n    }\n  ],\n  \"usage\": {\n    \"input_tokens\": 11,\n    \"output_tokens\": 13\n  }\n}\n",
	}
	want := map[string][3]any{"compact": {int64(7), int64(9), "gpt-x"}, "pretty": {int64(11), int64(13), "claude-y"}}
	for name, body := range bodies {
		for _, chunk := range []int{1 << 10, 32 << 10, 1 << 20} {
			tap := &usageTap{}
			start := time.Now()
			for i := 0; i < len(body); i += chunk {
				tap.Write([]byte(body[i:min(i+chunk, len(body))]))
			}
			in, out, model := tap.result()
			if w := want[name]; in != w[0] || out != w[1] || model != w[2] {
				t.Fatalf("%s chunk %d: in=%d out=%d model=%q, want %v", name, chunk, in, out, model, w)
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Fatalf("%s chunk %d took %v for %d bytes: not linear", name, chunk, took, len(body))
			}
			if len(tap.tail) > 2*tapTailBytes || tap.buf.Len() != 0 {
				t.Fatalf("%s chunk %d kept tail=%d head=%d bytes", name, chunk, len(tap.tail), tap.buf.Len())
			}
		}
	}
	// A body just under the cap is parsed whole, as before.
	small := `{"model":"m","usage":{"prompt_tokens":1,"completion_tokens":2}}`
	tap := &usageTap{}
	tap.Write([]byte(small))
	if in, out, model := tap.result(); in != 1 || out != 2 || model != "m" {
		t.Fatalf("small body: %d %d %q", in, out, model)
	}
	// SSE over the cap keeps working: usage in the last line of a long stream.
	tap = &usageTap{}
	line := "data: {\"model\":\"s\",\"delta\":\"" + strings.Repeat("q", 60<<10) + "\"}\n\n"
	for i := 0; i < 200; i++ { // ~12 MiB
		tap.Write([]byte(line))
	}
	tap.Write([]byte("data: {\"model\":\"s\",\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}\n\n"))
	if in, out, model := tap.result(); in != 3 || out != 4 || model != "s" {
		t.Fatalf("long sse: %d %d %q", in, out, model)
	}
}

// 4: a cold events page reads its uncached finished turns eight at a time, keeps
// their order, and the second page is served from the cache.
func TestR35EventsColdPageReadsInParallel(t *testing.T) {
	e := setup(t)
	count := countGuestOps(t, e)
	id := e.createVM()
	const n = 24
	for i := 0; i < n; i++ {
		if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": fmt.Sprintf("turn-%02d", i), "new": true}); st != 202 {
			t.Fatalf("prompt %d: %d %v", i, st, out)
		}
	}
	e.settle()
	e.do(e.owner, "GET", "/v1/vms/"+id+"/prompts", nil)
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
	path := "/api/v1/sandboxes/" + id + "/events?sort=asc&limit=200"
	logs0 := count("cmd_logs")
	start := time.Now()
	all := e.ok("GET", path, nil, 200)["events"].([]any)
	took := time.Since(start)
	prompts := map[string]string{} // task id -> prompt text
	responses := 0
	for _, x := range all {
		ev := x.(map[string]any)
		d := ev["data"].(map[string]any)
		if ev["type"] == "prompt" {
			prompts[ev["taskId"].(string)], _ = d["prompt"].(string)
		}
	}
	for _, x := range all {
		ev := x.(map[string]any)
		if ev["type"] != "response" {
			continue
		}
		responses++
		txt, _ := ev["data"].(map[string]any)["text"].(string)
		if want := prompts[ev["taskId"].(string)]; want == "" || !strings.Contains(txt, want) {
			t.Fatalf("response of %v has text %q, want the output of prompt %q", ev["taskId"], txt, want)
		}
	}
	if responses != n {
		t.Fatalf("response events = %d, want %d", responses, n)
	}
	if got := count("cmd_logs") - logs0; got != n {
		t.Fatalf("cold page made %d log reads, want %d", got, n)
	}
	if p := peak.Load(); p < 2 || p > listRefreshParallel {
		t.Fatalf("peak concurrent log reads = %d, want 2..%d", p, listRefreshParallel)
	}
	t.Logf("cold page of %d turns: %v, peak %d", n, took, peak.Load())
	logs1 := count("cmd_logs")
	if again := e.ok("GET", path, nil, 200)["events"].([]any); len(again) != len(all) || count("cmd_logs") != logs1 {
		t.Fatalf("second page read the guest again: %d events, %d reads", len(again), count("cmd_logs")-logs1)
	}
}

// 5: the event cache replaces an entry when a later read is longer, and keeps
// its byte accounting.
func TestR35EventCachePutReplacesShorter(t *testing.T) {
	c := newEventTextCache()
	k := eventTextKey{vm: "v", cmd: "c"}
	c.put(k, "abc")
	c.put(k, "abcdef")
	if got, _ := c.get(k); got != "abcdef" {
		t.Fatalf("after a longer put: %q", got)
	}
	c.put(k, "ab") // a shorter read never shrinks it
	if got, _ := c.get(k); got != "abcdef" {
		t.Fatalf("after a shorter put: %q", got)
	}
	if c.bytes != 6 || c.ll.Len() != 1 {
		t.Fatalf("bytes=%d entries=%d, want 6 and 1", c.bytes, c.ll.Len())
	}
	c.forget(nil, "v")
	if _, ok := c.get(k); ok || c.bytes != 0 {
		t.Fatalf("forget left bytes=%d", c.bytes)
	}
}
