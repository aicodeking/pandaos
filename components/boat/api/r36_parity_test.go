package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// retypeRunning is a running VM (no ttl_s: boatd omits 0) that a type change
// resizes and starts again.
func retypeRunning(t *testing.T, e *env) string {
	t.Helper()
	id := e.createVM()
	e.boatd.mu.Lock()
	delete(e.boatd.vms[id], "ttl_s")
	e.boatd.mu.Unlock()
	return id
}

// 1: the resize worked, the start failed and so did the restore: 500
// restore_failed with the start's error, like the resize-failure path.
func TestR36RetypeRestoreFailed(t *testing.T) {
	// r46: a running sandbox answers 409 sandbox_running_stop_first to a resume with a
	// type, before anything is stopped, so no restore can fail.
	e := setup(t)
	id := retypeRunning(t, e)
	e.failNextStarts(2)
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/resume",
		map[string]any{"type": "large", "ttlSeconds": 1200}, nil)
	if st != 409 || errCode(out) != "sandbox_running_stop_first" {
		t.Fatalf("resume = %d %v, want 409 sandbox_running_stop_first", st, out)
	}
	e.boatd.mu.Lock()
	size, state := e.boatd.vms[id]["size"], e.boatd.vms[id]["state"]
	e.boatd.mu.Unlock()
	if size == "large" || state != "running" {
		t.Fatalf("VM = %v/%v, want untouched and running", size, state)
	}
}

// 1b: a VM that never auto-stops (no ttl_s from boatd) is restored with ttl_s 0
// sent explicitly, not left on the retype's ttl.
func TestR36RetypeRestoreKeepsNeverAutoStop(t *testing.T) {
	// r46: refused with 409 before the stop; the VM's never-auto-stop is untouched.
	e := setup(t)
	id := retypeRunning(t, e)
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/resume",
		map[string]any{"type": "large", "ttlSeconds": 1200}, nil)
	if st != 409 {
		t.Fatalf("resume on a running sandbox with a type = %d %v, want 409", st, out)
	}
	e.boatd.mu.Lock()
	state, ttl := e.boatd.vms[id]["state"], e.boatd.vms[id]["ttl_s"]
	e.boatd.mu.Unlock()
	if state != "running" || (ttl != nil && ttl != float64(0)) {
		t.Fatalf("VM = %v ttl %v, want running, never auto-stop", state, ttl)
	}
}

// 2: the guest says a command exited while an inherited child still appends to
// its stdout. Inside the settle window a repeat GET reads again and shows the
// longer text; past it the entry is final and costs no guest call.
func TestR36EventTextSettleWindow(t *testing.T) {
	oldSettle, oldRecheck := eventTextSettle, eventTextRecheck
	eventTextSettle, eventTextRecheck = 1500*time.Millisecond, 0
	t.Cleanup(func() { eventTextSettle, eventTextRecheck = oldSettle, oldRecheck })
	e := setup(t)
	count := countGuestOps(t, e)
	id := e.createVM()
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": "settle me", "new": true}); st != 202 {
		t.Fatalf("prompt: %d %v", st, out)
	}
	e.settle()
	e.do(e.owner, "GET", "/v1/vms/"+id+"/prompts", nil)
	path := "/api/v1/sandboxes/" + id + "/events?sort=asc&type=response&limit=200"
	text := func() string {
		evs := e.ok("GET", path, nil, 200)["events"].([]any)
		if len(evs) != 1 {
			t.Fatalf("response events = %d, want 1", len(evs))
		}
		s, _ := evs[0].(map[string]any)["data"].(map[string]any)["text"].(string)
		return s
	}
	grow := func(extra string) {
		e.boatd.mu.Lock()
		defer e.boatd.mu.Unlock()
		for _, c := range e.boatd.cmds {
			c.log += extra
		}
	}
	first := text()
	if !strings.HasPrefix(first, "ran: ") || strings.Contains(first, "late child output") {
		t.Fatalf("first text = %q", first)
	}
	grow("late child output\n")
	logs0 := count("cmd_logs")
	if got := text(); !strings.Contains(got, "late child output") {
		t.Fatalf("a repeat GET inside the settle window = %q, want the longer text", got)
	}
	if count("cmd_logs") == logs0 {
		t.Fatal("no guest read inside the settle window")
	}
	time.Sleep(eventTextSettle + 100*time.Millisecond)
	grow("after the window\n")
	logs1 := count("cmd_logs")
	for i := 0; i < 3; i++ {
		if got := text(); !strings.Contains(got, "late child output") || strings.Contains(got, "after the window") {
			t.Fatalf("text past the window = %q, want the settled text", got)
		}
	}
	if d := count("cmd_logs") - logs1; d != 0 {
		t.Fatalf("%d guest reads past the settle window, want 0", d)
	}
}

// The cache never replaces an entry with a shorter read, and a failed recheck
// serves what it has (the cache API behind the window).
func TestR36EventTextRecheckGate(t *testing.T) {
	oldSettle, oldRecheck, oldNow := eventTextSettle, eventTextRecheck, eventTextNow
	t.Cleanup(func() { eventTextSettle, eventTextRecheck, eventTextNow = oldSettle, oldRecheck, oldNow })
	eventTextSettle, eventTextRecheck = 10*time.Second, 2*time.Second
	now := time.Unix(1000, 0)
	eventTextNow = func() time.Time { return now }
	c := newEventTextCache()
	k := eventTextKey{vm: "v", cmd: "c"}
	c.putSized(k, "abc", 3)
	if _, re, ok := c.peek(k); !ok || re {
		t.Fatalf("fresh entry: recheck=%v ok=%v, want a plain hit", re, ok)
	}
	now = now.Add(3 * time.Second)
	if _, re, _ := c.peek(k); !re {
		t.Fatal("entry 3s old: want a recheck")
	}
	c.putSized(k, "abc", 3) // checked again, unchanged
	if _, re, _ := c.peek(k); re {
		t.Fatal("rechecked just now: want a plain hit")
	}
	now = now.Add(20 * time.Second)
	if _, re, _ := c.peek(k); re {
		t.Fatal("past the window: want final")
	}
	c.putSized(k, "abcdef", 6)
	if got, _ := c.get(k); got != "abcdef" {
		t.Fatalf("longer read: %q", got)
	}
}

// ---- AI proxy usage metering ----

func aiMeterTotals(e *env) (requests, in, out int64) {
	e.srv.aiMeter.mu.Lock()
	defer e.srv.aiMeter.mu.Unlock()
	for _, r := range e.srv.aiMeter.rows {
		requests += r.Requests
		in += r.InputTokens
		out += r.OutputTokens
	}
	return
}

func waitUntil36(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// aiOpenAIVM is a VM whose token the proxy accepts for the openai provider.
func aiOpenAIVM(t *testing.T) (*env, *aiFake, string, string) {
	t.Helper()
	e, up, proxy := aiSetup(t)
	if st, c := e.do(e.owner, "PUT", "/v1/ai/credentials/openai", map[string]any{"value": "sk-REAL-KEY-123456"}); st != 200 {
		t.Fatalf("set credential: %d %v", st, c)
	}
	e.createVM()
	vars := e.boatd.envOps[0]["vars"].(map[string]any)
	tok, _ := vars["OPENAI_API_KEY"].(string)
	if tok == "" {
		t.Fatalf("vm env has no openai token: %v", vars)
	}
	return e, up, proxy.URL, tok
}

func aiPost(t *testing.T, ctx context.Context, base, path, tok, body string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "POST", base+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func TestR36AIInjectUsage(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     string // "" = unchanged
	}{
		{"stream", `{"model":"m","stream":true,"messages":[]}`, `"stream_options":{"include_usage":true}`},
		{"empty options", `{"stream":true,"stream_options":{}}`, `"stream_options":{"include_usage":true}`},
		{"other options kept", `{"stream":true,"stream_options":{"x":1}}`, `"x":1`},
		{"no stream", `{"model":"m","messages":[]}`, ""},
		{"stream false", `{"stream":false}`, ""},
		{"caller chose false", `{"stream":true,"stream_options":{"include_usage":false}}`, `"stream_options":{"include_usage":true}`}, // forced: metering is not optional
		{"caller chose true", `{"stream":true,"stream_options":{"include_usage":true}}`, `"stream_options":{"include_usage":true}`},
		{"not an object", `[1,2]`, ""},
		{"not json", `stream`, ""},
		{"bad options", `{"stream":true,"stream_options":5}`, ""},
	} {
		got, changed := aiInjectUsage([]byte(tc.in))
		if tc.want == "" {
			if changed || string(got) != tc.in {
				t.Errorf("%s: changed %q to %q", tc.name, tc.in, got)
			}
			continue
		}
		if !changed || !strings.Contains(string(got), tc.want) || !strings.Contains(string(got), `"include_usage":true`) {
			t.Errorf("%s: %q -> %q (%v), want %s and include_usage", tc.name, tc.in, got, changed, tc.want)
		}
		var m map[string]any
		if err := json.Unmarshal(got, &m); err != nil {
			t.Errorf("%s: result is not JSON: %v", tc.name, err)
		}
	}
	// Large numbers survive the round trip untouched.
	got, _ := aiInjectUsage([]byte(`{"stream":true,"seed":12345678901234567890}`))
	if !strings.Contains(string(got), "12345678901234567890") {
		t.Errorf("number rewritten: %s", got)
	}
}

func TestR36AIProxyInjectsStreamOptions(t *testing.T) {
	e, up, base, tok := aiOpenAIVM(t)
	_ = e
	up.mu.Lock()
	up.reply = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"model\":\"gpt-x\",\"choices\":[]}\n\ndata: {\"model\":\"gpt-x\",\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":6}}\n\ndata: [DONE]\n\n")
	}
	up.mu.Unlock()
	do := func(path, body string) string {
		resp, err := aiPost(t, context.Background(), base, path, tok, body)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		up.mu.Lock()
		defer up.mu.Unlock()
		return up.bodies[len(up.bodies)-1]
	}
	if got := do("/openai/v1/chat/completions", `{"model":"gpt-x","stream":true}`); !strings.Contains(got, `"stream_options":{"include_usage":true}`) {
		t.Fatalf("upstream body = %s, want stream_options injected", got)
	}
	if got := do("/openai/v1/chat/completions", `{"stream":true,"stream_options":{"include_usage":false}}`); !strings.Contains(got, `"include_usage":true`) {
		t.Errorf("caller's include_usage:false was not forced on: %s", got)
	}
	for name, tc := range map[string][2]string{
		"non-streaming":  {"/openai/v1/chat/completions", `{"model":"gpt-x"}`},
		"other endpoint": {"/openai/v1/responses", `{"stream":true}`},
	} {
		if got := do(tc[0], tc[1]); got != tc[1] {
			t.Errorf("%s: upstream body = %s, want it unchanged (%s)", name, got, tc[1])
		}
	}
	waitUntil36(t, "usage", func() bool { _, in, out := aiMeterTotals(e); return in == 16 && out == 24 })
}

// A client that hangs up mid-stream does not lose the final usage chunk: the proxy
// keeps draining the provider (bounded) into the tap.
func TestR36AIProxyDrainsAfterClientAbort(t *testing.T) {
	e, up, base, tok := aiOpenAIVM(t)
	release := make(chan struct{})
	up.mu.Lock()
	up.reply = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"model\":\"gpt-x\",\"choices\":[{}]}\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: {\"model\":\"gpt-x\",\"usage\":{\"prompt_tokens\":21,\"completion_tokens\":34}}\n\ndata: [DONE]\n\n")
	}
	up.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := aiPost(t, ctx, base, "/openai/v1/chat/completions", tok, `{"stream":true}`)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}
	cancel() // the client leaves before the usage chunk
	resp.Body.Close()
	time.Sleep(100 * time.Millisecond)
	close(release)
	waitUntil36(t, "the drained usage", func() bool { _, in, out := aiMeterTotals(e); return in == 21 && out == 34 })
}

// The drain is bounded in time and in bytes.
func TestR36AIProxyDrainBounds(t *testing.T) {
	oldMax, oldBytes := aiDrainMax, aiDrainBytes
	aiDrainMax, aiDrainBytes = 300*time.Millisecond, 64<<10
	t.Cleanup(func() { aiDrainMax, aiDrainBytes = oldMax, oldBytes })

	t.Run("time", func(t *testing.T) {
		_, up, base, tok := aiOpenAIVM(t)
		upDone := make(chan struct{})
		up.mu.Lock()
		up.reply = func(w http.ResponseWriter, r *http.Request) {
			defer close(upDone)
			io.WriteString(w, "data: {}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done() // never sends more: only the proxy hanging up ends it
		}
		up.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		resp, err := aiPost(t, ctx, base, "/openai/v1/chat/completions", tok, `{"stream":true}`)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Read(make([]byte, 8))
		cancel()
		resp.Body.Close()
		select {
		case <-upDone:
		case <-time.After(5 * time.Second):
			t.Fatal("the proxy kept the provider call open past the drain bound")
		}
	})

	t.Run("bytes", func(t *testing.T) {
		e, up, base, tok := aiOpenAIVM(t)
		var sent atomic.Int64
		first := make(chan struct{})
		upDone := make(chan struct{})
		up.mu.Lock()
		up.reply = func(w http.ResponseWriter, r *http.Request) {
			defer close(upDone)
			chunk := "data: {\"x\":\"" + strings.Repeat("a", 16<<10) + "\"}\n\n"
			for i := 0; i < 400 && r.Context().Err() == nil; i++ {
				if _, err := io.WriteString(w, chunk); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				sent.Add(1)
				if i == 0 {
					close(first)
					time.Sleep(50 * time.Millisecond)
				}
			}
		}
		up.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		resp, err := aiPost(t, ctx, base, "/openai/v1/chat/completions", tok, `{"stream":true}`)
		if err != nil {
			t.Fatal(err)
		}
		<-first
		cancel()
		resp.Body.Close()
		select {
		case <-upDone:
		case <-time.After(10 * time.Second):
			t.Fatal("upstream was never cut off")
		}
		if n := sent.Load(); n >= 400 {
			t.Fatalf("the proxy drained all %d chunks, want a cutoff near %d KiB", n, aiDrainBytes>>10)
		}
		waitUntil36(t, "the request counted", func() bool { r, _, _ := aiMeterTotals(e); return r == 1 })
	})
}

// A response with no usage object is logged as unmetered, not silently 0/0; one
// with usage is not.
func TestR36AIProxyLogsUnmetered(t *testing.T) {
	logs := &syncWriter{}
	old := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(old) })
	e, up, base, tok := aiOpenAIVM(t)
	reply := func(body string) {
		up.mu.Lock()
		up.reply = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, body)
		}
		up.mu.Unlock()
	}
	call := func() {
		resp, err := aiPost(t, context.Background(), base, "/openai/v1/chat/completions", tok, `{"stream":true}`)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	reply("data: {\"model\":\"gpt-x\",\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\n")
	call()
	waitUntil36(t, "first request", func() bool { r, _, _ := aiMeterTotals(e); return r == 1 })
	if strings.Contains(logs.String(), "unmetered") {
		t.Fatalf("a metered response was logged unmetered: %s", logs.String())
	}
	reply("data: {\"model\":\"gpt-x\",\"choices\":[]}\n\ndata: [DONE]\n\n")
	call()
	waitUntil36(t, "second request", func() bool { r, _, _ := aiMeterTotals(e); return r == 2 })
	if !strings.Contains(logs.String(), "ai proxy: unmetered response") {
		t.Fatalf("a response with no usage was not logged: %q", logs.String())
	}
}

// syncWriter is a log sink the test can read while the log package writes.
type syncWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncWriter) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// ---- usage tap ----

// A final usage line after an SSE line over the cap is still parsed, whether the
// long line spans many chunks or arrives with it in one.
func TestR36UsageTapSSEAfterOversizedLine(t *testing.T) {
	long := "data: {\"model\":\"m\",\"delta\":\"" + strings.Repeat("q", 9<<20) + "\"}\n"
	final := "data: {\"model\":\"m\",\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}\n\ndata: [DONE]\n\n"
	for _, chunk := range []int{4 << 10, 32 << 10, 1 << 20} {
		tap := &usageTap{}
		all := "data: {\"model\":\"m\"}\n\n" + long + final
		for i := 0; i < len(all); i += chunk {
			tap.Write([]byte(all[i:min(i+chunk, len(all))]))
		}
		if in, out, model := tap.result(); in != 3 || out != 4 || model != "m" {
			t.Fatalf("chunk %d: in=%d out=%d model=%q, want 3 4 m", chunk, in, out, model)
		}
		if tap.skipping || tap.buf.Len() != 0 {
			t.Fatalf("chunk %d: tap kept state after the stream (skipping=%v, buf=%d)", chunk, tap.skipping, tap.buf.Len())
		}
	}
	// The oversized line itself carries the usage at its end.
	tap := &usageTap{}
	line := "data: {\"model\":\"m\",\"pad\":\"" + strings.Repeat("z", 9<<20) + "\",\"usage\":{\"input_tokens\":8,\"output_tokens\":9}}\n\n"
	for i := 0; i < len(line); i += 32 << 10 {
		tap.Write([]byte(line[i:min(i+32<<10, len(line))]))
	}
	if in, out, _ := tap.result(); in != 8 || out != 9 {
		t.Fatalf("usage inside the oversized line: %d %d", in, out)
	}
	// And a line that never ends before the stream does still gives its tail.
	tap = &usageTap{}
	partial := strings.TrimSuffix(line, "\n\n")
	for i := 0; i < len(partial); i += 32 << 10 {
		tap.Write([]byte(partial[i:min(i+32<<10, len(partial))]))
	}
	if in, out, _ := tap.result(); in != 8 || out != 9 {
		t.Fatalf("unterminated oversized line: %d %d", in, out)
	}
}

// scan reuses its state: an SSE chunk with no data line costs no allocation, where
// each call used to make a 64 KiB scanner buffer.
func TestR36UsageTapScanAllocs(t *testing.T) {
	tap := &usageTap{}
	tap.Write([]byte("data: {\"model\":\"m\"}\n\n"))
	chunk := []byte(": keep-alive\n\n")
	tap.Write(chunk)
	allocs := testing.AllocsPerRun(200, func() { tap.Write(chunk) })
	if allocs > 1 {
		t.Fatalf("%v allocations per SSE chunk, want ~0", allocs)
	}
	// Results are the same as before for lines split over chunks, CRLF included.
	tap = &usageTap{}
	for _, part := range []string{"data: {\"usage\":{\"input_tok", "ens\":5,\"output_tokens\":6}}\r", "\n\r\ndata: [DONE]\r\n"} {
		tap.Write([]byte(part))
	}
	if in, out, _ := tap.result(); in != 5 || out != 6 {
		t.Fatalf("split CRLF line: %d %d", in, out)
	}
}
