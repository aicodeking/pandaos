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
	// Per-job memory cap when the job does not set one (percent of RAM).
	defaultMemoryMax = "80%"
	jobUser          = "panda-agent"
)

// Runner executes one job in dir, its private working directory. Injected so
// tests can fake it.
type Runner func(ctx context.Context, j Job, dir string) Result

// defaultRunner prefers systemd-run. sandboxed reports whether jobs run as
// jobUser, in which case their workdirs must be chowned to it.
func defaultRunner(cfg config) (r Runner, kind string, sandboxed bool) {
	if runtime.GOOS == "linux" {
		bin, err1 := exec.LookPath("systemd-run")
		ctl, err2 := exec.LookPath("systemctl")
		if err1 == nil && err2 == nil {
			return systemdRunner(bin, ctl, cfg), "systemd-run", true
		}
	}
	return execRunner, "exec", false
}

func jobTimeout(j Job) time.Duration {
	if j.TimeoutSec <= 0 {
		return defaultTimeout
	}
	return time.Duration(j.TimeoutSec) * time.Second
}

func systemdArgs(cfg config, j Job, dir string) []string {
	mem := defaultMemoryMax
	if j.MemoryMaxMB > 0 {
		mem = fmt.Sprintf("%dM", j.MemoryMaxMB)
	}
	args := []string{
		"--wait", "--collect", "--pipe", "--quiet",
		"--unit=" + jobUnit(j), "--uid=" + jobUser,
		"--working-directory=" + dir, "--setenv=HOME=" + dir,
		"-p", "PrivateTmp=yes",
		"-p", "ProtectSystem=strict",
		"-p", "ProtectHome=read-only",
		"-p", "NoNewPrivileges=yes",
		"-p", "ReadWritePaths=" + dir,
		"-p", "MemoryMax=" + mem,
		"-p", "TasksMax=512",
		"-p", fmt.Sprintf("RuntimeMaxSec=%d", int(jobTimeout(j).Seconds())),
		"-p", "TimeoutStopSec=10",
	}
	if cfg.cpuQuota != "" {
		args = append(args, "-p", "CPUQuota="+cfg.cpuQuota)
	}
	for _, p := range cfg.jobRWPaths {
		args = append(args, "-p", "ReadWritePaths=-"+p) // "-": skip if missing
	}
	return append(append(args, "--"), j.Argv...)
}

func jobUnit(j Job) string { return "panda-job-" + j.ID + ".service" }

func systemdRunner(bin, systemctl string, cfg config) Runner {
	return func(ctx context.Context, j Job, dir string) Result {
		// Killing systemd-run does not stop the unit, so cancels and timeouts
		// stop it explicitly. systemd enforces RuntimeMaxSec; the context
		// deadline is only a backstop.
		stop := func() { exec.Command(systemctl, "stop", "--no-block", jobUnit(j)).Run() }
		return run(ctx, jobTimeout(j)+30*time.Second, bin, systemdArgs(cfg, j, dir), "", stop)
	}
}

func execRunner(ctx context.Context, j Job, dir string) Result {
	return run(ctx, jobTimeout(j), j.Argv[0], j.Argv[1:], dir, nil)
}

// run executes name in dir; onCancel, if set, runs before the process is
// killed when ctx ends.
func run(ctx context.Context, timeout time.Duration, name string, args []string, dir string, onCancel func()) Result {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var stdout, stderr capWriter
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second
	if onCancel != nil {
		cmd.Cancel = func() error { onCancel(); return cmd.Process.Kill() }
	}
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
