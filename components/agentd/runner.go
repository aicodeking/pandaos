package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

const (
	maxOutput      = 64 << 10
	defaultTimeout = time.Hour
	jobUser        = "panda-agent"
)

// Runner executes one job. Injected so tests can fake it.
type Runner func(ctx context.Context, j Job) Result

func defaultRunner() (Runner, string) {
	if runtime.GOOS == "linux" {
		if bin, err := exec.LookPath("systemd-run"); err == nil {
			return systemdRunner(bin), "systemd-run"
		}
	}
	return execRunner, "exec"
}

func jobTimeout(j Job) time.Duration {
	if j.TimeoutSec <= 0 {
		return defaultTimeout
	}
	return time.Duration(j.TimeoutSec) * time.Second
}

func systemdRunner(bin string) Runner {
	return func(ctx context.Context, j Job) Result {
		t := jobTimeout(j)
		args := []string{"--wait", "--collect", "--pipe", "--quiet"}
		if j.MemoryMaxMB > 0 {
			args = append(args, "-p", fmt.Sprintf("MemoryMax=%dM", j.MemoryMaxMB))
		}
		args = append(args, "-p", fmt.Sprintf("RuntimeMaxSec=%d", int(t.Seconds())), "--uid="+jobUser, "--")
		// systemd enforces RuntimeMaxSec; the context deadline is only a backstop.
		// ponytail: killing systemd-run may leave the unit running until RuntimeMaxSec; stop it via `systemctl stop` if that matters.
		return run(ctx, t+30*time.Second, bin, append(args, j.Argv...))
	}
}

func execRunner(ctx context.Context, j Job) Result {
	return run(ctx, jobTimeout(j), j.Argv[0], j.Argv[1:])
}

func run(ctx context.Context, timeout time.Duration, name string, args []string) Result {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var stdout, stderr capWriter
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()

	code := 0
	var ee *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		code = 124
		stderr.Write([]byte("\npanda-agentd: job timed out\n"))
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		code = -1
		stderr.Write([]byte("\npanda-agentd: " + err.Error() + "\n"))
	}
	return Result{ExitCode: code, Stdout: string(stdout.b), Stderr: string(stderr.b)}
}

// capWriter keeps the first maxOutput bytes and drops the rest, so a chatty
// job cannot balloon daemon memory.
type capWriter struct{ b []byte }

func (w *capWriter) Write(p []byte) (int, error) {
	if room := maxOutput - len(w.b); room > 0 {
		w.b = append(w.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func truncate(s string) string {
	if len(s) > maxOutput {
		return s[:maxOutput]
	}
	return s
}
