package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// r34: guest RPC counts on hot read paths (events, logs, sandbox list) and the
// usage tap's cost on large bodies.

func (e *env) settle() { time.Sleep(promptRefreshTTL + 50*time.Millisecond) }

// Finished turns' output is immutable: the first events GET reads each once,
// repeats read nothing, a running turn is live, and a turn that ends is
// cached from its first read.
func TestR34EventsCacheFinishedTurns(t *testing.T) {
	e := setup(t)
	count := countGuestOps(t, e)
	id := e.createVM()
	const n = 10
	for i := 0; i < n; i++ {
		if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": fmt.Sprintf("turn %d", i), "new": true}); st != 202 {
			t.Fatalf("prompt %d: %d %v", i, st, out)
		}
	}
	e.settle()
	e.do(e.owner, "GET", "/v1/vms/"+id+"/prompts", nil) // refresh: every turn has ended
	path := "/api/v1/sandboxes/" + id + "/events?sort=asc&type=response&limit=200"
	responses := func() []any { return e.ok("GET", path, nil, 200)["events"].([]any) }

	logs0, status0 := count("cmd_logs"), count("cmd_status")
	evs := responses()
	if len(evs) != n {
		t.Fatalf("response events = %d, want %d", len(evs), n)
	}
	for _, x := range evs {
		if txt, _ := x.(map[string]any)["data"].(map[string]any)["text"].(string); !strings.HasPrefix(txt, "ran: ") {
			t.Fatalf("response text = %q", txt)
		}
	}
	first := count("cmd_logs") - logs0
	t.Logf("first events GET: %d cmd_logs, %d cmd_status", first, count("cmd_status")-status0)
	if first > 2*n {
		t.Fatalf("first GET made %d log RPCs for %d turns, want <= %d", first, n, 2*n)
	}
	logs1 := count("cmd_logs")
	for i := 0; i < 5; i++ {
		if got := responses(); len(got) != n || got[0].(map[string]any)["data"].(map[string]any)["text"] == "" {
			t.Fatalf("cached GET lost events or text: %v", got)
		}
	}
	if d := count("cmd_logs") - logs1; d != 0 {
		t.Fatalf("5 repeat GETs made %d guest log RPCs, want 0", d)
	}

	// A running turn has no response event and is never cached; once it ends its
	// output is read once and then served from memory.
	_, run := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": "sleep-forever", "new": true})
	_ = run
	e.settle()
	if got := responses(); len(got) != n {
		t.Fatalf("running turn produced a response event: %d events", len(got))
	}
	e.finishAllCmds()
	e.settle()
	logs2 := count("cmd_logs")
	got := responses()
	if len(got) != n+1 {
		t.Fatalf("after the turn ended: %d response events, want %d", len(got), n+1)
	}
	if d := count("cmd_logs") - logs2; d < 1 {
		t.Fatalf("the finished turn was not read live (%d log RPCs)", d)
	}
	logs3 := count("cmd_logs")
	responses()
	responses()
	if d := count("cmd_logs") - logs3; d != 0 {
		t.Fatalf("a turn that ended was not cached: %d log RPCs on repeat", d)
	}
}

func TestR34EventTextCacheBounded(t *testing.T) {
	c := newEventTextCache()
	s := &server{}
	k := func(i int) eventTextKey { return eventTextKey{s, "vm", fmt.Sprint(i)} }
	for i := 0; i < eventTextCacheEntries+100; i++ {
		c.put(k(i), "x")
	}
	if c.ll.Len() != eventTextCacheEntries || len(c.m) != eventTextCacheEntries {
		t.Fatalf("entries = %d/%d, want %d", c.ll.Len(), len(c.m), eventTextCacheEntries)
	}
	if _, ok := c.get(k(0)); ok {
		t.Fatal("the oldest entry survived the cap")
	}
	if _, ok := c.get(k(eventTextCacheEntries + 99)); !ok {
		t.Fatal("the newest entry was evicted")
	}
	big := strings.Repeat("a", eventTextCacheBytes/2)
	c.put(eventTextKey{s, "vm", "b1"}, big)
	c.put(eventTextKey{s, "vm", "b2"}, big)
	c.put(eventTextKey{s, "vm", "b3"}, big)
	if c.bytes > eventTextCacheBytes {
		t.Fatalf("cache holds %d bytes, cap %d", c.bytes, eventTextCacheBytes)
	}
	if _, ok := c.get(eventTextKey{s, "vm", "b3"}); !ok {
		t.Fatal("newest big entry missing")
	}
	c.forget(s, "vm")
	if c.ll.Len() != 0 || c.bytes != 0 {
		t.Fatalf("forget left %d entries, %d bytes", c.ll.Len(), c.bytes)
	}
}

// cmd_logs carries exit and signal, so reading logs of a finished command is
// one RPC; an older guest without them costs one cmd_status more.
func TestR34LogsExitFromCmdLogs(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			e := setup(t)
			count := countGuestOps(t, e)
			e.boatd.mu.Lock()
			e.boatd.legacyLogs = legacy
			e.boatd.mu.Unlock()
			id := e.createVM()
			st, c := e.do(e.owner, "POST", "/v1/vms/"+id+"/commands", map[string]any{"cmd": []string{"false"}})
			if st != 201 {
				t.Fatalf("start: %d %v", st, c)
			}
			cid := c["id"].(string)
			logs0, status0 := count("cmd_logs"), count("cmd_status")
			st, out := e.do(e.owner, "GET", "/v1/vms/"+id+"/commands/"+cid+"/logs", nil)
			if st != 200 || out["exit"] != float64(1) {
				t.Fatalf("logs: %d %v, want exit 1", st, out)
			}
			wantStatus := 0
			if legacy {
				wantStatus = 1
			}
			if d := count("cmd_logs") - logs0; d != 1 {
				t.Errorf("cmd_logs calls = %d, want 1", d)
			}
			if d := count("cmd_status") - status0; d != wantStatus {
				t.Errorf("cmd_status calls = %d, want %d", d, wantStatus)
			}
		})
	}
}

// A sandbox list is a display read: it shares one refresh per running turn
// within the TTL, where a single sandbox GET still joins a fresh one.
func TestR34SandboxListUsesRefreshTTL(t *testing.T) {
	e := setup(t)
	count := countGuestOps(t, e)
	const n = 5
	for i := 0; i < n; i++ {
		id := e.createVM()
		if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/prompts", map[string]any{"prompt": "sleep-forever", "new": true}); st != 202 {
			t.Fatalf("prompt: %d %v", st, out)
		}
	}
	e.settle()
	before := count("cmd_status")
	e.ok("GET", "/api/v1/sandboxes", nil, 200)
	first := count("cmd_status") - before
	e.ok("GET", "/api/v1/sandboxes", nil, 200)
	e.ok("GET", "/api/v1/sandboxes", nil, 200)
	if d := count("cmd_status") - before - first; d != 0 {
		t.Fatalf("2 more list GETs inside the TTL made %d cmd_status calls, want 0 (first made %d)", d, first)
	}
	t.Logf("first list GET: %d cmd_status for %d running turns", first, n)
	e.finishAllCmds()
	e.settle()
	for _, x := range e.ok("GET", "/api/v1/sandboxes", nil, 200)["sandboxes"].([]any) {
		if st := x.(map[string]any)["state"]; st == "busy" {
			t.Errorf("list after the TTL still shows a busy sandbox: %v", x)
		}
	}
}

// The tap classifies a body once and scans only new bytes: usage is the same
// and the work is linear in the body, not quadratic.
func TestR34UsageTapLinear(t *testing.T) {
	body := `{"model":"gpt-x","pad":"` + strings.Repeat("z", 6<<20) + `","usage":{"prompt_tokens":3,"completion_tokens":4}}`
	for _, chunk := range []int{1 << 10, 32 << 10} {
		tap := &usageTap{}
		start := time.Now()
		for i := 0; i < len(body); i += chunk {
			tap.Write([]byte(body[i:min(i+chunk, len(body))]))
		}
		in, out, model := tap.result()
		if in != 3 || out != 4 || model != "gpt-x" {
			t.Fatalf("chunk %d: in=%d out=%d model=%s", chunk, in, out, model)
		}
		t.Logf("chunk %d: %v for %d bytes", chunk, time.Since(start), len(body))
	}
	// A marker split across chunks is still found, in any chunking.
	sse := ": ping\n\n" + "data: {\"model\":\"m\",\"usage\":{\"input_tokens\":5,\"output_tokens\":6}}\n\n"
	sse = strings.NewReplacer(`\n`, "\n").Replace(sse)
	for chunk := 1; chunk <= len(sse); chunk++ {
		tap := &usageTap{}
		for i := 0; i < len(sse); i += chunk {
			tap.Write([]byte(sse[i:min(i+chunk, len(sse))]))
		}
		if in, out, model := tap.result(); in != 5 || out != 6 || model != "m" {
			t.Fatalf("sse chunk %d: in=%d out=%d model=%s", chunk, in, out, model)
		}
	}
}

func BenchmarkUsageTapJSON(b *testing.B) {
	chunk := []byte(strings.Repeat("a", 32<<10))
	for i := 0; i < b.N; i++ {
		tap := &usageTap{}
		tap.Write([]byte(`{"model":"m","pad":"`))
		for j := 0; j < 192; j++ { // ~6 MiB
			tap.Write(chunk)
		}
		tap.Write([]byte(`","usage":{"prompt_tokens":1,"completion_tokens":2}}`))
		tap.result()
	}
}

func BenchmarkUsageTapOpaque(b *testing.B) { // neither JSON nor SSE: marker search only
	chunk := []byte(strings.Repeat("a", 32<<10))
	for i := 0; i < b.N; i++ {
		tap := &usageTap{}
		for j := 0; j < 192; j++ {
			tap.Write(chunk)
		}
		tap.result()
	}
}
