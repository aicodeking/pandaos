package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

const jobUnitPattern = "panda-job-*.service"

// systemctlFunc runs systemctl and returns its stdout. Injected so tests can fake it.
type systemctlFunc func(ctx context.Context, args ...string) ([]byte, error)

func execSystemctl(bin string) systemctlFunc {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, bin, args...).Output()
	}
}

// cleanupUnits stops and resets job units left over from a previous agentd
// (crash, SIGKILL) so their ids can run again, and returns those job ids.
func cleanupUnits(ctx context.Context, sc systemctlFunc) ([]string, error) {
	out, err := sc(ctx, "list-units", "--all", "--plain", "--no-legend", "--no-pager", jobUnitPattern)
	if err != nil {
		return nil, fmt.Errorf("list job units: %w", err)
	}
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		id, ok1 := strings.CutPrefix(f[0], "panda-job-")
		id, ok2 := strings.CutSuffix(id, ".service")
		if ok1 && ok2 && jobIDRe.MatchString(id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	_, err1 := sc(ctx, "stop", jobUnitPattern)
	_, err2 := sc(ctx, "reset-failed", jobUnitPattern)
	return ids, errors.Join(err1, err2)
}

// freeMB returns the space available to unprivileged users on path's filesystem, in MiB.
func freeMB(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize) >> 20), nil
}

func (d *daemon) checkDisk() error {
	if d.cfg.minFreeMB <= 0 {
		return nil
	}
	free, err := d.freeMB(d.cfg.stateDir)
	if err != nil {
		return fmt.Errorf("check free space on %s: %w", d.cfg.stateDir, err)
	}
	if free < d.cfg.minFreeMB {
		return fmt.Errorf("low disk space: %d MiB free on %s, need %d MiB (-min-free-mb)", free, d.cfg.stateDir, d.cfg.minFreeMB)
	}
	return nil
}
