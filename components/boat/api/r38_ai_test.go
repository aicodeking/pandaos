package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- r38: body byte budget, read deadline, strict inject, bounded usage ----

// withAIBudgets sets the proxy's body limits for one test.
func withAIBudgets(t *testing.T, global, perVM int64, deadline time.Duration) {
	t.Helper()
	g, v, d := aiBudgetGlobal, aiBudgetPerVM, aiBodyDeadline
	aiBudgetGlobal, aiBudgetPerVM, aiBodyDeadline = global, perVM, deadline
	t.Cleanup(func() { aiBudgetGlobal, aiBudgetPerVM, aiBodyDeadline = g, v, d })
}

// stallRequest opens a request that declares n body bytes, sends only sent of
// them and waits: it holds n bytes of the proxy's budget. The returned conn is
// the stalled client.
func stallRequest(t *testing.T, base, path, tok string, n, sent int) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		path, tok, n, `{"stream":true,"p":"`+strings.Repeat("x", max(0, sent-20)))
	return conn
}

func heldAIBytes(e *env) int64 {
	e.srv.aiInflight.mu.Lock()
	defer e.srv.aiInflight.mu.Unlock()
	return e.srv.aiInflight.heldTotal
}

func waitHeld(t *testing.T, e *env, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for heldAIBytes(e) != want {
		if time.Now().After(deadline) {
			t.Fatalf("held bytes = %d, want %d", heldAIBytes(e), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func postStatus(t *testing.T, base, tok, body string, chunked bool) (int, http.Header) {
	t.Helper()
	var rd io.Reader = strings.NewReader(body)
	if chunked {
		rd = io.MultiReader(rd)
	}
	req, _ := http.NewRequest("POST", base+"/openai/v1/chat/completions", rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header
}

// A VM's buffered bodies are capped: while one request holds the budget, the next
// is refused with 429 + Retry-After, sized or chunked; the budget comes back.
func TestR38AIBodyBudgetPerVM(t *testing.T) {
	e, up, base, tok := aiOpenAIVM(t)
	withAIBudgets(t, 1<<30, 4000, time.Minute)
	stallRequest(t, base, "/openai/v1/chat/completions", tok, 3000, 100)
	waitHeld(t, e, 3000)

	big := `{"stream":true,"p":"` + strings.Repeat("y", 2000) + `"}`
	for _, chunked := range []bool{false, true} {
		st, h := postStatus(t, base, tok, big, chunked)
		if st != 429 || h.Get("Retry-After") == "" {
			t.Fatalf("chunked=%v: status %d Retry-After %q, want 429 + Retry-After", chunked, st, h.Get("Retry-After"))
		}
	}
	up.mu.Lock()
	n := len(up.bodies)
	up.mu.Unlock()
	if n != 0 {
		t.Fatalf("a refused body reached the provider (%d)", n)
	}
	if got := heldAIBytes(e); got != 3000 {
		t.Fatalf("refused requests changed the held bytes: %d", got)
	}
	// A body that fits still works while the other is stalled.
	if st, _ := postStatus(t, base, tok, `{"stream":true}`, false); st != 200 {
		t.Fatalf("small body: %d", st)
	}
}

// All VMs together are capped too: 503 + Retry-After.
func TestR38AIBodyBudgetGlobal(t *testing.T) {
	e, _, base, tok := aiOpenAIVM(t)
	withAIBudgets(t, 5000, 1<<30, time.Minute)
	stallRequest(t, base, "/openai/v1/chat/completions", tok, 4000, 100)
	waitHeld(t, e, 4000)
	st, h := postStatus(t, base, tok, `{"stream":true,"p":"`+strings.Repeat("y", 2000)+`"}`, false)
	if st != 503 || h.Get("Retry-After") == "" {
		t.Fatalf("status %d Retry-After %q, want 503 + Retry-After", st, h.Get("Retry-After"))
	}
}

// Concurrent oversize requests cannot hold more than the budget at once, and
// every byte is returned when they end.
func TestR38AIBodyBudgetConcurrent(t *testing.T) {
	e, _, base, tok := aiOpenAIVM(t)
	e.srv.aiLim = newLimiter(0, 0) // not the point here
	withAIBudgets(t, 1<<30, 10_000, time.Minute)
	body := `{"stream":true,"p":"` + strings.Repeat("z", 3000) + `"}`
	var mu sync.Mutex
	var peak int64
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			h := heldAIBytes(e)
			mu.Lock()
			peak = max(peak, h)
			mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()
	var wg sync.WaitGroup
	codes := make(chan int, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, _ := postStatus(t, base, tok, body, i%2 == 0)
			codes <- st
		}()
	}
	wg.Wait()
	close(stop)
	close(codes)
	for st := range codes {
		if st != 200 && st != 429 {
			t.Errorf("status %d, want 200 or 429", st)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > 10_000 {
		t.Fatalf("held %d buffered bytes at once, budget 10000", peak)
	}
	waitHeld(t, e, 0)
}

// A body that is too slow is cut off at the deadline with 408, its budget is
// returned, and nothing reaches the provider.
func TestR38AIBodyReadDeadline(t *testing.T) {
	e, up, base, tok := aiOpenAIVM(t)
	withAIBudgets(t, 1<<30, 1<<30, 300*time.Millisecond)
	conn := stallRequest(t, base, "/openai/v1/chat/completions", tok, 1000, 100)
	waitHeld(t, e, 1000)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a stalled body: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 408 {
		t.Fatalf("status %d, want 408", resp.StatusCode)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("cut off after %v", d)
	}
	waitHeld(t, e, 0)
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.bodies) != 0 {
		t.Fatal("a stalled body reached the provider")
	}
}

// Normal requests are unaffected, and the deadline is not left on the connection.
func TestR38AINormalRequestsStillWork(t *testing.T) {
	e, up, base, tok := aiOpenAIVM(t)
	withAIBudgets(t, 1<<20, 1<<20, 200*time.Millisecond)
	for i := 0; i < 3; i++ { // keep-alive: the same conn is reused across the deadline
		if st, _ := postStatus(t, base, tok, `{"stream":true,"model":"m"}`, i == 1); st != 200 {
			t.Fatalf("request %d: %d", i, st)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if got := lastUpstreamBody(up); !strings.Contains(got, `"include_usage":true`) {
		t.Fatalf("not patched: %s", got)
	}
	waitHeld(t, e, 0)
}

func TestR38AIByteReservation(t *testing.T) {
	l := newInflight(0)
	a, b := l.reserve("vm1", 100, 150), l.reserve("vm2", 100, 150)
	if err := a.add(90); err != nil {
		t.Fatal(err)
	}
	if err := a.add(20); err == nil {
		t.Fatal("per-VM limit not enforced")
	}
	if err := b.add(80); err == nil {
		t.Fatal("global limit not enforced")
	}
	if err := b.add(60); err != nil {
		t.Fatal(err)
	}
	a.release()
	a.release() // twice: harmless
	b.release()
	if l.heldTotal != 0 || len(l.held) != 0 {
		t.Fatalf("leak: %d %v", l.heldTotal, l.held)
	}
}

// ---- strict inject ----

func TestR38AIRejectsMalformedInjectBodies(t *testing.T) {
	_, up, base, tok := aiOpenAIVM(t)
	for _, tc := range []struct{ name, body string }{
		{"BOM", "\xef\xbb\xbf" + `{"stream":true}`},
		{"stream number", `{"stream":1}`},
		{"stream string", `{"stream":"true"}`},
		{"stream object", `{"stream":{}}`},
		{"array", `[{"stream":true}]`},
		{"scalar", `"x"`},
		{"null", `null`},
		{"not json", `stream=true`},
		{"duplicate stream, last wrong", `{"stream":true,"stream":1}`},
	} {
		for _, chunked := range []bool{false, true} {
			resp, err := aiPostChunk(t, base, "/openai/v1/chat/completions", tok, tc.body, chunked)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 400 || !strings.Contains(string(b), `"invalid_request"`) {
				t.Errorf("%s (chunked=%v): %d %s, want 400 invalid_request", tc.name, chunked, resp.StatusCode, b)
			}
		}
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.bodies) != 0 {
		t.Fatalf("%d malformed bodies reached the provider", len(up.bodies))
	}
}

func aiPostChunk(t *testing.T, base, path, tok, body string, chunked bool) (*http.Response, error) {
	t.Helper()
	var rd io.Reader = strings.NewReader(body)
	if chunked {
		rd = io.MultiReader(rd)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "POST", base+path, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	return http.DefaultClient.Do(req)
}

// stream absent, false or null is not an error: forwarded as sent.
func TestR38AIValidNonStreamBodiesForwardedAsSent(t *testing.T) {
	_, up, base, tok := aiOpenAIVM(t)
	for _, body := range []string{`{"model":"m"}`, `{"stream":false}`, ` {"stream":null,"a":[1, 2]} `} {
		if st, _ := postStatus(t, base, tok, body, false); st != 200 {
			t.Fatalf("%s: %d", body, st)
		}
		if got := lastUpstreamBody(up); got != body {
			t.Errorf("forwarded %q, sent %q", got, body)
		}
	}
}

// Every OpenAI-style completions endpoint of the allow-list gets include_usage.
func TestR38AIInjectsOnEveryCompletionsPath(t *testing.T) {
	for _, tc := range []struct {
		provider, path string
		want           bool
	}{
		{"openai", "v1/chat/completions", true},
		{"openai", "v1/completions", true},
		{"openrouter", "api/v1/completions", true},
		{"openrouter", "api/v1/chat/completions", true},
		{"deepseek", "chat/completions", true},
		{"deepseek", "beta/completions", true},
		{"deepseek", "completions", true},
		{"mistral", "v1/fim/completions", false}, // not known to accept stream_options
		{"mistral", "v1/agents/completions", false},
		{"xai", "v1/completions", true},
		{"anthropic", "v1/complete", false},
		{"anthropic", "v1/messages", false},
		{"openai", "v1/embeddings", false},
		{"openai", "v1/responses", false},
		{"openai", "v1/models", false},
	} {
		r := httptest.NewRequest("POST", "/"+tc.provider+"/"+tc.path, strings.NewReader(`{"stream":true}`))
		r.SetPathValue("path", tc.path)
		if got := aiMayInjectUsage(tc.provider, r); got != tc.want {
			t.Errorf("%s %s: inject=%v, want %v", tc.provider, tc.path, got, tc.want)
		}
		// Every injectable path is in the allow-list (the table and the rule agree).
		if tc.want && !aiPathAllowed(tc.provider, r, tc.path) {
			t.Errorf("%s %s injects but is not allowed", tc.provider, tc.path)
		}
	}
	r := httptest.NewRequest("GET", "/openai/v1/completions", nil)
	r.SetPathValue("path", "v1/completions")
	if aiMayInjectUsage("openai", r) {
		t.Error("GET injects")
	}
}

func TestR38AIProxyInjectsOnLegacyCompletions(t *testing.T) {
	_, up, base, tok := aiOpenAIVM(t)
	resp, err := aiPostChunk(t, base, "/openai/v1/completions", tok, `{"stream":true,"stream_options":{"include_usage":false},"prompt":"hi"}`, true)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(lastUpstreamBody(up), `"include_usage":true`) {
		t.Fatalf("status %d, upstream %s", resp.StatusCode, lastUpstreamBody(up))
	}
	// strictness applies there too
	resp, err = aiPostChunk(t, base, "/openai/v1/completions", tok, `{"stream":"true"}`, false)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("string stream on /v1/completions: %d", resp.StatusCode)
	}
}

func TestR38AIPrepareBodyDropsNothing(t *testing.T) {
	big := strings.Repeat("a", 1<<16)
	in := `{"b":1,"stream":true,"messages":[{"c":"` + big + `"}],"stream_options":{"x":2}}`
	out, herr := aiPrepareBody([]byte(in))
	if herr != nil || !strings.Contains(string(out), big) || !strings.Contains(string(out), `"x":2`) ||
		!strings.Contains(string(out), `"include_usage":true`) {
		t.Fatalf("%v %.80s", herr, out)
	}
	// stream_options of a bad type: forwarded as sent, the provider answers
	bad := `{"stream":true,"stream_options":5}`
	if out, herr := aiPrepareBody([]byte(bad)); herr != nil || string(out) != bad {
		t.Fatalf("%v %s", herr, out)
	}
}

// ---- bounded AI usage state ----

func newUsageState() *State {
	st := &State{}
	st.init()
	return st
}

func useVM(st *State, team, vm, provider, model, day string, req, in, out int64) {
	d := &AIUsageRow{TeamID: team, VMID: vm, Provider: provider, Model: model, Requests: req, InputTokens: in, OutputTokens: out, Day: day}
	now := time.Now().UTC()
	addAIUsage(st.AIUsage, aiUsageKey(team, vm, provider, model), d, "", now)
	addAIUsage(st.AIUsageDays, aiUsageKey(team, vm, provider, model)+"/"+day, d, day, now)
}

func sumRows(m map[string]*AIUsageRow, team string) (req, in, out int64) {
	for _, r := range m {
		if r.TeamID == team {
			req, in, out = req+r.Requests, in+r.InputTokens, out+r.OutputTokens
		}
	}
	return
}

// 10k create/use/delete cycles leave a bounded number of rows and the same totals.
func TestR38AIUsageBoundedAfterManyDeletedVMs(t *testing.T) {
	st := newUsageState()
	today := time.Now().UTC().Format(time.DateOnly)
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format(time.DateOnly)
	useVM(st, "t2", "keep", "openai", "m", today, 7, 70, 7) // another team: untouched
	const n = 10000
	for i := 0; i < n; i++ {
		vm := fmt.Sprintf("vm-%d", i)
		useVM(st, "t1", vm, "openai", "gpt", today, 1, 10, 2)
		useVM(st, "t1", vm, "openai", "gpt", yesterday, 1, 5, 1)
		useVM(st, "t1", vm, "anthropic", "claude", today, 2, 3, 4)
		foldDeletedVMAIUsage(st, vm)
	}
	if got := len(st.AIUsage); got > 10 {
		t.Errorf("lifetime rows = %d after %d deleted VMs", got, n)
	}
	if got := len(st.AIUsageDays); got > 10 {
		t.Errorf("per-day rows = %d after %d deleted VMs", got, n)
	}
	for name, m := range map[string]map[string]*AIUsageRow{"lifetime": st.AIUsage, "days": st.AIUsageDays} {
		req, in, out := sumRows(m, "t1")
		if req != 4*n || in != 18*n || out != 7*n {
			t.Errorf("%s totals = %d/%d/%d, want %d/%d/%d", name, req, in, out, 4*n, 18*n, 7*n)
		}
		if req, in, out := sumRows(m, "t2"); req != 7 || in != 70 || out != 7 {
			t.Errorf("%s: another team's totals changed: %d/%d/%d", name, req, in, out)
		}
	}
	// per-day counts survive the fold, by day
	if r := st.AIUsageDays[aiUsageKey("t1", aiDeletedVM, "openai", "gpt")+"/"+yesterday]; r == nil || r.Requests != n || r.Day != yesterday {
		t.Errorf("yesterday's aggregate = %+v", r)
	}
	// folding twice, or an unknown VM, changes nothing
	foldDeletedVMAIUsage(st, "vm-1")
	foldDeletedVMAIUsage(st, "nobody")
	foldDeletedVMAIUsage(st, aiDeletedVM)
	if req, _, _ := sumRows(st.AIUsage, "t1"); req != 4*n {
		t.Errorf("totals after repeated folds: %d", req)
	}
}

// Live VMs over the cap: the oldest per-day rows fold into the team aggregate and
// then drop; lifetime rows (the totals) never change.
func TestR38AIUsageDaysCapped(t *testing.T) {
	old := aiUsageDaysMaxRows
	aiUsageDaysMaxRows = 50
	t.Cleanup(func() { aiUsageDaysMaxRows = old })
	st := newUsageState()
	now := time.Now().UTC()
	var wantReq int64
	for vm := 0; vm < 20; vm++ {
		for d := 0; d < 10; d++ {
			useVM(st, "t1", fmt.Sprintf("vm%d", vm), "openai", "m", now.AddDate(0, 0, -d).Format(time.DateOnly), 1, 1, 1)
			wantReq++
		}
	}
	pruneAIUsageDays(st, now)
	if got := len(st.AIUsageDays); got > aiUsageDaysMaxRows {
		t.Fatalf("per-day rows = %d, cap %d", got, aiUsageDaysMaxRows)
	}
	if req, _, _ := sumRows(st.AIUsage, "t1"); req != wantReq || len(st.AIUsage) != 20 {
		t.Fatalf("lifetime rows changed: %d requests in %d rows", req, len(st.AIUsage))
	}
	// the newest day is intact (the oldest were folded/dropped first)
	today := now.Format(time.DateOnly)
	var todayReq int64
	for _, r := range st.AIUsageDays {
		if r.Day == today {
			todayReq += r.Requests
		}
	}
	if todayReq != 20 {
		t.Fatalf("today's requests = %d, want 20", todayReq)
	}
	// 400-day retention still applies
	st.AIUsageDays["t1/old"] = &AIUsageRow{TeamID: "t1", Day: now.AddDate(0, 0, -401).Format(time.DateOnly)}
	pruneAIUsageDays(st, now)
	if st.AIUsageDays["t1/old"] != nil {
		t.Fatal("a row past the retention window was kept")
	}
}

// A request that ends after its VM was deleted counts for the team aggregate.
func TestR38AIFlushFoldsRetiredVM(t *testing.T) {
	e := setup(t)
	team := e.srv.store.Snapshot().Teams
	var tid string
	for id := range team {
		tid = id
	}
	if err := e.srv.store.Update(func(st *State) error {
		st.AIVMs["gone"] = &AIVM{TeamID: tid, Retired: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.srv.aiMeter.add(tid, "gone", "openai", "m", 5, 6)
	e.srv.aiMeter.flush(e.srv)
	st := e.srv.store.Snapshot()
	for k, r := range st.AIUsage {
		if r.VMID == "gone" {
			t.Errorf("row of the retired VM kept: %s", k)
		}
	}
	if r := st.AIUsage[aiUsageKey(tid, aiDeletedVM, "openai", "m")]; r == nil || r.Requests != 1 || r.InputTokens != 5 {
		t.Fatalf("aggregate = %+v", r)
	}
}

// Reads visit only the caller's team, include the unflushed batch, never change
// the stored rows, and follow every flush (the index is rebuilt after a write).
func TestR38AIUsageIndexedByTeam(t *testing.T) {
	st := newUsageState()
	day := time.Now().UTC().Format(time.DateOnly)
	for i := 0; i < 50; i++ {
		useVM(st, fmt.Sprintf("other%d", i%5), fmt.Sprintf("vm%d", i), "openai", "m", day, 1, 1, 1)
	}
	useVM(st, "mine", "vmA", "openai", "m", day, 2, 20, 2)
	useVM(st, "mine", "vmB", "openai", "m", day, 1, 10, 1)
	pending := map[string]*AIUsageRow{
		aiUsageKey("mine", "vmA", "openai", "m") + "/" + day:  {TeamID: "mine", VMID: "vmA", Provider: "openai", Model: "m", Day: day, Requests: 1, InputTokens: 5},
		aiUsageKey("mine", "vmC", "openai", "m2") + "/" + day: {TeamID: "mine", VMID: "vmC", Provider: "openai", Model: "m2", Day: day, Requests: 1},
		aiUsageKey("zzz", "vmZ", "openai", "m") + "/" + day:   {TeamID: "zzz", VMID: "vmZ", Provider: "openai", Model: "m", Day: day, Requests: 9},
	}
	rows := teamAIUsageRows(st, pending, "mine", "", true)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (vmA, vmB, vmC)", len(rows))
	}
	for _, r := range rows {
		if r.TeamID != "mine" {
			t.Errorf("foreign row %+v", r)
		}
		if r.VMID == "vmA" && (r.Requests != 3 || r.InputTokens != 25) {
			t.Errorf("vmA = %+v, want the batch added", r)
		}
	}
	if r := st.AIUsageDays[aiUsageKey("mine", "vmA", "openai", "m")+"/"+day]; r.Requests != 2 || r.InputTokens != 20 {
		t.Fatalf("the stored row was changed by a read: %+v", r)
	}
	if rows := teamAIUsageRows(st, nil, "mine", "vmB", false); len(rows) != 1 || rows[0].VMID != "vmB" {
		t.Fatalf("vm filter: %+v", rows)
	}
	// a mutator drops the index: the next read sees the change
	useVM(st, "mine", "vmD", "openai", "m", day, 1, 1, 1)
	st.dropAIUsageIndex()
	if rows := teamAIUsageRows(st, nil, "mine", "", false); len(rows) != 3 {
		t.Fatalf("after a write: %d rows, want 3", len(rows))
	}
	// the index is carried by clone but a flush through the store rebuilds it
	if cl := st.clone(); len(teamAIUsageRows(cl, nil, "mine", "", false)) != 3 {
		t.Fatal("clone lost rows")
	}
}

func TestR38AIUsageReadsFollowFlushes(t *testing.T) {
	e := setup(t)
	var tid string
	for id := range e.srv.store.Snapshot().Teams {
		tid = id
	}
	read := func() int64 {
		var n int64
		for _, r := range e.srv.aiUsageRows(tid, "") {
			n += r.Requests
		}
		return n
	}
	e.srv.aiMeter.add(tid, "vm1", "openai", "m", 1, 1)
	if read() != 1 { // unflushed
		t.Fatal("pending not shown")
	}
	e.srv.aiMeter.flush(e.srv)
	if read() != 1 {
		t.Fatal("flushed not shown")
	}
	e.srv.aiMeter.add(tid, "vm1", "openai", "m", 1, 1)
	e.srv.aiMeter.add(tid, "vm2", "openai", "m", 1, 1)
	if read() != 3 {
		t.Fatalf("pending over flushed: %d", read())
	}
	e.srv.aiMeter.flush(e.srv)
	e.srv.aiMeter.flush(e.srv)
	if read() != 3 || len(e.srv.aiUsageDays(tid, "", time.Now().AddDate(0, 0, -1))) != 2 {
		t.Fatalf("after second flush: %d", read())
	}
}

// ---- compat: the gate does not render usage ----

func TestR38CompatAIUsageGate(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	var tid string
	for id := range e.srv.store.Snapshot().Teams {
		tid = id
	}
	var vm string
	for id := range e.srv.store.Snapshot().CompatByVM {
		vm = id
	}
	e.srv.aiMeter.add(tid, vm, "openai", "m", 4, 5)
	e.srv.aiMeter.add(tid, "gone-vm", "openai", "m", 6, 7) // an old VM's usage, as folded on delete
	e.srv.aiMeter.flush(e.srv)
	if err := e.srv.store.Update(func(st *State) error {
		foldDeletedVMAIUsage(st, "gone-vm")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	u := e.ok("GET", "/api/v1/ai-usage?days=3", nil, 200)
	if tot := u["totals"].(map[string]any); tot["requests"] != 2.0 || tot["inputTokens"] != 10.0 {
		t.Fatalf("totals = %v (deleted VMs' usage stays in the team totals)", tot)
	}
	sbs := u["sandboxes"].([]any)
	if len(sbs) != 1 || sbs[0].(map[string]any)["sandboxId"] != bx {
		t.Fatalf("sandboxes = %v (the aggregate is not a sandbox)", sbs)
	}
	// the gate still refuses a key without the scope, and a VM that is not the team's
	if st, _, _ := e.compat(e.newKey("vms:write"), "GET", "/api/v1/ai-usage", nil, nil); st != 403 {
		t.Errorf("key without vms:read: %d, want 403", st)
	}
	if st, _, _ := e.compat(e.newKey("vms:write"), "GET", "/api/v1/sandboxes/"+bx+"/ai-usage", nil, nil); st != 403 {
		t.Errorf("sandbox usage, key without vms:read: %d, want 403", st)
	}
	if st, _, _ := e.compat(e.owner, "GET", "/api/v1/sandboxes/bx_nonexist/ai-usage", nil, nil); st != 404 {
		t.Errorf("unknown sandbox: %d, want 404", st)
	}
	su := e.ok("GET", "/api/v1/sandboxes/"+bx+"/ai-usage", nil, 200)
	if tot := su["totals"].(map[string]any); tot["requests"] != 1.0 {
		t.Fatalf("sandbox totals = %v", tot)
	}
}
