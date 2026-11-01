// Command boat is the Boat CLI. Every command accepts --json for stable JSONL.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// version is set at release build time with -ldflags "-X main.version=vX.Y.Z".
var version = "0.1.0"

const usage = `boat - persistent Ubuntu VMs for AI agents

  boat login --url URL --key KEY       save credentials (~/.config/boat/config.json)
  boat whoami | images | usage
  boat new [--name N] [--size S] [--image I] [--ttl 2h] [--idle-stop 30m|never] [--snapshot SID] [--label k=v]
                [--secrets A,B | --no-secrets] [--env NAME | --no-env] [--from CODE] [--wait-setup]
  boat list | info <vm> | ps <vm> | vm-usage <vm>
  boat ssh <vm> [-- ssh-args]          boat scp <src> <dst>   (vm:/path on one side)
  boat exec <vm> [--detach] -- cmd [args]
  boat cmd start|ls|status|logs [-f]|signal [--signal S]|stop|history <vm> [id] [-- cmd]
  boat files ls|rm [-r]|mv [--overwrite]|search [--content] [--regex]|info|mkdir|chmod <vm> ...
  boat prompt <vm> "text" [--agent claude|codex|opencode] [--model M] [--async] [--new] [--resume CONV]
  boat prompts ls|status|logs [-f] <vm> [id]    boat conversations <vm>
  boat steer <vm> "text" [--prompt-id ID]       boat interrupt <vm> [--prompt-id ID]
  boat env ls | new NAME | show E | set E | default E | build E | rm E   [--image I] [--size S] [--setup TEXT|--setup-file F] [--var K=V] [--secrets A,B]
  boat fork <vm> [--name N]            boat stop|start|rm|delete <vm>   boat resume <vm> [--type S]
  boat resize <vm> [SIZE] [--disk GIB] [--discard-memory]
  boat desktop <vm> [--no-open]
  boat ports ls|add|rm|url <vm> [port] [--auth private|public] [--public] [--ttl 1h]
  boat snapshots <vm> [ls | create [--name N] | rm <id> | download <id> [-o F] | share <id> [--team T] [--ttl 24h] | shares <id> | unshare <id> <share>]
  boat snapshots team | shared | import FILE [--name N] [--image I] [--size S] | take --share ID | take --code CODE | delete <id>
  boat ai credentials [ls | set PROVIDER [KEY|-] | rm PROVIDER] | proxy [on|off] | usage [vm] | block <vm> | unblock <vm>
  boat telemetry [show | set ENDPOINT [-H k=v] | unset]
  boat secrets ls | set NAME [VALUE|-] | rotate NAME [VALUE|-] | rm NAME
  boat keys [ls | create [--name N] [--scopes a,b] [--team T] | rm ID | rotate ID [--overlap 24h] | usage ID]
  boat teams [ls | create NAME [--owner U] | show ID | rm ID | add-member TEAM NAME [--role R] | rm-member TEAM USER]
  boat upgrade [--version vX.Y.Z]       replace this binary with a release build (checksum-verified)
  boat webhooks [ls | create URL [--events a,b] | show ID | update ID [--url U] [--events a,b] | rm ID | deliveries ID | test ID | rotate ID [--overlap 24h]]

Global: --json (JSON lines on stdout, errors as JSON on stderr). Env: BOAT_URL, BOAT_API_KEY, BOAT_CONFIG.`

type app struct {
	out, errw io.Writer
	in        io.Reader
	getenv    func(string) string
	json      bool
	cfg       Config
	c         *client
	execProg  func(argv []string) error // replaces the process (ssh)
	open      func(url string) error
	exe       func() (string, error) // os.Executable; replaced in tests
}

func main() {
	a := &app{out: os.Stdout, errw: os.Stderr, in: os.Stdin, getenv: os.Getenv, execProg: execReplace, open: openURL}
	os.Exit(a.run(os.Args[1:]))
}

func isDarwin() bool { return runtime.GOOS == "darwin" }

func openURL(u string) error {
	name, args := openerArgs(u)
	return exec.Command(name, args...).Start()
}

// exitError carries a remote exit status (exec/prompt) or a usage error (2).
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

var errUsage = errors.New("usage")

func (a *app) run(args []string) int {
	// --json is global and may appear anywhere before "--".
	var rest []string
	for i, x := range args {
		if x == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if x == "--json" || x == "-json" {
			a.json = true
			continue
		}
		rest = append(rest, x)
	}
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "-h" || rest[0] == "--help" {
		fmt.Fprintln(a.out, usage)
		return 0
	}
	err := a.dispatch(rest[0], rest[1:])
	var ee exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.code
	case errors.Is(err, errUsage):
		fmt.Fprintln(a.errw, usage)
		return 2
	}
	if a.json {
		e := map[string]any{"message": err.Error()}
		var ae *apiError
		if errors.As(err, &ae) {
			e = map[string]any{"status": ae.Status, "code": ae.Code, "message": ae.Message, "request_id": ae.RequestID}
		}
		json.NewEncoder(a.errw).Encode(map[string]any{"error": e})
	} else {
		fmt.Fprintln(a.errw, "boat:", err)
	}
	return 1
}

func (a *app) dispatch(cmd string, args []string) error {
	if cmd == "login" {
		return a.login(args)
	}
	if cmd == "upgrade" {
		return a.upgrade(args)
	}
	if cmd == "version" {
		return a.emit(obj{"version": version}, func() { fmt.Fprintln(a.out, "boat", version) })
	}
	h, ok := commands[cmd]
	if !ok {
		return fmt.Errorf("unknown command %q (see boat help): %w", cmd, errUsage)
	}
	cfg, err := loadConfig(a.getenv)
	if err != nil {
		return err
	}
	if cfg.URL == "" || cfg.Key == "" {
		return errors.New("not logged in: run boat login --url URL --key KEY")
	}
	a.cfg = cfg
	a.c = &client{base: cfg.URL, key: cfg.Key, hc: &http.Client{}}
	return h(a, args)
}

// parse handles flags interspersed with positional args. Everything after the
// first "--" is returned as is (flag.Parse would swallow the "--").
func parse(fs *flag.FlagSet, args []string) ([]string, []string, error) {
	fs.SetOutput(io.Discard)
	var after []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, after = args[:i], args[i+1:]
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, nil, fmt.Errorf("%v: %w", err, errUsage)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, after, nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

func (a *app) emit(v any, text func()) error {
	if a.json {
		return json.NewEncoder(a.out).Encode(v)
	}
	text()
	return nil
}

// emitList writes one JSON line per item, or a table.
func (a *app) emitList(items []obj, cols ...string) error {
	if a.json {
		enc := json.NewEncoder(a.out)
		for _, it := range items {
			if err := enc.Encode(it); err != nil {
				return err
			}
		}
		return nil
	}
	tw := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, strings.ToUpper(strings.Join(cols, "\t")))
	for _, it := range items {
		row := make([]string, len(cols))
		for i, c := range cols {
			row[i] = s(it, c)
		}
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	return tw.Flush()
}

func (a *app) login(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	u := fs.String("url", "", "API base URL")
	k := fs.String("key", "", "API key (boat_...)")
	if _, _, err := parse(fs, args); err != nil {
		return err
	}
	if *u == "" || *k == "" {
		return fmt.Errorf("login needs --url and --key: %w", errUsage)
	}
	if *k == "-" {
		b, err := io.ReadAll(io.LimitReader(a.in, 4096))
		if err != nil {
			return err
		}
		*k = strings.TrimSpace(string(b))
	}
	c := &client{base: *u, key: *k, hc: &http.Client{Timeout: 30 * time.Second}}
	var me obj
	if err := c.call("GET", "/v1/me", nil, &me); err != nil {
		return err
	}
	if err := saveConfig(a.getenv, Config{URL: strings.TrimRight(*u, "/"), Key: *k}); err != nil {
		return err
	}
	team, _ := me["team"].(map[string]any)
	return a.emit(obj{"ok": true, "url": strings.TrimRight(*u, "/"), "team": team, "role": me["role"]}, func() {
		fmt.Fprintf(a.out, "Logged in to %s (team %v, role %v)\n", *u, team["name"], me["role"])
	})
}

func need(pos []string, n int, what string) error {
	if len(pos) != n {
		return fmt.Errorf("expected %s: %w", what, errUsage)
	}
	return nil
}

func splitList(v string) []string {
	var out []string
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func sub(args []string, def string, allowed ...string) (string, []string) {
	if len(args) > 0 && slices.Contains(allowed, args[0]) {
		return args[0], args[1:]
	}
	return def, args
}
