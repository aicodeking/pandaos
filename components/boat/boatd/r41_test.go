package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func stoppedWithMemory(t *testing.T, m *Manager, name string) VM {
	t.Helper()
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Team: "t1", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if v, err = m.Stop(ctx, v.ID, "api"); err != nil || !v.HasSnapshot {
		t.Fatalf("stop: %+v %v", v, err)
	}
	return v
}

// A fresh_boot start whose memory removal fails (other than "already gone") fails
// instead of restoring the memory it promised to discard.
func TestR41FreshBootRemovalFailureFailsStart(t *testing.T) {
	m, h := newTestManager(t, nil)
	v := stoppedWithMemory(t, m, "fresh")
	root := m.root(v.ID)
	m.removeFile = func(p string) error {
		return &os.PathError{Op: "remove", Path: p, Err: syscall.EACCES}
	}
	before := len(h.vms)
	_, err := m.startVM(context.Background(), v.ID, nil, nil, true)
	if err == nil || !errors.Is(err, syscall.EACCES) {
		t.Fatalf("start must fail with the removal error: %v", err)
	}
	got, _ := m.get(v.ID)
	if got.State != Error || got.PID != 0 {
		t.Fatalf("vm must be left in error, not running: %+v", got)
	}
	if len(h.vms) != before {
		t.Fatalf("no VMM may be launched after a failed discard")
	}
	for _, f := range []string{vmstate, memFile} {
		if !exists(filepath.Join(root, f)) {
			t.Fatalf("%s must be untouched by the failing remover", f)
		}
	}
}

// A file that is already gone is not a failure, and a remover that leaves the pair in
// place cannot make fresh_boot restore it: restore is forced off.
func TestR41FreshBootNotExistIsFineAndNeverRestores(t *testing.T) {
	m, h := newTestManager(t, nil)
	v := stoppedWithMemory(t, m, "fresh")
	m.removeFile = func(p string) error {
		if filepath.Base(p) == vmstate {
			return os.ErrNotExist
		}
		return nil // the pair stays on disk
	}
	got, err := m.startVM(context.Background(), v.ID, nil, nil, true)
	if err != nil || got.State != Running {
		t.Fatalf("start: %+v %v", got, err)
	}
	fc := h.fcOf(t, m, got.ID)
	if fc.called("PUT /snapshot/load") || !fc.called("PUT /actions") {
		t.Fatalf("fresh_boot must cold boot even if the pair still exists: %v", fc.calls)
	}
}

// A real removal failure (read-only dir) fails the start. Skipped as root.
func TestR41FreshBootRealRemovalFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	m, _ := newTestManager(t, nil)
	v := stoppedWithMemory(t, m, "fresh")
	root := m.root(v.ID)
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(root, 0o700) })
	if _, err := m.startVM(context.Background(), v.ID, nil, nil, true); err == nil {
		t.Fatal("start must fail when the memory cannot be removed")
	}
	if !exists(filepath.Join(root, vmstate)) {
		t.Fatal("vmstate must still be there")
	}
}

// Two forks of one parent: the second allocates while the first is parked in freeze;
// freeze itself stays serialized.
func TestR41SameParentAllocRunsWhileFirstForkFreezes(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	p, err := m.Create(ctx, CreateReq{Team: "t1", Name: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	gated, inFreeze, maxInFreeze := 0, 0, 0
	entered, release := make(chan struct{}), make(chan struct{})
	m.freezeGate = func(string) {
		mu.Lock()
		gated++
		inFreeze++
		if inFreeze > maxInFreeze {
			maxInFreeze = inFreeze
		}
		first := gated == 1
		mu.Unlock()
		if first {
			close(entered)
			<-release
		}
		mu.Lock()
		inFreeze--
		mu.Unlock()
	}
	errs := make(chan error, 2)
	fork := func() {
		_, err := m.Fork(ctx, p.ID, "", "t1", nil)
		errs <- err
	}
	go fork()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first fork never reached freeze")
	}
	go fork()
	children := func() int {
		n := 0
		for _, v := range m.list("t1") {
			if v.ParentID == p.ID {
				n++
			}
		}
		return n
	}
	eventually(t, "second fork's child allocated while the first is in freeze", func() bool { return children() == 2 })
	select {
	case err := <-errs:
		t.Fatalf("a fork finished while the first is parked: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("fork did not finish")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if gated != 2 || maxInFreeze != 1 {
		t.Fatalf("freeze must stay serialized: gated=%d max concurrent=%d", gated, maxInFreeze)
	}
}

// The parent changes between alloc and freeze: the allocation is rolled back whole.
func TestR41ParentChangedBetweenAllocAndFreezeRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Manager, string) error
	}{
		{"deleted", func(m *Manager, id string) error { _, err := m.Delete(context.Background(), id); return err }},
		{"stopped", func(m *Manager, id string) error { _, err := m.Stop(context.Background(), id, "api"); return err }},
		{"restarted", func(m *Manager, id string) error {
			if _, err := m.Stop(context.Background(), id, "api"); err != nil {
				return err
			}
			_, err := m.startVM(context.Background(), id, nil, nil, false)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, parent, _ := pendingForkSetup(t)
			park := newParkedSave(m, func(v VM) bool { return v.PendingCopy && !v.CopyReady })
			done := make(chan error, 1)
			go func() {
				_, err := m.fork(context.Background(), parent.ID, "child", "t1", nil, true)
				done <- err
			}()
			select {
			case <-park.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("alloc never reached its fsync")
			}
			// The parent lock is free during alloc: the change is not blocked.
			within(t, "parent change during the child's alloc", 10*time.Second, func() {
				if err := tc.change(m, parent.ID); err != nil {
					t.Error(err)
				}
			})
			m.mu.Lock()
			var cid string
			for id := range m.allocating {
				cid = id
			}
			m.mu.Unlock()
			if cid == "" {
				t.Fatal("child reservation not held while its write is parked")
			}
			close(park.release)
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("fork must fail when the parent changed")
				}
			case <-time.After(20 * time.Second):
				t.Fatal("fork did not finish")
			}
			m.mu.Lock()
			_, inVMs := m.vms[cid]
			nAlloc := len(m.allocating)
			m.mu.Unlock()
			if inVMs || nAlloc != 0 {
				t.Fatalf("leaked child: in vms=%v allocating=%d", inVMs, nAlloc)
			}
			if exists(m.vmDir(cid)) {
				t.Fatalf("child dir %s leaked", m.vmDir(cid))
			}
			if m.lockBusy(cid) {
				t.Fatal("child lifecycle lock leaked")
			}
			// Stop spawns background syncSnapshot which briefly takes parent's lock to record SnapSynced.
			eventually(t, "parent lifecycle lock free", func() bool { return !m.lockBusy(parent.ID) })
			if tc.name != "deleted" {
				if got, err := m.get(parent.ID); err != nil || got.State != Running && got.State != Stopped {
					t.Fatalf("parent damaged: %+v %v", got, err)
				}
			}
		})
	}
}
