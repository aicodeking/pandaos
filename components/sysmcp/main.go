// Command panda-sys is a read-mostly MCP stdio server that lets agents
// inspect (and, when allowed, update) the PandaOS box they run on.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
)

const version = "0.1.0"

const (
	defaultConfig = "/etc/panda/sys.json"
	defaultURL    = "https://cache.nixos.org/"
)

const usage = `usage: panda-sys mcp [-config ` + defaultConfig + `]
  mcp     serve MCP over stdio
  tools   print the tool list
`

// Config is written by module.nix. Missing file = all defaults (mutations off).
type Config struct {
	AllowMutations  bool   `json:"allowMutations"`
	ConnectivityURL string `json:"connectivityUrl"`
}

func loadConfig(path string) (Config, error) {
	c := Config{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return c, err
	}
	if err == nil {
		if err := json.Unmarshal(data, &c); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
	}
	if c.ConnectivityURL == "" {
		c.ConnectivityURL = defaultURL
	}
	if err := checkURL(c.ConnectivityURL); err != nil {
		return c, fmt.Errorf("%s: connectivityUrl: %w", path, err)
	}
	return c, nil
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "panda-sys:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("missing command\n%s", usage)
	}
	cmd, args := args[0], args[1:]
	fl := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cfgPath := fl.String("config", defaultConfig, "config file")
	if err := fl.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	switch cmd {
	case "mcp":
		return newServer(cfg).ServeMCP(stdin, stdout)
	case "tools":
		_, err := fmt.Fprintln(stdout, toolsJSON)
		return err
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}
