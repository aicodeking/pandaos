package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	routeGenPoll              = 2 * time.Second
	routeGenTimeout           = time.Second
	routeGenMaxAge            = 2*routeGenPoll + routeGenTimeout
	desktopRefreshMinInterval = 2 * time.Second
)

type desktopRefreshCall struct {
	done chan struct{}
}

// routeGens follows boat-api's revocations of port tokens: per VM, the generation
// a token must be signed with. Tokens are plain HMACs, so this table is the only
// thing that can end one before it expires.
type routeGens struct {
	url    string // boat-api's /internal/route-gens
	client *http.Client
	now    func() time.Time
	every  time.Duration // poll period; 0 = routeGenPoll
	// onRefresh runs after every successful poll (live.go re-checks open requests).
	onRefresh          func()
	desktopMinInterval time.Duration // 0 = desktopRefreshMinInterval; < 0 = disabled

	mu       sync.RWMutex
	gens     map[string]int
	desktops map[string]bool // VMs with a minted desktop link
	ready    bool
	fetched  time.Time // request start, so a slow response cannot extend freshness
	next     atomic.Uint64
	applied  uint64

	desktopFlightMu    sync.Mutex
	desktopFlight      *desktopRefreshCall
	lastDesktopRefresh time.Time
}

func newRouteGens(api string) *routeGens {
	return &routeGens{url: api + "/internal/route-gens", client: &http.Client{Timeout: routeGenTimeout}, now: time.Now}
}

// refresh replaces the table. Any failure, a 404 included (an API without
// revocations cannot be told from a misrouted one), keeps the last good table,
// usable only until routeGenMaxAge. Before the first good one every private
// token is refused.
func (g *routeGens) refresh(ctx context.Context) error {
	g.mu.RLock()
	nowFn := g.now
	g.mu.RUnlock()
	started := nowFn()
	seq := g.next.Add(1)
	req, err := http.NewRequestWithContext(ctx, "GET", g.url, nil)
	if err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var body struct {
		Gens     map[string]*int `json:"gens"`
		Desktops json.RawMessage `json:"desktops"`
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	decoder := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64<<20))
	if err := decoder.Decode(&body); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	// Consume the entire bounded document: a valid prefix must not hide a
	// second response, trailing garbage, or an oversized body.
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return fmt.Errorf("parse: expected end of snapshot, got %v", err)
	}
	if body.Gens == nil {
		return fmt.Errorf("parse: missing or null gens")
	}
	gens := make(map[string]int, len(body.Gens))
	for id, gen := range body.Gens {
		if gen == nil || *gen < 0 {
			return fmt.Errorf("parse: invalid generation for %q", id)
		}
		gens[id] = *gen
	}
	// Older producers may omit desktops; when present it must be an array
	// of strings, not null (including null array elements).
	var ids []*string
	if len(body.Desktops) > 0 {
		if err := json.Unmarshal(body.Desktops, &ids); err != nil || ids == nil {
			return fmt.Errorf("parse: invalid desktops")
		}
	}
	desktops := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == nil {
			return fmt.Errorf("parse: null desktop")
		}
		desktops[*id] = true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if seq > g.applied {
		g.gens, g.desktops, g.ready = gens, desktops, true
		g.fetched, g.applied = started, seq
	}
	return nil
}

// poll keeps the table current; outages fail closed after routeGenMaxAge.
func (g *routeGens) poll(ctx context.Context) {
	failing := false
	every := g.every
	if every <= 0 {
		every = routeGenPoll
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		err := g.refresh(ctx)
		if err == nil && g.onRefresh != nil {
			g.onRefresh()
		}
		if err != nil && !failing {
			log.Printf("route generations: %v (keeping the previous table)", err)
		}
		failing = err != nil
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// fresh reports whether the snapshot is still usable. The caller holds g.mu.
func (g *routeGens) fresh() bool {
	return g.ready && g.now().Sub(g.fetched) < routeGenMaxAge
}

// peek is vmid's generation from the current table without fetching one;
// fresh is false once the table is stale (or was never loaded).
func (g *routeGens) peek(vmid string) (n int, fresh bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.gens[vmid], g.fresh()
}

// gen returns vmid's generation, refreshing a missing or stale table once.
// Without a fresh table a revoked token cannot be told from a good one.
func (g *routeGens) gen(ctx context.Context, vmid string) (n int, ok bool) {
	if g == nil {
		return 0, true // no -api: revocation is not wired up
	}
	g.mu.RLock()
	n, ok = g.gens[vmid], g.fresh()
	g.mu.RUnlock()
	if ok {
		return n, true
	}
	_ = g.refresh(ctx)
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.gens[vmid], g.fresh()
}

// refreshDesktop coalesces concurrent on-demand refreshes from hasDesktop and
// enforces a minimum interval between them, so probes for unknown desktop hosts
// cannot storm boat-api's /internal/route-gens endpoint.
func (g *routeGens) refreshDesktop(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	minInterval := g.desktopMinInterval
	if minInterval == 0 {
		minInterval = desktopRefreshMinInterval
	}
	g.mu.RLock()
	nowFn := g.now
	g.mu.RUnlock()
	if nowFn == nil {
		nowFn = time.Now
	}

	g.desktopFlightMu.Lock()
	if call := g.desktopFlight; call != nil {
		g.desktopFlightMu.Unlock()
		select {
		case <-call.done:
		case <-ctx.Done():
		}
		return
	}
	now := nowFn()
	if minInterval > 0 && !g.lastDesktopRefresh.IsZero() {
		if elapsed := now.Sub(g.lastDesktopRefresh); elapsed >= 0 && elapsed < minInterval {
			g.desktopFlightMu.Unlock()
			return
		}
	}
	call := &desktopRefreshCall{done: make(chan struct{})}
	g.desktopFlight = call
	g.desktopFlightMu.Unlock()

	defer func() {
		g.desktopFlightMu.Lock()
		g.lastDesktopRefresh = nowFn()
		g.desktopFlight = nil
		close(call.done)
		g.desktopFlightMu.Unlock()
	}()

	timeout := routeGenTimeout
	if g.client != nil && g.client.Timeout > 0 {
		timeout = g.client.Timeout
	}
	flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	_ = g.refresh(flightCtx)
}

// hasDesktop reports whether boat-api minted a desktop link for vmid. Without
// -api every routed VM counts. A VM not in the table triggers at most one
// coalesced refresh per desktopRefreshMinInterval: the browser follows a fresh
// link sooner than the next poll, while probes for unknown hosts cannot storm
// boat-api.
func (g *routeGens) hasDesktop(ctx context.Context, vmid string) bool {
	if g == nil {
		return true
	}
	known := func() bool {
		g.mu.RLock()
		defer g.mu.RUnlock()
		return g.fresh() && g.desktops[vmid]
	}
	if known() {
		return true
	}
	g.refreshDesktop(ctx)
	return known()
}
