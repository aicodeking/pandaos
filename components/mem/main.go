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
  write -kind K -title T [-tags a,b] [-overwrite] [-body B | stdin]
  append (-path P | -title T) [-body B | stdin]
  delete -path P -confirm
  episode -task T -outcome success|partial|failure [-files a,b] [-body SUMMARY | stdin]
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
	overwrite := fl.Bool("overwrite", false, "write: replace an existing memory with this title")
	confirm := fl.Bool("confirm", false, "delete: really delete")
	path := fl.String("path", "", "append/delete: memory path")
	task := fl.String("task", "", "episode: the task, one line")
	outcome := fl.String("outcome", "", "episode: success|partial|failure")
	files := fl.String("files", "", "episode: comma-separated files touched")
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
	var err error
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
		name, a.Title, a.Overwrite = "mem_write", *title, *overwrite
		if a.Body, err = bodyOrStdin(*body, stdin); err != nil {
			return err
		}
		if *tags != "" {
			a.Tags = strings.Split(*tags, ",")
		}
	case "append":
		name, a.Path, a.Title = "mem_append", *path, *title
		if a.Text, err = bodyOrStdin(*body, stdin); err != nil {
			return err
		}
	case "delete":
		name, a.Path, a.Confirm = "mem_delete", *path, *confirm
	case "episode":
		name, a.Task, a.Outcome = "mem_log_episode", *task, *outcome
		if a.Summary, err = bodyOrStdin(*body, stdin); err != nil {
			return err
		}
		if *files != "" {
			a.Files = strings.Split(*files, ",")
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

func bodyOrStdin(body string, stdin io.Reader) (string, error) {
	if body != "" {
		return body, nil
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxBody+1))
	return string(data), err
}
