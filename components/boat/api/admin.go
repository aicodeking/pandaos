package main

import (
	"errors"
	"log"
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
}

func viewKey(k *APIKey, token string) keyView {
	sc := k.Scopes
	if sc == nil {
		sc = []string{}
	}
	return keyView{ID: k.ID, UserID: k.UserID, TeamID: k.TeamID, Name: k.Name, Scopes: sc, Created: k.Created, Token: token, Rotated: k.Rotated, PrevUntil: k.PrevUntil}
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

func (s *server) usage(w http.ResponseWriter, r *http.Request, p *principal) {
	// boatd counts from ?since= up to now; `from` is accepted as an alias.
	q := url.Values{"team": {p.Team.ID}}
	for _, k := range []string{"since", "from"} {
		if v := r.URL.Query().Get(k); v != "" {
			if _, err := time.Parse(time.RFC3339, v); err != nil {
				writeErr(w, r, 400, "invalid_request", k+" must be RFC 3339")
				return
			}
			q.Set("since", v)
		}
	}
	var out any
	if err := s.boatd.call(r.Context(), "GET", "/v1/usage?"+q.Encode(), nil, &out); err != nil {
		writeUpstream(w, r, err)
		return
	}
	writeJSON(w, 200, out)
}

// ---- teams ----

type teamView struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Role    string    `json:"role"`
	Members []Member  `json:"members,omitempty"`
	Created time.Time `json:"created"`
}

func (s *server) listTeams(w http.ResponseWriter, r *http.Request, p *principal) {
	out := []teamView{}
	for _, t := range s.store.Snapshot().Teams {
		if role := t.role(p.User.ID); role != "" {
			out = append(out, teamView{ID: t.ID, Name: t.Name, Role: role, Created: t.Created})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	writeJSON(w, 200, map[string]any{"teams": out})
}

func (s *server) getTeam(w http.ResponseWriter, r *http.Request, p *principal) {
	t := s.store.Snapshot().Teams[r.PathValue("id")]
	if t == nil || t.role(p.User.ID) == "" {
		writeErr(w, r, 404, "not_found", "team not found")
		return
	}
	writeJSON(w, 200, teamView{ID: t.ID, Name: t.Name, Role: t.role(p.User.ID), Members: t.Members, Created: t.Created})
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
	err := s.store.Update(func(st *State) error {
		ownerID := p.User.ID
		if body.Owner != "" {
			u := &User{ID: "usr_" + randHex(6), Name: body.Owner, Created: now}
			st.Users[u.ID] = u
			ownerID = u.ID
			k, tok := mintKey(s.keys, st, u.ID, t.ID, "initial", nil)
			out["owner"] = map[string]any{"user": u, "key": viewKey(k, tok)}
		}
		t.Members = []Member{{UserID: ownerID, Role: "owner"}}
		st.Teams[t.ID] = t
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	role := "owner"
	if body.Owner != "" {
		role = ""
	}
	out["id"], out["name"], out["role"], out["members"], out["created"] = t.ID, t.Name, role, t.Members, t.Created
	writeJSON(w, 201, out)
}

// deleteTeam is operator-only and refuses while the team still has VMs.
func (s *server) deleteTeam(w http.ResponseWriter, r *http.Request, p *principal) {
	id := r.PathValue("id")
	if !p.Operator {
		writeErr(w, r, 403, "forbidden", "only the instance operator can delete teams")
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
	if err := s.boatd.call(r.Context(), "GET", "/v1/vms?team="+url.QueryEscape(id), nil, &list); err != nil {
		writeUpstream(w, r, err)
		return
	}
	for _, vm := range list.VMs {
		if str(vm, "team") == id {
			writeErr(w, r, 409, "conflict", "team still has VMs; delete them first")
			return
		}
	}
	// Imported snapshots belong to no VM, so nothing else would ever delete them.
	if err := s.deleteSnapshotsOf(r, id); err != nil {
		writeUpstream(w, r, err)
		return
	}
	err := s.store.Update(func(st *State) error {
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
				delete(st.AIVMs, k)
			}
		}
		for k, sh := range st.SnapShares {
			if sh.TeamID == id || sh.Grantee == id {
				delete(st.SnapShares, k)
			}
		}
		delete(st.AIProxyOff, id)
		delete(st.Telemetry, id)
		pruneOrphanUsers(st)
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
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
	if err := s.boatd.call(r.Context(), "GET", "/v1/snapshots?team="+url.QueryEscape(team), nil, &list); err != nil {
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

// teamAdmin checks the caller is owner/admin of the team in the path.
func (s *server) teamAdmin(w http.ResponseWriter, r *http.Request, p *principal) (*Team, string, bool) {
	t := s.store.Snapshot().Teams[r.PathValue("id")]
	role := ""
	if t != nil {
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
	t, role, ok := s.teamAdmin(w, r, p)
	if !ok {
		return
	}
	if body.Role == "owner" && role != "owner" {
		writeErr(w, r, 403, "forbidden", "only owners can add owners")
		return
	}
	u := &User{ID: "usr_" + randHex(6), Name: body.Name, Created: time.Now().UTC()}
	var kv keyView
	err := s.store.Update(func(st *State) error {
		team := st.Teams[t.ID]
		if team == nil {
			return errConflict
		}
		if !underLimit(teamKeyCount(st, t.ID), s.cfg.MaxKeysPerTeam) {
			return errLimit
		}
		st.Users[u.ID] = u
		team.Members = append(slices.Clone(team.Members), Member{UserID: u.ID, Role: body.Role})
		k, tok := mintKey(s.keys, st, u.ID, t.ID, "initial", nil)
		kv = viewKey(k, tok)
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
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
		team := st.Teams[t.ID]
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
		for id, k := range st.Keys {
			if k.UserID == uid && k.TeamID == t.ID {
				delete(st.Keys, id)
			}
		}
		pruneOrphanUsers(st)
		return nil
	})
	if err != nil {
		writeErr(w, r, 409, "conflict", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) storeErr(w http.ResponseWriter, r *http.Request, err error) {
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
		Name   string   `json:"name"`
		TeamID string   `json:"team_id"`
		Scopes []string `json:"scopes"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Name == "" {
		body.Name = "key"
	}
	if !reName.MatchString(body.Name) {
		writeErr(w, r, 400, "invalid_request", "invalid key name")
		return
	}
	if body.TeamID == "" {
		body.TeamID = p.Team.ID
	}
	t := s.store.Snapshot().Teams[body.TeamID]
	if t == nil || t.role(p.User.ID) == "" {
		writeErr(w, r, 404, "not_found", "team not found")
		return
	}
	scopes := body.Scopes
	if len(scopes) == 0 {
		scopes = p.Scopes // never broader than the minting key
	}
	if len(scopes) == 0 { // stored empty, it would mean the role's full scopes
		writeErr(w, r, 403, "forbidden", "this key has no scopes to grant")
		return
	}
	allowed := effectiveScopes(nil, t.role(p.User.ID))
	for _, sc := range scopes {
		if !slices.Contains(allScopes, sc) {
			writeErr(w, r, 400, "invalid_request", "unknown scope "+sc)
			return
		}
		if !p.has(sc) || !slices.Contains(allowed, sc) {
			writeErr(w, r, 403, "forbidden", "cannot grant scope "+sc)
			return
		}
	}
	var kv keyView
	err := s.store.Update(func(st *State) error {
		if !underLimit(teamKeyCount(st, t.ID), s.cfg.MaxKeysPerTeam) {
			return errLimit
		}
		k, tok := mintKey(s.keys, st, p.User.ID, t.ID, body.Name, slices.Clone(scopes))
		kv = viewKey(k, tok)
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
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
	st := s.store.Snapshot()
	if !canManageKey(st, p, st.Keys[id]) {
		writeErr(w, r, 404, "not_found", "key not found")
		return
	}
	secret := randHex(32)
	var kv keyView
	err := s.store.Update(func(st *State) error {
		cur := st.Keys[id]
		if cur == nil {
			return errConflict
		}
		now := time.Now().UTC()
		cur.Rotated = &now
		cur.PrevSalt, cur.PrevHash, cur.PrevUntil = "", "", nil
		if overlap > 0 {
			until := now.Add(overlap)
			cur.PrevSalt, cur.PrevHash, cur.PrevUntil = cur.Salt, cur.Hash, &until
		}
		cur.Salt = randHex(16)
		cur.Hash = s.keys.HashSecret(cur.Salt, secret)
		kv = viewKey(cur, tokenPrefix+id+"_"+secret)
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
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
	if err := s.store.Update(func(st *State) error { delete(st.Keys, id); return nil }); err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- secrets ----

func (s *server) listSecrets(w http.ResponseWriter, r *http.Request, p *principal) {
	out := []map[string]any{}
	for _, sec := range s.store.Snapshot().Secrets {
		if sec.TeamID == p.Team.ID {
			out = append(out, map[string]any{"name": sec.Name, "updated": sec.Updated})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	writeJSON(w, 200, map[string]any{"secrets": out})
}

func (s *server) putSecret(w http.ResponseWriter, r *http.Request, p *principal) {
	name := r.PathValue("name")
	var body struct {
		Value *string `json:"value"`
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
	err = s.store.Update(func(st *State) error {
		k := secretKey(p.Team.ID, name)
		if !secretFits(st, p.Team.ID, k, len(sealed)) {
			return errLimit
		}
		st.Secrets[k] = &Secret{TeamID: p.Team.ID, Name: name, Sealed: sealed, Updated: now}
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	out := map[string]any{"name": name, "updated": now}
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
	if err := s.store.Update(func(st *State) error { delete(st.Secrets, k); return nil }); err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
