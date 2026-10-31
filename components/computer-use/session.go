package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

// runSession is labwc's startup command (-s) in headless mode. It waits until
// the output has its configured size, holds an idle virtual pointer and
// keyboard (so the seat never loses those capabilities between tool calls),
// serves the app launcher socket, then writes $XDG_RUNTIME_DIR/ready. It
// returns only on failure; the startup script then stops labwc, so systemd
// restarts the whole session.
func runSession(cfg Config) error {
	d, err := waitOutput(cfg, 20*time.Second)
	if err != nil {
		return err
	}
	if err := d.pointer(); err != nil {
		return fmt.Errorf("virtual pointer: %w", err)
	}
	if err := d.idleKeyboard(cfg.Env["XDG_RUNTIME_DIR"]); err != nil {
		return fmt.Errorf("virtual keyboard: %w", err)
	}
	// Park the cursor in the corner so it never hovers over an app at start.
	if err := errors.Join(d.moveTo(d.out.Width-1, d.out.Height-1), d.sync()); err != nil {
		return err
	}
	sock := cfg.runtimeFile("launch.sock")
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfg.runtimeFile("ready"), []byte(fmt.Sprintf("%dx%d\n", d.out.Width, d.out.Height)), 0o600); err != nil {
		return err
	}
	log.Printf("panda-cu session ready: %s %dx%d", d.out.Name, d.out.Width, d.out.Height)
	lost := make(chan error, 1)
	go func() { // the compositor going away ends the session
		for {
			if err := d.sync(); err != nil {
				lost <- fmt.Errorf("display connection lost: %w", err)
				return
			}
			time.Sleep(5 * time.Second)
		}
	}()
	accepted := make(chan error, 1)
	l := &launcher{cfg: cfg}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				accepted <- err
				return
			}
			go l.serve(c)
		}
	}()
	select {
	case err = <-lost:
	case err = <-accepted:
	}
	return err
}

// waitOutput polls until wlr-randr's custom mode is in effect.
func waitOutput(cfg Config, timeout time.Duration) (*desktop, error) {
	deadline := time.Now().Add(timeout)
	for {
		d, err := openDesktop(cfg.Env)
		if err == nil && (cfg.Width == 0 || (d.out.Width == cfg.Width && d.out.Height == cfg.Height)) {
			return d, nil
		}
		if err == nil {
			err = fmt.Errorf("output is %dx%d, want %dx%d", d.out.Width, d.out.Height, cfg.Width, cfg.Height)
			d.Close()
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// launcher starts allowlisted apps for `panda-cu mcp` and remembers how
// they ended. Protocol: one JSON request line; the reply line is {} or
// {"error"}; for a launch, one more line {appExit} when the app ends (if the
// client still listens). {"op":"exits"} answers {"exits":[...]}.
type launcher struct {
	cfg   Config
	mu    sync.Mutex
	exits []appExit // last 20
}

func (l *launcher) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	enc := json.NewEncoder(c)
	reply := func(err error) {
		res := map[string]string{}
		if err != nil {
			res["error"] = err.Error()
		}
		_ = enc.Encode(res)
	}
	var req launchReq
	if err := json.NewDecoder(c).Decode(&req); err != nil {
		reply(err)
		return
	}
	if req.Op == "exits" {
		l.mu.Lock()
		_ = enc.Encode(map[string]any{"exits": l.exits})
		l.mu.Unlock()
		return
	}
	if !launchArgOK(req.Arg) {
		reply(errors.New("argument must be an absolute path or an http(s) URL"))
		return
	}
	argv, err := appArgv(l.cfg, req.App, req.Arg)
	if err != nil {
		reply(err)
		return
	}
	pid, done, err := startApp(req.App, argv, os.Environ(), os.Stderr, false)
	if err != nil {
		reply(err)
		return
	}
	log.Printf("launched %s (pid %d): %v", req.App, pid, argv)
	reply(nil)
	_ = c.SetDeadline(time.Now().Add(launchWait + 5*time.Second))
	ended := make(chan appExit, 1)
	go func() { // recorded even after the client stopped listening
		e := <-done
		log.Printf("app %s (pid %d) ended: %s", e.App, e.PID, e.Status)
		l.mu.Lock()
		l.exits = append(l.exits, e)
		if len(l.exits) > 20 {
			l.exits = l.exits[1:]
		}
		l.mu.Unlock()
		ended <- e
	}()
	select { // tell the waiting cu_launch if the app ends soon
	case e := <-ended:
		_ = enc.Encode(e)
	case <-time.After(launchWait):
	}
}
