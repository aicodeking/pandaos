package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// parkedSave wraps m.saveState: a write of a VM in a state `park` accepts blocks (after
// signalling entered) until release is closed. It records the states written, in order.
type parkedSave struct {
	real    func(string, VM) error
	park    func(VM) bool
	entered chan State
	release chan struct{}

	mu      sync.Mutex
	written []State
}

func newParkedSave(m *Manager, park func(VM) bool) *parkedSave {
	p := &parkedSave{real: m.saveState, park: park, entered: make(chan State, 8), release: make(chan struct{})}
	m.mu.Lock()
	m.saveState = p.save
	m.mu.Unlock()
	return p
}

func (p *parkedSave) save(dir string, v VM) error {
	if p.park(v) {
		p.entered <- v.State
		<-p.release
	}
	err := p.real(dir, v)
	p.mu.Lock()
	p.written = append(p.written, v.State)
	p.mu.Unlock()
	return err
}

func (p *parkedSave) states() []State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]State(nil), p.written...)
}

func within(t *testing.T, what string, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s blocked for %v", what, d)
	}
}

func onDisk(t *testing.T, m *Manager, id string) VM {
	t.Helper()
	var v VM
	if err := readJSON(filepath.Join(m.vmDir(id), "vm.json"), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func stopDirect(m *Manager, id string) (VM, error) {
	return m.update(id, "r36", func(x *VM) { x.State, x.PID = Stopped, 0 })
}

// A Stopped commit's fsync is parked: Get, List and updates of other VMs proceed, and
// the commit returns only once vm.json is durable.
func TestR36StoppedFsyncOffGlobalLock(t *testing.T) {
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

	returned := make(chan error, 1)
	go func() { _, err := stopDirect(m, a.ID); returned <- err }()
	select {
	case st := <-ps.entered:
		if st != Stopped {
			t.Fatalf("parked %s", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop never reached the write")
	}

	within(t, "get", 2*time.Second, func() {
		v, err := m.get(a.ID)
		if err != nil || v.State != Stopped {
			t.Errorf("memory not swapped before the fsync: %+v %v", v, err)
		}
	})
	within(t, "list", 2*time.Second, func() {
		if got := len(m.list("")); got != 2 {
			t.Errorf("list: %d", got)
		}
	})
	within(t, "update of another VM", 2*time.Second, func() {
		if _, err := m.update(b.ID, "", func(x *VM) { x.Labels = map[string]string{"k": "v"} }); err != nil {
			t.Error(err)
		}
	})
	within(t, "usage", 2*time.Second, func() {
		if _, err := m.Usage(time.Time{}); err != nil {
			t.Error(err)
		}
	})
	select {
	case <-returned:
		t.Fatal("stop answered before its state was durable")
	default:
	}
	if got := onDisk(t, m, a.ID); got.State == Stopped {
		t.Fatal("test proves nothing: the file was already written")
	}

	close(ps.release)
	if err := <-returned; err != nil {
		t.Fatal(err)
	}
	if got := onDisk(t, m, a.ID); got.State != Stopped {
		t.Fatalf("not durable on return: %s", got.State)
	}
	m.mu.Lock()
	inflight := m.persist[a.ID].inflight
	m.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("inflight leaked: %d", inflight)
	}
}

// A reap wave: 8 stops with parked fsyncs all hold no lock between them.
func TestR36ReapWaveDoesNotSerialise(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 8; i++ {
		v, err := m.Create(ctx, CreateReq{Team: "t1"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, v.ID)
	}
	ps := newParkedSave(m, func(v VM) bool { return v.State == Stopped })
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := stopDirect(m, id); err != nil {
				t.Error(err)
			}
		}()
	}
	for range ids { // every fsync is in flight at once: none waited for another
		select {
		case <-ps.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the stops serialised on a lock")
		}
	}
	close(ps.release)
	wg.Wait()
	for _, id := range ids {
		if onDisk(t, m, id).State != Stopped {
			t.Fatalf("%s not durable", id)
		}
	}
}

// Two commits of one VM: the second waits for the first's write and lands after it.
func TestR36SameVMWritesInOrder(t *testing.T) {
	m, _ := newTestManager(t, nil)
	a, err := m.Create(context.Background(), CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	first := true
	var fmu sync.Mutex
	ps := newParkedSave(m, func(v VM) bool {
		fmu.Lock()
		defer fmu.Unlock()
		p := first && v.State == Stopped
		if p {
			first = false
		}
		return p
	})
	r1 := make(chan error, 1)
	go func() { _, err := stopDirect(m, a.ID); r1 <- err }()
	<-ps.entered
	r2 := make(chan error, 1)
	go func() {
		_, err := m.update(a.ID, "r36", func(x *VM) { x.State, x.Error = Error, "boom" })
		r2 <- err
	}()
	// The second commit swapped its state in, and waits behind the first write.
	eventually(t, "second commit swapped", func() bool { v, _ := m.get(a.ID); return v.State == Error })
	select {
	case <-r2:
		t.Fatal("second commit overtook the parked write")
	case <-time.After(50 * time.Millisecond):
	}
	close(ps.release)
	if err := <-r1; err != nil {
		t.Fatal(err)
	}
	if err := <-r2; err != nil {
		t.Fatal(err)
	}
	if got := ps.states(); len(got) != 2 || got[0] != Stopped || got[1] != Error {
		t.Fatalf("write order %v", got)
	}
	if got := onDisk(t, m, a.ID); got.State != Error {
		t.Fatalf("an older write overwrote the newer: %s", got.State)
	}
}

// An older sequence arriving after a newer one is skipped, and closed writes stay closed.
func TestR36OlderSequenceNeverOverwrites(t *testing.T) {
	p := &vmPersist{}
	var got []int
	rec := func(n int) func() error { return func() error { got = append(got, n); return nil } }
	if err := p.write(2, false, "x", "", rec(2)); err != nil {
		t.Fatal(err)
	}
	if err := p.write(1, false, "x", "", rec(1)); err != nil {
		t.Fatal(err)
	}
	if err := p.write(3, false, "x", "", func() error { return errors.New("fail") }); err == nil {
		t.Fatal("error swallowed")
	}
	if err := p.write(3, false, "x", "", rec(3)); err != nil { // a failed write does not consume its sequence
		t.Fatal(err)
	}
	p.closeWrites()
	p.write(4, false, "x", "", rec(4))
	if len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("writes %v, want [2 3]", got)
	}
}

// A real write failure surfaces, the state stays published and flushDirty retries it.
func TestR36WriteFailureSurfacesAndRetries(t *testing.T) {
	m, _ := newTestManager(t, nil)
	a, err := m.Create(context.Background(), CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	real := m.saveState
	m.mu.Lock()
	m.saveState = func(string, VM) error { return errors.New("boom") }
	m.mu.Unlock()
	if _, err := stopDirect(m, a.ID); err == nil {
		t.Fatal("write error swallowed")
	}
	m.mu.Lock()
	dirty := m.dirty[a.ID]
	m.saveState = real
	m.mu.Unlock()
	if !dirty {
		t.Fatal("failed write not remembered")
	}
	m.flushDirty()
	if got := onDisk(t, m, a.ID); got.State != Stopped {
		t.Fatalf("retry did not land: %s", got.State)
	}
	m.mu.Lock()
	dirty = m.dirty[a.ID]
	m.mu.Unlock()
	if dirty {
		t.Fatal("dirty flag not cleared")
	}
}

// flushDirty's retry fsyncs outside m.mu too.
func TestR36FlushDirtyOffGlobalLock(t *testing.T) {
	m, _ := newTestManager(t, nil)
	a, err := m.Create(context.Background(), CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	real := m.saveState
	m.mu.Lock()
	m.saveState = func(string, VM) error { return syscall.ENOSPC }
	m.mu.Unlock()
	if _, err := stopDirect(m, a.ID); err != nil {
		t.Fatalf("a full disk must not fail the stop: %v", err)
	}
	m.mu.Lock()
	m.saveState = real
	m.mu.Unlock()
	ps := newParkedSave(m, func(v VM) bool { return true })
	done := make(chan struct{})
	go func() { m.flushDirty(); close(done) }()
	<-ps.entered
	within(t, "get during flushDirty", 2*time.Second, func() { m.get(a.ID) })
	close(ps.release)
	<-done
	if got := onDisk(t, m, a.ID); got.State != Stopped {
		t.Fatalf("flush did not land: %s", got.State)
	}
}

// Delete waits for a write in flight and no later write lands in the removed dir.
func TestR36DeleteClosesWrites(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	a, err := m.Create(ctx, CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if exists(m.vmDir(a.ID)) {
		t.Fatal("dir survived")
	}
	m.mu.Lock()
	_, kept := m.persist[a.ID]
	m.mu.Unlock()
	if kept {
		t.Fatal("persist record leaked")
	}
}

// N identical Usage calls (serial or concurrent) read the ledger once; a ledger append
// or a state change makes the next call read again; a different `since` has its own entry.
func TestR36UsageCache(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	m.usage.read(time.Time{}) // build the index
	base := m.usage.reads.Load()
	reads := func() int64 { return m.usage.reads.Load() - base }

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Usage(time.Time{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for i := 0; i < 20; i++ {
		if _, err := m.Usage(time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if reads() != 1 {
		t.Fatalf("40 identical calls read the ledger %d times, want 1", reads())
	}
	rep, _ := m.Usage(time.Time{})
	rep.Teams[0].BySize["x"] = 1 // a caller cannot corrupt the cache
	rep.VMs = nil
	again, _ := m.Usage(time.Time{})
	if len(again.VMs) != 1 || again.Teams[0].BySize["x"] != 0 {
		t.Fatalf("cache aliased: %+v", again)
	}
	if reads() != 1 {
		t.Fatalf("reads %d", reads())
	}

	m.Usage(time.Unix(100, 0))
	if reads() != 2 {
		t.Fatalf("another since must read: %d", reads())
	}

	// A stop closes an interval (a ledger append): the next report has it.
	if _, err := m.Stop(ctx, v.ID, ""); err != nil {
		t.Fatal(err)
	}
	rep, _ = m.Usage(time.Time{})
	if reads() != 3 {
		t.Fatalf("append did not invalidate: %d", reads())
	}
	if len(rep.VMs) != 1 || rep.VMs[0].ID != v.ID {
		t.Fatalf("closed interval missing: %+v", rep.VMs)
	}
}

func TestR36UsageCacheTTLAndError(t *testing.T) {
	m, _ := newTestManager(t, nil)
	if _, err := m.Create(context.Background(), CreateReq{Team: "t1"}); err != nil {
		t.Fatal(err)
	}
	clock := time.Now().UTC()
	m.now = func() time.Time { return clock }
	m.Usage(time.Time{})
	n := m.usage.reads.Load()
	clock = clock.Add(usageCacheTTL - time.Second)
	m.Usage(time.Time{})
	if m.usage.reads.Load() != n {
		t.Fatal("re-read inside the TTL")
	}
	clock = clock.Add(2 * time.Second)
	rep, _ := m.Usage(time.Time{})
	if m.usage.reads.Load() != n+1 {
		t.Fatal("TTL did not expire the report")
	}
	if !rep.Until.Equal(clock) {
		t.Fatalf("until %v, want %v", rep.Until, clock)
	}
}

// storage() answers a hit from the generation, without copying the VMs; every kind of
// inventory change is seen.
func TestR36StorageFastPath(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	m.storage() // warm
	base, hits := m.storageCache.walks.Load(), m.storageCache.hits.Load()
	for i := 0; i < 100; i++ {
		st, _ := m.storage()
		if st["t1"].VMs != 1 {
			t.Fatalf("%+v", st["t1"])
		}
	}
	if w := m.storageCache.walks.Load() - base; w != 0 {
		t.Fatalf("%d walks on a warm cache", w)
	}
	if h := m.storageCache.hits.Load() - hits; h != 100 {
		t.Fatalf("%d fast hits, want 100", h)
	}
	walks := func() int64 { return m.storageCache.walks.Load() - base }

	// A committed-size change (disk grow) is seen without any invalidate call.
	if _, err := m.update(v.ID, "grow", func(x *VM) { x.DiskGiB += 3 }); err != nil {
		t.Fatal(err)
	}
	st, _ := m.storage()
	if walks() != 1 || st["t1"].CommittedBytes != vmCommit(mustGet(t, m, v.ID)) {
		t.Fatalf("size change not seen: walks=%d %+v", walks(), st["t1"])
	}
	// A state change alone does not walk.
	if _, err := stopDirect(m, v.ID); err != nil {
		t.Fatal(err)
	}
	m.storage()
	if walks() != 1 {
		t.Fatalf("a state change walked: %d", walks())
	}
	// A snapshot put in the map by a path that never bumped the generation (import)
	// still misses: the counts are part of the key.
	m.mu.Lock()
	m.snaps["snX"] = Snapshot{ID: "snX", Team: "t1", VMID: v.ID, DiskGiB: 1}
	m.mu.Unlock()
	st, _ = m.storage()
	if walks() != 2 || st["t1"].Snapshots != 1 {
		t.Fatalf("uncounted snapshot missed: walks=%d %+v", walks(), st["t1"])
	}
	m.mu.Lock()
	delete(m.snaps, "snX")
	m.mu.Unlock()
	if _, err := m.Delete(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	st, _ = m.storage()
	if len(st) != 0 {
		t.Fatalf("delete not seen: %+v", st)
	}
}

func mustGet(t *testing.T, m *Manager, id string) VM {
	t.Helper()
	v, err := m.get(id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	log.SetOutput(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return buf.Write(p) }))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

const r36Rec = `{"kind":"vm","id":"vmA","team":"t","size":"default","start":"2026-01-01T00:00:00Z","end":"2026-01-01T00:01:00Z"}`

// A torn last record must not swallow the next one, with or without a read first.
func TestR36LedgerTornTail(t *testing.T) {
	for _, readFirst := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "usage.jsonl")
		torn := r36Rec + "\n" + r36Rec[:40]
		if err := os.WriteFile(path, []byte(torn), 0o600); err != nil {
			t.Fatal(err)
		}
		logs := captureLog(t)
		l := &ledger{path: path}
		if readFirst {
			if recs, err := l.read(time.Time{}); err != nil || len(recs) != 1 {
				t.Fatalf("read: %d %v", len(recs), err)
			}
		}
		next := usageRec{"vm", "vmB", "t", "default", time.Unix(1_800_000_000, 0).UTC(), time.Unix(1_800_000_060, 0).UTC()}
		if err := l.append(next); err != nil {
			t.Fatal(err)
		}
		l2 := &ledger{path: path} // a fresh process reading the file
		recs, err := l2.read(time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 2 || recs[1].ID != "vmB" {
			t.Fatalf("readFirst=%v: the new record was swallowed: %+v", readFirst, recs)
		}
		b, _ := os.ReadFile(path)
		if bytes.Contains(b, []byte(r36Rec[:40]+`{`)) || bytes.Count(b, []byte("\n")) != 2 {
			t.Fatalf("torn bytes left in the file: %q", b)
		}
		if !bytes.Contains(logs.Bytes(), []byte("torn")) {
			t.Fatalf("the dropped bytes were not logged: %q", logs.String())
		}
	}
}

// A whole record that only lost its newline is kept.
func TestR36LedgerWholeRecordWithoutNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := os.WriteFile(path, []byte(r36Rec), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &ledger{path: path}
	if err := l.append(usageRec{"vm", "vmB", "t", "default", time.Unix(1_800_000_000, 0).UTC(), time.Unix(1_800_000_060, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	recs, err := (&ledger{path: path}).read(time.Time{})
	if err != nil || len(recs) != 2 {
		t.Fatalf("%d records, %v", len(recs), err)
	}
}

// No newline anywhere (one torn record): the file is emptied, the next record parses.
func TestR36LedgerAllTorn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	os.WriteFile(path, []byte(r36Rec[:30]), 0o600)
	l := &ledger{path: path}
	if err := l.append(usageRec{"vm", "vmB", "t", "default", time.Unix(1_800_000_000, 0).UTC(), time.Unix(1_800_000_060, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	recs, _ := (&ledger{path: path}).read(time.Time{})
	if len(recs) != 1 || recs[0].ID != "vmB" {
		t.Fatalf("%+v", recs)
	}
}

// Unparsable lines in the middle are skipped and logged (once, not on every query).
func TestR36LedgerLogsUnparsableLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	os.WriteFile(path, []byte(r36Rec+"\n{garbage\n"+r36Rec+"\n"), 0o600)
	logs := captureLog(t)
	l := &ledger{path: path}
	recs, err := l.read(time.Time{})
	if err != nil || len(recs) != 2 {
		t.Fatalf("%d records, %v", len(recs), err)
	}
	if bytes.Count(logs.Bytes(), []byte("unparsable")) != 1 {
		t.Fatalf("log: %q", logs.String())
	}
	l.read(time.Time{})
	if bytes.Count(logs.Bytes(), []byte("unparsable")) != 1 {
		t.Fatalf("logged again on a re-read: %q", logs.String())
	}
}
