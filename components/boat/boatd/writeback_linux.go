package main

import (
	"os"
	"syscall"
)

const syncFileRangeWrite = 2 // SYNC_FILE_RANGE_WRITE: start writeback, do not wait for it

// kickWriteback starts writeback of f's dirty pages without waiting for it.
func kickWriteback(f *os.File) error {
	return syscall.SyncFileRange(int(f.Fd()), 0, 0, syncFileRangeWrite)
}
