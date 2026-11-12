package main

import "syscall"

// noNetwork: a new, empty network namespace (only lo, down).
func noNetwork(a *syscall.SysProcAttr) *syscall.SysProcAttr {
	a.Cloneflags |= syscall.CLONE_NEWNET
	return a
}
