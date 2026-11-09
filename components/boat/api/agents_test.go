package main

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The argv of each agent, from its --help (none of these CLIs runs in CI).
func TestNewAgentArgv(t *testing.T) {
	cases := []struct {
		agent, model, session string
		first                 bool
		want                  []string
	}{
		{"pi", "", "S1", true, []string{"pi", "-p", "--mode", "json", "--session-id", "S1", "--", "hi"}},
		{"pi", "M", "S1", false, []string{"pi", "-p", "--mode", "json", "--session-id", "S1", "--model", "M", "--", "hi"}},
		{"prime", "", "", true, []string{"prime-agent", "-p", "--mode", "json", "hi"}},
		{"prime", "M", "", true, []string{"prime-agent", "-p", "--mode", "json", "--model", "M", "hi"}},
		{"kimi", "", "", true, []string{"kimi", "-p", "hi", "--yolo", "--output-format", "stream-json"}},
		{"kimi", "M", "K1", false, []string{"kimi", "-p", "hi", "--yolo", "--output-format", "stream-json", "-m", "M", "-S", "K1"}},
		{"mistral", "", "", true, []string{"vibe", "-p", "hi", "--auto-approve", "--output", "streaming"}},
		{"mistral", "devstral", "V1", false, []string{"vibe", "-p", "hi", "--auto-approve", "--output", "streaming", "--model", "devstral", "--resume", "V1"}},
	}
	for _, c := range cases {
		if got := agentArgv(c.agent, "hi", c.model, c.session, c.first); !slices.Equal(got, c.want) {
			t.Errorf("%s %q %q: %q, want %q", c.agent, c.model, c.session, got, c.want)
		}
	}
	if got := agentArgv("prime", "-h", "", "", true); got[len(got)-1] != " -h" {
		t.Errorf("prime prompt starting with - is passed as a flag: %q", got)
	}
	for name, want := range map[string][]string{
		"pi":      {"pi", "-p", "--mode", "json", "--", "hi"},
		"prime":   {"prime-agent", "-p", "--mode", "json", "hi"},
		"kimi":    {"kimi", "-p", "hi", "--yolo", "--output-format", "stream-json"},
		"mistral": {"vibe", "-p", "hi", "--auto-approve", "--output", "streaming"},
	} {
		if got := agents[name]("hi", ""); !slices.Equal(got, want) {
			t.Errorf("sync %s: %q, want %q", name, got, want)
		}
	}
}

func TestAgentAliasesAndSessions(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	p := "/v1/vms/" + id + "/prompts"
	// Aliases resolve to the canonical agent, and the conversation records it.
	_, run := e.do(e.owner, "POST", p, map[string]any{"prompt": "go", "agent": "claude-code", "new": true})
	if run["agent"] != "claude" || !strings.HasPrefix(lastCmdArgv(e), "claude") {
		t.Errorf("claude-code alias: %v %s", run, lastCmdArgv(e))
	}
	_, run = e.do(e.owner, "POST", p, map[string]any{"prompt": "go", "agent": "prime-agent", "new": true})
	if run["agent"] != "prime" || !strings.HasPrefix(lastCmdArgv(e), "prime-agent") {
		t.Errorf("prime-agent alias: %v %s", run, lastCmdArgv(e))
	}
	if st, out := e.do(e.owner, "POST", p, map[string]any{"prompt": "go", "agent": "nope"}); st != 400 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "mistral") {
		t.Errorf("unknown agent: %d %v", st, out)
	}
	// pi: boat-api picks the session and passes it on every turn.
	_, run = e.do(e.owner, "POST", p, map[string]any{"prompt": "go", "agent": "pi", "new": true})
	first := lastCmdArgv(e)
	e.do(e.owner, "POST", p, map[string]any{"prompt": "again", "conversation_id": run["conversation_id"]})
	uuid := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	if sid := uuid.FindString(first); sid == "" || !strings.Contains(lastCmdArgv(e), "--session-id"+sid) {
		t.Errorf("pi session not kept: %s / %s", first, lastCmdArgv(e))
	}
	// kimi and mistral learn it from their output, like codex.
	for agent, sess := range map[string]string{"kimi": "kimi_s1", "mistral": "vibe_s1"} {
		_, run := e.do(e.owner, "POST", p, map[string]any{"prompt": "go", "agent": agent, "new": true})
		if strings.Contains(lastCmdArgv(e), sess) {
			t.Errorf("%s first turn already has a session", agent)
		}
		e.do(e.owner, "GET", p+"/"+run["id"].(string), nil)
		if st, _ := e.do(e.owner, "POST", p, map[string]any{"prompt": "again", "conversation_id": run["conversation_id"]}); st != 202 || !strings.Contains(lastCmdArgv(e), sess) {
			t.Errorf("%s resume: %d %s", agent, st, lastCmdArgv(e))
		}
	}
	// Synchronous prompt accepts the new names and aliases too.
	for _, a := range []string{"pi", "prime", "prime-agent", "kimi", "mistral", "claude-code"} {
		if st, _ := e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "x", "agent": a}); st != 200 {
			t.Errorf("sync prompt %s: %d", a, st)
		}
	}
}
