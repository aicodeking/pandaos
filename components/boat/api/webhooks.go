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
	"net/url"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ---- handlers ----

type webhookView struct {
	ID      string    `json:"id"`
	URL     string    `json:"url"`
	Events  []string  `json:"events"`
	Created time.Time `json:"created"`
	Secret  string    `json:"secret,omitempty"` // only on creation
}

func viewHook(h *Webhook, secret string) webhookView {
	ev := h.Events
	if ev == nil {
		ev = []string{}
	}
	return webhookView{ID: h.ID, URL: h.URL, Events: ev, Created: h.Created, Secret: secret}
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
	u, err := url.Parse(body.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(s.cfg.AllowPrivateHook && u.Scheme == "http")) || len(body.URL) > 2048 {
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
	sem     chan struct{}
}

// ponytail: deliveries are in-memory goroutines (max 64 concurrent); pending
// retries are lost on restart. Add a persisted outbox if at-least-once across
// restarts matters.
func newDispatcher(s *server, allowPrivate bool) *dispatcher {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		dialer.Control = denyPrivate
	}
	tr := &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second}
	return &dispatcher{
		s:       s,
		client:  &http.Client{Transport: tr, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		backoff: []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour},
		sem:     make(chan struct{}, 64),
	}
}

// denyPrivate blocks SSRF into the host or LAN (checked after DNS resolution).
func denyPrivate(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) || isCGNAT(ip) {
		return fmt.Errorf("webhook target %s is not a public address", host)
	}
	return nil
}

func isCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64
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

func (d *dispatcher) deliver(ctx context.Context, h Webhook, ev Event) {
	d.sem <- struct{}{}
	defer func() { <-d.sem }()
	secret, err := d.s.keys.Open(h.SealedSecret, "webhook/"+h.ID)
	if err != nil {
		log.Printf("webhook %s: cannot decrypt secret: %v", h.ID, err)
		return
	}
	body, _ := json.Marshal(ev)
	for attempt := 1; ; attempt++ {
		status, err := d.post(ctx, h, ev, secret, body)
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

func (d *dispatcher) post(ctx context.Context, h Webhook, ev Event, secret, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", h.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "boat-webhooks/1")
	req.Header.Set("X-Boat-Event", ev.Type)
	req.Header.Set("X-Boat-Delivery", ev.ID)
	req.Header.Set("X-Boat-Signature", SignWebhook(secret, time.Now().Unix(), body))
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
		go d.s.syncPublicPorts(ctx)
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
