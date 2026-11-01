package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func putFile(t *testing.T, m *Manager, id, path, data string) {
	t.Helper()
	sock, err := m.sock(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentCall(context.Background(), sock, map[string]any{"op": "put_file", "path": path,
		"offset": 0, "last": true, "data_b64": b64(data)}); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycle(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	_, events, cancel := m.ev.subscribe(0)
	defer cancel()

	v, err := m.Create(ctx, CreateReq{Name: "a", Team: "t1", Env: map[string]string{"K": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	if v.State != Running || !v.AgentReady || v.IP != "10.200.0.2" || v.Tap != "bt2" || v.MAC != "06:00:0a:c8:00:02" {
		t.Fatalf("created: %+v", v)
	}
	fc := h.fcOf(t, m, v.ID)
	if st, _ := os.Stat(filepath.Join(m.root(v.ID), diskFile)); st.Size() != 1<<30 {
		t.Fatalf("disk not grown to quota: %d", st.Size())
	}
	if len(fc.idents) != 1 || fc.idents[0]["ipv4_cidr"] != "10.200.0.2/16" || fc.idents[0]["hostname"] != "a" || fc.env["K"] != "v" {
		t.Fatalf("guest init: %v %v", fc.idents, fc.env)
	}
	if !fc.called("PUT /actions") || fc.tap != "bt2" || !h.ran("ip tuntap add dev bt2 mode tap") {
		t.Fatal("not cold booted on its tap")
	}
	putFile(t, m, v.ID, "/home/dev/x", "hello")

	// stop: pause + snapshot + kill; files land atomically.
	v, err = m.Stop(ctx, v.ID, "api")
	if err != nil || v.State != Stopped || !v.HasSnapshot || v.PID != 0 {
		t.Fatalf("stop: %+v %v", v, err)
	}
	for _, f := range []string{vmstate, memFile} {
		if !exists(filepath.Join(m.root(v.ID), f)) || exists(filepath.Join(m.root(v.ID), "new."+f)) {
			t.Fatalf("snapshot file %s not in place", f)
		}
	}
	if !fc.called("PATCH /vm") || !fc.called("PUT /snapshot/create") {
		t.Fatal("stop did not snapshot")
	}

	// start: restore, delete the now-stale snapshot, fix the guest clock.
	v, err = m.Start(ctx, v.ID, nil)
	if err != nil || v.State != Running {
		t.Fatalf("start: %+v %v", v, err)
	}
	fc = h.fcOf(t, m, v.ID)
	if !fc.called("PUT /snapshot/load") || fc.called("PUT /actions") || !fc.clockSet {
		t.Fatal("start did not restore from snapshot")
	}
	if exists(filepath.Join(m.root(v.ID), vmstate)) {
		t.Fatal("stale snapshot kept after restore")
	}

	// fork a running VM: parent resumed, child restored with new identity + disk copy.
	c, err := m.Fork(ctx, v.ID, "b", "t1")
	if err != nil || c.State != Running || c.IP != "10.200.0.3" || c.ParentID != v.ID {
		t.Fatalf("fork: %+v %v", c, err)
	}
	if fc.paused {
		t.Fatal("parent left paused")
	}
	cfc := h.fcOf(t, m, c.ID)
	if !cfc.called("PUT /snapshot/load") || cfc.tap != "bt3" {
		t.Fatalf("child not restored on its own tap: %v %s", cfc.calls, cfc.tap)
	}
	if len(cfc.idents) != 1 || cfc.idents[0]["ipv4_cidr"] != "10.200.0.3/16" || cfc.idents[0]["mac"] != "06:00:0a:c8:00:03" {
		t.Fatalf("child identity: %v", cfc.idents)
	}
	if cfc.disk()["/home/dev/x"] != b64("hello") {
		t.Fatal("child disk is not a copy of the parent's")
	}
	if _, err := m.Fork(ctx, v.ID, "c", "other-team"); !errors.Is(err, errNotFound) {
		t.Fatalf("cross-team fork: %v", err)
	}

	// named snapshot of the child, then a new VM from it.
	s, err := m.CreateSnapshot(ctx, c.ID, "snap1")
	if err != nil || !s.HasMem {
		t.Fatalf("snapshot: %+v %v", s, err)
	}
	d, err := m.Create(ctx, CreateReq{Team: "t1", Snapshot: s.ID})
	if err != nil || d.State != Running || d.FromSnapshot != s.ID {
		t.Fatalf("from snapshot: %+v %v", d, err)
	}
	if h.fcOf(t, m, d.ID).disk()["/home/dev/x"] != b64("hello") {
		t.Fatal("snapshot lost the disk")
	}

	// fork a stopped VM (from its stop snapshot).
	if _, err := m.Stop(ctx, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	e, err := m.Fork(ctx, d.ID, "", "")
	if err != nil || e.State != Running || !h.fcOf(t, m, e.ID).called("PUT /snapshot/load") {
		t.Fatalf("fork stopped: %+v %v", e, err)
	}

	rep, err := m.Usage(time.Time{})
	if err != nil || len(rep.VMs) < 4 || len(rep.Snapshots) != 1 {
		t.Fatalf("usage: %+v %v", rep, err)
	}
	for _, x := range m.list("") {
		if _, err := m.Delete(ctx, x.ID); err != nil {
			t.Fatal(err)
		}
		if exists(m.vmDir(x.ID)) {
			t.Fatal("vm dir left behind")
		}
	}
	if exists(m.snapDir(s.ID)) {
		t.Fatal("snapshot of deleted vm left behind")
	}
	seen := map[string]bool{}
	for len(events) > 0 {
		ev := <-events
		seen[ev.Type+":"+string(ev.State)] = true
	}
	for _, k := range []string{"vm.state:creating", "vm.state:running", "vm.state:stopping", "vm.state:stopped",
		"vm.state:starting", "vm.state:forking", "snapshot.created:", "snapshot.deleted:", "vm.deleted:"} {
		if !seen[k] {
			t.Errorf("no %s event (saw %v)", k, seen)
		}
	}
}

func TestQuota(t *testing.T) {
	m, _ := newTestManager(t, func(c *Config) {
		c.Teams = map[string]TeamPolicy{"t": {MaxVMs: 1}}
		c.DefaultTeam = TeamPolicy{MaxVCPU: 2}
	})
	ctx := context.Background()
	a, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	var ae *apiErr
	if _, err := m.Create(ctx, CreateReq{Team: "t"}); !errors.As(err, &ae) || ae.code != 429 {
		t.Fatalf("want 429, got %v", err)
	}
	if _, err := m.Fork(ctx, a.ID, "", ""); !errors.As(err, &ae) || ae.code != 429 {
		t.Fatalf("fork past quota: %v", err)
	}
	m.Stop(ctx, a.ID, "")
	b, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ctx, a.ID, nil); !errors.As(err, &ae) || ae.code != 429 {
		t.Fatalf("start past quota: %v", err)
	}
	m.Delete(ctx, b.ID)
	if _, err := m.Start(ctx, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, CreateReq{Team: "x", Size: "large"}); !errors.As(err, &ae) || ae.code != 429 {
		t.Fatalf("vcpu quota: %v", err)
	}
}

func TestReconcile(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	a, _ := m.Create(ctx, CreateReq{Name: "a"})
	b, _ := m.Create(ctx, CreateReq{Name: "b"})
	// b looks like boatd died mid-stop with a half-written snapshot.
	m.update(b.ID, "", func(v *VM) { v.State = Stopping })
	os.WriteFile(filepath.Join(m.root(b.ID), "new."+memFile), []byte("x"), 0o600)
	h.Kill(b.PID)

	m2, err := newManager(m.cfg, h.run, h)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := m2.get(a.ID); v.State != Running || v.PID != a.PID {
		t.Fatalf("running vm not adopted: %+v", v)
	}
	if v, _ := m2.get(b.ID); v.State != Stopped || v.HasSnapshot || exists(filepath.Join(m.root(b.ID), "new."+memFile)) {
		t.Fatalf("dead vm not reconciled: %+v", v)
	}
	// the adopted process exits on its own (guest poweroff) -> stopped, cold next time.
	h.Kill(a.PID)
	eventually(t, "adopted exit noticed", func() bool { v, _ := m2.get(a.ID); return v.State == Stopped })
	if v, _ := m2.get(b.ID); v.State != Stopped {
		t.Fatal(v.State)
	}
	if _, err := m2.Start(ctx, b.ID, nil); err != nil || !h.fcOf(t, m2, b.ID).called("PUT /actions") {
		t.Fatalf("cold start after crash: %v", err)
	}
}

func TestReap(t *testing.T) {
	m, h := newTestManager(t, func(c *Config) { c.IdleStopS = 60 })
	ctx := context.Background()
	ttl, _ := m.Create(ctx, CreateReq{Name: "ttl", TTLS: 30})
	busy, _ := m.Create(ctx, CreateReq{Name: "busy"})
	never, _ := m.Create(ctx, CreateReq{Name: "never", IdleStopS: -1})
	real := m.now
	m.now = func() time.Time { return real().Add(45 * time.Second) }
	m.reap(ctx) // agent reports idle_s=0: only the TTL fires
	if v, _ := m.get(ttl.ID); v.State != Stopped {
		t.Fatalf("ttl: %s", v.State)
	}
	if v, _ := m.get(busy.ID); v.State != Running {
		t.Fatal("busy vm stopped")
	}
	h.mu.Lock()
	h.idleS = 3600
	h.mu.Unlock()
	m.now = func() time.Time { return real().Add(2 * time.Hour) }
	m.reap(ctx)
	if v, _ := m.get(busy.ID); v.State != Stopped {
		t.Fatalf("idle: %s", v.State)
	}
	if v, _ := m.get(never.ID); v.State != Running {
		t.Fatal("idle_stop_s=-1 vm stopped")
	}
}

func TestValidate(t *testing.T) {
	m, _ := newTestManager(t, nil)
	for _, r := range []CreateReq{
		{Name: "Bad_Name"}, {Image: "../etc"}, {Size: "huge"}, {Team: "a b"},
		{Env: map[string]string{"1X": "y"}}, {Labels: map[string]string{"a b": "c"}}, {Image: "missing"},
	} {
		var ae *apiErr
		if _, err := m.Create(context.Background(), r); !errors.As(err, &ae) || ae.code != 400 {
			t.Errorf("%+v: want 400, got %v", r, err)
		}
	}
	if len(m.list("")) != 0 {
		t.Fatal("invalid request allocated a vm")
	}
	if err := (Config{Subnet: "10.0.0.0/8"}).validate(); err == nil || !strings.Contains(err.Error(), "/16") {
		t.Fatal(err)
	}
}

func TestStopAll(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	a, _ := m.Create(ctx, CreateReq{})
	b, _ := m.Create(ctx, CreateReq{})
	m.stopAll(ctx, "boatd shutdown")
	for _, id := range []string{a.ID, b.ID} {
		if v, _ := m.get(id); v.State != Stopped || !v.HasSnapshot || !v.ResumeOnBoot {
			t.Fatalf("%+v", v)
		}
	}
	m.resumeAll(ctx)
	eventually(t, "resumed", func() bool { v, _ := m.get(b.ID); return v.State == Running && v.AgentReady })
	eventually(t, "resumed", func() bool { v, _ := m.get(a.ID); return v.State == Running && !v.ResumeOnBoot })
}
