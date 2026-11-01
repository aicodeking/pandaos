package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Port routes. The gateway serves <port>-<vmid>.<ports domain>. A route is
// private by default: the gateway requires a token minted by
// GET /v1/vms/{id}/ports/{port}/url (HMAC with the desktop key, see SignPort).
// Public routes are listed in cfg.PublicPortsFile and are open; only they
// also get boatd's raw TCP host port (which cannot carry a token).

const maxPortURLTTL = 7 * 24 * time.Hour

func portKey(vmID string, port int) string { return vmID + ":" + strconv.Itoa(port) }

func portNum(v any) int {
	f, _ := v.(float64)
	return int(f)
}

// vmPorts returns the boatd port objects annotated with "auth".
func (s *server) vmPorts(vm VM) []map[string]any {
	pub := s.store.Snapshot().PublicPorts
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
		if pub[portKey(str(vm, "id"), portNum(pm["guest_port"]))] {
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
	// Record the mode first so a route never opens before it is known.
	if err := s.setPortPublic(id, body.Port, public); err != nil {
		s.storeErr(w, r, err)
		return
	}
	var out map[string]any
	if err := s.boatd.call(r.Context(), "POST", vmPath(id, "ports"), map[string]any{"port": body.Port, "public": public}, &out); err != nil {
		writeUpstream(w, r, err)
		return
	}
	s.syncPublicPorts(r.Context())
	if out == nil {
		out = map[string]any{"guest_port": body.Port}
	}
	out["auth"] = body.Auth
	writeJSON(w, 201, out)
}

func (s *server) setPortPublic(id string, port int, public bool) error {
	return s.store.Update(func(st *State) error {
		if public {
			st.PublicPorts[portKey(id, port)] = true
		} else {
			delete(st.PublicPorts, portKey(id, port))
		}
		return nil
	})
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
	if err := s.setPortPublic(id, port, false); err != nil {
		log.Printf("%s: %v", info(r).id, err)
	}
	s.syncPublicPorts(r.Context())
	writeJSON(w, 200, map[string]any{"ok": true})
}

// SignPort: hex(HMAC-SHA256(key, "port.<vmid>.<port>.<exp>")) with the desktop key.
// Desktop links sign "<vmid>.<exp>"; boatd ids are "vm-<hex>", so the
// "port." prefix keeps the two payload spaces apart.
func SignPort(key []byte, vmID string, port int, exp int64) string {
	return signHex(key, fmt.Sprintf("port.%s.%d.%d", vmID, port, exp))
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
	base, err := url.Parse(s.cfg.PortsBaseURL)
	if s.cfg.PortsBaseURL == "" || err != nil || base.Host == "" || len(s.cfg.DesktopKey) == 0 {
		writeErr(w, r, 501, "not_configured", "port routes are not configured (-ports-base-url, -desktop-key-file)")
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
		u.RawQuery = url.Values{"boat_exp": {strconv.FormatInt(exp, 10)}, "boat_sig": {SignPort(s.cfg.DesktopKey, id, port, exp)}}.Encode()
		out["expires_at"] = time.Unix(exp, 0).UTC()
		out["token"] = fmt.Sprintf("%d.%s", exp, SignPort(s.cfg.DesktopKey, id, port, exp))
	}
	out["url"] = u.String()
	writeJSON(w, 200, out)
}

// syncPublicPorts rewrites cfg.PublicPortsFile from state ∩ boatd's live VMs:
//
//	{"version":1,"public":["8080-vm-1a2b…", …]}
//
// Anything not listed is private at the gateway.
func (s *server) syncPublicPorts(ctx context.Context) {
	if err := s.syncPublicPortsErr(ctx); err != nil {
		log.Printf("public ports sync: %v", err)
	}
}

func (s *server) syncPublicPortsErr(ctx context.Context) error {
	if s.cfg.PublicPortsFile == "" {
		return nil
	}
	s.portsMu.Lock()
	defer s.portsMu.Unlock()
	var list struct {
		VMs []VM `json:"vms"`
	}
	if err := s.boatd.call(ctx, "GET", "/v1/vms", nil, &list); err != nil {
		return err
	}
	live := map[string]bool{}
	for _, vm := range list.VMs {
		live[str(vm, "id")] = true
	}
	routes := []string{}
	for k := range s.store.Snapshot().PublicPorts {
		id, port, _ := strings.Cut(k, ":")
		if live[id] {
			routes = append(routes, port+"-"+id)
		}
	}
	sort.Strings(routes)
	b, _ := json.Marshal(map[string]any{"version": 1, "public": routes})
	return writeAtomicMode(s.cfg.PublicPortsFile, append(b, '\n'), 0o640)
}
