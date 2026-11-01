package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Event is one state change, streamed on GET /v1/events (SSE).
type Event struct {
	Seq    int64     `json:"seq"`
	Time   time.Time `json:"time"`
	Type   string    `json:"type"` // vm.state | vm.deleted | snapshot.created | snapshot.deleted
	VMID   string    `json:"vm_id,omitempty"`
	SnapID string    `json:"snapshot_id,omitempty"`
	Team   string    `json:"team"`
	Size   string    `json:"size,omitempty"`
	State  State     `json:"state,omitempty"`
	Prev   State     `json:"prev,omitempty"`
	Reason string    `json:"reason,omitempty"` // ttl | idle | exited | ...
}

const eventRing = 1024

// broker fans events out to SSE subscribers and keeps the last eventRing for
// Last-Event-ID resume. Slow subscribers are dropped (they reconnect with
// Last-Event-ID); events are lost only if they fall more than eventRing behind.
type broker struct {
	mu   sync.Mutex
	seq  int64
	ring []Event
	subs map[chan Event]struct{}
}

func newBroker() *broker { return &broker{subs: map[chan Event]struct{}{}} }

func (b *broker) publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Seq = b.seq
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	b.ring = append(b.ring, e)
	if len(b.ring) > eventRing {
		b.ring = b.ring[len(b.ring)-eventRing:]
	}
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
			delete(b.subs, ch)
			close(ch)
		}
	}
}

// subscribe returns events after seq `after` plus a live channel.
func (b *broker) subscribe(after int64) ([]Event, chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var backlog []Event
	for _, e := range b.ring {
		if e.Seq > after {
			backlog = append(backlog, e)
		}
	}
	ch := make(chan Event, 256)
	b.subs[ch] = struct{}{}
	return backlog, ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
	}
}

// usageRec is one closed billing interval, appended to usage.jsonl.
type usageRec struct {
	Kind  string    `json:"kind"` // vm | snapshot
	ID    string    `json:"id"`
	Team  string    `json:"team"`
	Size  string    `json:"size,omitempty"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type ledger struct {
	mu   sync.Mutex
	path string
}

func (l *ledger) append(r usageRec) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// ponytail: full scan of usage.jsonl per query; rotate/aggregate if it grows past ~100 MB.
func (l *ledger) read() ([]usageRec, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []usageRec
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r usageRec
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

type vmUsage struct {
	ID      string  `json:"id"`
	Team    string  `json:"team"`
	Size    string  `json:"size"`
	Seconds float64 `json:"vm_seconds"`
}

type snapUsage struct {
	ID   string  `json:"id"`
	Team string  `json:"team"`
	Days float64 `json:"snapshot_days"`
}

type teamUsage struct {
	Team         string             `json:"team"`
	VMSeconds    float64            `json:"vm_seconds"`
	BySize       map[string]float64 `json:"vm_seconds_by_size"`
	SnapshotDays float64            `json:"snapshot_days"`
}

type usageReport struct {
	Since     time.Time     `json:"since"`
	Until     time.Time     `json:"until"`
	VMs       []vmUsage     `json:"vms"`
	Snapshots []snapUsage   `json:"snapshots"`
	Teams     []teamUsage   `json:"teams"`
	Storage   []teamStorage `json:"storage"` // bytes now, per team
	Disk      *diskSpace    `json:"disk,omitempty"`
}

// summarize clips intervals (closed + still-open) to [since, until].
func summarize(recs []usageRec, since, until time.Time) usageReport {
	rep := usageReport{Since: since, Until: until, VMs: []vmUsage{}, Snapshots: []snapUsage{}, Teams: []teamUsage{}}
	vmIdx, snIdx, tIdx := map[string]int{}, map[string]int{}, map[string]int{}
	team := func(t string) *teamUsage {
		i, ok := tIdx[t]
		if !ok {
			i = len(rep.Teams)
			tIdx[t] = i
			rep.Teams = append(rep.Teams, teamUsage{Team: t, BySize: map[string]float64{}})
		}
		return &rep.Teams[i]
	}
	for _, r := range recs {
		s, e := r.Start, r.End
		if s.Before(since) {
			s = since
		}
		if e.After(until) {
			e = until
		}
		d := e.Sub(s).Seconds()
		if d <= 0 {
			continue
		}
		t := team(r.Team)
		if r.Kind == "snapshot" {
			i, ok := snIdx[r.ID]
			if !ok {
				i = len(rep.Snapshots)
				snIdx[r.ID] = i
				rep.Snapshots = append(rep.Snapshots, snapUsage{ID: r.ID, Team: r.Team})
			}
			rep.Snapshots[i].Days += d / 86400
			t.SnapshotDays += d / 86400
			continue
		}
		i, ok := vmIdx[r.ID]
		if !ok {
			i = len(rep.VMs)
			vmIdx[r.ID] = i
			rep.VMs = append(rep.VMs, vmUsage{ID: r.ID, Team: r.Team, Size: r.Size})
		}
		rep.VMs[i].Seconds += d
		t.VMSeconds += d
		t.BySize[r.Size] += d
	}
	return rep
}
