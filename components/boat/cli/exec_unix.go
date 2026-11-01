//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// execReplace replaces this process, so the terminal belongs to ssh.
func execReplace(argv []string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, os.Environ())
}

// replaceSelf swaps the running binary. A running file can be renamed over here.
func replaceSelf(tmp, self string) error { return os.Rename(tmp, self) }

func openerArgs(u string) (string, []string) {
	if isDarwin() {
		return "open", []string{u}
	}
	return "xdg-open", []string{u}
}
