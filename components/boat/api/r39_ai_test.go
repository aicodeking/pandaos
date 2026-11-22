package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- r39: per-team body budget, progress rule, pooled upstream client,
// bounded per-day usage, deleteTeam, kept index, escaped upstream paths ----

func withR39Budgets(t *testing.T, global, team, vm int64) {
	t.Helper()
	g, tm, v := aiBudgetGlobal, aiBudgetPerTeam, aiBudgetPerVM
	aiBudgetGlobal, aiBudgetPerTeam, aiBudgetPerVM = global, team, vm
	t.Cleanup(func() { aiBudgetGlobal, aiBudgetPerTeam, aiBudgetPerVM = g, tm, v })
}

func withR39Progress(t *testing.T, minBytes int, window time.Duration) {
	t.Helper()
	b, w := aiMinProgress, aiProgressWindow
	aiMinProgress, aiProgressWindow = minBytes, window
	t.Cleanup(func() { aiMinProgress, aiProgressWindow = b, w })
}

// aiTeamVMs gives an existing team an openai credential and n registered VMs;
// it returns their proxy tokens.
func aiTeamVMs(t *testing.T, e *env, team, prefix string, n int) []string {
	t.Helper()
	sealed, err := e.srv.keys.Seal([]byte("sk-REAL-"+team), "ai/"+aiCredKey(team, "openai"))
	if err != nil {
		t.Fatal(err)
	}
	toks := make([]string, n)
	if err := e.srv.store.Update(func(st *State) error {
		st.AICreds[aiCredKey(team, "openai")] = &AICredential{TeamID: team, Provider: "openai", Sealed: sealed, Last4: "abcd", Updated: time.Now().UTC()}
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("%s-%d", prefix, i)
			st.AIVMs[id] = &AIVM{TeamID: team, Gen: 1}
			toks[i] = e.srv.aiToken(id, 1)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return toks
}

func bodyOf(n int) string { return `{"stream":true,"p":"` + strings.Repeat("y", n-23) + `"}` }

// One team's VMs cannot take the budget of another team: past the team's share
// its next body gets 429, and another team's body still goes through.
func TestR39AITeamBudgetIsolatesTeams(t *testing.T) {
	e, up, proxy := aiSetup(t)
	e.srv.aiLim = newLimiter(0, 0)
	withR39Budgets(t, 40<<10, 20<<10, 1<<30)
	teamA, _ := e.newTeam("alpha")
	teamB, _ := e.newTeam("beta")
	a := aiTeamVMs(t, e, teamA, "a", 3)
	b := aiTeamVMs(t, e, teamB, "b", 1)
	path := "/openai/v1/chat/completions"
	stallRequest(t, proxy.URL, path, a[0], 100000, 5000) // each holds an 8 KiB buffer
	stallRequest(t, proxy.URL, path, a[1], 100000, 5000)
	waitHeld(t, e, 16384)

	st, h := postStatus(t, proxy.URL, a[2], bodyOf(5000), false)
	if st != 429 || h.Get("Retry-After") == "" {
		t.Fatalf("third VM of the full team: %d (Retry-After %q), want 429", st, h.Get("Retry-After"))
	}
	if st, _ := postStatus(t, proxy.URL, b[0], bodyOf(5000), false); st != 200 {
		t.Fatalf("another team's body: %d, want 200", st)
	}
	up.mu.Lock()
	n := len(up.bodies)
	up.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d bodies reached the provider, want only team B's", n)
	}
	e.srv.aiInflight.mu.Lock()
	held := e.srv.aiInflight.heldTeam[teamA]
	e.srv.aiInflight.mu.Unlock()
	if held != 16384 {
		t.Fatalf("team A holds %d, want 16384", held)
	}
}

// Without the team share the shared budget is what one team can exhaust: the
// 503 the team budget exists to keep off other teams.
func TestR39AIGlobalBudgetStillApplies(t *testing.T) {
	e, _, proxy := aiSetup(t)
	e.srv.aiLim = newLimiter(0, 0)
	withR39Budgets(t, 20<<10, 1<<30, 1<<30)
	teamA, _ := e.newTeam("alpha")
	teamB, _ := e.newTeam("beta")
	a := aiTeamVMs(t, e, teamA, "a", 2)
	b := aiTeamVMs(t, e, teamB, "b", 1)
	path := "/openai/v1/chat/completions"
	stallRequest(t, proxy.URL, path, a[0], 100000, 5000)
	stallRequest(t, proxy.URL, path, a[1], 100000, 5000)
	waitHeld(t, e, 16384)
	if st, h := postStatus(t, proxy.URL, b[0], bodyOf(5000), false); st != 503 || h.Get("Retry-After") == "" {
		t.Fatalf("status %d, want 503 + Retry-After", st)
	}
}

func TestR39AIByteReservationTeam(t *testing.T) {
	l := newInflight(0)
	a := l.reserveTeam("vm1", "t1", 100, 120, 1000)
	b := l.reserveTeam("vm2", "t1", 100, 120, 1000)
	c := l.reserveTeam("vm3", "t2", 100, 120, 1000)
	if err := a.add(90); err != nil {
		t.Fatal(err)
	}
	err := b.add(40)
	if eb, ok := err.(*errByteBudget); !ok || !eb.perTeam || eb.perKey {
		t.Fatalf("team limit not reported: %v", err)
	}
	if err := c.add(100); err != nil {
		t.Fatalf("another team: %v", err)
	}
	if aiBudgetError(err).status != 429 {
		t.Fatal("a team limit must be 429, not 503")
	}
	a.release()
	b.release()
	c.release()
	if l.heldTotal != 0 || len(l.held) != 0 || len(l.heldTeam) != 0 {
		t.Fatalf("leak: %d %v %v", l.heldTotal, l.held, l.heldTeam)
	}
}

// A declared Content-Length is not charged: a client that declares 30 MiB and
// sends 100 bytes holds one small buffer. Over the body cap it is refused.
func TestR39AIChargesBytesAsTheyArrive(t *testing.T) {
	e, up, base, tok := aiOpenAIVM(t)
	withR39Budgets(t, 1<<30, 1<<30, 64<<10)
	stallRequest(t, base, "/openai/v1/chat/completions", tok, 30<<20, 100)
	waitHeld(t, e, 4096)
	// The VM still has room for real work while the declaration stands.
	if st, _ := postStatus(t, base, tok, `{"stream":true}`, false); st != 200 {
		t.Fatalf("status %d, want 200", st)
	}
	over := stallRequest(t, base, "/openai/v1/chat/completions", tok, aiMaxBody+1, 100)
	over.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(over), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("declared over the cap: %d, want 413", resp.StatusCode)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.bodies) != 1 {
		t.Fatalf("%d bodies reached the provider, want 1", len(up.bodies))
	}
}

// A body that trickles in (a byte now and then) is cut off by the progress rule,
// long before the overall deadline; its bytes come back.
func TestR39AIProgressRuleAbortsTrickle(t *testing.T) {
	e, up, base, tok := aiOpenAIVM(t)
	withR39Progress(t, 64<<10, 300*time.Millisecond)
	conn := stallRequest(t, base, "/openai/v1/chat/completions", tok, 200000, 100)
	waitHeld(t, e, 4096)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				if _, err := conn.Write([]byte("z")); err != nil {
					return
				}
			}
		}
	}()
	start := time.Now()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("a trickling body got no answer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 408 {
		t.Fatalf("status %d, want 408", resp.StatusCode)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("cut off after %v, want about one window", d)
	}
	waitHeld(t, e, 0)
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.bodies) != 0 {
		t.Fatal("a trickled body reached the provider")
	}
}

// A body that keeps the pace (the minimum every window) is not cut off even if
// it takes longer than one window.
func TestR39AIProgressRuleLetsSteadyBodyThrough(t *testing.T) {
	e, up, base, tok := aiOpenAIVM(t)
	withR39Progress(t, 1024, 500*time.Millisecond)
	body := bodyOf(4400)
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /openai/v1/chat/completions HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n", tok, len(body))
	for i := 0; i < len(body); i += 1100 {
		io.WriteString(conn, body[i:min(i+1100, len(body))])
		time.Sleep(150 * time.Millisecond)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("a steady body: %d, want 200", resp.StatusCode)
	}
	if got := lastUpstreamBody(up); !strings.Contains(got, `"include_usage":true`) || !strings.Contains(got, strings.Repeat("y", 4000)) {
		t.Fatalf("body not forwarded whole and patched (%d bytes)", len(got))
	}
	waitHeld(t, e, 0)
}

// The budget comes back once the body is on the wire, not when the provider's
// headers return.
func TestR39AIBudgetReleasedAfterBodyIsSent(t *testing.T) {
	e, up, base, tok := aiOpenAIVM(t)
	hold := make(chan struct{})
	up.mu.Lock()
	up.reply = func(w http.ResponseWriter, r *http.Request) {
		<-hold // the provider "thinks": the body is long read, no header sent
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"m","usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}
	up.mu.Unlock()
	done := make(chan int, 1)
	go func() {
		st, _ := postStatus(t, base, tok, bodyOf(5000), false)
		done <- st
	}()
	waitUntil36(t, "the provider to have the body", func() bool {
		up.mu.Lock()
		defer up.mu.Unlock()
		return len(up.bodies) == 1
	})
	waitHeld(t, e, 0) // while the answer is still pending
	select {
	case st := <-done:
		t.Fatalf("the request ended early: %d", st)
	default:
	}
	close(hold)
	if st := <-done; st != 200 {
		t.Fatalf("status %d, want 200", st)
	}
	waitHeld(t, e, 0)
}

// A pooled connection that died is retried with the held body (GetBody).
func TestR39AIHeldBodyReplays(t *testing.T) {
	h := &aiHeldBody{buf: []byte("abc"), res: newInflight(0).reserve("k", 0, 0)}
	rc, err := h.getBody()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(rc); string(b) != "abc" {
		t.Fatalf("replay = %q", b)
	}
	first := h.reader()
	h.drop()
	h.drop() // twice: harmless
	if _, err := h.getBody(); err == nil {
		t.Fatal("a dropped body can be replayed")
	}
	if _, err := io.ReadAll(first); err == nil {
		t.Fatal("a dropped body can be read")
	}
}

// ---- upstream client ----

func tlsClientFor(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	c := newAIClient()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// Eight callers at once, three rounds: the pool keeps the connections (the
// default of two idle per host dialed again every round), and with HTTP/2 a
// handful of connections carry everything.
func TestR39AIClientReusesConnections(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("h2=%v", h2), func(t *testing.T) {
			var conns, protoH2 atomic.Int64
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if r.ProtoMajor == 2 {
					protoH2.Add(1)
				}
				time.Sleep(40 * time.Millisecond) // keep the callers concurrent
				io.WriteString(w, "ok")
			}))
			srv.EnableHTTP2 = h2
			srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
				if s == http.StateNew {
					conns.Add(1)
				}
			}
			srv.StartTLS()
			defer srv.Close()
			c := tlsClientFor(t, srv)
			for round := 0; round < 3; round++ {
				var wg sync.WaitGroup
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						resp, err := c.Post(srv.URL, "application/json", strings.NewReader(`{}`))
						if err != nil {
							t.Error(err)
							return
						}
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
					}()
				}
				wg.Wait()
			}
			if n := conns.Load(); n > 10 {
				t.Fatalf("%d connections for 24 requests, want <= 10 (reuse)", n)
			}
			if h2 && protoH2.Load() != 24 {
				t.Fatalf("%d of 24 requests used HTTP/2", protoH2.Load())
			}
		})
	}
}

func TestR39AIClientPoolSettings(t *testing.T) {
	tr := newAIClient().Transport.(*http.Transport)
	if tr.MaxIdleConnsPerHost != 64 || tr.MaxIdleConns != 256 || !tr.ForceAttemptHTTP2 {
		t.Fatalf("pool = %d/%d h2=%v", tr.MaxIdleConnsPerHost, tr.MaxIdleConns, tr.ForceAttemptHTTP2)
	}
}

// ---- escaped upstream paths ----

func TestR39AIUpstreamPathStaysEscaped(t *testing.T) {
	_, up, base, tok := aiOpenAIVM(t)
	for _, tc := range []struct{ in, want string }{
		{"/openai/v1/models/x%3Fa=b", "/v1/models/x%3Fa=b"},
		{"/openai/v1/models/x%23frag", "/v1/models/x%23frag"},
		{"/openai/v1/models/a%20b", "/v1/models/a%20b"},
		{"/openai/v1/models/x%3Fa=b?real=1", "/v1/models/x%3Fa=b?real=1"},
		{"/openai/v1/models/gpt-4o", "/v1/models/gpt-4o"},
	} {
		req, _ := http.NewRequest("GET", base+tc.in, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", tc.in, resp.StatusCode)
		}
		up.mu.Lock()
		got := up.paths[len(up.paths)-1]
		up.mu.Unlock()
		if got != tc.want {
			t.Errorf("%s reached the provider as %q, want %q", tc.in, got, tc.want)
		}
	}
	for in, want := range map[string]string{"v1/models/x?a=b": "v1/models/x%3Fa=b", "a/b#c": "a/b%23c", "plain": "plain"} {
		if got := aiEscapePath(in); got != want {
			t.Errorf("aiEscapePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- per-day usage cap ----

// refCapAIUsageDays is the previous algorithm (sort every key), the reference
// the windowed cap must agree with.
func refCapAIUsageDays(m map[string]*AIUsageRow, limit int) {
	order := func() []string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, b := m[keys[i]], m[keys[j]]
			if a.Day != b.Day {
				return a.Day < b.Day
			}
			return keys[i] < keys[j]
		})
		return keys
	}
	for _, k := range order() {
		if len(m) <= limit {
			return
		}
		if r := m[k]; r != nil && r.VMID != aiDeletedVM {
			foldAIRow(m, k, r)
		}
	}
	for _, k := range order() {
		if len(m) <= limit {
			return
		}
		delete(m, k)
	}
}

func randomDayRows(seed int64, n int) map[string]*AIUsageRow {
	rnd := rand.New(rand.NewSource(seed))
	now := time.Now().UTC()
	m := map[string]*AIUsageRow{}
	for i := 0; i < n; i++ {
		team := fmt.Sprintf("t%d", rnd.Intn(3))
		vm := fmt.Sprintf("vm%d", rnd.Intn(60))
		if rnd.Intn(8) == 0 {
			vm = aiDeletedVM
		}
		prov, model := []string{"openai", "mistral"}[rnd.Intn(2)], fmt.Sprintf("m%d", rnd.Intn(3))
		day := now.AddDate(0, 0, -rnd.Intn(40)).Format(time.DateOnly)
		r := &AIUsageRow{TeamID: team, VMID: vm, Provider: prov, Model: model, Day: day, Requests: 1 + int64(rnd.Intn(5)), InputTokens: int64(rnd.Intn(100)), Updated: now}
		m[aiUsageKey(team, vm, prov, model)+"/"+day] = r
	}
	return m
}

func copyRows(m map[string]*AIUsageRow) map[string]*AIUsageRow {
	out := make(map[string]*AIUsageRow, len(m))
	for k, r := range m {
		cp := *r
		out[k] = &cp
	}
	return out
}

func TestR39AIUsageDaysCapMatchesFullSort(t *testing.T) {
	old := aiUsageDaysMaxRows
	t.Cleanup(func() { aiUsageDaysMaxRows = old })
	for _, limit := range []int{40, 300, 1200, 2400} {
		for seed := int64(1); seed <= 4; seed++ {
			aiUsageDaysMaxRows = limit
			rows := randomDayRows(seed, 3000)
			ref := copyRows(rows)
			st := newUsageState()
			st.AIUsageDays = copyRows(rows)
			pruneAIUsageDays(st, time.Now())
			refCapAIUsageDays(ref, limit)
			if len(st.AIUsageDays) != len(ref) {
				t.Fatalf("limit %d seed %d: %d rows, reference %d", limit, seed, len(st.AIUsageDays), len(ref))
			}
			for k, want := range ref {
				got := st.AIUsageDays[k]
				if got == nil || got.Requests != want.Requests || got.InputTokens != want.InputTokens {
					t.Fatalf("limit %d seed %d: row %s = %+v, reference %+v", limit, seed, k, got, want)
				}
			}
		}
	}
}

// At the cap a flush that adds a few rows folds a few: it sorts a window of the
// oldest days, not the table, and finishes quickly.
func TestR39AIUsageDaysOverCapFlushBounded(t *testing.T) {
	if aiUsageDaysMaxRows != 20000 {
		t.Fatalf("per-day cap = %d, want 20000", aiUsageDaysMaxRows)
	}
	e := setup(t)
	now := time.Now().UTC()
	if err := e.srv.store.Update(func(st *State) error {
		st.Teams["t1"] = &Team{ID: "t1", Name: "t1"} // r40: the flush skips rows of unknown teams
		for vm := 0; vm < 200; vm++ {
			for d := 0; d < 100; d++ { // 20000 rows: exactly at the cap
				useVM(st, "t1", fmt.Sprintf("vm%d", vm), "openai", "m", now.AddDate(0, 0, -d).Format(time.DateOnly), 1, 1, 1)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.srv.store.Snapshot().AIUsageDays); n != 20000 {
		t.Fatalf("setup: %d rows", n)
	}
	for round := 0; round < 5; round++ {
		for i := 0; i < 30; i++ {
			e.srv.aiMeter.add("t1", fmt.Sprintf("new%d-%d", round, i), "openai", "m", 1, 1)
		}
		before := aiCapSortedRows.Load()
		start := time.Now()
		e.srv.aiMeter.flushFinal(e.srv)
		took := time.Since(start)
		if n := len(e.srv.store.Snapshot().AIUsageDays); n > aiUsageDaysMaxRows {
			t.Fatalf("round %d: %d per-day rows, cap %d", round, n, aiUsageDaysMaxRows)
		}
		if sorted := aiCapSortedRows.Load() - before; sorted > 2000 {
			t.Fatalf("round %d: the cap sorted %d keys, want a window (<= 2000) of the 20000", round, sorted)
		}
		if took > 5*time.Second {
			t.Fatalf("round %d: the flush took %v", round, took)
		}
	}
	var life int64
	for _, r := range e.srv.store.Snapshot().AIUsage {
		life += r.Requests
	}
	if life != 20000+5*30 {
		t.Fatalf("lifetime requests = %d: the cap must not touch them", life)
	}
}

// ---- deleteTeam ----

func TestR39DeleteTeamDropsAIUsage(t *testing.T) {
	e := setup(t)
	tid, _ := e.newTeam("doomed")
	keep := e.teamID()
	now := time.Now().UTC()
	e.srv.aiMeter.addAt(now, tid, "vmx", "openai", "m", 3, 4)
	e.srv.aiMeter.addAt(now, keep, "vmk", "openai", "m", 5, 6)
	e.srv.aiMeter.flushFinal(e.srv)
	// Build the index, then leave a count unflushed: both must go with the team.
	e.srv.aiUsageRows(tid, "")
	e.srv.aiMeter.addAt(now, tid, "vmx", "openai", "m", 1, 1)
	if st, out := e.do(e.owner, "DELETE", "/v1/teams/"+tid, nil); st != 200 {
		t.Fatalf("delete team: %d %v", st, out)
	}
	snap := e.srv.store.Snapshot()
	for _, m := range []map[string]*AIUsageRow{snap.AIUsage, snap.AIUsageDays} {
		for k, r := range m {
			if r.TeamID == tid {
				t.Errorf("usage row of the deleted team kept: %s", k)
			}
		}
	}
	if len(snap.AIUsage) != 1 || len(snap.AIUsageDays) != 1 {
		t.Fatalf("other teams' rows changed: %d lifetime, %d per-day", len(snap.AIUsage), len(snap.AIUsageDays))
	}
	if rows := e.srv.aiUsageRows(tid, ""); len(rows) != 0 {
		t.Fatalf("the index still lists %d rows of the deleted team", len(rows))
	}
	e.srv.aiMeter.flushFinal(e.srv)
	for _, r := range e.srv.store.Snapshot().AIUsage {
		if r.TeamID == tid {
			t.Fatal("an unflushed count resurrected a row of the deleted team")
		}
	}
	if rows := e.srv.aiUsageRows(keep, ""); len(rows) != 1 || rows[0].Requests != 1 {
		t.Fatalf("the other team's usage: %+v", rows)
	}
}

// ---- the per-team index survives counter-only flushes ----

func TestR39AIUsageIndexKeptAcrossCounterFlushes(t *testing.T) {
	e := setup(t)
	m, s := e.srv.aiMeter, e.srv
	if err := s.store.Update(func(st *State) error { // r40: the flush skips rows of unknown teams
		st.Teams["tx"] = &Team{ID: "tx", Name: "tx"}
		st.Teams["ty"] = &Team{ID: "ty", Name: "ty"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Format(time.DateOnly)
	sum := func(st *State, perDay bool) (n int64) {
		for _, r := range teamAIUsageRows(st, nil, "tx", "", perDay) {
			n += r.Requests
		}
		return
	}
	m.add("tx", "vm1", "openai", "m", 1, 1)
	m.add("tx", "vm2", "openai", "m", 1, 1)
	m.add("ty", "vm3", "openai", "m", 1, 1)
	m.flushFinal(s) // new keys: no index
	if s.store.Snapshot().sharedAIUsageIndex() != nil {
		t.Fatal("a flush that added keys kept a stale index")
	}
	first := s.store.Snapshot()
	if sum(first, false) != 2 || first.sharedAIUsageIndex() == nil {
		t.Fatal("index not built by a read")
	}
	m.add("tx", "vm1", "openai", "m", 1, 1)
	m.add("tx", "vm1", "openai", "m", 1, 1)
	m.flushFinal(s) // counters only
	second := s.store.Snapshot()
	if second.sharedAIUsageIndex() == nil {
		t.Fatal("a counter-only flush dropped the index")
	}
	if got := sum(second, false); got != 4 {
		t.Fatalf("lifetime after the flush = %d, want 4 (the index must follow the new rows)", got)
	}
	if got := sum(second, true); got != 4 {
		t.Fatalf("per-day after the flush = %d, want 4", got)
	}
	if got := sum(first, false); got != 2 {
		t.Fatalf("the published snapshot changed: %d, want 2", got)
	}
	if rows := teamAIUsageRows(second, nil, "tx", "vm1", false); len(rows) != 1 || rows[0].Requests != 3 {
		t.Fatalf("vm index: %+v", rows)
	}
	for _, r := range teamAIUsageRows(second, nil, "tx", "", true) {
		if w := second.AIUsageDays[aiUsageKey(r.TeamID, r.VMID, r.Provider, r.Model)+"/"+day]; w == nil || w.Requests != r.Requests {
			t.Fatalf("the index lists %+v, the state has %+v", r, w)
		}
	}
	// A write to something else carries the index over unchanged.
	if err := s.store.Update(func(st *State) error { st.Seq++; return nil }); err != nil {
		t.Fatal(err)
	}
	if s.store.Snapshot().sharedAIUsageIndex() == nil {
		t.Fatal("an unrelated write dropped the index")
	}
	// A new key invalidates it.
	m.add("tx", "vm9", "openai", "m", 1, 1)
	m.flushFinal(s)
	if s.store.Snapshot().sharedAIUsageIndex() != nil {
		t.Fatal("a new key kept the index")
	}
	if got := sum(s.store.Snapshot(), false); got != 5 {
		t.Fatalf("after the new key = %d, want 5", got)
	}
}
