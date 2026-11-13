package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// execAsync posts a "slow" exec and returns its reply lines once it ends.
func execAsync(t *testing.T, srv *httptest.Server, id string) <-chan []map[string]any {
	t.Helper()
	out := make(chan []map[string]any, 1)
	go func() {
		b, _ := json.Marshal(map[string]any{"argv": []string{"slow"}, "timeout_s": 20})
		resp, err := http.Post(srv.URL+"/v1/vms/"+id+"/exec", "application/json", bytes.NewReader(b))
		if err != nil {
			out <- []map[string]any{{"error": err.Error()}}
			return
		}
		defer resp.Body.Close()
		var lines []map[string]any
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			var l map[string]any
			json.Unmarshal(sc.Bytes(), &l)
			lines = append(lines, l)
		}
		out <- lines
	}()
	return out
}

func execResult(lines []map[string]any) string {
	var stdout string
	exit := "none"
	for _, l := range lines {
		if d, ok := l["data_b64"].(string); ok {
			b, _ := base64.StdEncoding.DecodeString(d)
			stdout += string(b)
		}
		if e, ok := l["exit"]; ok {
			exit = fmt.Sprint(e)
		}
		if e, ok := l["error"]; ok {
			exit = fmt.Sprint("error: ", e)
		}
	}
	return fmt.Sprintf("%q exit=%s", stdout, exit)
}

// An exec running while its VM is forked (each fork's snapshot resets the
// guest's vsock connections) reattaches after each and returns its own output
// and exit status once, in its own time.
func TestExecSurvivesForksOfItsVM(t *testing.T) {
	m, h := newTestManager(t, nil)
	h.slowGo = make(chan struct{})
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Name: "src", Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	fc := h.fcOf(t, m, v.ID)
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	res := execAsync(t, srv, v.ID)
	eventually(t, "the exec session", func() bool { fc.mu.Lock(); defer fc.mu.Unlock(); return len(fc.sessions) == 1 })
	for i := range 3 {
		if _, err := m.Fork(ctx, v.ID, fmt.Sprintf("c%d", i), "t1", nil); err != nil {
			t.Fatal(err)
		}
	}
	close(h.slowGo)
	select {
	case lines := <-res:
		if got := execResult(lines); got != `"begin\nend\n" exit=7` {
			t.Fatalf("exec across 3 forks: %s (%v)", got, lines)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("exec across forks of its VM never returned")
	}
}

// An exec request the reset swallowed before the agent took it is sent again
// on the reattach (its session id makes it run at most once).
func TestExecLostInAResetIsResent(t *testing.T) {
	m, h := newTestManager(t, nil)
	h.slowGo = make(chan struct{})
	close(h.slowGo)
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Name: "src", Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	fc := h.fcOf(t, m, v.ID)
	h.mu.Lock()
	h.loseExec = 1
	h.mu.Unlock()
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	res := execAsync(t, srv, v.ID)
	eventually(t, "the lost exec", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.loseExec == 0 })
	if _, err := m.Fork(ctx, v.ID, "c", "t1", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case lines := <-res:
		if got := execResult(lines); got != `"begin\nend\n" exit=7` {
			t.Fatalf("resent exec: %s", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a lost exec was not resent")
	}
	if fc.called("agent exec") && len(fc.sessions) != 1 {
		t.Fatalf("sessions: %d", len(fc.sessions))
	}
}
