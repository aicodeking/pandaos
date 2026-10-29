package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var mem = server{Key: "panda-mem", Command: "panda-mem", Args: []string{"mcp", "-socket", "/run/panda-mem/sock"}, Env: map[string]string{}}
var sys = server{Key: "panda-sys", Command: "panda-sys", Args: []string{"mcp"}, Env: map[string]string{"A": `q"\x`}}

func claudeApply(t *testing.T, old string, ss ...server) string {
	t.Helper()
	out, err := targets[0].apply([]byte(old), ss)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestClaudeJSONMergeKeepsUserAndIsIdempotent(t *testing.T) {
	old := `{"numStartups": 12345678901234567890, "mcpServers": {"mine": {"command": "x"}, "panda-stale": {"command": "gone"}}}`
	once := claudeApply(t, old, mem, sys)
	var got map[string]any
	if err := decodeJSON([]byte(once), &got); err != nil {
		t.Fatal(err)
	}
	srv := got["mcpServers"].(map[string]any)
	if _, ok := srv["mine"]; !ok {
		t.Fatal("user entry dropped")
	}
	if _, ok := srv["panda-stale"]; ok {
		t.Fatal("stale owned entry kept")
	}
	if srv["panda-mem"].(map[string]any)["command"] != "panda-mem" || srv["panda-sys"] == nil {
		t.Fatalf("owned entries missing: %v", srv)
	}
	if got["numStartups"].(json.Number).String() != "12345678901234567890" {
		t.Fatal("number mangled")
	}
	if twice := claudeApply(t, once, mem, sys); twice != once {
		t.Fatalf("not idempotent:\n%s\n%s", once, twice)
	}
	// Already in sync: byte-identical, even with odd formatting.
	if again := claudeApply(t, once, mem, sys); again != once {
		t.Fatal("rewrote an in-sync file")
	}
}

func TestJSONEmptyAndBad(t *testing.T) {
	if out := claudeApply(t, ""); out != "" {
		t.Fatalf("empty registry created a file: %q", out)
	}
	if !strings.Contains(claudeApply(t, "", mem), `"panda-mem"`) {
		t.Fatal("new file lacks server")
	}
	if _, err := targets[0].apply([]byte("{oops"), []server{mem}); err == nil {
		t.Fatal("bad JSON accepted")
	}
	if _, err := targets[0].apply([]byte(`{"mcpServers": []}`), []server{mem}); err == nil {
		t.Fatal("non-object section accepted")
	}
}

func TestOpencodeShape(t *testing.T) {
	out, err := targets[2].apply(nil, []server{mem})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MCP map[string]struct {
			Type    string   `json:"type"`
			Command []string `json:"command"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	e := got.MCP["panda-mem"]
	if e.Type != "local" || strings.Join(e.Command, " ") != "panda-mem mcp -socket /run/panda-mem/sock" {
		t.Fatalf("%+v", e)
	}
}

const userTOML = `model = "o3"

[mcp_servers.mine]
command = "mine"

[mcp_servers.panda-old]
command = "old"

[mcp_servers.panda-old.env]
X = "1"

[profiles.fast]
model = "o4-mini"
`

func TestCodexTOML(t *testing.T) {
	once := mergeCodexTOML(userTOML, []server{mem, sys})
	for _, want := range []string{"[mcp_servers.mine]\ncommand = \"mine\"", "[profiles.fast]\nmodel = \"o4-mini\"", `model = "o3"`,
		"[mcp_servers.panda-mem]\ncommand = \"panda-mem\"\nargs = [\"mcp\", \"-socket\", \"/run/panda-mem/sock\"]\n",
		`env = { "A" = "q\"\\x" }`} {
		if !strings.Contains(once, want) {
			t.Fatalf("missing %q in:\n%s", want, once)
		}
	}
	if strings.Contains(once, "panda-old") || strings.Contains(once, `X = "1"`) {
		t.Fatalf("stale owned table kept:\n%s", once)
	}
	if twice := mergeCodexTOML(once, []server{mem, sys}); twice != once {
		t.Fatalf("not idempotent:\n%s\n---\n%s", once, twice)
	}
	if got := mergeCodexTOML(once, nil); strings.Contains(got, "panda-") || !strings.Contains(got, "[profiles.fast]") {
		t.Fatalf("empty registry did not remove owned tables:\n%s", got)
	}
	if got := mergeCodexTOML("", nil); got != "" {
		t.Fatalf("created content from nothing: %q", got)
	}
	if got := tomlString("a\nb"); got != `"a\u000Ab"` {
		t.Fatal(got)
	}
}

func TestRegistry(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) { os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644) }
	write("mem.json", `{"command":"panda-mem","args":["mcp"]}`)
	write("panda-sys.json", `{"command":"panda-sys","args":[],"env":{"K":"v"}}`)
	write("Bad Name.json", `{"command":"x"}`)
	write("nocmd.json", `{"args":[]}`)
	write("extra.json", `{"command":"x","bogus":1}`)
	ss, err := loadRegistry(dir)
	if err == nil {
		t.Fatal("bad files not reported")
	}
	if len(ss) != 2 || ss[0].Key != "panda-mem" || ss[1].Key != "panda-sys" || ss[0].Env == nil {
		t.Fatalf("%+v", ss)
	}
}

// End to end: fake claude/codex homes, sync twice, user entries survive.
func TestSyncEndToEnd(t *testing.T) {
	home, reg := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(reg, "mem.json"), []byte(`{"command":"panda-mem","args":["mcp"],"env":{}}`), 0o644)
	md := filepath.Join(reg, "AGENTS.md")
	os.WriteFile(md, []byte("# brief\n"), 0o644)
	registryDir, agentsMD, claudeInclude = reg, md, "@"+md
	t.Setenv("PATH", "")
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"mine":{"command":"x"}}}`), 0o600)
	os.Mkdir(filepath.Join(home, ".claude"), 0o700)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(userTOML), 0o600)

	var out, errb bytes.Buffer
	if rc := syncAll(home, true, &out, &errb); rc != 0 {
		t.Fatalf("rc=%d %s", rc, errb.String())
	}
	snap := func() string {
		var s string
		for _, p := range []string{".claude.json", ".codex/config.toml", ".claude/CLAUDE.md"} {
			b, _ := os.ReadFile(filepath.Join(home, p))
			s += string(b)
		}
		return s
	}
	first := snap()
	if !strings.Contains(first, `"mine"`) || !strings.Contains(first, "[mcp_servers.mine]") || !strings.Contains(first, "[mcp_servers.panda-mem]") {
		t.Fatal(first)
	}
	if st, _ := os.Stat(filepath.Join(home, ".claude.json")); st.Mode().Perm() != 0o600 {
		t.Fatal("mode changed")
	}
	if l, _ := os.Readlink(filepath.Join(home, "AGENTS.md")); l != md {
		t.Fatal("AGENTS.md not linked")
	}
	if l, _ := os.Readlink(filepath.Join(home, ".codex", "AGENTS.md")); l != md {
		t.Fatal("codex AGENTS.md not linked")
	}
	out.Reset()
	if rc := syncAll(home, true, &out, &errb); rc != 0 || snap() != first {
		t.Fatalf("second sync changed things: rc=%d\n%s", rc, out.String())
	}
	if strings.Count(snap(), "@"+md) != 1 {
		t.Fatal("include line duplicated")
	}
	if !strings.Contains(out.String(), "skip opencode: not installed") {
		t.Fatal(out.String())
	}
	if rc := doctor(home, &out); !strings.Contains(out.String(), "ok   claude") {
		t.Fatalf("rc=%d %s", rc, out.String())
	}
}
