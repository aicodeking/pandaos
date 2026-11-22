package main

import (
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// A request body is read only after the caller has shown it may send it:
// a body aimed at a VM waits for the VM's ownership check, and a body on a
// short route must arrive within BodyTimeout. With the per-team in-flight
// cap (teamAll) this bounds what one tenant can make boat-api buffer.

const (
	defaultBodyTimeout       = 30 * time.Second
	defaultUploadIdleTimeout = 30 * time.Second
	defaultUploadMaxDuration = 2 * time.Hour
)

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
	if !streamRoutes[pattern] && !uploadRoutes[pattern] && r.Body != nil && r.Body != http.NoBody {
		d := s.cfg.BodyTimeout
		if d <= 0 {
			d = defaultBodyTimeout
		}
		// ErrNotSupported (in-process calls) leaves the server's own timeouts.
		http.NewResponseController(w).SetReadDeadline(time.Now().Add(d))
	}
	if vmBodyRoute(pattern) {
		vm, ok := s.ownVM(w, r, p)
		if !ok {
			return false
		}
		info(r).vmOK, info(r).vm = true, vm
	}
	return true
}

type uploadProgressReader struct {
	body        io.ReadCloser
	rc          *http.ResponseController
	idle        time.Duration
	maxDeadline time.Time
}

func (u *uploadProgressReader) Read(p []byte) (int, error) {
	now := time.Now()
	if !now.Before(u.maxDeadline) {
		return 0, os.ErrDeadlineExceeded
	}
	deadline := now.Add(u.idle)
	if u.maxDeadline.Before(deadline) {
		deadline = u.maxDeadline
	}
	if err := u.rc.SetReadDeadline(deadline); err != nil {
		wait := time.Until(deadline)
		if wait <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		timer := time.AfterFunc(wait, func() { _ = u.body.Close() })
		n, rerr := u.body.Read(p)
		if !timer.Stop() && rerr != nil {
			return n, os.ErrDeadlineExceeded
		}
		return n, rerr
	}
	return u.body.Read(p)
}

func (u *uploadProgressReader) Close() error { return u.body.Close() }

func (s *server) uploadBodyReader(w http.ResponseWriter, body io.ReadCloser) (io.ReadCloser, func()) {
	idle := s.cfg.UploadIdleTimeout
	if idle <= 0 {
		idle = defaultUploadIdleTimeout
	}
	maxDur := s.cfg.UploadMaxDuration
	if maxDur <= 0 {
		maxDur = defaultUploadMaxDuration
	}
	rc := http.NewResponseController(w)
	rd := &uploadProgressReader{
		body:        body,
		rc:          rc,
		idle:        idle,
		maxDeadline: time.Now().Add(maxDur),
	}
	return rd, func() { _ = rc.SetReadDeadline(time.Time{}) }
}
