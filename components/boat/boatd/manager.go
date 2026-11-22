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
	incarnation string // unpredictable process identity; never restored from disk
	cfg         Config
	run         runner
	procs       procs
	ev          *broker
	usage       *ledger
	now         func() time.Time

	mu                   sync.Mutex
	vms                  map[string]VM
	snaps                map[string]Snapshot
	locks                map[string]*sync.Mutex
	lockRefs             map[string]int // holders+waiters per id; entry pruned at zero (guarded by mu)
	dones                map[string]<-chan struct{}
	active               map[string]time.Time // last exec/file/desktop seen by boatd
	freed                map[int]time.Time    // quarantined VM indexes -> end of quarantine (quarantine.go)
	resets               resetHub             // vsock resets, for exec streams to reattach (execstream.go)
	netMu                sync.Mutex
	publishedReady       map[string]string // vmID -> SSHBootEpoch already open in nftables (under netMu)
	publishedQuarantined map[string]string // vmID -> SSHBootEpoch already fenced in nftables (under netMu)
	publishedSSHPaused   map[string]string // vmID -> SSHBootEpoch fenced in @ssh_pause_* sets (under netMu)
	dynQuarantined       map[string]VM     // vmID -> VM fenced via incremental @quarantine_* set elements (under netMu)
	dynSSHPaused         map[string]VM     // vmID -> VM fenced via incremental @ssh_pause_* set elements (under netMu)
	lastNftRuleset       string            // last ruleset applied via nft -f - (under netMu); empty = invalidated
	lastRoutes           *routeFile        // last routes.json written (under netMu)
	lastRoutesPath       string
	// syncNet group-commit coalescing (lock order: syncNetMu is a leaf lock, never held across syncNetOnce).
	syncNetMu   sync.Mutex
	syncNetRun  *syncNetBatch
	syncNetWait *syncNetBatch
	// snapDirMu: syncSaved rewrites snap.json behind CreateSnapshot; a delete of
	// the snapshot meanwhile must not race it (RemoveAll fails with ENOTEMPTY when
	// the temp file appears mid-removal). Taken before m.mu, never inside it.
	snapDirMu sync.Mutex

	imagesReady   chan struct{} // closed when images are NOCOW: a clone of one that is not stays CoW
	watchStop     chan struct{} // closed when tests/shutdown detach background process watchers
	watchOnce     sync.Once
	reservedSnaps map[string]int // by team: storage being written, see reservation
	reservedBytes map[string]int64
	reaping       map[string]bool // VMs a reap goroutine is on
	allocating    map[string]VM   // VMs whose slot is reserved but whose dir is still being made (outside mu)
	reapSlots     chan struct{}   // stops running at once: each writes a memory dump
	reapWG        sync.WaitGroup
	// Deletes whose dir could not be removed: ids with a background retry running
	// (under mu), and its first delay (0 = deleteRetryMin; tests shorten it).
	deleteRetries      map[string]bool
	deleteRetryDelay   time.Duration
	tombstoneDirMu     sync.Mutex
	tombstoneDirSynced atomic.Bool
	syncDirFn          func(string) error // test hook; nil = syncDir
	teamVolMu          sync.Mutex
	dumpSlots          chan struct{}                          // host-wide semaphore bounding concurrent memory dumps
	memMaxRetries      map[string]bool                        // VMs with a background memory.max restore loop (under mu)
	snapSyncs          map[string]*snapSyncCall               // in-flight syncSnapshot calls per VM (under mu)
	fsyncSnapFile      func(path string) (os.FileInfo, error) // test hook for syncSnapshot; nil = fsyncFileInfo

	storageCache storageCache // storage() walk results (snapshot.go)
	usageCache   usageCache   // assembled Usage() reports (lifecycle.go)
	// Generations, under mu. storageGen moves when what storage() sums changes (a VM or
	// snapshot created or deleted, a VM's size or team changed); stateGen when a VM
	// changes state (running intervals feed the usage report).
	storageGen  uint64
	stateGen    uint64
	persist     map[string]*vmPersist                            // per-VM vm.json write ordering (store.go)
	saveRelease func(path string, v any, mode os.FileMode) error // fsynced write of an authorization; nil = writeJSONAtomic. A test hook
	removeFile  func(path string) error                          // removes a discarded memory file; nil = os.Remove. A test hook
	freezeGate  func(parentID string)                            // runs just before a fork freezes its parent, under the parent lock. A test hook

	dio dumpIO // dumps capped while VMs boot (writeback.go)

	ctFlushErrs atomic.Int64      // conntrack flushes that failed: stale flows may survive
	ctState     map[string]uint8  // per-IP conntrack clean/dirty state (under mu)
	ctDirtyGen  map[string]uint64 // per-IP dirty generation to avoid flush-vs-boot race (under mu)
	portProbe   func(int) bool    // test hook for freePortLocked TCP bind probe; nil = tcpPortAvailable
	exportMu    sync.Mutex        // guards exportReg and stateDir/exports.json
	exportReg   map[string]exportRecord

	statfs func(path string) (free, total uint64, err error)

	dirty     map[string]bool // VMs whose vm.json is behind (ENOSPC)
	saveState func(dir string, v VM) error

	exportArgv func(dir string, files []string) []string
	unpackArgv []string // stdin tar.zst -> stdout tar
	hostMem    int      // MiB available to VMs before overcommit (RAM - reserve)
	hostCPU    int

	memAvail      func() int      // MemAvailable in MiB; 0 = unknown
	warming       map[string]bool // template builds in flight
	warmSettle    time.Duration   // wait for free page reporting before saving a template
	ballooned     bool            // only memoryPressure (one ticker goroutine) uses these two
	balloonPct    int
	balloonOnce   sync.Once
	balloonClient *http.Client
}

// saveVMState fsyncs only a stop or an error, the states a host crash must not forget.
// The others are recomputed after a crash (a dead VM's state comes from its files), and
// each fsync ran under the global lock: 5.1 s with a memory dump in writeback (run 41600185173).
func saveVMState(dir string, v VM) error {
	path := filepath.Join(dir, "vm.json")
	if v.State == Stopped || v.State == Error || v.PendingCopy {
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
	incarnation, err := sshNonce()
	if err != nil {
		return nil, err
	}
	m := &Manager{incarnation: incarnation, cfg: cfg, run: run, procs: p, ev: newBroker(),
		usage: &ledger{path: filepath.Join(cfg.StateDir, "usage.jsonl")},
		now:   func() time.Time { return time.Now().UTC() },
		vms:   map[string]VM{}, snaps: map[string]Snapshot{}, locks: map[string]*sync.Mutex{}, lockRefs: map[string]int{},
		dones: map[string]<-chan struct{}{}, active: map[string]time.Time{}, statfs: statfs,
		memAvail: func() int { return meminfoMiB("MemAvailable") }, warming: map[string]bool{}, warmSettle: 3 * time.Second,
		exportArgv: tarZstdArgv, unpackArgv: []string{"zstd", "-dcq"}, dirty: map[string]bool{},
		saveState: saveVMState, reservedSnaps: map[string]int{}, reservedBytes: map[string]int64{},
		publishedReady: map[string]string{}, publishedQuarantined: map[string]string{}, publishedSSHPaused: map[string]string{},
		dynQuarantined: map[string]VM{}, dynSSHPaused: map[string]VM{},
		reaping: map[string]bool{}, allocating: map[string]VM{}, reapSlots: make(chan struct{}, reapParallel),
		dumpSlots: make(chan struct{}, maxConcurrentDumps), memMaxRetries: map[string]bool{},
		snapSyncs: map[string]*snapSyncCall{},
		ctState:   map[string]uint8{}, ctDirtyGen: map[string]uint64{},
		imagesReady: make(chan struct{}), watchStop: make(chan struct{})}
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
	if err := m.ensureTombstoneDir(); err != nil {
		return nil, err
	}
	cfg.enableKSM()
	rerr := m.reconcile()
	if rerr == nil {
		m.retryLeftoverTombstones()
	}
	return m, rerr
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
		if m.cfg.JailGID > 0 {
			return m.cfg.JailUIDBase + v.Index, m.cfg.JailGID
		}
		return m.cfg.JailUIDBase + v.Index, m.cfg.JailUIDBase + v.Index
	}
	return m.cfg.FCUID, m.cfg.FCGID
}

// lock serialises lifecycle ops on one VM. Entries are refcounted and
// dropped when the last holder/waiter releases, so the map does not grow
// with VM churn.
func (m *Manager) lock(id string) func() {
	m.mu.Lock()
	l, ok := m.locks[id]
	if !ok {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	if m.lockRefs == nil {
		m.lockRefs = map[string]int{}
	}
	m.lockRefs[id]++
	m.mu.Unlock()
	l.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.Unlock()
			m.mu.Lock()
			if m.lockRefs[id]--; m.lockRefs[id] <= 0 {
				delete(m.lockRefs, id)
				if m.locks[id] == l {
					delete(m.locks, id)
				}
			}
			m.mu.Unlock()
		})
	}
}

func (m *Manager) lockBusy(id string) bool {
	m.mu.Lock()
	l := m.locks[id]
	m.mu.Unlock()
	// An entry exists only while someone holds or waits for the lock.
	return l != nil
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

type vmSSHSummary struct {
	ID              string `json:"id"`
	Team            string `json:"team"`
	State           State  `json:"state"`
	IP              string `json:"ip"`
	AgentReady      bool   `json:"agent_ready"`
	PendingCopy     bool   `json:"pending_copy,omitempty"`
	SSHBootEpoch    string `json:"ssh_boot_epoch"`
	SSHPolicyReady  bool   `json:"ssh_policy_ready"`
	SSHPolicyDigest string `json:"ssh_policy_digest,omitempty"`
	createdAt       time.Time
}

func (v VM) sshSummary() vmSSHSummary {
	return vmSSHSummary{
		ID:              v.ID,
		Team:            v.Team,
		State:           v.State,
		IP:              v.IP,
		AgentReady:      v.AgentReady,
		PendingCopy:     v.PendingCopy,
		SSHBootEpoch:    v.SSHBootEpoch,
		SSHPolicyReady:  v.SSHPolicyReady,
		SSHPolicyDigest: v.SSHPolicyDigest,
		createdAt:       v.CreatedAt,
	}
}

func (m *Manager) getSSHSummary(id string, includePending bool) (vmSSHSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vms[id]
	if !ok || (!includePending && v.PendingCopy) {
		return vmSSHSummary{}, errNotFound
	}
	return v.sshSummary(), nil
}

func (m *Manager) listSSHSummary(team string, includePending bool) []vmSSHSummary {
	m.mu.Lock()
	out := make([]vmSSHSummary, 0, len(m.vms))
	for _, v := range m.vms {
		if !includePending && v.PendingCopy {
			continue
		}
		if team != "" && v.Team != team {
			continue
		}
		out = append(out, v.sshSummary())
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].createdAt.Before(out[j].createdAt) })
	return out
}

// update copies the VM, applies fn, persists, swaps it in, and publishes a
// state event. Leaving Running closes a billing interval.
//
// A commit that must be durable (a stop or an error: saveVMState fsyncs it) does not
// fsync under m.mu: the new state is swapped in, and the vm.json write runs after the
// lock is released, ordered per VM by sequence number (vmPersist). update returns
// only once the file is durable, so a caller replies to its client after the same
// barrier as before; Get, List and the other VMs' updates meanwhile are not held up.
// The state event and the closed billing interval are published at the swap, ahead
// of the fsync; a host crash in between shows the previous state on disk, which
// reconcile treats as it always has (a dead VM's state comes from its files).
func (m *Manager) update(id, reason string, fn func(*VM)) (VM, error) {
	m.mu.Lock()
	old, ok := m.vms[id]
	if !ok {
		m.mu.Unlock()
		return VM{}, errNotFound
	}
	nv := old.clone()
	fn(&nv)
	if old.PendingCopy || nv.PendingCopy {
		m.mu.Unlock()
		return m.updatePending(id, reason, fn)
	}
	nv, finish := m.commitDeferLocked(old, nv, reason)
	m.mu.Unlock()
	return nv, finish()
}

// updateInMemory applies fn and publishes the transition (in-memory state,
// closed billing interval, state event, cache generations) without writing or
// fsyncing vm.json. Used by deleteVM after a durable tombstone is already on
// disk and immediately before removeVMState deletes the VM directory.
func (m *Manager) updateInMemory(id, reason string, fn func(*VM)) (VM, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.vms[id]
	if !ok {
		return VM{}, errNotFound
	}
	nv := old.clone()
	fn(&nv)
	nv = m.stampLocked(old, nv)
	if p := m.persist[id]; p != nil {
		p.seq++
	}
	delete(m.dirty, id)
	m.applyLocked(old, nv, reason)
	return nv, nil
}

// updatePending commits a transition of a copy awaiting authorization (or its
// release) with its fsynced vm.json written outside m.mu. The order is the strict one
// commitLocked keeps for these transitions: the new state is made durable first and
// swapped in (published, visible to Get/List/start) only after, so a copy is never
// authorized in memory ahead of the disk, and a failed write leaves the old state
// with no disk-full fallback. m.mu is not held through the fsync: Get, List and the
// other VMs proceed meanwhile.
//
// The write is numbered under m.mu like any commit (and counted in-flight, so the
// deferred commits of this VM queue behind it instead of fsyncing under the lock). A
// commit of the same VM that overtakes it makes this one stale: the transition is
// recomputed from the newer state and written again, so the disk never ends on a
// state derived from an old one.
func (m *Manager) updatePending(id, reason string, fn func(*VM)) (VM, error) {
	// One such transition of a VM at a time: two of them would keep invalidating each
	// other's sequence number. Only the rare plain commit can still overtake one.
	m.mu.Lock()
	pre := &m.persistLocked(id).pre
	m.mu.Unlock()
	pre.Lock()
	defer pre.Unlock()
	for {
		m.mu.Lock()
		old, ok := m.vms[id]
		if !ok {
			m.mu.Unlock()
			return VM{}, errNotFound
		}
		nv := old.clone()
		fn(&nv)
		if !old.PendingCopy && !nv.PendingCopy { // authorized meanwhile: nothing strict left
			nv, finish := m.commitDeferLocked(old, nv, reason)
			m.mu.Unlock()
			return nv, finish()
		}
		nv = m.stampLocked(old, nv)
		p := m.persistLocked(id)
		p.seq++
		seq := p.seq
		p.inflight++
		save := m.saveState
		release, saveRelease := old.PendingCopy && !nv.PendingCopy, m.saveRelease
		if saveRelease == nil {
			saveRelease = writeJSONAtomic
		}
		m.mu.Unlock()

		werr := p.write(seq, true, id, m.vmDir(id), func() error {
			if release {
				return saveRelease(filepath.Join(m.vmDir(id), "vm.json"), nv, 0o600)
			}
			return save(m.vmDir(id), nv)
		})

		m.mu.Lock()
		p.inflight--
		switch {
		case werr != nil:
			m.mu.Unlock()
			return VM{}, werr
		case p.seq != seq:
			m.mu.Unlock() // overtaken: rebase on the newer state
			continue
		}
		if _, ok := m.vms[id]; !ok {
			m.mu.Unlock()
			return VM{}, errNotFound
		}
		delete(m.dirty, id)
		m.applyLocked(old, nv, reason)
		m.mu.Unlock()
		return nv, nil
	}
}

// commitDeferLocked commits nv (replacing old) for a caller that holds m.mu and can
// finish outside it: the state is swapped in and published under the lock, and the
// returned finish waits for the vm.json write, which runs outside m.mu (the caller
// must call it after Unlock, exactly once). It returns the stamped VM.
//
// Authorization transitions (PendingCopy) and a non-durable state with no write in
// flight keep the strict inline path of commitLocked: nothing to fsync, nothing to
// wait behind. Anything else (a stop or error that fsyncs, or any write queued behind
// a deferred one of the same VM, whose vmPersist.mu may be held through an fsync)
// writes after the lock is released, so one VM's slow fsync never stalls the others.
func (m *Manager) commitDeferLocked(old, nv VM, reason string) (VM, func() error) {
	p := m.persistLocked(nv.ID)
	if old.PendingCopy || nv.PendingCopy || (!needsSync(nv) && p.inflight == 0) {
		err := m.commitLocked(old, nv, reason)
		if err == nil {
			nv = m.vms[nv.ID].clone()
		}
		return nv, func() error { return err }
	}
	id := nv.ID
	nv = m.stampLocked(old, nv)
	p.seq++
	seq := p.seq
	p.inflight++
	save := m.saveState
	durable := needsSync(nv)
	m.applyLocked(old, nv, reason)
	return nv, func() error {
		werr := p.write(seq, durable, id, m.vmDir(id), func() error { return save(m.vmDir(id), nv) })
		m.mu.Lock()
		p.inflight--
		err := m.settleWriteLocked(p, seq, id, werr)
		m.mu.Unlock()
		return err
	}
}

// needsSync: the states saveVMState fsyncs.
func needsSync(v VM) bool { return v.State == Stopped || v.State == Error || v.PendingCopy }

// persistLocked returns id's write-ordering record. Caller holds m.mu.
func (m *Manager) persistLocked(id string) *vmPersist {
	p := m.persist[id]
	if p == nil {
		if m.persist == nil {
			m.persist = map[string]*vmPersist{}
		}
		p = &vmPersist{}
		m.persist[id] = p
	}
	return p
}

// settleWriteLocked books the outcome of a vm.json write done outside m.mu. A full
// disk is absorbed (memory is right, flushDirty retries the file), any other failure
// is returned and also retried by flushDirty: the state is already swapped in and
// published, so rolling it back would lie to everyone who saw it.
func (m *Manager) settleWriteLocked(p *vmPersist, seq uint64, id string, err error) error {
	switch {
	case err == nil:
		if p.seq == seq {
			delete(m.dirty, id) // a newer commit settles its own
		}
		return nil
	case errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT):
		log.Printf("%s: state not persisted, disk full", id)
		m.dirty[id] = true
		return nil
	}
	m.dirty[id] = true
	return err
}

// stampLocked sets the clock, SSH gate and run timestamps for every commit.
func (m *Manager) stampLocked(old, nv VM) VM {
	if nv.State != Running {
		nv.SSHPolicyReady = false
	}
	now := m.now()
	nv.UpdatedAt = now
	if nv.State == Running && old.State != Running {
		nv.StartedAt = &now
		nv.StoppedAt = nil
	}
	if nv.State == Stopped {
		switch {
		case old.State != Stopped:
			nv.StoppedAt = &now
		case old.StoppedAt != nil:
			nv.StoppedAt = old.StoppedAt
		default:
			// Freeze the legacy fallback before a stopped edit changes updated_at.
			t := old.UpdatedAt
			nv.StoppedAt = &t
		}
	}
	return nv
}

func (m *Manager) commitLocked(old, nv VM, reason string) error {
	nv = m.stampLocked(old, nv)
	p := m.persistLocked(nv.ID)
	p.seq++
	seq := p.seq
	// Inline: this caller holds m.mu, so the write (ordered behind any deferred one
	// of the same VM) happens under it, as it always did.
	var err error
	if old.PendingCopy && !nv.PendingCopy {
		err = p.write(seq, true, nv.ID, m.vmDir(nv.ID), func() error { return writeJSONAtomic(filepath.Join(m.vmDir(nv.ID), "vm.json"), nv, 0o600) })
	} else {
		save := m.saveState
		err = p.write(seq, needsSync(nv), nv.ID, m.vmDir(nv.ID), func() error { return save(m.vmDir(nv.ID), nv) })
	}
	if err != nil && (old.PendingCopy || nv.PendingCopy) {
		return err // authorization transitions must never use the disk-full fallback
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		// A full disk or team quota (a tenant can cause either) must not lock anyone out
		// of stopping or deleting a VM: memory stays right, flushDirty retries the file.
		log.Printf("%s: state not persisted, disk full", nv.ID)
		m.dirty[nv.ID] = true
	} else if err != nil {
		return err
	} else {
		delete(m.dirty, nv.ID)
	}
	m.applyLocked(old, nv, reason)
	return nil
}

// applyLocked swaps nv in and publishes what the change means: generations for the
// storage and usage caches, the closed billing interval, the state event.
func (m *Manager) applyLocked(old, nv VM, reason string) {
	now := nv.UpdatedAt
	m.vms[nv.ID] = nv
	if old.ID == "" {
		m.storageCache.invalidate() // a new VM
	}
	if old.ID == "" || old.DiskGiB != nv.DiskGiB || old.MemMiB != nv.MemMiB || old.Team != nv.Team {
		m.storageGen++
	}
	if old.ID == "" || old.State != nv.State || !sameTime(old.StartedAt, nv.StartedAt) || old.Size != nv.Size || old.Team != nv.Team {
		m.stateGen++
	}
	if old.State == Running && nv.State != Running && old.StartedAt != nil {
		if err := m.usage.append(usageRec{"vm", nv.ID, nv.Team, nv.Size, *old.StartedAt, now}); err != nil {
			log.Printf("usage: %v", err)
		}
	}
	if old.State != nv.State && !nv.PendingCopy {
		m.ev.publish(Event{Type: "vm.state", VMID: nv.ID, Team: nv.Team, Size: nv.Size, State: nv.State, Prev: old.State, Reason: reason})
	}
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// quotaLocked: would adding s to team exceed its policy? exclude = VM being started.
func (m *Manager) quotaLocked(team string, s Size, exclude string) error {
	p := m.cfg.team(team)
	n, cpu, mem := 1, s.VCPU, s.MemMiB
	m.eachVMLocked(func(v VM) {
		if v.Team == team && v.ID != exclude && v.State.active() {
			n, cpu, mem = n+1, cpu+v.VCPU, mem+v.MemMiB
		}
	})
	// Whole host, all teams. A VM's cgroup allows it its RAM plus the VMM's overhead.
	var hostMem, hostCPU int
	m.eachVMLocked(func(v VM) {
		if v.ID != exclude && v.State.active() {
			hostMem, hostCPU = hostMem+v.MemMiB+m.cfg.VMOverheadMiB, hostCPU+v.VCPU
		}
	})
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

// eachVMLocked visits every VM that holds resources: the published ones and those
// still being allocated. Caller holds m.mu.
func (m *Manager) eachVMLocked(fn func(VM)) {
	for _, v := range m.vms {
		fn(v)
	}
	for _, v := range m.allocating {
		fn(v)
	}
}

// alloc checks quota, assigns a network slot and persists v in state st. The slow
// part (btrfs subvolume, conntrack) runs outside m.mu: under the lock only the slot
// is reserved, in m.allocating, which quota and slot accounting count.
func (m *Manager) alloc(v VM, st State) (VM, error) {
	m.mu.Lock()
	v, err := m.reserveVMLocked(v, st)
	m.mu.Unlock()
	if err != nil {
		return VM{}, err
	}
	if err := m.makeVMDir(v); err != nil {
		m.dropReservation(v.ID, "")
		return VM{}, err
	}
	m.flushConntrack(v.IP, v.IPv6, v.PublicIP) // a previous owner's flows
	if v.PendingCopy {
		return m.publishPending(v)
	}
	m.mu.Lock()
	delete(m.allocating, v.ID)
	// The reservation leaves m.allocating in the same critical section that publishes
	// the VM, so quota and slot accounting never see a gap.
	if err := m.commitLocked(VM{}, v, "created"); err != nil {
		m.allocating[v.ID] = v // keep the slot reserved until the dir is gone
		m.mu.Unlock()
		m.dropReservation(v.ID, v.ID)
		return VM{}, err
	}
	m.mu.Unlock()
	return v, nil
}

// publishPending publishes a new copy awaiting authorization. Its vm.json is fsynced
// (saveVMState), and the fsync must not run under m.mu: the write happens first, with
// the reservation still held in m.allocating (quota and slot accounting keep counting
// it, nothing can see the VM), and only a durable VM is swapped in. A failed write
// publishes nothing and rolls the reservation back, as the inline commit did.
func (m *Manager) publishPending(v VM) (VM, error) {
	m.mu.Lock()
	sv := m.stampLocked(VM{}, v)
	p := m.persistLocked(v.ID)
	p.seq++
	seq := p.seq
	save := m.saveState
	m.mu.Unlock()
	err := p.write(seq, true, v.ID, m.vmDir(v.ID), func() error { return save(m.vmDir(v.ID), sv) })
	if err != nil {
		m.dropReservation(v.ID, v.ID) // the slot stays reserved until the dir is gone
		return VM{}, err
	}
	m.mu.Lock()
	delete(m.allocating, v.ID)
	delete(m.dirty, v.ID)
	m.applyLocked(VM{}, sv, "created")
	m.mu.Unlock()
	return v, nil
}

// dropReservation releases a reservation after a failed alloc, removing the VM dir
// first (removeDir != "") so the slot is never reused while the dir still exists.
func (m *Manager) dropReservation(id, removeDir string) {
	if removeDir != "" {
		m.removeVMDir(removeDir)
	}
	m.mu.Lock()
	delete(m.allocating, id)
	m.mu.Unlock()
}

// reserveVMLocked checks quota and picks the ID and network slot, recording v in
// m.allocating. Caller holds m.mu.
func (m *Manager) reserveVMLocked(v VM, st State) (VM, error) {
	if err := m.quotaLocked(v.Team, Size{v.VCPU, v.MemMiB, v.DiskGiB}, ""); err != nil {
		return VM{}, err
	}
	if err := m.storageQuotaLocked(v.Team, 1, 0, vmCommit(v)); err != nil {
		return VM{}, err
	}
	used, pub := map[int]bool{}, map[string]bool{}
	m.eachVMLocked(func(o VM) { used[o.Index], pub[o.PublicIP] = true, true })
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
	m.allocating[v.ID] = v
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
		if err := os.MkdirAll(link, 0o700); err != nil {
			return err
		}
		noCompress(link) // before anything is created in it
		return nil
	}
	if err := m.ensureTeamVol(team); err != nil {
		return err
	}
	d := filepath.Join(m.teamVol(team), kind, id)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	noCompress(d)
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

type syncNetBatch struct {
	ready chan struct{} // closed when this batch may start executing
	done  chan struct{} // closed when this batch finishes executing
	err   error
}

// syncNet reloads nftables and routes.json from the current VM set. Concurrent
// callers arriving while a reload is in flight coalesce into a single follow-up
// pass (group-commit), guaranteeing every caller observes a reload that started
// after its mutation while avoiding N sequential `nft -f -` reloads.
// An nft error means the isolation rules are not in force: callers starting a VM
// must fail rather than run it unfiltered. Already-published ready VMs remain live
// in routes.json across reloads (zero host-wide blackout); newly-promoted VMs
// are withheld from routes.json until nft -f - succeeds.
func (m *Manager) syncNet() error {
	m.syncNetMu.Lock()
	if m.syncNetRun == nil {
		b := &syncNetBatch{done: make(chan struct{})}
		m.syncNetRun = b
		m.syncNetMu.Unlock()
		return m.runSyncNetBatch(b)
	}
	if m.syncNetWait != nil {
		b := m.syncNetWait
		m.syncNetMu.Unlock()
		<-b.done
		return b.err
	}
	b := &syncNetBatch{ready: make(chan struct{}), done: make(chan struct{})}
	m.syncNetWait = b
	m.syncNetMu.Unlock()
	<-b.ready
	return m.runSyncNetBatch(b)
}

func (m *Manager) runSyncNetBatch(b *syncNetBatch) (err error) {
	defer func() {
		m.syncNetMu.Lock()
		b.err = err
		close(b.done)
		if m.syncNetWait != nil {
			next := m.syncNetWait
			m.syncNetRun = next
			m.syncNetWait = nil
			close(next.ready)
		} else {
			m.syncNetRun = nil
		}
		m.syncNetMu.Unlock()
	}()
	return m.syncNetOnce()
}

func (m *Manager) syncNetOnce() error {
	m.netMu.Lock()
	defer m.netMu.Unlock()
	vms := m.list("")
	rs := ruleset(m.cfg, vms)
	routesPath := filepath.Join(m.cfg.RunDir, "routes.json")
	if m.lastNftRuleset != rs || len(m.dynQuarantined) > 0 || len(m.dynSSHPaused) > 0 {
		preVMs := m.preNftVMsLocked(vms)
		preRF := buildRouteFile(m.cfg, preVMs)
		if m.lastRoutesPath != routesPath || routePreSyncNeeded(m.lastRoutes, preRF) {
			if err := writeRouteFile(m.cfg, preRF); err != nil {
				m.lastRoutes = nil
				m.lastNftRuleset = ""
				clear(m.dynQuarantined)
				clear(m.dynSSHPaused)
				preRS := ruleset(m.cfg, preVMs)
				ferr := m.run(preRS, "nft", "-f", "-")
				if ferr != nil {
					m.stopUnfencedGuests(preVMs)
				} else {
					m.recordNftLocked(preVMs)
				}
				return errors.Join(fmt.Errorf("routes: %w", err), ferr)
			}
			cp := preRF
			m.lastRoutes = &cp
			m.lastRoutesPath = routesPath
		}
		if err := m.run(rs, "nft", "-f", "-"); err != nil {
			m.lastNftRuleset = ""
			clear(m.dynQuarantined)
			clear(m.dynSSHPaused)
			m.stopUnfencedGuests(vms)
			return fmt.Errorf("nft: %w", err)
		}
		m.lastNftRuleset = rs
		clear(m.dynQuarantined)
		clear(m.dynSSHPaused)
	}
	postRF := buildRouteFile(m.cfg, vms)
	_, statErr := os.Stat(routesPath)
	if statErr != nil || m.lastRoutes == nil || m.lastRoutesPath != routesPath || !sameRouteFile(*m.lastRoutes, postRF) {
		if err := writeRouteFile(m.cfg, postRF); err != nil {
			m.lastRoutes = nil
			m.lastNftRuleset = ""
			clear(m.dynQuarantined)
			clear(m.dynSSHPaused)
			preVMs := m.preNftVMsLocked(vms)
			ferr := m.run(ruleset(m.cfg, preVMs), "nft", "-f", "-")
			if ferr != nil {
				m.stopUnfencedGuests(preVMs)
			} else {
				m.recordNftLocked(preVMs)
			}
			return errors.Join(fmt.Errorf("routes: %w", err), ferr)
		}
		cp := postRF
		m.lastRoutes = &cp
		m.lastRoutesPath = routesPath
	}
	m.recordNftLocked(vms)
	return nil
}

// syncQuarantine fences v via incremental `nft add element` on the dedicated
// quarantine sets when the tables are already loaded, avoiding a full `nft -f -`
// reload on every fork/snapshot pause. Falls back to full syncNet if the table is
// not initialized or the incremental update fails (fail-closed preserved).
func (m *Manager) syncQuarantine(v VM) error {
	m.netMu.Lock()
	if m.lastNftRuleset == "" || v.IP == "" || v.Tap == "" || v.SSHBootEpoch == "" || m.publishedSSHPaused[v.ID] != "" {
		m.netMu.Unlock()
		return m.syncNet()
	}
	routesPath := filepath.Join(m.cfg.RunDir, "routes.json")
	if m.publishedQuarantined[v.ID] == v.SSHBootEpoch && m.publishedReady[v.ID] == "" {
		vms := m.list("")
		preRF := buildRouteFile(m.cfg, m.preNftVMsLocked(vms))
		if m.lastRoutesPath != routesPath || routePreSyncNeeded(m.lastRoutes, preRF) {
			if err := writeRouteFile(m.cfg, preRF); err != nil {
				m.lastRoutes = nil
				m.lastNftRuleset = ""
				clear(m.dynQuarantined)
				clear(m.dynSSHPaused)
				m.netMu.Unlock()
				return errors.Join(fmt.Errorf("routes: %w", err), m.syncNet())
			}
			cp := preRF
			m.lastRoutes = &cp
			m.lastRoutesPath = routesPath
		}
		m.netMu.Unlock()
		return nil
	}
	if m.publishedReady[v.ID] != v.SSHBootEpoch {
		m.netMu.Unlock()
		return m.syncNet()
	}
	vms := m.list("")
	preVMs := m.preNftVMsLocked(vms)
	preRF := buildRouteFile(m.cfg, preVMs)
	if m.lastRoutesPath != routesPath || routePreSyncNeeded(m.lastRoutes, preRF) {
		if err := writeRouteFile(m.cfg, preRF); err != nil {
			m.lastRoutes = nil
			m.lastNftRuleset = ""
			clear(m.dynQuarantined)
			clear(m.dynSSHPaused)
			m.netMu.Unlock()
			return errors.Join(fmt.Errorf("routes: %w", err), m.syncNet())
		}
		cp := preRF
		m.lastRoutes = &cp
		m.lastRoutesPath = routesPath
	}
	if err := m.run("", "nft", quarantineNFTArgs("add", v)...); err != nil {
		m.lastNftRuleset = ""
		clear(m.dynQuarantined)
		clear(m.dynSSHPaused)
		m.netMu.Unlock()
		return m.syncNet()
	}
	if m.dynQuarantined == nil {
		m.dynQuarantined = map[string]VM{}
	}
	m.dynQuarantined[v.ID] = v
	delete(m.publishedReady, v.ID)
	m.publishedQuarantined[v.ID] = v.SSHBootEpoch
	m.netMu.Unlock()
	return nil
}

// syncSSHPause fences new inbound TCP/22 SYNs to an already-ready VM via
// incremental `nft add element` on the @ssh_pause_* sets without removing v
// from routes.json or disrupting existing HTTP/WebSocket/SSE sessions. Falls
// back to full syncNet if the table is not loaded or incremental add fails.
func (m *Manager) syncSSHPause(v VM) error {
	m.netMu.Lock()
	if m.lastNftRuleset == "" || v.IP == "" || v.Tap == "" || v.SSHBootEpoch == "" ||
		m.publishedReady[v.ID] != v.SSHBootEpoch {
		m.netMu.Unlock()
		return m.syncNet()
	}
	if m.publishedSSHPaused[v.ID] == v.SSHBootEpoch {
		m.netMu.Unlock()
		return nil
	}
	if err := m.run("", "nft", sshPauseNFTArgs("add", v)...); err != nil {
		m.lastNftRuleset = ""
		clear(m.dynQuarantined)
		clear(m.dynSSHPaused)
		m.netMu.Unlock()
		return m.syncNet()
	}
	if m.dynSSHPaused == nil {
		m.dynSSHPaused = map[string]VM{}
	}
	if m.publishedSSHPaused == nil {
		m.publishedSSHPaused = map[string]string{}
	}
	m.dynSSHPaused[v.ID] = v
	m.publishedSSHPaused[v.ID] = v.SSHBootEpoch
	m.netMu.Unlock()
	return nil
}

// syncClearSSHPause releases v from the transient @ssh_pause_* sets via
// incremental `nft delete element` after rotate-token succeeds, avoiding a full
// `nft -f -` reload. Falls back to full syncNet on error or mismatched state.
func (m *Manager) syncClearSSHPause(v VM) error {
	m.netMu.Lock()
	if m.publishedSSHPaused[v.ID] == "" {
		m.netMu.Unlock()
		return nil
	}
	dp, inDyn := m.dynSSHPaused[v.ID]
	target := v
	if inDyn {
		target = dp
	}
	if m.lastNftRuleset == "" || !v.sshNetworkReady() ||
		target.IP != v.IP || target.IPv6 != v.IPv6 || target.Tap != v.Tap ||
		m.publishedSSHPaused[v.ID] != v.SSHBootEpoch || m.publishedReady[v.ID] != v.SSHBootEpoch {
		m.netMu.Unlock()
		return m.syncNet()
	}
	if err := m.run("", "nft", sshPauseNFTArgs("delete", target)...); err != nil {
		m.lastNftRuleset = ""
		clear(m.dynQuarantined)
		clear(m.dynSSHPaused)
		m.netMu.Unlock()
		return m.syncNet()
	}
	delete(m.dynSSHPaused, v.ID)
	delete(m.publishedSSHPaused, v.ID)
	if !inDyn {
		// v was loaded via static elements in lastNftRuleset during a mid-pause syncNet;
		// clear cached ruleset string so a future syncNet cannot mistake kernel state.
		m.lastNftRuleset = ""
	}
	m.netMu.Unlock()
	return nil
}

// syncUnquarantine releases v from the dedicated quarantine sets via incremental
// `nft delete element` after a pause/resume verification, updating routes.json only
// after nft succeeds. Falls back to full syncNet if v was not dynamically
// quarantined, the tables were reloaded, or the incremental update fails.
func (m *Manager) syncUnquarantine(v VM) error {
	m.netMu.Lock()
	dq, ok := m.dynQuarantined[v.ID]
	if !ok || m.lastNftRuleset == "" || !v.sshNetworkReady() ||
		dq.IP != v.IP || dq.IPv6 != v.IPv6 || dq.Tap != v.Tap ||
		m.publishedQuarantined[v.ID] != v.SSHBootEpoch {
		m.netMu.Unlock()
		return m.syncNet()
	}
	if err := m.run("", "nft", quarantineNFTArgs("delete", dq)...); err != nil {
		m.lastNftRuleset = ""
		clear(m.dynQuarantined)
		m.netMu.Unlock()
		return m.syncNet()
	}
	delete(m.dynQuarantined, v.ID)
	delete(m.publishedQuarantined, v.ID)
	m.publishedReady[v.ID] = v.SSHBootEpoch
	vms := m.list("")
	postRF := buildRouteFile(m.cfg, m.preNftVMsLocked(vms))
	routesPath := filepath.Join(m.cfg.RunDir, "routes.json")
	_, statErr := os.Stat(routesPath)
	if statErr != nil || m.lastRoutes == nil || m.lastRoutesPath != routesPath || !sameRouteFile(*m.lastRoutes, postRF) {
		if err := writeRouteFile(m.cfg, postRF); err != nil {
			// Re-fence immediately in nftables before returning error; caller also clears SSHPolicyReady and syncs.
			_ = m.run("", "nft", quarantineNFTArgs("add", dq)...)
			m.lastRoutes = nil
			m.lastNftRuleset = ""
			clear(m.dynQuarantined)
			delete(m.publishedReady, v.ID)
			m.netMu.Unlock()
			return fmt.Errorf("routes: %w", err)
		}
		cp := postRF
		m.lastRoutes = &cp
		m.lastRoutesPath = routesPath
	}
	m.netMu.Unlock()
	return nil
}

// syncRoutes updates routes.json (e.g. recording newly Running quarantined VM IPs)
// without reloading nftables when quarantine rules for those VMs are already active.
// Only VMs already committed in publishedReady are emitted as live routes.
func (m *Manager) syncRoutes() error {
	m.netMu.Lock()
	defer m.netMu.Unlock()
	vms := m.list("")
	preVMs := m.preNftVMsLocked(vms)
	rf := buildRouteFile(m.cfg, preVMs)
	routesPath := filepath.Join(m.cfg.RunDir, "routes.json")
	if err := writeRouteFile(m.cfg, rf); err != nil {
		m.lastRoutes = nil
		return fmt.Errorf("routes: %w", err)
	}
	cp := rf
	m.lastRoutes = &cp
	m.lastRoutesPath = routesPath
	return nil
}

func (m *Manager) preNftVMsLocked(vms []VM) []VM {
	pre := make([]VM, len(vms))
	copy(pre, vms)
	for i := range pre {
		if !pre[i].sshNetworkReady() || m.publishedReady[pre[i].ID] != pre[i].SSHBootEpoch {
			pre[i].SSHPolicyReady = false
		}
	}
	return pre
}

func (m *Manager) recordNftLocked(vms []VM) {
	clear(m.publishedReady)
	clear(m.publishedQuarantined)
	clear(m.publishedSSHPaused)
	for _, v := range vms {
		if v.sshNetworkReady() {
			m.publishedReady[v.ID] = v.SSHBootEpoch
			if v.sshPauseFenced {
				if m.publishedSSHPaused == nil {
					m.publishedSSHPaused = map[string]string{}
				}
				m.publishedSSHPaused[v.ID] = v.SSHBootEpoch
			}
		} else if v.SSHBootEpoch != "" && v.State != Stopped && v.State != Error {
			m.publishedQuarantined[v.ID] = v.SSHBootEpoch
		}
	}
}

func (m *Manager) isSSHPausedInNft(v VM) bool {
	if v.SSHBootEpoch == "" {
		return false
	}
	m.netMu.Lock()
	defer m.netMu.Unlock()
	return m.publishedSSHPaused[v.ID] == v.SSHBootEpoch
}

func (m *Manager) isQuarantinedInNft(v VM) bool {
	if v.SSHBootEpoch == "" {
		return false
	}
	m.netMu.Lock()
	defer m.netMu.Unlock()
	return m.publishedQuarantined[v.ID] == v.SSHBootEpoch && m.publishedReady[v.ID] == ""
}

func (m *Manager) isPublishedReadyInNft(v VM) bool {
	if !v.sshNetworkReady() {
		return false
	}
	m.netMu.Lock()
	defer m.netMu.Unlock()
	return m.publishedReady[v.ID] == v.SSHBootEpoch && m.lastRoutes != nil && m.lastRoutes.VMs[v.ID].IP == v.IP
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
	if v.PendingCopy || v.State != Running {
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
	m.finishTombstones()
	all, err := loadAll[VM](filepath.Join(m.cfg.StateDir, "vms"), "vm.json")
	if err != nil {
		return err
	}
	vms := all[:0:0]
	for _, v := range all {
		if m.tombstoned(v.ID) {
			// Its delete started and could not be finished: never resume it. It stays
			// as a terminal error VM (its disk still counts against its team) until
			// the background retry removes the dir.
			log.Printf("%s: delete was interrupted and could not be finished; keeping it as an error VM", v.ID)
			v = doomedVM(v)
		}
		vms = append(vms, v)
	}
	// A dir whose removal failed half way may have lost its vm.json: the tombstone's
	// copy of the record keeps the VM known (and counted) until the dir is gone.
	known := map[string]bool{}
	for _, v := range vms {
		known[v.ID] = true
	}
	for _, id := range m.tombstonedIDs() {
		if rec, ok := m.readTombstone(id); ok && !known[id] && exists(m.vmDir(id)) {
			vms = append(vms, doomedVM(rec))
		}
	}
	for i := range vms {
		v := &vms[i]
		epoch, err := m.newSSHEpoch()
		if err != nil {
			return err
		}
		v.SSHBootEpoch, v.SSHPolicyReady, v.SSHPolicyDigest, v.sshConfig, v.sshHelperDigest, v.sshPauseFenced = epoch, false, "", nil, "", false
		m.vms[v.ID] = *v
		if v.State.active() || v.PID > 0 {
			m.markConntrackDirty(v.IP, v.IPv6, v.PublicIP)
		}
	}
	// A live adopted guest must be fenced before any agent work or publication.
	if err := m.syncNet(); err != nil {
		return err
	}
	for _, s := range snaps {
		if _, ok := m.snaps[s.ID]; !ok {
			continue // went with a VM whose interrupted delete was just finished
		}
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
	promotedAdopted := false
	for _, v := range vms {
		if v.PendingCopy {
			if v.PID > 0 && m.procs.Alive(v.PID, v.ID) {
				m.procs.Kill(v.PID)
			}
			nv := v.clone()
			nv.State, nv.PID, nv.AgentReady, nv.ResumeOnBoot = Forking, 0, false, false
			if nv.HasSnapshot && !nv.SnapSynced {
				os.Remove(filepath.Join(m.root(v.ID), vmstate))
				os.Remove(filepath.Join(m.root(v.ID), memFile))
				nv.HasSnapshot = false
			}
			m.mu.Lock()
			err := m.commitLocked(v, nv, "")
			m.mu.Unlock()
			if err != nil {
				return err
			}
			continue
		}
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
			if m.cfg.standaloneReady(v) {
				if _, err := m.update(v.ID, "", func(x *VM) { x.SSHPolicyReady = true }); err != nil {
					return err
				}
				promotedAdopted = true
			}
			go func(v VM) { // a crash may have left them lifted; a new dump owns its limits
				unlock := m.lock(v.ID)
				defer unlock()
				if m.dumpGeneration(v.ID) == 0 {
					m.setMemLimits(v)
				}
			}(v)
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
		billed := v
		if v.StartedAt != nil && m.usage.hasVM(v.ID, *v.StartedAt) {
			// The stop that ended this run closed its interval in the ledger, but its
			// vm.json never became durable: do not bill the run a second time.
			billed.StartedAt = nil
		}
		m.mu.Lock()
		err := m.commitLocked(billed, nv, "boatd restart")
		m.mu.Unlock()
		if err != nil {
			return err
		}
		delVMNet(m.run, v)
	}
	if promotedAdopted {
		return m.syncNet()
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

func (m *Manager) closeWatches() {
	m.watchOnce.Do(func() { close(m.watchStop) })
	m.flushUsage()
}

// flushUsage makes every ledger record written so far durable (shutdown, barriers).
func (m *Manager) flushUsage() {
	if m.usage == nil {
		return
	}
	if err := m.usage.flush(); err != nil {
		log.Printf("usage: flush: %v", err)
	}
}

// watch marks a VM stopped when its firecracker exits on its own (guest
// reboot/poweroff, crash). Exits caused by our own ops find PID changed.
func (m *Manager) watch(id string, pid int, done <-chan struct{}) {
	select {
	case <-done:
	case <-m.watchStop:
		return
	}
	closeAgentConns(filepath.Join(m.root(id), vsockUDS)) // the VMM is gone: so are its vsock conns
	unlock := m.lock(id)
	defer unlock()
	select {
	case <-m.watchStop:
		return
	default:
	}
	v, err := m.get(id)
	if err != nil || v.PID != pid || v.State != Running {
		return
	}
	log.Printf("%s: firecracker pid %d exited on its own (cgroup oom_kill %d)", id, pid, m.oomKills(id))
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
	closeAgentConns(filepath.Join(m.root(id), vsockUDS)) // the guest's vsock dies with the VMM
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
	// done closes immediately with pidfd (Linux) or cmd.Wait; if Adopt fell back to
	// polling (capped at 50ms), poll Alive with fast exponential backoff so killWait
	// still detects VMM exit in ~1-2ms instead of waiting for Adopt's next tick.
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; ; i++ {
		select {
		case <-done:
			return
		default:
		}
		if !m.procs.Alive(pid, id) {
			return
		}
		rem := time.Until(deadline)
		if rem <= 0 {
			log.Printf("%s: pid %d did not exit after SIGKILL", id, pid)
			return
		}
		timer := time.NewTimer(min(procExitStep(i), rem))
		select {
		case <-done:
			timer.Stop()
			return
		case <-timer.C:
		}
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

// flushDirty retries state writes that hit a full disk. The writes run outside m.mu,
// through the same per-VM ordering as every commit, so a retry never lands over a
// newer state and never holds up the API behind an fsync.
func (m *Manager) flushDirty() {
	type job struct {
		id  string
		p   *vmPersist
		seq uint64
		v   VM
	}
	var jobs []job
	m.mu.Lock()
	save := m.saveState
	for id := range m.dirty {
		v, ok := m.vms[id]
		if !ok {
			delete(m.dirty, id)
			continue
		}
		p := m.persistLocked(id)
		p.seq++
		p.inflight++
		jobs = append(jobs, job{id, p, p.seq, v})
	}
	m.mu.Unlock()
	errs := make([]error, len(jobs))
	for i, j := range jobs {
		errs[i] = j.p.write(j.seq, needsSync(j.v), j.id, m.vmDir(j.id), func() error { return save(m.vmDir(j.id), j.v) })
	}
	m.mu.Lock()
	for i, j := range jobs {
		j.p.inflight--
		if errs[i] == nil && j.p.seq == j.seq {
			delete(m.dirty, j.id)
		}
	}
	m.mu.Unlock()
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

// shutdownReason: a stop that cannot wait for a retry (stop falls back to a cold shutdown).
const shutdownReason = "boatd shutdown"

func (m *Manager) stopAllLimit() int {
	if n := cap(m.dumpSlots); n > 0 {
		return n
	}
	return maxConcurrentDumps
}

// stopAll snapshots every running VM with bounded concurrency (service stop / host reboot).
func (m *Manager) stopAll(ctx context.Context, reason string) {
	var running []VM
	for _, v := range m.list("") {
		if v.State == Running {
			running = append(running, v)
		}
	}
	if len(running) > 0 {
		limit := min(m.stopAllLimit(), len(running))
		jobs := make(chan VM, len(running))
		for _, v := range running {
			jobs <- v
		}
		close(jobs)
		var wg sync.WaitGroup
		for range limit {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for v := range jobs {
					stopped, err := m.stopOpts(ctx, v.ID, reason, false, false, true)
					if err != nil {
						log.Printf("%s: stop: %v", v.ID, err)
					}
					if stopped.State == Stopped && stopped.HasSnapshot {
						m.syncSnapshot(v.ID) // we are exiting: ensure snapshot is durable before return
					}
				}
			}()
		}
		wg.Wait()
	}
	m.waitSnapSyncs(ctx)
	m.flushUsage() // the stops above closed billing intervals: durable before we exit
}

// resumeAll starts VMs that stopAll stopped (in the background).
func (m *Manager) resumeAll(ctx context.Context) {
	for _, v := range m.list(warmTeam) { // a template build cut short by a restart
		if _, err := m.Delete(ctx, v.ID); err != nil {
			log.Printf("%s: %v", v.ID, err)
		}
	}
	for _, v := range m.list("") {
		if !v.PendingCopy && v.ResumeOnBoot && v.State == Stopped {
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

// reap stops VMs past their TTL or idle limit. Called every 30 s. It checks host-side
// TTL and host-recorded last activity first so idle guests are not woken by a vsock
// health RPC every tick, and acquires a reapSlots token before spawning a goroutine so
// concurrency stays bounded at reapParallel. A VM already being handled or holding its
// lifecycle lock is skipped until the next tick.
func (m *Manager) reap(ctx context.Context) {
	now := m.now()
	for _, v := range m.list("") {
		if v.State != Running {
			continue
		}
		ttlDue := v.ExpiresAt != nil && now.After(*v.ExpiresAt)
		limit := m.idleLimit(v)
		idleDue := !ttlDue && limit > 0 && now.Sub(m.hostLastActive(v)) > limit
		if !ttlDue && !idleDue {
			continue
		}
		if m.lockBusy(v.ID) {
			continue
		}
		m.mu.Lock()
		busy := m.reaping[v.ID]
		if !busy {
			m.reaping[v.ID] = true
		}
		m.mu.Unlock()
		if busy {
			continue
		}
		if m.reapSlots != nil {
			select {
			case m.reapSlots <- struct{}{}:
			case <-ctx.Done():
				m.mu.Lock()
				delete(m.reaping, v.ID)
				m.mu.Unlock()
				return
			}
		}
		m.reapWG.Add(1)
		go func(v VM) {
			defer m.reapWG.Done()
			defer func() {
				m.mu.Lock()
				delete(m.reaping, v.ID)
				m.mu.Unlock()
				if m.reapSlots != nil {
					<-m.reapSlots
				}
			}()
			m.reapVM(ctx, v, now)
		}(v)
	}
}

func (m *Manager) reapVM(ctx context.Context, v VM, now time.Time) {
	reason := ""
	if v.ExpiresAt != nil && now.After(*v.ExpiresAt) {
		reason = "ttl"
	} else if limit := m.idleLimit(v); limit > 0 {
		last := m.hostLastActive(v)
		if now.Sub(last) > limit && now.Sub(m.guestLastActive(ctx, v, last)) > limit {
			reason = "idle"
		}
	}
	if reason == "" || m.lockBusy(v.ID) {
		return
	}
	if reason == "ttl" { // a PATCH may have moved the deadline while this waited for a slot
		if cur, err := m.get(v.ID); err != nil || cur.ExpiresAt == nil || !now.After(*cur.ExpiresAt) {
			return
		}
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

// hostLastActive returns the latest activity timestamp known to the host (boot/start
// or boatd exec/file/desktop calls) without contacting the guest agent.
func (m *Manager) hostLastActive(v VM) time.Time {
	last := v.CreatedAt
	if v.StartedAt != nil {
		last = *v.StartedAt
	}
	m.mu.Lock()
	if t := m.active[v.ID]; t.After(last) {
		last = t
	}
	m.mu.Unlock()
	return last
}

// lastActive = latest of boot, boatd-seen activity and the agent's idle_s
// (exec, ssh sessions, desktop input inside the guest).
func (m *Manager) lastActive(ctx context.Context, v VM) time.Time {
	return m.guestLastActive(ctx, v, m.hostLastActive(v))
}

func (m *Manager) guestLastActive(ctx context.Context, v VM, last time.Time) time.Time {
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

// If nft cannot install a quarantine, old established rules may still admit
// traffic. Stop unverified processes rather than claiming a fence exists.
// netMu is held; no VM lifecycle lock is acquired (watchers also call syncNet).
func (m *Manager) stopUnfencedGuests(vms []VM) {
	for _, v := range vms {
		if v.sshNetworkReady() || v.PID <= 0 {
			continue
		}
		current, err := m.get(v.ID)
		if err != nil || current.PID != v.PID || current.SSHBootEpoch != v.SSHBootEpoch {
			continue
		}
		m.killWait(v.ID, v.PID)
		m.mu.Lock()
		current, ok := m.vms[v.ID]
		if ok && current.PID == v.PID && current.SSHBootEpoch == v.SSHBootEpoch {
			nv := current.clone()
			nv.State, nv.PID, nv.SSHPolicyReady = Error, 0, false
			nv.Error = "network quarantine could not be installed"
			// The Error state swaps in under the lock (the denial holds even if storage
			// fails: flushDirty retries the file); its fsync runs after the unlock.
			_, finish := m.commitDeferLocked(current, nv, "quarantine failed")
			m.mu.Unlock()
			if err := finish(); err != nil {
				log.Printf("%s: quarantine failure state: %v", v.ID, err)
			}
			continue
		}
		m.mu.Unlock()
	}
}
