//go:build !linux

package main

import "syscall"

func noNetwork(a *syscall.SysProcAttr) *syscall.SysProcAttr { return a }
