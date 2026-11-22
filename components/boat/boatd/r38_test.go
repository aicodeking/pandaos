package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---- fork release with fresh_boot ----

// prepareChild forks a running parent into a pending, copy-ready child.
func prepareChild(t *testing.T, m *Manager) (parent, child VM) {
	t.Helper()
	parent, err := m.Create(context.Background(), CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	child, err = m.fork(context.Background(), parent.ID, "", "t1", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	return parent, child
}

func releaseHTTP(t *testing.T, m *Manager, id string, body any) VM {
	t.Helper()
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	var raw []byte
	if code := call(t, srv, "POST", "/v1/vms/"+id+"/release", body, &raw); code != 200 {
		t.Fatalf("release: %d %s", code, raw)
	}
	var v VM
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestR38ReleaseWithoutFreshBootRestoresMemory(t *testing.T) {
	m, h := newTestManager(t, nil)
	_, c := prepareChild(t, m)
	if !c.HasSnapshot || !exists(filepath.Join(m.root(c.ID), memFile)) {
		t.Fatalf("setup: the prepared child has no memory copy: %+v", c)
	}
	v := releaseHTTP(t, m, c.ID, map[string]any{})
	if !h.fcOf(t, m, v.ID).called("PUT /snapshot/load") {
		t.Fatal("control: a plain release must restore the parent's memory")
	}
}

func TestR38ReleaseFreshBootColdBootsAndDropsMemory(t *testing.T) {
	m, h := newTestManager(t, nil)
	parent, c := prepareChild(t, m)
	root := m.root(c.ID)
	if err := os.WriteFile(filepath.Join(root, "mem.probe"), nil, 0o600); err != nil { // an unrelated file stays
		t.Fatal(err)
	}
	v := releaseHTTP(t, m, c.ID, map[string]any{"fresh_boot": true, "env": map[string]string{"A": "b"}})
	if v.State != Running || v.PendingCopy {
		t.Fatalf("not released: %+v", v)
	}
	fc := h.fcOf(t, m, v.ID)
	if fc.called("PUT /snapshot/load") || !fc.called("PUT /actions") {
		t.Fatalf("fresh_boot must cold boot, never load the parent's snapshot: %v", fc.calls)
	}
	for _, f := range []string{memFile, vmstate} {
		if exists(filepath.Join(root, f)) {
			t.Fatalf("child %s survived a fresh_boot release", f)
		}
	}
	if !exists(filepath.Join(root, diskFile)) || !exists(filepath.Join(root, "mem.probe")) {
		t.Fatal("fresh_boot removed more than the memory snapshot")
	}
	if got := onDisk(t, m, c.ID); got.HasSnapshot || got.SnapSynced {
		t.Fatalf("has_snapshot still recorded: %+v", got)
	}
	if _, err := m.get(parent.ID); err != nil {
		t.Fatal(err)
	}
}

func TestR38ReleaseFreshBootIsIdempotentAfterRelease(t *testing.T) {
	m, _ := newTestManager(t, nil)
	_, c := prepareChild(t, m)
	v1 := releaseHTTP(t, m, c.ID, map[string]any{"fresh_boot": true})
	v2, err := m.releaseCopy(context.Background(), c.ID, nil, true)
	if err != nil || v2.PID != v1.PID {
		t.Fatalf("a replayed release must not reboot: %+v %v", v2, err)
	}
}

func TestR38ReleaseFreshBootRemovalFailureKeepsCopyPending(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	m, _ := newTestManager(t, nil)
	_, c := prepareChild(t, m)
	root := m.root(c.ID)
	os.Chmod(root, 0o500) // the files cannot be unlinked
	t.Cleanup(func() { os.Chmod(root, 0o700) })
	if _, err := m.releaseCopy(context.Background(), c.ID, nil, true); err == nil {
		t.Fatal("release succeeded although the memory could not be discarded")
	}
	if got, _ := m.get(c.ID); !got.PendingCopy || got.PID != 0 {
		t.Fatalf("the copy escaped with its memory: %+v", got)
	}
}

// ---- tombstones outside the quota'd subvolume ----

func TestR38TombstoneLivesOutsideTheVMDir(t *testing.T) {
	m, _ := newTestManager(t, nil)
	v, err := m.Create(context.Background(), CreateReq{Name: "x", Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.writeTombstone(v.ID, v); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(m.cfg.StateDir, "tombstones", v.ID)
	if m.tombstonePath(v.ID) != want || !exists(want) {
		t.Fatalf("tombstone not at %s", want)
	}
	if exists(filepath.Join(m.vmDir(v.ID), tombstoneFile)) {
		t.Fatal("tombstone written inside the (quota'd) VM dir")
	}
	m.clearTombstone(v.ID)
	if m.tombstoned(v.ID) {
		t.Fatal("tombstone not cleared")
	}
}

func TestR38DeleteNeedsNoSpaceInTheVMDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	m, _ := newTestManager(t, nil)
	v, err := m.Create(context.Background(), CreateReq{Name: "full", Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	// The team's volume cannot take one more file: nothing may be created in the VM dir.
	dir := m.vmDir(v.ID)
	os.Chmod(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	var inDir bool
	inner := m.run
	m.run = func(stdin, name string, args ...string) error {
		inDir = inDir || exists(filepath.Join(dir, tombstoneFile))
		os.Chmod(dir, 0o700) // only the tombstone write is under test, removal needs to proceed
		return inner(stdin, name, args...)
	}
	if _, err := m.Delete(context.Background(), v.ID); err != nil {
		t.Fatalf("delete with a full team volume: %v", err)
	}
	if inDir {
		t.Fatal("the tombstone used the VM dir")
	}
	if m.tombstoned(v.ID) || exists(dir) {
		t.Fatal("tombstone or dir left behind")
	}
}

func TestR38LegacyInDirMarkerIsHonoredAndMigrated(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	gone, err := m.Create(ctx, CreateReq{Name: "gone", Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	keep, err := m.Create(ctx, CreateReq{Name: "keep", Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.vmDir(gone.ID), tombstoneFile), []byte("deleting\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.closeWatches()
	dead := newFakeHost()
	dead.next = h.next + 100
	m2, err := newManager(m.cfg, dead.run, dead)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m2.closeWatches)
	if _, err := m2.get(gone.ID); err == nil || exists(m2.vmDir(gone.ID)) {
		t.Fatal("a legacy-marked delete was not finished")
	}
	if m2.tombstoned(gone.ID) {
		t.Fatal("the finished delete left a tombstone")
	}
	if _, err := m2.get(keep.ID); err != nil {
		t.Fatalf("an unmarked VM vanished: %v", err)
	}
}

func TestR38TombstoneForAMissingDirIsCleared(t *testing.T) {
	m, _ := newTestManager(t, nil)
	if err := m.writeTombstone("vm-ghost", VM{}); err != nil {
		t.Fatal(err)
	}
	m.finishTombstones()
	if m.tombstoned("vm-ghost") {
		t.Fatal("a tombstone with no dir was kept forever")
	}
}

// lockDir makes dir's contents unremovable until the returned func is called.
func lockDir(t *testing.T, dir string) func() {
	t.Helper()
	sub := filepath.Join(dir, "stuck")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chmod(sub, 0o500)
	unlock := func() { os.Chmod(sub, 0o700) }
	t.Cleanup(unlock)
	return unlock
}

func TestR38FailedRemovalIsTerminalCountedAndRetried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	m, _ := newTestManager(t, nil)
	m.deleteRetryDelay = 20 * time.Millisecond
	v, err := m.Create(context.Background(), CreateReq{Name: "stuck", Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	unlock := lockDir(t, m.vmDir(v.ID))

	if _, err := m.Delete(context.Background(), v.ID); err == nil {
		t.Fatal("delete reported success with the dir still there")
	}
	got, err := m.get(v.ID)
	if err != nil || got.State != Error || !strings.Contains(got.Error, "delete failed") {
		t.Fatalf("a failed delete must leave a terminal error VM: %+v %v", got, err)
	}
	if !m.tombstoned(v.ID) {
		t.Fatal("tombstone dropped while the dir still exists")
	}
	_, serr := m.Start(context.Background(), v.ID, nil)
	if serr == nil || !strings.Contains(serr.Error(), "being deleted") {
		t.Fatalf("start of a half-deleted VM must be refused clearly: %v", serr)
	}
	// Its disk still counts against the team while the dir exists.
	m.mu.Lock()
	var counted bool
	m.eachVMLocked(func(x VM) { counted = counted || (x.ID == v.ID && x.Team == "t1") })
	m.mu.Unlock()
	if !counted {
		t.Fatal("the VM stopped counting while its dir exists")
	}

	unlock()
	eventually(t, "background retry removes the dir", func() bool { _, e := m.get(v.ID); return e != nil })
	if exists(m.vmDir(v.ID)) || m.tombstoned(v.ID) {
		t.Fatal("dir or tombstone left after the retry")
	}
}

func TestR38RestartKeepsAFailedDeleteAsErrorVM(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	m, h := newTestManager(t, nil)
	v, err := m.Create(context.Background(), CreateReq{Name: "stuck", Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	unlock := lockDir(t, m.vmDir(v.ID))
	if _, err := m.Delete(context.Background(), v.ID); err == nil {
		t.Fatal("delete reported success")
	}
	m.closeWatches()
	dead := newFakeHost()
	dead.next = h.next + 100
	m2, err := newManager(m.cfg, dead.run, dead)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m2.closeWatches)
	got, err := m2.get(v.ID)
	if err != nil || got.State != Error || got.PID != 0 {
		t.Fatalf("the half-deleted VM must stay (counted) as an error VM: %+v %v", got, err)
	}
	if _, err := m2.Start(context.Background(), v.ID, nil); err == nil {
		t.Fatal("a half-deleted VM was started after a restart")
	}
	unlock()
	if !m2.retryDelete(v.ID) {
		t.Fatal("retry did not finish the delete")
	}
	if _, err := m2.get(v.ID); err == nil || m2.tombstoned(v.ID) {
		t.Fatal("VM or tombstone survived the finished delete")
	}
}

// Only ENOSPC may cross the tombstone durability barrier. Faults are wrapped as
// real filesystem errors so the recovery must use errors.Is rather than ==.
func TestR38DeleteTombstoneErrors(t *testing.T) {
	for _, tc := range []struct {
		name         string
		first, retry error
	}{
		{"permission", syscall.EACCES, nil},
		{"io", syscall.EIO, nil},
		{"readonly", syscall.EROFS, nil},
		{"invalid_directory", syscall.ENOTDIR, nil},
		{"space_recovered", syscall.ENOSPC, nil},
		{"space_persistently_full", syscall.ENOSPC, syscall.ENOSPC},
		{"retry_io", syscall.ENOSPC, syscall.EIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, h := newTestManager(t, nil)
			ctx := context.Background()
			v, err := m.Create(ctx, CreateReq{Name: "full"})
			if err != nil {
				t.Fatal(err)
			}
			snap, err := m.CreateSnapshot(ctx, v.ID, "saved")
			if err != nil {
				t.Fatal(err)
			}
			eventually(t, "snapshot synced", func() bool {
				m.mu.Lock()
				defer m.mu.Unlock()
				return !m.snaps[snap.ID].Unsynced
			})
			payloads := []string{vmstate, memFile, diskFile, kernFile}
			for _, f := range payloads {
				path := filepath.Join(m.root(v.ID), f)
				if !exists(path) {
					if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			attempts, syncs := 0, 0
			m.syncFSFn = func(path string) error {
				syncs++
				if path != m.cfg.StateDir {
					t.Fatalf("sync filesystem: %s", path)
				}
				if h.Alive(v.PID, v.ID) {
					t.Fatal("runtime still alive at sync")
				}
				got, err := m.get(v.ID)
				if err != nil || got.State != Stopped || got.PID != 0 {
					t.Fatalf("teardown state: %+v %v", got, err)
				}
				if !exists(filepath.Join(m.vmDir(v.ID), "vm.json")) {
					t.Fatal("vm.json removed before retry")
				}
				for _, f := range payloads {
					if exists(filepath.Join(m.root(v.ID), f)) {
						t.Fatalf("payload %s survived before sync", f)
					}
				}
				if len(m.Snapshots(v.ID)) != 0 || exists(m.snapDir(snap.ID)) {
					t.Fatal("snapshot survived before sync")
				}
				return nil
			}
			m.tombstoneWriteFile = func(path string, body []byte, mode os.FileMode) error {
				attempts++
				fault := tc.first
				if attempts == 2 {
					if syncs != 1 {
						t.Fatal("retry happened before filesystem sync")
					}
					fault = tc.retry
				}
				if attempts > 2 {
					t.Fatal("more than one retry")
				}
				if fault != nil {
					return &os.PathError{Op: "open", Path: path, Err: fault}
				}
				return os.WriteFile(path, body, mode)
			}
			deleted, err := m.Delete(ctx, v.ID)
			if tc.first != syscall.ENOSPC {
				if !errors.Is(err, tc.first) {
					t.Fatalf("error = %v", err)
				}
				got, getErr := m.get(v.ID)
				if getErr != nil || got.State != Running || !h.Alive(v.PID, v.ID) {
					t.Fatalf("non-ENOSPC tore down VM: %+v %v", got, getErr)
				}
				for _, f := range payloads {
					if !exists(filepath.Join(m.root(v.ID), f)) {
						t.Fatalf("non-ENOSPC removed %s", f)
					}
				}
				if attempts != 1 || syncs != 0 || len(m.Snapshots(v.ID)) != 1 {
					t.Fatal("non-ENOSPC started recovery")
				}
				return
			}
			if attempts != 2 || syncs != 1 {
				t.Fatalf("attempts=%d syncs=%d", attempts, syncs)
			}
			if tc.retry != nil && tc.retry != syscall.ENOSPC {
				if !errors.Is(err, tc.retry) || !exists(filepath.Join(m.vmDir(v.ID), "vm.json")) {
					t.Fatalf("retry must fail closed: %v", err)
				}
				return
			}
			if err != nil || deleted.State != "deleted" {
				t.Fatalf("delete: %+v %v", deleted, err)
			}
			if exists(m.vmDir(v.ID)) || m.tombstoned(v.ID) {
				t.Fatal("VM dir or tombstone survived")
			}
			if _, err := m.get(v.ID); err == nil {
				t.Fatal("VM not forgotten")
			}
		})
	}
}
