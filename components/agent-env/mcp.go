package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// ownedPrefix marks the agent-config keys panda-agents writes. Everything
// else in an agent's config belongs to the user and is never touched.
const ownedPrefix = "panda-"

var serverName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

type server struct {
	Key     string            `json:"-"` // panda-<name>
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// loadRegistry reads dir/*.json. Bad files are skipped and reported in err;
// the good ones are still returned so one typo doesn't unsync every server.
func loadRegistry(dir string) ([]server, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []server
	var errs []error
	for _, p := range paths {
		s, err := loadServer(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			continue
		}
		out = append(out, s)
	}
	return out, errors.Join(errs...)
}

func loadServer(path string) (server, error) {
	name := strings.TrimSuffix(filepath.Base(path), ".json")
	if !serverName.MatchString(name) {
		return server{}, fmt.Errorf("server name %q must match %s", name, serverName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return server{}, err
	}
	var s server
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return server{}, fmt.Errorf("want {\"command\":...,\"args\":[...],\"env\":{}}: %w", err)
	}
	if s.Command == "" {
		return server{}, errors.New("command is empty")
	}
	if s.Args == nil {
		s.Args = []string{}
	}
	if s.Env == nil {
		s.Env = map[string]string{}
	}
	s.Key = ownedPrefix + strings.TrimPrefix(name, ownedPrefix)
	return s, nil
}

// target is one agent CLI whose MCP config we know how to edit.
type target struct {
	name   string
	bin    string
	marker string // home-relative; its existence also counts as installed
	config string // home-relative config file
	apply  func(old []byte, servers []server) ([]byte, error)
}

// Formats: Claude Code ~/.claude.json, Codex config.toml, opencode.json
// ("mcp", type local), Gemini CLI settings.json and Kimi CLI mcp.json (both
// Claude-style mcpServers).
var targets = []target{
	{"claude", "claude", ".claude", ".claude.json", jsonApply("mcpServers", func(s server) any {
		return map[string]any{"type": "stdio", "command": s.Command, "args": s.Args, "env": s.Env}
	})},
	{"codex", "codex", ".codex", ".codex/config.toml", func(old []byte, ss []server) ([]byte, error) {
		return []byte(mergeCodexTOML(string(old), ss)), nil
	}},
	{"opencode", "opencode", ".config/opencode", ".config/opencode/opencode.json", jsonApply("mcp", func(s server) any {
		return map[string]any{"type": "local", "command": append([]string{s.Command}, s.Args...), "environment": s.Env, "enabled": true}
	})},
	{"gemini", "gemini", ".gemini", ".gemini/settings.json", jsonApply("mcpServers", plainEntry)},
	{"kimi", "kimi", ".kimi", ".kimi/mcp.json", jsonApply("mcpServers", plainEntry)},
}

func plainEntry(s server) any {
	return map[string]any{"command": s.Command, "args": s.Args, "env": s.Env}
}

// jsonApply sets section.<key> for every server, drops stale panda-* keys,
// and leaves every other key alone. Unchanged input is returned byte for byte.
func jsonApply(section string, entry func(server) any) func([]byte, []server) ([]byte, error) {
	return func(old []byte, servers []server) ([]byte, error) {
		root := map[string]any{}
		if len(bytes.TrimSpace(old)) > 0 {
			if err := decodeJSON(old, &root); err != nil {
				return nil, fmt.Errorf("not valid JSON, left alone: %w", err)
			}
		}
		sec := map[string]any{}
		if v, ok := root[section]; ok && v != nil {
			m, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%q is not an object, left alone", section)
			}
			sec = m
		}
		next := map[string]any{}
		for k, v := range sec {
			if !strings.HasPrefix(k, ownedPrefix) {
				next[k] = v
			}
		}
		for _, s := range servers {
			v, err := normalize(entry(s))
			if err != nil {
				return nil, err
			}
			next[s.Key] = v
		}
		if _, had := root[section]; had && reflect.DeepEqual(sec, next) {
			return old, nil
		}
		if len(servers) == 0 && len(sec) == 0 {
			return old, nil // nothing to add, nothing to remove
		}
		updated := make(map[string]any, len(root)+1)
		for k, v := range root {
			updated[k] = v
		}
		updated[section] = next
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(updated); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
}

func decodeJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

// normalize gives an entry the same shapes decodeJSON produces, so
// DeepEqual against what's on disk works.
func normalize(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	return out, decodeJSON(b, &out)
}

// mergeCodexTOML drops every [mcp_servers.panda-*] table (and its subtables)
// and appends freshly rendered ones at the end. Line-based on purpose.
// ponytail: a user multi-line string containing a line that starts with "["
// would confuse the header scan; use a real TOML parser if that ever bites.
func mergeCodexTOML(old string, servers []server) string {
	var kept []string
	owned := false
	for _, line := range strings.Split(old, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			name := strings.TrimSpace(strings.TrimLeft(t, "["))
			owned = strings.HasPrefix(name, "mcp_servers."+ownedPrefix) ||
				strings.HasPrefix(name, `mcp_servers."`+ownedPrefix)
		}
		if !owned {
			kept = append(kept, line)
		}
	}
	rest := strings.TrimRight(strings.Join(kept, "\n"), " \t\r\n")
	var b strings.Builder
	for i, s := range servers {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[mcp_servers.%s]\ncommand = %s\nargs = [", s.Key, tomlString(s.Command))
		for j, a := range s.Args {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteString(tomlString(a))
		}
		b.WriteString("]\n")
		if len(s.Env) > 0 {
			keys := make([]string, 0, len(s.Env))
			for k := range s.Env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteString("env = { ")
			for j, k := range keys {
				if j > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "%s = %s", tomlString(k), tomlString(s.Env[k]))
			}
			b.WriteString(" }\n")
		}
	}
	block := b.String()
	switch {
	case rest == "" && block == "":
		return old
	case rest == "":
		return block
	case block == "":
		return rest + "\n"
	}
	return rest + "\n\n" + block
}

func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// planTarget returns the config path plus its current and desired contents.
func planTarget(home string, t target, servers []server) (path string, old, next []byte, err error) {
	path = filepath.Join(home, t.config)
	if real, e := filepath.EvalSymlinks(path); e == nil {
		path = real // keep dotfile-manager symlinks intact
	}
	old, err = os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return path, nil, nil, err
	}
	next, err = t.apply(old, servers)
	return path, old, next, err
}

// writeAtomic replaces path with data, keeping its mode (0600 if new).
func writeAtomic(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".panda-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
