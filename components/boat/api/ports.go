package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
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
	if public && p.VMID != "" && !p.Team.SelfToken.PublicPorts {
		writeErr(w, r, 403, "forbidden", "a VM's own token cannot open public ports; use a team key, or a team admin can allow it (PUT /v1/teams/{id}/self-token)")
		return
	}
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
	// A later add of this port must not bring its old tokens back. The route is
	// already gone, so a failure here is only logged.
	if _, err := s.bumpRouteGen(id); err != nil {
		log.Printf("%s: revoke tokens of %s: %v", info(r).id, id, err)
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// SignRoute: hex(HMAC-SHA256(desktopKey, "route.<label>.<exp>")), label =
// "<port>-<vmid>" (the gateway's host label). Desktop links sign
// "<vmid>.<exp>"; boatd ids are "vm-<hex>", so the "route." prefix keeps the
// payload spaces apart. Matches boat-gateway. After a VM's tokens are revoked
// (gen > 0) the payload is "route.<label>.g<gen>.<exp>", which kills every
// earlier token and cookie of that VM.
func SignRoute(key []byte, label string, gen int, exp int64) string {
	if gen == 0 {
		return signHex(key, fmt.Sprintf("route.%s.%d", label, exp))
	}
	return signHex(key, fmt.Sprintf("route.%s.g%d.%d", label, gen, exp))
}

func (s *server) bumpRouteGen(vmID string) (int, error) {
	gen := 0
	err := s.store.Update(func(st *State) error {
		st.RouteGens[vmID]++
		gen = st.RouteGens[vmID]
		return nil
	})
	return gen, err
}

// revokePortTokens ends every token, link and cookie of the VM's private ports
// and desktop.
func (s *server) revokePortTokens(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	gen, err := s.bumpRouteGen(r.PathValue("id"))
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "generation": gen})
}

// routeGens is what boat-gateway polls. The generations are not secret, but only
// the gateway on this host has a reason to ask: Caddy, which adds
// X-Forwarded-For, never forwards /internal/.
func (s *server) routeGens(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() || r.Header.Get("X-Forwarded-For") != "" {
		writeErr(w, r, 404, "not_found", "no such endpoint")
		return
	}
	st := s.store.Snapshot()
	desktops := []string{}
	for id := range st.DesktopVMs {
		desktops = append(desktops, id)
	}
	slices.Sort(desktops)
	writeJSON(w, 200, map[string]any{"gens": st.RouteGens, "desktops": desktops})
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
		if err := s.recordPortMinter(id, p.Key.ID); err != nil {
			s.storeErr(w, r, err)
			return
		}
		expAt := time.Now().Add(ttl)
		if p.Key.Expires != nil && p.Key.Expires.Before(expAt) {
			expAt = *p.Key.Expires // a link never outlives its key
		}
		exp := expAt.Unix()
		sig := SignRoute(PurposeKey(s.cfg.DesktopKey, purposePortRoute), label, s.store.Snapshot().RouteGens[id], exp)
		u.RawQuery = url.Values{"boat_exp": {strconv.FormatInt(exp, 10)}, "boat_sig": {sig}}.Encode()
		out["expires_at"] = time.Unix(exp, 0).UTC()
		out["token"] = fmt.Sprintf("%d.%s", exp, sig) // header X-Boat-Route-Token
	}
	out["url"] = u.String()
	writeJSON(w, 200, out)
}

// recordPortMinter notes that key minted a private link of vmID, so deleting
// the key or removing its member revokes the VM's port links (revokeLinksOf).
// A VM's own token (vm_ ids) is not recorded: it dies with the VM.
func (s *server) recordPortMinter(vmID, keyID string) error {
	if strings.HasPrefix(keyID, "vm_") || slices.Contains(s.store.Snapshot().PortMinters[vmID], keyID) {
		return nil
	}
	return s.store.Update(func(st *State) error {
		if !slices.Contains(st.PortMinters[vmID], keyID) {
			st.PortMinters[vmID] = append(slices.Clone(st.PortMinters[vmID]), keyID)
		}
		return nil
	})
}

// revokeLinksOf bumps the route generation of every VM whose private port (or desktop)
// links one of gone minted (inside Store.Update). The gateway then refuses
// them; signed file URLs check their key at use (keyAlive) instead.
func revokeLinksOf(st *State, gone func(keyID string) bool) {
	for vm, keys := range st.PortMinters {
		kept := slices.DeleteFunc(slices.Clone(keys), gone)
		if len(kept) == len(keys) {
			continue
		}
		st.RouteGens[vm]++
		if len(kept) == 0 {
			delete(st.PortMinters, vm)
		} else {
			st.PortMinters[vm] = kept
		}
	}
}

// keyAlive says whether the key that minted a signed URL still stands: it
// exists in team, has not expired, and its user is still a member. A VM's own
// token (vm_<id>) stands while the VM's token record does.
func (s *server) keyAlive(keyID, team string, now time.Time) bool {
	return keyAliveIn(s.store.Snapshot(), keyID, team, now)
}

func keyAliveIn(st *State, keyID, team string, now time.Time) bool {
	if vm, ok := strings.CutPrefix(keyID, "vm_"); ok {
		t := st.VMTokens[vm]
		return t != nil && t.TeamID == team
	}
	k, t := st.Keys[keyID], st.Teams[team]
	return k != nil && t != nil && k.TeamID == team && (k.Expires == nil || now.Before(*k.Expires)) && t.role(k.UserID) != ""
}
