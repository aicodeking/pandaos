package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"
)

// boat.dev's credential profiles over this server's AI credentials
// (/v1/ai/credentials): each team has exactly one profile, "default", that
// holds one API key per provider. Sign-ins, Bedrock, custom APIs, more profiles
// and more accounts per credential do not exist here.

// compatCredProviders maps boat.dev's credential ids to native providers.
var compatCredProviders = map[string]string{
	"anthropic-api-key":  "anthropic",
	"openai-api-key":     "openai",
	"openrouter-api-key": "openrouter",
	"deepseek-api-key":   "deepseek",
	"moonshot-api-key":   "moonshot",
	"mistral-api-key":    "mistral",
	"xai-api-key":        "xai",
}

const compatProfileName = "default"

func notSupported(w http.ResponseWriter, r *http.Request, msg string) {
	writeErr(w, r, http.StatusNotImplemented, "not_supported", msg+" is not supported on self-hosted boat")
}

// compatProfile checks the {profile} path value: the team's one profile, by
// its name or id (the team id).
func compatProfile(w http.ResponseWriter, r *http.Request, p *principal) bool {
	if v := r.PathValue("profile"); v == compatProfileName || v == p.Team.ID {
		return true
	}
	writeErr(w, r, 404, "profile_not_found", "no credential profile "+r.PathValue("profile")+"; this server has one per team, \"default\"")
	return false
}

// compatCredential resolves {credential}: 400 for an id boat.dev does not
// know, 501 for one that has no native provider.
func compatCredential(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("credential")
	if pr, ok := compatCredProviders[id]; ok {
		return pr, true
	}
	switch {
	case strings.HasSuffix(id, "-subscription") || strings.HasSuffix(id, "-bedrock") || id == "llmgateway-api-key" || id == "opencode-api-key":
		notSupported(w, r, "credential "+id)
	default:
		writeErr(w, r, 400, "invalid_request", "unknown credential "+id)
	}
	return "", false
}

func compatCredID(provider string) string {
	for id, pr := range compatCredProviders {
		if pr == provider {
			return id
		}
	}
	return ""
}

func (s *server) compatProfileView(p *principal) map[string]any {
	st := s.store.Snapshot()
	updated := p.Team.Created
	connected := []string{}
	for _, c := range st.AICreds {
		if c.TeamID != p.Team.ID || compatCredID(c.Provider) == "" {
			continue
		}
		connected = append(connected, compatCredID(c.Provider))
		if c.Updated.After(updated) {
			updated = c.Updated
		}
	}
	slices.Sort(connected)
	return map[string]any{
		"id": p.Team.ID, "name": compatProfileName, "isDefault": true,
		"owner":     map[string]any{"id": p.Team.ID, "kind": "org", "name": p.Team.Name, "aiProxyEnabled": !st.AIProxyOff[p.Team.ID]},
		"canManage": p.has("admin"), "connected": connected, "accounts": map[string]any{}, "customApis": []string{},
		"createdAt": p.Team.Created.UTC().Format(time.RFC3339), "updatedAt": updated.UTC().Format(time.RFC3339),
	}
}

func (s *server) writeProfile(w http.ResponseWriter, typ string, p *principal) {
	writeCompat(w, 200, typ, map[string]any{"success": true, "profile": s.compatProfileView(p)})
}

// compatAccounts: a credential here has at most one account, its key; the
// account id is the credential id.
func (s *server) writeAccounts(w http.ResponseWriter, status int, typ string, p *principal, provider string) {
	accounts := []map[string]any{}
	if c := s.store.Snapshot().AICreds[aiCredKey(p.Team.ID, provider)]; c != nil {
		accounts = append(accounts, map[string]any{"id": compatCredID(provider), "label": "…" + c.Last4, "active": true})
	}
	writeCompat(w, status, typ, map[string]any{"success": true, "profileId": p.Team.ID, "credential": compatCredID(provider), "accounts": accounts})
}

func (s *server) compatListProfiles(w http.ResponseWriter, r *http.Request, p *principal) {
	writeCompat(w, 200, "credential_profile.list", map[string]any{"profiles": []any{s.compatProfileView(p)}})
}

func (s *server) compatCreateProfile(w http.ResponseWriter, r *http.Request, p *principal) {
	notSupported(w, r, "more than one credential profile per team")
}

func (s *server) compatUpdateProfile(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Name      string          `json:"name"`
		IsDefault *bool           `json:"isDefault"`
		Access    json.RawMessage `json:"access"`
	}
	if !decode(w, r, &body) || !compatProfile(w, r, p) {
		return
	}
	if (body.Name != "" && body.Name != compatProfileName) || len(body.Access) > 0 {
		notSupported(w, r, "renaming the credential profile or setting its access")
		return
	}
	s.writeProfile(w, "credential_profile.updated", p)
}

func (s *server) compatDeleteProfile(w http.ResponseWriter, r *http.Request, p *principal) {
	if compatProfile(w, r, p) {
		writeErr(w, r, 409, "is_default", "the default credential profile cannot be deleted; remove its keys instead")
	}
}

func (s *server) compatSetProfileKey(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Value       string `json:"value"`
		KeepCurrent bool   `json:"keepCurrent"`
	}
	if !decode(w, r, &body) || !compatProfile(w, r, p) {
		return
	}
	provider, ok := compatCredential(w, r)
	if !ok {
		return
	}
	if body.KeepCurrent && s.store.Snapshot().AICreds[aiCredKey(p.Team.ID, provider)] != nil {
		notSupported(w, r, "more than one account per credential (keepCurrent)")
		return
	}
	res := s.callNative(r, p, "PUT /v1/ai/credentials/{provider}", s.putAICred, "", map[string]any{"value": body.Value}, "provider", provider)
	if !res.ok() {
		res.relay(w)
		return
	}
	s.writeProfile(w, "credential_profile.updated", p)
}

func (s *server) compatDeleteProfileKey(w http.ResponseWriter, r *http.Request, p *principal) {
	if !compatProfile(w, r, p) {
		return
	}
	provider, ok := compatCredential(w, r)
	if !ok {
		return
	}
	res := s.callNative(r, p, "DELETE /v1/ai/credentials/{provider}", s.deleteAICred, "", nil, "provider", provider)
	if !res.ok() {
		res.relay(w)
		return
	}
	s.writeProfile(w, "credential_profile.updated", p)
}

func (s *server) compatListAccounts(w http.ResponseWriter, r *http.Request, p *principal) {
	if !compatProfile(w, r, p) {
		return
	}
	if provider, ok := compatCredential(w, r); ok {
		s.writeAccounts(w, 200, "credential_account.list", p, provider)
	}
}

// compatAddAccount sets the credential's first key; a second one is 501.
func (s *server) compatAddAccount(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Value       string `json:"value"`
		KeepCurrent bool   `json:"keepCurrent"`
	}
	if !decode(w, r, &body) || !compatProfile(w, r, p) {
		return
	}
	provider, ok := compatCredential(w, r)
	if !ok {
		return
	}
	if s.store.Snapshot().AICreds[aiCredKey(p.Team.ID, provider)] != nil {
		notSupported(w, r, "more than one account per credential")
		return
	}
	res := s.callNative(r, p, "PUT /v1/ai/credentials/{provider}", s.putAICred, "", map[string]any{"value": body.Value}, "provider", provider)
	if !res.ok() {
		res.relay(w)
		return
	}
	s.writeAccounts(w, 201, "credential_account.added", p, provider)
}

// compatAccount checks {account} is the credential's one account.
func (s *server) compatAccount(w http.ResponseWriter, r *http.Request, p *principal) (string, bool) {
	if !compatProfile(w, r, p) {
		return "", false
	}
	provider, ok := compatCredential(w, r)
	if !ok {
		return "", false
	}
	if r.PathValue("account") != r.PathValue("credential") || s.store.Snapshot().AICreds[aiCredKey(p.Team.ID, provider)] == nil {
		writeErr(w, r, 404, "account_not_found", "no account "+r.PathValue("account"))
		return "", false
	}
	return provider, true
}

func (s *server) compatSwitchAccount(w http.ResponseWriter, r *http.Request, p *principal) {
	if provider, ok := s.compatAccount(w, r, p); ok {
		s.writeAccounts(w, 200, "credential_account.switched", p, provider) // already the one in use
	}
}

func (s *server) compatDeleteAccount(w http.ResponseWriter, r *http.Request, p *principal) {
	provider, ok := s.compatAccount(w, r, p)
	if !ok {
		return
	}
	res := s.callNative(r, p, "DELETE /v1/ai/credentials/{provider}", s.deleteAICred, "", nil, "provider", provider)
	if !res.ok() {
		res.relay(w)
		return
	}
	s.writeAccounts(w, 200, "credential_account.deleted", p, provider)
}

func (s *server) compatCustomAPI(w http.ResponseWriter, r *http.Request, p *principal) {
	if compatProfile(w, r, p) {
		notSupported(w, r, "custom APIs in credential profiles (use team secrets)")
	}
}

// compatSnapshotDownload streams the native archive of a snapshot
// (GET /v1/vms/{id}/snapshots/{sid}/download) instead of boat.dev's JSON of
// signed chunk URLs.
func (s *server) compatSnapshotDownload(w http.ResponseWriter, r *http.Request, p *principal) {
	list, fail := s.teamSnapshots(r, p)
	if fail != nil {
		fail.relay(w)
		return
	}
	want := r.PathValue("sid")
	for _, sn := range list {
		if snapshotUUID(p.Team.ID, str(sn, "id")) != want && str(sn, "id") != want {
			continue
		}
		if str(sn, "vm_id") == "" {
			notSupported(w, r, "downloading an imported snapshot")
			return
		}
		r2 := r.Clone(r.Context())
		r2.SetPathValue("id", str(sn, "vm_id"))
		r2.SetPathValue("sid", str(sn, "id"))
		s.downloadSnapshot(w, r2, p)
		return
	}
	writeErr(w, r, 404, "snapshot_not_found", "no snapshot "+want)
}

// compatUnsupported lists boat.dev operations with no equivalent here; each
// answers 501 not_supported rather than a bare 404.
var compatUnsupported = map[string]string{
	"GET /api/v1/orgs":                                        "organizations",
	"PATCH /api/v1/orgs/active":                               "organizations",
	"PATCH /api/v1/orgs/{org}":                                "organizations",
	"GET /api/v1/account/data-retention":                      "data retention settings",
	"PATCH /api/v1/account/data-retention":                    "data retention settings",
	"GET /api/v1/repos":                                       "repositories",
	"POST /api/v1/repos":                                      "repositories",
	"GET /api/v1/ci":                                          "boat CI",
	"POST /api/v1/ci/connect":                                 "boat CI",
	"POST /api/v1/ci/public":                                  "boat CI",
	"GET /api/v1/ci/jobs/{jobId}":                             "boat CI",
	"GET /api/v1/ci/jobs/{jobId}/logs":                        "boat CI",
	"POST /api/v1/environments/{id}/upgrade":                  "environment upgrades",
	"PUT /api/v1/environments/{id}/secret-files":              "environment secret files",
	"DELETE /api/v1/environments/{id}/secret-files":           "environment secret files",
	"POST /api/v1/environments/{id}/repos":                    "environment repositories",
	"DELETE /api/v1/environments/{id}/repos/{repositoryId}":   "environment repositories",
	"GET /api/v1/snapshots/{sid}/tree":                        "snapshot file trees (download the snapshot instead)",
	"GET /api/v1/snapshots/{sid}/files":                       "reading files from a snapshot (download the snapshot instead)",
}

func (s *server) compatProfileRoutes() {
	s.route("GET /api/v1/credential-profiles", "admin", s.compatListProfiles)
	s.route("POST /api/v1/credential-profiles", "admin", s.compatCreateProfile)
	s.route("PUT /api/v1/credential-profiles/{profile}", "admin", s.compatUpdateProfile)
	s.route("DELETE /api/v1/credential-profiles/{profile}", "admin", s.compatDeleteProfile)
	s.route("PUT /api/v1/credential-profiles/{profile}/keys/{credential}", "admin", s.compatSetProfileKey)
	s.route("DELETE /api/v1/credential-profiles/{profile}/keys/{credential}", "admin", s.compatDeleteProfileKey)
	s.route("GET /api/v1/credential-profiles/{profile}/accounts/{credential}", "admin", s.compatListAccounts)
	s.route("POST /api/v1/credential-profiles/{profile}/accounts/{credential}", "admin", s.compatAddAccount)
	s.route("PUT /api/v1/credential-profiles/{profile}/accounts/{credential}/{account}/active", "admin", s.compatSwitchAccount)
	s.route("DELETE /api/v1/credential-profiles/{profile}/accounts/{credential}/{account}", "admin", s.compatDeleteAccount)
	s.route("PUT /api/v1/credential-profiles/{profile}/custom-apis/{name}", "admin", s.compatCustomAPI)
	s.route("DELETE /api/v1/credential-profiles/{profile}/custom-apis/{name}", "admin", s.compatCustomAPI)
	s.route("GET /api/v1/snapshots/{sid}/download", "exec", s.compatSnapshotDownload)
	for pattern, what := range compatUnsupported {
		s.route(pattern, "", func(w http.ResponseWriter, r *http.Request, p *principal) { notSupported(w, r, what) })
	}
}
