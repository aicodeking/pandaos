package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type State string

const (
	Creating State = "creating"
	Running  State = "running"
	Stopping State = "stopping"
	Stopped  State = "stopped"
	Starting State = "starting"
	Forking  State = "forking"
	Error    State = "error"
)

// active states count against quotas and accrue VM-seconds.
func (s State) active() bool { return s != Stopped && s != Error }

type Port struct {
	GuestPort int    `json:"guest_port"`
	HostPort  int    `json:"host_port,omitempty"` // public TCP forward (public:true)
	Hostname  string `json:"hostname"`            // <port>-<id>.vm.<domain>, served by boat-gateway
}

// VM is persisted as vms/<id>/vm.json. Values are copied, never shared:
// Manager.update copies, applies, persists, then swaps the map entry.
type VM struct {
	SSHBootEpoch    string `json:"ssh_boot_epoch"`
	SSHPolicyReady  bool   `json:"ssh_policy_ready"`
	SSHPolicyDigest string `json:"ssh_policy_digest,omitempty"`
	sshConfig       []byte // runtime only: lookup credential must never enter vm.json
	sshHelperDigest string // runtime only: sha256 of installed sshPolicyHelper on SSHBootEpoch
	sshPauseFenced  bool   // runtime only: transient SSH-only pause-fence while awaiting token rotation

	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Team         string            `json:"team"`
	Image        string            `json:"image"`
	Size         string            `json:"size"`
	VCPU         int               `json:"vcpu"`
	MemMiB       int               `json:"mem_mib"`
	DiskGiB      int               `json:"disk_gib"`
	Labels       map[string]string `json:"labels,omitempty"`
	State        State             `json:"state"`
	Error        string            `json:"error,omitempty"`
	Index        int               `json:"index"` // network slot: IP, MAC, tap, jail uid
	IP           string            `json:"ip"`
	MAC          string            `json:"mac"`
	IPv6         string            `json:"ipv6,omitempty"`
	PublicIP     string            `json:"public_ip,omitempty"`
	Tap          string            `json:"tap"`
	PID          int               `json:"pid,omitempty"`
	HasSnapshot  bool              `json:"has_snapshot"`          // stop snapshot (mem+vmstate) present
	SnapSynced   bool              `json:"snap_synced,omitempty"` // stop snapshot fsynced: safe after a host crash
	DiskGrown    bool              `json:"disk_grown,omitempty"`  // tell the guest after its next restore
	AgentReady   bool              `json:"agent_ready"`
	DiffOK       bool              `json:"diff_ok,omitempty"`        // root/mem (or zeros) is a valid base for a diff snapshot
	ResumeOnBoot bool              `json:"resume_on_boot,omitempty"` // stopped by a boatd shutdown
	Ports        []Port            `json:"ports"`
	TTLS         int               `json:"ttl_s,omitempty"`
	IdleStopS    int               `json:"idle_stop_s,omitempty"`
	ExpiresAt    *time.Time        `json:"expires_at,omitempty"`
	ParentID     string            `json:"parent_id,omitempty"`
	PendingCopy  bool              `json:"pending_copy,omitempty"`
	CopyReady    bool              `json:"copy_ready,omitempty"`
	FromSnapshot string            `json:"from_snapshot,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	StartedAt    *time.Time        `json:"started_at,omitempty"`
	StoppedAt    *time.Time        `json:"stopped_at,omitempty"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

func (v VM) clone() VM {
	c := v
	c.sshConfig = append([]byte(nil), v.sshConfig...)
	c.Ports = append([]Port{}, v.Ports...)
	if v.Labels != nil {
		c.Labels = make(map[string]string, len(v.Labels))
		for k, x := range v.Labels {
			c.Labels[k] = x
		}
	}
	return c
}

// Snapshot is a frozen copy of disk (+ mem/vmstate when taken from a running
// or snapshot-stopped VM). Persisted as snapshots/<id>/snap.json.
type Snapshot struct {
	PendingCopy bool              `json:"pending_copy,omitempty"`
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	VMID        string            `json:"vm_id"`
	Team        string            `json:"team"`
	Image       string            `json:"image"`
	Size        string            `json:"size"`
	VCPU        int               `json:"vcpu"`
	MemMiB      int               `json:"mem_mib"`
	DiskGiB     int               `json:"disk_gib"`
	HasMem      bool              `json:"has_mem"`
	NoEnv       bool              `json:"no_env,omitempty"`
	Protected   bool              `json:"protected,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Unsynced    bool              `json:"unsynced,omitempty"` // memory not fsynced yet: lost in a host crash
	CreatedAt   time.Time         `json:"created_at"`
}

func newID(prefix string) string {
	b := make([]byte, 6)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// writeJSONAtomic writes tmp, fsyncs, renames: readers see old or new, never half.
// writeJSONNoSync is writeJSONAtomic without the durability: a crash may lose the
// file, never tear it.
func writeJSONNoSync(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), mode); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func writeJSONAtomic(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after the rename
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// vmPersist orders the vm.json writes of one VM. Every commit takes a sequence number
// under m.mu (seq, inflight); the write itself runs outside m.mu, one at a time under
// mu, and a write whose sequence is not newer than what is on disk (written) is
// skipped: an older state never overwrites a newer one, and the caller of the older
// commit still returns only after a state at least as new is durable (see write).
type vmPersist struct {
	mu       sync.Mutex // held for the whole write (fsync included), never with m.mu
	written  uint64     // newest sequence on disk; vmPersistClosed once the VM is deleted
	synced   bool       // the file on disk was fsynced (under mu)
	seq      uint64     // last sequence handed out (under m.mu)
	inflight int        // writes handed out outside m.mu and not finished (under m.mu)
	pre      sync.Mutex // serializes updatePending's write-then-swap per VM; taken before m.mu, never inside it

	syncFn func(path string) error // fsync of vm.json; nil = syncFile. A test hook
}

// vmPersistClosed makes every later write a no-op (the VM dir is being removed).
const vmPersistClosed = ^uint64(0)

// write runs save unless a newer state was already written. durable says the caller
// needs the state on disk to survive a host crash (save fsyncs it): when a newer,
// non-fsynced state landed first, the older write is skipped but the file is fsynced
// here, so the caller never returns on a promise only a page cache keeps. A failed
// write leaves written alone, so the next (newer) sequence still goes through.
func (p *vmPersist) write(seq uint64, durable bool, id, dir string, save func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if seq <= p.written {
		if durable && !p.synced && p.written != vmPersistClosed {
			sf := p.syncFn
			if sf == nil {
				sf = syncFile
			}
			if err := sf(filepath.Join(dir, "vm.json")); err != nil {
				return fmt.Errorf("%s: sync newer state: %w", id, err)
			}
			p.synced = true
		}
		return nil
	}
	if err := save(); err != nil {
		return err
	}
	p.written, p.synced = seq, durable
	return nil
}

// closeWrites waits for the write in flight and turns every later one into a no-op.
func (p *vmPersist) closeWrites() {
	p.mu.Lock()
	p.written = vmPersistClosed
	p.mu.Unlock()
}

// reopen undoes closeWrites after a failed delete (the VM stays): writes numbered
// after seq (the newest handed out) go through again. The caller holds m.mu for seq.
func (p *vmPersist) reopen(seq uint64) {
	p.mu.Lock()
	if p.written == vmPersistClosed {
		p.written, p.synced = seq, false
	}
	p.mu.Unlock()
}

// syncFile fsyncs path and its directory entry.
func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// loadAll reads every <dir>/*/<file> into a T.
func loadAll[T any](dir, file string) ([]T, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*", file))
	if err != nil {
		return nil, err
	}
	out := []T{}
	for _, p := range paths {
		var t T
		if err := readJSON(p, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}
