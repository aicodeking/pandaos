// Command panda-cu lets agents use a desktop like a person does: screenshots,
// mouse, keyboard and the AT-SPI accessibility tree, over MCP stdio.
//
//	panda-cu mcp      serve MCP over stdio (run by the agent CLI)
//	panda-cu session  in-session helper (labwc -s startup, headless mode only)
//	panda-cu call TOOL [JSON-ARGS]  run one tool, print one JSON value
//	panda-cu revoke [MINUTES]   stop computer use in this session (0: allow again)
//	panda-cu indicator [click]  waybar badge (JSON lines); click toggles revoke
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
	"time"
)

const version = "0.1.0"

const defaultConfig = "/etc/panda/cu.json"

const usage = `usage: panda-cu {mcp|session|tools|call TOOL [JSON-ARGS]|revoke [MINUTES]|indicator [click]} [-config ` + defaultConfig + `]`

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
	// AllowClipboardRead lets cu_clipboard_get read the person's clipboard
	// in attach mode (headless: always, it is the agent's own).
	AllowClipboardRead bool `json:"allowClipboardRead"`
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
	case "revoke":
		d := defaultRevoked
		if fl.NArg() > 0 {
			m, err := strconv.Atoi(fl.Arg(0))
			if err != nil || m < 0 || m > 24*60 {
				return fmt.Errorf("revoke: minutes must be 0..1440, got %q", fl.Arg(0))
			}
			d = time.Duration(m) * time.Minute
		}
		n, err := revoke(cfg, d)
		if err == nil && d == 0 {
			_, err = fmt.Fprintln(stdout, "computer use allowed again")
		} else if err == nil {
			_, err = fmt.Fprintf(stdout, "computer use stopped for %s; %d panda-cu processes ended\n", d, n)
		}
		return err
	case "indicator":
		if fl.Arg(0) == "click" {
			d := defaultRevoked
			if time.Now().Before(cfg.revokedUntil()) {
				d = 0
			}
			_, err := revoke(cfg, d)
			return err
		}
		return indicator(cfg, stdout)
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}
