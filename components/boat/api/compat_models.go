package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// boatDevProvider is boat.dev's name for each native agent (compatProvider's inverse).
var boatDevProvider = map[string]string{"claude": "claude-code", "prime": "prime-agent", "kimi": "kimi-code"}

// parseProviderModels reads -provider-models: "claude=sonnet,opus;codex=gpt-5".
func parseProviderModels(v string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, part := range strings.Split(v, ";") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		agent, list, ok := strings.Cut(part, "=")
		agent = canonicalAgent(compatProvider(strings.TrimSpace(agent)))
		if !ok || agents[agent] == nil {
			return nil, fmt.Errorf("-provider-models: %q: want agent=model,model with agent one of %s", part, agentNames)
		}
		for _, m := range strings.Split(list, ",") {
			if m = strings.TrimSpace(m); m != "" {
				if !reModel.MatchString(m) {
					return nil, fmt.Errorf("-provider-models: invalid model %q", m)
				}
				out[agent] = append(out[agent], m)
			}
		}
	}
	return out, nil
}

// compatProviderModels is GET /api/v1/provider-models: every agent harness
// installed in the guest image, with the models the operator listed for it
// (-provider-models). No harness here takes a reasoning effort or fast mode,
// so each model lists none; omitting model uses the harness's own default.
func (s *server) compatProviderModels(w http.ResponseWriter, r *http.Request, p *principal) {
	names := make([]string, 0, len(agents))
	for a := range agents {
		names = append(names, a)
	}
	slices.Sort(names)
	providers := make([]map[string]any, 0, len(names))
	for _, a := range names {
		models := []map[string]any{}
		for _, m := range s.cfg.ProviderModels[a] {
			models = append(models, map[string]any{"id": m, "name": m, "reasoningEfforts": []string{}, "fastMode": false})
		}
		providers = append(providers, map[string]any{"provider": orDefault(boatDevProvider[a], a), "harness": a, "models": models})
	}
	writeCompat(w, 200, "provider_models.list", map[string]any{"providers": providers})
}
