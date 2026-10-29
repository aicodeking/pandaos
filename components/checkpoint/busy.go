package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// busyPids lists this user's processes (other than us) whose cwd or an open
// file is dir or below it, read from m.Proc (/proc). Other users' processes
// cannot see into a 0700 home anyway, and their links are unreadable to us.
func (m *Manager) busyPids(dir string) []int {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	ents, err := os.ReadDir(m.Proc)
	if err != nil {
		return nil // no procfs (not Linux): nothing to check
	}
	uid, self := os.Getuid(), os.Getpid()
	var pids []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		base := filepath.Join(m.Proc, e.Name())
		fi, err := os.Lstat(base)
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != uid {
			continue
		}
		links, _ := filepath.Glob(filepath.Join(base, "fd", "*"))
		for _, l := range append([]string{filepath.Join(base, "cwd")}, links...) {
			if t, err := os.Readlink(l); err == nil && (t == dir || strings.HasPrefix(t, dir+"/")) {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}
