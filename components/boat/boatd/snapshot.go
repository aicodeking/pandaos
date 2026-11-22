package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	diff, _, err := m.dumpSnapshot(ctx, v, fsync)
	if err != nil {
		return v, err
	}
	nv, err := m.installSnapshot(v, diff, fsync)
	if err != nil {
		m.setMemLimits(v)
	}
	return nv, err
}

// dumpSnapshot leaves the VM paused and root/snap.vmstate + its memory (a sparse
// diff if the return is true) written, not yet installed as root/mem. The generation
// owns cleanup of its lifted limits; zero means no dump remains to clean up.
func (m *Manager) dumpSnapshot(ctx context.Context, v VM, fsync bool) (bool, uint64, error) {
	root := m.root(v.ID)
	t0 := time.Now()
	m.guestSync(ctx, v)
	t1 := time.Now()
	releaseDump, err := m.acquireDumpSlot(ctx)
	if err != nil {
		return false, 0, err
	}
	defer releaseDump()
	fc := newFC(filepath.Join(root, apiSock))
	if err := fc.pause(ctx); err != nil {
		return false, 0, err
	}
	t2 := time.Now()
	diff := m.cfg.DiffSnapshots && v.DiffOK
	noCompress(root) // a VM from before makeStoreDir set it
	os.Remove(filepath.Join(root, memNew))
	generation := m.liftMemLimits(v)
	memPath, typ := memNew, "Full"
	if diff {
		memPath, typ = snapDiff, "Diff"
	}
	stopWB := func() {}
	if !fsync {
		stopWB = m.startWriteback(filepath.Join(root, memPath))
	}
	log.Printf("%s: dump-write generation=%d phase=start unix_ms=%d", v.ID, generation, time.Now().UnixMilli())
	err = fc.snapshot(ctx, typ, snapState, memPath, fsync)
	log.Printf("%s: dump-write generation=%d phase=end unix_ms=%d error=%v", v.ID, generation, time.Now().UnixMilli(), err)
	stopWB()
	closeAgentConns(filepath.Join(root, vsockUDS)) // a snapshot resets the guest's vsock conns
	if err != nil {
		m.setMemLimits(v)
		m.failSnapshot(v)
		return false, 0, err
	}
	written := allocatedBytes(filepath.Join(root, snapDiff)) + allocatedBytes(filepath.Join(root, memNew))
	log.Printf("%s: snapshot (%s) sync=%dms pause=%dms write=%dms written=%dMiB", v.ID, map[bool]string{true: "diff", false: "full"}[diff],
		t1.Sub(t0).Milliseconds(), t2.Sub(t1).Milliseconds(), time.Since(t2).Milliseconds(), written>>20)
	return diff, generation, nil
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
func (m *Manager) installSnapshot(v VM, diff, diffSynced bool) (VM, error) {
	root, stage := m.root(v.ID), filepath.Join(m.vmDir(v.ID), memStage)
	t0 := time.Now()
	os.Remove(stage)
	var err error
	if diff {
		err = m.mergeDiff(v, filepath.Join(root, snapDiff), stage, diffSynced)
	} else {
		err = os.Rename(filepath.Join(root, memNew), stage) // out of the VMM's reach; a planted link fails below
	}
	os.Remove(filepath.Join(root, snapDiff))
	if err == nil {
		err = sealMem(stage)
	}
	if err == nil {
		err = replaceMem(stage, filepath.Join(root, memFile))
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

// replaceMem renames stage over mem. Unlinking the last name of an 8 GiB memory file
// frees its extents in the caller, which took about 0.6 s of a fork's freeze on the CI
// runner (a fork's "install merge" past its merge, when the previous fork that shared
// the file was gone). An open descriptor to the old file moves that to its Close.
func replaceMem(stage, mem string) error {
	old, err := openJail(mem, os.O_RDONLY)
	if err == nil {
		defer func() { go old.Close() }()
	}
	return os.Rename(stage, mem)
}

// mergeDiff makes dst the VM's full memory: the data extents of the sparse
// diff on a reflink copy of root/mem. Without a base (first snapshot since a
// cold boot) the holes are all zeros, so the diff already is the full memory.
func (m *Manager) mergeDiff(v VM, diff, dst string, diffSynced bool) error {
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
	// fsync it (the guest was paused then). A fork's diff is a few MiB; a second named
	// snapshot's can be GiBs, and once flushed its extents are cloned below, not written
	// again (boat-e2e r11: a 2 GiB diff's copy took 2.3-10.5 s on top of its 1.8-11.4 s
	// flush, and a create meanwhile waited up to 12.6 s for its guest's disk).
	if !diffSynced {
		if m.isBooting() {
			m.waitBootQuiet(mergeDiffBootWait)
		}
		if err := fsyncFile(diff); err != nil {
			return err
		}
	}
	t2 := time.Now()
	err := m.copyExtents(diff, dst)
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
	return copyExtentsPaced(src, dst, nil)
}

func (m *Manager) copyExtents(src, dst string) error {
	return copyExtentsPaced(src, dst, m.paceDumpCopy)
}

func copyExtentsPaced(src, dst string, pace func(int64)) error {
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
	return copyDataPaced(in, out, st.Size(), true, pace)
}

type snapSyncCall struct {
	done chan struct{}
}

// startSyncSnapshot ensures a background syncSnapshot is running for id,
// registering it in m.snapSyncs before returning so concurrent callers (such as
// stopAll) observe and wait on it instead of duplicating fsyncs.
func (m *Manager) startSyncSnapshot(id string) {
	m.mu.Lock()
	v, ok := m.vms[id]
	if !ok || !v.HasSnapshot || v.SnapSynced {
		m.mu.Unlock()
		return
	}
	if inFlight := m.snapSyncs[id]; inFlight != nil {
		m.mu.Unlock()
		// An earlier snapshot's sync is still finishing; wait for it and re-check
		// so a replaced snapshot (Stop -> Start -> Stop) is never left unsynced.
		go func() {
			<-inFlight.done
			m.syncSnapshot(id)
		}()
		return
	}
	if m.snapSyncs == nil {
		m.snapSyncs = map[string]*snapSyncCall{}
	}
	call := &snapSyncCall{done: make(chan struct{})}
	m.snapSyncs[id] = call
	syncFn := m.fsyncSnapFile
	m.mu.Unlock()
	go m.finishSyncSnapshot(id, call, syncFn)
}

// waitSnapSyncs waits for any already in-flight background syncSnapshot calls
// to complete (or ctx cancellation) without starting new fsyncs.
func (m *Manager) waitSnapSyncs(ctx context.Context) {
	m.mu.Lock()
	dones := make([]<-chan struct{}, 0, len(m.snapSyncs))
	for _, call := range m.snapSyncs {
		dones = append(dones, call.done)
	}
	m.mu.Unlock()
	for _, ch := range dones {
		select {
		case <-ch:
		case <-ctx.Done():
			return
		}
	}
}

// syncSnapshot fsyncs a stop snapshot and records that it is durable. Stop
// does not wait for it (it is most of a stop's cost on a slow disk); until the
// flag is set, reconcile treats the snapshot as lost in a host crash and the
// VM cold-boots from its disk, which Firecracker fsyncs itself when
// snapshotting. Calls for the same VM are deduplicated so a shutdown sync
// joins an in-flight background sync or skips an already-synced snapshot
// rather than fsyncing the same files twice.
func (m *Manager) syncSnapshot(id string) {
	for {
		m.mu.Lock()
		v, ok := m.vms[id]
		if !ok || !v.HasSnapshot || v.SnapSynced {
			m.mu.Unlock()
			return
		}
		if inFlight := m.snapSyncs[id]; inFlight != nil {
			m.mu.Unlock()
			<-inFlight.done
			continue
		}
		if m.snapSyncs == nil {
			m.snapSyncs = map[string]*snapSyncCall{}
		}
		call := &snapSyncCall{done: make(chan struct{})}
		m.snapSyncs[id] = call
		syncFn := m.fsyncSnapFile
		m.mu.Unlock()

		m.finishSyncSnapshot(id, call, syncFn)
		return
	}
}

func (m *Manager) finishSyncSnapshot(id string, call *snapSyncCall, syncFn func(string) (os.FileInfo, error)) {
	defer func() {
		m.mu.Lock()
		delete(m.snapSyncs, id)
		close(call.done)
		m.mu.Unlock()
	}()
	m.runSyncSnapshot(id, syncFn)
}

func (m *Manager) runSyncSnapshot(id string, syncFn func(string) (os.FileInfo, error)) {
	if syncFn == nil {
		syncFn = fsyncFileInfo
	}
	m.waitBootQuiet(postDumpSyncBootWait)
	root := m.root(id)
	var infos [2]os.FileInfo
	for i, f := range []string{memFile, vmstate} {
		m.waitBootQuiet(postDumpSyncBootWait)
		info, err := syncFn(filepath.Join(root, f))
		if err != nil {
			return // restored or deleted meanwhile
		}
		infos[i] = info
	}
	unlock := m.lock(id) // not racing Delete, which removes the dir vm.json lives in
	defer unlock()
	for i, f := range []string{memFile, vmstate} {
		st, err := os.Lstat(filepath.Join(root, f))
		if err != nil || !os.SameFile(infos[i], st) {
			return // replaced or removed while syncing
		}
	}
	m.update(id, "", func(x *VM) { x.SnapSynced = x.HasSnapshot })
}

// settleAfterDump starts the base's writeback now, so that the next merge finds
// it clean, then gives the VM its memory limits back only if this dump still owns them.
func (m *Manager) settleAfterDump(v VM, generation uint64) {
	m.waitBootQuietWhile(postDumpSyncBootWait, func() bool {
		return m.dumpGeneration(v.ID) == generation
	})
	if m.dumpGeneration(v.ID) == generation {
		if err := fsyncFile(filepath.Join(m.root(v.ID), memFile)); err != nil {
			log.Printf("%s: flush mem: %v", v.ID, err)
		}
	}
	unlock := m.lock(v.ID) // not lowering the limits under the next dump's lift
	defer unlock()
	if m.dumpGeneration(v.ID) == generation {
		m.setMemLimits(v)
	}
}

func allocatedBytes(path string) int64 {
	var st syscall.Stat_t
	if syscall.Lstat(path, &st) != nil {
		return 0
	}
	return int64(st.Blocks) * 512
}

func fsyncFile(path string) error {
	_, err := fsyncFileInfo(path)
	return err
}

func fsyncFileInfo(path string) (os.FileInfo, error) {
	f, err := openJail(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return nil, err
	}
	return f.Stat()
}

// syncSaved makes a saved snapshot's memory durable behind the API call: until
// snap.json says so, reconcile treats it as disk-only after a host crash.
func (m *Manager) syncSaved(s Snapshot) {
	m.waitBootQuiet(postDumpSyncBootWait)
	dir := m.snapDir(s.ID)
	for _, f := range []string{memFile, vmstate} {
		m.waitBootQuiet(postDumpSyncBootWait)
		if err := fsyncFile(filepath.Join(dir, f)); err != nil {
			return // deleted meanwhile
		}
	}
	m.snapDirMu.Lock()
	defer m.snapDirMu.Unlock()
	m.mu.Lock()
	cur, ok := m.snaps[s.ID]
	m.mu.Unlock()
	if !ok {
		return // deleted while its files were synced: the dir is gone or going
	}
	s = cur // release may have changed PendingCopy since the sync started
	s.Unsynced = false
	if err := writeJSONAtomic(filepath.Join(dir, "snap.json"), s, 0o600); err != nil {
		log.Printf("%s: %v", s.ID, err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.snaps[s.ID]; ok {
		m.snaps[s.ID] = s
		m.snapsChangedLocked()
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
// paused only for the (diff) snapshot and the disk reflink. When reprovisionSSH
// is true, the caller (boat-api) is rotating SSH credentials right after prepare,
// so the source VM stays quarantined after resuming until its new policy is installed.
func (m *Manager) freeze(ctx context.Context, v VM, dst string, reprovisionSSH bool) (bool, error) {
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
	// Same VMM continuity keeps its epoch; ingress is fenced across the pause.
	// When boat-api will immediately rotate credentials on an already-ready VM
	// (reprovisionSSH), use the transient SSH-only pause-fence so live parent
	// sessions stay in routes.json and installSSHPolicy takes the rotate-token
	// fast path with 0 full nft reloads. Unready VMs always take full quarantine.
	pauseFence := reprovisionSSH && v.SSHPolicyReady && len(v.sshConfig) > 0 && v.sshHelperDigest != ""
	if pauseFence {
		if err := m.quarantineSSHPause(v); err != nil {
			return false, err
		}
	} else {
		if err := m.quarantineSSH(v); err != nil {
			return false, err
		}
	}
	diff, generation, err := m.dumpSnapshot(ctx, v, false)
	t0 := time.Now()
	if err == nil {
		err = m.cp(filepath.Join(root, diskFile), filepath.Join(dst, diskFile))
		if err != nil {
			m.failSnapshot(v)
		}
	}
	t1 := time.Now()
	rerr := resumeAfterSnapshot(context.WithoutCancel(ctx), newFC(filepath.Join(root, apiSock)))
	resumeFailed := rerr != nil
	if resumeFailed && pauseFence {
		if qerr := m.quarantineSSH(v); qerr != nil {
			log.Printf("%s: full quarantine after failed resume: %v", v.ID, qerr)
		}
	}
	if rerr == nil && !reprovisionSSH {
		rerr = m.restoreSSHAfterPause(context.WithoutCancel(ctx), v)
	}
	if rerr != nil {
		log.Printf("%s: resume after snapshot: %v", v.ID, rerr)
		if err == nil {
			m.failSnapshot(v)
			if resumeFailed {
				err = fmt.Errorf("vm left paused: resume after snapshot: %w", rerr)
			} else {
				err = fmt.Errorf("SSH policy verification after snapshot: %w", rerr)
			}
		}
	}
	m.resets.fire(v.ID) // the snapshot reset the guest's vsock connections, now it runs again
	if err != nil {
		if generation != 0 {
			m.setMemLimits(v)
		}
		return false, err
	}
	// The rest is immutable (root/mem is only ever renamed over): no need to stay paused.
	_, err = m.installSnapshot(v, diff, false)
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
	go m.settleAfterDump(v, generation)
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
	return m.createSnapshot(ctx, vmID, name, false)
}

func (m *Manager) createSnapshot(ctx context.Context, vmID, name string, pending bool) (Snapshot, error) {
	return m.createSnapshotOpts(ctx, vmID, name, pending, false)
}

func (m *Manager) createSnapshotOpts(ctx context.Context, vmID, name string, pending, reprovisionSSH bool) (Snapshot, error) {
	if len(name) > 128 {
		return Snapshot{}, errf(400, "name too long")
	}
	if m.cfg.HugePages {
		return Snapshot{}, errHugePages
	}
	unlock := m.lock(vmID)
	defer unlock()
	v, err := m.get(vmID)
	if err != nil {
		return Snapshot{}, err
	}
	if v.PendingCopy || (v.State != Running && v.State != Stopped) {
		return Snapshot{}, errf(http.StatusConflict, "vm is %s", v.State)
	}
	noEnv := v.Labels["boat.no_env"] == "1"
	var snapLabels map[string]string
	if noEnv {
		snapLabels = map[string]string{"boat.no_env": "1"}
	}
	s := Snapshot{PendingCopy: pending, ID: newID("snap-"), Name: name, VMID: v.ID, Team: v.Team, Image: v.Image, Size: v.Size,
		VCPU: v.VCPU, MemMiB: v.MemMiB, DiskGiB: v.DiskGiB, CreatedAt: m.now(), HasMem: v.State == Running || v.HasSnapshot,
		NoEnv: noEnv, Labels: snapLabels}
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
	s.HasMem, err = m.freeze(ctx, v, dir, reprovisionSSH)
	s.Unsynced = s.HasMem
	if err == nil {
		// No fsync here: 3-4 s behind a dump in writeback. syncSaved persists it.
		if pending {
			err = writeJSONAtomic(filepath.Join(dir, "snap.json"), s, 0o600)
		} else {
			err = writeJSONNoSync(filepath.Join(dir, "snap.json"), s, 0o600)
		}
	}
	if err != nil {
		m.removeStoreDir(dir)
		return Snapshot{}, err
	}
	m.mu.Lock()
	res.releaseLocked()
	m.snaps[s.ID] = s
	m.snapsChangedLocked()
	m.mu.Unlock()
	if s.HasMem {
		go m.syncSaved(s)
	}
	if !pending {
		m.ev.publish(Event{Type: "snapshot.created", VMID: v.ID, SnapID: s.ID, Team: v.Team})
	}
	return s, nil
}

func (m *Manager) releaseSnapshot(vmID, sid string) (Snapshot, error) {
	m.snapDirMu.Lock()
	defer m.snapDirMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.snaps[sid]
	if !ok || s.VMID != vmID {
		return Snapshot{}, errNotFound
	}
	if !s.PendingCopy {
		return s, nil
	}
	s.PendingCopy = false
	if err := writeJSONAtomic(filepath.Join(m.snapDir(sid), "snap.json"), s, 0o600); err != nil {
		return Snapshot{}, err
	}
	m.snaps[sid] = s
	m.snapsChangedLocked()
	m.ev.publish(Event{Type: "snapshot.created", VMID: vmID, SnapID: sid, Team: s.Team})
	return s, nil
}

func (m *Manager) DeleteSnapshot(vmID, sid string) error {
	m.mu.Lock()
	s, ok := m.snaps[sid]
	if ok && s.VMID == vmID {
		delete(m.snaps, sid)
		m.snapsChangedLocked()
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
// team and for the host. Committed bytes upper-bound nominal VM and snapshot size;
// hostStorageCheck caps nominal committed bytes at total * storageOvercommit, while
// checkFloor enforces the live minFreePct / minFreeGiB reserve on actual free space.
func (m *Manager) storageQuotaLocked(team string, addVMs, addSnaps int, addBytes int64) error {
	p := m.cfg.team(team)
	vms, snaps, bytes := addVMs, addSnaps+m.reservedSnaps[team], addBytes+m.reservedBytes[team]
	host := addBytes
	m.eachVMLocked(func(v VM) {
		if v.Team == team {
			vms, bytes = vms+1, bytes+vmCommit(v)
		}
		host += vmCommit(v)
	})
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

// hostStorageCheck caps nominal committed bytes across all teams at total * storageOvercommit
// (checkFloor separately enforces the live minFreePct / minFreeGiB reserve on free space).
// Only growth is refused: a host already past the cap (a lowered setting) can still stop, delete and shrink.
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

// storageCacheTTL bounds how stale storage() may be: a walk of every VM and
// snapshot dir is the cost of a usage GET, and dashboards poll it.
var storageCacheTTL = 5 * time.Second

// storageCache memoises storage() for storageCacheTTL, with one walk shared by
// concurrent callers (singleflight). It is invalidated on snapshot create/delete
// and VM create, and keyed on a fingerprint of what exists (ids, teams, committed
// bytes) so a VM delete, or any path that forgot to invalidate, misses it too.
// The zero value is ready to use.
type storageCache struct {
	mu     sync.Mutex
	at     time.Time
	print  uint64
	gen    uint64 // bumped by invalidate
	valid  bool
	out    map[string]teamStorage
	disk   diskSpace
	flight *storageFlight
	walks  atomic.Int64 // walks done, for tests
	hits   atomic.Int64 // peek hits (no O(n) copy of the VMs), for tests

	key   storageKey // generation of what out was walked for; keyed marks it set
	keyed bool
}

// storageKey is the O(1) identity of the inventory storage() sums: Manager.storageGen
// (bumped on VM/snapshot create, delete and size changes) and the two counts. The
// counts catch a path that forgot to bump the generation (a snapshot import).
type storageKey struct {
	gen        uint64
	vms, snaps int
}

// peek returns the cached walk without walking or copying anything, if it is fresh
// and was walked for key. The returned map is never mutated by the cache: copy it.
func (c *storageCache) peek(key storageKey, ttl time.Duration) (map[string]teamStorage, diskSpace, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && c.keyed && c.key == key && time.Since(c.at) < ttl {
		c.hits.Add(1)
		return c.out, c.disk, true
	}
	return nil, diskSpace{}, false
}

type storageFlight struct {
	gen   uint64
	print uint64
	key   storageKey
	keyed bool
	done  chan struct{}
	out   map[string]teamStorage
	disk  diskSpace
}

func (c *storageCache) invalidate() {
	c.mu.Lock()
	c.gen++
	c.valid = false
	c.mu.Unlock()
}

// fingerprint summarises the VMs and snapshots (order-independent).
func storagePrint(vms []VM, snaps []Snapshot) uint64 {
	h := func(s string) uint64 {
		x := uint64(14695981039346656037)
		for i := 0; i < len(s); i++ {
			x = (x ^ uint64(s[i])) * 1099511628211
		}
		return x
	}
	var p uint64
	for _, v := range vms {
		p += h("v/"+v.ID+"/"+v.Team) ^ uint64(vmCommit(v))
	}
	for _, s := range snaps {
		p += h("s/"+s.ID+"/"+s.Team) ^ uint64(snapCommit(s))
	}
	return p ^ uint64(len(vms))<<48 ^ uint64(len(snaps))<<32
}

func copyStorage(in map[string]teamStorage) map[string]*teamStorage {
	out := make(map[string]*teamStorage, len(in))
	for k, v := range in {
		v := v
		out[k] = &v
	}
	return out
}

// get returns the cached walk if fresh and for the same VM/snapshot set, else walks
// once for all concurrent callers.
func (c *storageCache) get(print uint64, ttl time.Duration, walk func() (map[string]teamStorage, diskSpace)) (map[string]*teamStorage, diskSpace) {
	return c.getKeyed(storageKey{}, false, print, ttl, walk)
}

// getKeyed is get that also records key (when keyed) as the identity of the result,
// for peek.
func (c *storageCache) getKeyed(key storageKey, keyed bool, print uint64, ttl time.Duration, walk func() (map[string]teamStorage, diskSpace)) (map[string]*teamStorage, diskSpace) {
	c.mu.Lock()
	if c.valid && c.print == print && time.Since(c.at) < ttl {
		if keyed {
			c.key, c.keyed = key, true // same content under a newer generation
		}
		out, d := copyStorage(c.out), c.disk
		c.mu.Unlock()
		return out, d
	}
	f := c.flight
	if f == nil || f.gen != c.gen || f.print != print {
		f = &storageFlight{gen: c.gen, print: print, key: key, keyed: keyed, done: make(chan struct{})}
		c.flight = f
		go func() {
			c.walks.Add(1)
			f.out, f.disk = walk()
			c.mu.Lock()
			if c.flight == f {
				c.flight = nil
			}
			if f.gen == c.gen { // not invalidated while walking
				c.out, c.disk, c.print, c.at, c.valid = f.out, f.disk, f.print, time.Now(), true
				c.key, c.keyed = f.key, f.keyed
			}
			c.mu.Unlock()
			close(f.done)
		}()
	}
	c.mu.Unlock()
	<-f.done
	return copyStorage(f.out), f.disk
}

// snapsChangedLocked: a snapshot was created, replaced or deleted. Caller holds m.mu.
func (m *Manager) snapsChangedLocked() {
	m.storageCache.invalidate()
	m.storageGen++
	m.stateGen++ // snapshots are billed: they are part of the usage report
}

// storage: per-team committed and on-disk bytes, cached for storageCacheTTL. A hit
// costs O(teams) under m.mu: the cache is checked against the inventory generation
// before any VM or snapshot is copied.
func (m *Manager) storage() (map[string]*teamStorage, diskSpace) {
	m.mu.Lock()
	key := storageKey{m.storageGen, len(m.vms), len(m.snaps)}
	if out, d, ok := m.storageCache.peek(key, storageCacheTTL); ok {
		m.mu.Unlock()
		return copyStorage(out), d
	}
	vms := make([]VM, 0, len(m.vms))
	for _, v := range m.vms {
		vms = append(vms, v)
	}
	snaps := make([]Snapshot, 0, len(m.snaps))
	for _, s := range m.snaps {
		snaps = append(snaps, s)
	}
	m.mu.Unlock()
	return m.storageCache.getKeyed(key, true, storagePrint(vms, snaps), storageCacheTTL, func() (map[string]teamStorage, diskSpace) {
		return m.walkStorage(vms, snaps)
	})
}

// walkStorage walks every VM and snapshot dir.
func (m *Manager) walkStorage(vms []VM, snaps []Snapshot) (map[string]teamStorage, diskSpace) {
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
	res := make(map[string]teamStorage, len(out))
	for k, v := range out {
		res[k] = *v
	}
	return res, d
}
