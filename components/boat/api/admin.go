package main

import (
	"errors"
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
)

const maxSecretBytes = 64 << 10

type keyView struct {
	ID      string    `json:"id"`
	UserID  string    `json:"user_id"`
	TeamID  string    `json:"team_id"`
	Name    string    `json:"name"`
	Scopes  []string  `json:"scopes"`
	Created time.Time `json:"created"`
	Token   string    `json:"token,omitempty"` // only on creation
}

func viewKey(k *APIKey, token string) keyView {
	sc := k.Scopes
	if sc == nil {
		sc = []string{}
	}
	return keyView{ID: k.ID, UserID: k.UserID, TeamID: k.TeamID, Name: k.Name, Scopes: sc, Created: k.Created, Token: token}
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
		u := &User{ID: "usr_" + randHex(6), Name: userName, Created: now}
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
		"role": p.Role, "scopes": p.Scopes, "key_id": p.Key.ID})
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

func (s *server) createTeam(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !reName.MatchString(body.Name) {
		writeErr(w, r, 400, "invalid_request", "invalid team name")
		return
	}
	t := &Team{ID: "team_" + randHex(6), Name: body.Name, Members: []Member{{UserID: p.User.ID, Role: "owner"}}, Created: time.Now().UTC()}
	if err := s.store.Update(func(st *State) error { st.Teams[t.ID] = t; return nil }); err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 201, teamView{ID: t.ID, Name: t.Name, Role: "owner", Members: t.Members, Created: t.Created})
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
		return nil
	})
	if err != nil {
		writeErr(w, r, 409, "conflict", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) storeErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errConflict) {
		writeErr(w, r, 409, "conflict", "concurrent modification, retry")
		return
	}
	writeErr(w, r, 500, "store_error", "could not persist change")
}

// ---- keys ----

func (s *server) listKeys(w http.ResponseWriter, r *http.Request, p *principal) {
	out := []keyView{}
	for _, k := range s.store.Snapshot().Keys {
		if k.TeamID == p.Team.ID && (k.UserID == p.User.ID || p.has("admin")) {
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

func (s *server) deleteKey(w http.ResponseWriter, r *http.Request, p *principal) {
	id := r.PathValue("id")
	k := s.store.Snapshot().Keys[id]
	if k == nil || k.TeamID != p.Team.ID || (k.UserID != p.User.ID && !p.has("admin")) {
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
	err = s.store.Update(func(st *State) error {
		st.Secrets[secretKey(p.Team.ID, name)] = &Secret{TeamID: p.Team.ID, Name: name, Sealed: sealed, Updated: now}
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"name": name, "updated": now})
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
