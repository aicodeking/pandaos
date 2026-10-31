package main

// The person's controls over attach mode, where an agent drives their own
// screen: every tool call marks the session active (the waybar badge from
// `panda-cu indicator` shows it), and `panda-cu revoke` (Super+Escape)
// stops running panda-cu processes and refuses new calls for a while.
// These are a stop button, not a sandbox: an agent running as the same user
// could remove the mark. A private agent desktop is headless mode.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	activeFor      = 5 * time.Second // the badge stays on this long after a call
	defaultRevoked = 10 * time.Minute
)

func (c Config) guardFile(name string) string {
	return filepath.Join(c.Env["XDG_RUNTIME_DIR"], "panda-cu-guard", name)
}

// revokedUntil is when the person's stop ends (zero: not stopped).
func (c Config) revokedUntil() time.Time {
	b, err := os.ReadFile(c.guardFile("revoked-until"))
	if err != nil {
		return time.Time{}
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// guard runs before every tool call in attach mode.
func (s *Server) guard() error {
	if s.cfg.Mode != "attach" {
		return nil
	}
	if until := s.cfg.revokedUntil(); time.Now().Before(until) {
		return fmt.Errorf("refused: the person at this screen stopped computer use (Super+Escape) until %s", until.Format(time.TimeOnly))
	}
	f := s.cfg.guardFile("active")
	if !filepath.IsAbs(f) {
		return nil // no session runtime dir: nothing to mark
	}
	if os.MkdirAll(filepath.Dir(f), 0o700) == nil {
		_ = os.WriteFile(f, nil, 0o600) // mtime = last call
	}
	return nil
}

// revoke stops computer use for d: it records the end time and terminates
// this user's other `panda-cu mcp|call` processes.
func revoke(cfg Config, d time.Duration) (int, error) {
	f := cfg.guardFile("revoked-until")
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return 0, err
	}
	until := time.Now().Add(d).Unix()
	if d <= 0 {
		until = 0
	}
	if err := os.WriteFile(f, []byte(strconv.FormatInt(until, 10)+"\n"), 0o600); err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, nil
	}
	return killCU("/proc", os.Getuid(), os.Getpid()), nil
}

// killCU sends SIGTERM to uid's panda-cu processes running mcp or call.
func killCU(proc string, uid, self int) int {
	ents, _ := os.ReadDir(proc)
	n := 0
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		fi, err := os.Stat(filepath.Join(proc, e.Name()))
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != uid {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(proc, e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if isCUAction(bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0})) {
			if syscall.Kill(pid, syscall.SIGTERM) == nil {
				n++
			}
		}
	}
	return n
}

func isCUAction(argv [][]byte) bool {
	if len(argv) < 2 {
		return false
	}
	base := filepath.Base(string(argv[0]))
	cmd := string(argv[1])
	return (base == "panda-cu" || base == ".panda-cu-wrapped") && (cmd == "mcp" || cmd == "call")
}

// indicatorState is the waybar custom module JSON.
func indicatorState(cfg Config, now time.Time) map[string]string {
	if until := cfg.revokedUntil(); now.Before(until) {
		return map[string]string{"text": "agent stopped", "class": "revoked",
			"tooltip": "Computer use is stopped until " + until.Format(time.TimeOnly) + ". Click to allow it again."}
	}
	if fi, err := os.Stat(cfg.guardFile("active")); err == nil && now.Sub(fi.ModTime()) < activeFor {
		return map[string]string{"text": "AGENT", "class": "active",
			"tooltip": "An agent is using this screen. Super+Escape (or click) stops it for 10 minutes."}
	}
	return map[string]string{"text": "", "class": "idle"}
}

// indicator prints one JSON line per change, for waybar's exec (no interval).
func indicator(cfg Config, w io.Writer) error {
	last := ""
	for {
		b, _ := json.Marshal(indicatorState(cfg, time.Now()))
		if string(b) != last {
			if _, err := fmt.Fprintln(w, string(b)); err != nil {
				return err
			}
			last = string(b)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
