package main

import (
	"net/http"
	"strings"
	"time"
)

// A request body is read only after the caller has shown it may send it:
// a body aimed at a VM waits for the VM's ownership check, and a body on a
// short route must arrive within BodyTimeout. With the per-team in-flight
// cap (teamAll) this bounds what one tenant can make boat-api buffer.

const defaultBodyTimeout = 30 * time.Second

// vmBodyRoute: a route that reads a body and names a VM.
func vmBodyRoute(pattern string) bool {
	method, path, _ := strings.Cut(pattern, " ")
	if method != http.MethodPost && method != http.MethodPut && method != http.MethodPatch {
		return false
	}
	return strings.HasPrefix(path, "/v1/vms/{id}") || strings.HasPrefix(path, "/api/v1/sandboxes/{id}")
}

// guardBody runs before a route's handler; false means it already answered.
func (s *server) guardBody(w http.ResponseWriter, r *http.Request, p *principal, pattern string) bool {
	if strings.Contains(pattern, " "+compatPrefix) {
		if !s.compatResolve(w, r, p) {
			return false
		}
	}
	if !streamRoutes[pattern] && !uploadRoutes[pattern] && r.Body != nil && r.Body != http.NoBody {
		d := s.cfg.BodyTimeout
		if d <= 0 {
			d = defaultBodyTimeout
		}
		// ErrNotSupported (in-process calls) leaves the server's own timeouts.
		http.NewResponseController(w).SetReadDeadline(time.Now().Add(d))
	}
	if vmBodyRoute(pattern) {
		if _, ok := s.ownVM(w, r, p); !ok {
			return false
		}
	}
	return true
}
