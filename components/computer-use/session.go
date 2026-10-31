package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// runSession is labwc's autostart in headless mode. It waits until the output
// has its configured size, holds an idle virtual pointer and keyboard (so the
// seat never loses those capabilities between tool calls), serves the app
// launcher socket, then writes $XDG_RUNTIME_DIR/ready.
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
	go func() { // the compositor going away ends the session
		for {
			if err := d.sync(); err != nil {
				log.Fatalf("display connection lost: %v", err)
			}
			time.Sleep(5 * time.Second)
		}
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go serveLaunch(cfg, c)
	}
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

// launchArgOK: the only extra argument is an absolute path or an http(s) URL,
// never a flag.
func launchArgOK(arg string) bool {
	return arg == "" || strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://")
}

func serveLaunch(cfg Config, c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	reply := func(err error) {
		res := map[string]string{}
		if err != nil {
			res["error"] = err.Error()
		}
		_ = json.NewEncoder(c).Encode(res)
	}
	var req launchReq
	if err := json.NewDecoder(c).Decode(&req); err != nil {
		reply(err)
		return
	}
	if !launchArgOK(req.Arg) {
		reply(errors.New("argument must be an absolute path or an http(s) URL"))
		return
	}
	argv, err := appArgv(cfg, req.App, req.Arg)
	if err != nil {
		reply(err)
		return
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr // the journal of panda-cu.service
	cmd.Dir = os.Getenv("HOME")
	if err := cmd.Start(); err != nil {
		reply(err)
		return
	}
	log.Printf("launched %s (pid %d): %v", req.App, cmd.Process.Pid, argv)
	go cmd.Wait() //nolint:errcheck // reap
	reply(nil)
}
