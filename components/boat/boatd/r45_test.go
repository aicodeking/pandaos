package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// r45AggDays counts the aggregate lines in the file and the oldest day among them.
func r45AggDays(t *testing.T, l *ledger) (n int, oldest time.Time) {
	t.Helper()
	b, err := os.ReadFile(l.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		var ln ledgerLine
		if len(line) == 0 || json.Unmarshal(line, &ln) != nil || !ln.Agg {
			continue
		}
		n++
		if oldest.IsZero() || ln.Day.Before(oldest) {
			oldest = ln.Day
		}
	}
	return n, oldest
}

// Aggregates older than the aggregate retention are pruned; every window that starts
// at or after the prune cutoff keeps its exact totals, and an all-time query counts
// from the cutoff.
func TestR45PruneAggregatesKeepsRequestableWindows(t *testing.T) {
	l := r43Ledger(t, r43Recs(1500))
	const keep = 500 * 24 * time.Hour
	l.aggRetention = keep
	aggCut := l.aggCutoff(r43Now, r43Retention)
	if want := r43Now.Add(-keep).Truncate(24 * time.Hour); !aggCut.Equal(want) {
		t.Fatalf("aggCutoff %v want %v", aggCut, want)
	}
	windows := map[string]time.Time{
		"cut-of-aggs":   aggCut,
		"after-aggs":    aggCut.Add(5 * 24 * time.Hour), // a day boundary, as billing periods are
		"record-cutoff": r43Cutoff(),
		"recent":        r43Now.Add(-48 * time.Hour),
		"billing-month": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
	before := map[string]usageReport{}
	for k, s := range windows {
		before[k] = r43Report(t, l, s)
	}
	allTime := r43Report(t, l, aggCut) // what the all-time query must become
	oldBefore := r43Report(t, l, time.Time{})

	if _, err := l.compact(r43Now, r43Retention); err != nil {
		t.Fatal(err)
	}
	for k, s := range windows {
		r43SameTeams(t, k, r43Report(t, l, s), before[k])
	}
	r43SameTeams(t, "all-time", r43Report(t, l, time.Time{}), allTime)
	if n, oldest := r45AggDays(t, l); n == 0 || oldest.Before(aggCut) {
		t.Fatalf("aggregates after pruning: %d lines, oldest %v (cutoff %v)", n, oldest, aggCut)
	}
	r44SameIndex(t, l, r44Fresh(t, l))
	// Something really was pruned: the all-time total is below the unpruned one.
	if g, w := r43Teams(r43Report(t, l, time.Time{})), r43Teams(oldBefore); g[0].VMSeconds+g[1].VMSeconds+g[2].VMSeconds >= w[0].VMSeconds+w[1].VMSeconds+w[2].VMSeconds {
		t.Fatalf("nothing pruned: %+v vs %+v", g, w)
	}
}

// A pass with no record to fold still runs when aggregates have aged out, and the
// default retention (5 years) prunes nothing the other tests keep.
func TestR45PruneOnlyPassAndDefault(t *testing.T) {
	l := r43Ledger(t, r43Recs(800))
	if _, err := l.compact(r43Now, r43Retention); err != nil {
		t.Fatal(err)
	}
	n0, _ := r45AggDays(t, l)
	want := r43Report(t, l, time.Time{})
	if n, err := l.compact(r43Now, r43Retention); err != nil || n != 0 {
		t.Fatalf("second pass folded %d: %v", n, err)
	}
	if n1, _ := r45AggDays(t, l); n1 != n0 {
		t.Fatalf("default retention changed the aggregates: %d -> %d", n0, n1)
	}
	r43SameTeams(t, "default keeps all", r43Report(t, l, time.Time{}), want)

	// The window moves on with no new record to fold: the prune alone rewrites.
	l.aggRetention = 450 * 24 * time.Hour
	size0, _ := fileSize(l.path)
	if _, err := l.compact(r43Now, r43Retention); err != nil {
		t.Fatal(err)
	}
	n2, oldest := r45AggDays(t, l)
	size1, _ := fileSize(l.path)
	if cut := l.aggCutoff(r43Now, r43Retention); n2 >= n0 || size1 >= size0 || oldest.Before(cut) {
		t.Fatalf("prune-only pass: aggs %d -> %d, bytes %d -> %d, oldest %v, cutoff %v", n0, n2, size0, size1, oldest, cut)
	}
	// Idempotent.
	if _, err := l.compact(r43Now, r43Retention); err != nil {
		t.Fatal(err)
	}
	if n3, _ := r45AggDays(t, l); n3 != n2 {
		t.Fatalf("second prune changed %d -> %d", n2, n3)
	}
}

// A configured record retention longer than the aggregate retention keeps aggregates
// at least as long as the records.
func TestR45AggCutoffNeverBeforeRecordCutoff(t *testing.T) {
	l := &ledger{aggRetention: 10 * 24 * time.Hour}
	if got, want := l.aggCutoff(r43Now, r43Retention), r43Cutoff(); !got.Equal(want) {
		t.Fatalf("aggCutoff %v want the record cutoff %v", got, want)
	}
	if got := (&ledger{}).aggCutoff(r43Now, r43Retention); !got.Equal(r43Now.Add(-ledgerAggRetention).Truncate(24 * time.Hour)) {
		t.Fatalf("default aggCutoff %v", got)
	}
}

// A query with no `since` parses the whole ledger, but not under l.mu: an append made
// while it scans completes at once (no timing: the scan is parked on a hook), and the
// totals are those of the snapshot, then include the append.
func TestR45SinceLessReadDoesNotBlockAppend(t *testing.T) {
	l := r43Ledger(t, r43Recs(3000))
	want := r43Report(t, l, time.Time{})

	entered, release := make(chan struct{}), make(chan struct{})
	l.scanHook = func() {
		close(entered)
		<-release
	}
	type res struct {
		recs []usageRec
		aggs []aggRow
		err  error
	}
	done := make(chan res, 1)
	go func() {
		recs, aggs, err := l.readAll(time.Time{})
		done <- res{recs, aggs, err}
	}()
	<-entered // the reader holds its snapshot and is parked before the scan

	appended := make(chan error, 1)
	extra := usageRec{"vm", "vm-during-scan", "ta", "small", r43Now.Add(-time.Hour), r43Now}
	go func() { appended <- l.append(extra) }()
	select {
	case err := <-appended:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second): // failure detector only
		close(release)
		t.Fatal("append blocked behind a since-less read")
	}
	// A second reader is not blocked either.
	l.scanHook = nil
	if _, _, err := l.readAll(r43Now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	close(release)
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	rep := summarize(r.recs, time.Time{}, r43Now)
	applyAggs(&rep, r.aggs, time.Time{})
	r43SameTeams(t, "snapshot at read start", rep, want)

	// The append is in the next read.
	want2 := summarize(append(append([]usageRec{}, r.recs...), extra), time.Time{}, r43Now)
	applyAggs(&want2, r.aggs, time.Time{})
	r43SameTeams(t, "after append", r43Report(t, l, time.Time{}), want2)
}

// A compaction that renames a new file in while a reader is between its snapshot and
// its scan changes nothing for that reader.
func TestR45CompactionDuringScanKeepsSnapshot(t *testing.T) {
	l := r43Ledger(t, r43Recs(1500))
	want := r43Report(t, l, time.Time{})
	entered, release := make(chan struct{}), make(chan struct{})
	l.scanHook = func() {
		close(entered)
		<-release
	}
	var got usageReport
	done := make(chan struct{})
	go func() {
		defer close(done)
		recs, aggs, err := l.readAll(time.Time{})
		if err != nil {
			t.Error(err)
			return
		}
		got = summarize(recs, time.Time{}, r43Now)
		applyAggs(&got, aggs, time.Time{})
	}()
	<-entered
	l.scanHook = nil
	if n, err := l.compact(r43Now, r43Retention); err != nil || n == 0 {
		t.Fatalf("folded %d: %v", n, err)
	}
	close(release)
	<-done
	r43SameTeams(t, "reader across a compaction", got, want)
}
