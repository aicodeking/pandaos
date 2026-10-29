package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"slices"
	"strconv"
	"time"
)

const (
	maxOutput   = 64 << 10 // per exec, stdout+stderr
	execTimeout = 10 * time.Second
)

// Execer runs one argv (never a shell) and returns combined output, capped
// at maxOutput. Tests swap in a fake.
type Execer func(ctx context.Context, argv []string) ([]byte, error)

// capWriter keeps the first max bytes and drops the rest.
type capWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	if room := w.max - w.buf.Len(); room < n {
		w.truncated = true
		p = p[:max(room, 0)]
	}
	w.buf.Write(p)
	return n, nil // claim all of it so the child neither blocks nor gets SIGPIPE
}

func realExec(ctx context.Context, argv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "SYSTEMD_COLORS=0", "SYSTEMD_PAGER="}
	cmd.WaitDelay = 2 * time.Second
	w := &capWriter{max: maxOutput}
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	out := w.buf.Bytes()
	if w.truncated {
		out = append(out, "\n[output truncated]\n"...)
	}
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	return out, err
}

// run executes argv with a time limit.
func (s *Server) run(timeout time.Duration, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := s.exec(ctx, argv)
	return string(out), err
}

// callerInGroup: root, or a process whose real groups include name.
func callerInGroup(name string) bool {
	if os.Geteuid() == 0 {
		return true
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		return false
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return false
	}
	ids, err := os.Getgroups() // the process's real groups, not /etc/group
	return err == nil && (os.Getegid() == gid || slices.Contains(ids, gid))
}
