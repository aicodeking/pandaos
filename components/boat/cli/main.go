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
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"text/tabwriter"
)

// version is set at release build time with -ldflags "-X main.version=vX.Y.Z".
var version = "0.1.0"

const usage = `boat - persistent Ubuntu VMs for AI agents

  boat login --url URL [KEY | --key KEY | --key-stdin]   save credentials (~/.config/boat/config.json);
                                       KEY may be the JSON boat-api admin init prints     boat logout
  boat whoami | images | usage
  boat new [--name N] [--size|--type S] [--image I] [--ttl 2h] [--idle-stop 30m|never] [--snapshot SID] [--label k=v] [-e K=V]
                [--secrets A,B | --no-secrets] [--env NAME | --no-env] [--from SNAPSHOT|CODE] [--count N] [--fail-fast] [--wait-setup]
  boat run [--from S] [--type T] [--push DIR] [--cwd D] [--timeout SECS] [-e K=V] [--environment E] [--keep] [--no-env] -- cmd [args]
  boat push <vm> <dir> [--to PATH]     copy a directory (git-tracked and new files)
  boat list | info <vm> | ps <vm> | vm-usage <vm>
  boat ssh <vm> [-- ssh-args]          boat scp <src> <dst>   (vm:/path on one side)
  boat ssh-config <vm> [--print]       write ~/.ssh/boat/<vm>.conf for VS Code, Cursor, ...
  boat forward <vm> --remote P [--local P] [--bind A]    boat forward <vm> --reverse --local P [--remote P] [--bind A]
  boat exec <vm> [--detach] -- cmd [args]
  boat cmd start|ls|status|logs [-f]|signal [--signal S]|stop|history <vm> [id] [-- cmd]
  boat files ls|rm [-r]|mv [--overwrite]|search [--content] [--regex]|info|mkdir|chmod <vm> ...
  boat prompt <vm> "text" [--agent|--provider claude|codex|opencode|pi|prime|kimi|mistral] [--model M] [--async] [--new] [--resume CONV]
  boat prompts ls|status|logs [-f] <vm> [id]    boat conversations <vm>
  boat steer <vm> "text" [--prompt-id ID]       boat interrupt <vm> [--prompt-id ID]
  boat env ls | new NAME | show E | set E | default E | build E | rm E   [--image I] [--size S] [--setup TEXT|--setup-file F] [--var K=V] [--secrets A,B]
  boat rename <vm> NAME                boat api-key create NAME [--preset read-only|full-sandbox|ci|admin | --actions a,b] | list | usage ID | rotate ID | revoke ID
  boat fork <vm> [--name N] [--fail-fast]   boat stop <vm> [--force]   boat rm|delete <vm>
  boat extend <vm> --hours N | --ttl SECS | --no-auto-stop | --by 2h   auto-stop: set the time left, or push it out
  boat limits                          sandboxes active and whether one can start
  boat history <vm> [command-id] [-f] [--limit N]    commands of a VM, newest first, or one's output
  boat snapshot <vm> NAME | snapshot rm NAME          save (replaces NAME) or remove a named snapshot
  boat webhook create URL [--name N] [--event ready|error|archived|...] | list | rotate ID | remove ID   (boat.dev events)
  boat start|resume <vm> [--type S] [--fail-fast]
  boat resize <vm> [SIZE] [--disk GIB] [--discard-memory]
  boat desktop <vm> [--no-open]
  boat ports ls|add|rm|url <vm> [port] [--auth private|public] [--public] [--ttl 1h]
  boat host <vm> <port> [--public|--private]   expose a port and print its URL
  boat snapshots <vm> [ls | create [--name N] | rm <id> | download <id> [-o F] | share <id> [--team T] [--ttl 24h] | shares <id> | unshare <id> <share>]
  boat snapshots team | shared | import FILE [--name N] [--image I] [--size S] | take --share ID | take --code CODE | delete <id>
  boat ai credentials [ls | set PROVIDER [KEY|-] | rm PROVIDER] | proxy [on|off] | usage [vm] | block <vm> | unblock <vm>
  boat telemetry [show | set ENDPOINT [-H k=v] | unset]
  boat secrets ls | set NAME [VALUE|-] [--no-inject] | rotate NAME [VALUE|-] | rm NAME
  boat keys [ls | create [--name N] [--scopes a,b] [--team T] | rm ID | rotate ID [--overlap 24h] | usage ID]
  boat teams [ls | create NAME [--owner U] | show ID | rm ID | add-member TEAM NAME [--role R] | rm-member TEAM USER]
  boat upgrade|self-update [--version vX.Y.Z]   replace this binary with a release build (checksum-verified)
  boat completions bash|zsh|fish|powershell     print a shell completion script
  boat webhooks [ls | create URL [--events a,b] | show ID | update ID [--url U] [--events a,b] | rm ID | deliveries ID | test ID | rotate ID [--overlap 24h]]

A <vm> can be an id, a unique name, current (last created or forked in this shell) or self (inside a VM).
Inside a VM: host <port> [--public] | host list | host url <port> | host hide <port>.
Global: --json (JSON lines on stdout, errors as JSON on stderr). Env: BOAT_URL, BOAT_API_KEY, BOAT_CONFIG.`

type app struct {
	out, errw io.Writer
	in        io.Reader
	getenv    func(string) string
	json      bool
	cfg       Config
	c         *client
	execProg  func(argv []string) error // replaces the process (ssh)
	runProg   func(argv []string) error // runs and waits (forward)
	shellID   int                       // parent process: scopes the current VM
	open      func(url string) error
	exe       func() (string, error) // os.Executable; replaced in tests
}

func main() {
	a := &app{out: os.Stdout, errw: os.Stderr, in: os.Stdin, getenv: os.Getenv, execProg: execReplace,
		runProg: runForeground, shellID: os.Getppid(), open: openURL}
	args := os.Args[1:]
	if filepath.Base(os.Args[0]) == "host" { // the symlink in the guest image
		args = hostArgs(args)
	}
	os.Exit(a.run(args))
}

func isDarwin() bool { return runtime.GOOS == "darwin" }

func runForeground(argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

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
	switch cmd {
	case "upgrade", "self-update":
		return a.upgrade(args)
	case "logout":
		return a.logout(args)
	case "completions":
		return a.completions(args)
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
		if a.inGuest() {
			return errors.New("this VM has no API key: boat-api injects BOAT_URL and a key for this VM only when the operator sets -vm-api-url (or -public-url); otherwise, on your computer, store a key as team secrets (boat keys create --scopes vms:read,vms:write,exec; boat secrets set BOAT_API_KEY; boat secrets set BOAT_URL) and restart the VM")
		}
		return errors.New("not logged in: run boat login --url URL --key KEY")
	}
	a.cfg = cfg
	a.c = &client{base: cfg.URL, key: cfg.Key, hc: &http.Client{}, alias: a.vmAlias}
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
