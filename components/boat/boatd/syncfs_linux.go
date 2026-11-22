package main

import (
	"os"
	"runtime"
	"syscall"
)

// syncFS commits filesystem-wide changes, including btrfs delayed extent frees.
// Directory fsync alone does not flush those frees across the filesystem.
func syncFS(path string) error {
	// The frozen syscall package omits SYS_SYNCFS on amd64. Use Linux ABI
	// numbers for our host architectures; sync all filesystems on other ports.
	var trap uintptr
	switch runtime.GOARCH {
	case "amd64":
		trap = 306
	case "arm64", "riscv64", "loong64":
		trap = 267
	default:
		syscall.Sync()
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		_, _, errno := syscall.Syscall(trap, f.Fd(), 0, 0)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return &os.PathError{Op: "syncfs", Path: path, Err: errno}
		}
		return nil
	}
}
