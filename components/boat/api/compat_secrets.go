package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// boat.dev's /secrets and /ai-proxy on the team secrets (admin.go) and the AI
// credentials proxy switch (aiproxy.go).
//
// boat.dev's /secrets is the env every sandbox gets. Here that is the team
// secrets an admin marked default-injected (Secret.Inject), so:
//
//   - GET lists them as envContents "NAME=" lines: values are never returned.
//   - POST envContents sets each NAME=value (sealed, marked default-injected).
//     "NAME=" with no value keeps a secret's value and marks it. A
//     default-injected secret left out is unmarked, not deleted (DELETE
//     /v1/secrets/{name} deletes). secretFiles are not supported.
//
// Both need admin: only an admin may decide what every VM gets.

const compatSecretsEnv = "env_team_secrets"

func (s *server) compatSecretsView(team string) map[string]any {
	names := []string{}
	for _, sec := range s.store.Snapshot().Secrets {
		if sec.TeamID == team && sec.Inject {
			names = append(names, sec.Name)
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n + "=\n")
	}
	return map[string]any{"success": true, "environmentId": compatSecretsEnv, "envContents": b.String(), "secretFiles": []any{}}
}

func (s *server) compatSecrets(w http.ResponseWriter, r *http.Request, p *principal) {
	writeCompat(w, 200, "secrets.info", s.compatSecretsView(p.Team.ID))
}

// parseEnvContents reads KEY=value lines (blank lines and # comments skipped,
// one pair of surrounding quotes stripped). keep holds the names given with
// no value.
func parseEnvContents(text string) (vals map[string]string, keep []string, msg string) {
	vals = map[string]string{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		k = strings.TrimSpace(k)
		if !ok || !reSecretName.MatchString(k) {
			return nil, nil, "envContents line " + itoa(i+1) + ": want NAME=value with an UPPER_SNAKE_CASE name"
		}
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if v == "" {
			keep = append(keep, k)
			continue
		}
		vals[k] = v
	}
	return vals, keep, ""
}

func (s *server) compatUpdateSecrets(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		EnvContents *string `json:"envContents"`
		SecretFiles []any   `json:"secretFiles"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.SecretFiles) > 0 {
		writeErr(w, r, 400, "invalid_request", "secretFiles are not supported by this server: write files with PUT /api/v1/sandboxes/{id}/files or an environment's setup")
		return
	}
	if body.EnvContents == nil {
		writeCompat(w, 200, "secrets.updated", s.withPushed(s.compatSecretsView(p.Team.ID), 0))
		return
	}
	vals, keep, msg := parseEnvContents(*body.EnvContents)
	st := s.store.Snapshot()
	for _, k := range keep {
		if msg == "" && st.Secrets[secretKey(p.Team.ID, k)] == nil {
			msg = "secret " + k + " has no value yet: give one (NAME=value)"
		}
	}
	if msg != "" {
		writeErr(w, r, 400, "invalid_request", msg)
		return
	}
	names := make([]string, 0, len(vals))
	for k := range vals {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		res := s.callNative(r, p, "PUT /v1/secrets/{name}", s.putSecret, "", map[string]any{"value": vals[k], "inject": true}, "name", k)
		if !res.ok() {
			res.relay(w)
			return
		}
	}
	marked := map[string]bool{}
	for _, k := range append(names, keep...) {
		marked[k] = true
	}
	err := s.store.Update(func(st *State) error {
		for _, sec := range st.Secrets {
			if sec.TeamID == p.Team.ID && sec.Inject != marked[sec.Name] {
				c := *sec
				c.Inject = marked[sec.Name]
				st.Secrets[secretKey(sec.TeamID, sec.Name)] = &c
			}
		}
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeCompat(w, 200, "secrets.updated", s.withPushed(s.compatSecretsView(p.Team.ID), len(names)))
}

// withPushed adds boat.dev's pushed counts: secrets reach VMs at their next
// start here, so nothing is pushed to running ones.
func (s *server) withPushed(v map[string]any, updated int) map[string]any {
	v["pushed"] = map[string]any{"updated": 0, "failed": 0, "secretsChanged": updated, "appliesAt": "next start"}
	return v
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// ---- AI credentials proxy switch ----

func (s *server) compatAIProxyView(team string) map[string]any {
	enabled := !s.store.Snapshot().AIProxyOff[team]
	return map[string]any{"success": true, "accountId": team, "org": true, "enabled": enabled, "enabledAt": nil,
		"pricing":  map[string]any{"dailyDollars": 0, "dollarsPer1000Requests": 0},
		"proxyUrl": orDefault(s.cfg.AIProxyURL, orDefault(s.cfg.PublicURL, "http://localhost/"))}
}

func (s *server) compatGetAIProxy(w http.ResponseWriter, r *http.Request, p *principal) {
	writeCompat(w, 200, "ai_proxy.info", s.compatAIProxyView(p.Team.ID))
}

func (s *server) compatPutAIProxy(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decode(w, r, &body) {
		return
	}
	res := s.callNative(r, p, "PUT /v1/ai/proxy", s.putAIProxy, "", body)
	if !res.ok() {
		res.relay(w)
		return
	}
	writeCompat(w, 200, "ai_proxy.updated", s.compatAIProxyView(p.Team.ID))
}
