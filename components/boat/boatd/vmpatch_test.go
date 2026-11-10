package main

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPatchVM(t *testing.T) {
	clock := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	m, _ := newTestManager(t, nil)
	m.now = func() time.Time { return clock }
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	ctx := context.Background()
	a, err := m.Create(ctx, CreateReq{Name: "a", Team: "t", TTLS: 600})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, CreateReq{Name: "b", Team: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, CreateReq{Name: "c", Team: "other"}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/vms/" + a.ID
	var v VM
	var e map[string]any
	patch := func(body string, out any) int { // fresh v and e: omitempty fields must not linger
		v, e = VM{}, nil
		return call(t, srv, "PATCH", path, body, out)
	}

	// Validation, as create; nothing changes on a refused request.
	for _, body := range []string{`{}`, `{"name":"Bad!"}`, `{"ttl_s":-1}`, `{"extend_s":0}`, `{"name":"x","extend_s":-5}`, `{`} {
		if c := patch(body, &e); c != 400 {
			t.Fatalf("%s: %d %v", body, c, e)
		}
	}
	if c := call(t, srv, "PATCH", "/v1/vms/vm-nope", `{"name":"x"}`, &e); c != 404 {
		t.Fatalf("unknown vm: %d", c)
	}

	// Rename: unique within the team, free across teams, to itself is a no-op.
	if c := patch(`{"name":"b"}`, &e); c != 409 {
		t.Fatalf("name of another VM of the team: %d %v", c, e)
	}
	if c := patch(`{"name":"c"}`, &v); c != 200 || v.Name != "c" || v.ID != a.ID {
		t.Fatalf("rename to another team's name: %d %+v", c, v)
	}
	if c := patch(`{"name":"c"}`, &v); c != 200 || v.Name != "c" {
		t.Fatalf("rename to itself: %d %+v", c, v)
	}
	if g, _ := m.get(a.ID); g.Name != "c" {
		t.Fatalf("rename not kept: %+v", g)
	}

	// ttl_s while running: TTLS and a fresh deadline from now.
	clock = clock.Add(100 * time.Second)
	if c := patch(`{"ttl_s":3600}`, &v); c != 200 || v.TTLS != 3600 || v.ExpiresAt == nil || !v.ExpiresAt.Equal(clock.Add(time.Hour)) {
		t.Fatalf("ttl_s: %d %+v", c, v)
	}
	// extend_s pushes the deadline out from itself...
	if c := patch(`{"extend_s":60}`, &v); c != 200 || !v.ExpiresAt.Equal(clock.Add(time.Hour+time.Minute)) || v.TTLS != 3600 {
		t.Fatalf("extend_s: %d %+v", c, v)
	}
	// ...or from now once the deadline is past.
	clock = clock.Add(2 * time.Hour)
	if c := patch(`{"extend_s":60}`, &v); c != 200 || !v.ExpiresAt.Equal(clock.Add(time.Minute)) {
		t.Fatalf("extend_s past the deadline: %d %+v", c, v)
	}
	// ttl_s 0 clears the auto-stop; extend_s then has nothing to extend.
	if c := patch(`{"ttl_s":0}`, &v); c != 200 || v.TTLS != 0 || v.ExpiresAt != nil {
		t.Fatalf("ttl_s 0: %d %+v", c, v)
	}
	if c := patch(`{"extend_s":60}`, &e); c != 400 {
		t.Fatalf("extend_s without a TTL: %d %v", c, e)
	}
	// Both in one request: ttl_s first, then extend_s on top.
	if c := patch(`{"ttl_s":100,"extend_s":50}`, &v); c != 200 || !v.ExpiresAt.Equal(clock.Add(150*time.Second)) {
		t.Fatalf("ttl_s + extend_s: %d %+v", c, v)
	}

	// Stopped: ttl_s is kept for the next start, extend_s is a conflict.
	if _, err := m.Stop(ctx, a.ID, "api"); err != nil {
		t.Fatal(err)
	}
	if c := patch(`{"extend_s":60}`, &e); c != 409 {
		t.Fatalf("extend_s on a stopped VM: %d %v", c, e)
	}
	if c := patch(`{"ttl_s":900}`, &v); c != 200 || v.TTLS != 900 || v.ExpiresAt != nil {
		t.Fatalf("ttl_s on a stopped VM: %d %+v", c, v)
	}
	if c := patch(`{"name":"d","extend_s":60}`, &e); c != 409 {
		t.Fatalf("rename + extend on a stopped VM: %d %v", c, e)
	}
	if g, _ := m.get(a.ID); g.Name != "c" {
		t.Fatalf("a refused request renamed the VM: %+v", g)
	}
	s, err := m.Start(ctx, a.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.ExpiresAt == nil || !s.ExpiresAt.Equal(clock.Add(900*time.Second)) {
		t.Fatalf("start did not use the patched ttl_s: %+v", s)
	}

	// Persisted: a restarted boatd sees the new name and TTL.
	m2, err := newManager(m.cfg, m.run, m.procs)
	if err != nil {
		t.Fatal(err)
	}
	if g, err := m2.get(a.ID); err != nil || g.Name != "c" || g.TTLS != 900 {
		t.Fatalf("patch not persisted: %+v %v", g, err)
	}
}
