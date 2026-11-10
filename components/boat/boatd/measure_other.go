//go:build !linux

package main

import "os"

// openDirect: no O_DIRECT here (macOS has F_NOCACHE); for tests only, boatd runs on Linux.
func openDirect(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}
