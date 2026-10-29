package main

import (
	"runtime"
	"syscall"
	"unsafe"
)

const (
	btrfsSuperMagic = 0x9123683E
	btrfsFirstIno   = 256 // inode number of every subvolume root
	renameExchange  = 1 << 1
)

var sysRenameat2 = map[string]uintptr{"amd64": 316, "arm64": 276}[runtime.GOARCH]

func isSubvol(p string) bool {
	var st syscall.Stat_t
	var sf syscall.Statfs_t
	if syscall.Lstat(p, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFDIR || st.Ino != btrfsFirstIno {
		return false
	}
	return syscall.Statfs(p, &sf) == nil && uint32(sf.Type) == btrfsSuperMagic
}

// swap exchanges a and b atomically with renameat2(RENAME_EXCHANGE),
// falling back to two renames where that is unavailable.
func swap(a, b string) error {
	if sysRenameat2 != 0 {
		pa, err := syscall.BytePtrFromString(a)
		if err != nil {
			return err
		}
		pb, err := syscall.BytePtrFromString(b)
		if err != nil {
			return err
		}
		_, _, e := syscall.Syscall6(sysRenameat2, uintptr(atFDCWD), uintptr(unsafe.Pointer(pa)),
			uintptr(atFDCWD), uintptr(unsafe.Pointer(pb)), renameExchange, 0)
		if e == 0 {
			return nil
		}
	}
	return swapRenames(a, b)
}

var atFDCWD = -100 // AT_FDCWD
