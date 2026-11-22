package main

import (
	"errors"
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
	if public && !p.has("exec") {
		writeErr(w, r, http.StatusForbidden, "forbidden", "public port exposure requires scope exec")
		return
	}
	if public && p.VMID != "" {
		st, now := s.store.Snapshot(), time.Now()
		team := st.Teams[p.Team.ID]
		tok := st.VMTokens[p.VMID]
		if team == nil || !team.SelfToken.PublicPorts || st.ForeignVMs[p.VMID] || tok == nil || tok.Retired || tok.RestrictSelf || tok.TeamID != p.Team.ID || !keyAliveIn(st, p.Key.ID, p.Team.ID, "vms:write", now) {
			writeErr(w, r, 403, "forbidden", "a VM's own token cannot open public ports; use a team key, or a team admin can allow it (PUT /v1/teams/{id}/self-token)")
			return
		}
	}
	vm, ok := s.ownVM(w, r, p)
	if !ok {
		return
	}
	if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
		return
	}
	if public && p.VMID == "" && s.revalidatePrincipal(w, r, p, "exec") == nil {
		return
	}
	if public && p.VMID != "" {
		st, now := s.store.Snapshot(), time.Now()
		team := st.Teams[p.Team.ID]
		tok := st.VMTokens[p.VMID]
		if team == nil || !team.SelfToken.PublicPorts || st.ForeignVMs[p.VMID] || tok == nil || tok.Retired || tok.RestrictSelf || tok.TeamID != p.Team.ID || !keyAliveIn(st, p.Key.ID, p.Team.ID, "vms:write", now) {
			writeErr(w, r, 403, "forbidden", "a VM's own token cannot open public ports; use a team key, or a team admin can allow it (PUT /v1/teams/{id}/self-token)")
			return
		}
	}
	prevAuth := ""
	for _, pm := range s.vmPorts(vm) {
		if portNum(pm["guest_port"]) == body.Port {
			prevAuth = str(pm, "auth")
			break
		}
	}
	id := r.PathValue("id")
	var out map[string]any
	if err := s.boatd.call(r.Context(), "POST", vmPath(id, "ports"), map[string]any{"port": body.Port, "public": public}, &out); err != nil {
		writeUpstream(w, r, err)
		return
	}
	if prevAuth != "" && prevAuth != body.Auth {
		if _, err := s.bumpRouteGen(id); err != nil {
			log.Printf("%s: revoke tokens of %s on port privacy change: %v", info(r).id, id, err)
		}
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
	if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
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
	if s.revalidatePrincipal(w, r, p, "exec") == nil {
		return
	}
	gen, err := s.bumpRouteGen(r.PathValue("id"))
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "generation": gen})
}

// hasForwardingHeaders reports whether the request carries any proxy hop
// header: Forwarded or any X-Forwarded-* (X-Real-Ip too). Internal routes
// answer only requests that reached the API directly.
func hasForwardingHeaders(r *http.Request) bool {
	for name := range r.Header {
		l := strings.ToLower(name)
		if l == "forwarded" || l == "x-real-ip" || strings.HasPrefix(l, "x-forwarded-") {
			return true
		}
	}
	return false
}

// directLoopback: the peer is a loopback address and no proxy touched the
// request. Shared by /internal/route-gens and /internal/ssh/authorize.
func directLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback() && !hasForwardingHeaders(r)
}

// routeGens is what boat-gateway polls. The generations are not secret, but only
// the gateway on this host has a reason to ask: Caddy, which adds
// X-Forwarded-For, never forwards /internal/.
func (s *server) routeGens(w http.ResponseWriter, r *http.Request) {
	if !directLoopback(r) {
		writeErr(w, r, 404, "not_found", "no such endpoint")
		return
	}
	now := time.Now()
	st := s.store.Snapshot()
	gens := st.RouteGens
	if hasDeadPortMinters(st, now) {
		if err := s.store.Update(func(st *State) error {
			revokeDeadPortMinters(st, time.Now())
			return nil
		}); err == nil {
			st = s.store.Snapshot()
			gens = st.RouteGens
		} else {
			gens = bumpedRouteGensSnapshot(st, now)
		}
	}
	desktops := []string{}
	for id := range st.DesktopVMs {
		desktops = append(desktops, id)
	}
	slices.Sort(desktops)
	writeJSON(w, 200, map[string]any{"gens": gens, "desktops": desktops})
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
		gen, exp, err := s.recordPortMinter(id, p, false, ttl)
		if err != nil {
			if errors.Is(err, errLinkUnauthorized) {
				writeErr(w, r, 403, "forbidden", err.Error())
			} else {
				s.storeErr(w, r, err)
			}
			return
		}
		sig := SignRoute(PurposeKey(s.cfg.DesktopKey, purposePortRoute), label, gen, exp)
		u.RawQuery = url.Values{"boat_exp": {strconv.FormatInt(exp, 10)}, "boat_sig": {sig}}.Encode()
		out["expires_at"] = time.Unix(exp, 0).UTC()
		out["token"] = fmt.Sprintf("%d.%s", exp, sig) // header X-Boat-Route-Token
	}
	out["url"] = u.String()
	writeJSON(w, 200, out)
}

var errLinkUnauthorized = errors.New("authorization to mint this private link no longer stands")

// Authorization, registration and generation capture share revocation's lock.
// Signing a later generation would let a revoked request mint a live link.
func (s *server) recordPortMinter(vmID string, p *principal, desktop bool, ttl time.Duration) (gen int, exp int64, err error) {
	err = s.store.Update(func(st *State) error {
		now := time.Now()
		revokeDeadPortMinters(st, now)
		if !p.has("exec") || !keyAliveIn(st, p.Key.ID, p.Team.ID, "exec", now) ||
			(p.Key.Expires != nil && !now.Before(*p.Key.Expires)) {
			return errLinkUnauthorized
		}
		if target := st.VMTokens[vmID]; target != nil && (target.Retired || target.TeamID != p.Team.ID) {
			return errLinkUnauthorized
		}
		key := p.Key
		var genEntry string
		if p.VMID != "" {
			if p.VMID != vmID || !strings.HasPrefix(key.ID, "vm_"+p.VMID+"@") {
				return errLinkUnauthorized
			}
		} else {
			current := currentKeyPrincipal(st, p, now)
			if current == nil || !current.has("exec") {
				return errLinkUnauthorized
			}
			key = current.Key
			genEntry = fmt.Sprintf("%s@%d", current.Key.ID, current.KeyGen)
		}
		expAt := now.Add(ttl)
		if key.Expires != nil && key.Expires.Before(expAt) {
			expAt = *key.Expires
		}
		if p.Key.Expires != nil && p.Key.Expires.Before(expAt) {
			expAt = *p.Key.Expires
		}
		if !slices.Contains(st.PortMinters[vmID], p.Key.ID) {
			st.PortMinters[vmID] = append(st.PortMinters[vmID], p.Key.ID)
		}
		if genEntry != "" && !slices.Contains(st.PortMinterGens[vmID], genEntry) {
			st.PortMinterGens[vmID] = append(st.PortMinterGens[vmID], genEntry)
		}
		if desktop {
			st.DesktopVMs[vmID] = true
		}
		gen, exp = st.RouteGens[vmID], expAt.Unix()
		return nil
	})
	return
}

func hasLinksOf(st *State, gone func(keyID string) bool) bool {
	for _, keys := range st.PortMinters {
		if slices.ContainsFunc(keys, gone) {
			return true
		}
	}
	for _, gens := range st.PortMinterGens {
		for _, e := range gens {
			id, _, _ := strings.Cut(e, "@")
			if gone(e) || gone(id) {
				return true
			}
		}
	}
	return false
}

// revokeLinksOf bumps the route generation of every VM whose private port (or desktop)
// links one of gone minted (inside Store.Update). The gateway then refuses
// them; signed file URLs check their key at use (keyAlive) instead.
func revokeLinksOf(st *State, gone func(keyID string) bool) {
	vms := map[string]bool{}
	for vm := range st.PortMinters {
		vms[vm] = true
	}
	for vm := range st.PortMinterGens {
		vms[vm] = true
	}
	for vm := range vms {
		keys := st.PortMinters[vm]
		kept := slices.DeleteFunc(slices.Clone(keys), gone)
		gens := st.PortMinterGens[vm]
		keptGens := slices.DeleteFunc(slices.Clone(gens), func(e string) bool {
			id, _, _ := strings.Cut(e, "@")
			return gone(e) || gone(id)
		})
		if len(kept) == len(keys) && len(keptGens) == len(gens) {
			continue
		}
		st.RouteGens[vm]++
		if len(kept) == 0 {
			delete(st.PortMinters, vm)
		} else {
			st.PortMinters[vm] = kept
		}
		if len(keptGens) == 0 {
			delete(st.PortMinterGens, vm)
		} else {
			st.PortMinterGens[vm] = keptGens
		}
	}
}

func portMinterGenAlive(st *State, entry string, now time.Time) (string, bool) {
	keyID, genStr, ok := strings.Cut(entry, "@")
	gen, err := strconv.ParseInt(genStr, 10, 64)
	if !ok || err != nil || keyID == "" {
		return keyID, false
	}
	k := st.Keys[keyID]
	if k == nil {
		return keyID, false
	}
	return keyID, keyAliveGenIn(st, keyID, gen, k.TeamID, "exec", now)
}

func barePortMinterAlive(st *State, keyID string, now time.Time) bool {
	if rest, ok := strings.CutPrefix(keyID, "vm_"); ok {
		vmPart, _, _ := strings.Cut(rest, "@")
		tok := st.VMTokens[vmPart]
		return tok != nil && keyAliveIn(st, keyID, tok.TeamID, "exec", now)
	}
	k := st.Keys[keyID]
	return k != nil && keyAliveGenIn(st, keyID, 0, k.TeamID, "exec", now)
}

func vmHasDeadPortMinters(st *State, vm string, now time.Time) bool {
	hadGen := map[string]bool{}
	for _, entry := range st.PortMinterGens[vm] {
		keyID, alive := portMinterGenAlive(st, entry, now)
		if keyID != "" {
			hadGen[keyID] = true
		}
		if !alive {
			return true
		}
	}
	for _, keyID := range st.PortMinters[vm] {
		if !strings.HasPrefix(keyID, "vm_") && hadGen[keyID] {
			continue
		}
		if !barePortMinterAlive(st, keyID, now) {
			return true
		}
	}
	return false
}

func hasDeadPortMinters(st *State, now time.Time) bool {
	for vm := range st.PortMinterGens {
		if vmHasDeadPortMinters(st, vm, now) {
			return true
		}
	}
	for vm := range st.PortMinters {
		if vmHasDeadPortMinters(st, vm, now) {
			return true
		}
	}
	return false
}

func bumpedRouteGensSnapshot(st *State, now time.Time) map[string]int {
	out := make(map[string]int, len(st.RouteGens)+len(st.PortMinters))
	for k, v := range st.RouteGens {
		out[k] = v
	}
	vms := map[string]bool{}
	for vm := range st.PortMinters {
		vms[vm] = true
	}
	for vm := range st.PortMinterGens {
		vms[vm] = true
	}
	for vm := range vms {
		if vmHasDeadPortMinters(st, vm, now) {
			out[vm]++
		}
	}
	return out
}

func revokeDeadPortMinters(st *State, now time.Time) {
	vms := map[string]bool{}
	for vm := range st.PortMinters {
		vms[vm] = true
	}
	for vm := range st.PortMinterGens {
		vms[vm] = true
	}
	for vm := range vms {
		hadGen := map[string]bool{}
		liveGen := map[string]bool{}
		var keptGens []string
		dead := false
		for _, entry := range st.PortMinterGens[vm] {
			keyID, alive := portMinterGenAlive(st, entry, now)
			if keyID != "" {
				hadGen[keyID] = true
			}
			if alive {
				keptGens = append(keptGens, entry)
				liveGen[keyID] = true
			} else {
				dead = true
			}
		}
		var keptKeys []string
		for _, keyID := range st.PortMinters[vm] {
			if !strings.HasPrefix(keyID, "vm_") && hadGen[keyID] {
				if liveGen[keyID] {
					keptKeys = append(keptKeys, keyID)
				} else {
					dead = true
				}
				continue
			}
			if barePortMinterAlive(st, keyID, now) {
				keptKeys = append(keptKeys, keyID)
			} else {
				dead = true
			}
		}
		if !dead {
			continue
		}
		st.RouteGens[vm]++
		if len(keptGens) == 0 {
			delete(st.PortMinterGens, vm)
		} else {
			st.PortMinterGens[vm] = keptGens
		}
		if len(keptKeys) == 0 {
			delete(st.PortMinters, vm)
		} else {
			st.PortMinters[vm] = keptKeys
		}
	}
}

// keyAlive says whether the key that minted a signed URL still stands: it
// exists in team, has not expired, its user is still a member, and (scope
// != "") it still holds scope. A generation minter stands only in its signing
// epoch and generation; durable vm_<id> grants stand until the VM is retired.
func (s *server) keyAlive(keyID, team, scope string, now time.Time) bool {
	return keyAliveIn(s.store.Snapshot(), keyID, team, scope, now)
}

func keyAliveGenIn(st *State, keyID string, keyGen int64, team, scope string, now time.Time) bool {
	if strings.HasPrefix(keyID, "vm_") {
		return keyAliveIn(st, keyID, team, scope, now)
	}
	return keyAliveIn(st, keyID, team, scope, now) && keyGenValid(st.Keys[keyID], keyGen, now)
}

func keyAliveIn(st *State, keyID, team, scope string, now time.Time) bool {
	if rest, ok := strings.CutPrefix(keyID, "vm_"); ok {
		vm, gen, hasGen := strings.Cut(rest, "@")
		t := st.VMTokens[vm]
		if t == nil {
			return false
		}
		expected := strconv.Itoa(t.Gen)
		if st.CopyCredentialsV1 {
			expected = "copy-v1." + expected
		}
		return !t.Retired && !t.NoSelfAPI && st.Teams[team] != nil && t.TeamID == team && (!hasGen || gen == expected) &&
			(scope == "" || slices.Contains(selfScopes, scope))
	}
	k, t := st.Keys[keyID], st.Teams[team]
	if k == nil || t == nil || k.TeamID != team || (k.Expires != nil && !now.Before(*k.Expires)) {
		return false
	}
	role := t.role(k.UserID)
	return role != "" && (scope == "" || slices.Contains(effectiveScopes(k.Scopes, role), scope))
}
