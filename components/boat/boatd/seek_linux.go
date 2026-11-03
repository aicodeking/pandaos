package main

import "syscall"

const seekData, seekHole = 3, 4 // SEEK_DATA, SEEK_HOLE

const prSetMemoryMerge = 67

func setMemoryMerge() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetMemoryMerge, 1, 0); e != 0 {
		return e
	}
	return nil
}
