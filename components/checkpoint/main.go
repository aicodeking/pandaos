// Command panda-ckpt keeps instant copy-on-write btrfs checkpoints of agent
// workspaces (~/work/<name>), runs unprivileged as the workspace owner, and
// serves the same operations over MCP stdio.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const version = "0.1.0"

const usage = `usage: panda-ckpt <command> [args] [--json]
  init <name>                         create ~/work/<name> as a subvolume
  adopt <path>                        convert the plain directory ~/work/<name> in place
  create <ws> [-m label]              read-only checkpoint
  list [<ws>]                         checkpoints (or workspaces)
  diff <ws> <id> [<id2>|current]      changed files, unified diff for small text files
  restore <ws> <id>                   safety checkpoint, then atomic swap back to <id>
  fork <ws> <id> <newname>            new writable workspace from <id>
  prune <ws> [--keep N] [--older-than D]   delete beyond newest N and older than D (e.g. 7d, 12h)
  gc                                  remove leftovers of interrupted operations
  auto [--keep-auto 24]               checkpoint changed workspaces (timer), keep newest N auto
  mcp                                 serve MCP over stdio
`

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "panda-ckpt:", err)
		os.Exit(1)
	}
}

// parseInterleaved lets flags follow positionals ("create ws -m x").
func parseInterleaved(fl *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fl.Parse(args); err != nil {
			return nil, err
		}
		if fl.NArg() == 0 {
			return pos, nil
		}
		pos, args = append(pos, fl.Arg(0)), fl.Args()[1:]
	}
}

// parseAge accepts Go durations plus a "d" (days) suffix.
func parseAge(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		d, err := strconv.Atoi(n)
		if err != nil || d < 0 {
			return 0, fmt.Errorf("bad --older-than %q", s)
		}
		return time.Duration(d) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad --older-than %q", s)
	}
	return d, nil
}

func nargs(cmd string, pos []string, min, max int) error {
	if len(pos) < min || len(pos) > max {
		return fmt.Errorf("%s: wrong number of arguments\n%s", cmd, usage)
	}
	return nil
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("missing command\n%s", usage)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return runWith(NewManager(home), args, stdin, stdout)
}

func runWith(m *Manager, args []string, stdin io.Reader, stdout io.Writer) error {
	cmd, args := args[0], args[1:]
	fl := flag.NewFlagSet(cmd, flag.ContinueOnError)
	asJSON := fl.Bool("json", false, "machine-readable output")
	label := fl.String("m", "", "checkpoint label")
	keep := fl.Int("keep", 0, "prune: keep the newest N")
	older := fl.String("older-than", "", "prune: only delete checkpoints older than D")
	keepAuto := fl.Int("keep-auto", 24, "auto: auto checkpoints kept per workspace")
	pos, err := parseInterleaved(fl, args)
	if err != nil {
		return err
	}
	arg := func(i int) string {
		if i < len(pos) {
			return pos[i]
		}
		return ""
	}

	var v any
	switch cmd {
	case "mcp":
		return m.ServeMCP(stdin, stdout)
	case "init":
		if err = nargs(cmd, pos, 1, 1); err == nil {
			v, err = m.Init(arg(0))
		}
	case "adopt":
		if err = nargs(cmd, pos, 1, 1); err == nil {
			v, err = m.Adopt(arg(0))
		}
	case "create":
		if err = nargs(cmd, pos, 1, 1); err == nil {
			v, err = m.Create(arg(0), *label)
		}
	case "list":
		if err = nargs(cmd, pos, 0, 1); err == nil && len(pos) == 0 {
			v, err = m.Workspaces()
		} else if err == nil {
			v, err = m.List(arg(0))
		}
	case "diff":
		if err = nargs(cmd, pos, 2, 3); err == nil {
			v, err = m.Diff(arg(0), arg(1), arg(2))
		}
	case "restore":
		if err = nargs(cmd, pos, 2, 2); err == nil {
			v, err = m.Restore(arg(0), arg(1))
		}
	case "fork":
		if err = nargs(cmd, pos, 3, 3); err == nil {
			v, err = m.Fork(arg(0), arg(1), arg(2))
		}
	case "prune":
		var age time.Duration
		if err = nargs(cmd, pos, 1, 1); err == nil {
			if age, err = parseAge(*older); err == nil {
				v, err = m.Prune(arg(0), *keep, age)
			}
		}
	case "gc":
		if err = nargs(cmd, pos, 0, 0); err == nil {
			v, err = m.GC()
		}
	case "auto":
		if *keepAuto < 1 {
			return fmt.Errorf("--keep-auto must be >= 1")
		}
		if err = nargs(cmd, pos, 0, 0); err == nil {
			v, err = m.Auto(*keepAuto)
		}
	case "version":
		v = version
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}
	// auto still reports the other workspaces when one of them fails.
	if _, partial := v.([]AutoResult); v != nil && (err == nil || partial) {
		if perr := printOut(stdout, v, *asJSON); perr != nil && err == nil {
			err = perr
		}
	}
	return err
}

func printOut(w io.Writer, v any, asJSON bool) error {
	if asJSON {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	}
	var b strings.Builder
	switch x := v.(type) {
	case Workspace:
		fmt.Fprintf(&b, "%s\t%s\n", x.Name, x.Path)
	case []Workspace:
		for _, ws := range x {
			fmt.Fprintf(&b, "%s\t%s\n", ws.Name, ws.Path)
		}
	case Meta:
		fmt.Fprintln(&b, x.ID)
	case []Meta:
		for _, c := range x {
			fmt.Fprintf(&b, "%s\t%s\t%-11s\t%d\t%s\n", c.ID, c.Created.Local().Format(time.DateTime), c.Reason, c.SizeEstimate, c.Label)
		}
	case []Change:
		for _, c := range x {
			fmt.Fprintf(&b, "%-8s %s\n", c.Kind, c.Path)
		}
		for _, c := range x {
			b.WriteString(c.Diff)
		}
	case RestoreResult:
		fmt.Fprintf(&b, "restored %s to %s (safety checkpoint %s)\n", x.Workspace, x.Restored, x.Safety)
		if x.Warning != "" {
			fmt.Fprintln(&b, "warning:", x.Warning)
		}
	case []AutoResult:
		for _, r := range x {
			fmt.Fprintf(&b, "%s\tcreated=%s pruned=%d %s%s\n", r.Workspace, r.Created, len(r.Pruned), r.Skipped, r.Error)
		}
	case []string:
		for _, s := range x {
			fmt.Fprintln(&b, s)
		}
	default:
		fmt.Fprintln(&b, x)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
