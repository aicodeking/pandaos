package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---- r37: AI metering cannot be bypassed by the request's shape ----

// aiPostRaw posts body with the given framing: chunked (unknown length) or sized.
func aiPostRaw(t *testing.T, base, path, tok, ctype, body string, chunked bool) {
	t.Helper()
	var rd io.Reader = strings.NewReader(body)
	if chunked {
		rd = io.MultiReader(rd) // hides the length: the client sends chunked
	}
	req, _ := http.NewRequestWithContext(context.Background(), "POST", base+path, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func lastUpstreamBody(up *aiFake) string {
	up.mu.Lock()
	defer up.mu.Unlock()
	return up.bodies[len(up.bodies)-1]
}

func TestR37AIProxyPatchesEveryRequestShape(t *testing.T) {
	_, up, base, tok := aiOpenAIVM(t)
	up.mu.Lock()
	up.reply = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"model\":\"gpt-x\",\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}
	up.mu.Unlock()
	pad := strings.Repeat("x", 2<<20) // over the old 1 MiB cap
	for _, tc := range []struct {
		name, ctype, body string
		chunked           bool
	}{
		{"chunked", "application/json", `{"stream":true,"model":"m"}`, true},
		{"include_usage false", "application/json", `{"stream":true,"stream_options":{"include_usage":false}}`, false},
		{"include_usage false, chunked", "application/json", `{"stream":true,"stream_options":{"include_usage":false}}`, true},
		{"include_usage not a bool", "application/json", `{"stream":true,"stream_options":{"include_usage":"no"}}`, false},
		{"large body", "application/json", `{"stream":true,"messages":[{"content":"` + pad + `"}]}`, false},
		{"large chunked body", "application/json", `{"stream":true,"messages":[{"content":"` + pad + `"}]}`, true},
		{"no content type", "", `{"stream":true}`, false},
		{"text/plain content type", "text/plain", `{"stream":true}`, false},
		{"duplicate keys", "application/json", `{"stream":true,"stream_options":{"include_usage":true,"include_usage":false}}`, false},
	} {
		aiPostRaw(t, base, "/openai/v1/chat/completions", tok, tc.ctype, tc.body, tc.chunked)
		got := lastUpstreamBody(up)
		var m struct {
			Stream  bool `json:"stream"`
			Options struct {
				IncludeUsage *bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if err := json.Unmarshal([]byte(got), &m); err != nil {
			t.Errorf("%s: upstream body is not JSON: %v", tc.name, err)
			continue
		}
		if !m.Stream || m.Options.IncludeUsage == nil || !*m.Options.IncludeUsage {
			head := got
			if len(head) > 200 {
				head = head[:200]
			}
			t.Errorf("%s: upstream body not forced to include_usage:true: %s", tc.name, head)
		}
		if strings.Contains(tc.name, "large") && !strings.Contains(got, pad) {
			t.Errorf("%s: the large body was altered (len %d)", tc.name, len(got))
		}
		up.mu.Lock()
		te := up.headers[len(up.headers)-1].Get("Transfer-Encoding")
		up.mu.Unlock()
		if te != "" {
			t.Errorf("%s: forwarded with Transfer-Encoding %q, want a sized body", tc.name, te)
		}
	}
}

// A body over aiMaxBody is refused, not forwarded unmetered.

func TestR37AIProxyRefusesBodyOverCap(t *testing.T) {
	_, up, base, tok := aiOpenAIVM(t)
	up.mu.Lock()
	n := len(up.bodies)
	up.mu.Unlock()
	body := `{"stream":true,"p":"` + strings.Repeat("x", aiMaxBody+1) + `"}`
	resp, err := aiPost(t, context.Background(), base, "/openai/v1/chat/completions", tok, body)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("status %d, want 413", resp.StatusCode)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.bodies) != n {
		t.Fatal("an over-cap body reached the provider")
	}
}

func TestR37AIInjectUsageForcesTrue(t *testing.T) {
	for _, in := range []string{
		`{"stream":true,"stream_options":{"include_usage":false}}`,
		`{"stream":true,"stream_options":{"include_usage":true}}`,
		`{"stream":true,"stream_options":null}`,
		`{"stream":true,"stream_options":{"include_usage":null,"x":1}}`,
	} {
		got, ok := aiInjectUsage([]byte(in))
		if !ok || !strings.Contains(string(got), `"include_usage":true`) || strings.Contains(string(got), `"include_usage":false`) {
			t.Errorf("%s -> %s (%v)", in, got, ok)
		}
	}
	for _, in := range []string{`{"stream":false,"stream_options":{"include_usage":false}}`, `{"x":1}`, `nope`, `{"stream":true,"stream_options":[]}`} {
		if got, ok := aiInjectUsage([]byte(in)); ok || string(got) != in {
			t.Errorf("%s changed to %s", in, got)
		}
	}
}

// ---- r37: unmetered answers are counted ----

func TestR37UnmeteredRowCounted(t *testing.T) {
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
		aiPostRaw(t, base, "/openai/v1/chat/completions", tok, "application/json", `{"stream":true}`, false)
	}
	reply("data: {\"model\":\"gpt-x\",\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n\n")
	call()
	reply("data: {\"model\":\"gpt-x\",\"choices\":[]}\n\ndata: [DONE]\n\n")
	call()
	call()
	waitUntil36(t, "three requests", func() bool { r, _, _ := aiMeterTotals(e); return r == 3 })

	check := func(label string) {
		t.Helper()
		st, out := e.do(e.owner, "GET", "/v1/ai/usage", nil)
		if st != 200 {
			t.Fatalf("%s: %d %v", label, st, out)
		}
		tot := out["totals"].(map[string]any)
		if tot["requests"] != 3.0 || tot["unmetered"] != 2.0 || tot["input_tokens"] != 3.0 {
			t.Fatalf("%s: totals = %v", label, tot)
		}
		rows := out["usage"].([]any)
		if len(rows) != 1 || rows[0].(map[string]any)["unmetered"] != 2.0 {
			t.Fatalf("%s: rows = %v", label, rows)
		}
	}
	check("pending") // read straight from the meter's batch
	e.srv.aiMeter.flush(e.srv)
	check("flushed") // folded into the store
}

// ---- r37: reading and flushing AI usage never waits for an fsync ----

func TestR37AIUsageReadDoesNotFsync(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	vm := e.nativeOf(bx)
	team := e.teamID()
	e.srv.aiMeter.record(team, vm, "openai", "m", 5, 6, false)
	e.srv.aiMeter.record(team, vm, "openai", "m", 0, 0, true)

	// Drain async Store.Apply flushes from newSandbox (coalesceCreateStore / compatIDs)
	// before taking the baseline disk-write count.
	if err := e.srv.store.Flush(); err != nil {
		t.Fatal(err)
	}
	const fsync = 1500 * time.Millisecond
	slowFsync(t, fsync)
	writes := e.srv.store.writes.Load()

	timed := func(name string, f func()) {
		t.Helper()
		t0 := time.Now()
		f()
		if d := time.Since(t0); d > fsync/3 {
			t.Fatalf("%s took %v: it waited for an fsync", name, d)
		}
	}
	timed("native team usage", func() {
		st, out := e.do(e.owner, "GET", "/v1/ai/usage", nil)
		tot, _ := out["totals"].(map[string]any)
		if st != 200 || tot["requests"] != 2.0 || tot["input_tokens"] != 5.0 || tot["unmetered"] != 1.0 {
			t.Fatalf("usage = %d %v", st, out)
		}
	})
	timed("native vm usage", func() {
		st, out := e.do(e.owner, "GET", "/v1/vms/"+vm+"/ai-usage", nil)
		if tot, _ := out["totals"].(map[string]any); st != 200 || tot["requests"] != 2.0 {
			t.Fatalf("vm usage = %d %v", st, out)
		}
	})
	timed("compat usage", func() {
		out := e.ok("GET", "/api/v1/ai-usage?days=1", nil, 200)
		if tot, _ := out["totals"].(map[string]any); tot["requests"] != 2.0 {
			t.Fatalf("compat usage = %v", out)
		}
	})
	timed("flush", func() { e.srv.aiMeter.flush(e.srv) })
	timed("read after flush", func() {
		_, out := e.do(e.owner, "GET", "/v1/ai/usage", nil)
		if tot, _ := out["totals"].(map[string]any); tot["requests"] != 2.0 || tot["unmetered"] != 1.0 {
			t.Fatalf("usage after flush = %v", out)
		}
	})
	if got := e.srv.store.writes.Load(); got != writes {
		t.Fatalf("%d disk writes during reads and a flush, want 0 inline", got-writes)
	}
	// A second flush with nothing pending is a no-op; counts are not doubled.
	e.srv.aiMeter.flush(e.srv)
	_, out := e.do(e.owner, "GET", "/v1/ai/usage", nil)
	if tot, _ := out["totals"].(map[string]any); tot["requests"] != 2.0 {
		t.Fatalf("usage counted twice: %v", out)
	}
}

// The lazy write still reaches the disk, and the final flush is durable.
func TestR37AIUsageReachesDisk(t *testing.T) {
	e := setup(t)
	vm := e.createVM()
	team := e.teamID()
	e.srv.aiMeter.record(team, vm, "openai", "m", 7, 8, false)
	e.srv.aiMeter.flush(e.srv)
	e.srv.aiMeter.record(team, vm, "openai", "m", 1, 1, true)
	e.srv.flushAIUsageFinal() // durable
	if err := e.srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	re, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	row := re.Snapshot().AIUsage[aiUsageKey(team, vm, "openai", "m")]
	if row == nil || row.Requests != 2 || row.InputTokens != 8 || row.Unmetered != 1 {
		t.Fatalf("row after reopen = %+v", row)
	}
}

// ---- r37: State.Seq ----

func TestR37StateSeqMonotonicAndPersisted(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var a, b uint64
	if err := s.Update(func(st *State) error { a = st.nextSeq(); b = st.nextSeq(); return nil }); err != nil {
		t.Fatal(err)
	}
	if a == 0 || b != a+1 {
		t.Fatalf("seq = %d, %d", a, b)
	}
	s.Close()
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var c uint64
	s2.Update(func(st *State) error { c = st.nextSeq(); return nil })
	if c != b+1 {
		t.Fatalf("seq after reopen = %d, want %d", c, b+1)
	}
}
