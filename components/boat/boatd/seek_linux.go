package main

import (
	"os"
	"syscall"
)

const seekData, seekHole = 3, 4 // SEEK_DATA, SEEK_HOLE

const prSetMemoryMerge = 67

func setMemoryMerge() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetMemoryMerge, 1, 0); e != 0 {
		return e
	}
	return nil
}

// punchHole frees [off, off+n) of f: it reads back as zeros, and dirty page
// cache pages there are dropped instead of written back.
func punchHole(f *os.File, off, n int64) error {
	const punch = 0x02 | 0x01 // FALLOC_FL_PUNCH_HOLE | FALLOC_FL_KEEP_SIZE
	return syscall.Fallocate(int(f.Fd()), punch, off, n)
}
