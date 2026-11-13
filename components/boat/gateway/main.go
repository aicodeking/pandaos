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
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultDesktopPort = 6080

type vm struct {
	IP          string `json:"ip"`
	Ports       []int  `json:"ports"`
	Public      []int  `json:"public"`
	DesktopPort int    `json:"desktopPort"`
	Team        string `json:"team"`
}

type routeFile struct {
	VMs map[string]vm `json:"vms"`
}

// routes caches routes.json, re-reading it when it changes on disk.
type routes struct {
	path  string
	mu    sync.Mutex
	stamp string
	vms   map[string]vm
}

func (r *routes) lookup(id string) (vm, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fi, err := os.Stat(r.path)
	if err != nil {
		// No file yet (boatd not up) or it was removed: no routes.
		r.stamp, r.vms = "", nil
		return vm{}, false
	}
	stamp := fmt.Sprint(fi.ModTime().UnixNano(), fi.Size())
	if stamp != r.stamp {
		vms, err := readRoutes(r.path)
		if err != nil {
			// Keep the last good table; boatd writes atomically, so this is rare.
			log.Printf("routes: %v (keeping previous table)", err)
		} else {
			r.stamp, r.vms = stamp, vms
		}
	}
	v, ok := r.vms[id]
	return v, ok && net.ParseIP(v.IP) != nil
}

func readRoutes(path string) (map[string]vm, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f routeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return f.VMs, nil
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
	host := hostOnly(r.Host)
	switch {
	case host == g.desktopDomain:
		g.desktopLink(w, r)
	case strings.HasSuffix(host, "."+g.desktopDomain):
		vmid, ok := label(host, g.desktopDomain)
		if !ok {
			http.NotFound(w, r)
			return
		}
		g.desktop(w, r, vmid)
	default:
		port, vmid, ok := g.parsePortHost(host)
		if !ok {
			http.NotFound(w, r)
			return
		}
		g.portRoute(w, r, port, vmid)
	}
}

// desktopLink moves boat-api's desktop.<d>/<vmid>?exp&sig link to the VM's
// own origin. It sets nothing on the shared desktop.<d> origin.
func (g *gateway) desktopLink(w http.ResponseWriter, r *http.Request) {
	vmid := strings.Trim(r.URL.Path, "/")
	if _, ok := g.routes.lookup(vmid); !ok || strings.Contains(vmid, "/") || !r.URL.Query().Has("sig") {
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

func (g *gateway) desktop(w http.ResponseWriter, r *http.Request, vmid string) {
	v, ok := g.routes.lookup(vmid)
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
	check := func(tok string) error { _, err := g.verify(tok, purposeDesktopLink, sign); return err }
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
	proxy(v.IP, port, g.guestCookies(vmid)).ServeHTTP(w, r)
}

func (g *gateway) portRoute(w http.ResponseWriter, r *http.Request, port int, vmid string) {
	v, ok := g.routes.lookup(vmid)
	if !ok || !slices.Contains(v.Ports, port) {
		http.Error(w, "no such VM port route", http.StatusNotFound)
		return
	}
	if !slices.Contains(v.Public, port) {
		gen, ready := g.gens.gen(r.Context(), vmid)
		if !ready {
			http.Error(w, "private port: token revocations are unavailable, retry shortly", http.StatusServiceUnavailable)
			return
		}
		sign := func(k []byte, exp int64) string { return signRoute(k, port, vmid, gen, exp) }
		check := func(tok string) error { _, err := g.verify(tok, purposePortRoute, sign); return err }
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
	proxy(v.IP, port, "").ServeHTTP(w, r)
}

// withoutQuery is r's path and query minus the named parameters.
func withoutQuery(r *http.Request, drop ...string) string {
	q := r.URL.Query()
	for _, k := range drop {
		q.Del(k)
	}
	u := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.RequestURI()
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
func proxy(ip string, port int, guestCookie string) http.Handler {
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(ip, strconv.Itoa(port))}
	return &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
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
	}, ModifyResponse: func(resp *http.Response) error {
		cookies := resp.Header["Set-Cookie"]
		out := make([]string, len(cookies))
		for i, c := range cookies {
			out[i] = hostOnlyCookie(c)
		}
		if len(out) > 0 {
			resp.Header["Set-Cookie"] = out
		}
		return nil
	}}
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
		v, known = g.routes.lookup(vmid)
		known = known && g.gens.hasDesktop(r.Context(), vmid)
	} else if port, vmid, ok := g.parsePortHost(host); ok {
		v, known = g.routes.lookup(vmid)
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

func main() {
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
	legacy := flag.Int64("legacy-hmac-until", 0, "accept tokens signed with the master key (from before per-purpose keys) that expire by this unix time; 0 = start + 8 days, -1 = never")
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
		key: key, routes: &routes{path: *routesPath}, now: time.Now,
	}
	if g.certs, err = newCertQuota(*certLimit, *certWindow, *certGlobal, *certGlobalWindow, *certState); err != nil {
		log.Fatalf("cert quota: %v", err)
	}
	switch {
	case *legacy == 0:
		g.legacyUntil = time.Now().Add(legacyWindow)
	case *legacy > 0:
		g.legacyUntil = time.Unix(*legacy, 0)
	}
	if *api != "" {
		g.gens = newRouteGens(strings.TrimRight(*api, "/"))
		go g.gens.poll(context.Background())
	}
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
