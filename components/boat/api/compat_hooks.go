package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Deliveries to webhooks registered through /api/v1 use boat.dev's events,
// payload and headers (docs.boat.dev/webhooks): sandbox.ready when a VM
// becomes usable, sandbox.archived when it stops, sandbox.error when it
// fails; data.sandbox.id is its bx_ id; signed with X-Ascii-Signature
// v1=hex(HMAC-SHA256(secret, delivery_id + "." + timestamp + "." + body)).
// hydrated, degraded and recovered have no source here and are never sent.
//
// sandbox.ready means "usable", which includes its environment: boatd publishes
// vm.state running before boat-api has pushed the VM's env, so while a create,
// fork or cold start is still pushing one (envGate.expect) the running event is
// held. It is sent once the push succeeded (recordEnvResult), and dropped for a
// sandbox.error carrying the env error when the push failed. The native vm.*
// events and boatd's own "state":"running" do not wait: they say the VM runs, not
// that its environment arrived (GET /v1/vms/{id} shows agent_ready and env_error).

// boatDevEvent turns a native vm.state event into boat.dev's, or ok=false.
func (d *dispatcher) boatDevEvent(ctx context.Context, ev Event) (Event, bool) {
	return d.boatDevEventOpt(ctx, ev, true)
}

// boatDevEventOpt is boatDevEvent; hold=false sends a running event the gate
// already held (it must not be held again).
func (d *dispatcher) boatDevEventOpt(ctx context.Context, ev Event, hold bool) (Event, bool) {
	var raw map[string]any
	if ev.Type != "vm.state" || json.Unmarshal(ev.Data, &raw) != nil {
		return Event{}, false
	}
	state, prev := str(raw, "state"), str(raw, "prev")
	typ := ""
	switch {
	case state == "running" && prev != "running":
		typ = "sandbox.ready"
	case state == "stopped" && prev != "stopped":
		typ = "sandbox.archived"
	case state == "error" && prev != "error":
		typ = "sandbox.error"
	default:
		return Event{}, false
	}
	vmID := str(raw, "vm_id")
	name := ""
	if vm, ok := raw["vm"].(map[string]any); ok {
		vmID, name = orDefault(vmID, str(vm, "id")), displayName(d.s.store.Snapshot(), vm)
	}
	if typ == "sandbox.archived" {
		d.gate.forget(vmID)
	}
	if hold && typ == "sandbox.ready" && d.gate.hold(ev, vmID, d) {
		return Event{}, false // sent (or dropped) when the env push settles
	}
	if name == "" && vmID != "" {
		var vm VM
		if d.s.boatd.call(ctx, "GET", vmPath(vmID), nil, &vm) == nil {
			name = displayName(d.s.store.Snapshot(), vm)
		}
	}
	data := map[string]any{"sandbox": map[string]any{"id": d.s.compatID(ev.TeamID, vmID), "name": name},
		"previousState": compatState(prev, false), "state": compatState(state, false)}
	b, _ := json.Marshal(data)
	return Event{ID: "evt_" + randHex(16), Type: typ, Created: ev.Created, TeamID: ev.TeamID, Data: b}, true
}

// boatDevBody is the payload boat.dev sends.
func boatDevBody(ev Event) []byte {
	b, _ := json.Marshal(map[string]any{"id": ev.ID, "type": ev.Type, "createdAt": ev.Created.UTC().Format("2006-01-02T15:04:05.000Z"),
		"data": ev.Data})
	return b
}

// signBoatDev sets boat.dev's delivery headers.
func signBoatDev(req *http.Request, secret []byte, ev Event, body []byte, attempt int) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	m := hmac.New(sha256.New, secret)
	fmt.Fprintf(m, "%s.%s.", ev.ID, ts)
	m.Write(body)
	req.Header.Set("X-Ascii-Event", ev.Type)
	req.Header.Set("X-Ascii-Delivery", ev.ID)
	req.Header.Set("X-Ascii-Timestamp", ts)
	req.Header.Set("X-Ascii-Signature", "v1="+hex.EncodeToString(m.Sum(nil)))
	req.Header.Set("X-Ascii-Attempt", strconv.Itoa(attempt))
}

// ---- sandbox.ready waits for the env push ----

const (
	readyHoldMax = 2 * time.Minute  // a held ready event is sent after this at the latest
	envLateWin   = 30 * time.Second // a running event this soon after a failed push is dropped
)

type heldReady struct {
	ev    Event
	timer *time.Timer
}

// envGate holds boat.dev sandbox.ready events of VMs whose env push is pending.
// The zero value is ready to use.
type envGate struct {
	mu       sync.Mutex
	ctx      context.Context
	inflight map[string]int        // team: create/fork/start calls still pushing env
	held     map[string]*heldReady // vm id: its running event, waiting for the push
	failed   map[string]time.Time  // vm id: when its last env push failed
}

func (g *envGate) setCtx(ctx context.Context) {
	g.mu.Lock()
	g.ctx = ctx
	g.mu.Unlock()
}

// bg is the dispatcher's context (the process's), Background before watch starts.
func (g *envGate) bg() context.Context {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ctx == nil {
		return context.Background()
	}
	return g.ctx
}

// forget drops what the gate knows of vmID (it stopped).
func (g *envGate) forget(vmID string) {
	g.mu.Lock()
	delete(g.failed, vmID)
	g.mu.Unlock()
}

// hold keeps ev when an env push may still be pending for its VM (a call of
// its team is in flight) and drops it when the VM's last push just failed.
func (g *envGate) hold(ev Event, vmID string, d *dispatcher) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t, ok := g.failed[vmID]; ok && time.Since(t) < envLateWin {
		return true
	}
	if g.inflight[ev.TeamID] == 0 || vmID == "" {
		return false
	}
	if old := g.held[vmID]; old != nil {
		old.timer.Stop()
	}
	if g.held == nil {
		g.held = map[string]*heldReady{}
	}
	h := &heldReady{ev: ev}
	h.timer = time.AfterFunc(readyHoldMax, func() {
		g.mu.Lock()
		cur := g.held[vmID]
		if cur == h {
			delete(g.held, vmID)
		}
		g.mu.Unlock()
		if cur == h {
			d.sendReady(ev)
		}
	})
	g.held[vmID] = h
	return true
}

// expectEnv marks one call of team as pushing an env; call release when it is
// done (also on its error paths): held events nobody settled are sent then.
func (d *dispatcher) expectEnv(team string) (release func()) {
	if d == nil {
		return func() {}
	}
	g := &d.gate
	g.mu.Lock()
	if g.inflight == nil {
		g.inflight = map[string]int{}
	}
	g.inflight[team]++
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			var flush []Event
			g.mu.Lock()
			if g.inflight[team]--; g.inflight[team] <= 0 {
				delete(g.inflight, team)
				for id, h := range g.held {
					if h.ev.TeamID == team {
						h.timer.Stop()
						flush = append(flush, h.ev)
						delete(g.held, id)
					}
				}
			}
			g.mu.Unlock()
			for _, ev := range flush {
				d.sendReady(ev)
			}
		})
	}
}

// envSettled is told (recordEnvResult) how vmID's env push ended. changed: the
// stored env state changed. A held ready is sent on success; a failure sends
// sandbox.error (and drops the held ready).
//
// vm, when not nil, is the VM as the caller holds it at a failure the caller is
// about to fail closed (stop): the sandbox.error is then built from it, at once,
// because a later read of the VM would find it stopped and send nothing.
func (d *dispatcher) envSettled(vmID string, failed, changed bool, vm VM) {
	if d == nil {
		return
	}
	g := &d.gate
	g.mu.Lock()
	h := g.held[vmID]
	if h != nil {
		h.timer.Stop()
		delete(g.held, vmID)
	}
	if failed {
		if g.failed == nil {
			g.failed = map[string]time.Time{}
		}
		g.failed[vmID] = time.Now()
	} else {
		delete(g.failed, vmID)
	}
	g.mu.Unlock()
	switch {
	case failed && vm != nil:
		d.sendEnvError(vmID, h, vm) // no read of boatd: it publishes before the caller stops the VM
	case failed && (changed || h != nil):
		go d.sendEnvError(vmID, h, nil)
	case !failed && h != nil:
		d.sendReady(h.ev)
	}
}

// sendReady delivers a held running event as sandbox.ready.
func (d *dispatcher) sendReady(ev Event) {
	go func() {
		ctx, cancel := context.WithTimeout(d.gate.bg(), 30*time.Second)
		defer cancel()
		if bd, ok := d.boatDevEventOpt(ctx, ev, false); ok {
			d.publishBoatDev(d.gate.bg(), bd)
		}
	}()
}

// sendEnvError delivers sandbox.error for a VM whose env never reached the
// guest; h is its held running event, if any. known is the VM in hand (see
// envSettled): it replaces the read of boatd and the check that it still runs.
func (d *dispatcher) sendEnvError(vmID string, h *heldReady, known VM) {
	ctx, cancel := context.WithTimeout(d.gate.bg(), 30*time.Second)
	defer cancel()
	vm := known
	team, prev := "", "running"
	if h != nil {
		team = h.ev.TeamID
		var raw map[string]any
		if json.Unmarshal(h.ev.Data, &raw) == nil && str(raw, "prev") != "" {
			prev = str(raw, "prev")
		}
	}
	if known == nil {
		if err := d.s.boatd.call(ctx, "GET", vmPath(vmID), nil, &vm); err != nil && h == nil {
			return
		}
	}
	if team == "" {
		team = str(vm, "team")
	}
	if team == "" || (known == nil && vm != nil && str(vm, "state") != "running") {
		return // gone or stopped meanwhile: archived/deleted says it
	}
	data := map[string]any{"sandbox": map[string]any{"id": d.s.compatID(team, vmID), "name": displayName(d.s.store.Snapshot(), vm)},
		"previousState": compatState(prev, false), "state": "error", "error": envErrorMessage}
	b, _ := json.Marshal(data)
	d.publishBoatDev(d.gate.bg(), Event{ID: "evt_" + randHex(16), Type: "sandbox.error", Created: time.Now().UTC(), TeamID: team, Data: b})
}

// emitEnvRecovered sends sandbox.ready for a running VM whose earlier env
// failure a retry just cleared (boatd publishes no state change for it).
func (d *dispatcher) emitEnvRecovered(vmID string) {
	if d == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(d.gate.bg(), 30*time.Second)
		defer cancel()
		var vm VM
		if d.s.boatd.call(ctx, "GET", vmPath(vmID), nil, &vm) != nil || str(vm, "state") != "running" {
			return
		}
		team := str(vm, "team")
		data := map[string]any{"sandbox": map[string]any{"id": d.s.compatID(team, vmID), "name": displayName(d.s.store.Snapshot(), vm)},
			"previousState": "error", "state": compatState("running", false)}
		b, _ := json.Marshal(data)
		d.publishBoatDev(d.gate.bg(), Event{ID: "evt_" + randHex(16), Type: "sandbox.ready", Created: time.Now().UTC(), TeamID: team, Data: b})
	}()
}

// publishBoatDev fans a boat.dev event out to the team's /api/v1 webhooks.
func (d *dispatcher) publishBoatDev(ctx context.Context, bd Event) {
	for _, h := range d.s.store.Snapshot().Webhooks {
		if h.TeamID == bd.TeamID && h.Format == hookFormatBoatDev && matchEvent(h.Events, bd.Type) {
			go d.deliver(ctx, *h, bd)
		}
	}
}
