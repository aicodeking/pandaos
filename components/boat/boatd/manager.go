package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// apiErr carries an HTTP status to the API layer.

type Manager struct {
	cfg   Config
	run   runner
	procs procs
	ev    *broker
	usage *ledger
	now   func() time.Time

	mu     sync.Mutex
	vms    map[string]VM
	snaps  map[string]Snapshot
	locks  map[string]*sync.Mutex
	dones  map[string]<-chan struct{}
	active map[string]time.Time // last exec/file/desktop seen by boatd
	freed  map[int]time.Time    // quarantined VM indexes -> end of quarantine (quarantine.go)
	netMu  sync.Mutex
	// snapDirMu: syncSaved rewrites snap.json behind CreateSnapshot; a delete of
	// the snapshot meanwhile must not race it (RemoveAll fails with ENOTEMPTY when
	// the temp file appears mid-removal). Taken before m.mu, never inside it.
	snapDirMu sync.Mutex

	imagesReady   chan struct{}  // closed when images are NOCOW: a clone of one that is not stays CoW
	reservedSnaps map[string]int // by team: storage being written, see reservation
	reservedBytes map[string]int64
	reaping       map[string]bool // VMs a reap goroutine is on
	reapSlots     chan struct{}   // stops running at once: each writes a memory dump
	reapWG        sync.WaitGroup
	teamVolMu     sync.Mutex

	ctFlushErrs atomic.Int64 // conntrack flushes that failed: stale flows may survive

	statfs func(path string) (free, total uint64, err error)

	dirty     map[string]bool // VMs whose vm.json is behind (ENOSPC)
	saveState func(dir string, v VM) error

	exportArgv func(dir string, files []string) []string
	unpackArgv []string // stdin tar.zst -> stdout tar
	hostMem    int      // MiB available to VMs before overcommit (RAM - reserve)
	hostCPU    int

	memAvail   func() int      // MemAvailable in MiB; 0 = unknown
	warming    map[string]bool // template builds in flight
	warmSettle time.Duration   // wait for free page reporting before saving a template
	ballooned  bool            // only memoryPressure (one ticker goroutine) uses these two
	balloonPct int
}

// saveVMState fsyncs only a stop or an error, the states a host crash must not forget.
// The others are recomputed after a crash (a dead VM's state comes from its files), and
// each fsync ran under the global lock: 5.1 s with a memory dump in writeback (run 41600185173).
func saveVMState(dir string, v VM) error {
	path := filepath.Join(dir, "vm.json")
	if v.State == Stopped || v.State == Error {
		return writeJSONAtomic(path, v, 0o600)
	}
	return writeJSONNoSync(path, v, 0o600)
}

func newManager(cfg Config, run runner, p procs) (*Manager, error) {
	for _, d := range []string{"vms", "snapshots", "images"} {
		if err := os.MkdirAll(filepath.Join(cfg.StateDir, d), 0o711); err != nil {
			return nil, err
		}
	}
	m := &Manager{cfg: cfg, run: run, procs: p, ev: newBroker(),
		usage: &ledger{path: filepath.Join(cfg.StateDir, "usage.jsonl")},
		now:   func() time.Time { return time.Now().UTC() },
		vms:   map[string]VM{}, snaps: map[string]Snapshot{}, locks: map[string]*sync.Mutex{},
		dones: map[string]<-chan struct{}{}, active: map[string]time.Time{}, statfs: statfs,
		memAvail: func() int { return meminfoMiB("MemAvailable") }, warming: map[string]bool{}, warmSettle: 3 * time.Second,
		exportArgv: tarZstdArgv, unpackArgv: []string{"zstd", "-dcq"}, dirty: map[string]bool{},
		saveState: saveVMState, reservedSnaps: map[string]int{}, reservedBytes: map[string]int64{},
		reaping: map[string]bool{}, reapSlots: make(chan struct{}, reapParallel), imagesReady: make(chan struct{})}
	m.hostMem, m.hostCPU = cfg.HostMemMiB, cfg.HostCPUs
	if m.hostMem == 0 {
		m.hostMem = hostMemMiB()
	}
	if m.hostCPU == 0 {
		m.hostCPU = runtime.NumCPU()
	}
	m.hostMem -= cfg.HostReserveMiB
	m.loadFreed()
	if err := m.enableQuota(); err != nil {
		return nil, err
	}
	// Minutes for big images on a slow disk (28 s for 5.5 GiB on the CI runner), once per
	// image. boatd serves meanwhile, and VMs it re-adopts must not wait: only creates do.
	go func() {
		defer close(m.imagesReady)
		if cfg.DiskNoCow {
			m.convertImages()
		}
	}()
	cfg.enableKSM()
	return m, m.reconcile()
}

func (m *Manager) vmDir(id string) string   { return filepath.Join(m.cfg.StateDir, "vms", id) }
func (m *Manager) snapDir(id string) string { return filepath.Join(m.cfg.StateDir, "snapshots", id) }
func (m *Manager) imageDir(n string) string { return filepath.Join(m.cfg.StateDir, "images", n) }

// root is the dir firecracker sees as its cwd (plain) or chroot (jailer).
func (m *Manager) root(id string) string {
	if m.cfg.Jailer != "" {
		return filepath.Join(m.vmDir(id), filepath.Base(m.cfg.Firecracker), id, "root")
	}
	return filepath.Join(m.vmDir(id), "root")
}

// ids: one uid/gid per VM under the jailer, else the shared firecracker user.
func (m *Manager) ids(v VM) (int, int) {
	if m.cfg.Jailer != "" {
		return m.cfg.JailUIDBase + v.Index, m.cfg.JailUIDBase + v.Index
	}
	return m.cfg.FCUID, m.cfg.FCGID
}

// lock serialises lifecycle ops on one VM.
func (m *Manager) lock(id string) func() {
	m.mu.Lock()
	l, ok := m.locks[id]
	if !ok {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (m *Manager) lockBusy(id string) bool {
	m.mu.Lock()
	l := m.locks[id]
	m.mu.Unlock()
	if l == nil || l.TryLock() {
		if l != nil {
			l.Unlock()
		}
		return false
	}
	return true
}

func (m *Manager) get(id string) (VM, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vms[id]
	if !ok {
		return VM{}, errNotFound
	}
	return v.clone(), nil
}

func (m *Manager) list(team string) []VM {
	m.mu.Lock()
	out := []VM{}
	for _, v := range m.vms {
		if team == "" || v.Team == team {
			out = append(out, v.clone())
		}
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// update copies the VM, applies fn, persists, swaps it in, and publishes a
// state event. Leaving Running closes a billing interval.
func (m *Manager) update(id, reason string, fn func(*VM)) (VM, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.vms[id]
	if !ok {
		return VM{}, errNotFound
	}
	nv := old.clone()
	fn(&nv)
	return nv, m.commitLocked(old, nv, reason)
}

func (m *Manager) commitLocked(old, nv VM, reason string) error {
	now := m.now()
	nv.UpdatedAt = now
	if nv.State == Running && old.State != Running {
		nv.StartedAt = &now
	}
	if err := m.saveState(m.vmDir(nv.ID), nv); errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		// A full disk or team quota (a tenant can cause either) must not lock anyone out
		// of stopping or deleting a VM: memory stays right, flushDirty retries the file.
		log.Printf("%s: state not persisted, disk full", nv.ID)
		m.dirty[nv.ID] = true
	} else if err != nil {
		return err
	} else {
		delete(m.dirty, nv.ID)
	}
	m.vms[nv.ID] = nv
	if old.State == Running && nv.State != Running && old.StartedAt != nil {
		if err := m.usage.append(usageRec{"vm", nv.ID, nv.Team, nv.Size, *old.StartedAt, now}); err != nil {
			log.Printf("usage: %v", err)
		}
	}
	if old.State != nv.State {
		m.ev.publish(Event{Type: "vm.state", VMID: nv.ID, Team: nv.Team, Size: nv.Size, State: nv.State, Prev: old.State, Reason: reason})
	}
	return nil
}

// quotaLocked: would adding s to team exceed its policy? exclude = VM being started.
func (m *Manager) quotaLocked(team string, s Size, exclude string) error {
	p := m.cfg.team(team)
	n, cpu, mem := 1, s.VCPU, s.MemMiB
	for _, v := range m.vms {
		if v.Team == team && v.ID != exclude && v.State.active() {
			n, cpu, mem = n+1, cpu+v.VCPU, mem+v.MemMiB
		}
	}
	// Whole host, all teams. A VM's cgroup allows it its RAM plus the VMM's overhead.
	var hostMem, hostCPU int
	for _, v := range m.vms {
		if v.ID != exclude && v.State.active() {
			hostMem, hostCPU = hostMem+v.MemMiB+m.cfg.VMOverheadMiB, hostCPU+v.VCPU
		}
	}
	if err := m.realMemCheck(s); err != nil {
		return err
	}
	switch {
	case m.hostMem > 0 && float64(hostMem+s.MemMiB+m.cfg.VMOverheadMiB) > float64(m.hostMem)*m.cfg.MemOvercommit:
		return errCapacity("host capacity: %d MiB of VM RAM already active (limit %.0f)", hostMem, float64(m.hostMem)*m.cfg.MemOvercommit)
	case m.hostCPU > 0 && float64(hostCPU+s.VCPU) > float64(m.hostCPU)*m.cfg.CPUOvercommit:
		return errCapacity("host capacity: %d vCPUs already active (limit %.0f)", hostCPU, float64(m.hostCPU)*m.cfg.CPUOvercommit)
	case p.MaxVMs > 0 && n > p.MaxVMs:
		return errf(http.StatusTooManyRequests, "quota: team %s may run %d VMs", team, p.MaxVMs)
	case p.MaxVCPU > 0 && cpu > p.MaxVCPU:
		return errf(http.StatusTooManyRequests, "quota: team %s may use %d vCPUs", team, p.MaxVCPU)
	case p.MaxMemMiB > 0 && mem > p.MaxMemMiB:
		return errf(http.StatusTooManyRequests, "quota: team %s may use %d MiB RAM", team, p.MaxMemMiB)
	}
	return nil
}

// alloc checks quota, assigns a network slot and persists v in state st.
func (m *Manager) alloc(v VM, st State) (VM, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.allocLocked(v, st)
}

func (m *Manager) allocLocked(v VM, st State) (VM, error) {
	if err := m.quotaLocked(v.Team, Size{v.VCPU, v.MemMiB, v.DiskGiB}, ""); err != nil {
		return VM{}, err
	}
	if err := m.storageQuotaLocked(v.Team, 1, 0, vmCommit(v)); err != nil {
		return VM{}, err
	}
	used, pub := map[int]bool{}, map[string]bool{}
	for _, o := range m.vms {
		used[o.Index], pub[o.PublicIP] = true, true
	}
	idx := m.pickIndexLocked(used)
	if idx == 0 {
		return VM{}, errCapacity("no free VM addresses")
	}
	v.ID = newID("vm-")
	if v.Name == "" {
		v.Name = v.ID
	}
	v.Index, v.IP, v.MAC, v.Tap, v.IPv6 = idx, m.cfg.ipFor(idx), macFor(idx), tapFor(idx), m.cfg.ipv6For(idx)
	for _, a := range m.cfg.PublicIPv4 {
		if !pub[a] {
			v.PublicIP = a // ponytail: held until delete, even while stopped
			break
		}
	}
	v.State, v.Ports, v.CreatedAt = st, []Port{}, m.now()
	if err := m.makeVMDir(v); err != nil {
		return VM{}, err
	}
	m.flushConntrack(v.IP, v.IPv6, v.PublicIP) // a previous owner's flows
	if err := m.commitLocked(VM{}, v, "created"); err != nil {
		m.removeVMDir(v.ID)
		return VM{}, err
	}
	return v, nil
}

// makeVMDir: see makeStoreDir.
func (m *Manager) makeVMDir(v VM) error {
	if err := m.makeStoreDir("vms", v.Team, v.ID); err != nil {
		return err
	}
	return os.MkdirAll(m.root(v.ID), 0o700)
}

func (m *Manager) removeVMDir(id string) error { return m.removeStoreDir(m.vmDir(id)) }

// ---- btrfsQgroups: one limited subvolume per team ----
//
// A team's VMs and snapshots live in teams/t_<team>, whose exclusive bytes are capped,
// with vms/<id> and snapshots/<id> symlinks to them: the kernel's limit is on the
// tenant, and one subvolume keeps hard-linked memory files working within the team (a
// subvolume per VM made each of those a reflink, which waits for the file's writeback).
// Data shared with the image by reflink does not count. Past the limit the team's own
// guests get write errors and nobody else notices.

// teamVolSlackPct: above maxDiskGiB, which committed storage already stays within. A
// stop's memory dump exists twice for a moment, and the limit must not fail those first.
const teamVolSlackPct = 125

func (m *Manager) teamVol(team string) string {
	return filepath.Join(m.cfg.StateDir, "teams", "t_"+team) // the prefix keeps reTeam's "." and ".." one plain name
}

func (m *Manager) limitTeamVol(team string) error {
	limit := "none"
	if g := m.cfg.team(team).MaxDiskGiB; g > 0 {
		limit = strconv.FormatInt(int64(g)<<30/100*teamVolSlackPct, 10)
	}
	return m.run("", "btrfs", "qgroup", "limit", "-e", limit, m.teamVol(team))
}

// quotaCtl is a variable for the tests.
var quotaCtl = enableSimpleQuota

// enableQuota at startup. Simple quotas (Linux >= 6.7), not full ones: those walk every
// extent's references at each commit, which took a fork from 1.31 to 3.25 s (run
// 41305928387). Simple quotas charge an extent to the subvolume that wrote it, which is
// the question here: an image's reflinks cost a team nothing.
func (m *Manager) enableQuota() error {
	if !m.cfg.BtrfsQgroups {
		return nil
	}
	if err := quotaCtl(m.cfg.StateDir); err != nil {
		if m.cfg.UntrustedTenants {
			return fmt.Errorf("btrfsQgroups: %s: %w", m.cfg.StateDir, err)
		}
		log.Printf("btrfsQgroups: no simple quotas on %s (needs btrfs and Linux 6.7), so no per-team limit: %v", m.cfg.StateDir, err)
		m.cfg.BtrfsQgroups = false
		return nil
	}
	vols, _ := filepath.Glob(filepath.Join(m.cfg.StateDir, "teams", "t_*"))
	for _, d := range vols { // a limit from a config that changed
		if err := m.limitTeamVol(strings.TrimPrefix(filepath.Base(d), "t_")); err != nil {
			return err
		}
	}
	return nil
}

// makeStoreDir creates vms/<id> or snapshots/<id> for a team's data.
func (m *Manager) makeStoreDir(kind, team, id string) error {
	link := filepath.Join(m.cfg.StateDir, kind, id)
	if !m.cfg.BtrfsQgroups {
		return os.MkdirAll(link, 0o700)
	}
	if err := m.ensureTeamVol(team); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(m.teamVol(team), kind, id), 0o700); err != nil {
		return err
	}
	return os.Symlink(filepath.Join("..", "teams", "t_"+team, kind, id), link)
}

func (m *Manager) ensureTeamVol(team string) error {
	m.teamVolMu.Lock()
	defer m.teamVolMu.Unlock()
	d := m.teamVol(team)
	if exists(d) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(d), 0o711); err != nil {
		return err
	}
	if err := m.run("", "btrfs", "subvolume", "create", d); err != nil {
		return err
	}
	if err := os.Chmod(d, 0o711); err != nil {
		return err
	}
	if err := m.limitTeamVol(team); err != nil {
		m.run("", "btrfs", "subvolume", "delete", d)
		return err
	}
	return nil
}

// removeStoreDir: a dir made by makeStoreDir, or one from before team subvolumes.
func (m *Manager) removeStoreDir(link string) error {
	if st, err := os.Lstat(link); err == nil && st.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(link)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(filepath.Dir(link), target)); err != nil {
			return err
		}
		return os.Remove(link)
	}
	if m.cfg.BtrfsQgroups && m.run("", "btrfs", "subvolume", "delete", link) == nil {
		return nil // a VM dir that was its own subvolume
	}
	return os.RemoveAll(link)
}

// syncNet reloads nftables and routes.json from the current VM set. An nft
// error means the isolation rules are not in force: callers starting a VM must
// fail rather than run it unfiltered.
func (m *Manager) syncNet() error {
	m.netMu.Lock()
	defer m.netMu.Unlock()
	vms := m.list("")
	err := m.run(ruleset(m.cfg, vms), "nft", "-f", "-")
	if err != nil {
		log.Printf("nft: %v", err)
	}
	if rerr := writeRoutes(m.cfg, vms); rerr != nil {
		log.Printf("routes: %v", rerr)
	}
	return err
}

func (m *Manager) touch(id string) {
	m.mu.Lock()
	m.active[id] = m.now()
	m.mu.Unlock()
}

// sock returns the agent socket of a running VM and records activity.
func (m *Manager) sock(id string) (string, error) {
	v, err := m.get(id)
	if err != nil {
		return "", err
	}
	if v.State != Running {
		return "", errf(http.StatusConflict, "vm is %s, not running", v.State)
	}
	m.touch(id)
	return filepath.Join(m.root(id), vsockUDS), nil
}

// reconcile loads state after a boatd (re)start: re-adopts live firecracker
// processes, and marks VMs whose process is gone as stopped.
func (m *Manager) reconcile() error {
	m.dropTorn("snapshots", "snap.json")
	m.dropTorn("vms", "vm.json")
	snaps, err := loadAll[Snapshot](filepath.Join(m.cfg.StateDir, "snapshots"), "snap.json")
	if err != nil {
		return err
	}
	for _, s := range snaps {
		m.snaps[s.ID] = s
	}
	vms, err := loadAll[VM](filepath.Join(m.cfg.StateDir, "vms"), "vm.json")
	if err != nil {
		return err
	}
	for _, v := range vms {
		m.vms[v.ID] = v
	}
	for _, s := range snaps {
		if s.Unsynced {
			// Maybe a host crash before the fsync: keep the disk, drop the memory.
			log.Printf("%s: snapshot memory was not synced; it restores cold", s.ID)
			os.Remove(filepath.Join(m.snapDir(s.ID), vmstate))
			os.Remove(filepath.Join(m.snapDir(s.ID), memFile))
			s.HasMem, s.Unsynced = false, false
			if err := writeJSONAtomic(filepath.Join(m.snapDir(s.ID), "snap.json"), s, 0o600); err != nil {
				return err
			}
			m.snaps[s.ID] = s
		}
	}
	for _, v := range vms {
		if v.State == Stopped && v.HasSnapshot && !v.SnapSynced {
			// Maybe a host crash before the fsync: the memory file cannot be trusted.
			log.Printf("%s: stop snapshot was not synced; the next start cold-boots", v.ID)
			nv := v.clone()
			nv.HasSnapshot = false
			os.Remove(filepath.Join(m.root(v.ID), vmstate))
			os.Remove(filepath.Join(m.root(v.ID), memFile))
			m.mu.Lock()
			err := m.commitLocked(v, nv, "")
			m.mu.Unlock()
			if err != nil {
				return err
			}
			continue
		}
		alive := v.PID > 0 && m.procs.Alive(v.PID, v.ID)
		if alive && v.State == Running {
			go m.setMemLimits(v)                 // a crash may have left them lifted for a dump
			for _, p := range l2Pins(m.cfg, v) { // a VM started by a boatd without them
				if err := m.run("", p[0], p[1:]...); err != nil {
					log.Printf("%s: %v", v.ID, err)
				}
			}
			done := m.procs.Adopt(v.PID, v.ID)
			m.dones[v.ID] = done
			go m.watch(v.ID, v.PID, done)
			continue
		}
		if alive { // crashed mid-operation: the VM is in an unknown state
			m.procs.Kill(v.PID)
		}
		if !v.State.active() {
			continue
		}
		root := m.root(v.ID)
		for _, f := range []string{snapState, snapDiff, memNew} {
			os.Remove(filepath.Join(root, f))
		}
		os.Remove(filepath.Join(m.vmDir(v.ID), memStage))
		nv := m.deadState(v)
		// It was running and nobody stopped it: boatd or the host died. Its memory
		// is gone; bring it back cold from its disk (see resumeAll).
		nv.ResumeOnBoot = nv.ResumeOnBoot || (v.State == Running && !alive && nv.State == Stopped && m.cfg.ResumeCrashed)
		m.mu.Lock()
		err := m.commitLocked(v, nv, "boatd restart")
		m.mu.Unlock()
		if err != nil {
			return err
		}
		delVMNet(m.run, v)
	}
	return nil
}

// dropTorn: a first state file is written without an fsync, so a host crash can
// leave it empty. Replacing renames are safe on btrfs and ext4; a new file is not.
// Such a VM was still being created, and such a snapshot was never durable.
func (m *Manager) dropTorn(sub, file string) {
	paths, _ := filepath.Glob(filepath.Join(m.cfg.StateDir, sub, "*", file))
	for _, p := range paths {
		var v map[string]any
		if err := readJSON(p, &v); err != nil {
			log.Printf("%s: dropping what a crash left torn: %v", p, err)
			m.removeStoreDir(filepath.Dir(p))
		}
	}
}

func (m *Manager) deadState(v VM) VM {
	nv := v.clone()
	root := m.root(v.ID)
	nv.PID, nv.AgentReady = 0, false
	nv.HasSnapshot = exists(filepath.Join(root, vmstate)) && exists(filepath.Join(root, memFile))
	nv.State = Stopped
	if !exists(filepath.Join(root, diskFile)) {
		nv.State, nv.Error = Error, "disk missing after an interrupted "+string(v.State)
	}
	return nv
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// watch marks a VM stopped when its firecracker exits on its own (guest
// reboot/poweroff, crash). Exits caused by our own ops find PID changed.
func (m *Manager) watch(id string, pid int, done <-chan struct{}) {
	<-done
	unlock := m.lock(id)
	defer unlock()
	v, err := m.get(id)
	if err != nil || v.PID != pid || v.State != Running {
		return
	}
	log.Printf("%s: firecracker pid %d exited on its own", id, pid)
	m.procs.Kill(pid) // never leave a VM running behind a "stopped" record
	delVMNet(m.run, v)
	if _, err := m.update(id, "exited", func(v *VM) {
		v.State, v.PID, v.HasSnapshot, v.AgentReady = Stopped, 0, false, false
	}); err != nil {
		log.Printf("%s: %v", id, err)
	}
	m.syncNet()
}

// killWait kills the VM's firecracker and waits for it to be gone.
func (m *Manager) killWait(id string, pid int) {
	if pid <= 0 {
		return
	}
	m.mu.Lock()
	done := m.dones[id]
	m.mu.Unlock()
	m.procs.Kill(pid)
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		log.Printf("%s: pid %d did not exit after SIGKILL", id, pid)
	}
}

// Guest serial and VMM logs go to firecracker.log through an O_APPEND file, so
// it needs no reader and survives a boatd restart; rotateLogs keeps it bounded:
// past logCapBytes the tail moves to firecracker.log.1 and the file is truncated.
const (
	logCapBytes    = 16 << 20
	logTailBytes   = 1 << 20
	logRotateEvery = time.Second
)

// flushDirty retries state writes that hit a full disk.
func (m *Manager) flushDirty() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.dirty {
		v, ok := m.vms[id]
		if !ok || m.saveState(m.vmDir(id), v) == nil {
			delete(m.dirty, id)
		}
	}
}

func (m *Manager) rotateLogs() {
	for _, v := range m.list("") {
		if v.State != Stopped && v.State != Error {
			if err := rotateLog(filepath.Join(m.vmDir(v.ID), "firecracker.log")); err != nil {
				log.Printf("%s: log: %v", v.ID, err)
			}
		}
	}
}

func rotateLog(path string) error {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= logCapBytes {
		return nil // no file yet
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	tail := make([]byte, logTailBytes)
	n, err := f.ReadAt(tail, st.Size()-logTailBytes)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if err := os.WriteFile(path+".1", tail[:n], 0o600); err != nil {
		return err
	}
	return os.Truncate(path, 0)
}

// stopAll snapshots every running VM in parallel (service stop / host reboot).
func (m *Manager) stopAll(ctx context.Context, reason string) {
	var wg sync.WaitGroup
	for _, v := range m.list("") {
		if v.State == Running {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := m.Stop(ctx, v.ID, reason); err != nil {
					log.Printf("%s: stop: %v", v.ID, err)
					return
				}
				m.syncSnapshot(v.ID) // we are exiting: the background one would not finish
				m.update(v.ID, "", func(x *VM) { x.ResumeOnBoot = true })
			}()
		}
	}
	wg.Wait()
}

// resumeAll starts VMs that stopAll stopped (in the background).
func (m *Manager) resumeAll(ctx context.Context) {
	for _, v := range m.list(warmTeam) { // a template build cut short by a restart
		if _, err := m.Delete(ctx, v.ID); err != nil {
			log.Printf("%s: %v", v.ID, err)
		}
	}
	for _, v := range m.list("") {
		if v.ResumeOnBoot && v.State == Stopped {
			m.update(v.ID, "", func(x *VM) { x.ResumeOnBoot = false })
			go func() {
				if _, err := m.Start(ctx, v.ID, nil); err != nil {
					log.Printf("%s: resume on boot: %v", v.ID, err)
				}
			}()
		}
	}
}

const reapParallel = 8

// reap stops VMs past their TTL or idle limit. Called every 30 s. It never waits: one
// goroutine per VM, because any guest can hold its own up (a health probe that times
// out, a start waiting for the guest with the VM's lock held), and that must not delay
// anybody else's stop. A VM still being handled from the last tick is skipped.
func (m *Manager) reap(ctx context.Context) {
	now := m.now()
	for _, v := range m.list("") {
		if v.State != Running {
			continue
		}
		m.mu.Lock()
		busy := m.reaping[v.ID]
		m.reaping[v.ID] = true
		m.mu.Unlock()
		if busy {
			continue
		}
		m.reapWG.Add(1)
		go func() {
			defer m.reapWG.Done()
			m.reapVM(ctx, v, now)
			m.mu.Lock()
			delete(m.reaping, v.ID)
			m.mu.Unlock()
		}()
	}
}

func (m *Manager) reapVM(ctx context.Context, v VM, now time.Time) {
	reason := ""
	if v.ExpiresAt != nil && now.After(*v.ExpiresAt) {
		reason = "ttl"
	} else if limit := m.idleLimit(v); limit > 0 && now.Sub(m.lastActive(ctx, v)) > limit {
		reason = "idle"
	}
	if reason == "" || m.lockBusy(v.ID) { // busy: next tick, and no slot held meanwhile
		return
	}
	select {
	case m.reapSlots <- struct{}{}:
		defer func() { <-m.reapSlots }()
	case <-ctx.Done():
		return
	}
	if _, err := m.Stop(ctx, v.ID, reason); err != nil && !errors.Is(err, errNotFound) {
		log.Printf("%s: auto-stop (%s): %v", v.ID, reason, err)
	}
}

func (m *Manager) idleLimit(v VM) time.Duration {
	s := m.cfg.IdleStopS
	if v.IdleStopS != 0 {
		s = v.IdleStopS
	}
	return time.Duration(max(s, 0)) * time.Second
}

// lastActive = latest of boot, boatd-seen activity and the agent's idle_s
// (exec, ssh sessions, desktop input inside the guest).
func (m *Manager) lastActive(ctx context.Context, v VM) time.Time {
	last := v.CreatedAt
	if v.StartedAt != nil {
		last = *v.StartedAt
	}
	m.mu.Lock()
	if t := m.active[v.ID]; t.After(last) {
		last = t
	}
	m.mu.Unlock()
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := agentCall(c, filepath.Join(m.root(v.ID), vsockUDS), map[string]string{"op": "health"})
	if err != nil {
		return m.now() // can't tell: never stop a VM we can't see into
	}
	if r.IdleS != nil {
		if t := m.now().Add(-time.Duration(*r.IdleS * float64(time.Second))); t.After(last) {
			last = t
		}
	}
	return last
}
