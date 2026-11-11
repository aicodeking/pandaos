package main

import (
	"context"
	"errors"
	"fmt"
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
	memStage  = "mem.stage" // in the VM dir, beyond the VMM: the memory file being built
)

// takeSnapshot syncs the guest, pauses the VM and leaves root/snap.vmstate +
// an updated root/mem. The VM stays paused: callers resume (fork, snapshot)
// or kill (stop) it.
func (m *Manager) takeSnapshot(ctx context.Context, v VM, fsync bool) (VM, error) {
	diff, err := m.dumpSnapshot(ctx, v, fsync)
	if err != nil {
		return v, err
	}
	return m.installSnapshot(v, diff)
}

// dumpSnapshot leaves the VM paused and root/snap.vmstate + its memory (a sparse
// diff if the return is true) written, not yet installed as root/mem.
func (m *Manager) dumpSnapshot(ctx context.Context, v VM, fsync bool) (bool, error) {
	root := m.root(v.ID)
	t0 := time.Now()
	m.guestSync(ctx, v)
	t1 := time.Now()
	fc := newFC(filepath.Join(root, apiSock))
	if err := fc.pause(ctx); err != nil {
		return false, err
	}
	t2 := time.Now()
	diff := m.cfg.DiffSnapshots && v.DiffOK
	os.Remove(filepath.Join(root, memNew))
	m.liftMemLimits(v)
	var err error
	if diff {
		err = fc.snapshot(ctx, "Diff", snapState, snapDiff, fsync)
	} else {
		err = fc.snapshot(ctx, "Full", snapState, memNew, fsync)
	}
	if err != nil {
		m.setMemLimits(v)
		m.failSnapshot(v)
		return false, err
	}
	written := allocatedBytes(filepath.Join(root, snapDiff)) + allocatedBytes(filepath.Join(root, memNew))
	log.Printf("%s: snapshot (%s) sync=%dms pause=%dms write=%dms written=%dMiB", v.ID, map[bool]string{true: "diff", false: "full"}[diff],
		t1.Sub(t0).Milliseconds(), t2.Sub(t1).Milliseconds(), time.Since(t2).Milliseconds(), written>>20)
	return diff, nil
}

// failSnapshot drops a partial snapshot. Firecracker may have cleared its dirty
// bitmap: only a full snapshot is safe next.
func (m *Manager) failSnapshot(v VM) VM {
	root := m.root(v.ID)
	for _, f := range []string{snapDiff, memNew, snapState} {
		os.Remove(filepath.Join(root, f))
	}
	os.Remove(filepath.Join(m.vmDir(v.ID), memStage))
	nv, _ := m.update(v.ID, "", func(x *VM) { x.DiffOK = false })
	return nv
}

// installSnapshot makes what dumpSnapshot wrote root/mem. The VM may be running:
// root/mem is only replaced, and this is where a dirty base gets flushed (a reflink
// waits for its source's writeback), which is no reason to keep the guest paused.
func (m *Manager) installSnapshot(v VM, diff bool) (VM, error) {
	root, stage := m.root(v.ID), filepath.Join(m.vmDir(v.ID), memStage)
	t0 := time.Now()
	os.Remove(stage)
	var err error
	if diff {
		err = m.mergeDiff(v, filepath.Join(root, snapDiff), stage)
	} else {
		err = os.Rename(filepath.Join(root, memNew), stage) // out of the VMM's reach; a planted link fails below
	}
	os.Remove(filepath.Join(root, snapDiff))
	if err == nil {
		err = sealMem(stage)
	}
	if err == nil {
		err = os.Rename(stage, filepath.Join(root, memFile))
	}
	if err != nil {
		return m.failSnapshot(v), err
	}
	log.Printf("%s: snapshot install merge=%dms", v.ID, time.Since(t0).Milliseconds())
	if v.DiffOK == m.cfg.DiffSnapshots {
		return v, nil // vm.json's fsync took 4.3 s with the dump in writeback, under the global lock
	}
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
		return truncateJail(dst, int64(v.MemMiB)<<20)
	}
	t0 := time.Now()
	if err := m.cp(base, dst); err != nil {
		return err
	}
	if err := os.Chmod(dst, 0o600); err != nil { // the sealed base's mode came along
		return err
	}
	if err := truncateJail(dst, int64(v.MemMiB)<<20); err != nil {
		return err
	}
	t1 := time.Now()
	// SEEK_DATA walks a dirty file's delalloc: 1-2.5 s for a 17 MiB diff. Firecracker did not
	// fsync it (the guest was paused then), and it is only ever a few MiB when a base exists.
	if err := fsyncFile(diff); err != nil {
		return err
	}
	t2 := time.Now()
	err := copyExtents(diff, dst)
	log.Printf("%s: merge reflink=%dms flush=%dms copy=%dms", v.ID, t1.Sub(t0).Milliseconds(), t2.Sub(t1).Milliseconds(), time.Since(t2).Milliseconds())
	return err
}

// sealMem: memory files are immutable, owned by root and world-readable, because
// VMs of one team hard-link them (see copyMem). Only the VM's chroot reaches one.
func sealMem(path string) error {
	f, err := openJail(path, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	return sealFile(f)
}

// copyMem gives dst the memory file src. Within a team that is a hard link: no
// flush of a dirty file (a reflink of 3.8 GiB waits for the disk, 15 s on the CI
// runner) and no space. Across teams it must stay a clone: one inode means shared
// host page cache, which is a page-sharing side channel between guests.
func (m *Manager) copyMem(src, dst string, sameTeam bool) error {
	if sameTeam {
		f, err := openJail(src, os.O_RDONLY)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := sealFile(f); err != nil {
			return err
		}
		os.Remove(dst)
		if linkFile(f, dst) == nil { // EXDEV for a warm template: it is outside the team's subvolume
			return nil
		}
	}
	return m.cp(src, dst)
}

// copyExtents overlays the data regions of src on dst.
func copyExtents(src, dst string) error {
	in, err := openJail(src, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := openJail(dst, os.O_WRONLY)
	if err != nil {
		return err
	}
	defer out.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	return copySparse(in, out, st.Size())
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

// settleAfterDump starts the base's writeback now, so that the next merge finds
// it clean, then gives the VM its memory limits back.
func (m *Manager) settleAfterDump(v VM) {
	if err := fsyncFile(filepath.Join(m.root(v.ID), memFile)); err != nil {
		log.Printf("%s: flush mem: %v", v.ID, err)
	}
	m.setMemLimits(v)
}

func allocatedBytes(path string) int64 {
	var st syscall.Stat_t
	if syscall.Lstat(path, &st) != nil {
		return 0
	}
	return int64(st.Blocks) * 512
}

func fsyncFile(path string) error {
	f, err := openJail(path, os.O_RDONLY)
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
	s.Unsynced = false
	m.snapDirMu.Lock()
	defer m.snapDirMu.Unlock()
	m.mu.Lock()
	_, ok := m.snaps[s.ID]
	m.mu.Unlock()
	if !ok {
		return // deleted while its files were synced: the dir is gone or going
	}
	if err := writeJSONAtomic(filepath.Join(dir, "snap.json"), s, 0o600); err != nil {
		log.Printf("%s: %v", s.ID, err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.snaps[s.ID]; ok {
		m.snaps[s.ID] = s
	}
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
	if _, err := agentCall(c, sock, map[string]string{"op": "sync"}); err == nil || !errors.As(err, &ae) {
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
			var err error
			if f == memFile {
				err = m.copyMem(filepath.Join(root, f), filepath.Join(dst, f), true)
			} else {
				err = m.cp(filepath.Join(root, f), filepath.Join(dst, f))
			}
			if err != nil {
				return false, err
			}
		}
		return v.HasSnapshot, nil
	}
	diff, err := m.dumpSnapshot(ctx, v, false)
	t0 := time.Now()
	if err == nil {
		err = m.cp(filepath.Join(root, diskFile), filepath.Join(dst, diskFile))
		if err != nil {
			m.failSnapshot(v)
		}
	}
	t1 := time.Now()
	if rerr := resumeAfterSnapshot(context.WithoutCancel(ctx), newFC(filepath.Join(root, apiSock))); rerr != nil {
		log.Printf("%s: resume after snapshot: %v", v.ID, rerr)
		if err == nil {
			m.failSnapshot(v)
			err = fmt.Errorf("vm left paused: resume after snapshot: %w", rerr)
		}
	}
	m.resets.fire(v.ID) // the snapshot reset the guest's vsock connections, now it runs again
	if err != nil {
		m.setMemLimits(v)
		return false, err
	}
	// The rest is immutable (root/mem is only ever renamed over): no need to stay paused.
	_, err = m.installSnapshot(v, diff)
	if err == nil {
		err = m.cp(filepath.Join(root, kernFile), filepath.Join(dst, kernFile))
	}
	if err == nil {
		err = m.copyMem(filepath.Join(root, memFile), filepath.Join(dst, memFile), true)
	}
	if err == nil {
		err = m.cp(filepath.Join(root, snapState), filepath.Join(dst, vmstate)) // VM dirs may be subvolumes: no rename
	}
	os.Remove(filepath.Join(root, snapState))
	go m.settleAfterDump(v)
	log.Printf("%s: freeze disk=%dms rest=%dms", v.ID, t1.Sub(t0).Milliseconds(), time.Since(t1).Milliseconds())
	return err == nil, err
}

// resumeRetries: a paused source runs nothing and answers no exec, so a resume
// the VMM refuses for a moment is worth a few more tries.
var resumeRetries = []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 2 * time.Second}

func resumeAfterSnapshot(ctx context.Context, fc *fcClient) error {
	err := fc.resume(ctx)
	for _, d := range resumeRetries {
		if err == nil {
			return nil
		}
		time.Sleep(d)
		err = fc.resume(ctx)
	}
	return err
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
	res, err := m.reserve(v.Team, 1, snapCommit(s)) // held while the dump runs, other VMs of the team included
	if err != nil {
		return Snapshot{}, err
	}
	defer res.release()
	if err = m.checkFloor(uint64(v.MemMiB) << 20); err != nil {
		return Snapshot{}, err
	}
	if err = m.makeStoreDir("snapshots", v.Team, s.ID); err != nil {
		return Snapshot{}, err
	}
	dir := m.snapDir(s.ID)
	s.HasMem, err = m.freeze(ctx, v, dir)
	s.Unsynced = s.HasMem
	if err == nil {
		// No fsync here: 3-4 s behind a dump in writeback. syncSaved persists it.
		err = writeJSONNoSync(filepath.Join(dir, "snap.json"), s, 0o600)
	}
	if err != nil {
		m.removeStoreDir(dir)
		return Snapshot{}, err
	}
	m.mu.Lock()
	res.releaseLocked()
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
	m.snapDirMu.Lock() // after a syncSaved still writing snap.json; it sees the delete and stops
	defer m.snapDirMu.Unlock()
	return m.removeStoreDir(m.snapDir(sid))
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

// reservation holds quota for storage still being written, so that concurrent
// operations of one team, on different VMs, cannot all pass the same check.
type reservation struct {
	m     *Manager
	team  string
	snaps int
	bytes int64
}

func (m *Manager) reserve(team string, snaps int, bytes int64) (*reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.storageQuotaLocked(team, 0, snaps, bytes); err != nil {
		return nil, err
	}
	m.reservedSnaps[team] += snaps
	m.reservedBytes[team] += bytes
	return &reservation{m, team, snaps, bytes}, nil
}

// grow charges n more bytes, within quota.
func (r *reservation) grow(n int64) error { return r.set(r.bytes + n) }

// set replaces the charged bytes with n, within quota.
func (r *reservation) set(n int64) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if err := r.m.storageQuotaLocked(r.team, 0, 0, n-r.bytes); err != nil {
		return err
	}
	r.m.reservedBytes[r.team] += n - r.bytes
	r.bytes = n
	return nil
}

// releaseLocked: in the critical section that adds what was reserved to m.snaps.
func (r *reservation) releaseLocked() {
	r.m.reservedSnaps[r.team] -= r.snaps
	r.m.reservedBytes[r.team] -= r.bytes
	r.snaps, r.bytes = 0, 0
}

func (r *reservation) release() {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	r.releaseLocked()
}

// storageQuotaLocked: total VMs (any state), snapshot count, committed bytes, for the
// team and for the host. Committed bytes are a true upper bound on what a VM or
// snapshot can occupy, so at storageOvercommit 1 no tenant can fill the disk at all.
func (m *Manager) storageQuotaLocked(team string, addVMs, addSnaps int, addBytes int64) error {
	p := m.cfg.team(team)
	vms, snaps, bytes := addVMs, addSnaps+m.reservedSnaps[team], addBytes+m.reservedBytes[team]
	host := addBytes
	for _, v := range m.vms {
		if v.Team == team {
			vms, bytes = vms+1, bytes+vmCommit(v)
		}
		host += vmCommit(v)
	}
	for _, s := range m.snaps {
		if s.Team == team {
			snaps, bytes = snaps+1, bytes+snapCommit(s)
		}
		host += snapCommit(s)
	}
	for _, b := range m.reservedBytes {
		host += b
	}
	if err := m.hostStorageCheck(host, addBytes); err != nil {
		return err
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

// hostStorageCheck caps what all teams together may commit. Only growth is refused:
// a host already past the cap (a lowered setting) can still stop, delete and shrink.
func (m *Manager) hostStorageCheck(committed, adding int64) error {
	if m.cfg.StorageOvercommit <= 0 || adding <= 0 {
		return nil
	}
	_, total, err := m.statfs(m.cfg.StateDir)
	if err != nil || total == 0 {
		return nil // checkFloor reports a statfs failure
	}
	if limit := float64(total) * m.cfg.StorageOvercommit; float64(committed) > limit {
		return errf(http.StatusInsufficientStorage, "host disk: %d GiB committed would pass the limit of %.0f GiB", committed>>30, limit/(1<<30))
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
	if real, err := filepath.EvalSymlinks(dir); err == nil { // Walk does not follow its root (makeStoreDir)
		dir = real
	}
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
