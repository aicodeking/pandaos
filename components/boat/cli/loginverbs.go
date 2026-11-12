package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// boat.dev's account verbs: login [key] / --key-stdin, logout, rename,
// api-key, completions, self-update (= upgrade).

func init() {
	commands["rename"] = cmdRename
	commands["api-key"] = cmdAPIKey
	commands["__vms"] = cmdCompleteVMs
}

// normalizeKey takes a bare token or the JSON that boat-api admin init (or
// teams create / add-member) prints, and returns the token.
func normalizeKey(raw string) (string, error) {
	k := strings.TrimSpace(raw)
	if !strings.HasPrefix(k, "{") {
		return k, nil
	}
	var v struct {
		Token string `json:"token"`
		Key   struct {
			Token string `json:"token"`
		} `json:"key"`
	}
	if err := json.Unmarshal([]byte(k), &v); err != nil {
		return "", fmt.Errorf("the key looks like JSON but does not parse: %v", err)
	}
	if t := orStr(v.Token, v.Key.Token); t != "" {
		return t, nil
	}
	return "", errors.New(`the JSON has no "token": give the boat_... value`)
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (a *app) login(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	u := fs.String("url", "", "API base URL (default: the saved one, or BOAT_URL)")
	k := fs.String("key", "", "API key: boat_..., the JSON boat-api admin init prints, or - for stdin")
	fromStdin := fs.Bool("key-stdin", false, "read the key from standard input")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 || (len(pos) == 1 && (*k != "" || *fromStdin)) || (*k != "" && *fromStdin) {
		return fmt.Errorf("give the key once: [key], --key or --key-stdin: %w", errUsage)
	}
	if len(pos) == 1 {
		*k = pos[0]
	}
	if *fromStdin || *k == "-" {
		b, err := io.ReadAll(io.LimitReader(a.in, 4096))
		if err != nil {
			return err
		}
		*k = string(b)
	}
	key, err := normalizeKey(*k)
	if err != nil {
		return err
	}
	if *u == "" {
		cfg, _ := loadConfig(a.getenv)
		*u = cfg.URL
	}
	if *u == "" || key == "" {
		return fmt.Errorf("login needs --url and a key: %w", errUsage)
	}
	base := apiOrigin(*u)
	c := &client{base: base, key: key, hc: &http.Client{Timeout: 30 * time.Second}}
	var me obj
	if err := c.call("GET", "/v1/me", nil, &me); err != nil {
		return err
	}
	if err := saveConfig(a.getenv, Config{URL: base, Key: key}); err != nil {
		return err
	}
	team, _ := me["team"].(map[string]any)
	return a.emit(obj{"ok": true, "url": base, "team": team, "role": me["role"]}, func() {
		fmt.Fprintf(a.out, "Logged in to %s (team %v, role %v)\n", base, team["name"], me["role"])
	})
}

// logout clears the saved key and keeps the URL. BOAT_API_KEY still applies.
func (a *app) logout(args []string) error {
	if _, _, err := parse(flag.NewFlagSet("logout", flag.ContinueOnError), args); err != nil {
		return err
	}
	p := configPath(a.getenv)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return a.emit(obj{"ok": true}, func() { fmt.Fprintln(a.out, "Logged out") })
	}
	if err != nil {
		return err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	if err := saveConfig(a.getenv, Config{URL: c.URL}); err != nil {
		return err
	}
	return a.emit(obj{"ok": true}, func() {
		fmt.Fprintln(a.out, "Logged out")
		if a.getenv("BOAT_API_KEY") != "" {
			fmt.Fprintln(a.errw, "BOAT_API_KEY is still set in this shell")
		}
	})
}

func cmdRename(a *app, args []string) error {
	pos, _, err := parse(flag.NewFlagSet("rename", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if err := need(pos, 2, "VM NAME"); err != nil {
		return err
	}
	vm, err := a.c.resolveVM(pos[0])
	if err != nil {
		return err
	}
	var out obj
	if err := a.c.call("PATCH", vmPath(s(vm, "id")), obj{"name": pos[1]}, &out); err != nil {
		return err
	}
	return a.emit(out, func() { fmt.Fprintf(a.out, "%s is now %q\n", s(out, "id"), s(out, "name")) })
}

// cmdAPIKey is boat.dev's api-key: create goes through /api/v1 (presets and
// boat.dev's refusals of --ttl/--sandbox/--env), the rest is boat keys.
func cmdAPIKey(a *app, args []string) error {
	op, args := sub(args, "", "create", "list", "ls", "usage", "rotate", "revoke")
	switch op {
	case "list", "ls":
		return cmdKeys(a, append([]string{"ls"}, args...))
	case "usage":
		return cmdKeys(a, append([]string{"usage"}, args...))
	case "rotate": // boat.dev: the old secret stops working at once
		return cmdKeys(a, append([]string{"rotate", "--overlap", "0"}, args...))
	case "revoke":
		return cmdKeys(a, append([]string{"rm"}, args...))
	case "":
		return fmt.Errorf("api-key create|list|usage|rotate|revoke: %w", errUsage)
	}
	fs := flag.NewFlagSet("api-key create", flag.ContinueOnError)
	ttl := fs.String("ttl", "", "keys never expire here: omit, or never")
	preset := fs.String("preset", "", "read-only, full-sandbox, ci or admin")
	actions := fs.String("actions", "", "comma-separated scopes (not with --preset)")
	var sandboxes, envs multi
	fs.Var(&sandboxes, "sandbox", "refused: keys cannot be limited to sandboxes here")
	fs.Var(&envs, "env", "refused: keys cannot be limited to environments here")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "NAME"); err != nil {
		return err
	}
	if *preset != "" && *actions != "" {
		return fmt.Errorf("--preset and --actions do not go together: %w", errUsage)
	}
	body := omitNil(obj{"name": pos[0], "preset": *preset, "ttl": *ttl, "actions": splitList(*actions),
		"sandboxIds": []string(sandboxes), "environmentIds": []string(envs)})
	for _, k := range []string{"preset", "ttl"} {
		if body[k] == "" {
			delete(body, k)
		}
	}
	var out obj
	if err := a.c.call("POST", "/api/v1/api-keys/scoped", body, &out); err != nil {
		return err
	}
	key, _ := out["apiKey"].(map[string]any)
	return a.emit(out, func() {
		fmt.Fprintf(a.out, "%s\n(key %v; shown once, store it now)\n", s(out, "secret"), key["id"])
	})
}

// cmdCompleteVMs prints VM ids and names for the completion scripts.
func cmdCompleteVMs(a *app, args []string) error {
	var list struct{ VMs []obj }
	if err := a.c.call("GET", "/v1/vms", nil, &list); err != nil {
		return err
	}
	for _, v := range list.VMs {
		fmt.Fprintln(a.out, s(v, "id"))
		if n := s(v, "name"); n != "" {
			fmt.Fprintln(a.out, n)
		}
	}
	return nil
}

// ponytail: no 15 s result cache like boat.dev's; each tab asks the API.
var completionScripts = map[string]string{
	"bash": `_boat() {
  local cur=${COMP_WORDS[COMP_CWORD]}
  if [ "$COMP_CWORD" -eq 1 ]; then
    COMPREPLY=($(compgen -W "@CMDS@" -- "$cur"))
  else
    COMPREPLY=($(compgen -W "$(boat __vms 2>/dev/null) current self" -- "$cur"))
  fi
}
complete -o default -F _boat boat
`,
	"zsh": `#compdef boat
_boat() {
  if (( CURRENT == 2 )); then
    compadd -- @CMDS@
  else
    compadd -- $(boat __vms 2>/dev/null) current self
    _files
  fi
}
compdef _boat boat
`,
	"fish": `complete -c boat -f -n __fish_use_subcommand -a "@CMDS@"
complete -c boat -n 'not __fish_use_subcommand' -a "(boat __vms 2>/dev/null) current self"
`,
	"powershell": `Register-ArgumentCompleter -Native -CommandName boat -ScriptBlock {
  param($word, $ast, $cursor)
  $n = $ast.CommandElements.Count
  if ($word) { $n-- }
  if ($n -le 1) { $items = '@CMDS@' -split ' ' } else { $items = @(boat __vms 2>$null) + 'current', 'self' }
  $items | Where-Object { $_ -like "$word*" } | ForEach-Object { [System.Management.Automation.CompletionResult]::new($_) }
}
`,
}

func (a *app) completions(args []string) error {
	pos, _, err := parse(flag.NewFlagSet("completions", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || completionScripts[pos[0]] == "" {
		return fmt.Errorf("completions bash|zsh|fish|powershell: %w", errUsage)
	}
	names := []string{"login", "logout", "upgrade", "self-update", "version", "completions", "help"}
	for k := range commands {
		if !strings.HasPrefix(k, "__") {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	_, err = io.WriteString(a.out, strings.ReplaceAll(completionScripts[pos[0]], "@CMDS@", strings.Join(names, " ")))
	return err
}
