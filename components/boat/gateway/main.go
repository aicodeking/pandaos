// boat-gateway: the HTTP router behind Caddy (Caddy does TLS).
//
//	<port>-<vmid>.vm.<domain>  -> <vm ip>:<port>   (ports listed in routes.json)
//	desktop.<domain>/<vmid>/   -> <vm ip>:<desktopPort>, signed-URL token required
//
// Routes come from boatd's /run/boat/routes.json, re-read when its mtime or
// size changes, so no Caddy reload is needed when VMs or ports change.
// A second loopback listener answers Caddy's on-demand TLS "ask" checks.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cookieName         = "boat_gw"      // ours: the signed link, Path=/<vmid>/
	guestCookie        = "boat_desktop" // the guest agent's desktop proxy (:6080)
	defaultDesktopPort = 6080
)

type vm struct {
	IP          string `json:"ip"`
	Ports       []int  `json:"ports"`
	DesktopPort int    `json:"desktopPort"`
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
	domain string // e.g. boat.example.com
	key    []byte // desktop token HMAC key
	routes *routes
	now    func() time.Time
}

// guestToken is the token boat-api starts the guest desktop with, so the
// gateway can pass the guest proxy's check without storing per-VM state:
// hex(HMAC-SHA256(key, "guest-desktop.<vmid>")).
func guestToken(key []byte, vmid string) string {
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "guest-desktop.%s", vmid)
	return hex.EncodeToString(m.Sum(nil))
}

// sign is boat-api's SignDesktop: hex(HMAC-SHA256(key, "<vmid>.<exp>")).
// Links look like /<vmid>?exp=<unix>&sig=<hex>; the cookie holds "<exp>.<sig>".
func sign(key []byte, vmid string, exp int64) string {
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%s.%d", vmid, exp)
	return hex.EncodeToString(m.Sum(nil))
}

func (g *gateway) verify(vmid, token string) (time.Time, error) {
	expStr, sig, ok := strings.Cut(token, ".")
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if !ok || err != nil {
		return time.Time{}, errors.New("malformed token")
	}
	if !hmac.Equal([]byte(sign(g.key, vmid, exp)), []byte(strings.ToLower(sig))) {
		return time.Time{}, errors.New("bad signature")
	}
	t := time.Unix(exp, 0)
	if !g.now().Before(t) {
		return time.Time{}, errors.New("token expired")
	}
	return t, nil
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// parsePortHost splits "<port>-<vmid>.vm.<domain>".
func (g *gateway) parsePortHost(host string) (int, string, bool) {
	label, ok := strings.CutSuffix(host, ".vm."+g.domain)
	if !ok || strings.Contains(label, ".") {
		return 0, "", false
	}
	portStr, vmid, ok := strings.Cut(label, "-")
	port, err := strconv.Atoi(portStr)
	if !ok || err != nil || port < 1 || port > 65535 || vmid == "" {
		return 0, "", false
	}
	return port, vmid, true
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	if host == "desktop."+g.domain {
		g.desktop(w, r)
		return
	}
	port, vmid, ok := g.parsePortHost(host)
	if !ok {
		http.NotFound(w, r)
		return
	}
	v, ok := g.routes.lookup(vmid)
	if !ok || !hasPort(v.Ports, port) {
		http.Error(w, "no such VM port route", http.StatusNotFound)
		return
	}
	proxy(v.IP, port, "", "").ServeHTTP(w, r)
}

func hasPort(ports []int, p int) bool {
	for _, q := range ports {
		if q == p {
			return true
		}
	}
	return false
}

func (g *gateway) desktop(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/")
	vmid, _, hasSlash := strings.Cut(rest, "/")
	v, ok := g.routes.lookup(vmid)
	if vmid == "" || !ok {
		http.NotFound(w, r)
		return
	}
	prefix := "/" + vmid
	if !hasSlash {
		u := *r.URL
		u.Path = prefix + "/"
		http.Redirect(w, r, u.RequestURI(), http.StatusFound)
		return
	}
	if q := r.URL.Query(); q.Has("sig") {
		tok := q.Get("exp") + "." + q.Get("sig")
		exp, err := g.verify(vmid, tok)
		if err != nil {
			http.Error(w, "desktop link invalid: "+err.Error(), http.StatusForbidden)
			return
		}
		// Trade the URL token for a cookie, then drop it from the URL so it
		// does not linger in history or leak via Referer.
		http.SetCookie(w, &http.Cookie{
			Name: cookieName, Value: tok, Path: prefix + "/", Expires: exp,
			HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		})
		u := *r.URL
		q.Del("exp")
		q.Del("sig")
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.RequestURI(), http.StatusFound)
		return
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		http.Error(w, "desktop link required: get one from the API (GET /v1/vms/<id>/desktop)", http.StatusUnauthorized)
		return
	}
	if _, err := g.verify(vmid, c.Value); err != nil {
		http.Error(w, "desktop session invalid: "+err.Error(), http.StatusForbidden)
		return
	}
	port := v.DesktopPort
	if port == 0 {
		port = defaultDesktopPort
	}
	proxy(v.IP, port, prefix, guestToken(g.key, vmid)).ServeHTTP(w, r)
}

// proxy forwards to ip:port and strips a path prefix. With a guest token
// (desktop), the client's boat_* cookies are replaced by the guest proxy's
// cookie, so our link never reaches the guest and clients cannot pick the
// guest token. Caddy's X-Forwarded-* headers are kept.
func proxy(ip string, port int, stripPrefix, guestTok string) http.Handler {
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(ip, strconv.Itoa(port))}
	return &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)
		pr.Out.Host = pr.In.Host
		pr.Out.URL.Path = "/" + strings.TrimPrefix(strings.TrimPrefix(pr.In.URL.Path, stripPrefix), "/")
		pr.Out.URL.RawPath = ""
		for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host"} {
			if v := pr.In.Header.Values(h); len(v) > 0 {
				pr.Out.Header[h] = v
			}
		}
		if guestTok != "" {
			// One Cookie header: RFC 6265 allows no more.
			kept := []string{guestCookie + "=" + guestTok}
			for _, c := range pr.In.Cookies() {
				if c.Name != cookieName && c.Name != guestCookie {
					kept = append(kept, c.Name+"="+c.Value)
				}
			}
			pr.Out.Header.Set("Cookie", strings.Join(kept, "; "))
		}
	}}
}

// ask answers Caddy's on_demand_tls check: 200 only for hosts we would serve,
// so nobody can make us request certificates for random names.
func (g *gateway) ask(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.URL.Query().Get("domain"))
	if host == "desktop."+g.domain || host == "api."+g.domain {
		return
	}
	if port, vmid, ok := g.parsePortHost(host); ok {
		if v, ok := g.routes.lookup(vmid); ok && hasPort(v.Ports, port) {
			return
		}
	}
	http.Error(w, "unknown host", http.StatusNotFound)
}

// loadKey mirrors boat-api's LoadMasterKey: 32 raw bytes or 64 hex chars.
func loadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) == 32 {
		return b, nil
	}
	k, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(k) != 32 {
		return nil, fmt.Errorf("%s: want 32 raw bytes or 64 hex chars", path)
	}
	return k, nil
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8742", "HTTP address Caddy proxies to")
	askListen := flag.String("ask-listen", "127.0.0.1:8743", "on-demand TLS ask address (empty disables)")
	domain := flag.String("domain", "", "base domain, e.g. boat.example.com")
	routesPath := flag.String("routes", "/run/boat/routes.json", "boatd route table")
	keyPath := flag.String("desktop-key", "/var/lib/boat/secrets/desktop.key", "HMAC key for desktop links")
	flag.Parse()
	if *domain == "" {
		log.Fatal("-domain is required")
	}
	key, err := loadKey(*keyPath)
	if err != nil {
		log.Fatalf("desktop key: %v", err)
	}
	g := &gateway{domain: strings.ToLower(*domain), key: key, routes: &routes{path: *routesPath}, now: time.Now}
	if *askListen != "" {
		go func() {
			log.Fatal(http.ListenAndServe(*askListen, http.HandlerFunc(g.ask)))
		}()
	}
	srv := &http.Server{Addr: *listen, Handler: g, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("boat-gateway on %s for *.vm.%s and desktop.%s", *listen, g.domain, g.domain)
	log.Fatal(srv.ListenAndServe())
}
