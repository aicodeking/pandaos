package main

import (
	"os"
	"syscall"
)

func openDirect(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_DIRECT|syscall.O_NOFOLLOW, 0o600)
}
