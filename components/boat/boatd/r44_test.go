package main

import (
	"reflect"
	"testing"
	"time"
)

// r44Fresh is a second ledger over the same file, its index built by a full scan.
func r44Fresh(t *testing.T, l *ledger) *ledger {
	t.Helper()
	fresh := &ledger{path: l.path}
	if _, err := fresh.read(time.Time{}); err != nil {
		t.Fatal(err)
	}
	return fresh
}

func r44SameIndex(t *testing.T, got, want *ledger) {
	t.Helper()
	got.mu.Lock()
	defer got.mu.Unlock()
	if !got.built || got.count != want.count || got.indexed != want.indexed || !got.maxEnd.Equal(want.maxEnd) ||
		!reflect.DeepEqual(got.marks, want.marks) || !reflect.DeepEqual(got.aggs, want.aggs) {
		t.Fatalf("installed index differs from a fresh scan:\n got built=%v count=%d indexed=%d marks=%d aggs=%d\nwant count=%d indexed=%d marks=%d aggs=%d",
			got.built, got.count, got.indexed, len(got.marks), len(got.aggs),
			want.count, want.indexed, len(want.marks), len(want.aggs))
	}
}

// After compaction the first (windowed) read scans no whole file, totals are unchanged,
// and the installed index is exactly what a fresh scan builds.
func TestR44CompactionInstallsIndexNoRescan(t *testing.T) {
	l := r43Ledger(t, r43Recs(4000))
	sinces := r43Sinces()
	before := map[string]usageReport{}
	for name, s := range sinces {
		before[name] = r43Report(t, l, s)
	}
	builds := l.builds.Load()
	if _, err := l.compact(r43Now, r43Retention); err != nil {
		t.Fatal(err)
	}
	r44SameIndex(t, l, r44Fresh(t, l))
	for name, s := range sinces {
		r43SameTeams(t, name, r43Report(t, l, s), before[name])
	}
	if got := l.builds.Load(); got != builds {
		t.Fatalf("index rebuilt by a full scan after compaction (%d -> %d builds)", builds, got)
	}
}

// Records appended during the rewrite (bulk, then the last few under the lock) are in
// the installed index too, and an append after it keeps it in step.
func TestR44TailAppendedDuringCompactionIsIndexed(t *testing.T) {
	l := r43Ledger(t, r43Recs(2500))
	n := 0
	add := func(c int) {
		for i := 0; i < c; i++ {
			s := r43Now.Add(-time.Duration(n+1) * time.Minute)
			if err := l.append(usageRec{"vm", "vm-tail-" + string(rune('a'+n%26)) + string(rune('a'+n/26%26)), "tz", "small", s, s.Add(30 * time.Second)}); err != nil {
				t.Error(err)
			}
			n++
		}
	}
	l.compactHook = func(stage string) error {
		switch stage {
		case "rewritten":
			add(1500) // above ledgerCatchUpBytes: copied without the lock
		case "caught-up":
			add(3)
		}
		return nil
	}
	if _, err := l.compact(r43Now, r43Retention); err != nil {
		t.Fatal(err)
	}
	l.compactHook = nil
	builds := l.builds.Load()
	r44SameIndex(t, l, r44Fresh(t, l))
	if err := l.append(usageRec{"vm", "vm-after", "ta", "small", r43Now.Add(-time.Hour), r43Now}); err != nil {
		t.Fatal(err)
	}
	got, err := l.read(r43Now.Add(-2 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range got {
		found = found || r.ID == "vm-after"
	}
	if !found || l.builds.Load() != builds {
		t.Fatalf("append after compaction: found=%v builds %d -> %d", found, builds, l.builds.Load())
	}
	r44SameIndex(t, l, r44Fresh(t, l))
}

// An append concurrent with the first read after compaction is not held up by a scan:
// the read takes l.mu only for a tail scan from the last mark.
func TestR44AppendDuringFirstReadAfterCompaction(t *testing.T) {
	l := r43Ledger(t, r43Recs(20000))
	if _, err := l.compact(r43Now, r43Retention); err != nil {
		t.Fatal(err)
	}
	builds := l.builds.Load()
	done := make(chan error, 1)
	go func() {
		_, _, err := l.readAll(r43Now.Add(-24 * time.Hour))
		done <- err
	}()
	if err := l.append(usageRec{"vm", "vm-conc", "ta", "small", r43Now.Add(-time.Hour), r43Now}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if l.builds.Load() != builds {
		t.Fatal("the first read after compaction scanned the whole file")
	}
	r44SameIndex(t, l, r44Fresh(t, l))
}
