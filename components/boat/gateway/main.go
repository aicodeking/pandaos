// boat-gateway: the HTTP router behind Caddy (Caddy does TLS).
//
//	<port>-<vmid>.<vmDomain>        -> <vm ip>:<port>. Private unless the port
//	                                   is in routes.json "public": needs a route
//	                                   token (header, or query -> host-only cookie)
//	<vmid>.<desktopDomain>          -> <vm ip>:<desktopPort>, signed link -> cookie
//	<desktopDomain>/<vmid>?exp&sig  -> 302 to <vmid>.<desktopDomain>/?exp&sig
//
// Every VM desktop and every port route is its own origin, so one tenant's
// page cannot read or plant another's cookies. vmDomain should be a separate
// registrable domain (docs/boat.md) so tenant apps cannot set cookies for
// the API or desktop names either.
//
// Routes come from boatd's /run/boat/routes.json, re-read when its mtime or
// size changes; no Caddy reload is needed. A second loopback listener
// answers Caddy's on-demand TLS "ask" checks.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	defaultDesktopPort       = 6080
	dialTimeout              = 5 * time.Second
	headerTimeout            = 60 * time.Second
	defaultRouteStatInterval = 50 * time.Millisecond
)

type vm struct {
	IP          string `json:"ip"`
	Ports       []int  `json:"ports"`
	Public      []int  `json:"public"`
	DesktopPort int    `json:"desktopPort"`
	Team        string `json:"team"`
}

type routeFile struct {
	VMs         map[string]vm `json:"vms"`
	Quarantined []string      `json:"quarantined"`
}

// routeTable is an immutable snapshot of routes.json.
type routeTable struct {
	vms         map[string]vm
	quarantined map[string]bool
}

func (t *routeTable) lookup(id string) (vm, bool) {
	if t == nil {
		return vm{}, false
	}
	v, ok := t.vms[id]
	return v, ok && net.ParseIP(v.IP) != nil
}

func (t *routeTable) isQuarantined(ip string) bool {
	return t != nil && ip != "" && t.quarantined[ip]
}

// routes caches routes.json, re-reading it when it changes on disk.
type routes struct {
	path            string
	stat            func(string) (os.FileInfo, error) // nil defaults to os.Stat
	now             func() time.Time                  // nil defaults to time.Now
	minStatInterval time.Duration                     // 0 = stat every call (immediate reload in tests)

	mu        sync.RWMutex
	checked   bool
	lastCheck time.Time
	stamp     string
	table     atomic.Pointer[routeTable]
}

func newRoutes(path string) *routes {
	return &routes{path: path, minStatInterval: defaultRouteStatInterval}
}

func normalizeIP(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if addr, err := netip.ParseAddr(s); err == nil {
		return addr.Unmap().String()
	}
	return s
}

func parsePeerIP(raw string) (netip.Addr, string) {
	s := strings.TrimSpace(raw)
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	} else if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	if addr, err := netip.ParseAddr(s); err == nil {
		u := addr.Unmap()
		return u, u.String()
	}
	return netip.Addr{}, s
}

func routesPruned(prev, next map[string]vm, quarantined map[string]bool) []string {
	var pruned []string
	seen := make(map[string]bool)
	add := func(ip string) {
		if ip != "" && !seen[ip] {
			seen[ip] = true
			pruned = append(pruned, ip)
		}
	}
	for id, old := range prev {
		oldIP := normalizeIP(old.IP)
		cur, ok := next[id]
		if !ok || oldIP != normalizeIP(cur.IP) || quarantined[oldIP] {
			add(oldIP)
		}
	}
	for ip := range quarantined {
		add(normalizeIP(ip))
	}
	slices.Sort(pruned)
	return pruned
}

func (r *routes) refresh(onPrune func(prunedIPs []string, all bool)) *routeTable {
	return r.refreshWithMode(onPrune, false)
}

func (r *routes) refreshNow(onPrune func(prunedIPs []string, all bool)) *routeTable {
	return r.refreshWithMode(onPrune, true)
}

func (r *routes) refreshWithMode(onPrune func(prunedIPs []string, all bool), force bool) *routeTable {
	if r == nil {
		return nil
	}
	statFn := r.stat
	if statFn == nil {
		statFn = os.Stat
	}
	nowFn := r.now
	if nowFn == nil {
		nowFn = time.Now
	}

	// Fast path 1: throttled stat interval (production default: 50ms).
	// Hot-path requests take only a shared RLock and skip os.Stat.
	if !force && r.minStatInterval > 0 {
		now := nowFn()
		r.mu.RLock()
		if r.checked {
			if elapsed := now.Sub(r.lastCheck); elapsed >= 0 && elapsed < r.minStatInterval {
				snap := r.table.Load()
				r.mu.RUnlock()
				return snap
			}
		}
		r.mu.RUnlock()
	}

	// Fast path 2: unthrottled (minStatInterval == 0) or forced refresh.
	// Stat outside the exclusive lock; if unchanged, serve under shared RLock.
	var (
		preStatDone bool
		preFi       os.FileInfo
		preErr      error
		preStamp    string
	)
	if force || r.minStatInterval <= 0 {
		preFi, preErr = statFn(r.path)
		preStatDone = true
		if preErr == nil {
			preStamp = fmt.Sprint(preFi.ModTime().UnixNano(), preFi.Size())
			r.mu.RLock()
			if r.checked && r.stamp == preStamp {
				snap := r.table.Load()
				r.mu.RUnlock()
				return snap
			}
			r.mu.RUnlock()
		}
	}

	var prunedIPs []string
	var pruneAll bool
	r.mu.Lock()
	now := nowFn()
	if !force && r.minStatInterval > 0 && r.checked {
		if elapsed := now.Sub(r.lastCheck); elapsed >= 0 && elapsed < r.minStatInterval {
			snap := r.table.Load()
			r.mu.Unlock()
			return snap
		}
	}
	prev := r.table.Load()
	var fi os.FileInfo
	var err error
	if preStatDone {
		fi, err = preFi, preErr
	} else {
		fi, err = statFn(r.path)
	}
	if err != nil {
		// No file yet (boatd not up) or it was removed: no routes.
		pruneAll = (prev != nil && len(prev.vms) > 0) || r.stamp != ""
		r.stamp = ""
		r.table.Store(nil)
	} else {
		stamp := preStamp
		if !preStatDone {
			stamp = fmt.Sprint(fi.ModTime().UnixNano(), fi.Size())
		}
		if stamp != r.stamp {
			vms, quarantined, readErr := readRoutes(r.path)
			if readErr != nil {
				// Keep the last good table; boatd writes atomically, so this is rare.
				log.Printf("routes: %v (keeping previous table)", readErr)
			} else {
				var prevVMs map[string]vm
				if prev != nil {
					prevVMs = prev.vms
				}
				prunedIPs = routesPruned(prevVMs, vms, quarantined)
				r.stamp = stamp
				r.table.Store(&routeTable{vms: vms, quarantined: quarantined})
			}
		}
	}
	r.checked = true
	r.lastCheck = now
	snap := r.table.Load()
	r.mu.Unlock()
	if onPrune != nil && (pruneAll || len(prunedIPs) > 0) {
		onPrune(prunedIPs, pruneAll)
	}
	return snap
}

func (r *routes) lookup(id string) (vm, bool) {
	return r.refresh(nil).lookup(id)
}

func readRoutes(path string) (map[string]vm, map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var f routeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var quarantined map[string]bool
	if len(f.Quarantined) > 0 {
		quarantined = make(map[string]bool, len(f.Quarantined))
		for _, raw := range f.Quarantined {
			if ip := normalizeIP(raw); ip != "" {
				quarantined[ip] = true
			}
		}
	}
	return f.VMs, quarantined, nil
}

type gateway struct {
	domain        string    // api.<domain> (for the ask check)
	vmDomain      string    // <port>-<vmid>.<vmDomain>
	desktopDomain string    // <vmid>.<desktopDomain>
	key           []byte    // master HMAC key shared with boat-api; tokens use purposeKey(key, …)
	legacyUntil   time.Time // master-key (pre-split) tokens verify until then; zero = never
	routes        *routes
	gens          *routeGens // nil = no revocation (no -api)
	certs         *certQuota // nil = no per-team limit on new certificates
	now           func() time.Time
	dialContext   func(ctx context.Context, network, addr string) (net.Conn, error)
	trMu          sync.RWMutex
	transports    map[string]*http.Transport
	live          liveSet // admitted proxied requests, re-checked while open (live.go)
}

func (g *gateway) httpTransport(rawIP string) *http.Transport {
	ip := normalizeIP(rawIP)
	g.trMu.RLock()
	tr := g.transports[ip]
	g.trMu.RUnlock()
	if tr != nil {
		return tr
	}
	g.trMu.Lock()
	if g.transports == nil {
		g.transports = make(map[string]*http.Transport)
	}
	tr = g.transports[ip]
	if tr == nil {
		dial := g.dialContext
		if dial == nil {
			dial = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
		}
		tr = &http.Transport{
			DialContext:           dial,
			ResponseHeaderTimeout: headerTimeout,
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			DisableCompression:    false,
		}
		g.transports[ip] = tr
	}
	g.trMu.Unlock()
	return tr
}

func (g *gateway) pruneRoutes(prunedIPs []string, all bool) {
	var toClose []*http.Transport
	g.trMu.Lock()
	if all {
		for ip, tr := range g.transports {
			delete(g.transports, ip)
			toClose = append(toClose, tr)
		}
	} else {
		for _, rawIP := range prunedIPs {
			ip := normalizeIP(rawIP)
			if tr, ok := g.transports[ip]; ok {
				delete(g.transports, ip)
				toClose = append(toClose, tr)
			}
		}
	}
	g.trMu.Unlock()
	for _, tr := range toClose {
		tr.CloseIdleConnections()
	}
}

func (g *gateway) snapshotRoutes() *routeTable {
	if g.routes == nil {
		return nil
	}
	return g.routes.refresh(g.pruneRoutes)
}

func (g *gateway) snapshotRoutesNow() *routeTable {
	if g.routes == nil {
		return nil
	}
	return g.routes.refreshNow(g.pruneRoutes)
}

func (g *gateway) lookupVM(id string) (vm, bool) {
	return g.snapshotRoutes().lookup(id)
}

func isQuarantinedRequestSnapshot(snap *routeTable, r *http.Request) bool {
	if snap == nil {
		return false
	}
	peerAddr, peerIP := parsePeerIP(r.RemoteAddr)
	if snap.isQuarantined(peerIP) {
		return true
	}
	if peerAddr.IsValid() && peerAddr.IsLoopback() {
		for _, hdr := range []string{"X-Forwarded-For", "X-Real-Ip"} {
			for _, line := range r.Header.Values(hdr) {
				for _, part := range strings.Split(line, ",") {
					if _, fwdIP := parsePeerIP(part); snap.isQuarantined(fwdIP) {
						return true
					}
				}
			}
		}
	}
	return false
}

func (g *gateway) isQuarantinedRequest(r *http.Request) bool {
	return isQuarantinedRequestSnapshot(g.snapshotRoutes(), r)
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// label returns the single DNS label in front of suffix, if host is one.
func label(host, suffix string) (string, bool) {
	l, ok := strings.CutSuffix(host, "."+suffix)
	return l, ok && l != "" && !strings.Contains(l, ".")
}

// parsePortHost splits "<port>-<vmid>.<vmDomain>".
func (g *gateway) parsePortHost(host string) (int, string, bool) {
	l, ok := label(host, g.vmDomain)
	if !ok {
		return 0, "", false
	}
	portStr, vmid, ok := strings.Cut(l, "-")
	port, err := strconv.Atoi(portStr)
	if !ok || err != nil || port < 1 || port > 65535 || vmid == "" {
		return 0, "", false
	}
	return port, vmid, true
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	snap := g.snapshotRoutes()
	if isQuarantinedRequestSnapshot(snap, r) {
		http.Error(w, "quarantined vm cannot access gateway", http.StatusForbidden)
		return
	}
	host := hostOnly(r.Host)
	switch {
	case host == g.desktopDomain:
		g.desktopLink(w, r, snap)
	case strings.HasSuffix(host, "."+g.desktopDomain):
		vmid, ok := label(host, g.desktopDomain)
		if !ok {
			http.NotFound(w, r)
			return
		}
		g.desktop(w, r, vmid, snap)
	default:
		port, vmid, ok := g.parsePortHost(host)
		if !ok {
			http.NotFound(w, r)
			return
		}
		g.portRoute(w, r, port, vmid, snap)
	}
}

// desktopLink moves boat-api's desktop.<d>/<vmid>?exp&sig link to the VM's
// own origin. It sets nothing on the shared desktop.<d> origin.
func (g *gateway) desktopLink(w http.ResponseWriter, r *http.Request, snap *routeTable) {
	vmid := strings.Trim(r.URL.Path, "/")
	if _, ok := snap.lookup(vmid); !ok || strings.Contains(vmid, "/") || !r.URL.Query().Has("sig") {
		http.NotFound(w, r)
		return
	}
	u := url.URL{Scheme: scheme(r), Host: vmid + "." + r.Host, Path: "/", RawQuery: r.URL.RawQuery}
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// scheme is what the client used: Caddy sets X-Forwarded-Proto.
func scheme(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
		return p
	}
	return "http"
}

func (g *gateway) desktop(w http.ResponseWriter, r *http.Request, vmid string, snap *routeTable) {
	v, ok := snap.lookup(vmid)
	if !ok {
		http.NotFound(w, r)
		return
	}
	gen, ready := g.gens.gen(r.Context(), vmid)
	if !ready {
		http.Error(w, "desktop: link revocations are unavailable, retry shortly", http.StatusServiceUnavailable)
		return
	}
	sign := func(k []byte, exp int64) string { return signDesktop(k, vmid, gen, exp) }
	var admitExp time.Time // expiry of the cookie token that admits this request
	check := func(tok string) error {
		exp, err := g.verify(tok, purposeDesktopLink, sign)
		if err == nil {
			admitExp = exp
		}
		return err
	}
	// The desktop page is not embeddable, and only its own origin may use it.
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'self'")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	if err := checkOrigin(r, nil); err != nil {
		http.Error(w, "desktop: "+err.Error(), http.StatusForbidden)
		return
	}
	if q := r.URL.Query(); q.Has("sig") {
		tok := q.Get("exp") + "." + q.Get("sig")
		exp, err := g.verify(tok, purposeDesktopLink, sign)
		if err != nil {
			http.Error(w, "desktop link invalid: "+err.Error(), http.StatusForbidden)
			return
		}
		setSession(w, r, desktopCookie, tok, exp, http.SameSiteStrictMode, withoutQuery(r, "exp", "sig"))
		return
	}
	// Top-level navigations from a sibling origin are refused too: only a signed
	// link (above) may enter from elsewhere, not the session cookie.
	if site := r.Header.Get("Sec-Fetch-Site"); site == "same-site" || site == "cross-site" {
		http.Error(w, "desktop: open it from a link (GET /v1/vms/<id>/desktop)", http.StatusForbidden)
		return
	}
	if err := anyCookie(r, desktopCookie, check); err != nil {
		http.Error(w, "desktop link required (GET /v1/vms/<id>/desktop): "+err.Error(), http.StatusUnauthorized)
		return
	}
	port := v.DesktopPort
	if port == 0 {
		port = defaultDesktopPort
	}
	g.serveLive(w, r, snap, &liveReq{vmid: vmid, ip: normalizeIP(v.IP), port: port, desktop: true, gated: true, gen: gen, exp: admitExp},
		g.proxy(v.IP, port, g.guestCookies(vmid)))
}

func (g *gateway) portRoute(w http.ResponseWriter, r *http.Request, port int, vmid string, snap *routeTable) {
	v, ok := snap.lookup(vmid)
	if !ok || !slices.Contains(v.Ports, port) {
		http.Error(w, "no such VM port route", http.StatusNotFound)
		return
	}
	live := &liveReq{vmid: vmid, ip: normalizeIP(v.IP), port: port, public: slices.Contains(v.Public, port)}
	if !live.public {
		gen, ready := g.gens.gen(r.Context(), vmid)
		live.gated, live.gen = true, gen
		if !ready {
			http.Error(w, "private port: token revocations are unavailable, retry shortly", http.StatusServiceUnavailable)
			return
		}
		sign := func(k []byte, exp int64) string { return signRoute(k, port, vmid, gen, exp) }
		check := func(tok string) error {
			exp, err := g.verify(tok, purposePortRoute, sign)
			if err == nil {
				live.exp = exp
			}
			return err
		}
		if q := r.URL.Query(); q.Has("boat_sig") {
			tok := q.Get("boat_exp") + "." + q.Get("boat_sig")
			exp, err := g.verify(tok, purposePortRoute, sign)
			if err != nil {
				http.Error(w, "route link invalid: "+err.Error(), http.StatusForbidden)
				return
			}
			setSession(w, r, routeCookie, tok, exp, http.SameSiteLaxMode, withoutQuery(r, "boat_exp", "boat_sig"))
			return
		}
		var err error
		if h := r.Header.Get(routeHeader); h != "" {
			err = check(h) // a program holding the token: no ambient credential involved
		} else if err = checkOrigin(r, func(host string) bool {
			// Another port of the same VM is the same tenant's app.
			_, other, ok := g.parsePortHost(host)
			return ok && other == vmid
		}); err != nil {
			http.Error(w, "private port: "+err.Error(), http.StatusForbidden)
			return
		} else {
			err = anyCookie(r, routeCookie, check)
		}
		if err != nil {
			http.Error(w, "private port: send "+routeHeader+" or open a link from GET /v1/vms/<id>/ports/<port>/url ("+err.Error()+")", http.StatusUnauthorized)
			return
		}
	}
	g.serveLive(w, r, snap, live, g.proxy(v.IP, port, ""))
}

// withoutQuery is r's path and query minus the named parameters, normalized to a
// same-origin relative path starting with a single "/".
func withoutQuery(r *http.Request, drop ...string) string {
	q := r.URL.Query()
	for _, k := range drop {
		q.Del(k)
	}
	u := url.URL{Path: "/" + strings.TrimLeft(r.URL.Path, "/\\"), RawQuery: q.Encode()}
	return safeRedirectTarget(u.RequestURI())
}

// hostOnlyCookie drops a Set-Cookie line's Domain attribute. Every VM origin is a
// sibling under vmDomain, so a Domain cookie from one tenant's app would reach
// all the others. Without the attribute the browser keeps it for this host.
func hostOnlyCookie(line string) string {
	parts := strings.Split(line, ";")
	kept := make([]string, 0, len(parts))
	for i, p := range parts {
		name, _, _ := strings.Cut(p, "=")
		if i == 0 || !strings.EqualFold(strings.TrimSpace(name), "domain") {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ";")
}

// proxy forwards to ip:port. Our cookies and route header never reach the
// guest; for the desktop, guestCookie ("name=value") is added for the guest
// agent's own token check. Caddy's X-Forwarded-* headers are kept. Responses
// lose any cookie Domain (hostOnlyCookie).
func (g *gateway) proxy(ip string, port int, guestCookie string) http.Handler {
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(ip, strconv.Itoa(port))}
	return &httputil.ReverseProxy{
		Transport: g.httpTransport(ip),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host"} {
				if v := pr.In.Header.Values(h); len(v) > 0 {
					pr.Out.Header[h] = v
				}
			}
			pr.Out.Header.Del(routeHeader)
			var kept []string
			if guestCookie != "" {
				kept = append(kept, guestCookie)
			}
			for _, c := range pr.In.Cookies() {
				if !strings.HasPrefix(c.Name, "__Host-boat_") && c.Name != "boat_desktop" {
					kept = append(kept, c.Name+"="+c.Value)
				}
			}
			// One Cookie header: RFC 6265 allows no more.
			pr.Out.Header.Del("Cookie")
			if len(kept) > 0 {
				pr.Out.Header.Set("Cookie", strings.Join(kept, "; "))
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			cookies := resp.Header["Set-Cookie"]
			out := make([]string, len(cookies))
			for i, c := range cookies {
				out[i] = hostOnlyCookie(c)
			}
			if len(out) > 0 {
				resp.Header["Set-Cookie"] = out
			}
			return nil
		},
	}
}

// ask answers Caddy's on_demand_tls check: 200 only for hosts we would serve,
// so nobody can make us request certificates for random names: a desktop
// host only once boat-api minted a link for it, and per team only so many new
// hostnames per window (the CA's quota is shared by every tenant).
func (g *gateway) ask(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.URL.Query().Get("domain"))
	if host == g.desktopDomain || host == "api."+g.domain {
		return
	}
	var v vm
	known := false
	if vmid, ok := label(host, g.desktopDomain); ok {
		v, known = g.lookupVM(vmid)
		known = known && g.gens.hasDesktop(r.Context(), vmid)
	} else if port, vmid, ok := g.parsePortHost(host); ok {
		v, known = g.lookupVM(vmid)
		known = known && slices.Contains(v.Ports, port)
	}
	if !known {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	if !g.certs.allow(v.Team, host, g.now()) {
		http.Error(w, "too many new certificates (team or instance limit), retry later", http.StatusTooManyRequests)
	}
}

// certQuota caps the new hostnames approved per team per window, and across
// all teams per global window (Let's Encrypt allows 50 new certificates per
// registered domain per week); a hostname approved once is always approved
// again (renewals). With a state path, approvals survive restarts.
type certQuota struct {
	limit        int // per team; 0 = none
	window       time.Duration
	globalLimit  int // all teams; 0 = none
	globalWindow time.Duration
	path         string // "" = in memory only

	mu     sync.Mutex
	issued map[string]bool
	recent map[string][]time.Time // team -> approvals within the window
	global []time.Time            // all approvals within the global window
}

type certQuotaState struct {
	Issued map[string]bool        `json:"issued"`
	Recent map[string][]time.Time `json:"recent"`
	Global []time.Time            `json:"global"`
}

func newCertQuota(limit int, window time.Duration, globalLimit int, globalWindow time.Duration, path string) (*certQuota, error) {
	if limit <= 0 && globalLimit <= 0 {
		return nil, nil
	}
	q := &certQuota{limit: limit, window: window, globalLimit: globalLimit, globalWindow: globalWindow, path: path,
		issued: map[string]bool{}, recent: map[string][]time.Time{}}
	if path == "" {
		return q, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return q, nil
	}
	if err != nil {
		return nil, err
	}
	var st certQuotaState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if st.Issued != nil {
		q.issued = st.Issued
	}
	if st.Recent != nil {
		q.recent = st.Recent
	}
	q.global = st.Global
	return q, nil
}

func (q *certQuota) allow(team, host string, now time.Time) bool {
	if q == nil {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.issued[host] {
		return true
	}
	kept := slices.DeleteFunc(slices.Clone(q.recent[team]), func(t time.Time) bool { return now.Sub(t) >= q.window })
	global := slices.DeleteFunc(slices.Clone(q.global), func(t time.Time) bool { return now.Sub(t) >= q.globalWindow })
	q.recent[team], q.global = kept, global
	if (q.limit > 0 && len(kept) >= q.limit) || (q.globalLimit > 0 && len(global) >= q.globalLimit) {
		return false
	}
	q.recent[team] = append(kept, now)
	q.global = append(global, now)
	q.issued[host] = true
	q.save()
	return true
}

// save writes the state (inside q.mu). A failed write is logged: approvals
// then only live in memory until the next successful one.
func (q *certQuota) save() {
	if q.path == "" {
		return
	}
	b, err := json.Marshal(certQuotaState{Issued: q.issued, Recent: q.recent, Global: q.global})
	if err == nil {
		tmp := q.path + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, q.path)
		}
	}
	if err != nil {
		log.Printf("cert quota state: %v", err)
	}
}

var (
	jumpURLRe     = regexp.MustCompile(`\Ahttp://127\.0\.0\.1:([0-9]{1,5})/internal/ssh/authorize\z`)
	jumpKeyTypeRe = regexp.MustCompile(`\A(ssh|ecdsa|sk)-[A-Za-z0-9@._+-]+\z`)
	jumpOptionsRe = regexp.MustCompile(`\Arestrict,port-forwarding((?:,permitopen="[0-9.]+:22")+)\z`)
	jumpTargetRe  = regexp.MustCompile(`permitopen="([0-9.]+):22"`)
)

const (
	jumpMaxKeyBytes    = 16384
	jumpMaxKeyB64Chars = ((jumpMaxKeyBytes + 2) / 3) * 4
	jumpMaxBodyBytes   = 65536
	jumpMaxHeaderBytes = 32768
	jumpMaxKeyLines    = 200
)

type headerTapConn struct {
	net.Conn
	buf        []byte
	rawHeaders []byte
	done       bool
	overflow   bool
}

func (c *headerTapConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && !c.done && !c.overflow {
		c.buf = append(c.buf, p[:n]...)
		if idx := bytes.Index(c.buf, []byte("\r\n\r\n")); idx >= 0 {
			c.rawHeaders = append([]byte(nil), c.buf[:idx]...)
			c.buf = nil
			c.done = true
		} else if idx := bytes.Index(c.buf, []byte("\n\n")); idx >= 0 {
			c.rawHeaders = append([]byte(nil), c.buf[:idx]...)
			c.buf = nil
			c.done = true
		} else if len(c.buf) > jumpMaxHeaderBytes {
			c.buf = nil
			c.overflow = true
		}
	}
	return n, err
}

func isASCIIBase64(s string) bool {
	if s == "" || len(s) > jumpMaxKeyB64Chars {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		ok := (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '+' || ch == '/' || ch == '='
		if !ok {
			return false
		}
	}
	return true
}

func validateJumpKey(keyType, keyBlob string) error {
	if !jumpKeyTypeRe.MatchString(keyType) || !isASCIIBase64(keyBlob) {
		return errors.New("invalid key format")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(keyBlob)
	if err != nil || len(raw) > jumpMaxKeyBytes || base64.StdEncoding.EncodeToString(raw) != keyBlob {
		return errors.New("invalid key base64")
	}
	if len(raw) <= 4 {
		return errors.New("truncated key")
	}
	typeLen := binary.BigEndian.Uint32(raw[:4])
	if uint64(typeLen) != uint64(len(keyType)) || len(raw) <= 4+len(keyType) || string(raw[4:4+len(keyType)]) != keyType {
		return errors.New("key type mismatch")
	}
	return nil
}

func validateJumpRawHeaders(raw []byte) (hasCL bool, expectedCL int, err error) {
	lines := strings.Split(string(raw), "\n")
	if len(lines) == 0 {
		return false, 0, errors.New("empty status line")
	}
	var clCount, teCount int
	for _, line := range lines[1:] {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			return false, 0, errors.New("obs-fold header")
		}
		name, val, ok := strings.Cut(line, ":")
		if !ok || name == "" || strings.TrimSpace(name) != name {
			return false, 0, errors.New("malformed header")
		}
		val = strings.Trim(val, " \t")
		switch {
		case strings.EqualFold(name, "Content-Length"):
			clCount++
			if clCount > 1 || val == "" {
				return false, 0, errors.New("duplicate or empty Content-Length")
			}
			for i := 0; i < len(val); i++ {
				if val[i] < '0' || val[i] > '9' {
					return false, 0, errors.New("non-digit Content-Length")
				}
			}
			n, convErr := strconv.ParseUint(val, 10, 64)
			if convErr != nil || n > jumpMaxBodyBytes {
				return false, 0, errors.New("Content-Length out of bounds")
			}
			hasCL = true
			expectedCL = int(n)
		case strings.EqualFold(name, "Transfer-Encoding"):
			teCount++
			if teCount > 1 || !strings.EqualFold(val, "chunked") {
				return false, 0, errors.New("unsupported Transfer-Encoding")
			}
		case strings.EqualFold(name, "Content-Encoding"):
			if !strings.EqualFold(val, "identity") {
				return false, 0, errors.New("unsupported Content-Encoding")
			}
		}
	}
	if clCount > 0 && teCount > 0 {
		return false, 0, errors.New("ambiguous Content-Length and Transfer-Encoding")
	}
	return hasCL, expectedCL, nil
}

func isValidPermitOpenIPv4(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is4() || addr.String() != ip {
		return false
	}
	if addr.IsUnspecified() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() {
		return false
	}
	b := addr.As4()
	if b[0] == 0 || b[0] >= 240 || b == [4]byte{255, 255, 255, 255} {
		return false
	}
	return true
}

func parseJumpAuthorizedKeys(body []byte, keyType, keyBlob string) ([]string, error) {
	if !utf8.Valid(body) || !json.Valid(body) {
		return nil, errors.New("invalid json")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("expected json object")
	}
	seenAuthKeys := false
	var lines []string
	for dec.More() {
		ktok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := ktok.(string)
		if !ok || key != "authorized_keys" || seenAuthKeys {
			return nil, errors.New("unexpected or duplicate json key")
		}
		seenAuthKeys = true
		vtok, err := dec.Token()
		if err != nil || vtok != json.Delim('[') {
			return nil, errors.New("authorized_keys must be an array")
		}
		for dec.More() {
			if len(lines) >= jumpMaxKeyLines {
				return nil, errors.New("too many authorized_keys lines")
			}
			etok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			line, ok := etok.(string)
			if !ok {
				return nil, errors.New("authorized_keys element must be string")
			}
			lines = append(lines, line)
		}
		if endArr, err := dec.Token(); err != nil || endArr != json.Delim(']') {
			return nil, errors.New("malformed authorized_keys array")
		}
	}
	if !seenAuthKeys {
		return nil, errors.New("missing authorized_keys")
	}
	if endObj, err := dec.Token(); err != nil || endObj != json.Delim('}') {
		return nil, errors.New("malformed json object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing json tokens")
	}

	suffix := " " + keyType + " " + keyBlob
	for _, line := range lines {
		if strings.ContainsAny(line, "\r\n\x00") || !strings.HasSuffix(line, suffix) {
			return nil, errors.New("line suffix mismatch")
		}
		options := line[:len(line)-len(suffix)]
		m := jumpOptionsRe.FindStringSubmatch(options)
		if m == nil {
			return nil, errors.New("invalid line options")
		}
		targets := jumpTargetRe.FindAllStringSubmatch(m[1], -1)
		if len(targets) == 0 {
			return nil, errors.New("missing permitopen targets")
		}
		for _, t := range targets {
			if !isValidPermitOpenIPv4(t[1]) {
				return nil, errors.New("invalid permitopen ip")
			}
		}
	}
	return lines, nil
}

func runJumpKeys(args []string, totalTimeout time.Duration) ([]string, error) {
	if len(args) != 4 {
		return nil, errors.New("want 4 args")
	}
	rawURL, keyType, keyBlob, user := args[0], args[1], args[2], args[3]
	m := jumpURLRe.FindStringSubmatch(rawURL)
	if m == nil || user != "jump" {
		return nil, errors.New("invalid url or user")
	}
	port, err := strconv.Atoi(m[1])
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid port")
	}
	if err := validateJumpKey(keyType, keyBlob); err != nil {
		return nil, err
	}

	reqBody, err := json.Marshal(struct {
		User    string `json:"user"`
		KeyType string `json:"key_type"`
		KeyBlob string `json:"key_blob"`
	}{
		User:    user,
		KeyType: keyType,
		KeyBlob: keyBlob,
	})
	if err != nil {
		return nil, err
	}

	if totalTimeout <= 0 {
		totalTimeout = 2 * time.Second
	}
	deadline := time.Now().Add(totalTimeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	dialTimeout := 500 * time.Millisecond
	if totalTimeout < dialTimeout {
		dialTimeout = totalTimeout
	}
	targetAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	var tap *headerTapConn
	tr := &http.Transport{
		Proxy:                  nil,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: jumpMaxHeaderBytes,
		DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: dialTimeout}
			conn, err := d.DialContext(dialCtx, "tcp", targetAddr)
			if err != nil {
				return nil, err
			}
			if err := conn.SetDeadline(deadline); err != nil {
				conn.Close()
				return nil, err
			}
			tap = &headerTapConn{Conn: conn}
			return tap, nil
		},
	}
	defer tr.CloseIdleConnections()

	client := &http.Client{
		Transport: tr,
		Timeout:   totalTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects forbidden")
		},
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+targetAddr+"/internal/ssh/authorize", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Cache-Control", "no-store")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent && tap != nil && tap.done && !tap.overflow {
		return nil, errJumpNoAuth
	}
	if resp.StatusCode != http.StatusOK || tap == nil || tap.overflow || !tap.done {
		return nil, errors.New("bad response status or headers")
	}
	hasCL, expectedCL, err := validateJumpRawHeaders(tap.rawHeaders)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, jumpMaxBodyBytes+1))
	if err != nil || len(body) > jumpMaxBodyBytes {
		return nil, errors.New("invalid response body length")
	}
	if hasCL && len(body) != expectedCL {
		return nil, errors.New("Content-Length mismatch")
	}
	// Ensure no trailing unconsumed bytes exist on chunked/EOF stream beyond jumpMaxBodyBytes.
	var extra [1]byte
	if n, readErr := resp.Body.Read(extra[:]); n > 0 || (readErr != nil && readErr != io.EOF) {
		return nil, errors.New("unexpected trailing response data")
	}
	return parseJumpAuthorizedKeys(body, keyType, keyBlob)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "jump-reaper" {
		if err := runJumpReaper(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "jump-reaper:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "jump-keys" {
		lines, err := runJumpKeys(os.Args[2:], 2*time.Second)
		if err != nil {
			os.Exit(1)
		}
		// Track the sshd connection so the reaper can end it when the grant goes
		// away. Untracked sessions could never be revoked: deny instead.
		if err := recordJumpSession(procfs{root: "/proc"}, jumpRecordDir(), os.Getppid(), os.Args[3], os.Args[4], lines, jumpAllowUntracked()); err != nil {
			fmt.Fprintln(os.Stderr, "jump-keys: cannot record session:", err)
			os.Exit(1)
		}
		var out strings.Builder
		for _, line := range lines {
			out.WriteString(line)
			out.WriteByte('\n')
		}
		if _, err := os.Stdout.WriteString(out.String()); err != nil {
			os.Exit(1)
		}
		return
	}
	listen := flag.String("listen", "127.0.0.1:8742", "HTTP address Caddy proxies to")
	askListen := flag.String("ask-listen", "127.0.0.1:8743", "on-demand TLS ask address (empty disables)")
	domain := flag.String("domain", "", "base domain, e.g. boat.example.com")
	vmDomain := flag.String("vm-domain", "", "port routes live at <port>-<vmid>.<vm-domain> (default vm.<domain>)")
	desktopDomain := flag.String("desktop-domain", "", "desktops live at <vmid>.<desktop-domain> (default desktop.<domain>)")
	routesPath := flag.String("routes", "/run/boat/routes.json", "boatd route table")
	keyPath := flag.String("desktop-key", "/var/lib/boat/secrets/desktop.key", "HMAC key shared with boat-api")
	api := flag.String("api", "", "boat-api base URL (http://127.0.0.1:8740), for port token revocations; empty disables them")
	certLimit := flag.Int("ask-team-limit", 10, "new TLS hostnames approved per team per -ask-window (already approved ones always pass); 0 = unlimited")
	certWindow := flag.Duration("ask-window", 24*time.Hour, "window of -ask-team-limit")
	certGlobal := flag.Int("ask-global-limit", 40, "new TLS hostnames approved across all teams per -ask-global-window (Let's Encrypt: 50 per registered domain per week); 0 = unlimited")
	certGlobalWindow := flag.Duration("ask-global-window", 7*24*time.Hour, "window of -ask-global-limit")
	certState := flag.String("ask-state", "", "file keeping certificate approvals across restarts (empty = memory only)")
	legacy := flag.Int64("legacy-hmac-until", 0, "accept tokens signed with the master key (from before per-purpose keys) that expire by this unix time; 0 or -1 = never (default). Set an absolute cutoff for a migration: a relative default would re-arm on every restart")
	flag.Parse()
	if *domain == "" {
		log.Fatal("-domain is required")
	}
	key, err := loadKey(*keyPath)
	if err != nil {
		log.Fatalf("desktop key: %v", err)
	}
	d := strings.ToLower(*domain)
	g := &gateway{
		domain: d, vmDomain: strings.ToLower(*vmDomain), desktopDomain: strings.ToLower(*desktopDomain),
		key: key, routes: newRoutes(*routesPath), now: time.Now,
	}
	if g.certs, err = newCertQuota(*certLimit, *certWindow, *certGlobal, *certGlobalWindow, *certState); err != nil {
		log.Fatalf("cert quota: %v", err)
	}
	g.legacyUntil = legacyCutoff(*legacy)
	if *api != "" {
		g.gens = newRouteGens(strings.TrimRight(*api, "/"))
		g.gens.onRefresh = func() { g.revalidateLive() }
		go g.gens.poll(context.Background())
	}
	go g.watchLive(context.Background(), routeGenPoll)
	if g.vmDomain == "" {
		g.vmDomain = "vm." + d
	}
	if g.desktopDomain == "" {
		g.desktopDomain = "desktop." + d
	}
	if *askListen != "" {
		go func() {
			log.Fatal(http.ListenAndServe(*askListen, http.HandlerFunc(g.ask)))
		}()
	}
	srv := &http.Server{Addr: *listen, Handler: g, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("boat-gateway on %s: *.%s, *.%s", *listen, g.vmDomain, g.desktopDomain)
	log.Fatal(srv.ListenAndServe())
}
