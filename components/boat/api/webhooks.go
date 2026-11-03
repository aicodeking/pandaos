package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---- handlers ----

type webhookView struct {
	ID        string     `json:"id"`
	URL       string     `json:"url"`
	Events    []string   `json:"events"`
	Created   time.Time  `json:"created"`
	Secret    string     `json:"secret,omitempty"` // only on creation and rotation
	PrevUntil *time.Time `json:"prev_until,omitempty"`
}

func viewHook(h *Webhook, secret string) webhookView {
	ev := h.Events
	if ev == nil {
		ev = []string{}
	}
	return webhookView{ID: h.ID, URL: h.URL, Events: ev, Created: h.Created, Secret: secret, PrevUntil: h.PrevUntil}
}

var reEventFilter = regexp.MustCompile(`^(\*|[a-z_]+(\.[a-z_]+)*(\.\*)?)$`)

func (s *server) listHooks(w http.ResponseWriter, r *http.Request, p *principal) {
	out := []webhookView{}
	for _, h := range s.store.Snapshot().Webhooks {
		if h.TeamID == p.Team.ID {
			out = append(out, viewHook(h, ""))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	writeJSON(w, 200, map[string]any{"webhooks": out})
}

func (s *server) createHook(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !s.validHookURL(body.URL) {
		writeErr(w, r, 400, "invalid_request", "url must be an absolute https URL")
		return
	}
	for _, e := range body.Events {
		if !reEventFilter.MatchString(e) {
			writeErr(w, r, 400, "invalid_request", "invalid event filter "+e)
			return
		}
	}
	h := &Webhook{ID: "wh_" + randHex(8), TeamID: p.Team.ID, URL: body.URL, Events: body.Events, Created: time.Now().UTC()}
	secret := "whsec_" + randHex(24)
	sealed, err := s.keys.Seal([]byte(secret), "webhook/"+h.ID)
	if err != nil {
		writeErr(w, r, 500, "crypto_error", "could not encrypt")
		return
	}
	h.SealedSecret = sealed
	err = s.store.Update(func(st *State) error {
		if !underLimit(teamHookCount(st, p.Team.ID), s.cfg.MaxHooksPerTeam) {
			return errLimit
		}
		st.Webhooks[h.ID] = h
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 201, viewHook(h, secret))
}

// getHook returns one webhook.
func (s *server) getHook(w http.ResponseWriter, r *http.Request, p *principal) {
	if h, ok := s.teamHook(w, r, p); ok {
		writeJSON(w, 200, viewHook(h, ""))
	}
}

// validHookURL reports whether u is an allowed delivery target.
func (s *server) validHookURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && len(raw) <= 2048 && (u.Scheme == "https" || (s.cfg.AllowPrivateHook && u.Scheme == "http"))
}

// updateHook changes the URL or event filter; the signing secret stays.
func (s *server) updateHook(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		URL    *string   `json:"url"`
		Events *[]string `json:"events"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.URL != nil && !s.validHookURL(*body.URL) {
		writeErr(w, r, 400, "invalid_request", "url must be an absolute https URL")
		return
	}
	if body.Events != nil {
		for _, e := range *body.Events {
			if !reEventFilter.MatchString(e) {
				writeErr(w, r, 400, "invalid_request", "invalid event filter "+e)
				return
			}
		}
	}
	h, ok := s.teamHook(w, r, p)
	if !ok {
		return
	}
	var out Webhook
	err := s.store.Update(func(st *State) error {
		cur := st.Webhooks[h.ID]
		if cur == nil {
			return errConflict
		}
		if body.URL != nil {
			cur.URL = *body.URL
		}
		if body.Events != nil {
			cur.Events = *body.Events
		}
		out = *cur
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, viewHook(&out, ""))
}

const (
	defaultOverlap = 24 * time.Hour
	maxOverlap     = 30 * 24 * time.Hour
)

// overlapFrom reads overlap_s (default 24h, 0 = no overlap).
func overlapFrom(v *int64) (time.Duration, bool) {
	if v == nil {
		return defaultOverlap, true
	}
	d := time.Duration(*v) * time.Second
	return d, *v >= 0 && d <= maxOverlap
}

// rotateHook replaces the signing secret. During the overlap, deliveries carry
// a signature for both secrets (two v1= values).
func (s *server) rotateHook(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		OverlapS *int64 `json:"overlap_s"`
	}
	if !decode(w, r, &body) {
		return
	}
	overlap, ok := overlapFrom(body.OverlapS)
	if !ok {
		writeErr(w, r, 400, "invalid_request", "overlap_s must be 0..2592000")
		return
	}
	h, ok := s.teamHook(w, r, p)
	if !ok {
		return
	}
	secret := "whsec_" + randHex(24)
	sealed, err := s.keys.Seal([]byte(secret), "webhook/"+h.ID)
	if err != nil {
		writeErr(w, r, 500, "crypto_error", "could not encrypt")
		return
	}
	until := time.Now().Add(overlap).UTC()
	var out Webhook
	err = s.store.Update(func(st *State) error {
		cur := st.Webhooks[h.ID]
		if cur == nil {
			return errConflict
		}
		cur.PrevSealedSecret, cur.PrevUntil = cur.SealedSecret, &until
		if overlap == 0 {
			cur.PrevSealedSecret, cur.PrevUntil = "", nil
		}
		cur.SealedSecret = sealed
		out = *cur
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	v := viewHook(&out, secret)
	writeJSON(w, 200, v)
}

func (s *server) teamHook(w http.ResponseWriter, r *http.Request, p *principal) (*Webhook, bool) {
	h := s.store.Snapshot().Webhooks[r.PathValue("id")]
	if h == nil || h.TeamID != p.Team.ID {
		writeErr(w, r, 404, "not_found", "webhook not found")
		return nil, false
	}
	return h, true
}

func (s *server) deleteHook(w http.ResponseWriter, r *http.Request, p *principal) {
	h, ok := s.teamHook(w, r, p)
	if !ok {
		return
	}
	err := s.store.Update(func(st *State) error {
		delete(st.Webhooks, h.ID)
		delete(st.Deliveries, h.ID)
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) hookDeliveries(w http.ResponseWriter, r *http.Request, p *principal) {
	h, ok := s.teamHook(w, r, p)
	if !ok {
		return
	}
	d := s.store.Snapshot().Deliveries[h.ID]
	if d == nil {
		d = []Delivery{}
	}
	writeJSON(w, 200, map[string]any{"deliveries": d})
}

func (s *server) testHook(w http.ResponseWriter, r *http.Request, p *principal) {
	h, ok := s.teamHook(w, r, p)
	if !ok {
		return
	}
	ev := Event{ID: "evt_" + randHex(8), Type: "webhook.test", Created: time.Now().UTC(), TeamID: p.Team.ID,
		Data: json.RawMessage(`{"message":"hello from boat"}`)}
	go s.hooks.deliver(context.Background(), *h, ev)
	writeJSON(w, 202, map[string]any{"event_id": ev.ID})
}

// ---- dispatcher ----

// Event is the webhook payload body.
type Event struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Created time.Time       `json:"created"`
	TeamID  string          `json:"team_id"`
	Data    json.RawMessage `json:"data"`
}

type dispatcher struct {
	s       *server
	client  *http.Client
	backoff []time.Duration // delay before attempt 2, 3, ...; attempts = len+1
	sem     chan struct{}   // in-flight HTTP attempts, all teams
	// Per team: in-flight attempts and pending deliveries, so one team with a
	// dead endpoint cannot use up the slots or memory of the others.
	teamInflight int
	teamPending  int
	mu           sync.Mutex
	teamSem      map[string]chan struct{}
	pending      map[string]int
}

// ponytail: deliveries are in-memory goroutines; only an HTTP attempt holds a
// slot, a retry sleeping does not. Pending retries are lost on restart. Add a
// persisted outbox if at-least-once across restarts matters.
func newDispatcher(s *server, allowPrivate bool) *dispatcher {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		dialer.Control = denyPrivate
	}
	tr := &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second}
	return &dispatcher{
		s:            s,
		client:       &http.Client{Transport: tr, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		backoff:      []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour},
		sem:          make(chan struct{}, 64),
		teamInflight: 8, teamPending: 1000,
		teamSem: map[string]chan struct{}{}, pending: map[string]int{},
	}
}

// nonPublicNets are never webhook targets: the IANA special-purpose registries
// plus cloud metadata. The IPv6 entries that embed an IPv4 address (::/96,
// NAT64, 6to4, Teredo, translated) would otherwise reach any IPv4 target.
var nonPublicNets = func() []netip.Prefix {
	var out []netip.Prefix
	for _, c := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "168.63.129.16/32", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/96", "::ffff:0:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/32", "2001:db8::/32", "2002::/16",
		"fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}()

// denyPrivate is the Dialer's Control: it sees the address actually being
// connected to, after DNS, so a name that rebinds to a private address fails
// here. It blocks SSRF into the host, the LAN and cloud metadata.
func denyPrivate(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("webhook target %s is not an address", host)
	}
	ip = ip.Unmap().WithZone("")
	for _, n := range nonPublicNets {
		if n.Contains(ip) {
			return fmt.Errorf("webhook target %s is not a public address", host)
		}
	}
	return nil
}

func matchEvent(filters []string, typ string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		if f == "*" || f == typ || (strings.HasSuffix(f, ".*") && strings.HasPrefix(typ, strings.TrimSuffix(f, "*"))) {
			return true
		}
	}
	return false
}

// publish fans an event out to every matching webhook of its team.
func (d *dispatcher) publish(ctx context.Context, ev Event) {
	for _, h := range d.s.store.Snapshot().Webhooks {
		if h.TeamID == ev.TeamID && matchEvent(h.Events, ev.Type) {
			go d.deliver(ctx, *h, ev)
		}
	}
}

// slotTeam is the team's in-flight semaphore.
func (d *dispatcher) slotTeam(team string) chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.teamSem[team]
	if c == nil {
		c = make(chan struct{}, max(d.teamInflight, 1))
		d.teamSem[team] = c
	}
	return c
}

// addPending reserves a delivery for the team; false = over its limit.
func (d *dispatcher) addPending(team string, delta int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if delta > 0 && d.teamPending > 0 && d.pending[team] >= d.teamPending {
		return false
	}
	d.pending[team] += delta
	return true
}

// attempt does one POST holding a global and a team slot; the slots are
// released before any backoff sleep.
func (d *dispatcher) attempt(ctx context.Context, h Webhook, ev Event, body []byte) (int, error) {
	team := d.slotTeam(h.TeamID)
	for _, c := range []chan struct{}{team, d.sem} {
		select {
		case c <- struct{}{}:
			defer func(c chan struct{}) { <-c }(c)
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	// Reread: a rotation during a retry must sign with the current secret.
	if cur := d.s.store.Snapshot().Webhooks[h.ID]; cur != nil {
		h = *cur
	}
	secret, err := d.s.keys.Open(h.SealedSecret, "webhook/"+h.ID)
	if err != nil {
		return 0, fmt.Errorf("decrypt signing secret: %w", err)
	}
	var older [][]byte
	if h.PrevUntil != nil && time.Now().Before(*h.PrevUntil) {
		if old, err := d.s.keys.Open(h.PrevSealedSecret, "webhook/"+h.ID); err == nil {
			older = append(older, old)
		}
	}
	return d.post(ctx, h, ev, body, secret, older)
}

func (d *dispatcher) deliver(ctx context.Context, h Webhook, ev Event) {
	if !d.addPending(h.TeamID, 1) {
		d.record(Delivery{ID: "dlv_" + randHex(8), WebhookID: h.ID, EventID: ev.ID, Event: ev.Type,
			Error: "dropped: too many pending deliveries for this team", At: time.Now().UTC()})
		return
	}
	defer d.addPending(h.TeamID, -1)
	body, _ := json.Marshal(ev)
	for attempt := 1; ; attempt++ {
		status, err := d.attempt(ctx, h, ev, body)
		ok := err == nil && status/100 == 2
		rec := Delivery{ID: "dlv_" + randHex(8), WebhookID: h.ID, EventID: ev.ID, Event: ev.Type,
			Attempt: attempt, Status: status, OK: ok, At: time.Now().UTC()}
		if err != nil {
			rec.Error = err.Error()
		}
		d.record(rec)
		if ok || attempt > len(d.backoff) || (status/100 == 4 && status != 408 && status != 429) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d.backoff[attempt-1]):
		}
		if d.s.store.Snapshot().Webhooks[h.ID] == nil {
			return // deleted while waiting
		}
	}
}

func (d *dispatcher) post(ctx context.Context, h Webhook, ev Event, body, secret []byte, older [][]byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", h.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "boat-webhooks/1")
	req.Header.Set("X-Boat-Event", ev.Type)
	req.Header.Set("X-Boat-Delivery", ev.ID)
	req.Header.Set("X-Boat-Signature", SignWebhook(secret, time.Now().Unix(), body, older...))
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func (d *dispatcher) record(rec Delivery) {
	err := d.s.store.Update(func(st *State) error {
		if st.Webhooks[rec.WebhookID] == nil {
			return nil
		}
		list := append(st.Deliveries[rec.WebhookID], rec)
		if len(list) > maxDeliveriesPerHook {
			list = list[len(list)-maxDeliveriesPerHook:]
		}
		st.Deliveries[rec.WebhookID] = list
		return nil
	})
	if err != nil {
		log.Printf("record delivery: %v", err)
	}
}

// watch follows boatd's SSE event stream forever, reconnecting with backoff.
func (d *dispatcher) watch(ctx context.Context) {
	wait := time.Second
	for ctx.Err() == nil {
		err := d.consume(ctx)
		if ctx.Err() != nil {
			return
		}
		log.Printf("boatd events: %v; reconnecting in %s", err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Minute)
	}
}

func (d *dispatcher) consume(ctx context.Context) error {
	resp, err := d.s.boatd.do(ctx, "GET", "/v1/events", nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var evType string
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) > 0 {
				d.handleRaw(ctx, evType, strings.Join(data, "\n"))
			}
			evType, data = "", nil
		case strings.HasPrefix(line, "event:"):
			evType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("stream closed")
}

// handleRaw maps a boatd event to a team. It accepts {type, team, vm:{id,team}}
// or {type, vm_id}; the SSE "event:" field is a fallback for type.
func (d *dispatcher) handleRaw(ctx context.Context, sseType, data string) {
	var raw map[string]any
	if json.Unmarshal([]byte(data), &raw) != nil {
		return
	}
	typ := str(raw, "type")
	if typ == "" {
		typ = sseType
	}
	team := str(raw, "team")
	vmID := str(raw, "vm_id")
	if vm, ok := raw["vm"].(map[string]any); ok {
		if team == "" {
			team = str(vm, "team")
		}
		if vmID == "" {
			vmID = str(vm, "id")
		}
		raw["vm"] = publicVM(vm)
	}
	if team == "" && vmID != "" {
		var vm VM
		if d.s.boatd.call(ctx, "GET", vmPath(vmID), nil, &vm) == nil {
			team = str(vm, "team")
		}
	}
	if strings.HasPrefix(typ, "vm.") {
		go d.s.syncJumpKeys(ctx) // ips change on start/fork/delete
	}
	if typ == "vm.deleted" && vmID != "" { // also TTL/operator deletes outside boat-api
		d.s.forgetVM(vmID)
	}
	// Every stop kills the run's AI proxy token (TTL and idle stops included).
	if typ == "vm.state" && str(raw, "state") == "stopped" && vmID != "" && d.s.store.Snapshot().AIVMs[vmID] != nil {
		if _, err := d.s.aiBumpVM(vmID, team); err != nil {
			log.Printf("rotate AI token of %s: %v", vmID, err)
		}
	}
	if strings.HasPrefix(typ, "vm.") && team != "" {
		d.s.emitVMEvent(ctx, typ, team, vmID, str(raw, "state"))
	}
	if typ == "" || team == "" {
		return
	}
	delete(raw, "env")
	b, _ := json.Marshal(raw)
	id := str(raw, "id")
	if id == "" {
		id = "evt_" + randHex(8)
	}
	d.publish(ctx, Event{ID: id, Type: typ, Created: time.Now().UTC(), TeamID: team, Data: b})
}
