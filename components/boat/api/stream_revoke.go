package main

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// Stream revocation (r27): route() authenticates a request once, but a stream
// (SSE events, exec, log follow, attach, prompt) can stay open for minutes or
// hours. For those requests route() derives a cancellable context and
// watchStream re-resolves the principal against the live store every
// streamRecheckEvery; when the key is revoked, rotated away, expired, or its
// user left the team (or lost the route's scope) the context is cancelled.
// Handlers pass r.Context() to every upstream boatd call, so the upstream body
// read fails and the handler returns, closing the client's connection.
//
// Covered: every pattern in streamRoutes and uploadRoutes, the compat
// events/prompt/steer-less streams listed in revocableExtra, and any request
// with Accept: text/event-stream, an Upgrade header, or ?follow= / ?stream=
// set to 1/true. See isStreamRequest.

// streamRecheckEvery is how often an open stream re-validates its caller.
const streamRecheckEvery = 3 * time.Second

// revocableExtra are streaming routes not already in streamRoutes/uploadRoutes.
var revocableExtra = map[string]bool{
	"GET /api/v1/sandboxes/{id}/events":  true,
	"POST /api/v1/sandboxes/{id}/prompt": true,
	"POST /v1/vms/{id}/prompts":          true,
}

// isStreamRequest reports whether the request can outlive its authentication
// check by a meaningful time and so must be re-validated while open.
func isStreamRequest(pattern string, r *http.Request) bool {
	if streamRoutes[pattern] || uploadRoutes[pattern] || revocableExtra[pattern] {
		return true
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream") {
		return true
	}
	if r.Header.Get("Upgrade") != "" {
		return true
	}
	q := r.URL.Query()
	for _, k := range []string{"follow", "stream"} {
		switch strings.ToLower(q.Get(k)) {
		case "1", "true", "yes":
			return true
		}
	}
	return false
}

func (s *server) streamInterval() time.Duration {
	if s.streamRecheck > 0 {
		return s.streamRecheck
	}
	return streamRecheckEvery
}

// watchStream returns r carrying a context that is cancelled once p stops
// being a live principal for scope; stop must be called when the handler ends.
func (s *server) watchStream(r *http.Request, p *principal, scope string) (*http.Request, func()) {
	return s.watchAlive(r, func(st *State, now time.Time) bool {
		_, err := requirePrincipalIn(st, p, scope, now)
		return err == nil
	})
}

// watchAlive is watchStream's engine for credentials that are not a principal
// (signed download links, snapshot shares): every streamInterval it evaluates
// alive against the live store and cancels the returned request's context the
// first time it reports false. stop must be called when the handler ends.
func (s *server) watchAlive(r *http.Request, alive func(*State, time.Time) bool) (*http.Request, func()) {
	ctx, cancel := context.WithCancel(r.Context())
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(s.streamInterval())
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case now := <-t.C:
				if !alive(s.store.Snapshot(), now) {
					cancel()
					return
				}
			}
		}
	}()
	return r.WithContext(ctx), func() {
		close(done)
		cancel()
	}
}
