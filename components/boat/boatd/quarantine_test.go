package main

import (
	"context"
	"testing"
	"time"
)

// A deleted VM's address must not go to the next VM at once: boat-api's ssh jump
// grants (permitopen="<ip>:22") follow boatd's VM list asynchronously, so the
// next owner of the ip would be reachable with the previous owner's key until then.
func TestFreedIndexQuarantined(t *testing.T) {
	clock := time.Now().UTC() // newManager loads freed.json against the real clock
	m, _ := newTestManager(t, nil)
	m.now = func() time.Time { return clock }
	ctx := context.Background()
	a, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	b, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Index == a.Index || b.IP == a.IP {
		t.Fatalf("index %d (%s) reused right after its delete", b.Index, b.IP)
	}

	// Quarantine survives a boatd restart: freed.json.
	m2, err := newManager(m.cfg, m.run, m.procs)
	if err != nil {
		t.Fatal(err)
	}
	m2.now = func() time.Time { return clock }
	if until, ok := m2.freed[a.Index]; !ok || !until.After(clock) {
		t.Fatalf("quarantine of %d lost across a restart: %v", a.Index, m2.freed)
	}

	// After the period the lowest index is preferred again.
	clock = clock.Add(time.Duration(m.cfg.IndexQuarantineS)*time.Second + time.Second)
	c, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Index != a.Index {
		t.Fatalf("index %d not reused after its quarantine, got %d", a.Index, c.Index)
	}
}

// Quarantine never costs capacity: with every free index quarantined, the one
// freed longest ago is taken.
func TestPickIndexFallsBackToOldestFreed(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	m := &Manager{cfg: Config{IndexQuarantineS: 600}, now: func() time.Time { return now }, freed: map[int]time.Time{}}
	used := map[int]bool{}
	for i := 2; i <= maxIndex; i++ {
		used[i] = true
	}
	delete(used, 7)
	delete(used, 9)
	m.freed[7] = now.Add(500 * time.Second) // freed 100 s ago
	m.freed[9] = now.Add(400 * time.Second) // freed 200 s ago
	if got := m.pickIndexLocked(used); got != 9 {
		t.Fatalf("want the index freed longest ago (9), got %d", got)
	}
	used[9] = true
	if got := m.pickIndexLocked(used); got != 7 {
		t.Fatalf("want 7, got %d", got)
	}
	used[7] = true
	if got := m.pickIndexLocked(used); got != 0 {
		t.Fatalf("no free index must give 0, got %d", got)
	}
	m.cfg.IndexQuarantineS = 0 // off: lowest free index, as before
	delete(used, 9)
	delete(used, 7)
	if got := m.pickIndexLocked(used); got != 7 {
		t.Fatalf("quarantine off: want 7, got %d", got)
	}
}
