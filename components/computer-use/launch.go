package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

const launchWait = 20 * time.Second

// launchReq is the one-line JSON request `panda-cu session` accepts.
type launchReq struct {
	Op  string `json:"op,omitempty"` // "" = launch, "exits" = recent exits
	App string `json:"app,omitempty"`
	Arg string `json:"arg,omitempty"`
}

// appExit is how a launched app ended.
type appExit struct {
	App    string `json:"app"`
	PID    int    `json:"pid"`
	Status string `json:"status"` // "exit 0", "exit 1", "signal: killed"
	OK     bool   `json:"ok"`
	Stderr string `json:"stderr,omitempty"` // last lines
	At     string `json:"at"`
}

func appArgv(cfg Config, app, arg string) ([]string, error) {
	a, ok := cfg.Apps[app]
	if !ok || len(a.Argv) == 0 {
		ids := make([]string, 0, len(cfg.Apps))
		for id := range cfg.Apps {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		return nil, fmt.Errorf("app %q is not allowlisted (have: %s)", app, strings.Join(ids, ", "))
	}
	argv := slices.Clone(a.Argv)
	if arg != "" {
		argv = append(argv, arg)
	}
	return argv, nil
}

// launchArgOK: the only extra argument is an absolute path or an http(s) URL,
// never a flag.
func launchArgOK(arg string) bool {
	return arg == "" || strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://")
}

// tailBuf keeps the last max bytes written to it.
type tailBuf struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

// startApp starts argv and reports its end on the returned channel (once).
// stderr goes to errOut too (the journal, for the session).
func startApp(app string, argv, env []string, errOut io.Writer, setsid bool) (int, <-chan appExit, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	if home := os.Getenv("HOME"); home != "" {
		if fi, err := os.Stat(home); err == nil && fi.IsDir() {
			cmd.Dir = home
		}
	}
	tail := &tailBuf{max: 1500}
	cmd.Stderr = tail
	if errOut != nil {
		cmd.Stdout, cmd.Stderr = errOut, io.MultiWriter(errOut, tail)
	}
	if setsid {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	done := make(chan appExit, 1)
	go func() {
		err := cmd.Wait()
		e := appExit{App: app, PID: cmd.Process.Pid, Status: "exit 0", OK: err == nil, Stderr: tail.String(), At: time.Now().Format(time.TimeOnly)}
		if err != nil {
			e.Status = err.Error()
			if ee, ok := err.(*exec.ExitError); ok {
				e.Status = ee.ProcessState.String()
			}
		}
		done <- e
	}()
	return cmd.Process.Pid, done, nil
}

// launch starts an app inside the session: through `panda-cu session`'s
// socket in headless mode (apps then live in panda-cu.service, under its
// limits, not in the agent's sandbox), else (attach) as a detached child of
// this process. The channel reports the app's exit.
func (s *Server) launch(app, arg string) (<-chan appExit, error) {
	argv, err := appArgv(s.cfg, app, arg)
	if err != nil {
		return nil, err
	}
	if s.cfg.Mode != "headless" {
		_, done, err := startApp(app, argv, s.cfg.environ(), nil, true)
		return done, err
	}
	c, err := net.DialTimeout("unix", s.cfg.runtimeFile("launch.sock"), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("session launcher: %w", err)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(c).Encode(launchReq{App: app, Arg: arg}); err != nil {
		c.Close()
		return nil, err
	}
	r := bufio.NewReader(c)
	var res struct {
		Error string `json:"error"`
	}
	line, err := r.ReadBytes('\n')
	if err == nil {
		err = json.Unmarshal(line, &res)
	}
	if err != nil || res.Error != "" {
		c.Close()
		if err == nil {
			err = errors.New(res.Error)
		}
		return nil, fmt.Errorf("session launcher: %w", err)
	}
	done := make(chan appExit, 1)
	go func() { // the launcher writes one exit line when the app ends
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(launchWait + 5*time.Second))
		var e appExit
		if l, err := r.ReadBytes('\n'); err == nil && json.Unmarshal(l, &e) == nil && e.Status != "" {
			done <- e
		}
	}()
	return done, nil
}

// recentExits asks the session launcher which apps ended lately.
func (s *Server) recentExits() []appExit {
	if s.cfg.Mode != "headless" {
		return nil
	}
	c, err := net.DialTimeout("unix", s.cfg.runtimeFile("launch.sock"), time.Second)
	if err != nil {
		return nil
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	var res struct {
		Exits []appExit `json:"exits"`
	}
	if json.NewEncoder(c).Encode(launchReq{Op: "exits"}) != nil || json.NewDecoder(c).Decode(&res) != nil {
		return nil
	}
	return res.Exits
}

func exitErr(e appExit) error {
	msg := fmt.Sprintf("%s exited (%s) before showing a window", e.App, e.Status)
	if e.Stderr != "" {
		msg += "; its last output: " + e.Stderr
	}
	return errors.New(msg)
}

// launchTool starts app and waits for a window that was not there before
// (by compositor handle). An app that dies first is an error with its exit
// status and last output; one that exits 0 may have handed off to a running
// instance (single-instance apps), so the wait goes on until the deadline.
func (s *Server) launchTool(app, arg string) (any, error) {
	if _, err := appArgv(s.cfg, app, arg); err != nil {
		return nil, err
	}
	t, err := s.openWindows()
	if err != nil {
		return nil, err
	}
	defer t.d.Close()
	before := t.list()
	done, err := s.launch(app, arg)
	if err != nil {
		return nil, err
	}
	var exited *appExit
	stop := func() error {
		select {
		case e := <-done:
			exited = &e
			if !e.OK {
				return exitErr(e)
			}
		default:
		}
		return nil
	}
	w, err := t.waitNew(before, time.Now().Add(launchWait), stop)
	if err != nil {
		return nil, err
	}
	if w != nil {
		return map[string]any{"launched": app, "window": w, "windows": len(t.list())}, nil
	}
	if exited != nil {
		return nil, fmt.Errorf("%s exited (%s) and no new window appeared within %s", app, exited.Status, launchWait)
	}
	return map[string]any{"launched": app, "window": nil, "note": "still running but no new window within 20 s; check cu_windows"}, nil
}

func (s *Server) open(a toolArgs) (any, error) {
	switch {
	case a.URL != "" && a.Path != "":
		return nil, errors.New("give path or url, not both")
	case a.URL != "":
		u, err := url.Parse(a.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, errors.New("url must be http(s)://host/...")
		}
		return s.launchTool(s.cfg.Open["url"], u.String())
	case a.Path != "":
		p := a.Path
		if strings.HasPrefix(p, "~/") {
			p = filepath.Join(os.Getenv("HOME"), p[2:])
		}
		if !filepath.IsAbs(p) {
			return nil, errors.New("path must be absolute (or ~/...)")
		}
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		kind := "file"
		if fi.IsDir() {
			kind = "dir"
		}
		return s.launchTool(s.cfg.Open[kind], filepath.Clean(p))
	}
	return nil, errors.New("give path or url")
}
