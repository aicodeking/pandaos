// Command panda-mem is a shared, git-backed markdown memory store for agents,
// served over MCP stdio.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const version = "0.1.0"

const usage = `usage: panda-mem <command> [-dir /mem] [flags]
  mcp                                     serve MCP over stdio
  init                                    create the git repo
  write -kind K -title T [-tags a,b] [-body B | stdin]
  search [-kind K] [-limit N] QUERY
  read PATH
  list [-kind K] [-limit N]
  history [-limit N] [PATH]
`

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "panda-mem:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("missing command\n%s", usage)
	}
	cmd, args := args[0], args[1:]
	fl := flag.NewFlagSet(cmd, flag.ContinueOnError)
	dir := fl.String("dir", "/mem", "memory repo directory")
	kind := fl.String("kind", "", "episodic|semantic|procedural")
	title := fl.String("title", "", "memory title")
	body := fl.String("body", "", "memory body (default: read stdin)")
	tags := fl.String("tags", "", "comma-separated tags")
	limit := fl.Int("limit", 0, "max results")
	if err := fl.Parse(args); err != nil {
		return err
	}
	s := &Store{Dir: *dir}
	arg := strings.Join(fl.Args(), " ")
	var out string
	var err error
	switch cmd {
	case "mcp":
		if err := s.Init(); err != nil {
			return err
		}
		return s.ServeMCP(stdin, stdout)
	case "init":
		return s.Init()
	case "write":
		b := *body
		if b == "" {
			data, rerr := io.ReadAll(io.LimitReader(stdin, maxBody+1))
			if rerr != nil {
				return rerr
			}
			b = string(data)
		}
		var tl []string
		if *tags != "" {
			tl = strings.Split(*tags, ",")
		}
		out, err = s.Write(*kind, *title, b, tl)
	case "search":
		out, err = jsonText(s.Search(arg, *kind, orDefault(*limit, 10)))
	case "read":
		out, err = s.Read(arg)
	case "list":
		out, err = jsonText(s.List(*kind, orDefault(*limit, 50)))
	case "history":
		out, err = s.History(arg, orDefault(*limit, 20))
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, out)
	return err
}
