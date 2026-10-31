package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	maxOutput   = 32 << 20 // per exec: a PNG can be a few MB
	execTimeout = 10 * time.Second
)

// Execer runs one argv (never a shell) with optional stdin and returns
// stdout; stderr goes into the error. Tests swap in a fake.
type Execer func(ctx context.Context, env []string, stdin string, argv []string) ([]byte, error)

type capWriter struct {
	buf bytes.Buffer
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room < len(p) {
		w.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return w.buf.Write(p)
}

func realExec(ctx context.Context, env []string, stdin string, argv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.WaitDelay = 2 * time.Second
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, errb := &capWriter{max: maxOutput}, &capWriter{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = out, errb
	err := cmd.Run()
	// wl-copy forks a clipboard server that keeps our pipes open: the
	// command itself succeeded, so stop waiting for its child's output.
	// ponytail: costs WaitDelay (2 s) per wl-copy; give wl-copy /dev/null pipes if that matters.
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		err = nil
	}
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	if err != nil {
		return out.buf.Bytes(), fmt.Errorf("%s: %v: %s", argv[0], err, strings.TrimSpace(errb.buf.String()))
	}
	return out.buf.Bytes(), nil
}

func (s *Server) run(timeout time.Duration, stdin string, argv ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.exec(ctx, s.cfg.environ(), stdin, argv)
}
