package main

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestR50ExactVMUsageWindow(t *testing.T) {
	m, _ := newTestManager(t, nil)
	now := time.Date(2026, 11, 18, 14, 37, 51, 750000000, time.UTC)
	m.now = func() time.Time { return now }
	base := now.Add(-time.Hour)
	m.mu.Lock()
	m.vms["v"] = VM{ID: "v", Team: "t", Size: "large", State: Running, CreatedAt: base, StartedAt: &base}
	m.mu.Unlock()
	// A closed run wins over the stale open run; a subsequent run has a new size.
	for _, r := range []usageRec{
		{"vm", "v", "t", "small", base, base.Add(20 * time.Second)},
		{"vm", "v", "t", "small", base, base.Add(50 * time.Second)},
		{"vm", "v", "other", "large", base, now},
		{"vm", "other-vm", "t", "large", base, now},
	} {
		if err := m.usage.append(r); err != nil {
			t.Fatal(err)
		}
	}
	start := base.Add(40 * time.Second)
	m.mu.Lock()
	v := m.vms["v"]
	v.StartedAt = &start
	m.vms["v"] = v
	m.mu.Unlock()
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	since, until := base.Add(10*time.Second+125*time.Millisecond), base.Add(55*time.Second+375*time.Millisecond)
	q := url.Values{"team": {"t"}, "vm_id": {"v"}, "since": {since.Format(time.RFC3339Nano)}, "until": {until.Format(time.RFC3339Nano)}}
	var rep struct {
		Exact        bool        `json:"exact"`
		VMs          []vmUsage   `json:"vms"`
		Teams        []teamUsage `json:"teams"`
		Intervals    []usageRec  `json:"intervals"`
		Since, Until time.Time
	}
	before := m.usage.reads.Load()
	if st := call(t, srv, "GET", "/v1/usage?"+q.Encode(), nil, &rep); st != 200 {
		t.Fatalf("window status %d", st)
	}
	if !rep.Exact || !rep.Since.Equal(since) || !rep.Until.Equal(until) || len(rep.VMs) != 1 || rep.VMs[0].Seconds != 25.25 || len(rep.Intervals) != 2 {
		t.Fatalf("exact window: %+v", rep)
	}
	if len(rep.Teams) != 1 || rep.Teams[0].BySize["small"] != 9.875 || rep.Teams[0].BySize["large"] != 15.375 {
		t.Fatalf("sizes: %+v", rep.Teams)
	}
	if n := m.usage.reads.Load() - before; n != 1 {
		t.Fatalf("ledger snapshots %d, want 1", n)
	}
	// An obsolete duplicate whose true stop precedes since must not count.
	q.Set("until", base.Add(35*time.Second).Format(time.RFC3339Nano))
	q.Set("since", base.Add(25*time.Second).Format(time.RFC3339Nano))
	rep.VMs = nil
	if st := call(t, srv, "GET", "/v1/usage?"+q.Encode(), nil, &rep); st != 200 || len(rep.VMs) != 0 {
		t.Fatalf("stale duplicate: %d %+v", st, rep)
	}
	q.Set("team", "other")
	var out map[string]any
	if st := call(t, srv, "GET", "/v1/usage?"+q.Encode(), nil, &out); st != 404 {
		t.Fatalf("foreign VM: %d %v", st, out)
	}
	q.Del("team")
	if st := call(t, srv, "GET", "/v1/usage?"+q.Encode(), nil, &out); st != 400 {
		t.Fatalf("missing team: %d %v", st, out)
	}
	q.Set("team", "t")
	q.Set("since", now.Add(-401*24*time.Hour).Format(time.RFC3339Nano))
	q.Set("until", now.Format(time.RFC3339Nano))
	// This VM was created recently, so an all-time request is still fully retained.
	if st := call(t, srv, "GET", "/v1/usage?"+q.Encode(), nil, &out); st != 200 {
		t.Fatalf("recent creation: %d %v", st, out)
	}
	m.mu.Lock()
	v = m.vms["v"]
	v.CreatedAt = now.Add(-500 * 24 * time.Hour)
	m.vms["v"] = v
	m.mu.Unlock()
	if st := call(t, srv, "GET", "/v1/usage?"+q.Encode(), nil, &out); st != 501 {
		t.Fatalf("unretained: %d %v", st, out)
	}
}

func TestR50StoppedAtPersistsAcrossStoppedEdits(t *testing.T) {
	m, _ := newTestManager(t, nil)
	now := time.Date(2026, 11, 18, 14, 37, 51, 123456789, time.UTC)
	m.now = func() time.Time { return now }
	v := VM{ID: "vm-stop", Name: "old", Team: "t", State: Running, CreatedAt: now.Add(-time.Hour), StartedAt: &now}
	if err := os.MkdirAll(m.vmDir(v.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	err := m.commitLocked(VM{}, v, "")
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(17 * time.Second)
	stopped, err := m.update(v.ID, "stop", func(v *VM) { v.State = Stopped })
	if err != nil {
		t.Fatal(err)
	}
	if stopped.StoppedAt == nil || !stopped.StoppedAt.Equal(now) {
		t.Fatalf("stop: %+v", stopped)
	}
	stopTime := now
	now = now.Add(13 * time.Minute)
	name := "renamed"
	patched, err := m.Patch(v.ID, PatchReq{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if patched.StoppedAt == nil || !patched.StoppedAt.Equal(stopTime) || !patched.UpdatedAt.Equal(now) {
		t.Fatalf("patch: %+v", patched)
	}
	var disk VM
	if err := readJSON(filepath.Join(m.vmDir(v.ID), "vm.json"), &disk); err != nil || disk.StoppedAt == nil || !disk.StoppedAt.Equal(stopTime) {
		t.Fatalf("disk: %+v %v", disk, err)
	}
	resumed, err := m.update(v.ID, "start", func(v *VM) { v.State = Running })
	if err != nil {
		t.Fatal(err)
	}
	if resumed.StoppedAt != nil || resumed.StartedAt == nil || !resumed.StartedAt.Equal(now) {
		t.Fatalf("restart: %+v", resumed)
	}
	legacy := VM{ID: "legacy", State: Stopped, UpdatedAt: stopTime}
	got := m.stampLocked(legacy, legacy)
	if got.StoppedAt == nil || !got.StoppedAt.Equal(stopTime) {
		t.Fatalf("legacy: %+v", got)
	}
}

func TestR50RetypeAndStopMetadata(t *testing.T) {
	m, _ := newTestManager(t, nil)
	base := time.Date(2026, 11, 18, 14, 37, 17, 125000000, time.UTC)
	var delta atomic.Int64
	m.now = func() time.Time { return base.Add(time.Duration(delta.Load())) }
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Name: "usage", Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	delta.Store(int64(21*time.Second + 250*time.Millisecond))
	stopped, err := m.Stop(ctx, v.ID, "api")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.StoppedAt == nil || !stopped.StoppedAt.Equal(m.now()) {
		t.Fatalf("stop timestamp: %+v", stopped)
	}
	firstStop := *stopped.StoppedAt
	delta.Store(int64(43 * time.Second))
	resized, err := m.Resize(ctx, v.ID, ResizeReq{Size: "large", DiscardMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	if resized.StoppedAt == nil || !resized.StoppedAt.Equal(firstStop) {
		t.Fatalf("resize changed stop: %+v", resized)
	}
	v, err = m.Start(ctx, v.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.StoppedAt != nil || v.StartedAt == nil || !v.StartedAt.Equal(m.now()) {
		t.Fatalf("start metadata: %+v", v)
	}
	delta.Store(int64(59*time.Second + 750*time.Millisecond))
	rep, err := m.VMUsageWindow("t", v.ID, base.Add(10*time.Second), m.now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Teams) != 1 || rep.Teams[0].BySize["default"] != 11.25 || rep.Teams[0].BySize["large"] != 16.75 {
		t.Fatalf("retype window: %+v", rep)
	}
	stopped, err = m.Stop(ctx, v.ID, "ttl")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.StoppedAt == nil || !stopped.StoppedAt.Equal(m.now()) {
		t.Fatalf("second stop: %+v", stopped)
	}
	stopTime := *stopped.StoppedAt
	delta.Store(int64(83 * time.Second))
	again, err := m.Stop(ctx, v.ID, "idle")
	if err != nil {
		t.Fatal(err)
	}
	if again.StoppedAt == nil || !again.StoppedAt.Equal(stopTime) {
		t.Fatalf("repeated stop: %+v", again)
	}
}

func TestR50CompactedExactWindowUnsupported(t *testing.T) {
	m, _ := newTestManager(t, func(c *Config) { c.UsageRetentionDays = 1 })
	now := time.Date(2026, 11, 18, 14, 37, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	base := now.Add(-48 * time.Hour)
	if err := m.usage.append(usageRec{"vm", "v", "t", "default", base, base.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	m.compactUsage()
	// Raising retention afterwards cannot restore VM identities from aggregates.
	m.cfg.UsageRetentionDays = 400
	m.mu.Lock()
	m.vms["v"] = VM{ID: "v", Team: "t", State: Stopped, CreatedAt: base}
	m.mu.Unlock()
	if _, err := m.VMUsageWindow("t", "v", base, now); err == nil {
		t.Fatal("compacted history reported exact")
	}
	rep, err := m.VMUsageWindow("t", "v", now.Add(-time.Minute), now)
	if err != nil || !rep.Exact || len(rep.VMs) != 0 {
		t.Fatalf("retained empty window: %+v %v", rep, err)
	}
}

func TestR50IncompleteExactHistoryUnsupported(t *testing.T) {
	for _, row := range []string{
		"{broken\n",
		`{"kind":"vm","id":"v","team":"t","start":"2026-11-18T12:00:00Z","end":"2026-11-18T13:00:00Z"}` + "\n",
	} {
		m, _ := newTestManager(t, nil)
		now := time.Date(2026, 11, 18, 14, 37, 0, 0, time.UTC)
		m.now = func() time.Time { return now }
		m.mu.Lock()
		m.vms["v"] = VM{ID: "v", Team: "t", State: Stopped, CreatedAt: now.Add(-time.Hour)}
		m.mu.Unlock()
		if err := os.WriteFile(m.usage.path, []byte(row), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := m.VMUsageWindow("t", "v", now.Add(-time.Hour), now); err == nil {
			t.Fatal("incomplete history reported exact")
		}
	}
}

func TestR50ExitRecoveryAndForceStopTimestamps(t *testing.T) {
	m, h := newTestManager(t, nil)
	base := time.Date(2026, 11, 18, 14, 37, 17, 125000000, time.UTC)
	var delta atomic.Int64
	m.now = func() time.Time { return base.Add(time.Duration(delta.Load())) }
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Name: "metadata", Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	delta.Store(int64(17 * time.Second))
	h.Kill(v.PID)
	eventually(t, "stop timestamp after exit", func() bool {
		var got VM
		return readJSON(filepath.Join(m.vmDir(v.ID), "vm.json"), &got) == nil && got.State == Stopped && got.StoppedAt != nil && got.StoppedAt.Equal(m.now())
	})
	// A daemon restart preserves the persisted exit time across recovery.
	m.closeWatches()
	m2, err := newManager(m.cfg, h.run, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m2.closeWatches)
	got, err := m2.get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StoppedAt == nil || !got.StoppedAt.Equal(m.now()) {
		t.Fatalf("recovery timestamp: %+v", got)
	}
	m2.now = m.now
	delta.Store(int64(31 * time.Second))
	got, err = m2.Start(ctx, v.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.StoppedAt != nil {
		t.Fatalf("recovery restart kept stop: %+v", got)
	}
	delta.Store(int64(41*time.Second + 125*time.Millisecond))
	got, err = m2.stop(ctx, v.ID, "force", true)
	if err != nil {
		t.Fatal(err)
	}
	if got.StoppedAt == nil || !got.StoppedAt.Equal(m2.now()) {
		t.Fatalf("force timestamp: %+v", got)
	}
}

func TestR50LegacyActiveUsageUnsupported(t *testing.T) {
	m, _ := newTestManager(t, nil)
	m.mu.Lock()
	m.vms["v"] = VM{ID: "v", Team: "t", State: Running, Size: "default", CreatedAt: time.Now().UTC()}
	m.mu.Unlock()
	if _, err := m.VMUsageWindow("t", "v", time.Time{}, time.Time{}); err == nil {
		t.Fatal("missing active-run start reported exact")
	}
}
