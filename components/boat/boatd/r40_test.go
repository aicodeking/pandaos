package main

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func pendingForkSetup(t *testing.T) (*Manager, VM, VM) {
	t.Helper()
	m, _ := newTestManager(t, nil)
	p, err := m.Create(context.Background(), CreateReq{Team: "t1", Name: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := m.Create(context.Background(), CreateReq{Team: "t1", Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	return m, p, other
}

// While the fsynced vm.json of a pending fork is parked, Get and List and the other
// VMs' commits proceed; the copy is not published until the write is durable.
func TestR40PendingForkPrepareFsyncOffGlobalLock(t *testing.T) {
	m, p, other := pendingForkSetup(t)
	park := newParkedSave(m, func(v VM) bool { return v.PendingCopy && !v.CopyReady })
	type res struct {
		v   VM
		err error
	}
	done := make(chan res, 1)
	go func() {
		v, err := m.fork(context.Background(), p.ID, "child", "t1", nil, true)
		done <- res{v, err}
	}()
	select {
	case <-park.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the pending vm.json was never written")
	}
	within(t, "Get", 2*time.Second, func() {
		if _, err := m.get(other.ID); err != nil {
			t.Error(err)
		}
	})
	within(t, "List", 2*time.Second, func() {
		for _, v := range m.list("") {
			if v.PendingCopy {
				t.Errorf("copy published before its vm.json was durable: %+v", v)
			}
		}
	})
	within(t, "update of another VM", 2*time.Second, func() {
		if _, err := m.update(other.ID, "r40", func(x *VM) { x.Labels = map[string]string{"a": "b"} }); err != nil {
			t.Error(err)
		}
	})
	within(t, "second pending alloc", 2*time.Second, func() {
		m.mu.Lock()
		n := len(m.allocating)
		m.mu.Unlock()
		if n != 1 {
			t.Errorf("reservation not held while the write is parked: %d", n)
		}
	})
	select {
	case r := <-done:
		t.Fatalf("fork returned before its write was durable: %+v %v", r.v, r.err)
	default:
	}
	close(park.release)
	select {
	case r := <-done:
		if r.err != nil || !r.v.PendingCopy || !r.v.CopyReady {
			t.Fatalf("fork: %+v %v", r.v, r.err)
		}
		if d := onDisk(t, m, r.v.ID); !d.PendingCopy || !d.CopyReady {
			t.Fatalf("not durable: %+v", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fork did not finish")
	}
}

// A failed prepare write publishes nothing and returns the reservation.
func TestR40PendingForkPrepareWriteFailureRollsBack(t *testing.T) {
	m, p, _ := pendingForkSetup(t)
	real := m.saveState
	m.mu.Lock()
	m.saveState = func(dir string, v VM) error {
		if v.PendingCopy {
			return errors.New("disk on fire")
		}
		return real(dir, v)
	}
	m.mu.Unlock()
	before := len(m.list(""))
	if _, err := m.fork(context.Background(), p.ID, "", "t1", nil, true); err == nil {
		t.Fatal("write failure ignored")
	}
	m.mu.Lock()
	nres, nvms := len(m.allocating), len(m.vms)
	m.mu.Unlock()
	if nres != 0 || nvms != before {
		t.Fatalf("leaked: %d reservations, %d vms (was %d)", nres, nvms, before)
	}
}

// The release is not visible (the copy stays pending, not startable) until its
// vm.json is durable, and the fsync is off the global lock.
func TestR40ReleaseFsyncOffGlobalLockStaysPendingUntilDurable(t *testing.T) {
	m, p, other := pendingForkSetup(t)
	c, err := m.fork(context.Background(), p.ID, "child", "t1", nil, true)
	if err != nil || !c.PendingCopy || !c.CopyReady {
		t.Fatalf("fork: %+v %v", c, err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	m.mu.Lock()
	m.saveRelease = func(path string, v any, mode os.FileMode) error {
		close(entered)
		<-release
		return writeJSONAtomic(path, v, mode)
	}
	m.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := m.releaseCopy(context.Background(), c.ID, nil, false)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("release never wrote")
	}
	within(t, "Get of another VM", 2*time.Second, func() {
		if _, err := m.get(other.ID); err != nil {
			t.Error(err)
		}
	})
	within(t, "List", 2*time.Second, func() { m.list("") })
	cur, err := m.get(c.ID)
	if err != nil || !cur.PendingCopy {
		t.Fatalf("authorized in memory before the write was durable: %+v %v", cur, err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("release did not finish")
	}
	if d := onDisk(t, m, c.ID); d.PendingCopy {
		t.Fatalf("authorization not durable: %+v", d)
	}
}

// A failed authorization write leaves the copy pending and retryable.
func TestR40ReleaseWriteFailureLeavesCopyPending(t *testing.T) {
	m, p, _ := pendingForkSetup(t)
	c, err := m.fork(context.Background(), p.ID, "", "t1", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.saveRelease = func(string, any, os.FileMode) error { return errors.New("boom") }
	m.mu.Unlock()
	if _, err := m.releaseCopy(context.Background(), c.ID, nil, false); err == nil {
		t.Fatal("write failure ignored")
	}
	if cur, _ := m.get(c.ID); !cur.PendingCopy {
		t.Fatalf("authorized despite a failed write: %+v", cur)
	}
	m.mu.Lock()
	m.saveRelease = nil
	m.mu.Unlock()
	if v, err := m.releaseCopy(context.Background(), c.ID, nil, false); err != nil || v.PendingCopy {
		t.Fatalf("retry: %+v %v", v, err)
	}
}

// Two transitions of one pending copy racing each other both land, the last on disk
// being derived from the newest in-memory state.
func TestR40ConcurrentPendingUpdatesBothLand(t *testing.T) {
	m, p, _ := pendingForkSetup(t)
	c, err := m.fork(context.Background(), p.ID, "", "t1", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, k := range []string{"a", "b", "c", "d"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.update(c.ID, "r40", func(x *VM) {
				l := map[string]string{}
				for a, b := range x.Labels {
					l[a] = b
				}
				l[k] = "1"
				x.Labels = l
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	d := onDisk(t, m, c.ID)
	cur, _ := m.get(c.ID)
	if len(cur.Labels) != 4 || len(d.Labels) != 4 || !d.PendingCopy {
		t.Fatalf("lost an update: mem=%v disk=%v", cur.Labels, d.Labels)
	}
}
