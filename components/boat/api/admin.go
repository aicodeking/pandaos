package main

import (
	"errors"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

var (
	reName       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.@-]{0,63}$`)
	reSecretName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	errConflict  = errors.New("conflict")
	errLimit     = errors.New("team limit reached")
)

// teamKeyCount / teamHookCount count objects for per-team limits.
func teamKeyCount(st *State, team string) int {
	n := 0
	for _, k := range st.Keys {
		if k.TeamID == team {
			n++
		}
	}
	return n
}

func teamHookCount(st *State, team string) int {
	n := 0
	for _, h := range st.Webhooks {
		if h.TeamID == team {
			n++
		}
	}
	return n
}

func underLimit(n, max int) bool { return max <= 0 || n < max }

const maxSecretBytes = 64 << 10

func roleRank(role string) int {
	switch role {
	case "owner":
		return 3
	case "admin":
		return 2
	case "member":
		return 1
	}
	return 0
}

// canManageKey decides who may see, rotate or delete a key. Rotation returns a
// working token for that key, so nobody may touch a key that can do more than
// the caller: a key's user alone does not decide it (a vms:read key belongs to
// the same user as that user's full key). Another user's key also needs the
// admin scope and no higher role, and the operator's only the operator.
func canManageKey(st *State, p *principal, k *APIKey) bool {
	if k == nil || k.TeamID != p.Team.ID {
		return false
	}
	role := p.Team.role(k.UserID)
	for _, sc := range effectiveScopes(k.Scopes, role) {
		if !p.has(sc) {
			return false
		}
	}
	if k.UserID == p.User.ID {
		return true
	}
	if u := st.Users[k.UserID]; u != nil && u.Operator && !p.Operator {
		return false
	}
	return p.has("admin") && roleRank(role) <= roleRank(p.Role)
}

// rotateNote: secrets are copied into VMs (env file, process env, disk),
// so a new value reaches only VMs created or started afterwards.
const rotateNote = "new value applies to VMs created or started from now on; running VMs keep the old value " +
	"until restarted, and existing disks, forks and snapshots still contain it. Revoke the old value at its issuer."

type keyView struct {
	ID      string     `json:"id"`
	UserID  string     `json:"user_id"`
	TeamID  string     `json:"team_id"`
	Name    string     `json:"name"`
	Scopes  []string   `json:"scopes"`
	Created time.Time  `json:"created"`
	Token   string     `json:"token,omitempty"` // only on creation and rotation
	Rotated *time.Time `json:"rotated,omitempty"`
	// PrevUntil: the previous secret keeps working until then.
	PrevUntil *time.Time `json:"prev_until,omitempty"`
	Expires   *time.Time `json:"expires,omitempty"`
}

func viewKey(k *APIKey, token string) keyView {
	sc := k.Scopes
	if sc == nil {
		sc = []string{}
	}
	return keyView{ID: k.ID, UserID: k.UserID, TeamID: k.TeamID, Name: k.Name, Scopes: sc, Created: k.Created, Token: token, Rotated: k.Rotated, PrevUntil: k.PrevUntil, Expires: k.Expires}
}

// mintKey adds a key to st and returns it with its one-time token.
func mintKey(kr *Keyring, st *State, userID, teamID, name string, scopes []string) (*APIKey, string) {
	id, secret, token := NewToken()
	salt := randHex(16)
	k := &APIKey{ID: id, UserID: userID, TeamID: teamID, Name: name, Scopes: scopes,
		Salt: salt, Hash: kr.HashSecret(salt, secret), Created: time.Now().UTC()}
	st.Keys[id] = k
	return k, token
}

// childExpiry is the expiry of a key minted by parent with a requested TTL
// (0 = never): never later than the parent's own expiry. ok is false when the
// parent expires and no TTL was requested, so a short-lived key cannot mint an
// immortal one.
func childExpiry(parent *APIKey, ttlS int64, now time.Time) (exp *time.Time, ok bool) {
	if ttlS > 0 {
		e := now.Add(time.Duration(ttlS) * time.Second)
		exp = &e
	}
	if parent == nil || parent.Expires == nil {
		return exp, true
	}
	if exp == nil {
		return nil, false
	}
	if parent.Expires.Before(*exp) {
		e := *parent.Expires
		exp = &e
	}
	return exp, true
}

// inheritedExpiry: keys minted on someone else's behalf (a new member's or
// team owner's first key) expire with the minting key.
func inheritedExpiry(p *principal) *time.Time {
	if p.Key == nil || p.Key.Expires == nil {
		return nil
	}
	e := *p.Key.Expires
	return &e
}

// bootstrap creates the first owner, team and key. It fails if any user exists.
func bootstrap(store *Store, kr *Keyring, userName, teamName string) (keyView, error) {
	var out keyView
	err := store.Update(func(st *State) error {
		if len(st.Users) > 0 {
			return errors.New("already initialised: users exist")
		}
		now := time.Now().UTC()
		u := &User{ID: "usr_" + randHex(6), Name: userName, Created: now, Operator: true}
		t := &Team{ID: "team_" + randHex(6), Name: teamName, Members: []Member{{UserID: u.ID, Role: "owner"}}, Created: now}
		st.Users[u.ID], st.Teams[t.ID] = u, t
		k, tok := mintKey(kr, st, u.ID, t.ID, "bootstrap", nil)
		out = viewKey(k, tok)
		return nil
	})
	return out, err
}

func (s *server) me(w http.ResponseWriter, r *http.Request, p *principal) {
	writeJSON(w, 200, map[string]any{"user": p.User, "team": map[string]string{"id": p.Team.ID, "name": p.Team.Name},
		"role": p.Role, "scopes": p.Scopes, "key_id": p.Key.ID, "operator": p.Operator})
}

func (s *server) images(w http.ResponseWriter, r *http.Request, p *principal) {
	out := []map[string]any{}
	for i, im := range s.cfg.Images {
		out = append(out, map[string]any{"id": im, "default": i == 0})
	}
	writeJSON(w, 200, map[string]any{"images": out})
}

// parseUsageTime reads a usage window bound: RFC 3339, or a date-only
// YYYY-MM-DD taken as UTC midnight. dateOnly reports the second form.
func parseUsageTime(v string) (t time.Time, dateOnly bool, err error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, false, nil
	}
	t, err = time.Parse(time.DateOnly, v)
	return t, err == nil, err
}

func (s *server) usage(w http.ResponseWriter, r *http.Request, p *principal) {
	// boatd counts from ?since= up to now; `from` is accepted as an alias and `to`
	// closes the window.
	q := url.Values{"team": {p.Team.ID}}
	var since, to time.Time
	for _, k := range []string{"since", "from"} {
		if v := r.URL.Query().Get(k); v != "" {
			t, _, err := parseUsageTime(v)
			if err != nil {
				writeErr(w, r, 400, "invalid_request", k+" must be RFC 3339 or YYYY-MM-DD")
				return
			}
			since = t
			q.Set("since", t.UTC().Format(time.RFC3339))
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, dateOnly, err := parseUsageTime(v)
		if err != nil {
			writeErr(w, r, 400, "invalid_request", "to must be RFC 3339 or YYYY-MM-DD")
			return
		}
		if dateOnly {
			// A date-only `to` names a whole UTC day: the window ends at the next
			// midnight (exclusive), so from==to is that one day.
			t = t.AddDate(0, 0, 1)
		}
		if !since.IsZero() && !t.After(since) {
			writeErr(w, r, 400, "invalid_request", "to must be after from")
			return
		}
		to = t
	}
	var out map[string]any
	if err := s.boatd.call(r.Context(), "GET", "/v1/usage?"+q.Encode(), nil, &out); err != nil {
		writeUpstream(w, r, err)
		return
	}
	if !to.IsZero() && to.Before(time.Now()) {
		// boatd's report always runs to now. Usage is additive over time, so the
		// window [from, to] is the report since from minus the report since to.
		tail := url.Values{"team": {p.Team.ID}, "since": {to.UTC().Format(time.RFC3339)}}
		var after map[string]any
		if err := s.boatd.call(r.Context(), "GET", "/v1/usage?"+tail.Encode(), nil, &after); err != nil {
			writeUpstream(w, r, err)
			return
		}
		out = usageWindow(out, after, to)
	}
	// A sandbox in the env-error state is not billed: take those intervals out.
	writeJSON(w, 200, s.withoutEnvErrorSeconds(r.Context(), out, since, to))
}

// usageWindow returns full (usage since A until now) with after (usage since B
// until now) taken out, so it covers A..B. Neither input is modified.
func usageWindow(full, after map[string]any, until time.Time) map[string]any {
	out := make(map[string]any, len(full))
	for k, v := range full {
		out[k] = v
	}
	out["until"] = until.UTC()
	minus := func(list, tail any, key string, idOf func(map[string]any) string) []any {
		sub := map[string]float64{}
		if rows, ok := tail.([]any); ok {
			for _, row := range rows {
				if m, ok := row.(map[string]any); ok {
					f, _ := m[key].(float64)
					sub[idOf(m)] += f
				}
			}
		}
		res := []any{}
		rows, _ := list.([]any)
		for _, row := range rows {
			m, ok := row.(map[string]any)
			if !ok {
				continue
			}
			c := make(map[string]any, len(m))
			for k, v := range m {
				c[k] = v
			}
			f, _ := m[key].(float64)
			c[key] = math.Max(0, f-sub[idOf(m)])
			res = append(res, c)
		}
		return res
	}
	byID := func(m map[string]any) string { s, _ := m["id"].(string); return s }
	byTeam := func(m map[string]any) string { s, _ := m["team"].(string); return s }
	out["vms"] = minus(full["vms"], after["vms"], "vm_seconds", byID)
	out["snapshots"] = minus(full["snapshots"], after["snapshots"], "snapshot_days", byID)
	teams := minus(full["teams"], after["teams"], "vm_seconds", byTeam)
	teams = minus(teams, after["teams"], "snapshot_days", byTeam)
	for _, row := range teams {
		m := row.(map[string]any)
		sizes, _ := m["vm_seconds_by_size"].(map[string]any)
		var tail map[string]any
		afterTeams, _ := after["teams"].([]any)
		for _, t := range afterTeams {
			if tm, ok := t.(map[string]any); ok && byTeam(tm) == byTeam(m) {
				tail, _ = tm["vm_seconds_by_size"].(map[string]any)
			}
		}
		by := make(map[string]any, len(sizes))
		for size, v := range sizes {
			f, _ := v.(float64)
			g, _ := tail[size].(float64)
			by[size] = math.Max(0, f-g)
		}
		m["vm_seconds_by_size"] = by
	}
	out["teams"] = teams
	return out
}

// ---- teams ----

type teamView struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Role    string    `json:"role"`
	Members []Member  `json:"members,omitempty"`
	Created time.Time `json:"created"`
	// AcceptSharesFrom is shown to the team's own members only.
	AcceptSharesFrom []string         `json:"accept_shares_from,omitempty"`
	SelfToken        *SelfTokenPolicy `json:"self_token,omitempty"`
}

func (s *server) listTeams(w http.ResponseWriter, r *http.Request, p *principal) {
	out := []teamView{}
	canCrossTeam := p.Operator && p.has("admin")
	for _, t := range s.store.Snapshot().Teams {
		if t.ID != p.Team.ID && !canCrossTeam {
			continue
		}
		if role := t.role(p.User.ID); role != "" {
			out = append(out, teamView{ID: t.ID, Name: t.Name, Role: role, Created: t.Created})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	writeJSON(w, 200, map[string]any{"teams": out})
}

func (s *server) getTeam(w http.ResponseWriter, r *http.Request, p *principal) {
	t := s.store.Snapshot().Teams[r.PathValue("id")]
	if t == nil || (t.ID != p.Team.ID && !(p.Operator && p.has("admin"))) || t.role(p.User.ID) == "" {
		writeErr(w, r, 404, "not_found", "team not found")
		return
	}
	v := teamView{ID: t.ID, Name: t.Name, Role: t.role(p.User.ID), Created: t.Created}
	if p.has("admin") {
		v.Members = t.Members
		v.AcceptSharesFrom = t.AcceptSharesFrom
		v.SelfToken = &t.SelfToken
	}
	writeJSON(w, 200, v)
}

func currentTeamAdmin(st *State, p *principal, teamID string, now time.Time) (*Team, int, string, string) {
	current := currentKeyPrincipal(st, p, now)
	if current == nil {
		return nil, 401, "unauthorized", "missing or invalid API key"
	}
	if !current.has("admin") {
		return nil, 403, "forbidden", "team admin role required"
	}
	team := st.Teams[teamID]
	if team == nil || (team.ID != current.Team.ID && !current.Operator) || team.role(current.User.ID) == "" {
		return nil, 404, "not_found", "team not found"
	}
	role := team.role(current.User.ID)
	if team.ID == p.Team.ID && roleRank(p.Role) < roleRank(role) {
		role = p.Role
	}
	if role != "owner" && role != "admin" {
		return nil, 403, "forbidden", "team admin role required"
	}
	return team, 0, "", ""
}

// setSelfTokenPolicy: PUT /v1/teams/{id}/self-token. Lets VM tokens open
// public ports or add ssh keys on their own VM (both refused by default).
func (s *server) setSelfTokenPolicy(w http.ResponseWriter, r *http.Request, p *principal) {
	var body SelfTokenPolicy
	if !decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	errRefused := errors.New("self-token policy refused")
	status, code, message := 0, "", ""
	err := s.store.Update(func(st *State) error {
		team, n, c, m := currentTeamAdmin(st, p, id, time.Now().UTC())
		if team == nil {
			status, code, message = n, c, m
			return errRefused
		}
		nt := *team
		nt.SelfToken = body
		st.Teams[team.ID] = &nt
		return nil
	})
	if err != nil {
		if errors.Is(err, errRefused) {
			writeErr(w, r, status, code, message)
		} else {
			s.storeErr(w, r, err)
		}
		return
	}
	writeJSON(w, 200, body)
}

const maxShareAllow = 64

var reTeamID = regexp.MustCompile(`^team_[0-9a-f]{1,32}$`)

// setShareAllowlist: the recipient's consent for snapshot grants (snapshots.go).
func (s *server) setShareAllowlist(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Teams []string `json:"teams"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Teams) > maxShareAllow {
		writeErr(w, r, 400, "invalid_request", "at most 64 teams")
		return
	}
	for _, id := range body.Teams {
		if !reTeamID.MatchString(id) {
			writeErr(w, r, 400, "invalid_request", "invalid team id "+id)
			return
		}
	}
	id := r.PathValue("id")
	allow := slices.Clone(body.Teams)
	slices.Sort(allow)
	allow = slices.Compact(allow)
	errRefused := errors.New("share allowlist refused")
	status, code, message := 0, "", ""
	err := s.store.Update(func(st *State) error {
		team, n, c, m := currentTeamAdmin(st, p, id, time.Now().UTC())
		if team == nil {
			status, code, message = n, c, m
			return errRefused
		}
		team.AcceptSharesFrom = allow
		return nil
	})
	if err != nil {
		if errors.Is(err, errRefused) {
			writeErr(w, r, status, code, message)
		} else {
			s.storeErr(w, r, err)
		}
		return
	}
	if allow == nil {
		allow = []string{}
	}
	writeJSON(w, 200, map[string]any{"teams": allow})
}

// createTeam is operator-only: tenants are onboarded by the operator. With
// "owner", a new user owns the team and their first key is returned; without
// it the operator becomes the owner.
func (s *server) createTeam(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Name  string `json:"name"`
		Owner string `json:"owner"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !p.Operator {
		writeErr(w, r, 403, "forbidden", "only the instance operator can create teams")
		return
	}
	if !reName.MatchString(body.Name) || (body.Owner != "" && !reName.MatchString(body.Owner)) {
		writeErr(w, r, 400, "invalid_request", "invalid team or owner name")
		return
	}
	now := time.Now().UTC()
	t := &Team{ID: "team_" + randHex(6), Name: body.Name, Created: now}
	out := map[string]any{}
	errRefused := errors.New("team mint refused")
	status, code, message := 0, "", ""
	refuse := func(n int, c, m string) error { status, code, message = n, c, m; return errRefused }
	err := s.store.Update(func(st *State) error {
		current := currentKeyPrincipal(st, p, time.Now().UTC())
		if current == nil {
			return refuse(401, "unauthorized", "missing or invalid API key")
		}
		if !current.Operator {
			return refuse(403, "forbidden", "only the instance operator can create teams")
		}
		if !current.has("admin") {
			return refuse(403, "forbidden", "admin scope required")
		}
		ownerID := current.User.ID
		if body.Owner != "" {
			u := &User{ID: "usr_" + randHex(6), Name: body.Owner, Created: now}
			st.Users[u.ID] = u
			ownerID = u.ID
			k, tok := mintKey(s.keys, st, u.ID, t.ID, "initial", ownerKeyScopes(current))
			k.Expires = inheritedExpiry(current)
			if p.Key.Expires != nil && (k.Expires == nil || p.Key.Expires.Before(*k.Expires)) {
				k.Expires = inheritedExpiry(p)
			}
			out["owner"] = map[string]any{"user": u, "key": viewKey(k, tok)}
		}
		t.Members = []Member{{UserID: ownerID, Role: "owner"}}
		st.Teams[t.ID] = t
		return nil
	})
	if err != nil {
		if errors.Is(err, errRefused) {
			writeErr(w, r, status, code, message)
		} else {
			s.storeErr(w, r, err)
		}
		return
	}
	role := "owner"
	if body.Owner != "" {
		role = ""
	}
	out["id"], out["name"], out["role"], out["members"], out["created"] = t.ID, t.Name, role, t.Members, t.Created
	writeJSON(w, 201, out)
}

func (s *server) checkOperatorAdmin(w http.ResponseWriter, r *http.Request, st *State, p *principal) bool {
	cur := currentKeyPrincipal(st, p, time.Now().UTC())
	if cur == nil {
		writeErr(w, r, 401, "unauthorized", "missing or invalid API key")
		return false
	}
	if !cur.Operator {
		writeErr(w, r, 403, "forbidden", "only the instance operator can delete teams")
		return false
	}
	if !cur.has("admin") {
		writeErr(w, r, 403, "forbidden", "admin scope required")
		return false
	}
	return true
}

// deleteTeam is operator-only and refuses while the team still has VMs.
func (s *server) deleteTeam(w http.ResponseWriter, r *http.Request, p *principal) {
	id := r.PathValue("id")
	if !p.Operator {
		writeErr(w, r, 403, "forbidden", "only the instance operator can delete teams")
		return
	}
	if !s.checkOperatorAdmin(w, r, s.store.Snapshot(), p) {
		return
	}
	if s.store.Snapshot().Teams[id] == nil {
		writeErr(w, r, 404, "not_found", "team not found")
		return
	}
	if id == p.Team.ID {
		writeErr(w, r, 409, "conflict", "cannot delete the team of the key in use")
		return
	}
	var list struct {
		VMs []VM `json:"vms"`
	}
	if err := s.boatd.call(r.Context(), "GET", "/v1/vms?include_pending=true&team="+url.QueryEscape(id), nil, &list); err != nil {
		writeUpstream(w, r, err)
		return
	}
	for _, vm := range list.VMs {
		if str(vm, "team") == id {
			writeErr(w, r, 409, "conflict", "team still has VMs; delete them first")
			return
		}
	}
	if !s.checkOperatorAdmin(w, r, s.store.Snapshot(), p) {
		return
	}
	// Imported snapshots belong to no VM, so nothing else would ever delete them.
	if err := s.deleteSnapshotsOf(r, id); err != nil {
		writeUpstream(w, r, err)
		return
	}
	err := s.store.Update(func(st *State) error {
		now := time.Now().UTC()
		cur := currentKeyPrincipal(st, p, now)
		if cur == nil {
			return &authError{status: 401, code: "unauthorized", msg: "missing or invalid API key"}
		}
		if !cur.Operator {
			return &authError{status: 403, code: "forbidden", msg: "only the instance operator can delete teams"}
		}
		if !cur.has("admin") {
			return &authError{status: 403, code: "forbidden", msg: "admin scope required"}
		}
		if st.Teams[id] == nil {
			return &authError{status: 404, code: "not_found", msg: "team not found"}
		}
		delete(st.Teams, id)
		for k, v := range st.Keys {
			if v.TeamID == id {
				delete(st.Keys, k)
			}
		}
		for k, v := range st.Secrets {
			if v.TeamID == id {
				delete(st.Secrets, k)
			}
		}
		for k, v := range st.Webhooks {
			if v.TeamID == id {
				delete(st.Webhooks, k)
				delete(st.Deliveries, k)
			}
		}
		for k, g := range st.SSHGrants {
			if g.TeamID == id {
				delete(st.SSHGrants, k)
			}
		}
		for k, e := range st.Environments {
			if e.TeamID == id {
				delete(st.Environments, k)
			}
		}
		for k, c := range st.AICreds {
			if c.TeamID == id {
				delete(st.AICreds, k)
			}
		}
		for k, v := range st.AIVMs {
			if v.TeamID == id {
				retireCredentials(st, k)
			}
		}
		for vm, tok := range st.VMTokens {
			if tok.TeamID == id {
				retireCredentials(st, vm)
			}
		}
		for k, sh := range st.SnapShares {
			if sh.TeamID == id || sh.Grantee == id {
				delete(st.SnapShares, k)
			}
		}
		deleteTeamIdem(st, id)
		for k, op := range st.DeletionOps {
			if str(op, "_team") == id {
				delete(st.DeletionOps, k)
			}
		}
		for k, op := range st.StopOps {
			if str(op, "_team") == id {
				delete(st.StopOps, k)
			}
		}
		revokeDeadPortMinters(st, now)
		delete(st.AIProxyOff, id)
		delete(st.Telemetry, id)
		deleteTeamAIUsage(st, id)
		pruneOrphanUsers(st)
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	s.aiMeter.dropTeam(id) // counts not flushed yet would resurrect rows of the gone team
	// An import racing the first pass finished while the team's keys still worked.
	if err := s.deleteSnapshotsOf(r, id); err != nil {
		log.Printf("%s: snapshots of deleted team %s: %v", info(r).id, id, err)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "id": id})
}

func (s *server) deleteSnapshotsOf(r *http.Request, team string) error {
	var list struct {
		Snapshots []map[string]any `json:"snapshots"`
	}
	if err := s.boatd.call(r.Context(), "GET", "/v1/snapshots?include_pending=true&team="+url.QueryEscape(team), nil, &list); err != nil {
		return err
	}
	for _, sn := range list.Snapshots {
		if str(sn, "team") != team {
			continue
		}
		if err := s.boatd.call(r.Context(), "DELETE", "/v1/snapshots/"+url.PathEscape(str(sn, "id")), nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// pruneOrphanUsers removes non-operator users that belong to no team.
func pruneOrphanUsers(st *State) {
	for id, u := range st.Users {
		if u.Operator {
			continue
		}
		member := false
		for _, t := range st.Teams {
			if t.role(id) != "" {
				member = true
				break
			}
		}
		if !member {
			delete(st.Users, id)
		}
	}
}

// ownerKeyScopes: a new team owner's first key is no wider than the operator's
// minting key (nil = full scopes, only when the minting key has them all).
func ownerKeyScopes(p *principal) []string {
	if !slices.ContainsFunc(allScopes, func(sc string) bool { return !p.has(sc) }) {
		return nil
	}
	return slices.Clone(p.Scopes)
}

// teamAdmin checks the caller is owner/admin of the team in the path, through
// a key of that team: only the instance operator reaches other teams.
func (s *server) teamAdmin(w http.ResponseWriter, r *http.Request, p *principal) (*Team, string, bool) {
	t := s.store.Snapshot().Teams[r.PathValue("id")]
	role := ""
	if t != nil && (t.ID == p.Team.ID || p.Operator) {
		role = t.role(p.User.ID)
	}
	if role == "" {
		writeErr(w, r, 404, "not_found", "team not found")
		return nil, "", false
	}
	if role != "owner" && role != "admin" {
		writeErr(w, r, 403, "forbidden", "team admin role required")
		return nil, "", false
	}
	return t, role, true
}

// addMember creates a new user in the team and returns their first key.
func (s *server) addMember(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Role == "" {
		body.Role = "member"
	}
	if !reName.MatchString(body.Name) || !validRole(body.Role) {
		writeErr(w, r, 400, "invalid_request", "name and role (owner|admin|member) required")
		return
	}
	u := &User{ID: "usr_" + randHex(6), Name: body.Name, Created: time.Now().UTC()}
	var kv keyView
	errRefused := errors.New("member mint refused")
	status, code, message := 0, "", ""
	refuse := func(n int, c, m string) error { status, code, message = n, c, m; return errRefused }
	err := s.store.Update(func(st *State) error {
		current := currentKeyPrincipal(st, p, time.Now().UTC())
		if current == nil {
			return refuse(401, "unauthorized", "missing or invalid API key")
		}
		if !current.has("admin") {
			return refuse(403, "forbidden", "team admin role required")
		}
		team := st.Teams[r.PathValue("id")]
		if team == nil || (team.ID != current.Team.ID && !current.Operator) || team.role(current.User.ID) == "" {
			return refuse(404, "not_found", "team not found")
		}
		role := team.role(current.User.ID)
		if team.ID == p.Team.ID && roleRank(p.Role) < roleRank(role) {
			role = p.Role
		}
		if role != "owner" && role != "admin" {
			return refuse(403, "forbidden", "team admin role required")
		}
		if body.Role == "owner" && role != "owner" {
			return refuse(403, "forbidden", "only owners can add owners")
		}
		// Empty stored scopes mean the role's full authority, not no authority.
		scopes := effectiveScopes(current.Scopes, body.Role)
		if len(scopes) == 0 {
			return refuse(403, "forbidden", "this key has no scopes to grant for role "+body.Role)
		}
		if !slices.ContainsFunc(roleScopes(body.Role), func(sc string) bool { return !current.has(sc) }) {
			scopes = nil
		}
		if !underLimit(teamKeyCount(st, team.ID), s.cfg.MaxKeysPerTeam) {
			return errLimit
		}
		st.Users[u.ID] = u
		team.Members = append(slices.Clone(team.Members), Member{UserID: u.ID, Role: body.Role})
		k, tok := mintKey(s.keys, st, u.ID, team.ID, "initial", scopes)
		k.Expires = inheritedExpiry(current)
		if p.Key.Expires != nil && (k.Expires == nil || p.Key.Expires.Before(*k.Expires)) {
			k.Expires = inheritedExpiry(p)
		}
		kv = viewKey(k, tok)
		return nil
	})
	if err != nil {
		if errors.Is(err, errRefused) {
			writeErr(w, r, status, code, message)
		} else {
			s.storeErr(w, r, err)
		}
		return
	}
	writeJSON(w, 201, map[string]any{"user": u, "role": body.Role, "key": kv})
}

func (s *server) removeMember(w http.ResponseWriter, r *http.Request, p *principal) {
	t, role, ok := s.teamAdmin(w, r, p)
	if !ok {
		return
	}
	uid := r.PathValue("user")
	target := t.role(uid)
	if target == "" {
		writeErr(w, r, 404, "not_found", "member not found")
		return
	}
	if target == "owner" && role != "owner" {
		writeErr(w, r, 403, "forbidden", "only owners can remove owners")
		return
	}
	err := s.store.Update(func(st *State) error {
		now := time.Now().UTC()
		team, n, c, m := currentTeamAdmin(st, p, t.ID, now)
		if team == nil {
			return &authError{status: n, code: c, msg: m}
		}
		curRole := team.role(p.User.ID)
		if team.ID == p.Team.ID && roleRank(p.Role) < roleRank(curRole) {
			curRole = p.Role
		}
		curTarget := team.role(uid)
		if curTarget == "" {
			return &authError{status: 404, code: "not_found", msg: "member not found"}
		}
		if curTarget == "owner" && curRole != "owner" {
			return &authError{status: 403, code: "forbidden", msg: "only owners can remove owners"}
		}
		var kept []Member
		owners := 0
		for _, m := range team.Members {
			if m.UserID != uid {
				kept = append(kept, m)
				if m.Role == "owner" {
					owners++
				}
			}
		}
		if owners == 0 {
			return errors.New("cannot remove the last owner")
		}
		team.Members = kept
		gone := map[string]bool{}
		for id, k := range st.Keys {
			if k.UserID == uid && k.TeamID == t.ID {
				delete(st.Keys, id)
				gone[id] = true
			}
		}
		revokeLinksOf(st, func(k string) bool { return gone[k] })
		revokeDeadPortMinters(st, now)
		pruneOrphanUsers(st)
		return nil
	})
	if err != nil {
		var ae *authError
		if errors.As(err, &ae) {
			writeErr(w, r, ae.status, ae.code, ae.msg)
			return
		}
		writeErr(w, r, 409, "conflict", err.Error())
		return
	}
	s.syncJumpKeysForTeam(r.Context(), t.ID) // drops the member's ssh grants
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) storeErr(w http.ResponseWriter, r *http.Request, err error) {
	var ae *authError
	if errors.As(err, &ae) {
		writeErr(w, r, ae.status, ae.code, ae.msg)
		return
	}
	if errors.Is(err, errLimit) {
		writeErr(w, r, 403, "limit_reached", "team limit reached (keys, webhooks, secrets, shares, environments or ssh keys); delete unused ones or ask the operator")
		return
	}
	if errors.Is(err, errConflict) {
		writeErr(w, r, 409, "conflict", "concurrent modification, retry")
		return
	}
	writeErr(w, r, 500, "store_error", "could not persist change")
}

// ---- keys ----

func (s *server) listKeys(w http.ResponseWriter, r *http.Request, p *principal) {
	out := []keyView{}
	st := s.store.Snapshot()
	for _, k := range st.Keys {
		if canManageKey(st, p, k) {
			out = append(out, viewKey(k, ""))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	writeJSON(w, 200, map[string]any{"keys": out})
}

// createKey mints a key for the caller. Scopes must be a subset of the
// caller's own effective scopes and of their role in the target team.
func (s *server) createKey(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Name       string   `json:"name"`
		TeamID     string   `json:"team_id"`
		Scopes     []string `json:"scopes"`
		ExpiresInS int64    `json:"expires_in_s"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Name == "" {
		body.Name = "key"
	}
	if body.ExpiresInS < 0 || body.ExpiresInS > maxKeyTTLS {
		writeErr(w, r, 400, "invalid_request", "expires_in_s must be 1 to 31536000 (365 days), or left out for a key that never expires")
		return
	}
	if !reName.MatchString(body.Name) {
		writeErr(w, r, 400, "invalid_request", "invalid key name")
		return
	}
	if body.TeamID == "" {
		body.TeamID = p.Team.ID
	}
	var kv keyView
	// Refusals are returned from the callback, never written before persistence.
	errRefused := errors.New("key mint refused")
	status, code, message := 0, "", ""
	refuse := func(n int, c, m string) error { status, code, message = n, c, m; return errRefused }
	err := s.store.Update(func(st *State) error {
		now := time.Now().UTC()
		current := currentKeyPrincipal(st, p, now)
		if current == nil {
			return refuse(401, "unauthorized", "missing or invalid API key")
		}
		t := st.Teams[body.TeamID]
		if t == nil || t.role(current.User.ID) == "" {
			return refuse(404, "not_found", "team not found")
		}
		if t.ID != current.Team.ID && !current.Operator {
			return refuse(403, "forbidden", "this key can only mint keys for its own team")
		}
		expires, ok := childExpiry(current.Key, body.ExpiresInS, now)
		if !ok {
			return refuse(400, "invalid_request", "this key expires, so the keys it mints must expire too: set expires_in_s")
		}
		// An in-flight request cannot outlive its original parent expiry either.
		if p.Key.Expires != nil && (expires == nil || p.Key.Expires.Before(*expires)) {
			original, ok := childExpiry(p.Key, body.ExpiresInS, now)
			if !ok {
				return refuse(400, "invalid_request", "this key expires, so the keys it mints must expire too: set expires_in_s")
			}
			expires = original
		}
		scopes := body.Scopes
		if len(scopes) == 0 {
			scopes = current.Scopes
		}
		if len(scopes) == 0 {
			return refuse(403, "forbidden", "this key has no scopes to grant")
		}
		allowed := effectiveScopes(nil, t.role(current.User.ID))
		for _, sc := range scopes {
			if !slices.Contains(allScopes, sc) {
				return refuse(400, "invalid_request", "unknown scope "+sc)
			}
			if !current.has(sc) || !slices.Contains(allowed, sc) {
				return refuse(403, "forbidden", "cannot grant scope "+sc)
			}
		}
		if !underLimit(teamKeyCount(st, t.ID), s.cfg.MaxKeysPerTeam) {
			return errLimit
		}
		k, tok := mintKey(s.keys, st, current.User.ID, t.ID, body.Name, slices.Clone(scopes))
		k.Expires = expires
		kv = viewKey(k, tok)
		return nil
	})
	if err != nil {
		if errors.Is(err, errRefused) {
			writeErr(w, r, status, code, message)
		} else {
			s.storeErr(w, r, err)
		}
		return
	}
	writeJSON(w, 201, kv)
}

// rotateKey replaces a key's secret, keeping its id, scopes and team. The old
// secret works for overlap_s more (default 24h, 0 = revoke at once).
func (s *server) rotateKey(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		OverlapS *int64 `json:"overlap_s"`
	}
	if !decode(w, r, &body) {
		return
	}
	overlap, ok := overlapFrom(body.OverlapS)
	if !ok {
		writeErr(w, r, 400, "invalid_request", "overlap_s must be 0..2592000")
		return
	}
	id := r.PathValue("id")
	secret := randHex(32)
	var kv keyView
	errRefused := errors.New("key rotation refused")
	status := 0
	displacedPrev := false
	targetTeam := ""
	err := s.store.Update(func(st *State) error {
		now := time.Now().UTC()
		current := currentKeyPrincipal(st, p, now)
		if current == nil {
			status = 401
			return errRefused
		}
		if roleRank(p.Role) < roleRank(current.Role) {
			current.Role = p.Role
		}
		cur := st.Keys[id]
		if !canManageKey(st, current, cur) {
			status = 404
			return errRefused
		}
		targetTeam = cur.TeamID
		displacedPrev = cur.PrevHash != ""
		cur.Gen++
		cur.Rotated = &now
		cur.PrevSalt, cur.PrevHash, cur.PrevUntil = "", "", nil
		if overlap > 0 {
			until := now.Add(overlap)
			cur.PrevSalt, cur.PrevHash, cur.PrevUntil = cur.Salt, cur.Hash, &until
		}
		cur.Salt = randHex(16)
		cur.Hash = s.keys.HashSecret(cur.Salt, secret)
		revokeDeadPortMinters(st, now)
		kv = viewKey(cur, tokenPrefix+id+"_"+secret)
		return nil
	})
	if err != nil {
		if errors.Is(err, errRefused) && status == 401 {
			writeErr(w, r, 401, "unauthorized", "missing or invalid API key")
		} else if errors.Is(err, errRefused) {
			writeErr(w, r, 404, "not_found", "key not found")
		} else {
			s.storeErr(w, r, err)
		}
		return
	}
	if overlap == 0 || displacedPrev {
		s.syncJumpKeysForTeam(r.Context(), targetTeam)
	}
	writeJSON(w, 200, kv)
}

func (s *server) countKeyUse(id string) {
	v, _ := s.keyUse.LoadOrStore(id, &keyUsage{})
	u := v.(*keyUsage)
	u.requests.Add(1)
	u.last.Store(time.Now().Unix())
}

// keyUsageView reports requests since this process started and the last use.
func (s *server) keyUsageView(w http.ResponseWriter, r *http.Request, p *principal) {
	id := r.PathValue("id")
	st := s.store.Snapshot()
	if !canManageKey(st, p, st.Keys[id]) {
		writeErr(w, r, 404, "not_found", "key not found")
		return
	}
	out := map[string]any{"id": id, "requests": int64(0)}
	if v, ok := s.keyUse.Load(id); ok {
		u := v.(*keyUsage)
		out["requests"] = u.requests.Load()
		out["last_used"] = time.Unix(u.last.Load(), 0).UTC()
	}
	writeJSON(w, 200, out)
}

func (s *server) deleteKey(w http.ResponseWriter, r *http.Request, p *principal) {
	id := r.PathValue("id")
	st := s.store.Snapshot()
	if !canManageKey(st, p, st.Keys[id]) {
		writeErr(w, r, 404, "not_found", "key not found")
		return
	}
	targetTeam := ""
	if err := s.store.Update(func(st *State) error {
		now := time.Now().UTC()
		current := currentKeyPrincipal(st, p, now)
		if current == nil {
			return &authError{status: 401, code: "unauthorized", msg: "missing or invalid API key"}
		}
		if roleRank(p.Role) < roleRank(current.Role) {
			current.Role = p.Role
		}
		if !canManageKey(st, current, st.Keys[id]) {
			return &authError{status: 404, code: "not_found", msg: "key not found"}
		}
		targetTeam = st.Keys[id].TeamID
		delete(st.Keys, id)
		revokeLinksOf(st, func(k string) bool { return k == id })
		revokeDeadPortMinters(st, now)
		return nil
	}); err != nil {
		s.storeErr(w, r, err)
		return
	}
	s.syncJumpKeysForTeam(r.Context(), targetTeam) // drops the key's ssh grants
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- secrets ----

func (s *server) listSecrets(w http.ResponseWriter, r *http.Request, p *principal) {
	out := []map[string]any{}
	for _, sec := range s.store.Snapshot().Secrets {
		if sec.TeamID == p.Team.ID {
			out = append(out, map[string]any{"name": sec.Name, "updated": sec.Updated, "inject": sec.Inject})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	writeJSON(w, 200, map[string]any{"secrets": out})
}

func (s *server) putSecret(w http.ResponseWriter, r *http.Request, p *principal) {
	name := r.PathValue("name")
	var body struct {
		Value  *string `json:"value"`
		Inject *bool   `json:"inject"` // default-injected; absent keeps the current mark
	}
	if !decode(w, r, &body) {
		return
	}
	if !reSecretName.MatchString(name) {
		writeErr(w, r, 400, "invalid_request", "secret names are UPPER_SNAKE_CASE env var names")
		return
	}
	if body.Value == nil || len(*body.Value) > maxSecretBytes || strings.ContainsRune(*body.Value, 0) {
		writeErr(w, r, 400, "invalid_request", "value is required (max 64 KiB, no NUL)")
		return
	}
	sealed, err := s.keys.Seal([]byte(*body.Value), secretKey(p.Team.ID, name))
	if err != nil {
		writeErr(w, r, 500, "crypto_error", "could not encrypt")
		return
	}
	now := time.Now().UTC()
	existed := s.store.Snapshot().Secrets[secretKey(p.Team.ID, name)] != nil
	inject := false
	err = s.store.Update(func(st *State) error {
		if _, err := requirePrincipalIn(st, p, "admin", time.Now().UTC()); err != nil {
			return err
		}
		k := secretKey(p.Team.ID, name)
		if !secretFits(st, p.Team.ID, k, len(sealed)) {
			return errLimit
		}
		if old := st.Secrets[k]; old != nil {
			inject = old.Inject
		}
		if body.Inject != nil {
			inject = *body.Inject
		}
		st.Secrets[k] = &Secret{TeamID: p.Team.ID, Name: name, Sealed: sealed, Updated: now, Inject: inject}
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	out := map[string]any{"name": name, "updated": now, "inject": inject}
	if existed {
		out["rotated"] = true
		out["note"] = rotateNote
	}
	writeJSON(w, 200, out)
}

// secretFits reports whether the team stays within its caps after storing size
// sealed bytes under key k; replacing a secret frees the old value.
func secretFits(st *State, team, k string, size int) bool {
	n, total := 0, size
	for key, sec := range st.Secrets {
		if sec.TeamID == team && key != k {
			n++
			total += len(sec.Sealed)
		}
	}
	return n < maxSecretsPerTeam && total <= maxSecretBytesPerTeam
}

func (s *server) deleteSecret(w http.ResponseWriter, r *http.Request, p *principal) {
	k := secretKey(p.Team.ID, r.PathValue("name"))
	if s.store.Snapshot().Secrets[k] == nil {
		writeErr(w, r, 404, "not_found", "secret not found")
		return
	}
	if err := s.store.Update(func(st *State) error {
		if _, err := requirePrincipalIn(st, p, "admin", time.Now().UTC()); err != nil {
			return err
		}
		delete(st.Secrets, k)
		return nil
	}); err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
