package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Port routes. The gateway serves <port>-<vmid>.<vm domain>. A route is
// private by default: the gateway requires a token minted by
// GET /v1/vms/{id}/ports/{port}/url (HMAC with the desktop key, see
// SignRoute). Public ports (auth=public) are created with boatd public:true:
// they get a raw TCP host port, and boatd lists them as "public" in
// routes.json, which makes the gateway skip the token.

const maxPortURLTTL = 7 * 24 * time.Hour

func portNum(v any) int {
	f, _ := v.(float64)
	return int(f)
}

// vmPorts returns the boatd port objects annotated with "auth".
func (s *server) vmPorts(vm VM) []map[string]any {
	out := []map[string]any{}
	list, _ := vm["ports"].([]any)
	for _, x := range list {
		pm, ok := x.(map[string]any)
		if !ok {
			continue
		}
		c := make(map[string]any, len(pm)+1)
		for k, v := range pm {
			c[k] = v
		}
		c["auth"] = "private"
		if portNum(pm["host_port"]) > 0 { // boatd: public ports have a host port
			c["auth"] = "public"
		}
		out = append(out, c)
	}
	return out
}

func (s *server) listPorts(w http.ResponseWriter, r *http.Request, p *principal) {
	if vm, ok := s.ownVM(w, r, p); ok {
		writeJSON(w, 200, map[string]any{"ports": s.vmPorts(vm)})
	}
}

func (s *server) addPort(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Port   int    `json:"port"`
		Public bool   `json:"public"` // shorthand for auth=public
		Auth   string `json:"auth"`   // private (default) | public
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Port < 1 || body.Port > 65535 {
		writeErr(w, r, 400, "invalid_request", "port must be 1-65535")
		return
	}
	switch {
	case body.Auth == "" && body.Public:
		body.Auth = "public"
	case body.Auth == "":
		body.Auth = "private"
	case body.Auth != "private" && body.Auth != "public":
		writeErr(w, r, 400, "invalid_request", "auth must be private or public")
		return
	}
	public := body.Auth == "public"
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id := r.PathValue("id")
	var out map[string]any
	if err := s.boatd.call(r.Context(), "POST", vmPath(id, "ports"), map[string]any{"port": body.Port, "public": public}, &out); err != nil {
		writeUpstream(w, r, err)
		return
	}
	if out == nil {
		out = map[string]any{"guest_port": body.Port}
	}
	out["auth"] = body.Auth
	writeJSON(w, 201, out)
}

func pathPort(r *http.Request) (int, bool) {
	port, err := strconv.Atoi(r.PathValue("port"))
	return port, err == nil && port >= 1 && port <= 65535
}

func (s *server) removePort(w http.ResponseWriter, r *http.Request, p *principal) {
	port, ok := pathPort(r)
	if !ok {
		writeErr(w, r, 400, "invalid_request", "port must be 1-65535")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id := r.PathValue("id")
	path := vmPath(id, "ports") + "?port=" + strconv.Itoa(port)
	if err := s.boatd.call(r.Context(), "DELETE", path, nil, nil); err != nil {
		writeUpstream(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// SignRoute: hex(HMAC-SHA256(desktopKey, "route.<label>.<exp>")), label =
// "<port>-<vmid>" (the gateway's host label). Desktop links sign
// "<vmid>.<exp>"; boatd ids are "vm-<hex>", so the "route." prefix keeps the
// payload spaces apart. Matches boat-gateway.
func SignRoute(key []byte, label string, exp int64) string {
	return signHex(key, fmt.Sprintf("route.%s.%d", label, exp))
}

// vmDomainURL turns -vm-domain into a base URL: "vm.example.com" means
// https; a value with a scheme (dev: "http://vm.local:8742") is used as is.
func vmDomainURL(d string) (*url.URL, error) {
	if !strings.Contains(d, "://") {
		d = "https://" + d
	}
	u, err := url.Parse(d)
	if err == nil && u.Host == "" {
		err = fmt.Errorf("no host in %q", d)
	}
	return u, err
}

// portURL mints the gateway URL of a port route; private routes carry
// ?boat_exp=&boat_sig= which the gateway trades for a host-only cookie.
func (s *server) portURL(w http.ResponseWriter, r *http.Request, p *principal) {
	port, ok := pathPort(r)
	if !ok {
		writeErr(w, r, 400, "invalid_request", "port must be 1-65535")
		return
	}
	ttl := s.cfg.PortURLTTL
	if v := r.URL.Query().Get("ttl_s"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 60 || time.Duration(n)*time.Second > maxPortURLTTL {
			writeErr(w, r, 400, "invalid_request", "ttl_s must be 60..604800")
			return
		}
		ttl = time.Duration(n) * time.Second
	}
	base, err := vmDomainURL(s.cfg.VMDomain)
	if s.cfg.VMDomain == "" || err != nil || len(s.cfg.DesktopKey) == 0 {
		writeErr(w, r, 501, "not_configured", "port routes are not configured (-vm-domain, -desktop-key-file)")
		return
	}
	vm, ok := s.ownVM(w, r, p)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var route map[string]any
	for _, pm := range s.vmPorts(vm) {
		if portNum(pm["guest_port"]) == port {
			route = pm
		}
	}
	if route == nil {
		writeErr(w, r, 404, "not_found", "port is not exposed; add it first")
		return
	}
	label := str(route, "hostname")
	if label == "" {
		label = fmt.Sprintf("%d-%s", port, id)
	}
	u := url.URL{Scheme: base.Scheme, Host: label + "." + base.Host, Path: "/"}
	out := map[string]any{"url": "", "auth": route["auth"], "guest_port": port}
	if route["auth"] == "private" {
		exp := time.Now().Add(ttl).Unix()
		sig := SignRoute(s.cfg.DesktopKey, label, exp)
		u.RawQuery = url.Values{"boat_exp": {strconv.FormatInt(exp, 10)}, "boat_sig": {sig}}.Encode()
		out["expires_at"] = time.Unix(exp, 0).UTC()
		out["token"] = fmt.Sprintf("%d.%s", exp, sig) // header X-Boat-Route-Token
	}
	out["url"] = u.String()
	writeJSON(w, 200, out)
}
