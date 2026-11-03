package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	routeGenPoll    = 2 * time.Second
	routeGenTimeout = time.Second
)

// routeGens follows boat-api's revocations of port tokens: per VM, the generation
// a token must be signed with. Tokens are plain HMACs, so this table is the only
// thing that can end one before it expires.
type routeGens struct {
	url    string // boat-api's /internal/route-gens
	client *http.Client

	mu    sync.RWMutex
	gens  map[string]int
	ready bool
}

func newRouteGens(api string) *routeGens {
	return &routeGens{url: api + "/internal/route-gens", client: &http.Client{Timeout: routeGenTimeout}}
}

// refresh replaces the table. A 404 is an API without revocation, so every token
// is generation 0. Any other failure keeps the last good table.
func (g *routeGens) refresh(ctx context.Context) error {
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
		Gens map[string]int `json:"gens"`
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64<<20)).Decode(&body); err != nil {
			return fmt.Errorf("parse: %w", err)
		}
	case http.StatusNotFound:
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gens, g.ready = body.Gens, true
	return nil
}

// poll keeps the table current; a revocation takes effect within routeGenPoll.
func (g *routeGens) poll(ctx context.Context) {
	failing := false
	for {
		err := g.refresh(ctx)
		if err != nil && !failing {
			log.Printf("route generations: %v (keeping the previous table)", err)
		}
		failing = err != nil
		select {
		case <-ctx.Done():
			return
		case <-time.After(routeGenPoll):
		}
	}
}

// gen returns vmid's generation. Without a table yet (boat-api never answered
// since this process started) ok is false: a revoked token cannot be told from a
// good one, so the caller refuses.
func (g *routeGens) gen(ctx context.Context, vmid string) (n int, ok bool) {
	if g == nil {
		return 0, true // no -api: revocation is not wired up
	}
	g.mu.RLock()
	n, ok = g.gens[vmid], g.ready
	g.mu.RUnlock()
	if ok {
		return n, true
	}
	if err := g.refresh(ctx); err != nil {
		return 0, false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.gens[vmid], g.ready
}
