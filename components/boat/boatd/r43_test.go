package main

import (
	"bytes"
	"errors"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"
)

var r43Now = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

const r43Retention = 400 * 24 * time.Hour

// r43Cutoff is the UTC midnight compaction folds up to at r43Now.
func r43Cutoff() time.Time { return r43Now.Add(-r43Retention).Truncate(24 * time.Hour) }

// r43Recs: whole-second intervals over ~2 years before r43Now, three teams, two sizes,
// VMs and snapshots, some crossing midnights, the cutoff, and a few duplicates.
func r43Recs(n int) []usageRec {
	rng := rand.New(rand.NewSource(43))
	teams := []string{"ta", "tb", "tc"}
	sizes := []string{"small", "big"}
	var out []usageRec
	for i := 0; i < n; i++ {
		start := r43Now.Add(-time.Duration(rng.Intn(730*24*3600)) * time.Second)
		dur := time.Duration(1+rng.Intn(3*24*3600)) * time.Second // up to 3 days: crosses midnights
		r := usageRec{"vm", "vm-" + string(rune('a'+i%26)) + string(rune('a'+i/26%26)), teams[rng.Intn(3)], sizes[rng.Intn(2)], start, start.Add(dur)}
		if i%5 == 0 {
			r.Kind = "snapshot"
			r.ID = "snap-" + r.ID
		}
		if r.End.After(r43Now) {
			r.End = r43Now
		}
		out = append(out, r)
		if i%17 == 0 && r.Kind == "vm" { // the same run closed again by a later restart
			dup := r
			dup.End = r.End.Add(time.Hour)
			out = append(out, dup)
		}
	}
	// One that straddles the cutoff midnight: it ends after it and stays a record.
	cut := r43Cutoff()
	out = append(out, usageRec{"vm", "vm-straddle", "ta", "small", cut.Add(-5 * time.Hour), cut.Add(7 * time.Hour)})
	return out
}

func r43Ledger(t *testing.T, recs []usageRec) *ledger {
	t.Helper()
	l := &ledger{path: filepath.Join(t.TempDir(), "usage.jsonl")}
	for _, r := range recs {
		if err := l.append(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.flush(); err != nil {
		t.Fatal(err)
	}
	return l
}

// r43Report is what Manager.buildUsage computes from the ledger.
func r43Report(t *testing.T, l *ledger, since time.Time) usageReport {
	t.Helper()
	recs, aggs, err := l.readAll(since)
	if err != nil {
		t.Fatal(err)
	}
	rep := summarize(recs, since, r43Now)
	applyAggs(&rep, aggs, since)
	return rep
}

func r43Teams(rep usageReport) []teamUsage {
	out := append([]teamUsage{}, rep.Teams...)
	sort.Slice(out, func(i, j int) bool { return out[i].Team < out[j].Team })
	return out
}

func r43Near(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

// r43SameTeams: equal per-team totals (up to float summation order).
func r43SameTeams(t *testing.T, what string, got, want usageReport) {
	t.Helper()
	g, w := r43Teams(got), r43Teams(want)
	ok := len(g) == len(w)
	for i := 0; ok && i < len(g); i++ {
		ok = g[i].Team == w[i].Team && r43Near(g[i].VMSeconds, w[i].VMSeconds) &&
			r43Near(g[i].SnapshotDays, w[i].SnapshotDays) && len(g[i].BySize) == len(w[i].BySize)
		for k, v := range w[i].BySize {
			ok = ok && r43Near(g[i].BySize[k], v)
		}
	}
	if !ok {
		t.Fatalf("%s: team totals differ\n got %+v\nwant %+v", what, g, w)
	}
}

// A dense ledger (many intervals per day, as in production) shrinks, not just stops growing.
func TestR43DenseLedgerShrinks(t *testing.T) {
	var recs []usageRec
	cut := r43Cutoff()
	for i := 0; i < 4000; i++ {
		s := cut.Add(-time.Duration(i) * 20 * time.Minute)
		recs = append(recs, usageRec{"vm", "vm-" + strconv.Itoa(i), "t" + strconv.Itoa(i%3), "small", s.Add(-10 * time.Minute), s})
	}
	l := r43Ledger(t, recs)
	before, _ := fileSize(l.path)
	want := r43Report(t, l, time.Time{})
	if n, err := l.compact(r43Now, r43Retention); err != nil || n != 4000 {
		t.Fatalf("folded %d: %v", n, err)
	}
	after, _ := fileSize(l.path)
	if after*5 > before {
		t.Fatalf("ledger %d -> %d bytes: want at least 5x smaller", before, after)
	}
	r43SameTeams(t, "dense", r43Report(t, l, time.Time{}), want)
}

func r43Sinces() map[string]time.Time {
	cut := r43Cutoff()
	return map[string]time.Time{
		"none":             {},
		"day-before-cut":   cut.Add(-24 * time.Hour),
		"three-days-pre":   cut.Add(-72 * time.Hour),
		"cut":              cut,
		"after-cut":        cut.Add(3 * time.Hour),
		"recent":           r43Now.Add(-48 * time.Hour),
		"long-ago-aligned": time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// Billing totals are identical before and after compaction, for windows before, at
// and after the cutoff.
func TestR43TotalsIdenticalAfterCompaction(t *testing.T) {
	recs := r43Recs(600)
	l := r43Ledger(t, recs)
	before := map[string]usageReport{}
	for k, since := range r43Sinces() {
		before[k] = r43Report(t, l, since)
	}
	folded, err := l.compact(r43Now, r43Retention)
	if err != nil {
		t.Fatal(err)
	}
	if folded == 0 {
		t.Fatal("nothing folded")
	}
	for k, since := range r43Sinces() {
		r43SameTeams(t, k, r43Report(t, l, since), before[k])
	}
	// Per-VM rows after the cutoff are untouched.
	cut := r43Cutoff().Add(24 * time.Hour)
	a, b := r43Report(t, l, cut), summarize(recs, cut, r43Now)
	if !reflect.DeepEqual(a.VMs, b.VMs) || !reflect.DeepEqual(a.Snapshots, b.Snapshots) {
		t.Fatal("per-VM / per-snapshot rows after the cutoff changed")
	}
	// A second pass finds nothing and leaves the file alone; a later one merges.
	raw, _ := os.ReadFile(l.path)
	if n, err := l.compact(r43Now, r43Retention); err != nil || n != 0 {
		t.Fatalf("second compaction: %d %v", n, err)
	}
	if raw2, _ := os.ReadFile(l.path); !bytes.Equal(raw, raw2) {
		t.Fatal("a no-op compaction rewrote the file")
	}
	if _, err := l.compact(r43Now.Add(40*24*time.Hour), r43Retention); err != nil {
		t.Fatal(err)
	}
	cut2 := r43Now.Add(40 * 24 * time.Hour).Add(-r43Retention).Truncate(24 * time.Hour)
	for k, since := range r43Sinces() {
		if since.After(cut2) || since.Equal(since.Truncate(24*time.Hour)) { // else: a `since` inside a compacted day is pro rata
			r43SameTeams(t, k+"/second", r43Report(t, l, since), before[k])
		}
	}
}

// A crash at any point of a compaction leaves the old ledger intact and usable; the
// stale temp file is overwritten by the next compaction.
func TestR43CrashMidCompactionLeavesValidLedger(t *testing.T) {
	for _, stage := range []string{"rewritten", "synced"} {
		t.Run(stage, func(t *testing.T) {
			l := r43Ledger(t, r43Recs(300))
			want := r43Report(t, l, time.Time{})
			orig, _ := os.ReadFile(l.path)
			l.compactHook = func(s string) error {
				if s == stage {
					return errCompactCrash
				}
				return nil
			}
			if _, err := l.compact(r43Now, r43Retention); !errors.Is(err, errCompactCrash) {
				t.Fatalf("compact: %v", err)
			}
			if _, err := os.Stat(l.path + ".compact"); err != nil {
				t.Fatalf("the crash should leave its temp file: %v", err)
			}
			if got, _ := os.ReadFile(l.path); !bytes.Equal(got, orig) {
				t.Fatal("ledger changed before the rename")
			}
			// "Restart": a fresh ledger on the same file reads the same totals.
			fresh := &ledger{path: l.path}
			r43SameTeams(t, "after crash", r43Report(t, fresh, time.Time{}), want)
			if err := fresh.append(usageRec{"vm", "vm-new", "ta", "small", r43Now.Add(-time.Hour), r43Now}); err != nil {
				t.Fatal(err)
			}
			// The next compaction succeeds over the stale temp file.
			if n, err := fresh.compact(r43Now, r43Retention); err != nil || n == 0 {
				t.Fatalf("recovery compaction: %d %v", n, err)
			}
			if _, err := os.Stat(l.path + ".compact"); !os.IsNotExist(err) {
				t.Fatalf("temp file left behind: %v", err)
			}
			got, wantTeams := r43Teams(r43Report(t, fresh, time.Time{})), r43Teams(want)
			if got[0].Team != "ta" || !r43Near(got[0].VMSeconds, wantTeams[0].VMSeconds+3600) {
				t.Fatalf("totals after recovery: %+v vs %+v", got[0], wantTeams[0])
			}
		})
	}
}

// Records appended while the rewrite runs (small and large) survive the rename.
func TestR43AppendsDuringCompactionAreKept(t *testing.T) {
	l := r43Ledger(t, r43Recs(300))
	extra := func(i int) usageRec {
		s := r43Now.Add(-time.Duration(i+1) * time.Minute)
		return usageRec{"vm", "vm-late-" + string(rune('a'+i%26)) + string(rune('a'+i/26%26)), "tz", "small", s, s.Add(30 * time.Second)}
	}
	const small, large = 3, 1500 // large exceeds ledgerCatchUpBytes
	next := 0
	add := func(n int) {
		for i := 0; i < n; i++ {
			if err := l.append(extra(next)); err != nil {
				t.Error(err)
			}
			next++
		}
	}
	l.compactHook = func(stage string) error {
		switch stage {
		case "rewritten":
			add(large)
		case "caught-up":
			add(small)
		}
		return nil
	}
	if _, err := l.compact(r43Now, r43Retention); err != nil {
		t.Fatal(err)
	}
	l.compactHook = nil
	rep := r43Report(t, l, r43Now.Add(-48*time.Hour))
	var tz *teamUsage
	for i := range rep.Teams {
		if rep.Teams[i].Team == "tz" {
			tz = &rep.Teams[i]
		}
	}
	if tz == nil || tz.VMSeconds != float64(30*(small+large)) {
		t.Fatalf("appended records lost: %+v", tz)
	}
}

// A window that starts before the cutoff mixes aggregates and records correctly;
// one that starts on a UTC midnight is exact.
func TestR43WindowedQueryAcrossCutoff(t *testing.T) {
	cut := r43Cutoff()
	recs := []usageRec{
		{"vm", "vm-old", "ta", "small", cut.Add(-30 * time.Hour), cut.Add(-20 * time.Hour)},       // 10h, wholly before
		{"vm", "vm-edge", "ta", "small", cut.Add(-3 * time.Hour), cut},                            // ends at the cutoff: folded
		{"vm", "vm-over", "ta", "small", cut.Add(-2 * time.Hour), cut.Add(4 * time.Hour)},         // crosses it: stays
		{"vm", "vm-new", "tb", "big", cut.Add(48 * time.Hour), cut.Add(50 * time.Hour)},           // after
		{"snapshot", "snap-1", "ta", "small", cut.Add(-48 * time.Hour), cut.Add(-24 * time.Hour)}, // exactly 1 day
	}
	l := r43Ledger(t, recs)
	if n, err := l.compact(r43Now, r43Retention); err != nil || n != 3 {
		t.Fatalf("folded %d (want 3: old, edge, snap): %v", n, err)
	}
	h := func(d time.Duration) float64 { return d.Seconds() }
	teams := func(since time.Time) map[string]teamUsage {
		out := map[string]teamUsage{}
		for _, tu := range r43Report(t, l, since).Teams {
			out[tu.Team] = tu
		}
		return out
	}
	// From the cutoff: only the straddling record (4h of it) and the new one.
	if g := teams(cut); g["ta"].VMSeconds != h(4*time.Hour) || g["tb"].VMSeconds != h(2*time.Hour) {
		t.Fatalf("from cutoff: %+v", g)
	}
	// From the midnight before: adds vm-edge's 3h on that day (all of it) and the
	// part of vm-old and the snapshot on it.
	since := cut.Add(-24 * time.Hour)
	g := teams(since)
	// vm-old: cut-30h..cut-20h; the part on [cut-24h, cut) is cut-24h..cut-20h = 4h.
	// vm-edge: 3h. vm-over: cut-2h..cut+4h = 6h.
	if got, want := g["ta"].VMSeconds, h(4*time.Hour+3*time.Hour+6*time.Hour); got != want {
		t.Fatalf("window across cutoff: ta vm_seconds %v, want %v", got, want)
	}
	if got := g["ta"].SnapshotDays; got != 0 { // the snapshot ended exactly at since
		t.Fatalf("snapshot days %v", got)
	}
	// From long ago everything is there: snapshot 1 day, VM 10h+3h+6h.
	g = teams(time.Time{})
	if g["ta"].SnapshotDays != 1 || g["ta"].VMSeconds != h(19*time.Hour) || g["ta"].BySize["small"] != h(19*time.Hour) {
		t.Fatalf("all time: %+v", g["ta"])
	}
	// A since inside a compacted day counts that day pro rata (documented).
	g = teams(cut.Add(-24*time.Hour - 12*time.Hour))
	if g["ta"].VMSeconds <= h(9*time.Hour) || g["ta"].VMSeconds >= h(19*time.Hour) {
		t.Fatalf("mid-day since: %+v", g["ta"])
	}
}

// After compaction the index is dropped and rebuilt from the new file: marks, record
// count and the since-tail read all describe it, and appends go on indexing.
func TestR43IndexRebuiltAfterCompaction(t *testing.T) {
	recs := r43Recs(2500)
	l := r43Ledger(t, recs)
	if _, err := l.read(time.Time{}); err != nil {
		t.Fatal(err)
	}
	oldCount, oldMarks := l.count, len(l.marks)
	if oldMarks == 0 {
		t.Fatal("test needs a multi-mark ledger")
	}
	folded, err := l.compact(r43Now, r43Retention)
	if err != nil || folded == 0 {
		t.Fatalf("compact: %d %v", folded, err)
	}
	// r44: the index is installed at the rename, built while the file was written.
	l.mu.Lock()
	installed := l.built && l.count > 0 && len(l.marks) > 0 && l.aggs != nil
	l.mu.Unlock()
	if !installed {
		t.Fatal("the index should be valid right after the rename")
	}
	all, err := l.read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := fileSize(l.path)
	l.mu.Lock()
	built, count, marks, indexed, aggs := l.built, l.count, len(l.marks), l.indexed, len(l.aggs)
	l.mu.Unlock()
	if !built || count != len(all) || indexed != st || aggs == 0 {
		t.Fatalf("index not rebuilt: built=%v count=%d (read %d) indexed=%d (size %d) aggs=%d", built, count, len(all), indexed, st, aggs)
	}
	if count >= oldCount || marks > oldMarks {
		t.Fatalf("index did not shrink: count %d->%d marks %d->%d", oldCount, count, oldMarks, marks)
	}
	// Appending keeps the (rebuilt) index in step, and a since query reads the tail.
	n := l.reads.Load()
	late := usageRec{"vm", "vm-after", "ta", "small", r43Now.Add(-time.Hour), r43Now}
	if err := l.append(late); err != nil {
		t.Fatal(err)
	}
	got, err := l.read(r43Now.Add(-2 * time.Hour))
	if err != nil || l.reads.Load() != n+1 {
		t.Fatalf("read: %v", err)
	}
	found := false
	for _, r := range got {
		found = found || r.ID == "vm-after"
	}
	if !found || len(got) > 2*ledgerMarkEvery {
		t.Fatalf("tail read: found=%v n=%d", found, len(got))
	}
	// And a restart (fresh ledger) rebuilds the same aggregates from the file.
	fresh := &ledger{path: l.path}
	if _, err := fresh.read(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.aggs, l.aggs) {
		t.Fatal("aggregates differ after a restart")
	}
}

// Through the Manager: Usage() over a compacted ledger equals the uncompacted one, and
// the retention comes from the config (negative = never compact).
func TestR43ManagerUsageAfterCompaction(t *testing.T) {
	m, _ := newTestManager(t, func(c *Config) { c.UsageRetentionDays = 30 })
	m.now = func() time.Time { return r43Now }
	for _, r := range r43Recs(200) {
		if err := m.usage.append(r); err != nil {
			t.Fatal(err)
		}
	}
	want, err := m.Usage(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	m.compactUsage()
	got, err := m.Usage(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	r43SameTeams(t, "manager", got, want)
	if _, aggs, _ := m.usage.readAll(time.Time{}); len(aggs) == 0 {
		t.Fatal("no aggregates after compactUsage")
	}

	off, _ := newTestManager(t, func(c *Config) { c.UsageRetentionDays = -1 })
	off.now = func() time.Time { return r43Now }
	off.usage.append(usageRec{"vm", "v", "t", "s", r43Now.Add(-900 * 24 * time.Hour), r43Now.Add(-899 * 24 * time.Hour)})
	off.compactUsage()
	if _, aggs, _ := off.usage.readAll(time.Time{}); len(aggs) != 0 {
		t.Fatal("negative retention must not compact")
	}
	if (Config{}).usageRetention() != ledgerDefaultRetention || (Config{UsageRetentionDays: 7}).usageRetention() != 7*24*time.Hour {
		t.Fatal("usageRetention defaults")
	}
}
