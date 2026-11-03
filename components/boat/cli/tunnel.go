package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func init() {
	commands["forward"] = cmdForward
	commands["ssh-config"] = cmdSSHConfig
	commands["host"] = cmdHost
}

const (
	forwardTries = 5
	// hostURLTTLS is the API's longest signed URL: `boat host` should outlive a
	// session. boat.dev's token never expires; this one needs a re-run after 7 days.
	hostURLTTLS   = 7 * 24 * 60 * 60
	currentMaxAge = 7 * 24 * time.Hour
)

// Variables so tests need not wait.
var (
	// forwardHold: a tunnel that dies sooner never worked, so it is a startup
	// failure (port in use, no such key) and is not retried.
	forwardHold    = 5 * time.Second
	forwardBackoff = 3 * time.Second
)

// ---- VM aliases: current, self ----

func (a *app) currentFile() string {
	return filepath.Join(filepath.Dir(configPath(a.getenv)), "current", strconv.Itoa(a.shellID))
}

// setCurrent remembers the VM for this shell. The shell is our parent process,
// so no shell function is needed; a script or CI step gets its own, as on boat.dev.
//
// ponytail: keyed by parent PID, so a reused PID could see a stale VM within
// currentMaxAge; store the shell's start time if that ever bites.
func (a *app) setCurrent(id string) {
	p := a.currentFile()
	err := os.MkdirAll(filepath.Dir(p), 0o700)
	if err == nil {
		err = os.WriteFile(p, []byte(id+"\n"), 0o600)
	}
	if err != nil {
		fmt.Fprintln(a.errw, "boat: cannot remember the current VM:", err)
		return
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		return // pruning is housekeeping only
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > currentMaxAge {
			os.Remove(filepath.Join(filepath.Dir(p), e.Name()))
		}
	}
}

// vmAlias turns current and self into something resolveVM understands.
func (a *app) vmAlias(ref string) (string, error) {
	switch ref {
	case "current":
		p := a.currentFile()
		fi, err := os.Stat(p)
		if err != nil || time.Since(fi.ModTime()) > currentMaxAge {
			return "", errors.New("this shell has no current VM: run boat new or boat fork here, or pass an id")
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	case "self":
		if id := a.getenv("BOAT_ID"); id != "" {
			return id, nil
		}
		if !a.inGuest() {
			return "", errors.New("self works only inside a VM")
		}
		// boatd sets the VM's hostname to its name at create and fork.
		return os.Hostname()
	}
	return ref, nil
}

func (a *app) inGuest() bool {
	dir := a.getenv("BOAT_DIR")
	if dir == "" {
		dir = "/etc/boat"
	}
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}

// orCurrent gives ssh, exec and prompt the current VM when no VM is named.
func orCurrent(pos []string, want int) []string {
	if len(pos) == want-1 {
		return append([]string{"current"}, pos...)
	}
	return pos
}

// ---- in-guest `host` ----

// hostArgs maps the in-VM `host` command (a symlink to boat) to boat's.
func hostArgs(args []string) []string {
	if len(args) > 0 {
		rest := args[1:]
		switch args[0] {
		case "list":
			return append([]string{"ports", "ls", "self"}, rest...)
		case "hide":
			return append([]string{"ports", "rm", "self"}, rest...)
		case "url":
			for _, x := range rest {
				if x == "--public" || x == "--private" {
					return append([]string{"host", "self"}, rest...)
				}
			}
			return append([]string{"ports", "url", "self"}, rest...)
		}
	}
	return append([]string{"host", "self"}, args...)
}

// cmdHost exposes a port on an HTTPS URL and prints it.
func cmdHost(a *app, args []string) error {
	fl := flag.NewFlagSet("host", flag.ContinueOnError)
	public := fl.Bool("public", false, "a URL that needs no token")
	private := fl.Bool("private", false, "a URL with a token (default)")
	pos, _, err := parse(fl, args)
	if err != nil {
		return err
	}
	if err := need(pos, 2, "<vm> <port>"); err != nil {
		return err
	}
	if *public && *private {
		return fmt.Errorf("--public and --private conflict: %w", errUsage)
	}
	port, err := portArg("port", pos[1])
	if err != nil {
		return err
	}
	id, err := vmArg(a, pos[0])
	if err != nil {
		return err
	}
	access := "private"
	if *public {
		access = "public"
	}
	if err := a.c.call("POST", vmPath(id, "ports"), obj{"port": port, "auth": access}, nil); err != nil {
		return err
	}
	var u obj
	path := vmPath(id, "ports", url.PathEscape(pos[1]), "url") + "?ttl_s=" + strconv.Itoa(hostURLTTLS)
	if err := a.c.call("GET", path, nil, &u); err != nil {
		return err
	}
	return a.okLine(obj{"sandboxId": id, "port": port, "url": s(u, "url"), "access": access,
		"isProtected": access == "private", "localhost": false, "expires_at": u["expires_at"]}, s(u, "url"))
}

func portArg(name, v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("--%s must be a port number (1-65535): %w", name, errUsage)
	}
	return n, nil
}

// ---- ssh: shared by ssh, forward, ssh-config ----

// sshTarget authorizes the local key on the VM and returns where to connect.
func (a *app) sshTarget(id, ident string) (obj, error) {
	pub, err := a.publicKey(ident)
	if err != nil {
		return nil, err
	}
	var info obj
	err = a.c.call("POST", vmPath(id, "ssh"), obj{"public_key": pub}, &info)
	return info, err
}

// sshArgv builds the ssh command for a target, with extra options before the host.
func sshArgv(id string, info obj, extra ...string) []string {
	// VM ips are reused after a delete and every VM has its own host keys, so
	// known_hosts is keyed by VM id: a new VM on an old ip is not a "changed key".
	argv := []string{"ssh", "-p", s(info, "port"), "-o", "StrictHostKeyChecking=accept-new",
		"-o", "HostKeyAlias=boat-" + id}
	if j := s(info, "jump"); j != "" {
		// accept-new here too: with no tty the inner ssh cannot ask about the jump host.
		argv = append(argv, "-o", "ProxyCommand="+proxyCommand(j))
	}
	argv = append(argv, extra...)
	return append(argv, s(info, "user")+"@"+s(info, "host"))
}

func proxyCommand(jump string) string {
	return "ssh -o StrictHostKeyChecking=accept-new -W %h:%p " + sshJumpArgs(jump)
}

// cmdForward holds a tunnel in the foreground until Ctrl+C (which also ends ssh,
// in the same process group). A lost tunnel reconnects: resume can move the VM.
func cmdForward(a *app, args []string) error {
	fl := flag.NewFlagSet("forward", flag.ContinueOnError)
	remote := fl.Int("remote", 0, "port on the VM")
	local := fl.Int("local", 0, "port on this computer")
	bind := fl.String("bind", "127.0.0.1", "listening address, on the listening side")
	reverse := fl.Bool("reverse", false, "expose a local port inside the VM")
	ident := fl.String("identity", "", "public key file")
	pos, _, err := parse(fl, args)
	if err != nil {
		return err
	}
	if *reverse {
		if *remote == 0 {
			*remote = *local
		}
	} else if *local == 0 {
		*local = *remote
	}
	if _, err := portArg("remote", strconv.Itoa(*remote)); err != nil {
		return err
	}
	if _, err := portArg("local", strconv.Itoa(*local)); err != nil {
		return err
	}
	vm, err := oneVM(a, pos)
	if err != nil {
		return err
	}
	id := s(vm, "id")
	flagName, spec := "-L", fmt.Sprintf("%s:%d:127.0.0.1:%d", *bind, *local, *remote)
	if *reverse {
		flagName, spec = "-R", fmt.Sprintf("%s:%d:127.0.0.1:%d", *bind, *remote, *local)
	}
	opts := []string{"-N", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", flagName, spec}
	if a.json {
		info, err := a.sshTarget(id, *ident)
		if err != nil {
			return err
		}
		info["argv"] = sshArgv(id, info, opts...)
		return a.emit(info, nil)
	}
	fmt.Fprintf(a.errw, "forwarding %s %s on %s; Ctrl+C to stop\n", flagName, spec, id)
	return a.holdTunnel(id, *ident, opts)
}

func (a *app) holdTunnel(id, ident string, opts []string) error {
	lost := 0
	for {
		start := time.Now()
		info, err := a.sshTarget(id, ident)
		if err == nil {
			err = a.runProg(sshArgv(id, info, opts...))
		}
		if err == nil {
			return nil
		}
		if lost == 0 && time.Since(start) < forwardHold {
			return fmt.Errorf("tunnel failed to start: %w", err)
		}
		if time.Since(start) >= forwardHold {
			lost = 0 // it held for a while: this is a new loss, not the same one
		}
		if lost++; lost > forwardTries {
			return fmt.Errorf("tunnel lost, gave up after %d tries: %w", forwardTries, err)
		}
		fmt.Fprintf(a.errw, "boat: tunnel lost (%v); reconnecting (%d/%d)\n", err, lost, forwardTries)
		time.Sleep(forwardBackoff)
	}
}

// cmdSSHConfig writes the entry that docs.boat.dev's boat-ssh-config.sh writes,
// for VS Code, Cursor and anything else that reads ~/.ssh/config.
func cmdSSHConfig(a *app, args []string) error {
	fl := flag.NewFlagSet("ssh-config", flag.ContinueOnError)
	ident := fl.String("identity", "", "public key file")
	toStdout := fl.Bool("print", false, "print the entry instead of writing it")
	pos, _, err := parse(fl, args)
	if err != nil {
		return err
	}
	vm, err := oneVM(a, pos)
	if err != nil {
		return err
	}
	id := s(vm, "id")
	info, err := a.sshTarget(id, *ident)
	if err != nil {
		return err
	}
	names := id
	if n := s(vm, "name"); n != "" && n != id {
		names += " " + n
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Host %s\n  HostName %s\n  Port %s\n  User %s\n", names, s(info, "host"), s(info, "port"), s(info, "user"))
	if *ident != "" {
		fmt.Fprintf(&b, "  IdentityFile %q\n", strings.TrimSuffix(*ident, ".pub"))
	}
	fmt.Fprintf(&b, "  HostKeyAlias boat-%s\n  StrictHostKeyChecking accept-new\n", id)
	if j := s(info, "jump"); j != "" {
		fmt.Fprintf(&b, "  ProxyCommand %s\n", proxyCommand(j))
	}
	if *toStdout {
		return a.okLine(obj{"id": id, "config": b.String()}, strings.TrimRight(b.String(), "\n"))
	}
	dir := filepath.Join(homeDir(a.getenv), ".ssh", "boat")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, filepath.Base(id)+".conf")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return a.emit(obj{"id": id, "path": path, "config": b.String()}, func() {
		fmt.Fprintf(a.out, "Wrote %s\n", path)
		fmt.Fprintln(a.errw, "Add at the top of ~/.ssh/config: Include ~/.ssh/boat/*.conf")
		fmt.Fprintln(a.errw, "Run this again after a resume: the address can change.")
	})
}
