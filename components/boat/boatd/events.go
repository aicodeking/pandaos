package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Event is one state change, streamed on GET /v1/events (SSE).
type Event struct {
	Seq    int64     `json:"seq"`
	Time   time.Time `json:"time"`
	Type   string    `json:"type"` // vm.state | vm.deleted | snapshot.created | snapshot.deleted
	VMID   string    `json:"vm_id,omitempty"`
	SnapID string    `json:"snapshot_id,omitempty"`
	Team   string    `json:"team"`
	Size   string    `json:"size,omitempty"`
	State  State     `json:"state,omitempty"`
	Prev   State     `json:"prev,omitempty"`
	Reason string    `json:"reason,omitempty"` // ttl | idle | exited | ...
}

const eventRing = 1024

// broker fans events out to SSE subscribers and keeps the last eventRing for
// Last-Event-ID resume. Slow subscribers are dropped (they reconnect with
// Last-Event-ID); events are lost only if they fall more than eventRing behind.
type broker struct {
	mu   sync.Mutex
	seq  int64
	ring []Event
	subs map[chan Event]struct{}
}

func newBroker() *broker { return &broker{subs: map[chan Event]struct{}{}} }

func (b *broker) publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Seq = b.seq
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	b.ring = append(b.ring, e)
	if len(b.ring) > eventRing {
		b.ring = b.ring[len(b.ring)-eventRing:]
	}
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
			delete(b.subs, ch)
			close(ch)
		}
	}
}

// subscribe returns events after seq `after` plus a live channel.
func (b *broker) subscribe(after int64) ([]Event, chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var backlog []Event
	for _, e := range b.ring {
		if e.Seq > after {
			backlog = append(backlog, e)
		}
	}
	ch := make(chan Event, 256)
	b.subs[ch] = struct{}{}
	return backlog, ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
	}
}

// usageRec is one closed billing interval, appended to usage.jsonl.
type usageRec struct {
	Kind  string    `json:"kind"` // vm | snapshot
	ID    string    `json:"id"`
	Team  string    `json:"team"`
	Size  string    `json:"size,omitempty"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// ledger is usage.jsonl, append-only. Records are appended when an interval closes,
// so their End times only grow (but for clock steps and racing appends, which the
// index tolerates): a sparse index of byte offsets lets a query for [since, now]
// start at the first record that can still end after since, not at byte 0.
type ledger struct {
	mu   sync.Mutex
	path string

	marks   []ledgerMark // every ledgerMarkEvery records, from offset 0
	indexed int64        // bytes of the file the index covers
	count   int          // records in it
	maxEnd  time.Time    // latest End in it
	built   bool

	gen   atomic.Uint64 // bumped by every append: Usage() reports built before it are stale
	reads atomic.Int64  // read calls, for tests

	builds atomic.Int64 // full scans to build the index under mu, for tests

	offBuilds atomic.Int64 // startup index builds installed (scanned without mu), for tests
	buildHook func()       // test hook: runs in buildIndex after the size is pinned, before the unlocked scan

	repaired bool // the torn tail of a previous run was dealt with (under mu)

	// Compaction (ledger_compact.go): intervals older than the retention window are
	// folded into per-day aggregates, which scan loads into aggs (under mu).
	aggs         map[aggKey]int64
	compactMu    sync.Mutex               // one compaction at a time
	compactHook  func(stage string) error // test hook: an error aborts, leaving the temp file
	scanHook     func()                   // test hook: runs in readAll after l.mu is released, before the scan
	aggRetention time.Duration            // how long daily aggregates are kept; 0 = ledgerAggRetention

	// Durability (see append): records are written to the page cache under mu and
	// fsynced later, outside it, by flush.
	dirty    bool                 // written, not yet fsynced (under mu)
	timerOn  bool                 // a flush timer is armed (under mu)
	flushMu  sync.Mutex           // serialises flushes: a second Flush waits for one in flight
	syncFn   func(*os.File) error // fsync; nil = (*os.File).Sync. A test hook
	flushLag time.Duration        // delay before a background flush; 0 = ledgerFlushEvery
}

// ledgerMark: every record before off ends at or before maxEnd.
type ledgerMark struct {
	off    int64
	maxEnd time.Time
}

const ledgerMarkEvery = 1024

// ledgerFlushEvery is how long a written record may wait for its fsync.
//
// Crash safety: append does not fsync (that ran under the manager's global lock and
// stalled every API call for the duration of a slow disk). A record is therefore
// durable ledgerFlushEvery (+ one fsync) after it was written; a host crash or power
// loss inside that window can lose the records written in it, i.e. the last few
// closed billing intervals (a few hundred ms of stops/deletes). A boatd crash alone
// loses nothing: the bytes are already in the kernel. Clean shutdown flushes first.
// Callers that need a durability barrier call flush.
const ledgerFlushEvery = 250 * time.Millisecond

// append writes r (no fsync) and schedules a background flush. It never blocks on the
// disk beyond the write itself, so it is safe to call under the manager's lock.
func (l *ledger) append(r usageRec) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.repairTailLocked()
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	off, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	if l.built && off == l.indexed {
		l.note(r, off+int64(len(b))+1)
	} else {
		l.built = false // written behind our back: rebuild on the next read
	}
	l.dirty = true
	l.gen.Add(1)
	l.armLocked()
	return nil
}

// repairTailLocked deals, once per process before the first append or index build,
// with a torn last record (a host crash mid-write leaves bytes after the last
// newline). Left alone, the next append would glue its record onto them and both
// would be unparsable. A tail that is a whole record missing only its newline is
// kept and terminated; anything else is truncated to the last newline. No newline is
// ever written ahead of an append.
func (l *ledger) repairTailLocked() {
	if l.repaired {
		return
	}
	l.repaired = true
	f, err := os.OpenFile(l.path, os.O_RDWR, 0)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("usage: ledger tail check: %v", err)
		}
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, st.Size()-1); err != nil || last[0] == '\n' {
		return
	}
	// Walk back to the last newline.
	tailStart := int64(0)
	const chunk = 4096
	buf := make([]byte, chunk)
	for end := st.Size(); end > 0 && tailStart == 0; {
		start := max(end-chunk, 0)
		n, err := f.ReadAt(buf[:end-start], start)
		if err != nil && err != io.EOF {
			log.Printf("usage: ledger tail check: %v", err)
			return
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			tailStart = start + int64(i) + 1
			break
		}
		end = start
	}
	tail := make([]byte, st.Size()-tailStart)
	if _, err := f.ReadAt(tail, tailStart); err != nil && err != io.EOF {
		log.Printf("usage: ledger tail check: %v", err)
		return
	}
	var r usageRec
	if json.Unmarshal(tail, &r) == nil {
		if _, err := f.WriteAt([]byte{'\n'}, st.Size()); err != nil {
			log.Printf("usage: ledger: terminating the last record: %v", err)
			return
		}
		log.Printf("usage: ledger: last record had no newline (%d bytes): terminated, kept", len(tail))
		return
	}
	if err := f.Truncate(tailStart); err != nil {
		log.Printf("usage: ledger: dropping torn tail of %d bytes at offset %d: %v", len(tail), tailStart, err)
		return
	}
	log.Printf("usage: ledger: dropped a torn last record (%d bytes at offset %d, a crash mid-write)", len(tail), tailStart)
}

// armLocked starts the one-shot flush timer unless one is pending. The timer re-arms
// itself while records keep arriving and goes idle otherwise: no goroutine to stop.
func (l *ledger) armLocked() {
	if l.timerOn {
		return
	}
	l.timerOn = true
	d := l.flushLag
	if d <= 0 {
		d = ledgerFlushEvery
	}
	time.AfterFunc(d, l.timerFlush)
}

func (l *ledger) timerFlush() {
	if err := l.flush(); err != nil {
		log.Printf("usage: fsync: %v", err)
	}
	l.mu.Lock()
	l.timerOn = false
	if l.dirty {
		l.armLocked() // written during the fsync, or the fsync failed: try again
	}
	l.mu.Unlock()
}

// flush fsyncs every record written so far and returns once they are durable,
// waiting for a flush already in flight. The fsync runs without l.mu, so appends and
// reads (and the manager lock above them) never wait for the disk.
func (l *ledger) flush() error {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()
	l.mu.Lock()
	if !l.dirty {
		l.mu.Unlock()
		return nil
	}
	l.dirty = false
	path, syncFn := l.path, l.syncFn
	l.mu.Unlock()
	if syncFn == nil {
		syncFn = (*os.File).Sync
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil // nothing left to make durable
	}
	if err == nil {
		err = syncFn(f)
		f.Close()
	}
	if err != nil {
		l.mu.Lock()
		l.dirty = true
		l.mu.Unlock()
	}
	return err
}

// note adds the record that ends at byte next to the index.
func (l *ledger) note(r usageRec, next int64) {
	if r.End.After(l.maxEnd) {
		l.maxEnd = r.End
	}
	l.count++
	l.indexed = next
	if l.count%ledgerMarkEvery == 0 {
		l.marks = append(l.marks, ledgerMark{next, l.maxEnd})
	}
}

// read returns the records that may overlap [since, ...): those of the index's last
// mark that ends before since onwards. The first call (or one after the file changed
// behind boatd) scans the whole file once to build the index.
func (l *ledger) read(since time.Time) ([]usageRec, error) {
	recs, _, err := l.readAll(since)
	return recs, err
}

// readAll is read plus the compaction aggregates (see ledger_compact.go) that reach
// into [since, ...): the totals of intervals no longer in the file as records.
//
// l.mu is held only to bring the index up to date and to pin a consistent snapshot
// (file size, start offset, aggregates); the records themselves are parsed afterwards
// from the open file with no lock held, so a query with no `since` (all of the file)
// never blocks an append for the length of the scan. The snapshot stays consistent
// because the file is append-only and a compaction renames a new file over it, which
// leaves the open (old) one intact.
func (l *ledger) readAll(since time.Time) ([]usageRec, []aggRow, error) {
	f, size, from, aggs, err := l.snapshot(since)
	if err != nil || f == nil {
		return nil, nil, err
	}
	defer f.Close()
	if l.scanHook != nil {
		l.scanHook()
	}
	out, err := collectRecords(f, size, from, since)
	if err != nil {
		return nil, nil, err
	}
	return out, aggs, nil
}

// snapshot is the locked part of readAll. It returns a nil file when there is no
// ledger yet.
func (l *ledger) snapshot(since time.Time) (f *os.File, size, from int64, aggs []aggRow, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads.Add(1)
	l.repairTailLocked()
	f, err = os.Open(l.path)
	if os.IsNotExist(err) {
		l.reset()
		l.built = true
		return nil, 0, 0, nil, nil
	}
	if err != nil {
		return nil, 0, 0, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, 0, nil, err
	}
	size = st.Size()
	switch {
	case !l.built || size < l.indexed:
		l.reset()
		l.builds.Add(1)
		err = l.scan(f, size, 0, time.Time{}, nil)
		l.built = err == nil
	case l.indexed < size:
		err = l.scan(f, size, l.indexed, time.Time{}, nil) // records appended since: index only
	}
	if err != nil {
		f.Close()
		return nil, 0, 0, nil, err
	}
	// marks[i].maxEnd grows with i: take the last mark wholly before since.
	i := sort.Search(len(l.marks), func(i int) bool { return !l.marks[i].maxEnd.Before(since) }) - 1
	if i >= 0 {
		from = l.marks[i].off
	}
	return f, size, from, l.aggRowsLocked(since), nil
}

// collectRecords parses f[from:size) and returns the records ending after since. It
// touches no ledger state, so it runs without l.mu. Aggregate lines are skipped (the
// caller has them from the index) and so are unparsable ones, as in scan.
func collectRecords(f *os.File, size, from int64, since time.Time) ([]usageRec, error) {
	sc := bufio.NewScanner(io.NewSectionReader(f, from, size-from))
	var out []usageRec
	for sc.Scan() {
		var ln ledgerLine
		if json.Unmarshal(sc.Bytes(), &ln) != nil || ln.Agg {
			continue
		}
		if ln.End.After(since) {
			out = append(out, ln.usageRec)
		}
	}
	return out, sc.Err()
}

// hasVM: is there a record of VM id's run that started at start?
func (l *ledger) hasVM(id string, start time.Time) bool {
	recs, err := l.read(start)
	if err != nil {
		return false
	}
	for _, r := range recs {
		if r.Kind == "vm" && r.ID == id && r.Start.Equal(start) {
			return true
		}
	}
	return false
}

func (l *ledger) reset() {
	l.marks, l.indexed, l.count, l.maxEnd, l.built = nil, 0, 0, time.Time{}, false
	l.aggs = nil
}

// scan reads records from byte from. Without out it builds the index (from 0); with
// out it collects the records ending after since, and indexes what lies past l.indexed.
func (l *ledger) scan(f *os.File, size, from int64, since time.Time, out *[]usageRec) error {
	sc := bufio.NewScanner(io.NewSectionReader(f, from, size-from))
	off := from
	for sc.Scan() {
		line := sc.Bytes()
		next := min(off+int64(len(line))+1, size) // a torn last line has no newline
		var ln ledgerLine
		ok := json.Unmarshal(line, &ln) == nil
		if ok && ln.Agg { // a compaction aggregate: not a record, loaded once
			if next > l.indexed {
				l.addAgg(ln)
				l.indexed = next
			}
			off = next
			continue
		}
		r := ln.usageRec
		if !ok && len(bytes.TrimSpace(line)) > 0 && next > l.indexed {
			log.Printf("usage: ledger: skipping an unparsable record (%d bytes at offset %d)", len(line), off)
		}
		if ok && out != nil && r.End.After(since) {
			*out = append(*out, r)
		}
		if next > l.indexed && ok {
			l.note(r, next)
		} else if next > l.indexed {
			l.indexed = next // a torn line: skipped, like before
		}
		off = next
	}
	return sc.Err()
}

type vmUsage struct {
	ID      string  `json:"id"`
	Team    string  `json:"team"`
	Size    string  `json:"size"`
	Seconds float64 `json:"vm_seconds"`
}

type snapUsage struct {
	ID   string  `json:"id"`
	Team string  `json:"team"`
	Days float64 `json:"snapshot_days"`
}

type teamUsage struct {
	Team         string             `json:"team"`
	VMSeconds    float64            `json:"vm_seconds"`
	BySize       map[string]float64 `json:"vm_seconds_by_size"`
	SnapshotDays float64            `json:"snapshot_days"`
}

type usageReport struct {
	Exact     bool          `json:"exact,omitempty"`
	Intervals []usageRec    `json:"intervals,omitempty"`
	Since     time.Time     `json:"since"`
	Until     time.Time     `json:"until"`
	VMs       []vmUsage     `json:"vms"`
	Snapshots []snapUsage   `json:"snapshots"`
	Teams     []teamUsage   `json:"teams"`
	Storage   []teamStorage `json:"storage"` // bytes now, per team
	Disk      *diskSpace    `json:"disk,omitempty"`
}

func (r usageReport) clone() usageReport {
	c := r
	c.VMs = append([]vmUsage{}, r.VMs...)
	c.Snapshots = append([]snapUsage{}, r.Snapshots...)
	c.Storage = append([]teamStorage{}, r.Storage...)
	c.Teams = make([]teamUsage, len(r.Teams))
	for i, t := range r.Teams {
		c.Teams[i] = t
		c.Teams[i].BySize = make(map[string]float64, len(t.BySize))
		for k, v := range t.BySize {
			c.Teams[i].BySize[k] = v
		}
	}
	if r.Disk != nil {
		d := *r.Disk
		c.Disk = &d
	}
	return c
}

// usageGens identifies what a Usage() report was assembled from, apart from time.
type usageGens struct {
	storage, state, ledger uint64
	vms, snaps             int
}

type usageSince struct {
	sec  int64
	nsec int
}

type usageFlight struct {
	gens usageGens
	done chan struct{}
	rep  usageReport
	err  error
}

type usageEntry struct {
	used   uint64 // access tick, for LRU eviction
	gens   usageGens
	at     time.Time
	rep    usageReport
	ready  bool
	flight *usageFlight
}

// usageCacheMax bounds the distinct `since` values kept.
const usageCacheMax = 32

// usageCache memoises assembled Usage() reports per `since` for usageCacheTTL, with
// one assembly shared by concurrent callers, and drops them when the ledger, the VMs
// or the snapshots change (usageGens). The zero value is ready to use.
type usageCache struct {
	mu      sync.Mutex
	entries map[usageSince]*usageEntry
	tick    uint64
}

// evictLRULocked drops the least recently used entry (never one being assembled
// by a caller still waiting: its flight keeps its own report).
func (c *usageCache) evictLRULocked() {
	var oldest usageSince
	var oldestUsed uint64
	found := false
	for k, e := range c.entries {
		if !found || e.used < oldestUsed {
			oldest, oldestUsed, found = k, e.used, true
		}
	}
	if found {
		delete(c.entries, oldest)
	}
}

func (c *usageCache) get(m *Manager, since time.Time) (usageReport, error) {
	gens, now := m.usageGens(), m.now()
	sk := usageSince{since.Unix(), since.Nanosecond()}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[usageSince]*usageEntry{}
	}
	e := c.entries[sk]
	if e == nil {
		if len(c.entries) >= usageCacheMax {
			c.evictLRULocked() // one entry goes, not the whole map
		}
		e = &usageEntry{}
		c.entries[sk] = e
	}
	c.tick++
	e.used = c.tick
	if e.ready && e.gens == gens {
		if d := now.Sub(e.at); d >= 0 && d < usageCacheTTL {
			rep := e.rep.clone()
			c.mu.Unlock()
			return rep, nil
		}
	}
	if f := e.flight; f != nil && f.gens == gens {
		c.mu.Unlock()
		<-f.done
		return f.rep.clone(), f.err
	}
	f := &usageFlight{gens: gens, done: make(chan struct{})}
	e.flight = f
	c.mu.Unlock()

	f.rep, f.err = m.buildUsage(since)

	c.mu.Lock()
	if e.flight == f {
		e.flight = nil
	}
	if f.err == nil {
		// gens were read before the build: a record appended meanwhile makes the
		// next call miss, never the other way round.
		e.gens, e.at, e.rep, e.ready = gens, now, f.rep, true
	}
	c.mu.Unlock()
	close(f.done)
	return f.rep.clone(), f.err
}

// dedupeVMIntervals returns recs with one record per (VM id, start), the one that
// ended first. Order is kept; recs is not modified.
func dedupeVMIntervals(recs []usageRec) []usageRec {
	type key struct {
		id        string
		sec, nsec int64
	}
	first := map[key]int{}
	drop := map[int]bool{}
	for i, r := range recs {
		if r.Kind != "vm" {
			continue
		}
		k := key{r.ID, r.Start.Unix(), int64(r.Start.Nanosecond())}
		j, seen := first[k]
		switch {
		case !seen:
			first[k] = i
		case r.End.Before(recs[j].End):
			drop[j] = true
			first[k] = i
		default:
			drop[i] = true
		}
	}
	if len(drop) == 0 {
		return recs
	}
	out := make([]usageRec, 0, len(recs)-len(drop))
	for i, r := range recs {
		if !drop[i] {
			out = append(out, r)
		}
	}
	return out
}

// summarize clips intervals (closed + still-open) to [since, until].
//
// A VM interval is billed once: records with the same (id, start) are one interval
// seen twice (a stop whose vm.json never became durable, so the restart closed the
// same run again; or the open interval of a VM next to its closed record). The
// earliest End is the real stop.
func summarize(recs []usageRec, since, until time.Time) usageReport {
	recs = dedupeVMIntervals(recs)
	rep := usageReport{Since: since, Until: until, VMs: []vmUsage{}, Snapshots: []snapUsage{}, Teams: []teamUsage{}}
	vmIdx, snIdx, tIdx := map[string]int{}, map[string]int{}, map[string]int{}
	team := func(t string) *teamUsage {
		i, ok := tIdx[t]
		if !ok {
			i = len(rep.Teams)
			tIdx[t] = i
			rep.Teams = append(rep.Teams, teamUsage{Team: t, BySize: map[string]float64{}})
		}
		return &rep.Teams[i]
	}
	for _, r := range recs {
		s, e := r.Start, r.End
		if s.Before(since) {
			s = since
		}
		if e.After(until) {
			e = until
		}
		d := e.Sub(s).Seconds()
		if d <= 0 {
			continue
		}
		t := team(r.Team)
		if r.Kind == "snapshot" {
			i, ok := snIdx[r.ID]
			if !ok {
				i = len(rep.Snapshots)
				snIdx[r.ID] = i
				rep.Snapshots = append(rep.Snapshots, snapUsage{ID: r.ID, Team: r.Team})
			}
			rep.Snapshots[i].Days += d / 86400
			t.SnapshotDays += d / 86400
			continue
		}
		i, ok := vmIdx[r.ID]
		if !ok {
			i = len(rep.VMs)
			vmIdx[r.ID] = i
			rep.VMs = append(rep.VMs, vmUsage{ID: r.ID, Team: r.Team, Size: r.Size})
		}
		rep.VMs[i].Seconds += d
		t.VMSeconds += d
		t.BySize[r.Size] += d
	}
	return rep
}

// vmSnapshot pins bytes without building the aggregate index under m.mu. The
// exact path scans from zero to deduplicate runs, so it needs no sparse index.
// shortcut: scan retained history per VM query; add a VM index if polling cost requires it.
func (l *ledger) vmSnapshot() (*os.File, int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads.Add(1)
	l.repairTailLocked()
	f, err := os.Open(l.path)
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

// VMUsageWindow bypasses the rounded fleet cache. Pin state and ledger together so
// a concurrent stop cannot disappear between the closed and still-open intervals.
func (m *Manager) VMUsageWindow(team, id string, since, until time.Time) (usageReport, error) {
	m.mu.Lock()
	v, ok := m.vms[id]
	if !ok || v.PendingCopy || v.Team != team {
		m.mu.Unlock()
		return usageReport{}, errNotFound
	}
	if v.State == Running && (v.StartedAt == nil || v.Size == "") {
		m.mu.Unlock()
		return usageReport{}, errf(501, "exact per-VM usage is unavailable for a legacy run without start or size metadata")
	}
	now := m.now()
	if until.IsZero() || until.After(now) {
		until = now
	}
	from := since
	if from.Before(v.CreatedAt) {
		from = v.CreatedAt
	}
	// Compaction has no VM identity; older daily totals cannot answer this query.
	if retention := m.cfg.usageRetention(); retention > 0 && from.Before(now.Add(-retention).UTC().Truncate(aggDay)) {
		m.mu.Unlock()
		return usageReport{}, errf(501, "exact per-VM usage is unavailable before the ledger retention cutoff")
	}
	// Read retained runs from the beginning: an earlier stop can invalidate a later
	// duplicate even when that true stop precedes since.
	f, size, err := m.usage.vmSnapshot()
	m.mu.Unlock()
	if err != nil {
		return usageReport{}, err
	}
	var recs []usageRec
	if f != nil {
		defer f.Close()
		sc := bufio.NewScanner(io.NewSectionReader(f, 0, size))
		for sc.Scan() {
			var ln ledgerLine
			if json.Unmarshal(sc.Bytes(), &ln) != nil {
				return usageReport{}, errf(501, "exact per-VM usage is unavailable for an incomplete ledger")
			}
			if ln.Agg {
				if ln.Kind == "vm" && ln.Team == team && ln.Nanos > 0 && ln.Day.Before(until) && ln.Day.Add(aggDay).After(from) {
					return usageReport{}, errf(501, "exact per-VM usage is unavailable for compacted history")
				}
				continue
			}
			if ln.Kind == "vm" && ln.ID == id && ln.Team == team {
				if ln.Start.IsZero() || ln.End.IsZero() || ln.Size == "" || ln.End.Before(ln.Start) {
					return usageReport{}, errf(501, "exact per-VM usage is unavailable for incomplete legacy intervals")
				}
				recs = append(recs, ln.usageRec)
			}
		}
		if err := sc.Err(); err != nil {
			return usageReport{}, err
		}
	}
	if v.State == Running && v.StartedAt != nil {
		recs = append(recs, usageRec{"vm", id, team, v.Size, *v.StartedAt, now})
	}
	var clipped []usageRec
	for _, r := range dedupeVMIntervals(recs) {
		if r.Start.Before(from) {
			r.Start = from
		}
		if r.End.After(until) {
			r.End = until
		}
		if r.End.After(r.Start) {
			clipped = append(clipped, r)
		}
	}
	rep := summarize(clipped, from, until)
	rep.Exact, rep.Intervals = true, clipped
	return rep, nil
}
