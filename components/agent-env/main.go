// panda-agents wires the box's agent CLIs to PandaOS: it merges the MCP
// registry (/etc/panda/mcp.d/*.json) into each installed CLI's config, links
// the system brief (/etc/panda/AGENTS.md), and reports what is set up.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const usage = `usage:
  panda-agents mcp list
  panda-agents mcp sync [--user U]   merge the MCP registry into agent CLI configs
  panda-agents sync [--user U]       mcp sync, plus link AGENTS.md
  panda-agents doctor [--user U]
`

// Overridable for tests.
var (
	registryDir = envOr("PANDA_MCP_DIR", "/etc/panda/mcp.d")
	agentsMD    = envOr("PANDA_AGENTS_MD", "/etc/panda/AGENTS.md")
	memSocket   = envOr("PANDA_MEM_SOCKET", "/run/panda-mem/sock")
)

// claudeInclude is Claude Code's import syntax for another memory file.
var claudeInclude = "@" + agentsMD

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	cmd := strings.Join(args[:min(2, len(args))], " ")
	switch {
	case cmd == "mcp list":
		return mcpList(stdout, stderr)
	case cmd == "mcp sync":
		return withHome(args[2:], stderr, func(home string) int { return syncAll(home, false, stdout, stderr) })
	case len(args) > 0 && args[0] == "sync":
		return withHome(args[1:], stderr, func(home string) int { return syncAll(home, true, stdout, stderr) })
	case len(args) > 0 && args[0] == "doctor":
		return withHome(args[1:], stderr, func(home string) int { return doctor(home, stdout) })
	}
	fmt.Fprint(stderr, usage)
	return 2
}

// withHome parses --user and becomes that user when run as root, so files
// in their home are written with their rights (no root writes through
// symlinks they control).
func withHome(args []string, stderr io.Writer, f func(home string) int) int {
	fs := flag.NewFlagSet("panda-agents", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("user", "", "user whose agent configs to sync (default: you)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(stderr, "panda-agents:", err)
			return 1
		}
		return f(home)
	}
	u, err := user.Lookup(*name)
	if err != nil {
		fmt.Fprintln(stderr, "panda-agents:", err)
		return 1
	}
	if err := becomeUser(u); err != nil {
		fmt.Fprintln(stderr, "panda-agents:", err)
		return 1
	}
	os.Setenv("HOME", u.HomeDir)
	return f(u.HomeDir)
}

func becomeUser(u *user.User) error {
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if os.Geteuid() == uid {
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("--user %s needs root (or run it as %s)", u.Username, u.Username)
	}
	groups := []int{gid}
	if ids, err := u.GroupIds(); err == nil {
		for _, g := range ids {
			if n, err := strconv.Atoi(g); err == nil {
				groups = append(groups, n)
			}
		}
	}
	if err := syscall.Setgroups(groups); err != nil {
		return err
	}
	if err := syscall.Setgid(gid); err != nil {
		return err
	}
	return syscall.Setuid(uid)
}

func mcpList(stdout, stderr io.Writer) int {
	servers, err := loadRegistry(registryDir)
	for _, s := range servers {
		fmt.Fprintf(stdout, "%s\t%s %s\n", s.Key, s.Command, strings.Join(s.Args, " "))
	}
	if err != nil {
		fmt.Fprintln(stderr, "panda-agents:", err)
		return 1
	}
	return 0
}

func installed(home string, t target) bool {
	if _, err := exec.LookPath(t.bin); err == nil {
		return true
	}
	_, err := os.Stat(filepath.Join(home, t.marker))
	return err == nil
}

func syncAll(home string, withContext bool, stdout, stderr io.Writer) int {
	rc := 0
	servers, err := loadRegistry(registryDir)
	if err != nil {
		fmt.Fprintln(stderr, "panda-agents:", err)
		rc = 1
	}
	for _, t := range targets {
		if !installed(home, t) {
			fmt.Fprintf(stdout, "skip %s: not installed\n", t.name)
			continue
		}
		path, old, next, err := planTarget(home, t, servers)
		if err == nil && !bytes.Equal(old, next) {
			err = writeAtomic(path, next)
			if err == nil {
				fmt.Fprintf(stdout, "%s: updated %s (%d panda servers)\n", t.name, path, len(servers))
			}
		} else if err == nil {
			fmt.Fprintf(stdout, "%s: %s already in sync\n", t.name, path)
		}
		if err != nil {
			fmt.Fprintf(stderr, "panda-agents: %s: %s: %v\n", t.name, path, err)
			rc = 1
		}
	}
	if withContext {
		if err := linkContext(home, stdout); err != nil {
			fmt.Fprintln(stderr, "panda-agents:", err)
			rc = 1
		}
	}
	return rc
}

// linkContext links AGENTS.md where agents look for it, never replacing a
// user's own file, and adds an include line to Claude's user CLAUDE.md.
func linkContext(home string, stdout io.Writer) error {
	if _, err := os.Stat(agentsMD); err != nil {
		return err
	}
	links := []string{filepath.Join(home, "AGENTS.md")}
	for _, t := range targets {
		if t.name == "codex" && installed(home, t) {
			links = append(links, filepath.Join(home, ".codex", "AGENTS.md"))
		}
	}
	for _, l := range links {
		if err := symlinkIfFree(l, agentsMD, stdout); err != nil {
			return err
		}
	}
	if installed(home, targets[0]) {
		return ensureLine(filepath.Join(home, ".claude", "CLAUDE.md"), claudeInclude, stdout)
	}
	return nil
}

func symlinkIfFree(link, dest string, stdout io.Writer) error {
	if cur, err := os.Readlink(link); err == nil && cur == dest {
		return nil
	}
	if _, err := os.Lstat(link); err == nil {
		fmt.Fprintf(stdout, "leave %s alone: it is yours (add a pointer to %s yourself)\n", link, dest)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "linked %s -> %s\n", link, dest)
	return os.Symlink(dest, link)
}

func ensureLine(path, line string, stdout io.Writer) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if hasLine(data, line) {
		return nil
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	fmt.Fprintf(stdout, "added %q to %s\n", line, path)
	return writeAtomic(path, append(data, line+"\n"...))
}

func hasLine(data []byte, line string) bool {
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

// doctor prints the state of the agent setup. Exit 1 if something that
// should work does not (bad registry, stale MCP config, /mem unreachable).
func doctor(home string, stdout io.Writer) int {
	rc := 0
	bad := func(format string, a ...any) {
		rc = 1
		fmt.Fprintf(stdout, "FAIL "+format+"\n", a...)
	}
	servers, err := loadRegistry(registryDir)
	if err != nil {
		bad("registry: %v", err)
	} else {
		fmt.Fprintf(stdout, "ok   registry %s: %d servers\n", registryDir, len(servers))
	}
	for _, t := range targets {
		if !installed(home, t) {
			fmt.Fprintf(stdout, "--   %s: not installed\n", t.name)
			continue
		}
		path, old, next, err := planTarget(home, t, servers)
		switch {
		case err != nil:
			bad("%s (%s): %s: %v", t.name, version(t.bin), path, err)
		case !bytes.Equal(old, next):
			bad("%s (%s): MCP not synced in %s; run: panda-agents mcp sync", t.name, version(t.bin), path)
		default:
			fmt.Fprintf(stdout, "ok   %s (%s): MCP synced in %s\n", t.name, version(t.bin), path)
		}
	}
	if c, err := net.DialTimeout("unix", memSocket, 2*time.Second); err != nil {
		bad("/mem: %s unreachable: %v", memSocket, err)
	} else {
		c.Close()
		fmt.Fprintf(stdout, "ok   /mem: %s reachable\n", memSocket)
	}
	// Checkpoints are a warning only: the tool is optional on older images.
	if p, err := exec.LookPath("panda-ckpt"); err == nil {
		fmt.Fprintf(stdout, "ok   checkpoints: %s\n", p)
	} else if st, err := os.Stat("/.snapshots"); err == nil && st.IsDir() {
		fmt.Fprintln(stdout, "warn checkpoints: no panda-ckpt; hourly snapshots in /.snapshots only")
	} else {
		fmt.Fprintln(stdout, "warn checkpoints: no panda-ckpt and no /.snapshots")
	}
	if cur, err := os.Readlink(filepath.Join(home, "AGENTS.md")); err == nil && cur == agentsMD {
		fmt.Fprintf(stdout, "ok   ~/AGENTS.md -> %s\n", agentsMD)
	} else {
		fmt.Fprintf(stdout, "warn ~/AGENTS.md is not linked to %s; run: panda-agents sync\n", agentsMD)
	}
	return rc
}

func version(bin string) string {
	p, err := exec.LookPath(bin)
	if err != nil {
		return "config only, not on PATH"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, p, "--version").Output()
	if err != nil {
		return "version unknown"
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}
