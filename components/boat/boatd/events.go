package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sort"
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

// ledger is usage.jsonl, append-only. Records are appended when an interval closes,
// so their End times only grow (but for clock steps and racing appends, which the
// index tolerates): a sparse index of byte offsets lets a query for [since, now]
// start at the first record that can still end after since, not at byte 0.
type ledger struct {
	mu   sync.Mutex
	path string

	marks   []ledgerMark // every ledgerMarkEvery records, from offset 0
	indexed int64        // bytes of the file the index covers
	count   int          // records in it
	maxEnd  time.Time    // latest End in it
	built   bool
}

// ledgerMark: every record before off ends at or before maxEnd.
type ledgerMark struct {
	off    int64
	maxEnd time.Time
}

const ledgerMarkEvery = 1024

func (l *ledger) append(r usageRec) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	off, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	if l.built && off == l.indexed {
		l.note(r, off+int64(len(b))+1)
	} else {
		l.built = false // written behind our back: rebuild on the next read
	}
	return f.Sync()
}

// note adds the record that ends at byte next to the index.
func (l *ledger) note(r usageRec, next int64) {
	if r.End.After(l.maxEnd) {
		l.maxEnd = r.End
	}
	l.count++
	l.indexed = next
	if l.count%ledgerMarkEvery == 0 {
		l.marks = append(l.marks, ledgerMark{next, l.maxEnd})
	}
}

// read returns the records that may overlap [since, ...): those of the index's last
// mark that ends before since onwards. The first call (or one after the file changed
// behind boatd) scans the whole file once to build the index.
func (l *ledger) read(since time.Time) ([]usageRec, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if os.IsNotExist(err) {
		l.reset()
		l.built = true
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !l.built || st.Size() < l.indexed {
		l.reset()
		if err := l.scan(f, st.Size(), 0, time.Time{}, nil); err != nil {
			return nil, err
		}
		l.built = true
	}
	// marks[i].maxEnd grows with i: take the last mark wholly before since.
	i := sort.Search(len(l.marks), func(i int) bool { return !l.marks[i].maxEnd.Before(since) }) - 1
	from := int64(0)
	if i >= 0 {
		from = l.marks[i].off
	}
	var out []usageRec
	return out, l.scan(f, st.Size(), from, since, &out)
}

func (l *ledger) reset() {
	l.marks, l.indexed, l.count, l.maxEnd, l.built = nil, 0, 0, time.Time{}, false
}

// scan reads records from byte from. Without out it builds the index (from 0); with
// out it collects the records ending after since, and indexes what lies past l.indexed.
func (l *ledger) scan(f *os.File, size, from int64, since time.Time, out *[]usageRec) error {
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return err
	}
	sc := bufio.NewScanner(f)
	off := from
	for sc.Scan() {
		line := sc.Bytes()
		next := min(off+int64(len(line))+1, size) // a torn last line has no newline
		var r usageRec
		ok := json.Unmarshal(line, &r) == nil
		if ok && out != nil && r.End.After(since) {
			*out = append(*out, r)
		}
		if next > l.indexed && ok {
			l.note(r, next)
		} else if next > l.indexed {
			l.indexed = next // a torn line: skipped, like before
		}
		off = next
	}
	return sc.Err()
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
