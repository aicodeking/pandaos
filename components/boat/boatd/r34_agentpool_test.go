package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// r34Guest is a fake guest agent on a unix socket: CONNECT handshake, then one
// JSON request/reply per line on the same stream, as boat-agent.py handle_conn does.
type r34Guest struct {
	t        *testing.T
	sock     string
	ln       net.Listener
	accepts  atomic.Int64
	ops      atomic.Int64 // requests fully read
	execs    atomic.Int64
	dropNext atomic.Int64 // drop (close without reply) this many upcoming requests
	closed   chan struct{}
	mu       sync.Mutex
	conns    []net.Conn
	n        atomic.Int64 // closed-conn notifications
}

func newR34Guest(t *testing.T) *r34Guest {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "r34")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	g := &r34Guest{t: t, sock: filepath.Join(dir, "v.sock"), closed: make(chan struct{}, 64)}
	g.listen()
	t.Cleanup(func() { g.ln.Close(); g.closeAll() })
	return g
}

func (g *r34Guest) listen() {
	ln, err := net.Listen("unix", g.sock)
	if err != nil {
		g.t.Fatal(err)
	}
	g.ln = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			g.accepts.Add(1)
			g.mu.Lock()
			g.conns = append(g.conns, c)
			g.mu.Unlock()
			go g.serve(c)
		}
	}()
}

func (g *r34Guest) closeAll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.conns {
		c.Close()
	}
	g.conns = nil
}

func (g *r34Guest) serve(c net.Conn) {
	defer func() { c.Close(); g.closed <- struct{}{} }()
	r := bufio.NewReader(c)
	if _, err := r.ReadString('\n'); err != nil {
		return
	}
	c.Write([]byte("OK 1\n"))
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		g.ops.Add(1)
		var req struct {
			Op string `json:"op"`
		}
		json.Unmarshal(line, &req)
		if req.Op == "exec" {
			g.execs.Add(1)
		}
		if g.dropNext.Load() > 0 {
			g.dropNext.Add(-1)
			return
		}
		switch req.Op {
		case "exec":
			c.Write([]byte(`{"stream":"stdout","data_b64":"aGk="}` + "\n" + `{"exit":0}` + "\n"))
		case "partial": // a stream cut after one line: no final line, then the conn closes
			c.Write([]byte(`{"stream":"stdout","data_b64":"aGk="}` + "\n"))
			return
		case "bad":
			c.Write([]byte(`{"error":"boom"}` + "\n"))
		case "stray":
			c.Write([]byte(`{"ok":true}` + "\n" + `{"stream":"stdout"}` + "\n"))
		default:
			c.Write([]byte(`{"ok":true}` + "\n"))
		}
	}
}

func (g *r34Guest) waitClosed() bool {
	select {
	case <-g.closed:
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

func r34Call(t *testing.T, g *r34Guest, op string) (reply, error) {
	t.Helper()
	return agentCall(context.Background(), g.sock, map[string]string{"op": op})
}

func TestR34AgentPoolSequentialOpsOneDial(t *testing.T) {
	g := newR34Guest(t)
	before := agentDials.Load()
	for i := 0; i < 20; i++ {
		op := "health"
		if i%3 == 0 {
			op = "exec" // streamed reply then exit; conn must stay in sync
		}
		if r, err := r34Call(t, g, op); err != nil || !(r.OK || r.Exit != nil) {
			t.Fatalf("op %d (%s): %+v %v", i, op, r, err)
		}
	}
	if d := agentDials.Load() - before; d != 1 || g.accepts.Load() != 1 {
		t.Fatalf("dials=%d accepts=%d, want 1 (was 20 before pooling)", d, g.accepts.Load())
	}
}

func TestR34AgentPoolGuestErrorKeepsConn(t *testing.T) {
	g := newR34Guest(t)
	if _, err := r34Call(t, g, "bad"); err == nil {
		t.Fatal("want guest error")
	}
	if _, err := r34Call(t, g, "health"); err != nil {
		t.Fatal(err)
	}
	if g.accepts.Load() != 1 {
		t.Fatalf("accepts=%d, want 1", g.accepts.Load())
	}
}

func TestR34AgentPoolServerClosedIdleRedials(t *testing.T) {
	g := newR34Guest(t)
	if _, err := r34Call(t, g, "health"); err != nil {
		t.Fatal(err)
	}
	g.closeAll()
	g.waitClosed()
	// Even a non-idempotent op needs no retry: the peek sees the EOF before writing.
	if _, err := r34Call(t, g, "exec"); err != nil {
		t.Fatal(err)
	}
	if g.accepts.Load() != 2 || g.execs.Load() != 1 {
		t.Fatalf("accepts=%d execs=%d", g.accepts.Load(), g.execs.Load())
	}
}

func TestR34AgentPoolBrokenConnReadRetried(t *testing.T) {
	g := newR34Guest(t)
	if _, err := r34Call(t, g, "health"); err != nil {
		t.Fatal(err)
	}
	g.dropNext.Store(1) // looks alive when taken, dies on the next request
	if r, err := r34Call(t, g, "cmd_status"); err != nil || !r.OK {
		t.Fatalf("read op not retried transparently: %+v %v", r, err)
	}
	if g.accepts.Load() != 2 {
		t.Fatalf("accepts=%d, want 2", g.accepts.Load())
	}
}

func TestR34AgentPoolExecNotReplayedAfterWrite(t *testing.T) {
	g := newR34Guest(t)
	if _, err := r34Call(t, g, "health"); err != nil {
		t.Fatal(err)
	}
	g.dropNext.Store(1)
	if _, err := r34Call(t, g, "exec"); err == nil {
		t.Fatal("exec on a conn that died after the write must fail, not be replayed")
	}
	if g.execs.Load() != 1 || g.accepts.Load() != 1 {
		t.Fatalf("execs=%d accepts=%d, want 1/1 (no replay, no redial)", g.execs.Load(), g.accepts.Load())
	}
	// The broken conn was discarded: the next op works on a fresh dial.
	if _, err := r34Call(t, g, "exec"); err != nil || g.execs.Load() != 2 || g.accepts.Load() != 2 {
		t.Fatalf("recovery: err=%v execs=%d accepts=%d", err, g.execs.Load(), g.accepts.Load())
	}
}

func TestR34AgentPoolClosedOnVMStop(t *testing.T) {
	g := newR34Guest(t)
	if _, err := r34Call(t, g, "health"); err != nil {
		t.Fatal(err)
	}
	closeAgentConns(g.sock)
	if !g.waitClosed() {
		t.Fatal("pooled conn not closed by closeAgentConns")
	}
	if _, err := r34Call(t, g, "health"); err != nil || g.accepts.Load() != 2 {
		t.Fatalf("err=%v accepts=%d", err, g.accepts.Load())
	}
}

// A restarted VM recreates its vsock socket: an old conn that still looks alive
// must not be reused.
func TestR34AgentPoolStaleGenerationDropped(t *testing.T) {
	g := newR34Guest(t)
	if _, err := r34Call(t, g, "health"); err != nil {
		t.Fatal(err)
	}
	old := g.ln
	os.Remove(g.sock)
	time.Sleep(10 * time.Millisecond) // distinct mtime for the new socket file
	g.listen()
	defer old.Close()
	if _, err := r34Call(t, g, "exec"); err != nil {
		t.Fatal(err)
	}
	if !g.waitClosed() {
		t.Fatal("stale-generation conn was not closed")
	}
	if g.accepts.Load() != 2 || g.execs.Load() != 1 {
		t.Fatalf("accepts=%d execs=%d", g.accepts.Load(), g.execs.Load())
	}
}

func TestR34AgentPoolIdleTimeout(t *testing.T) {
	old := agentPoolIdleTimeout
	agentPoolIdleTimeout = 50 * time.Millisecond
	defer func() { agentPoolIdleTimeout = old }()
	g := newR34Guest(t)
	if _, err := r34Call(t, g, "health"); err != nil {
		t.Fatal(err)
	}
	if !g.waitClosed() {
		t.Fatal("idle conn not closed after the idle timeout")
	}
}

func TestR34AgentPoolStrayDataDropsConn(t *testing.T) {
	g := newR34Guest(t)
	if _, err := r34Call(t, g, "stray"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // let the extra line land
	if r, err := r34Call(t, g, "health"); err != nil || !r.OK {
		t.Fatalf("%+v %v", r, err)
	}
	if g.accepts.Load() != 2 {
		t.Fatalf("a conn with unsolicited data was reused (accepts=%d)", g.accepts.Load())
	}
}

func TestR34AgentPoolNoPoolOps(t *testing.T) {
	g := newR34Guest(t)
	for i := 0; i < 3; i++ {
		if _, err := r34Call(t, g, "shutdown"); err != nil {
			t.Fatal(err)
		}
	}
	if g.accepts.Load() != 3 {
		t.Fatalf("shutdown must use dedicated conns, accepts=%d", g.accepts.Load())
	}
}

func TestR34AgentPoolConcurrentRace(t *testing.T) {
	g := newR34Guest(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if _, err := r34Call(t, g, "health"); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	agentPool.mu.Lock()
	n := len(agentPool.idle[g.sock])
	agentPool.mu.Unlock()
	if n > agentPoolMaxIdle {
		t.Fatalf("idle=%d > cap %d", n, agentPoolMaxIdle)
	}
	if g.accepts.Load() >= 400 {
		t.Fatalf("no reuse: accepts=%d", g.accepts.Load())
	}
}

func TestR34AgentPoolCtxCancelNotPooled(t *testing.T) {
	g := newR34Guest(t)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := agentCall(ctx, g.sock, map[string]string{"op": "health"}); err != nil {
		t.Fatal(err)
	}
	cancel() // after the op: the pooled conn must not be poisoned by the late cancel
	time.Sleep(20 * time.Millisecond)
	if _, err := r34Call(t, g, "health"); err != nil {
		t.Fatal(err)
	}
	if g.accepts.Load() != 1 {
		t.Fatalf("accepts=%d, want 1", g.accepts.Load())
	}
}

// A snapshot resets the guest's vsock conns; sync precedes every snapshot, so it
// must drop the idle conns instead of letting a later op wait on a dead one.
func TestR34AgentPoolSyncIsBarrier(t *testing.T) {
	g := newR34Guest(t)
	if _, err := r34Call(t, g, "health"); err != nil {
		t.Fatal(err)
	}
	if _, err := r34Call(t, g, "sync"); err != nil {
		t.Fatal(err)
	}
	if !g.waitClosed() {
		t.Fatal("idle conn survived sync")
	}
	if _, err := r34Call(t, g, "health"); err != nil || g.accepts.Load() != 3 {
		t.Fatalf("err=%v accepts=%d, want 3 (health, sync, health)", err, g.accepts.Load())
	}
}

func r34Idle(sock string) int {
	agentPool.mu.Lock()
	defer agentPool.mu.Unlock()
	return len(agentPool.idle[sock])
}

// A VM's vsock path goes stale when it stops or is deleted: the manager empties
// the pool then, rather than leaving conns to the dead VMM idle until they expire.
func TestR34AgentPoolEmptiedOnStopAndDelete(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	for _, del := range []bool{false, true} {
		v, err := m.Create(ctx, CreateReq{Name: "pooled"})
		if err != nil {
			t.Fatal(err)
		}
		sock := filepath.Join(m.root(v.ID), vsockUDS)
		if _, err := agentCall(ctx, sock, map[string]string{"op": "health"}); err != nil {
			t.Fatal(err)
		}
		if r34Idle(sock) == 0 {
			t.Fatal("health conn was not pooled")
		}
		if del {
			_, err = m.Delete(ctx, v.ID)
		} else {
			_, err = m.Stop(ctx, v.ID, "")
		}
		if err != nil {
			t.Fatal(err)
		}
		if n := r34Idle(sock); n != 0 {
			t.Fatalf("delete=%v: %d idle conns left after the VM went away", del, n)
		}
	}
}
