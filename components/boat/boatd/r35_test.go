package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowSync is an fsync hook that parks until released and counts its calls.
type slowSync struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func newSlowSync() *slowSync {
	return &slowSync{entered: make(chan struct{}, 64), release: make(chan struct{})}
}

func (s *slowSync) fn(f *os.File) error {
	s.calls.Add(1)
	s.entered <- struct{}{}
	<-s.release
	return nil
}

func ledgerLines(t *testing.T, path string) int {
	t.Helper()
	recs, err := (&ledger{path: path}).read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return len(recs)
}

// A stop's ledger append must not fsync under the manager lock: Get, List and other
// updates finish while the fsync is still in progress, and the record is durable after
// flush.
func TestR35LedgerFsyncOffGlobalLock(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ss := newSlowSync()
	m.usage.syncFn, m.usage.flushLag = ss.fn, 5*time.Millisecond
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { _, err := m.Stop(ctx, v.ID, "api"); stopped <- err }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop blocked on the ledger fsync")
	}
	select {
	case <-ss.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("background flusher never ran fsync")
	}
	// fsync is now parked: nothing under m.mu may wait for it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := m.get(v.ID); err != nil {
			t.Error(err)
		}
		if len(m.list("")) == 0 {
			t.Error("empty list")
		}
		m.update(v.ID, "touch", func(x *VM) { x.Name = "renamed" })
		m.usage.append(usageRec{"vm", "x", "t", "s", time.Now(), time.Now()}) // append during an fsync
		if _, err := m.Usage(time.Time{}); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Get/List/update/append blocked while the ledger fsync was in progress")
	}
	if n := ledgerLines(t, m.usage.path); n != 2 {
		t.Fatalf("records visible before fsync: %d, want 2", n)
	}
	close(ss.release)
	if err := m.usage.flush(); err != nil {
		t.Fatal(err)
	}
	m.usage.mu.Lock()
	dirty := m.usage.dirty
	m.usage.mu.Unlock()
	if dirty {
		t.Fatal("ledger still dirty after flush")
	}
}

// flush is a barrier: a second caller waits for the fsync in flight instead of
// returning before the records are durable; and an fsync error keeps them dirty.
func TestR35LedgerFlushBarrierAndError(t *testing.T) {
	l := &ledger{path: filepath.Join(t.TempDir(), "usage.jsonl"), flushLag: time.Hour}
	ss := newSlowSync()
	l.syncFn = ss.fn
	if err := l.append(usageRec{"vm", "a", "t", "s", time.Now(), time.Now()}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); l.flush() }()
	<-ss.entered
	second := make(chan struct{})
	go func() { l.flush(); close(second) }()
	select {
	case <-second:
		t.Fatal("second flush returned while the first fsync was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(ss.release)
	wg.Wait()
	<-second

	l2 := &ledger{path: filepath.Join(t.TempDir(), "usage.jsonl"), flushLag: time.Hour}
	l2.syncFn = func(*os.File) error { return os.ErrInvalid }
	l2.append(usageRec{"vm", "a", "t", "s", time.Now(), time.Now()})
	if err := l2.flush(); err == nil {
		t.Fatal("fsync error swallowed")
	}
	var calls int
	l2.syncFn = func(*os.File) error { calls++; return nil }
	if err := l2.flush(); err != nil || calls != 1 {
		t.Fatalf("failed flush not retried: calls=%d err=%v", calls, err)
	}
}

// Clean shutdown (stopAll) and closeWatches flush the ledger even when the background
// timer has not fired.
func TestR35LedgerFlushedOnShutdown(t *testing.T) {
	m, _ := newTestManager(t, nil)
	var syncs atomic.Int64
	m.usage.syncFn, m.usage.flushLag = func(*os.File) error { syncs.Add(1); return nil }, time.Hour
	ctx := context.Background()
	if _, err := m.Create(ctx, CreateReq{Team: "t1"}); err != nil {
		t.Fatal(err)
	}
	m.stopAll(ctx, shutdownReason)
	if syncs.Load() != 1 {
		t.Fatalf("stopAll fsyncs = %d, want 1", syncs.Load())
	}
	if n := ledgerLines(t, m.usage.path); n != 1 {
		t.Fatalf("ledger records after shutdown: %d, want 1", n)
	}
	m.usage.append(usageRec{"vm", "z", "t", "s", time.Now(), time.Now()})
	m.closeWatches()
	if syncs.Load() != 2 {
		t.Fatalf("closeWatches fsyncs = %d, want 2", syncs.Load())
	}
}

// The background timer flushes without anyone asking, and goes idle afterwards.
func TestR35LedgerBackgroundFlush(t *testing.T) {
	l := &ledger{path: filepath.Join(t.TempDir(), "usage.jsonl"), flushLag: 5 * time.Millisecond}
	var syncs atomic.Int64
	l.syncFn = func(*os.File) error { syncs.Add(1); return nil }
	for i := 0; i < 20; i++ {
		l.append(usageRec{"vm", "a", "t", "s", time.Now(), time.Now()})
	}
	eventually(t, "background fsync", func() bool { return syncs.Load() >= 1 })
	time.Sleep(50 * time.Millisecond)
	n := syncs.Load()
	time.Sleep(50 * time.Millisecond)
	if syncs.Load() != n {
		t.Fatal("flusher keeps syncing with nothing to write")
	}
	l.mu.Lock()
	on := l.timerOn
	l.mu.Unlock()
	if on {
		t.Fatal("flush timer still armed while idle")
	}
	l.append(usageRec{"vm", "b", "t", "s", time.Now(), time.Now()}) // re-arms
	eventually(t, "second fsync", func() bool { return syncs.Load() > n })
}

// N rapid Usage() calls walk the dirs once; creating or deleting a snapshot, or
// deleting a VM, makes the next call walk again.
func TestR35StorageCache(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	m.storageCache.invalidate()
	base := m.storageCache.walks.Load()
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
	if w := m.storageCache.walks.Load() - base; w != 1 {
		t.Fatalf("40 Usage calls walked %d times, want 1", w)
	}
	walks := func() int64 { return m.storageCache.walks.Load() - base }

	s, err := m.CreateSnapshot(ctx, v.ID, "s1")
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := m.Usage(time.Time{})
	if walks() != 2 || len(rep.Storage) != 1 || rep.Storage[0].Snapshots != 1 {
		t.Fatalf("snapshot create not seen: walks=%d %+v", walks(), rep.Storage)
	}
	m.Usage(time.Time{})
	if walks() != 2 {
		t.Fatalf("cache not reused: %d", walks())
	}
	if err := m.DeleteSnapshot(v.ID, s.ID); err != nil {
		t.Fatal(err)
	}
	rep, _ = m.Usage(time.Time{})
	if walks() != 3 || rep.Storage[0].Snapshots != 0 {
		t.Fatalf("snapshot delete not seen: walks=%d %+v", walks(), rep.Storage)
	}
	if _, err := m.Delete(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	rep, _ = m.Usage(time.Time{})
	if walks() != 4 || len(rep.Storage) != 0 {
		t.Fatalf("VM delete not seen: walks=%d %+v", walks(), rep.Storage)
	}
}

// The TTL bounds staleness, and callers get copies they cannot corrupt the cache with.
func TestR35StorageCacheTTLAndCopies(t *testing.T) {
	old := storageCacheTTL
	storageCacheTTL = 40 * time.Millisecond
	defer func() { storageCacheTTL = old }()
	m, _ := newTestManager(t, nil)
	if _, err := m.Create(context.Background(), CreateReq{Team: "t1"}); err != nil {
		t.Fatal(err)
	}
	a, _ := m.storage()
	a["t1"].VMs = 99
	b, _ := m.storage()
	if b["t1"].VMs != 1 {
		t.Fatalf("cache aliased the caller's map: %+v", b["t1"])
	}
	n := m.storageCache.walks.Load()
	time.Sleep(60 * time.Millisecond)
	m.storage()
	if m.storageCache.walks.Load() != n+1 {
		t.Fatal("TTL did not expire the cache")
	}
}

// A walk that began before an invalidation must not repopulate the cache.
func TestR35StorageCacheInvalidateDuringWalk(t *testing.T) {
	var c storageCache
	release := make(chan struct{})
	started := make(chan struct{})
	res := make(chan map[string]*teamStorage, 1)
	go func() {
		out, _ := c.get(1, time.Hour, func() (map[string]teamStorage, diskSpace) {
			close(started)
			<-release
			return map[string]teamStorage{"t": {VMs: 1}}, diskSpace{}
		})
		res <- out
	}()
	<-started
	c.invalidate()
	close(release)
	if out := <-res; out["t"].VMs != 1 {
		t.Fatalf("walk result lost: %+v", out)
	}
	out, _ := c.get(1, time.Hour, func() (map[string]teamStorage, diskSpace) {
		return map[string]teamStorage{"t": {VMs: 2}}, diskSpace{}
	})
	if out["t"].VMs != 2 {
		t.Fatalf("stale walk repopulated the cache: %+v", out["t"])
	}
}

// A conn in flight while the snapshot barrier closes the socket's pool must not be
// returned to the pool afterwards.
func TestR35PoolGenerationDiscardsInFlightConn(t *testing.T) {
	g := newR34Guest(t)
	p := &connPool{idle: map[string][]*idleConn{}}
	ctx := context.Background()
	a, _, err := p.take(ctx, g.sock, "health")
	if err != nil {
		t.Fatal(err)
	}
	p.closeSock(g.sock) // closeAgentConns after a snapshot, while a is mid-op
	p.put(g.sock, "health", a, reply{})
	if n := len(p.idle[g.sock]); n != 0 {
		t.Fatalf("stale-generation conn pooled: %d idle", n)
	}
	if !g.waitClosed() {
		t.Fatal("stale conn not closed")
	}
	// A conn taken after the bump is pooled as usual.
	b, _, err := p.take(ctx, g.sock, "health")
	if err != nil {
		t.Fatal(err)
	}
	p.put(g.sock, "health", b, reply{})
	if n := len(p.idle[g.sock]); n != 1 {
		t.Fatalf("current-generation conn not pooled: %d idle", n)
	}
	// Popped from the pool, then outdated mid-op: discarded too.
	c, reused, err := p.take(ctx, g.sock, "health")
	if err != nil || !reused {
		t.Fatalf("take: reused=%v err=%v", reused, err)
	}
	p.closeSock(g.sock)
	p.put(g.sock, "health", c, reply{})
	if n := len(p.idle[g.sock]); n != 0 {
		t.Fatalf("popped conn outdated mid-op was pooled: %d idle", n)
	}
	p.closeSock(g.sock)
}

// Pruning the generation table keeps older in-flight conns discarded.
func TestR35PoolGenerationPrune(t *testing.T) {
	g := newR34Guest(t)
	p := &connPool{idle: map[string][]*idleConn{}}
	a, _, err := p.take(context.Background(), g.sock, "health")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= agentPoolMaxGens; i++ {
		p.closeSock(filepath.Join("/nonexistent", string(rune('a'+i%26)), "x", strconv.Itoa(i)))
	}
	if len(p.gens) > agentPoolMaxGens {
		t.Fatalf("generation table unbounded: %d", len(p.gens))
	}
	p.put(g.sock, "health", a, reply{})
	if len(p.idle[g.sock]) != 0 {
		t.Fatal("conn from before the prune was pooled")
	}
	b, _, err := p.take(context.Background(), g.sock, "health")
	if err != nil {
		t.Fatal(err)
	}
	p.put(g.sock, "health", b, reply{})
	if len(p.idle[g.sock]) != 1 {
		t.Fatal("conn taken after the prune not pooled")
	}
	p.closeSock(g.sock)
}
