package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"time"
)

// Ledger compaction: usage.jsonl is append-only, so without it it grows forever and a
// query without `since` parses all of it. compact folds the intervals that ended
// before the retention cutoff into per-(kind, team, size, UTC day) aggregates, kept as
// `{"agg":true,...}` lines in the same file, and drops those records.
//
//   - A day bucket holds the nanoseconds of the intervals' overlap with that day, so
//     totals are exact for any `since` on a UTC day boundary (billing periods) and, for
//     a `since` inside a compacted day, prorated over that one day.
//   - The cutoff is a UTC midnight, and only intervals ending at or before it are
//     folded, so no day bucket is ever split between an aggregate and a record.
//   - Duplicate (VM id, start) records (see dedupeVMIntervals) are collapsed first, so
//     compaction never changes what summarize would have billed.
//   - Aggregates are kept for ledgerAggRetention (5 years) and then pruned by the
//     same rewrite, so the file stops growing without bound. An aggregate goes only
//     when its whole day ended at or before the prune cutoff (a UTC midnight, never
//     later than the record cutoff), so any `since` at or after the cutoff gets exact
//     totals. A `since` before it (or none) counts from the cutoff on: the API and
//     the compat readers pass whatever the caller asked for, and billing windows are
//     months, so only an all-time query is shortened.
//   - Per-VM and per-snapshot rows of a report cover the records still in the file; the
//     per-team totals cover everything.

// ledgerDefaultRetention is how long intervals stay individually addressable.
const ledgerDefaultRetention = 400 * 24 * time.Hour

// ledgerAggRetention is how long a daily aggregate is kept. Raising the configured
// record retention past it keeps aggregates at least that long (see aggCutoff).
const ledgerAggRetention = 5 * 365 * 24 * time.Hour

// ledgerCompactEvery is how often boatd looks for records past the retention window.
const ledgerCompactEvery = 24 * time.Hour

const aggDay = 24 * time.Hour

// ledgerCatchUpBytes: when the tail written during the rewrite is no larger than this,
// it is copied under the ledger lock; a larger one is first copied without it.
const ledgerCatchUpBytes = 64 << 10

// errCompactCrash is what a test hook returns to stop a compaction where a crash would.
var errCompactCrash = errors.New("simulated crash mid-compaction")

type aggKey struct {
	Kind, Team, Size string
	Day              int64 // unix seconds of the UTC midnight
}

// aggRow is one aggregate: Nanos of Kind time (VM running / snapshot kept) in Day.
type aggRow struct {
	aggKey
	Nanos int64
}

// ledgerLine is any line of usage.jsonl: a usageRec, or an aggregate (Agg set).
type ledgerLine struct {
	usageRec
	Agg   bool      `json:"agg,omitempty"`
	Day   time.Time `json:"day,omitempty"`
	Nanos int64     `json:"nanos,omitempty"`
}

// aggLine is an aggregate as written to the file.
type aggLine struct {
	Agg   bool      `json:"agg"`
	Kind  string    `json:"kind"`
	Team  string    `json:"team"`
	Size  string    `json:"size,omitempty"`
	Day   time.Time `json:"day"`
	Nanos int64     `json:"nanos"`
}

func (l *ledger) addAgg(ln ledgerLine) {
	if l.aggs == nil {
		l.aggs = map[aggKey]int64{}
	}
	l.aggs[aggKey{ln.Kind, ln.Team, ln.Size, ln.Day.Unix()}] += ln.Nanos
}

// aggRowsLocked returns the aggregates of days that end after since.
func (l *ledger) aggRowsLocked(since time.Time) []aggRow {
	var out []aggRow
	for k, n := range l.aggs {
		if time.Unix(k.Day, 0).Add(aggDay).After(since) {
			out = append(out, aggRow{k, n})
		}
	}
	sort.Slice(out, func(i, j int) bool { return lessAggKey(out[i].aggKey, out[j].aggKey) })
	return out
}

func lessAggKey(a, b aggKey) bool {
	if a.Day != b.Day {
		return a.Day < b.Day
	}
	if a.Team != b.Team {
		return a.Team < b.Team
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	return a.Size < b.Size
}

// applyAggs adds the aggregates to the team totals of rep. A day that starts before
// since counts in proportion to the part of it after since.
func applyAggs(rep *usageReport, rows []aggRow, since time.Time) {
	if len(rows) == 0 {
		return
	}
	idx := map[string]int{}
	for i, t := range rep.Teams {
		idx[t.Team] = i
	}
	for _, r := range rows {
		day := time.Unix(r.Day, 0)
		n := float64(r.Nanos)
		if day.Before(since) {
			n *= float64(day.Add(aggDay).Sub(since)) / float64(aggDay)
		}
		sec := n / 1e9
		if sec <= 0 {
			continue
		}
		i, ok := idx[r.Team]
		if !ok {
			i = len(rep.Teams)
			idx[r.Team] = i
			rep.Teams = append(rep.Teams, teamUsage{Team: r.Team, BySize: map[string]float64{}})
		}
		t := &rep.Teams[i]
		if r.Kind == "snapshot" {
			t.SnapshotDays += sec / 86400
		} else {
			t.VMSeconds += sec
			t.BySize[r.Size] += sec
		}
	}
	sort.Slice(rep.Teams, func(i, j int) bool { return rep.Teams[i].Team < rep.Teams[j].Team })
}

// foldInterval adds the overlap of [start, end) with each UTC day to aggs.
func foldInterval(aggs map[aggKey]int64, r usageRec) {
	for d := r.Start.UTC().Truncate(aggDay); d.Before(r.End); d = d.Add(aggDay) {
		s, e := r.Start, r.End
		if s.Before(d) {
			s = d
		}
		if end := d.Add(aggDay); e.After(end) {
			e = end
		}
		if n := e.Sub(s); n > 0 {
			aggs[aggKey{r.Kind, r.Team, r.Size, d.Unix()}] += int64(n)
		}
	}
}

type vmKey struct {
	id        string
	sec, nsec int64
}

type vmSeen struct {
	end  time.Time
	kept bool
}

// compact folds the records that ended at or before the retention cutoff into
// aggregates and returns how many it folded. The file is rewritten to a temp file,
// fsynced and renamed over usage.jsonl; the original is untouched until the rename,
// so a crash at any point leaves a valid ledger (a stale temp file is ignored and
// overwritten by the next compaction). The ledger lock is held only to pin the file
// length at the start and to copy what was appended meanwhile at the end, not across
// the rewrite.
func (l *ledger) compact(now time.Time, retention time.Duration) (folded int, err error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := now.Add(-retention).UTC().Truncate(aggDay)
	aggCutoff := l.aggCutoff(now, retention)
	l.compactMu.Lock()
	defer l.compactMu.Unlock()

	l.mu.Lock()
	l.repairTailLocked()
	st, err := os.Stat(l.path)
	size := int64(0)
	if err == nil {
		size = st.Size()
	}
	l.mu.Unlock()
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	src, err := os.Open(l.path)
	if err != nil {
		return 0, err
	}
	defer src.Close()

	seen, old, err := l.compactSurvey(src, size, cutoff, aggCutoff)
	if err != nil || old == 0 {
		return 0, err
	}
	tmpPath := l.path + ".compact"
	tmp, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	crashed := false
	defer func() {
		tmp.Close()
		if err != nil && !crashed {
			os.Remove(tmpPath)
		}
	}()
	abort := func(stage string) error {
		if l.compactHook == nil {
			return nil
		}
		if e := l.compactHook(stage); e != nil {
			crashed = true
			return e
		}
		return nil
	}

	var idx *ledger // the new file's index, built as it is written (see installIndexLocked)
	if folded, idx, err = l.compactRewrite(src, tmp, size, cutoff, aggCutoff, seen); err != nil {
		return 0, err
	}
	idxOK := true
	if err = tmp.Sync(); err != nil {
		return 0, err
	}
	if err = abort("rewritten"); err != nil {
		return 0, err
	}
	// Records appended during the rewrite: copy the bulk without the lock, the rest
	// (and the rename) under it.
	copied := size
	if cur, e := fileSize(l.path); e == nil && cur-copied > ledgerCatchUpBytes {
		if err = copyRange(tmp, src, copied, cur); err != nil {
			return 0, err
		}
		if err = tmp.Sync(); err != nil {
			return 0, err
		}
		idxOK = idx.indexTail(src, copied, cur)
		copied = cur
	}
	if err = abort("caught-up"); err != nil {
		return 0, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cur, err := fileSize(l.path)
	if err != nil {
		return 0, err
	}
	if cur < copied {
		return 0, fmt.Errorf("usage: ledger shrank during compaction (%d < %d bytes)", cur, copied)
	}
	if cur > copied {
		if err = copyRange(tmp, src, copied, cur); err != nil {
			return 0, err
		}
		if err = tmp.Sync(); err != nil {
			return 0, err
		}
		idxOK = idxOK && idx.indexTail(src, copied, cur)
	}
	if err = abort("synced"); err != nil {
		return 0, err
	}
	if err = os.Rename(tmpPath, l.path); err != nil {
		return 0, err
	}
	if e := syncDir(dirOf(l.path)); e != nil {
		log.Printf("usage: ledger compaction: fsync of the directory: %v", e)
	}
	l.installIndexLocked(idx, idxOK)
	l.dirty = false
	l.gen.Add(1)
	return folded, nil
}

// buildIndex builds the index without holding l.mu across the scan, so a restart
// with a large ledger does not make appends (and the lifecycle ops above them) wait
// for the first usage read. It pins the size under the lock, scans [0, size) into a
// private index with no lock held, then installs it under the lock after scanning
// the small tail appended meanwhile. It installs nothing if a read or a compaction
// built the index first, or if the file was replaced or shortened; totals stay
// exact either way, since a read then builds as before.
func (l *ledger) buildIndex() {
	l.mu.Lock()
	if l.built {
		l.mu.Unlock()
		return
	}
	l.repairTailLocked()
	f, err := os.Open(l.path)
	if err != nil {
		l.mu.Unlock()
		return
	}
	defer f.Close()
	st, err := f.Stat()
	l.mu.Unlock()
	if err != nil {
		return
	}
	idx := &ledger{path: l.path, buildHook: l.buildHook}
	if idx.buildHook != nil {
		idx.buildHook()
	}
	if err := idx.scan(f, st.Size(), 0, time.Time{}, nil); err != nil {
		log.Printf("usage: ledger: startup index build: %v", err)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cur, err := os.Stat(l.path)
	if l.built || err != nil || !os.SameFile(st, cur) || cur.Size() < st.Size() {
		return
	}
	if idx.indexed < cur.Size() && idx.scan(f, cur.Size(), idx.indexed, time.Time{}, nil) != nil {
		return
	}
	l.installIndexLocked(idx, true)
	l.offBuilds.Add(1)
}

// aggCutoff is the UTC midnight at or before which whole-day aggregates are pruned:
// ledgerAggRetention (or the ledger's override) back from now, and never later than
// the record cutoff, so a day folded now is not pruned in the same pass.
func (l *ledger) aggCutoff(now time.Time, retention time.Duration) time.Time {
	keep := l.aggRetention
	if keep <= 0 {
		keep = ledgerAggRetention
	}
	if keep < retention {
		keep = retention
	}
	return now.Add(-keep).UTC().Truncate(aggDay)
}

// pruned: is the aggregate's whole day at or before the prune cutoff?
func pruned(day time.Time, aggCutoff time.Time) bool {
	return !day.Add(aggDay).After(aggCutoff)
}

// installIndexLocked replaces the index with idx, built during the rewrite from the
// bytes of the new file, so the first read after the rename scans nothing under l.mu.
// A tail it could not index (a torn or unparsable line) drops the index instead: the
// next read rebuilds it, as before.
func (l *ledger) installIndexLocked(idx *ledger, ok bool) {
	l.reset()
	if !ok {
		return
	}
	l.marks, l.indexed, l.count, l.maxEnd, l.aggs = idx.marks, idx.indexed, idx.count, idx.maxEnd, idx.aggs
	l.built = true
}

// indexTail adds the records in src[from:to), appended during the rewrite and copied
// verbatim to the end of the new file, to the index. It reports false if they are not
// whole, parsable records, which only a write behind boatd's back can cause.
func (l *ledger) indexTail(src *os.File, from, to int64) bool {
	br := bufio.NewReader(io.NewSectionReader(src, from, to-from))
	for {
		line, err := br.ReadBytes('\n')
		if err != nil && (err != io.EOF || len(line) > 0) {
			return false // a torn last line, or a read error
		}
		if err == io.EOF {
			return true
		}
		var ln ledgerLine
		if json.Unmarshal(line, &ln) != nil || ln.Agg {
			return false
		}
		l.note(ln.usageRec, l.indexed+int64(len(line)))
	}
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			if i == 0 {
				return "/"
			}
			return p[:i]
		}
	}
	return "."
}

func fileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// copyRange appends src[from:to) to dst.
func copyRange(dst io.Writer, src *os.File, from, to int64) error {
	_, err := io.Copy(dst, io.NewSectionReader(src, from, to-from))
	return err
}

// compactSurvey is the first pass over the first size bytes: for each (VM id, start)
// the earliest End (the real stop), and how many records are foldable or aggregates
// prunable (anything that makes the rewrite worth doing).
func (l *ledger) compactSurvey(src *os.File, size int64, cutoff, aggCutoff time.Time) (map[vmKey]*vmSeen, int, error) {
	seen := map[vmKey]*vmSeen{}
	old := 0
	sc := bufio.NewScanner(io.NewSectionReader(src, 0, size))
	for sc.Scan() {
		var ln ledgerLine
		if json.Unmarshal(sc.Bytes(), &ln) != nil {
			continue
		}
		if ln.Agg {
			if pruned(ln.Day, aggCutoff) {
				old++
			}
			continue
		}
		if !ln.End.After(cutoff) {
			old++
		}
		if ln.Kind != "vm" {
			continue
		}
		k := vmKey{ln.ID, ln.Start.Unix(), int64(ln.Start.Nanosecond())}
		if s := seen[k]; s == nil {
			seen[k] = &vmSeen{end: ln.End}
		} else if ln.End.Before(s.end) {
			s.end = ln.End
		}
	}
	return seen, old, sc.Err()
}

// compactRewrite is the second pass: records ending after cutoff are copied, the
// others folded; aggregates (old and new, minus the pruned ones) follow the records. It returns the number
// of records folded, and the index of what it wrote (marks, counts, aggregates), so the
// caller can install it instead of rescanning the new file.
func (l *ledger) compactRewrite(src *os.File, dst io.Writer, size int64, cutoff, aggCutoff time.Time, seen map[vmKey]*vmSeen) (int, *ledger, error) {
	w := bufio.NewWriter(dst)
	idx := &ledger{aggs: map[aggKey]int64{}}
	aggs := map[aggKey]int64{}
	folded, dropped := 0, 0
	sc := bufio.NewScanner(io.NewSectionReader(src, 0, size))
	for sc.Scan() {
		line := sc.Bytes()
		var ln ledgerLine
		if err := json.Unmarshal(line, &ln); err != nil {
			if len(line) > 0 {
				dropped++
			}
			continue
		}
		if ln.Agg {
			if !pruned(ln.Day, aggCutoff) {
				aggs[aggKey{ln.Kind, ln.Team, ln.Size, ln.Day.Unix()}] += ln.Nanos
			}
			continue
		}
		if ln.Kind == "vm" { // one record per (id, start): the earliest End, the first of those
			s := seen[vmKey{ln.ID, ln.Start.Unix(), int64(ln.Start.Nanosecond())}]
			if s.kept || !ln.End.Equal(s.end) {
				continue
			}
			s.kept = true
		}
		if !ln.End.After(cutoff) {
			foldInterval(aggs, ln.usageRec)
			folded++
			continue
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return 0, nil, err
		}
		idx.note(ln.usageRec, idx.indexed+int64(len(line))+1)
	}
	if err := sc.Err(); err != nil {
		return 0, nil, err
	}
	if dropped > 0 {
		log.Printf("usage: ledger compaction: dropped %d unparsable lines", dropped)
	}
	rows := make([]aggRow, 0, len(aggs))
	for k, n := range aggs {
		if !pruned(time.Unix(k.Day, 0), aggCutoff) { // also the days a folded interval covers
			rows = append(rows, aggRow{k, n})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return lessAggKey(rows[i].aggKey, rows[j].aggKey) })
	for _, r := range rows {
		b, _ := json.Marshal(aggLine{true, r.Kind, r.Team, r.Size, time.Unix(r.Day, 0).UTC(), r.Nanos})
		if _, err := w.Write(append(b, '\n')); err != nil {
			return 0, nil, err
		}
		idx.indexed += int64(len(b)) + 1
		idx.aggs[r.aggKey] = r.Nanos
	}
	if err := w.Flush(); err != nil {
		return 0, nil, err
	}
	return folded, idx, nil
}

// compactUsage folds the ledger's records past the retention window; boatd calls it
// at start and then daily.
func (m *Manager) compactUsage() {
	if m.usage == nil || m.cfg.usageRetention() <= 0 {
		return
	}
	n, err := m.usage.compact(m.now(), m.cfg.usageRetention())
	if err != nil {
		log.Printf("usage: ledger compaction: %v", err)
		return
	}
	if n > 0 {
		log.Printf("usage: ledger compaction: folded %d records older than %s into daily aggregates", n, m.cfg.usageRetention())
	}
}
