package main

import (
	"reflect"
	"testing"
	"time"
)

// The startup index build scans with no lock held: an append made from inside the
// scan window completes (a scan under l.mu would deadlock it), the record lands in
// the index through the tail catch-up, and the totals match a locked build.
func TestR47StartupBuildDoesNotBlockAppends(t *testing.T) {
	l := r43Ledger(t, r43Recs(3000))
	want := &ledger{path: l.path}
	extra := usageRec{"vm", "vm-extra", "ta", "small", r43Now.Add(-time.Hour), r43Now}

	appended := make(chan error, 1)
	l.buildHook = func() {
		done := make(chan error, 1)
		go func() { done <- l.append(extra) }()
		select {
		case err := <-done:
			appended <- err
		case <-time.After(30 * time.Second): // only a failure waits this long
			appended <- errBlocked
		}
	}
	l.buildIndex()
	if err := <-appended; err != nil {
		t.Fatalf("append during the scan: %v", err)
	}
	if got := l.offBuilds.Load(); got != 1 {
		t.Fatalf("startup build installed %d times, want 1", got)
	}
	builds := l.builds.Load()
	got, err := l.read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if l.builds.Load() != builds {
		t.Fatalf("first read rebuilt the index under the lock")
	}
	exp, err := want.read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, exp) || !contains(got, extra) {
		t.Fatalf("totals differ: %d records vs %d locked", len(got), len(exp))
	}
}

// A read that built the index first wins: the startup build installs nothing.
func TestR47StartupBuildYieldsToRead(t *testing.T) {
	l := r43Ledger(t, r43Recs(100))
	l.buildHook = func() {
		if _, err := l.read(time.Time{}); err != nil {
			t.Error(err)
		}
	}
	l.buildIndex()
	if l.offBuilds.Load() != 0 || l.builds.Load() != 1 {
		t.Fatalf("offBuilds=%d builds=%d", l.offBuilds.Load(), l.builds.Load())
	}
}

type r47Err string

func (e r47Err) Error() string { return string(e) }

const errBlocked = r47Err("append blocked while the index was being built")

func contains(rs []usageRec, r usageRec) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}
