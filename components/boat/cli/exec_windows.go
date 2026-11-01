//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
)

// execReplace: Windows cannot replace a process, so run the program as a child
// on this console and pass on its exit code.
func execReplace(argv []string) error {
	c := exec.Command(argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return exitError{ee.ExitCode()}
	}
	return err
}

// replaceSelf: a running .exe cannot be overwritten but can be renamed away. The
// next upgrade removes the .old file.
func replaceSelf(tmp, self string) error {
	old := self + ".old"
	os.Remove(old)
	if err := os.Rename(self, old); err != nil {
		return err
	}
	if err := os.Rename(tmp, self); err != nil {
		os.Rename(old, self) // put it back
		return err
	}
	return nil
}

func openerArgs(u string) (string, []string) {
	return "rundll32", []string{"url.dll,FileProtocolHandler", u}
}
