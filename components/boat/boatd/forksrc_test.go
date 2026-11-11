package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A running VM forked more often than Firecracker's API server takes open
// connections must come back resumed each time: boatd once left one keep-alive
// connection per API client open, so the 5th fork's resume got 503 and the
// source stayed paused, its execs hanging until their deadline.
func TestForkSourceResumedEveryTime(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Name: "src", Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	fc := h.fcOf(t, m, v.ID)
	for i := range 2 * fcMaxConns {
		c, err := m.Fork(ctx, v.ID, fmt.Sprintf("c%d", i), "t1")
		if err != nil {
			t.Fatalf("fork %d: %v", i, err)
		}
		fc.mu.Lock()
		paused := fc.paused
		fc.mu.Unlock()
		if paused {
			t.Fatalf("source left paused after fork %d", i)
		}
		if _, err := m.Delete(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "API connections closed", func() bool { return fc.openConns() == 0 })
}

// A resume the VMM refuses for a moment is retried; one it keeps refusing
// fails the fork instead of reporting a child while the source sits paused.
func TestForkRetriesResume(t *testing.T) {
	old := resumeRetries
	resumeRetries = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { resumeRetries = old })
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Name: "src", Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	fc := h.fcOf(t, m, v.ID)
	isPaused := func() bool { fc.mu.Lock(); defer fc.mu.Unlock(); return fc.paused }

	h.mu.Lock()
	h.failResume = 2
	h.mu.Unlock()
	if _, err := m.Fork(ctx, v.ID, "c1", "t1"); err != nil || isPaused() {
		t.Fatalf("fork after 2 refused resumes: err=%v paused=%v", err, isPaused())
	}

	h.mu.Lock()
	h.failResume = 100
	h.mu.Unlock()
	_, err = m.Fork(ctx, v.ID, "c2", "t1")
	if err == nil || !strings.Contains(err.Error(), "left paused") {
		t.Fatalf("fork with the source stuck paused: %v", err)
	}
	h.mu.Lock()
	h.failResume = 0
	h.mu.Unlock()
	if _, err := m.Fork(ctx, v.ID, "c3", "t1"); err != nil || isPaused() {
		t.Fatalf("fork once resumes work again: err=%v paused=%v", err, isPaused())
	}
}

// A guest that never accepts the vsock CONNECT (a paused VM) fails the dial in
// agentHandshakeTimeout, not at the caller's deadline (an exec's timeout_s+30).
func TestAgentHandshakeBounded(t *testing.T) {
	old := agentHandshakeTimeout
	agentHandshakeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { agentHandshakeTimeout = old })
	dir, err := os.MkdirTemp("/tmp", "hs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, vsockUDS)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // read nothing, answer nothing
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t0 := time.Now()
	if _, err := dialAgent(ctx, sock); err == nil || !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("dial a mute guest: %v", err)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("handshake waited %s", d)
	}
}

// After the handshake the caller's deadline applies again, not the handshake's.
func TestAgentDeadlineAfterHandshake(t *testing.T) {
	old := agentHandshakeTimeout
	agentHandshakeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { agentHandshakeTimeout = old })
	m, _ := newTestManager(t, nil)
	v, err := m.Create(context.Background(), CreateReq{Name: "a", Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	sock, _ := m.sock(v.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := dialAgent(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	time.Sleep(300 * time.Millisecond)
	if err := a.send(map[string]string{"op": "health"}); err != nil {
		t.Fatal(err)
	}
	if _, r, err := a.recv(); err != nil || !r.OK {
		t.Fatalf("health after the handshake window: %+v %v", r, err)
	}
}
