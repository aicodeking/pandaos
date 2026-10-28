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

const usage = `usage: panda-mem <command> [-dir /mem | -socket /run/panda-mem/sock] [flags]
  serve  -dir D -socket S                 single-writer daemon (owns the repo)
  mcp                                     serve MCP over stdio (proxies to -socket if set)
  init                                    create the git repo
  write -kind K -title T [-tags a,b] [-body B | stdin]
  search [-kind K] [-limit N] QUERY
  read PATH
  list [-kind K] [-limit N]
  history [-limit N] [PATH]
With neither -dir nor -socket, the client commands use ` + defaultSocket + ` if it exists.
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
	dir := fl.String("dir", "/mem", "memory repo directory (direct mode)")
	sock := fl.String("socket", "", "panda-mem serve socket (client mode)")
	kind := fl.String("kind", "", "episodic|semantic|procedural")
	title := fl.String("title", "", "memory title")
	body := fl.String("body", "", "memory body (default: read stdin)")
	tags := fl.String("tags", "", "comma-separated tags")
	limit := fl.Int("limit", 0, "max results")
	if err := fl.Parse(args); err != nil {
		return err
	}
	explicit := map[string]bool{}
	fl.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if !explicit["dir"] && !explicit["socket"] && cmd != "serve" && cmd != "init" {
		if _, err := os.Stat(defaultSocket); err == nil {
			*sock = defaultSocket
		}
	}
	s := &Store{Dir: *dir}
	arg := strings.Join(fl.Args(), " ")

	var name string
	a := toolArgs{Kind: *kind, Limit: *limit}
	switch cmd {
	case "serve":
		if *sock == "" {
			*sock = defaultSocket
		}
		return s.Serve(*sock)
	case "mcp":
		if *sock != "" {
			return proxy(*sock, stdin, stdout)
		}
		if err := s.Init(); err != nil {
			return err
		}
		if err := s.checkRepo(); err != nil {
			return err
		}
		return s.ServeMCP(stdin, stdout)
	case "init":
		return s.Init()
	case "write":
		name, a.Title, a.Body = "mem_write", *title, *body
		if a.Body == "" {
			data, err := io.ReadAll(io.LimitReader(stdin, maxBody+1))
			if err != nil {
				return err
			}
			a.Body = string(data)
		}
		if *tags != "" {
			a.Tags = strings.Split(*tags, ",")
		}
	case "search":
		name, a.Query = "mem_search", arg
	case "read":
		name, a.Path = "mem_read", arg
	case "list":
		name = "mem_list"
	case "history":
		name, a.Path = "mem_history", arg
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}
	var out string
	var err error
	if *sock != "" {
		out, err = remoteTool(*sock, name, a)
	} else {
		out, err = s.runTool(name, a)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, out)
	return err
}
