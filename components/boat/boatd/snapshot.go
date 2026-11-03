package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Memory snapshots
//
// root/mem is the VM's memory as of its last snapshot point: the file a
// restored VM is mmapped from (MAP_PRIVATE, so never written), or absent after
// a cold boot (= all zeros; Firecracker tracks every page dirtied since boot,
// VMM writes included). With dirty-page tracking on, a snapshot is a Diff:
// Firecracker writes only the pages dirtied since that point into a sparse
// file, which is merged onto a reflink copy of root/mem (instant on btrfs) and
// renamed over it. root/mem is only ever replaced, never modified in place, so
// a running VM's backing file and its forks' copies stay intact.

const (
	snapState = "snap." + vmstate
	snapDiff  = "snap.diff"
	memNew    = "mem.new"
)

// takeSnapshot syncs the guest, pauses the VM and leaves root/snap.vmstate +
// an updated root/mem. The VM stays paused: callers resume (fork, snapshot)
// or kill (stop) it.
func (m *Manager) takeSnapshot(ctx context.Context, v VM) (VM, error) {
	root := m.root(v.ID)
	t0 := time.Now()
	m.guestSync(ctx, v)
	t1 := time.Now()
	fc := newFC(filepath.Join(root, apiSock))
	if err := fc.pause(ctx); err != nil {
		return v, err
	}
	t2 := time.Now()
	diff := m.cfg.DiffSnapshots && v.DiffOK
	os.Remove(filepath.Join(root, memNew))
	var err error
	if diff {
		err = fc.snapshot(ctx, "Diff", snapState, snapDiff)
	} else {
		err = fc.snapshot(ctx, "Full", snapState, memNew)
	}
	t3 := time.Now()
	written := allocatedBytes(filepath.Join(root, snapDiff)) + allocatedBytes(filepath.Join(root, memNew))
	if err == nil && diff {
		err = m.mergeDiff(v, filepath.Join(root, snapDiff), filepath.Join(root, memNew))
	}
	os.Remove(filepath.Join(root, snapDiff))
	if err == nil {
		err = os.Rename(filepath.Join(root, memNew), filepath.Join(root, memFile))
	}
	if err != nil {
		os.Remove(filepath.Join(root, memNew))
		os.Remove(filepath.Join(root, snapState))
		// Firecracker may have cleared its dirty bitmap: only a full snapshot is safe next.
		v, _ = m.update(v.ID, "", func(x *VM) { x.DiffOK = false })
		return v, err
	}
	log.Printf("%s: snapshot (%s) sync=%dms pause=%dms write=%dms merge=%dms written=%dMiB", v.ID, map[bool]string{true: "diff", false: "full"}[diff],
		t1.Sub(t0).Milliseconds(), t2.Sub(t1).Milliseconds(), t3.Sub(t2).Milliseconds(), time.Since(t3).Milliseconds(), written>>20)
	return m.update(v.ID, "", func(x *VM) { x.DiffOK = m.cfg.DiffSnapshots })
}

// mergeDiff makes dst the VM's full memory: the data extents of the sparse
// diff on a reflink copy of root/mem. Without a base (first snapshot since a
// cold boot) the holes are all zeros, so the diff already is the full memory.
func (m *Manager) mergeDiff(v VM, diff, dst string) error {
	base := filepath.Join(m.root(v.ID), memFile)
	if !exists(base) {
		if err := os.Rename(diff, dst); err != nil {
			return err
		}
		return os.Truncate(dst, int64(v.MemMiB)<<20)
	}
	if err := m.cp(base, dst); err != nil {
		return err
	}
	if err := os.Truncate(dst, int64(v.MemMiB)<<20); err != nil {
		return err
	}
	if err := copyExtents(diff, dst); err != nil {
		return err
	}
	uid, gid := m.ids(v)
	if uid != 0 {
		return os.Chown(dst, uid, gid)
	}
	return nil
}

// copyExtents copies every data region of src (SEEK_DATA/SEEK_HOLE) to the
// same offsets of dst. Without hole support it copies the whole file.
func copyExtents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	st, err := in.Stat()
	if err != nil {
		out.Close()
		return err
	}
	for off := int64(0); off < st.Size(); {
		data, err := in.Seek(off, seekData)
		if errors.Is(err, syscall.ENXIO) {
			break // no data after off
		}
		if err != nil {
			data = off // no SEEK_DATA: treat the rest as data
		}
		hole, err := in.Seek(data, seekHole)
		if err != nil {
			hole = st.Size()
		}
		if _, err := io.Copy(io.NewOffsetWriter(out, data), io.NewSectionReader(in, data, hole-data)); err != nil {
			out.Close()
			return err
		}
		off = hole
	}
	return out.Close()
}

// syncSnapshot fsyncs a stop snapshot and records that it is durable. Stop
// does not wait for it (it is most of a stop's cost on a slow disk); until the
// flag is set, reconcile treats the snapshot as lost in a host crash and the
// VM cold-boots from its disk, which Firecracker fsyncs itself when
// snapshotting.
func (m *Manager) syncSnapshot(id string) {
	root := m.root(id)
	for _, f := range []string{memFile, vmstate} {
		if err := fsyncFile(filepath.Join(root, f)); err != nil {
			return // restored or deleted meanwhile
		}
	}
	unlock := m.lock(id) // not racing Delete, which removes the dir vm.json lives in
	defer unlock()
	m.update(id, "", func(x *VM) { x.SnapSynced = x.HasSnapshot })
}

func allocatedBytes(path string) int64 {
	var st syscall.Stat_t
	if syscall.Stat(path, &st) != nil {
		return 0
	}
	return int64(st.Blocks) * 512
}

func fsyncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// syncSaved makes a saved snapshot's memory durable behind the API call: until
// snap.json says so, reconcile treats it as disk-only after a host crash.
func (m *Manager) syncSaved(s Snapshot) {
	dir := m.snapDir(s.ID)
	for _, f := range []string{memFile, vmstate} {
		if err := fsyncFile(filepath.Join(dir, f)); err != nil {
			return // deleted meanwhile
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.snaps[s.ID]; !ok {
		return
	}
	s.Unsynced = false
	if err := writeJSONAtomic(filepath.Join(dir, "snap.json"), s, 0o600); err != nil {
		log.Printf("%s: %v", s.ID, err)
		return
	}
	m.snaps[s.ID] = s
}

// guestSync flushes the guest's page cache to its disk before a pause, so the
// disk copy is consistent on its own (cold boot fallback, disk-only forks).
func (m *Manager) guestSync(ctx context.Context, v VM) {
	if !v.AgentReady {
		return
	}
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	sock := filepath.Join(m.root(v.ID), vsockUDS)
	var ae *agentErr
	if _, err := agentCall(c, sock, map[string]any{"op": "sync", "drop_caches": m.cfg.SnapshotDropCaches}); err == nil || !errors.As(err, &ae) {
		return
	}
	a, err := dialAgent(c, sock) // older agents: no "sync" op
	if err != nil {
		return
	}
	defer a.Close()
	if a.send(map[string]any{"op": "exec", "argv": []string{"sync"}, "user": "root", "timeout_s": 10}) != nil {
		return
	}
	for {
		if _, r, err := a.recv(); err != nil || r.final() {
			return
		}
	}
}

// freeze copies v's disk+kernel (+vmstate/mem) into dst. A running VM is
// paused only for the (diff) snapshot and the disk reflink.
func (m *Manager) freeze(ctx context.Context, v VM, dst string) (bool, error) {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return false, err
	}
	root := m.root(v.ID)
	if v.State != Running {
		files := []string{diskFile, kernFile}
		if v.HasSnapshot {
			files = append(files, vmstate, memFile)
		}
		for _, f := range files {
			if err := m.cp(filepath.Join(root, f), filepath.Join(dst, f)); err != nil {
				return false, err
			}
		}
		return v.HasSnapshot, nil
	}
	v, err := m.takeSnapshot(ctx, v)
	t0 := time.Now()
	if err == nil {
		err = m.cp(filepath.Join(root, diskFile), filepath.Join(dst, diskFile))
	}
	t1 := time.Now()
	if rerr := newFC(filepath.Join(root, apiSock)).resume(context.WithoutCancel(ctx)); rerr != nil {
		log.Printf("%s: resume after snapshot: %v", v.ID, rerr)
	}
	// The rest is immutable (root/mem is only ever renamed over): no need to stay paused.
	for _, f := range []string{kernFile, memFile, snapState} {
		if err == nil {
			target := f
			if f == snapState {
				target = vmstate
			}
			err = m.cp(filepath.Join(root, f), filepath.Join(dst, target)) // VM dirs may be subvolumes: no rename
		}
	}
	os.Remove(filepath.Join(root, snapState))
	log.Printf("%s: freeze disk=%dms copies=%dms", v.ID, t1.Sub(t0).Milliseconds(), time.Since(t1).Milliseconds())
	return err == nil, err
}

func (m *Manager) Snapshots(vmID string) []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Snapshot{}
	for _, s := range m.snaps {
		if s.VMID == vmID {
			out = append(out, s)
		}
	}
	return out
}

func (m *Manager) CreateSnapshot(ctx context.Context, vmID, name string) (Snapshot, error) {
	if len(name) > 128 {
		return Snapshot{}, errf(400, "name too long")
	}
	unlock := m.lock(vmID)
	defer unlock()
	v, err := m.get(vmID)
	if err != nil {
		return Snapshot{}, err
	}
	if v.State != Running && v.State != Stopped {
		return Snapshot{}, errf(http.StatusConflict, "vm is %s", v.State)
	}
	s := Snapshot{ID: newID("snap-"), Name: name, VMID: v.ID, Team: v.Team, Image: v.Image, Size: v.Size,
		VCPU: v.VCPU, MemMiB: v.MemMiB, DiskGiB: v.DiskGiB, CreatedAt: m.now(), HasMem: v.State == Running || v.HasSnapshot}
	m.mu.Lock()
	err = m.storageQuotaLocked(v.Team, 0, 1, snapCommit(s))
	m.mu.Unlock()
	if err == nil {
		err = m.checkFloor(uint64(v.MemMiB) << 20)
	}
	if err != nil {
		return Snapshot{}, err
	}
	dir := m.snapDir(s.ID)
	s.HasMem, err = m.freeze(ctx, v, dir)
	s.Unsynced = s.HasMem
	if err == nil {
		err = writeJSONAtomic(filepath.Join(dir, "snap.json"), s, 0o600)
	}
	if err != nil {
		os.RemoveAll(dir)
		return Snapshot{}, err
	}
	m.mu.Lock()
	m.snaps[s.ID] = s
	m.mu.Unlock()
	if s.HasMem {
		go m.syncSaved(s)
	}
	m.ev.publish(Event{Type: "snapshot.created", VMID: v.ID, SnapID: s.ID, Team: v.Team})
	return s, nil
}

func (m *Manager) DeleteSnapshot(vmID, sid string) error {
	m.mu.Lock()
	s, ok := m.snaps[sid]
	if ok && s.VMID == vmID {
		delete(m.snaps, sid)
	}
	m.mu.Unlock()
	if !ok || s.VMID != vmID {
		return errf(404, "snapshot not found")
	}
	if err := m.usage.append(usageRec{"snapshot", s.ID, s.Team, s.Size, s.CreatedAt, m.now()}); err != nil {
		log.Printf("usage: %v", err)
	}
	m.ev.publish(Event{Type: "snapshot.deleted", VMID: vmID, SnapID: sid, Team: s.Team})
	return os.RemoveAll(m.snapDir(sid))
}

// ---- storage: committed bytes, free-space floor, accounting ----

// vmCommit: disk + mem file + the capped log and its rotated tail.
func vmCommit(v VM) int64 {
	return int64(v.DiskGiB)<<30 + int64(v.MemMiB)<<20 + logCapBytes + logTailBytes
}

func snapCommit(s Snapshot) int64 {
	b := int64(s.DiskGiB) << 30
	if s.HasMem {
		b += int64(s.MemMiB) << 20
	}
	return b
}

// storageQuotaLocked: total VMs (any state), snapshot count, committed bytes.
func (m *Manager) storageQuotaLocked(team string, addVMs, addSnaps int, addBytes int64) error {
	p := m.cfg.team(team)
	vms, snaps, bytes := addVMs, addSnaps, addBytes
	for _, v := range m.vms {
		if v.Team == team {
			vms, bytes = vms+1, bytes+vmCommit(v)
		}
	}
	for _, s := range m.snaps {
		if s.Team == team {
			snaps, bytes = snaps+1, bytes+snapCommit(s)
		}
	}
	switch {
	case p.MaxTotalVMs > 0 && addVMs > 0 && vms > p.MaxTotalVMs:
		return errf(http.StatusTooManyRequests, "quota: team %s may keep %d VMs", team, p.MaxTotalVMs)
	case p.MaxSnapshots > 0 && addSnaps > 0 && snaps > p.MaxSnapshots:
		return errf(http.StatusTooManyRequests, "quota: team %s may keep %d snapshots", team, p.MaxSnapshots)
	case p.MaxDiskGiB > 0 && bytes > int64(p.MaxDiskGiB)<<30:
		return errf(http.StatusTooManyRequests, "quota: team %s may commit %d GiB of storage", team, p.MaxDiskGiB)
	}
	return nil
}

type diskSpace struct {
	Free  uint64 `json:"free_bytes"`
	Total uint64 `json:"total_bytes"`
	Floor uint64 `json:"floor_bytes"`
}

func statfs(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize), nil
}

func (m *Manager) space() (diskSpace, error) {
	free, total, err := m.statfs(m.cfg.StateDir)
	floor := max(uint64(m.cfg.MinFreeGiB)<<30, total/100*uint64(m.cfg.MinFreePct))
	return diskSpace{free, total, floor}, err
}

// checkFloor refuses work that would take stateDir below its free-space floor.
func (m *Manager) checkFloor(need uint64) error {
	d, err := m.space()
	if err != nil {
		return err
	}
	if d.Free < d.Floor+need {
		return errf(http.StatusInsufficientStorage, "host disk: %d GiB free, floor %d GiB (+%d MiB needed)", d.Free>>30, d.Floor>>30, need>>20)
	}
	return nil
}

// dirBytes: apparent and allocated bytes of the files under dir. Reflinked
// extents are counted once per file (an upper bound on real use).
func dirBytes(dir string) (apparent, allocated int64) {
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err != nil || !fi.Mode().IsRegular() {
			return nil
		}
		apparent += fi.Size()
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			allocated += int64(st.Blocks) * 512
		}
		return nil
	})
	return
}

type teamStorage struct {
	Team              string `json:"team"`
	VMs               int    `json:"vms"`
	Snapshots         int    `json:"snapshots"`
	CommittedBytes    int64  `json:"committed_bytes"`
	VMApparentBytes   int64  `json:"vm_apparent_bytes"`
	VMAllocatedBytes  int64  `json:"vm_allocated_bytes"`
	SnapApparentBytes int64  `json:"snapshot_apparent_bytes"`
	SnapAllocBytes    int64  `json:"snapshot_allocated_bytes"`
}

// ponytail: walks every VM/snapshot dir per call; cache if usage is polled often.
func (m *Manager) storage() (map[string]*teamStorage, diskSpace) {
	m.mu.Lock()
	vms := make([]VM, 0, len(m.vms))
	for _, v := range m.vms {
		vms = append(vms, v)
	}
	snaps := make([]Snapshot, 0, len(m.snaps))
	for _, s := range m.snaps {
		snaps = append(snaps, s)
	}
	m.mu.Unlock()
	out := map[string]*teamStorage{}
	team := func(t string) *teamStorage {
		if out[t] == nil {
			out[t] = &teamStorage{Team: t}
		}
		return out[t]
	}
	for _, v := range vms {
		t := team(v.Team)
		a, b := dirBytes(m.vmDir(v.ID))
		t.VMs, t.CommittedBytes, t.VMApparentBytes, t.VMAllocatedBytes = t.VMs+1, t.CommittedBytes+vmCommit(v), t.VMApparentBytes+a, t.VMAllocatedBytes+b
	}
	for _, s := range snaps {
		t := team(s.Team)
		a, b := dirBytes(m.snapDir(s.ID))
		t.Snapshots, t.CommittedBytes, t.SnapApparentBytes, t.SnapAllocBytes = t.Snapshots+1, t.CommittedBytes+snapCommit(s), t.SnapApparentBytes+a, t.SnapAllocBytes+b
	}
	d, _ := m.space()
	return out, d
}
