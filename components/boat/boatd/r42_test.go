package main

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func r42Idle(sock string) int {
	agentPool.mu.Lock()
	defer agentPool.mu.Unlock()
	return len(agentPool.idle[sock])
}

// r42Stream runs one streamed exec (op names the fake guest's behavior) through
// sendExec + relayExec like execHandler, and returns the emitted lines.
func r42Stream(t *testing.T, g *r34Guest, op string) ([]string, bool) {
	t.Helper()
	ctx := context.Background()
	m := &Manager{}
	req := map[string]any{"op": op, "argv": []string{"x"}, "session": newID("x-")}
	a, err := sendExec(ctx, g.sock, req)
	if err != nil {
		t.Fatalf("sendExec: %v", err)
	}
	var mu sync.Mutex
	var lines []string
	sawFinal := false
	m.relayExec(ctx, "vm", g.sock, a, m.resets.watch("vm"), req, func(raw []byte, final bool) error {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, string(raw))
		sawFinal = sawFinal || final
		return nil
	})
	return lines, sawFinal
}

// The stream's initial conn comes from the pool (no new dial) and, after a clean
// final line, goes back: a run of streamed and plain ops shares one conn.
func TestR42StreamUsesPooledConnAndReturnsItClean(t *testing.T) {
	g := newR34Guest(t)
	if _, err := r34Call(t, g, "health"); err != nil {
		t.Fatal(err)
	}
	before := agentDials.Load()
	for i := 0; i < 3; i++ {
		lines, final := r42Stream(t, g, "exec")
		if !final || len(lines) != 2 || !strings.Contains(lines[1], `"exit":0`) {
			t.Fatalf("stream %d: %v final=%v", i, lines, final)
		}
		if n := r42Idle(g.sock); n != 1 {
			t.Fatalf("stream %d: idle=%d, want the conn back in the pool", i, n)
		}
	}
	if _, err := r34Call(t, g, "health"); err != nil {
		t.Fatal(err)
	}
	if d := agentDials.Load() - before; d != 0 || g.accepts.Load() != 1 {
		t.Fatalf("dials=%d accepts=%d, want the one pooled conn for everything", d, g.accepts.Load())
	}
}

// With an empty pool a stream dials once (and its conn then joins the pool).
func TestR42StreamDialsWhenPoolEmpty(t *testing.T) {
	g := newR34Guest(t)
	if _, final := r42Stream(t, g, "exec"); !final {
		t.Fatal("no final line")
	}
	if g.accepts.Load() != 1 || r42Idle(g.sock) != 1 {
		t.Fatalf("accepts=%d idle=%d", g.accepts.Load(), r42Idle(g.sock))
	}
}

// A stream that breaks (cut after one line, no final line) is never put back: the pool is empty and the next op dials fresh.
func TestR42BrokenStreamNotReturnedToPool(t *testing.T) {
	g := newR34Guest(t)
	if _, err := r34Call(t, g, "health"); err != nil {
		t.Fatal(err)
	}
	lines, _ := r42Stream(t, g, "partial")
	last := lines[len(lines)-1]
	if len(lines) != 2 || !strings.Contains(last, "guest agent:") {
		t.Fatalf("want the partial line then an error line: %v", lines)
	}
	if n := r42Idle(g.sock); n != 0 {
		t.Fatalf("idle=%d: a cut stream's conn must not be pooled", n)
	}
	// Nothing was pooled, so this dials; and the conn it leaves is then reused.
	if _, err := r34Call(t, g, "health"); err != nil {
		t.Fatal(err)
	}
	before := g.accepts.Load()
	if _, err := r34Call(t, g, "health"); err != nil {
		t.Fatal(err)
	}
	if g.accepts.Load() != before {
		t.Fatalf("a healthy conn must still be reused after a broken one was discarded")
	}
}

// A stream whose ctx ended (deadline spent) is closed, not pooled.
func TestR42CanceledStreamNotPooled(t *testing.T) {
	g := newR34Guest(t)
	ctx, cancel := context.WithCancel(context.Background())
	req := map[string]any{"op": "exec", "argv": []string{"x"}, "session": "x-1"}
	a, err := sendExec(ctx, g.sock, req)
	if err != nil {
		t.Fatal(err)
	}
	cancel() // the AfterFunc spent the conn's deadline
	a.finishStream(g.sock, reply{Exit: new(int)})
	if n := r42Idle(g.sock); n != 0 {
		t.Fatalf("idle=%d: a conn whose ctx ended must be closed", n)
	}
}
