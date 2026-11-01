package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
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
	AgentReady   bool              `json:"agent_ready"`
	DiffOK       bool              `json:"diff_ok,omitempty"`        // root/mem (or zeros) is a valid base for a diff snapshot
	ResumeOnBoot bool              `json:"resume_on_boot,omitempty"` // stopped by a boatd shutdown
	Ports        []Port            `json:"ports"`
	TTLS         int               `json:"ttl_s,omitempty"`
	IdleStopS    int               `json:"idle_stop_s,omitempty"`
	ExpiresAt    *time.Time        `json:"expires_at,omitempty"`
	ParentID     string            `json:"parent_id,omitempty"`
	FromSnapshot string            `json:"from_snapshot,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	StartedAt    *time.Time        `json:"started_at,omitempty"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

func (v VM) clone() VM {
	c := v
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
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	VMID      string    `json:"vm_id"`
	Team      string    `json:"team"`
	Image     string    `json:"image"`
	Size      string    `json:"size"`
	VCPU      int       `json:"vcpu"`
	MemMiB    int       `json:"mem_mib"`
	DiskGiB   int       `json:"disk_gib"`
	HasMem    bool      `json:"has_mem"`
	CreatedAt time.Time `json:"created_at"`
}

func newID(prefix string) string {
	b := make([]byte, 6)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// writeJSONAtomic writes tmp, fsyncs, renames: readers see old or new, never half.
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
	return os.Rename(tmp, path)
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
