package main

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func teamVMs(m *Manager, team string) int { return len(m.list(team)) }

// count: N VMs in one call, all or none.
func TestCreateBatch(t *testing.T) {
	m, h := newTestManager(t, func(c *Config) {
		c.Teams = map[string]TeamPolicy{"small": {MaxVMs: 2}}
	})
	srv := httptest.NewServer(m.routes())
	defer srv.Close()

	var out struct{ VMs []VM }
	if c := call(t, srv, "POST", "/v1/vms", map[string]any{"name": "w", "team": "t1", "count": 3}, &out); c != 201 || len(out.VMs) != 3 {
		t.Fatalf("batch: %d %+v", c, out)
	}
	for i, v := range out.VMs {
		if v.State != Running || !v.AgentReady || v.Name != []string{"w-1", "w-2", "w-3"}[i] {
			t.Fatalf("batch vm %d: %+v", i, v)
		}
	}
	// Over the team's quota: refused before anything boots, nothing left behind.
	var e map[string]string
	before := h.starts
	if c := call(t, srv, "POST", "/v1/vms", map[string]any{"team": "small", "count": 3}, &e); c != 429 {
		t.Fatalf("over quota: %d %v", c, e)
	}
	if teamVMs(m, "small") != 0 || h.starts != before {
		t.Fatalf("over quota left %d VMs, %d starts", teamVMs(m, "small"), h.starts-before)
	}
	// One of the boots fails: the others are deleted too.
	h.mu.Lock()
	h.failStart = h.starts + 2
	h.mu.Unlock()
	if c := call(t, srv, "POST", "/v1/vms", map[string]any{"team": "t2", "count": 3}, &e); c < 500 {
		t.Fatalf("failed boot: %d %v", c, e)
	}
	if n := teamVMs(m, "t2"); n != 0 {
		t.Fatalf("a failed batch left %d VMs", n)
	}
	for _, bad := range []int{-1, maxBatch + 1} {
		if c := call(t, srv, "POST", "/v1/vms", map[string]any{"team": "t3", "count": bad}, &e); c != 400 {
			t.Fatalf("count %d: %d", bad, c)
		}
	}
	// count 1 (or none) keeps the single-object reply.
	var v VM
	if c := call(t, srv, "POST", "/v1/vms", map[string]any{"team": "t3", "count": 1}, &v); c != 201 || v.ID == "" {
		t.Fatalf("count 1: %d %+v", c, v)
	}
}

// fail_fast: an admission refusal says no_ready_machine (host_capacity without it),
// and a create does not wait for images still converting.
func TestFailFast(t *testing.T) {
	m, _ := newTestManager(t, nil)
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	var a VM
	if c := call(t, srv, "POST", "/v1/vms", map[string]any{"team": "t1"}, &a); c != 201 {
		t.Fatalf("create: %d", c)
	}
	call(t, srv, "POST", "/v1/vms/"+a.ID+"/stop", nil, nil)
	m.hostMem = 1 // the host is full from now on
	var e map[string]string
	for _, tc := range []struct{ path, code string }{
		{"/v1/vms", codeHostCapacity},
		{"/v1/vms/" + a.ID + "/start", codeHostCapacity},
		{"/v1/vms/" + a.ID + "/fork", codeHostCapacity},
	} {
		body := map[string]any{"team": "t1"}
		if c := call(t, srv, "POST", tc.path, body, &e); c != 503 || e["code"] != tc.code {
			t.Fatalf("%s: %d %v", tc.path, c, e)
		}
		body["fail_fast"] = true
		if c := call(t, srv, "POST", tc.path, body, &e); c != 503 || e["code"] != codeNoReadyMachine {
			t.Fatalf("%s fail_fast: %d %v", tc.path, c, e)
		}
	}
	if c := call(t, srv, "POST", "/v1/vms", map[string]any{"team": "t1", "count": 2, "fail_fast": true}, &e); c != 503 || e["code"] != codeNoReadyMachine {
		t.Fatalf("batch fail_fast: %d %v", c, e)
	}
	m.hostMem = 1 << 20

	m.imagesReady = make(chan struct{}) // images converting
	if c := call(t, srv, "POST", "/v1/vms", map[string]any{"team": "t1", "fail_fast": true}, &e); c != 503 || e["code"] != codeNoReadyMachine {
		t.Fatalf("fail_fast during conversion: %d %v", c, e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := m.Create(ctx, CreateReq{Team: "t1"}); err == nil {
		t.Fatal("a create without fail_fast did not wait for the images")
	}
	close(m.imagesReady)
	if _, err := m.Create(context.Background(), CreateReq{Team: "t1"}); err != nil {
		t.Fatal(err)
	}
}

// force: killed now, no guest sync or snapshot; the next start cold-boots.
func TestForceStop(t *testing.T) {
	m, h := newTestManager(t, nil)
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	var v VM
	call(t, srv, "POST", "/v1/vms", map[string]any{"team": "t1"}, &v)
	call(t, srv, "POST", "/v1/vms/"+v.ID+"/stop", nil, nil) // a real stop first: a snapshot exists
	call(t, srv, "POST", "/v1/vms/"+v.ID+"/start", nil, &v)
	fc := h.fcOf(t, m, v.ID)
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/stop", map[string]any{"force": true}, &v); c != 200 || v.State != Stopped || v.HasSnapshot {
		t.Fatalf("force stop: %d %+v", c, v)
	}
	if fc.called("agent sync") || fc.called("PUT /snapshot/create") {
		t.Fatal("force stop synced or snapshotted the guest")
	}
	for _, f := range []string{memFile, vmstate} {
		if exists(m.root(v.ID) + "/" + f) {
			t.Fatalf("%s left after a force stop", f)
		}
	}
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/start", nil, &v); c != 200 || v.State != Running {
		t.Fatalf("start after force: %d %+v", c, v)
	}
	if h.fcOf(t, m, v.ID).called("PUT /snapshot/load") {
		t.Fatal("start after a force stop restored a snapshot")
	}
}
