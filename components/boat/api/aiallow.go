package main

import (
	"net/http"
	"strings"
)

// The AI proxy forwards only inference endpoints: what the providers' SDKs and
// the agent harnesses in the guest (claude, codex, opencode, kimi, vibe) call
// to run a model. A VM token must not reach a provider's files, batches,
// fine-tuning, assistants, admin or organization APIs with the team's real
// key. docs/boat.md, "AI credentials proxy", lists the same table.
//
// Each entry is "METHOD path" relative to the provider's upstream; a "*"
// segment matches exactly one path segment (an id). The whole request path
// must match one entry; "." and ".." segments, empty segments and encoded
// slashes are refused before matching.
var aiAllowed = map[string][]string{
	"anthropic": {
		"POST v1/messages", "POST v1/messages/count_tokens", "POST v1/complete",
		"GET v1/models", "GET v1/models/*",
	},
	"openai": {
		"POST v1/chat/completions", "POST v1/completions", "POST v1/embeddings",
		"POST v1/responses", "GET v1/responses/*", "DELETE v1/responses/*", "POST v1/responses/*/cancel",
		"GET v1/responses/*/input_items", "POST v1/responses/input_tokens", "POST v1/responses/compact",
		"GET v1/models", "GET v1/models/*",
	},
	"openrouter": {
		"POST api/v1/chat/completions", "POST api/v1/completions", "POST api/v1/responses", "POST api/v1/messages",
		"POST api/v1/embeddings", "GET api/v1/models", "GET api/v1/models/*/endpoints", "GET api/v1/generation",
	},
	"deepseek": {
		"POST chat/completions", "POST v1/chat/completions", "POST beta/completions", "POST completions",
		"GET models", "GET v1/models", "POST anthropic/v1/messages", "POST anthropic/v1/messages/count_tokens",
	},
	"mistral": {
		"POST v1/chat/completions", "POST v1/fim/completions", "POST v1/agents/completions", "POST v1/embeddings",
		"GET v1/models", "GET v1/models/*",
	},
	"moonshot": {
		"POST v1/chat/completions", "POST v1/embeddings", "GET v1/models", "GET v1/models/*",
		"POST v1/tokenizers/estimate-token-count", "POST anthropic/v1/messages", "POST anthropic/v1/messages/count_tokens",
	},
	"xai": {
		"POST v1/chat/completions", "POST v1/completions", "POST v1/responses", "GET v1/responses/*",
		"POST v1/messages", "POST v1/embeddings", "GET v1/models", "GET v1/models/*",
		"GET v1/language-models", "GET v1/language-models/*",
	},
}

// aiPathAllowed reports whether r may go to provider's upstream path.
func aiPathAllowed(provider string, r *http.Request, path string) bool {
	if strings.Contains(strings.ToLower(r.URL.EscapedPath()), "%2f") ||
		strings.Contains(strings.ToLower(r.URL.EscapedPath()), "%2e") {
		return false
	}
	segs := strings.Split(path, "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return false
		}
	}
	for _, entry := range aiAllowed[provider] {
		method, pattern, _ := strings.Cut(entry, " ")
		if method == r.Method && segmentsMatch(strings.Split(pattern, "/"), segs) {
			return true
		}
	}
	return false
}

func segmentsMatch(pattern, segs []string) bool {
	if len(pattern) != len(segs) {
		return false
	}
	for i, p := range pattern {
		if p != "*" && p != segs[i] {
			return false
		}
	}
	return true
}
