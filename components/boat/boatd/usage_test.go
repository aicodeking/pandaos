package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// A since query reads only the tail of usage.jsonl that can still overlap it, and
// sums the same as a full scan.
func TestLedgerIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rec := func(i int) usageRec {
		return usageRec{"vm", "vm-" + string(rune('a'+i%7)), "t", "s", t0.Add(time.Duration(i) * time.Minute), t0.Add(time.Duration(i+3) * time.Minute)}
	}
	const n = 5000
	const appended = 300 // each append fsyncs: the rest is written as boatd found it at start
	var pre []byte
	for i := 0; i < n-appended; i++ {
		b, _ := json.Marshal(rec(i))
		pre = append(append(pre, b...), '\n')
	}
	if err := os.WriteFile(path, pre, 0o600); err != nil {
		t.Fatal(err)
	}
	l := &ledger{path: path}
	if all, err := l.read(time.Time{}); err != nil || len(all) != n-appended {
		t.Fatalf("all: %d %v", len(all), err)
	}
	for i := n - appended; i < n; i++ { // through the indexed ledger
		if err := l.append(rec(i)); err != nil {
			t.Fatal(err)
		}
	}
	since := t0.Add(time.Duration(n-100) * time.Minute)
	got, err := l.read(since)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 100 || len(got) > 100+ledgerMarkEvery {
		t.Fatalf("since query read %d records, want the tail only", len(got))
	}
	var full []usageRec
	for i := 0; i < n; i++ {
		full = append(full, rec(i))
	}
	until := t0.Add(time.Duration(n+10) * time.Minute)
	if a, b := summarize(got, since, until), summarize(full, since, until); !reflect.DeepEqual(a, b) {
		t.Fatalf("indexed sums differ:\n%+v\n%+v", a, b)
	}
	// A record whose End is out of order (clock step) is still found.
	late := usageRec{"vm", "vm-late", "t", "s", t0, t0.Add(time.Duration(n+5) * time.Minute)}
	early := usageRec{"vm", "vm-early", "t", "s", t0, t0.Add(time.Minute)}
	l.append(late)
	l.append(early)
	got, _ = l.read(t0.Add(time.Duration(n+3) * time.Minute))
	if len(got) != 1 || got[0].ID != "vm-late" {
		t.Fatalf("after the last regular record: %+v", got)
	}
	// Truncated behind boatd's back (an operator rotating it): rebuilt, nothing stale.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	l.append(rec(1))
	if got, err := l.read(time.Time{}); err != nil || len(got) != 1 {
		t.Fatalf("after truncate: %d %v", len(got), err)
	}
}
