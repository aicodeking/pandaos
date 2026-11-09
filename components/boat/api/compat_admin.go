package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

// boat.dev's API keys and environments on the native ones (admin.go,
// environments.go). The models differ, so the mapping is narrow:
//
// Keys: boat.dev's actions are this server's four scopes; presets name sets
// of them. No expiry (ttl), no per-sandbox or per-environment restriction.
// Rotation replaces the secret at once, as on boat.dev.
//
// Environments: boat.dev's id is a UUID made from the native id; envContents
// is the environment's variables as KEY=value lines; passSecrets false selects
// no team secrets. Image, size and setup script are /v1 only.

var compatKeyPresets = map[string][]string{"read-only": {"vms:read"}, "full-sandbox": {"vms:read", "vms:write", "exec"},
	"ci": {"vms:read", "vms:write", "exec"}, "admin": allScopes}

func (s *server) compatKeyView(k *APIKey) map[string]any {
	var last any
	req := int64(0)
	if v, ok := s.keyUse.Load(k.ID); ok {
		u := v.(*keyUsage)
		req = u.requests.Load()
		last = time.Unix(u.last.Load(), 0).UTC()
	}
	scopes := k.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	return map[string]any{"id": k.ID, "name": k.Name, "credentialLane": "scoped-v1", "keyPrefix": tokenPrefix + k.ID,
		"keyLastFour": "****", "sandboxId": nil, "createdAt": k.Created, "lastUsedAt": last,
		"usage": map[string]any{"requests": req, "windowDays": 30}, "resources": map[string]any{"total": 0, "sandboxes": 0, "agents": 0},
		"expiresAt": nil, "expired": false, "expiringSoon": false, "scope": map[string]any{"actions": scopes}}
}

func (s *server) compatKeyList(p *principal) []map[string]any {
	st := s.store.Snapshot()
	var keys []*APIKey
	for _, k := range st.Keys {
		if canManageKey(st, p, k) {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Created.Before(keys[j].Created) })
	out := []map[string]any{}
	for _, k := range keys {
		out = append(out, s.compatKeyView(k))
	}
	return out
}

func (s *server) compatAPIKeys(w http.ResponseWriter, r *http.Request, p *principal) {
	writeCompat(w, 200, "api_keys.list", map[string]any{"apiKeys": s.compatKeyList(p), "catalog": map[string]any{
		"actions": allScopes, "presets": compatKeyPresets, "defaultTtl": "never", "maxTtl": "never", "scopedCreationEnabled": true}})
}

func (s *server) compatCreateKey(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Name           string   `json:"name"`
		TTL            string   `json:"ttl"`
		Preset         string   `json:"preset"`
		Actions        []string `json:"actions"`
		SandboxIDs     []string `json:"sandboxIds"`
		EnvironmentIDs []string `json:"environmentIds"`
	}
	if !decode(w, r, &body) {
		return
	}
	scopes := slices.Clone(body.Actions)
	switch {
	case body.TTL != "" && body.TTL != "never":
		writeErr(w, r, 400, "invalid_request", "keys do not expire on this server; leave ttl out")
		return
	case len(body.SandboxIDs) > 0 || len(body.EnvironmentIDs) > 0:
		writeErr(w, r, 400, "invalid_request", "keys cannot be limited to sandboxes or environments on this server")
		return
	case body.Preset != "":
		pre, ok := compatKeyPresets[body.Preset]
		if !ok {
			writeErr(w, r, 400, "invalid_request", "preset must be read-only, full-sandbox, ci or admin")
			return
		}
		scopes = append(scopes, pre...)
	}
	slices.Sort(scopes)
	res := s.callNative(r, p, "POST /v1/keys", s.createKey, "", map[string]any{"name": body.Name, "scopes": slices.Compact(scopes)})
	if !res.ok() {
		res.relay(w)
		return
	}
	var kv keyView
	json.Unmarshal(res.body.Bytes(), &kv)
	k := s.store.Snapshot().Keys[kv.ID]
	if k == nil {
		writeErr(w, r, 409, "conflict", "the key was deleted meanwhile")
		return
	}
	writeCompat(w, 201, "api_key.created", map[string]any{"apiKey": s.compatKeyView(k), "secret": kv.Token, "apiKeys": s.compatKeyList(p)})
}

func (s *server) compatRevokeKey(w http.ResponseWriter, r *http.Request, p *principal) {
	res := s.callNative(r, p, "DELETE /v1/keys/{id}", s.deleteKey, r.PathValue("id"), nil)
	if !res.ok() {
		res.relay(w)
		return
	}
	writeCompat(w, 200, "api_key.revoked", map[string]any{"apiKeys": s.compatKeyList(p)})
}

func (s *server) compatRotateKey(w http.ResponseWriter, r *http.Request, p *principal) {
	res := s.callNative(r, p, "POST /v1/keys/{id}/rotate", s.rotateKey, r.PathValue("id"), map[string]any{"overlap_s": 0})
	if !res.ok() {
		res.relay(w)
		return
	}
	var kv keyView
	json.Unmarshal(res.body.Bytes(), &kv)
	k := s.store.Snapshot().Keys[kv.ID]
	if k == nil {
		writeErr(w, r, 409, "conflict", "the key was deleted meanwhile")
		return
	}
	writeCompat(w, 200, "api_key.rotated", map[string]any{"apiKey": s.compatKeyView(k), "secret": kv.Token, "apiKeys": s.compatKeyList(p)})
}

func (s *server) compatKeyUsage(w http.ResponseWriter, r *http.Request, p *principal) {
	st := s.store.Snapshot()
	k := st.Keys[r.PathValue("id")]
	if !canManageKey(st, p, k) {
		writeErr(w, r, 404, "not_found", "key not found")
		return
	}
	v := s.compatKeyView(k)
	if v["lastUsedAt"] == nil {
		v["lastUsedAt"] = k.Created // boat.dev's usage answer has no null here
	}
	v["sandboxId"] = ""
	v["createdResources"] = []any{} // sandboxes created with this key: not recorded here
	writeCompat(w, 200, "api_key.usage", v)
}

// ---- environments ----

// envUUID is a stable UUID for a native environment id.
func envUUID(id string, version int) string {
	sum := sha256.Sum256([]byte("boat-env/" + id + "/" + strings.Repeat("v", version)))
	h := hex.EncodeToString(sum[:16])
	return h[:8] + "-" + h[8:12] + "-4" + h[13:16] + "-8" + h[17:20] + "-" + h[20:32]
}

func dotenv(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + m[k] + "\n")
	}
	return b.String()
}

// parseDotenv reads KEY=value lines; blank lines and # comments are skipped.
func parseDotenv(s string) (map[string]string, string) {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, "envContents lines must be KEY=value"
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out, ""
}

func compatEnvView(e *Environment) map[string]any {
	latest := envUUID(e.ID, e.Version)
	return map[string]any{"id": envUUID(e.ID, 0), "name": e.Name, "isDefault": e.IsDefault, "latestVersionId": latest,
		"safeForThirdParties": false, "passGithub": false, "passSecrets": e.Secrets == nil || len(*e.Secrets) > 0,
		"passSandboxCredentials": false, "passAgentsCredentials": false, "envContents": dotenv(e.Env), "secretFiles": []any{},
		"versions": []any{map[string]any{"id": latest, "versionNumber": e.Version, "sandboxCount": 0, "createdAt": e.Updated}}}
}

func (s *server) compatEnvList(p *principal) []map[string]any {
	var envs []*Environment
	for _, e := range s.store.Snapshot().Environments {
		if e.TeamID == p.Team.ID {
			envs = append(envs, e)
		}
	}
	sort.Slice(envs, func(i, j int) bool { return envs[i].Name < envs[j].Name })
	out := []map[string]any{}
	for _, e := range envs {
		out = append(out, compatEnvView(e))
	}
	return out
}

// pathEnv finds the team environment an {id} path names (its UUID, native id or name).
func (s *server) pathEnv(w http.ResponseWriter, r *http.Request, p *principal) (*Environment, bool) {
	ref := r.PathValue("id")
	for _, e := range s.store.Snapshot().Environments {
		if e.TeamID == p.Team.ID && (envUUID(e.ID, 0) == strings.ToLower(ref) || e.ID == ref || e.Name == ref) {
			return e, true
		}
	}
	writeErr(w, r, 404, "not_found", "environment not found")
	return nil, false
}

func (s *server) compatEnvironments(w http.ResponseWriter, r *http.Request, p *principal) {
	writeCompat(w, 200, "environments.list", map[string]any{"environments": s.compatEnvList(p)})
}

func (s *server) compatCreateEnv(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	res := s.callNative(r, p, "POST /v1/environments", s.createEnv, "", map[string]any{"name": body.Name})
	if !res.ok() {
		res.relay(w)
		return
	}
	writeCompat(w, 200, "environments.list", map[string]any{"environments": s.compatEnvList(p)})
}

func (s *server) compatUpdateEnv(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Name        *string `json:"name"`
		IsDefault   *bool   `json:"isDefault"`
		EnvContents *string `json:"envContents"`
		PassSecrets *bool   `json:"passSecrets"`
	}
	if !decode(w, r, &body) {
		return
	}
	e, ok := s.pathEnv(w, r, p)
	if !ok {
		return
	}
	in := map[string]any{}
	if body.Name != nil {
		in["name"] = *body.Name
	}
	if body.IsDefault != nil {
		in["is_default"] = *body.IsDefault
	}
	if body.EnvContents != nil {
		vars, msg := parseDotenv(*body.EnvContents)
		if msg != "" {
			writeErr(w, r, 400, "invalid_request", msg)
			return
		}
		in["env"] = vars
	}
	if body.PassSecrets != nil && !*body.PassSecrets {
		in["secrets"] = []string{}
	} else if body.PassSecrets != nil && e.Secrets != nil && len(*e.Secrets) == 0 {
		writeErr(w, r, 400, "invalid_request", "to pass secrets again, choose them with PUT /v1/environments/{id} (secrets)")
		return
	}
	res := s.callNative(r, p, "PUT /v1/environments/{id}", s.updateEnv, e.ID, in)
	if !res.ok() {
		res.relay(w)
		return
	}
	cur := s.store.Snapshot().Environments[e.ID]
	if cur == nil {
		writeErr(w, r, 409, "conflict", "the environment was deleted meanwhile")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "environment": compatEnvView(cur)})
}

func (s *server) compatDeleteEnv(w http.ResponseWriter, r *http.Request, p *principal) {
	e, ok := s.pathEnv(w, r, p)
	if !ok {
		return
	}
	view := compatEnvView(e)
	res := s.callNative(r, p, "DELETE /v1/environments/{id}", s.deleteEnv, e.ID, nil)
	if !res.ok() {
		res.relay(w)
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "environment": view})
}
