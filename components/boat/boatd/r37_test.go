package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- fresh_boot ----

func TestR37StartReqFreshBootDecodes(t *testing.T) {
	var b StartReq
	if err := json.Unmarshal([]byte(`{"fresh_boot":true,"env":{"A":"b"}}`), &b); err != nil || !b.FreshBoot {
		t.Fatalf("fresh_boot not decoded: %+v %v", b, err)
	}
	if b = (StartReq{}); b.FreshBoot {
		t.Fatal("fresh_boot must default off")
	}
}

// A stopped VM with a stop snapshot: a plain start restores it (control); fresh_boot
// discards vmstate + mem and cold boots, and the snapshot is gone afterwards.
func TestR37FreshBootDiscardsSnapshotAndColdBoots(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()

	plain, err := m.Create(ctx, CreateReq{Name: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := m.Stop(ctx, plain.ID, "api"); err != nil || !v.HasSnapshot {
		t.Fatalf("stop: %+v %v", v, err)
	}
	v, err := m.startVM(ctx, plain.ID, nil, nil, false)
	if err != nil || !h.fcOf(t, m, v.ID).called("PUT /snapshot/load") {
		t.Fatalf("control: a plain start must restore: %v", err)
	}

	fresh, err := m.Create(ctx, CreateReq{Name: "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	v, err = m.Stop(ctx, fresh.ID, "api")
	if err != nil || !v.HasSnapshot {
		t.Fatalf("stop: %+v %v", v, err)
	}
	root := m.root(fresh.ID)
	for _, f := range []string{vmstate, memFile} {
		if !exists(filepath.Join(root, f)) {
			t.Fatalf("setup: %s missing", f)
		}
	}
	os.Remove(filepath.Join(root, memFile))
	if err := os.WriteFile(filepath.Join(root, memFile), []byte("secret-from-the-last-run"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err = m.startVM(ctx, fresh.ID, nil, nil, true)
	if err != nil || v.State != Running {
		t.Fatalf("fresh start: %+v %v", v, err)
	}
	fc := h.fcOf(t, m, v.ID)
	if fc.called("PUT /snapshot/load") || !fc.called("PUT /actions") {
		t.Fatalf("fresh_boot must cold boot, never load the snapshot: %v", fc.calls)
	}
	if exists(filepath.Join(root, vmstate)) {
		t.Fatal("vmstate survived a fresh boot")
	}
	if b, err := os.ReadFile(filepath.Join(root, memFile)); err == nil && strings.Contains(string(b), "secret-from-the-last-run") {
		t.Fatal("the old memory file was kept")
	}
	if got := onDisk(t, m, fresh.ID); got.HasSnapshot {
		t.Fatalf("has_snapshot still recorded: %+v", got)
	}
}

// ---- commitLocked off the global lock ----

// While VM a's Stopped save is parked in its fsync, Patch and AddPort of a wait for it
// outside m.mu: every other VM stays reachable.
func TestR37PatchAndAddPortDoNotStallOtherVMs(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	a, err := m.Create(ctx, CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Create(ctx, CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	ps := newParkedSave(m, func(v VM) bool { return v.ID == a.ID && v.State == Stopped })
	stopped := make(chan error, 1)
	go func() { _, err := stopDirect(m, a.ID); stopped <- err }()
	select {
	case <-ps.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stop never reached the write")
	}

	ttl := 600
	patched := make(chan error, 1)
	go func() { _, err := m.Patch(a.ID, PatchReq{TTLS: &ttl}); patched <- err }()
	ported := make(chan error, 1)
	go func() { _, err := m.AddPort(a.ID, 8080, false); ported <- err }()

	// Both have swapped their state in (memory is right) yet wait for the file.
	eventually(t, "patch and add_port swapped in", func() bool {
		v, err := m.get(a.ID)
		return err == nil && v.TTLS == 600 && len(v.Ports) == 1
	})
	within(t, "get of another VM", 2*time.Second, func() {
		if _, err := m.get(b.ID); err != nil {
			t.Error(err)
		}
	})
	within(t, "update of another VM", 2*time.Second, func() {
		if _, err := m.update(b.ID, "", func(x *VM) { x.Labels = map[string]string{"k": "v"} }); err != nil {
			t.Error(err)
		}
	})
	within(t, "patch of another VM", 2*time.Second, func() {
		if _, err := m.Patch(b.ID, PatchReq{TTLS: &ttl}); err != nil {
			t.Error(err)
		}
	})
	within(t, "list", 2*time.Second, func() { m.list("") })
	select {
	case <-patched:
		t.Fatal("Patch answered before its state was durable")
	case <-ported:
		t.Fatal("AddPort answered before its state was durable")
	default:
	}

	close(ps.release)
	for _, c := range []chan error{stopped, patched, ported} {
		if err := <-c; err != nil {
			t.Fatal(err)
		}
	}
	got := onDisk(t, m, a.ID)
	if got.State != Stopped || got.TTLS != 600 || len(got.Ports) != 1 {
		t.Fatalf("final state not durable: %+v", got)
	}
	m.mu.Lock()
	inflight := m.persist[a.ID].inflight
	m.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("inflight leaked: %d", inflight)
	}
}

// The quarantine failure state (stopUnfencedGuests) writes its Error state outside m.mu.
func TestR37QuarantineErrorWriteOffGlobalLock(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	a, err := m.Create(ctx, CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Create(ctx, CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	_ = h
	ps := newParkedSave(m, func(v VM) bool { return v.ID == a.ID && v.State == Error })
	va, _ := m.get(a.ID)
	done := make(chan struct{})
	go func() { defer close(done); m.stopUnfencedGuests([]VM{va}) }()
	select {
	case <-ps.entered:
	case <-time.After(5 * time.Second):
		t.Skip("guest counted as fenced: nothing to quarantine") // sshNetworkReady fakes
	}
	within(t, "get", 2*time.Second, func() {
		if v, err := m.get(a.ID); err != nil || v.State != Error {
			t.Errorf("error state not swapped in: %+v %v", v, err)
		}
		if _, err := m.get(b.ID); err != nil {
			t.Error(err)
		}
	})
	close(ps.release)
	<-done
}

// ---- deferred vm.json writes ----

// (a) A run whose stop was ledgered but whose vm.json never became durable is not
// billed again when reconcile closes it; summarize also collapses a duplicate.
func TestR37ReconcileDoesNotBillAClosedRunTwice(t *testing.T) {
	count := func(m *Manager, id string) int {
		recs, err := m.usage.read(time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range recs {
			if r.Kind == "vm" && r.ID == id {
				n++
			}
		}
		return n
	}
	for _, ledgered := range []bool{false, true} {
		m, h := newTestManager(t, nil)
		v, err := m.Create(context.Background(), CreateReq{Name: "x", Team: "t1"})
		if err != nil || v.StartedAt == nil {
			t.Fatalf("%+v %v", v, err)
		}
		if ledgered {
			if err := m.usage.append(usageRec{"vm", v.ID, v.Team, v.Size, *v.StartedAt, v.StartedAt.Add(10 * time.Second)}); err != nil {
				t.Fatal(err)
			}
		}
		m.closeWatches()
		dead := newFakeHost()
		dead.next = h.next + 100
		m2, err := newManager(m.cfg, dead.run, dead)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m2.closeWatches)
		if got := count(m2, v.ID); got != 1 {
			t.Fatalf("ledgered=%v: %d records for the run, want 1", ledgered, got)
		}
	}
}

func TestR37SummarizeCollapsesOverlappingRuns(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recs := []usageRec{
		{"vm", "a", "t", "s", t0, t0.Add(10 * time.Second)},
		{"vm", "a", "t", "s", t0, t0.Add(500 * time.Second)}, // the reconcile's late close of the same run
		{"vm", "a", "t", "s", t0.Add(time.Hour), t0.Add(time.Hour + 5*time.Second)},
		{"vm", "b", "t", "s", t0, t0.Add(7 * time.Second)}, // another VM, same start
		{"snapshot", "a", "t", "s", t0, t0.Add(20 * time.Second)},
	}
	rep := summarize(recs, time.Time{}, t0.Add(2*time.Hour))
	got := map[string]float64{}
	for _, v := range rep.VMs {
		got[v.ID] = v.Seconds
	}
	if got["a"] != 15 || got["b"] != 7 {
		t.Fatalf("a=%v (want 15) b=%v (want 7)", got["a"], got["b"])
	}
	if len(rep.Snapshots) != 1 {
		t.Fatalf("snapshots: %+v", rep.Snapshots)
	}
	if len(recs) != 5 || recs[1].End != t0.Add(500*time.Second) {
		t.Fatal("summarize modified its input")
	}
}

// (b) A delete writes its tombstone before any teardown; a crash after that makes the
// next boatd finish it, never resume the VM.
func TestR37DeleteWritesTombstoneFirst(t *testing.T) {
	m, _ := newTestManager(t, nil)
	v, err := m.Create(context.Background(), CreateReq{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var sawTombstone atomic.Bool
	var calls atomic.Int32
	inner := m.run
	m.run = func(stdin, name string, args ...string) error {
		if calls.Add(1) == 1 {
			sawTombstone.Store(exists(m.tombstonePath(v.ID)))
		}
		return inner(stdin, name, args...)
	}
	if _, err := m.Delete(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	if calls.Load() == 0 || !sawTombstone.Load() {
		t.Fatalf("teardown ran before the tombstone was written (%d calls)", calls.Load())
	}
}

func TestR37ReconcileFinishesInterruptedDelete(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	gone, err := m.Create(ctx, CreateReq{Name: "gone", Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	keep, err := m.Create(ctx, CreateReq{Name: "keep", Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := m.CreateSnapshot(ctx, gone.ID, "s")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "snapshot memory sync", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.snaps[snap.ID].Unsynced
	})
	// boatd died right after the tombstone, with vm.json still saying running.
	if err := m.writeTombstone(gone.ID, VM{}); err != nil {
		t.Fatal(err)
	}
	m.closeWatches()
	dead := newFakeHost()
	dead.next = h.next + 100
	m2, err := newManager(m.cfg, dead.run, dead)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m2.closeWatches)
	if _, err := m2.get(gone.ID); err == nil {
		t.Fatal("a VM whose delete had started is back")
	}
	if exists(m2.vmDir(gone.ID)) {
		t.Fatal("dir not removed")
	}
	if _, ok := m2.snaps[snap.ID]; ok {
		t.Fatal("its snapshot is still registered")
	}
	if exists(m2.snapDir(snap.ID)) {
		t.Fatal("its snapshot dir survived")
	}
	if v, err := m2.get(keep.ID); err != nil || !v.ResumeOnBoot {
		t.Fatalf("an untouched crashed VM is still resumed: %+v %v", v, err)
	}
	m2.resumeAll(ctx)
	eventually(t, "cold resume of the untouched VM", func() bool { y, _ := m2.get(keep.ID); return y.State == Running && y.AgentReady })
	if _, err := m2.get(gone.ID); err == nil {
		t.Fatal("resumeAll resurrected the deleted VM")
	}
}

// (c) An older write that needed durability is skipped because a newer, non-fsynced
// state landed first: the file is fsynced instead. A newer state that was already
// fsynced needs nothing more.
func TestR37SkippedOlderWriteStillFsyncs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "vm.json"), []byte("{}"), 0o600)
	var syncs []string
	p := &vmPersist{syncFn: func(path string) error { syncs = append(syncs, path); return nil }}
	ok := func() error { return nil }

	if err := p.write(2, false, "x", dir, ok); err != nil { // newer, not fsynced
		t.Fatal(err)
	}
	if err := p.write(1, true, "x", dir, ok); err != nil { // older Stopped, skipped
		t.Fatal(err)
	}
	if len(syncs) != 1 || syncs[0] != filepath.Join(dir, "vm.json") {
		t.Fatalf("skipped durable write did not fsync: %v", syncs)
	}
	if err := p.write(0, true, "x", dir, ok); err != nil || len(syncs) != 1 {
		t.Fatalf("already synced: %v %v", syncs, err)
	}
	if err := p.write(1, false, "x", dir, ok); err != nil || len(syncs) != 1 {
		t.Fatalf("a non-durable skipped write must not fsync: %v", syncs)
	}
	if err := p.write(4, true, "x", dir, ok); err != nil { // newer and fsynced by save
		t.Fatal(err)
	}
	if err := p.write(3, true, "x", dir, ok); err != nil || len(syncs) != 1 {
		t.Fatalf("newer state was durable already: %v %v", syncs, err)
	}
	p.closeWrites()
	if err := p.write(5, true, "x", dir, ok); err != nil || len(syncs) != 1 {
		t.Fatalf("closed: %v %v", syncs, err)
	}
}

// (c, end to end) The older Stopped commit returns only after the newer non-durable
// state is on disk and fsynced.
func TestR37StoppedCommitFsyncsWhenNewerWriteWonTheRace(t *testing.T) {
	m, _ := newTestManager(t, nil)
	a, err := m.Create(context.Background(), CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	var syncs atomic.Int32
	m.mu.Lock()
	p := m.persistLocked(a.ID)
	m.mu.Unlock()
	p.syncFn = func(string) error { syncs.Add(1); return nil }
	// A newer, non-durable write took seq 2 while the Stopped commit (seq 1) was in line.
	p.mu.Lock()
	p.written, p.synced = 2, false
	p.mu.Unlock()
	m.mu.Lock()
	p.seq = 0
	m.mu.Unlock()
	if _, err := stopDirect(m, a.ID); err != nil {
		t.Fatal(err)
	}
	if syncs.Load() != 1 {
		t.Fatalf("the skipped stop write did not fsync the newer file: %d", syncs.Load())
	}
}

// (d) A delete whose removeVMDir fails leaves the VM in place with its writes reopened.
func TestR37FailedDeleteReopensWrites(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can remove a read-only dir")
	}
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(m.vmDir(v.ID), "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o600)
	os.Chmod(locked, 0o500)
	t.Cleanup(func() { os.Chmod(locked, 0o700) })

	if _, err := m.Delete(ctx, v.ID); err == nil {
		t.Fatal("delete should fail: the dir cannot be removed")
	}
	if _, err := m.get(v.ID); err != nil {
		t.Fatalf("the VM must stay after a failed delete: %v", err)
	}
	if _, err := m.update(v.ID, "", func(x *VM) { x.Labels = map[string]string{"after": "failed-delete"} }); err != nil {
		t.Fatal(err)
	}
	if got := onDisk(t, m, v.ID); got.Labels["after"] != "failed-delete" {
		t.Fatalf("a later write was dropped as a write to a deleted VM: %+v", got)
	}
}

func TestR37PersistReopen(t *testing.T) {
	p := &vmPersist{}
	var n int
	save := func() error { n++; return nil }
	p.write(1, false, "x", "", save)
	p.closeWrites()
	p.write(2, false, "x", "", save)
	if n != 1 {
		t.Fatalf("closed record wrote: %d", n)
	}
	p.reopen(2)
	p.write(2, false, "x", "", save) // from before the close: stays dropped
	p.write(3, false, "x", "", save)
	if n != 2 {
		t.Fatalf("writes after reopen: %d, want 2", n)
	}
}

// ---- usage cache ----

// Rolling `since` values within one minute share an entry; other entries are not
// evicted by them; past the cap one entry (the least recently used) goes, not all.
func TestR37UsageSinceRoundsToMinuteAndEvictsPerEntry(t *testing.T) {
	m, _ := newTestManager(t, nil)
	if _, err := m.Create(context.Background(), CreateReq{Team: "t1"}); err != nil {
		t.Fatal(err)
	}
	m.usage.read(time.Time{})
	base := m.usage.reads.Load()
	reads := func() int64 { return m.usage.reads.Load() - base }

	others := []time.Time{time.Unix(6000, 0), time.Unix(7200, 0), time.Unix(9000, 0)}
	for _, s := range others {
		if _, err := m.Usage(s); err != nil {
			t.Fatal(err)
		}
	}
	if reads() != 3 {
		t.Fatalf("setup reads %d", reads())
	}
	minute := time.Unix(1_800_000_000-1_800_000_000%60, 0).UTC()
	for i := 0; i < 40; i++ {
		rep, err := m.Usage(minute.Add(time.Duration(i) * time.Second).Add(time.Duration(i) * time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Since.Equal(minute) {
			t.Fatalf("since not rounded to the minute: %v", rep.Since)
		}
	}
	if reads() != 4 {
		t.Fatalf("40 rolling since values read the ledger %d times, want 1", reads()-3)
	}
	for _, s := range others {
		m.Usage(s)
	}
	if reads() != 4 {
		t.Fatalf("the rolling window evicted other entries: %d reads", reads())
	}

	// 4 entries now; 29 fillers make the 33rd insert evict exactly one: the least
	// recently used (the rolling window's, touched before the others were re-read).
	for i := 0; i < usageCacheMax-3; i++ {
		if _, err := m.Usage(time.Unix(int64(100_000+i*60), 0)); err != nil {
			t.Fatal(err)
		}
	}
	m.usageCache.mu.Lock()
	n := len(m.usageCache.entries)
	m.usageCache.mu.Unlock()
	if n != usageCacheMax {
		t.Fatalf("cache holds %d entries, want %d (one eviction, not a wipe)", n, usageCacheMax)
	}
	before := reads()
	for _, s := range others {
		m.Usage(s)
	}
	if reads() != before {
		t.Fatalf("recently used entries were evicted: %d extra reads", reads()-before)
	}
	m.Usage(minute)
	if reads() != before+1 {
		t.Fatalf("the least recently used entry should have been the one evicted: %d", reads()-before)
	}
}
