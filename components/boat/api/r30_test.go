package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// r30: env-failure lifecycle (cold start body, retry, exec gate, billing),
// per-day AI usage, no_env marker, AI stream revocation.

func (e *env) lastEnvVars() map[string]any {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	if len(e.boatd.envOps) == 0 {
		return nil
	}
	v, _ := e.boatd.envOps[len(e.boatd.envOps)-1]["vars"].(map[string]any)
	return v
}

func (e *env) spans(id string) []EnvErrorSpan { return e.srv.store.Snapshot().EnvErrorSpans[id] }

// 1: a cold start whose env push failed answers 200 with the VM (boatd did start
// it), agent_ready=false and env_error, like create and fork; compat resume answers
// the sandbox in state "error".
func TestR30ColdStartEnvFailureAnswersTheVM(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	e.setEnvFailures(false, true)
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil)
	if st != 200 || out["id"] != id || out["state"] != "running" || out["agent_ready"] != false || out["env_error"] == nil {
		t.Fatalf("cold start = %d %v", st, out)
	}
	if sp := e.spans(id); len(sp) != 1 || !sp[0].End.IsZero() {
		t.Fatalf("env-error span = %v, want one open", sp)
	}
	_, vm := e.do(e.owner, "GET", "/v1/vms/"+id, nil)
	if vm["env_error"] == nil {
		t.Fatalf("env_error not persisted: %v", vm)
	}

	bx := e.newSandbox(nil)
	base := "/api/v1/sandboxes/" + bx
	e.ok("POST", base+"/stop", nil, 202)
	cst, cout, _ := e.compat(e.owner, "POST", base+"/resume", nil, nil)
	sb, _ := cout["sandbox"].(map[string]any)
	if cst != 202 || sb["state"] != "error" || sb["error"] == nil || cout["status"] != "error" {
		t.Fatalf("compat resume = %d %v", cst, cout)
	}
}

// 2: an empty start of a running VM that still has env_error retries the push; a
// success clears it (and closes the span), a failure keeps it and still answers 200.
func TestR30EmptyStartRetriesTheEnvPush(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	e.setEnvFailures(true, false)
	bx := e.newSandbox(map[string]any{"env": map[string]string{"A": "1"}})
	id := e.nativeOf(bx)
	if _, vm := e.do(e.owner, "GET", "/v1/vms/"+id, nil); vm["env_error"] == nil {
		t.Fatalf("setup: no env_error: %v", vm)
	}
	// still failing: 200 with the error, not a bare 502
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil)
	if st != 200 || out["agent_ready"] != false || out["env_error"] == nil {
		t.Fatalf("failing retry = %d %v", st, out)
	}
	e.setEnvFailures(false, false)
	st, out = e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil)
	if st != 200 || out["env_error"] != nil || out["agent_ready"] == false {
		t.Fatalf("retry = %d %v", st, out)
	}
	vars := e.lastEnvVars()
	if vars["TEAM_SEC"] != "s3" || vars["BOAT_ID"] != id {
		t.Fatalf("retry env = %v, want team secrets and BOAT_ID", vars)
	}
	if sp := e.spans(id); len(sp) != 1 || sp[0].End.IsZero() {
		t.Fatalf("span after a good retry = %v, want one closed", sp)
	}
	if sb := e.ok("GET", "/api/v1/sandboxes/"+bx, nil, 200)["sandbox"].(map[string]any); sb["state"] == "error" {
		t.Fatalf("compat still error: %v", sb)
	}
	// a start with nothing to retry pushes nothing
	n := len(e.boatd.envOps)
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil); st != 200 || len(e.boatd.envOps) != n {
		t.Fatalf("clean no-op start pushed env (%d -> %d)", n, len(e.boatd.envOps))
	}
}

// 2+6: the retry honors the run's no_env, and no start of a running VM drops
// the no_env marker.
func TestR30RetryKeepsNoEnvRun(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "s3", "inject": true})
	id := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	e.setEnvFailures(false, true)
	// r37: a no_env cold start whose env push fails closes: 502, VM stopped.
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != 502 {
		t.Fatalf("failed no_env cold start = %d %v, want 502", st, out)
	}
	if _, out := e.do(e.owner, "GET", "/v1/vms/"+id, nil); out["state"] == "running" {
		t.Fatalf("failed no_env cold start left the VM running: %v", out)
	}
	e.setEnvFailures(false, false)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != 200 || out["env_error"] != nil {
		t.Fatalf("no_env start after the failure = %d %v", st, out)
	}
	e.boatd.mu.Lock()
	startVars, _ := e.boatd.starts[len(e.boatd.starts)-1]["env"].(map[string]any)
	e.boatd.mu.Unlock()
	if startVars["BOAT_ID"] != id || startVars["TEAM_SEC"] != nil {
		t.Fatalf("no_env start env = %v, want BOAT_ID and no team secrets", startVars)
	}
	if _, ok := e.srv.store.Snapshot().NoEnvRuns[id]; !ok {
		t.Fatal("no_env start left no marker")
	}
	// A plain start of the running no_env VM is a no-op: the marker stays...
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil); st != 200 {
		t.Fatalf("no-op start = %d", st)
	}
	if _, ok := e.srv.store.Snapshot().NoEnvRuns[id]; !ok {
		t.Fatal("a no-op start of a running no_env VM cleared its marker")
	}
	// ...and (r46: sticky) a cold start after a stop keeps it, with no team secrets.
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil); st != 200 {
		t.Fatalf("cold start = %d", st)
	}
	if _, ok := e.srv.store.Snapshot().NoEnvRuns[id]; !ok {
		t.Fatal("a cold plain start dropped the no_env marker")
	}
	e.boatd.mu.Lock()
	startVars, _ = e.boatd.starts[len(e.boatd.starts)-1]["env"].(map[string]any)
	e.boatd.mu.Unlock()
	if startVars["TEAM_SEC"] != nil {
		t.Fatalf("a plain cold start of a no_env VM pushed team secrets: %v", startVars)
	}
}

// 3: compat commands refuse (409 sandbox_not_ready) while the sandbox is in
// state error; native exec is unchanged; a retry lifts it.
func TestR30CompatCommandsRefuseWhileEnvError(t *testing.T) {
	e := setup(t)
	e.setEnvFailures(true, false)
	bx := e.newSandbox(nil)
	id := e.nativeOf(bx)
	for _, body := range []map[string]any{{"command": "echo hi"}, {"command": "echo hi", "detached": true}, {"command": "echo hi", "stream": true}} {
		st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/commands", body, nil)
		if st != 409 || errCode(out) != "sandbox_not_ready" {
			t.Fatalf("compat commands %v = %d %v, want 409 sandbox_not_ready", body, st, out)
		}
	}
	if st, _ := e.raw(e.owner, "POST", "/v1/vms/"+id+"/exec", map[string]any{"cmd": []string{"echo", "hi"}}); st != 200 {
		t.Fatalf("native exec = %d, want it unchanged", st)
	}
	e.setEnvFailures(false, false)
	e.ok("POST", "/api/v1/sandboxes/"+bx+"/resume", nil, 202)
	if st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/commands", map[string]any{"command": "echo hi"}, nil); st != 200 {
		t.Fatalf("compat command after the retry = %d %v", st, out)
	}
}

// 4: env-error spans open/close with the push results and stops, and are taken
// out of the usage reports, clipped to the window.
func TestR30SpansFollowTheLifecycle(t *testing.T) {
	e := setup(t)
	e.setEnvFailures(true, false)
	id := e.nativeOf(e.newSandbox(nil))
	if sp := e.spans(id); len(sp) != 1 || !sp[0].End.IsZero() {
		t.Fatalf("after a failed create: %v", sp)
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if sp := e.spans(id); len(sp) != 1 || sp[0].End.IsZero() {
		t.Fatalf("after stop: %v, want closed", sp)
	}
	// The same failure on the next cold start opens a new span.
	e.setEnvFailures(false, true)
	e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil)
	if sp := e.spans(id); len(sp) != 2 || !sp[1].End.IsZero() {
		t.Fatalf("after a failing cold start: %v, want two, the last open", sp)
	}
	e.do(e.owner, "DELETE", "/v1/vms/"+id, nil)
	waitFor(t, "span closed on delete", func() bool {
		sp := e.spans(id)
		return len(sp) == 2 && !sp[1].End.IsZero()
	})
}

func seedSpans(e *env, id string, spans ...EnvErrorSpan) {
	e.t.Helper()
	if err := e.srv.store.Update(func(st *State) error {
		st.EnvErrorSpans[id] = spans
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
}

func TestR30PerSandboxUsageExcludesEnvError(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	if e.nativeOf(bx) != "vm1" {
		t.Skip("fake usage report is for vm1")
	}
	now := time.Now().UTC().Truncate(time.Second)
	e.boatd.mu.Lock()
	e.boatd.usageWindowFn = func(q url.Values) map[string]any {
		since, _ := time.Parse(time.RFC3339Nano, q.Get("since"))
		return map[string]any{"exact": true, "since": since, "until": now, "intervals": []any{
			map[string]any{"kind": "vm", "id": "vm1", "team": q.Get("team"), "size": "default", "start": now.Add(-time.Hour), "end": now},
		}}
	}
	e.boatd.mu.Unlock()
	seedSpans(e, "vm1", EnvErrorSpan{Start: now.Add(-60 * time.Minute), End: now.Add(-30 * time.Minute)})
	// whole span inside the window
	since := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	_, u := e.do(e.owner, "GET", "/v1/vms/vm1/usage?since="+since, nil)
	if got := u["vm_seconds"].(float64); got != 1800 {
		t.Fatalf("usage = %v, want 3600-1800", u)
	}
	// The window clips runtime to 2700 seconds and the env-error span to 900.
	since = now.Add(-45 * time.Minute).UTC().Format(time.RFC3339)
	_, u = e.do(e.owner, "GET", "/v1/vms/vm1/usage?since="+since, nil)
	if got := u["vm_seconds"].(float64); got != 1800 {
		t.Fatalf("clipped usage = %v, want 2700-900", u)
	}
	// Compat billing has no authoritative provider pricing basis.
	if st, cu, _ := e.compat(e.owner, "GET", "/api/v1/sandboxes/"+bx+"/usage?since="+since, nil, nil); st != 501 || errCode(cu) != "not_supported" {
		t.Fatalf("compat usage = %d %v, want 501 not_supported", st, cu)
	}
	// an open span on a running VM runs to now
	seedSpans(e, "vm1", EnvErrorSpan{Start: now.Add(-10 * time.Minute)})
	_, u = e.do(e.owner, "GET", "/v1/vms/vm1/usage?since="+now.Add(-time.Hour).UTC().Format(time.RFC3339), nil)
	if got := u["vm_seconds"].(float64); got != 3000 {
		t.Fatalf("open-span usage = %v, want 3600-600", u)
	}
}

func TestR30TeamUsageExcludesEnvError(t *testing.T) {
	e := setup(t)
	now := time.Now()
	bd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		team := r.URL.Query().Get("team")
		_ = json.NewEncoder(w).Encode(map[string]any{"since": r.URL.Query().Get("since"), "until": "now",
			"vms": []any{
				map[string]any{"id": "vmA", "team": team, "size": "small", "vm_seconds": 7200.0},
				map[string]any{"id": "vmB", "team": team, "size": "default", "vm_seconds": 100.0},
			},
			"teams": []any{map[string]any{"team": team, "vm_seconds": 7300.0, "vm_seconds_by_size": map[string]any{"small": 7200.0, "default": 100.0}}}})
	}))
	t.Cleanup(bd.Close)
	e.srv.boatd = newBoatd(bd.URL)
	seedSpans(e, "vmA", EnvErrorSpan{Start: now.Add(-3 * time.Hour), End: now.Add(-2 * time.Hour)})
	since := url_(now.Add(-150 * time.Minute)) // clips the span to its last 30 minutes
	_, out := e.do(e.owner, "GET", "/v1/usage?since="+since, nil)
	vms := out["vms"].([]any)
	if got := vms[0].(map[string]any)["vm_seconds"].(float64); got < 5390 || got > 5410 {
		t.Fatalf("vmA = %v, want 7200-1800", vms[0])
	}
	if got := vms[1].(map[string]any)["vm_seconds"]; got != 100.0 {
		t.Fatalf("vmB = %v, want untouched", vms[1])
	}
	tm := out["teams"].([]any)[0].(map[string]any)
	by := tm["vm_seconds_by_size"].(map[string]any)
	if got := tm["vm_seconds"].(float64); got < 5490 || got > 5510 || by["default"] != 100.0 || by["small"].(float64) > 5410 {
		t.Fatalf("team = %v", tm)
	}
	// a window ending before the span starts is untouched
	_, out = e.do(e.owner, "GET", "/v1/usage?from="+url_(now.Add(-6*time.Hour))+"&to="+url_(now.Add(-4*time.Hour)), nil)
	if got := out["vms"].([]any)[0].(map[string]any)["vm_seconds"]; got != 0.0 {
		// the stub is cumulative-constant, so [from,to] = full - after = 0 here;
		// the point is only that nothing negative or double-counted shows up.
		t.Fatalf("window before the span = %v", out["vms"])
	}
}

func url_(t time.Time) string { return strings.ReplaceAll(t.UTC().Format(time.RFC3339), "+", "%2B") }

// 5: AI usage is counted per UTC day; ?days=N sums only its window, one row per
// day per credential/model; native totals stay lifetime sums.
func TestR30AIUsagePerDay(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	vm := e.nativeOf(bx)
	other := e.createVM()
	team := e.teamID()
	now := time.Now().UTC()
	day := func(n int) time.Time { return now.AddDate(0, 0, -n) }
	m := e.srv.aiMeter
	m.addAt(day(0), team, vm, "anthropic", "m", 10, 1)
	m.addAt(day(0), team, vm, "anthropic", "m", 10, 1)
	m.addAt(day(0), team, other, "anthropic", "m", 5, 5) // same key from another VM: merged at team level
	m.addAt(day(2), team, vm, "anthropic", "m", 7, 7)
	m.addAt(day(10), team, vm, "anthropic", "m", 100, 100)
	m.flush(e.srv)

	u := e.ok("GET", "/api/v1/ai-usage?days=3", nil, 200)
	rows := u["usage"].([]any)
	if len(rows) != 2 {
		t.Fatalf("team rows = %v, want one per day (2)", rows)
	}
	r0 := rows[0].(map[string]any) // oldest first
	r1 := rows[1].(map[string]any)
	if r0["day"] != day(2).Format("2006-01-02") || r0["requests"] != 1.0 || r0["inputTokens"] != 7.0 ||
		r1["day"] != day(0).Format("2006-01-02") || r1["requests"] != 3.0 || r1["inputTokens"] != 25.0 || r1["outputTokens"] != 7.0 {
		t.Fatalf("team rows = %v", rows)
	}
	if tot := u["totals"].(map[string]any); tot["requests"] != 4.0 || tot["inputTokens"] != 32.0 {
		t.Fatalf("totals = %v (the 10-day-old row must be outside)", tot)
	}
	if len(u["sandboxes"].([]any)) != 2 {
		t.Fatalf("sandboxes = %v", u["sandboxes"])
	}
	if wide := e.ok("GET", "/api/v1/ai-usage?days=30", nil, 200); wide["totals"].(map[string]any)["requests"] != 5.0 || len(wide["usage"].([]any)) != 3 {
		t.Fatalf("days=30 = %v", wide)
	}

	su := e.ok("GET", "/api/v1/sandboxes/"+bx+"/ai-usage?days=3", nil, 200)
	srows := su["usage"].([]any)
	if len(srows) != 2 || su["totals"].(map[string]any)["requests"] != 3.0 || su["totals"].(map[string]any)["inputTokens"] != 27.0 {
		t.Fatalf("sandbox usage = %v", su)
	}

	// native: lifetime sums (rows per VM/provider/model), unchanged shape
	_, nat := e.do(e.owner, "GET", "/v1/ai/usage", nil)
	if tot := nat["totals"].(map[string]any); tot["requests"] != 5.0 || tot["input_tokens"] != 132.0 {
		t.Fatalf("native totals = %v", tot)
	}
	_, nv := e.do(e.owner, "GET", "/v1/vms/"+vm+"/ai-usage", nil)
	if tot := nv["totals"].(map[string]any); tot["requests"] != 4.0 {
		t.Fatalf("native vm totals = %v", tot)
	}
}

// 5: lifetime rows that predate per-day counters are attributed, once, to their
// Updated day; old ones fall out of retention.
func TestR30AIUsageMigration(t *testing.T) {
	st := &State{}
	st.init()
	upd := time.Date(2026, 1, 2, 23, 59, 0, 0, time.UTC)
	st.AIUsage["t/vm1/anthropic/m"] = &AIUsageRow{TeamID: "t", VMID: "vm1", Provider: "anthropic", Model: "m", Requests: 9, InputTokens: 90, OutputTokens: 9, Updated: upd}
	st.AIUsage["t/vm1/anthropic/old"] = &AIUsageRow{TeamID: "t", VMID: "vm1", Provider: "anthropic", Model: "old", Requests: 1, Updated: time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)}
	migrateAIUsageDays(st)
	row := st.AIUsageDays["t/vm1/anthropic/m/2026-01-02"]
	if row == nil || row.Requests != 9 || row.InputTokens != 90 || row.Day != "2026-01-02" || !st.AIUsageDaysV1 {
		t.Fatalf("migrated = %+v", st.AIUsageDays)
	}
	if len(st.AIUsageDays) != 1 {
		t.Fatalf("retention: %v", st.AIUsageDays)
	}
	migrateAIUsageDays(st) // once
	if st.AIUsageDays["t/vm1/anthropic/m/2026-01-02"].Requests != 9 {
		t.Fatal("migrated twice")
	}
	// the clone keeps the flag and rows
	if cl := st.clone(); !cl.AIUsageDaysV1 || cl.AIUsageDays["t/vm1/anthropic/m/2026-01-02"].Requests != 9 {
		t.Fatal("clone lost per-day rows")
	}
}

func TestR30AIUsageRetention(t *testing.T) {
	st := &State{}
	st.init()
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -(aiUsageRetentionDays + 1)).Format(time.DateOnly)
	keep := now.AddDate(0, 0, -(aiUsageRetentionDays - 1)).Format(time.DateOnly)
	st.AIUsageDays["a/"+old] = &AIUsageRow{Day: old}
	st.AIUsageDays["a/"+keep] = &AIUsageRow{Day: keep}
	pruneAIUsageDays(st, now)
	if len(st.AIUsageDays) != 1 || st.AIUsageDays["a/"+keep] == nil {
		t.Fatalf("after prune: %v", st.AIUsageDays)
	}
}

func openAIStream(t *testing.T, e *env, up *aiFake, proxyURL, id string) chan struct{} {
	t.Helper()
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
	req, _ := http.NewRequest("POST", proxyURL+"/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
	req.Header.Set("X-Api-Key", tok)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != 200 {
		t.Fatalf("stream: %d", resp.StatusCode)
	}
	ended := make(chan struct{})
	go func() {
		io.Copy(io.Discard, resp.Body)
		close(ended)
	}()
	return ended
}

// 7: an open AI stream ends when the team's credential is replaced or deleted,
// or the team switches the proxy off.
func TestR30AIStreamEndsWhenCredentialChanges(t *testing.T) {
	for name, change := range map[string]func(e *env){
		"replaced": func(e *env) {
			e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "sk-ant-OTHER-KEY-654321"})
		},
		"deleted":   func(e *env) { e.do(e.owner, "DELETE", "/v1/ai/credentials/anthropic", nil) },
		"proxy off": func(e *env) { e.do(e.owner, "PUT", "/v1/ai/proxy", map[string]any{"enabled": false}) },
	} {
		t.Run(name, func(t *testing.T) {
			e, up, proxy := aiSetup(t)
			e.srv.streamRecheck = 50 * time.Millisecond
			if st, c := e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "sk-ant-REAL-KEY-123456"}); st != 200 {
				t.Fatalf("credential: %d %v", st, c)
			}
			id := e.createVM()
			ended := openAIStream(t, e, up, proxy.URL, id)
			expectOpen(t, "AI stream before the change", ended, 400*time.Millisecond)
			change(e)
			expectEnds(t, "AI stream after the credential "+name, ended, 3*time.Second)
		})
	}
}
