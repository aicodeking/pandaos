// Command panda-cu lets agents use a desktop like a person does: screenshots,
// mouse, keyboard and the AT-SPI accessibility tree, over MCP stdio.
//
//	panda-cu mcp      serve MCP over stdio (run by the agent CLI)
//	panda-cu session  in-session helper (labwc -s startup, headless mode only)
//	panda-cu call TOOL [JSON-ARGS]  run one tool, print one JSON value
//	panda-cu tools    print the tool list
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const version = "0.1.0"

const defaultConfig = "/etc/panda/cu.json"

const usage = `usage: panda-cu {mcp|session|tools|call TOOL [JSON-ARGS]} [-config ` + defaultConfig + `]`

// App is one allowlisted launcher entry (module.nix also installs it as a
// .desktop file). Argv[0] is an absolute store path.
type App struct {
	Name string   `json:"name"`
	Argv []string `json:"argv"`
}

// Config is written by module.nix. A missing file means attach mode with
// the caller's own /run/user/$UID session.
type Config struct {
	Mode   string            `json:"mode"` // "headless" | "attach"
	Width  int               `json:"width"`
	Height int               `json:"height"`
	Scale  float64           `json:"scale"` // output scale: AT-SPI boxes are logical px
	Env    map[string]string `json:"env"`   // session environment for helpers and apps
	Apps   map[string]App    `json:"apps"`
	// Open maps "dir", "file" and "url" to the app id cu_open uses.
	Open map[string]string `json:"open"`
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
	if c.Mode == "" {
		c.Mode = "attach"
	}
	if c.Mode != "attach" && c.Mode != "headless" {
		return c, fmt.Errorf("%s: mode %q (headless|attach)", path, c.Mode)
	}
	if c.Mode == "attach" { // a caller inside the session knows best
		if c.Env == nil {
			c.Env = map[string]string{}
		}
		for _, k := range []string{"XDG_RUNTIME_DIR", "WAYLAND_DISPLAY", "DBUS_SESSION_BUS_ADDRESS"} {
			if v := os.Getenv(k); v != "" && c.Env[k] == "" {
				c.Env[k] = v
			}
		}
	}
	c.Env = sessionEnv(c, os.Getuid())
	return c, nil
}

// sessionEnv fills in the display/bus variables the config leaves out:
// attach mode targets the caller's own login session in /run/user/$UID.
func sessionEnv(c Config, uid int) map[string]string {
	env := map[string]string{}
	for k, v := range c.Env {
		env[k] = v
	}
	if env["XDG_RUNTIME_DIR"] == "" {
		env["XDG_RUNTIME_DIR"] = "/run/user/" + strconv.Itoa(uid)
	}
	if env["WAYLAND_DISPLAY"] == "" {
		env["WAYLAND_DISPLAY"] = "wayland-0"
	}
	if env["DBUS_SESSION_BUS_ADDRESS"] == "" {
		env["DBUS_SESSION_BUS_ADDRESS"] = "unix:path=" + filepath.Join(env["XDG_RUNTIME_DIR"], "bus")
	}
	return env
}

// environ is PATH plus the session environment, for child processes.
func (c Config) environ() []string {
	out := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "LANG=C.UTF-8"}
	for k, v := range c.Env {
		if !strings.ContainsAny(k, "=\x00") {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func (c Config) runtimeFile(name string) string {
	return filepath.Join(c.Env["XDG_RUNTIME_DIR"], name)
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "panda-cu:", err)
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
	case "session":
		return runSession(cfg)
	case "call":
		return callCLI(newServer(cfg), fl.Args(), stdout)
	case "tools":
		_, err := fmt.Fprintln(stdout, toolsJSON)
		return err
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}
