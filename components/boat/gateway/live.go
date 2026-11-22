package main

// Revocation of connections that are already open (r30).
//
// Tokens and desktop links are checked when a request starts. A proxied
// request can run for hours (SSE, WebSocket, a long download), so without more
// it would outlive a revoked token, a rotated generation, a removed route, a
// port made private again or a VM whose IP was reused. Every proxied request is
// therefore registered here together with what admitted it, and re-checked
// against the current route table and revocation generations whenever the
// generations are polled (routeGenPoll) and on a timer of the same period.
// A request that no longer holds is cancelled: its context ends (which also
// closes the upstream leg of a WebSocket upgrade) and any hijacked client
// connection is closed.

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"
)

var errAccessRevoked = errors.New("gateway: access revoked while the connection was open")

// liveReq is one admitted, proxied request.
type liveReq struct {
	vmid    string
	ip      string // normalized upstream IP it was routed to
	port    int    // upstream port (the desktop port for a desktop)
	desktop bool
	public  bool      // admitted without a token: needs the port to stay public
	gated   bool      // admitted by a generation-bound token/link
	gen     int       // generation that token/link was verified against
	exp     time.Time // expiry of the admitting token/cookie; zero = none (public)

	cancel context.CancelCauseFunc

	mu     sync.Mutex
	closed bool
	conns  []net.Conn // hijacked client connections
}

// revoke ends the request: its context, and every hijacked connection.
func (l *liveReq) revoke(cause error) {
	l.mu.Lock()
	l.closed = true
	conns := l.conns
	l.conns = nil
	l.mu.Unlock()
	l.cancel(cause)
	for _, c := range conns {
		_ = c.Close()
	}
}

// track remembers a hijacked connection; one hijacked after revoke is closed
// at once.
func (l *liveReq) track(c net.Conn) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		_ = c.Close()
		return
	}
	l.conns = append(l.conns, c)
	l.mu.Unlock()
}

// liveWriter lets the registry see the connection a WebSocket upgrade takes
// over; everything else (Flush, deadlines) reaches the real writer via Unwrap.
type liveWriter struct {
	http.ResponseWriter
	l *liveReq
}

func (w *liveWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *liveWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.l.track(c)
	}
	return c, rw, err
}

// liveSet is the registry of admitted requests.
type liveSet struct {
	mu   sync.Mutex
	next uint64
	reqs map[uint64]*liveReq
}

func (s *liveSet) add(l *liveReq) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reqs == nil {
		s.reqs = make(map[uint64]*liveReq)
	}
	s.next++
	s.reqs[s.next] = l
	return s.next
}

func (s *liveSet) remove(id uint64) {
	s.mu.Lock()
	delete(s.reqs, id)
	s.mu.Unlock()
}

func (s *liveSet) snapshot() []*liveReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*liveReq, 0, len(s.reqs))
	for _, l := range s.reqs {
		out = append(out, l)
	}
	return out
}

// stillValid reports whether what admitted l still holds.
func (g *gateway) stillValid(l *liveReq, snap *routeTable) bool {
	v, ok := snap.lookup(l.vmid)
	if !ok || normalizeIP(v.IP) != l.ip || snap.isQuarantined(l.ip) {
		return false
	}
	public := false
	if l.desktop {
		port := v.DesktopPort
		if port == 0 {
			port = defaultDesktopPort
		}
		if port != l.port {
			return false
		}
	} else {
		if !slices.Contains(v.Ports, l.port) {
			return false
		}
		public = slices.Contains(v.Public, l.port)
		if l.public && !public {
			return false // it passed without a token; now one is required
		}
	}
	if !l.exp.IsZero() && g.now().After(l.exp) {
		return false // the admitting token or link has expired
	}
	if l.gated && !public && g.gens != nil {
		cur, fresh := g.gens.peek(l.vmid)
		if !fresh || cur != l.gen {
			return false // revoked, or no fresh table to tell (fail closed)
		}
	}
	return true
}

// revalidateLive re-checks every open request and ends the ones that no
// longer hold. It returns how many it ended.
func (g *gateway) revalidateLive() int {
	reqs := g.live.snapshot()
	if len(reqs) == 0 {
		return 0
	}
	snap := g.snapshotRoutesNow()
	n := 0
	for _, l := range reqs {
		if !g.stillValid(l, snap) {
			l.revoke(errAccessRevoked)
			n++
		}
	}
	return n
}

// watchLive re-checks on a timer, which also covers route changes and a
// generation table that went stale without any poll succeeding.
func (g *gateway) watchLive(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.revalidateLive()
		}
	}
}

// serveLive proxies under the registry. A request whose admission already
// lapsed between its check and now is refused instead of proxied.
func (g *gateway) serveLive(w http.ResponseWriter, r *http.Request, snap *routeTable, l *liveReq, h http.Handler) {
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	l.cancel = cancel
	id := g.live.add(l)
	defer g.live.remove(id)
	if !g.stillValid(l, snap) {
		http.Error(w, "access revoked, retry shortly", http.StatusForbidden)
		return
	}
	h.ServeHTTP(&liveWriter{ResponseWriter: w, l: l}, r.WithContext(ctx))
}
