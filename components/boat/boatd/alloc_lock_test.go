package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// gateRun wraps m.run: a command matching match blocks on release after signalling
// entered; failWith, when set, makes it fail instead of running.
type gateRun struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGateRun(m *Manager, match func(name string, args []string) bool, failWith error) *gateRun {
	g := &gateRun{entered: make(chan struct{}), release: make(chan struct{})}
	inner := m.run
	m.run = func(stdin, name string, args ...string) error {
		if match(name, args) {
			g.once.Do(func() { close(g.entered) })
			<-g.release
			if failWith != nil {
				return failWith
			}
		}
		return inner(stdin, name, args...)
	}
	return g
}

func isSubvolCreate(name string, args []string) bool {
	return name == "btrfs" && len(args) >= 2 && args[0] == "subvolume" && args[1] == "create"
}

func isConntrack(name string, _ []string) bool { return name == "conntrack" }

func btrfsManager(t *testing.T, mod func(*Config)) (*Manager, *fakeHost) {
	return newTestManager(t, func(c *Config) {
		c.BtrfsQgroups = true
		if mod != nil {
			mod(c)
		}
	})
}

func baseVM(team string) VM {
	return VM{Team: team, Size: "default", VCPU: 1, MemMiB: 512, DiskGiB: 1}
}

type allocResult struct {
	v   VM
	err error
}

func startAlloc(m *Manager, v VM) <-chan allocResult {
	ch := make(chan allocResult, 1)
	go func() {
		got, err := m.alloc(v, Creating)
		ch <- allocResult{got, err}
	}()
	return ch
}

func waitCh[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out: %s", what)
		panic("unreachable")
	}
}

// readersProceed: m.mu is free, so list/get/update on another VM finish while alloc is
// stuck in a subprocess.
func readersProceed(t *testing.T, m *Manager, other string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.list("")
		m.get(other)
		if _, err := m.update(other, "", func(*VM) {}); err != nil {
			t.Errorf("update: %v", err)
		}
		m.lockBusy(other)
	}()
	waitCh(t, done, "m.mu held while alloc runs a subprocess")
}

func TestAllocDoesNotHoldLockDuringSubprocesses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		match func(string, []string) bool
	}{
		{"makeVMDir btrfs", isSubvolCreate},
		{"flushConntrack", isConntrack},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := btrfsManager(t, nil)
			other, err := m.alloc(baseVM("t1"), Stopped) // before the gate: team vol exists
			if err != nil {
				t.Fatal(err)
			}
			g := newGateRun(m, tc.match, nil)
			team := "t1"
			// A new team needs its subvolume created: that is the blocked btrfs call.
			if tc.name == "makeVMDir btrfs" {
				team = "t2"
			}
			res := startAlloc(m, baseVM(team))
			waitCh(t, g.entered, "subprocess never started")
			readersProceed(t, m, other.ID)
			close(g.release)
			r := waitCh(t, res, "alloc")
			if r.err != nil {
				t.Fatal(r.err)
			}
			if _, err := m.get(r.v.ID); err != nil {
				t.Fatalf("allocated VM not published: %v", err)
			}
			m.mu.Lock()
			n := len(m.allocating)
			m.mu.Unlock()
			if n != 0 {
				t.Fatalf("%d reservations left", n)
			}
		})
	}
}

func TestAllocReservationNotHandedOutTwice(t *testing.T) {
	m, _ := btrfsManager(t, func(c *Config) { c.DefaultTeam.MaxTotalVMs = 2 })
	first := baseVM("t1")
	g := newGateRun(m, isSubvolCreate, nil)
	res1 := startAlloc(m, first)
	waitCh(t, g.entered, "first alloc in subprocess")

	// A second alloc proceeds past the lock (team vol create is serialised by the
	// gate only for its first call; the team volume mutex makes it wait), so run it
	// after release; the point is the reservation is visible to accounting now.
	m.mu.Lock()
	var reserved VM
	for _, v := range m.allocating {
		reserved = v
	}
	used := map[int]bool{}
	m.eachVMLocked(func(o VM) { used[o.Index] = true })
	idx := m.pickIndexLocked(used)
	m.mu.Unlock()
	if reserved.ID == "" || reserved.Index == 0 {
		t.Fatal("no reservation recorded while alloc is in flight")
	}
	if idx == reserved.Index {
		t.Fatalf("index %d would be handed out twice", idx)
	}
	close(g.release)
	r1 := waitCh(t, res1, "first alloc")
	if r1.err != nil {
		t.Fatal(r1.err)
	}
	r2 := waitCh(t, startAlloc(m, baseVM("t1")), "second alloc")
	if r2.err != nil {
		t.Fatal(r2.err)
	}
	if r1.v.Index == r2.v.Index || r1.v.ID == r2.v.ID {
		t.Fatalf("duplicate slot: %d/%d %s/%s", r1.v.Index, r2.v.Index, r1.v.ID, r2.v.ID)
	}
}

func TestAllocReservationCountsAgainstQuota(t *testing.T) {
	m, _ := btrfsManager(t, func(c *Config) { c.DefaultTeam.MaxTotalVMs = 1 })
	g := newGateRun(m, isSubvolCreate, nil)
	res1 := startAlloc(m, baseVM("t1"))
	waitCh(t, g.entered, "first alloc in subprocess")
	_, err := m.alloc(baseVM("t1"), Creating) // while the first is mid-flight
	var he *apiErr
	if err == nil || !errors.As(err, &he) || he.code != http.StatusTooManyRequests {
		t.Fatalf("second alloc beyond MaxTotalVMs: %v", err)
	}
	close(g.release)
	if r := waitCh(t, res1, "first alloc"); r.err != nil {
		t.Fatal(r.err)
	}
}

func TestAllocRollbackOnSubprocessFailure(t *testing.T) {
	m, _ := btrfsManager(t, nil)
	boom := errors.New("btrfs: boom")
	g := newGateRun(m, isSubvolCreate, boom)
	close(g.release)
	_, err := m.alloc(baseVM("t1"), Creating)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("alloc error: %v", err)
	}
	m.mu.Lock()
	nres, nvms := len(m.allocating), len(m.vms)
	m.mu.Unlock()
	if nres != 0 || nvms != 0 {
		t.Fatalf("leaked: %d reservations, %d vms", nres, nvms)
	}
	// The slot is free again: the next alloc takes the lowest index.
	m.run = newFakeHost().run
	v, err := m.alloc(baseVM("t1"), Creating)
	if err != nil {
		t.Fatal(err)
	}
	if v.Index != 2 {
		t.Fatalf("index %d not reused after rollback", v.Index)
	}
}

func TestAllocRollbackOnCommitFailure(t *testing.T) {
	m, _ := btrfsManager(t, nil)
	m.saveState = func(string, VM) error { return errors.New("disk on fire") }
	_, err := m.alloc(baseVM("t1"), Creating)
	if err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("alloc error: %v", err)
	}
	m.mu.Lock()
	nres, nvms := len(m.allocating), len(m.vms)
	m.mu.Unlock()
	if nres != 0 || nvms != 0 {
		t.Fatalf("leaked: %d reservations, %d vms", nres, nvms)
	}
	if entries, _ := os.ReadDir(filepath.Join(m.cfg.StateDir, "vms")); len(entries) != 0 {
		t.Fatalf("VM dir left behind: %v", entries)
	}
}

func TestAllocBatchStillAllOrNone(t *testing.T) {
	m, _ := btrfsManager(t, func(c *Config) { c.DefaultTeam.MaxTotalVMs = 2 })
	bases := []VM{baseVM("t1"), baseVM("t1"), baseVM("t1")}
	if _, err := m.allocBatch(bases); err == nil {
		t.Fatal("batch past quota succeeded")
	}
	if got := m.list(""); len(got) != 0 {
		t.Fatalf("%d VMs left after failed batch", len(got))
	}
	vms, err := m.allocBatch(bases[:2])
	if err != nil || len(vms) != 2 || vms[0].Index == vms[1].Index {
		t.Fatalf("batch: %v %v", vms, err)
	}
}
