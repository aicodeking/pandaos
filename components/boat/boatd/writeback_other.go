//go:build !linux

package main

import "os"

// kickWriteback: Linux only (sync_file_range); boatd runs on Linux.
func kickWriteback(f *os.File) error { return nil }
