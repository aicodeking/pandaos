package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"syscall"
	"unsafe"
)

const (
	ficloneIoctl    = 0x40049409
	ficloneRange    = 0x4020940d // FICLONERANGE: _IOW(0x94, 13, struct file_clone_range)
	atFdcwd         = -100
	atSymlinkFollow = 0x400
	oPath           = 0x200000

	btrfsIocQuotaCtl       = 0xc0109428
	btrfsQuotaEnableSimple = 4
)

func ficlone(dst, src *os.File) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), ficloneIoctl, src.Fd()); e != 0 {
		return e
	}
	return nil
}

// cloneRange reflinks src's [off, off+n) to the same offset in dst.
func cloneRange(dst, src *os.File, off, n int64) error {
	arg := struct{ fd, srcOff, n, dstOff int64 }{int64(src.Fd()), off, n, off}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), ficloneRange, uintptr(unsafe.Pointer(&arg))); e != 0 {
		return e
	}
	return nil
}

// linkFile hard-links the open file itself: its path may have been swapped since.
func linkFile(f *os.File, dst string) error {
	from, err := syscall.BytePtrFromString(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
	if err != nil {
		return err
	}
	to, err := syscall.BytePtrFromString(dst)
	if err != nil {
		return err
	}
	fdcwd := atFdcwd
	if _, _, e := syscall.Syscall6(syscall.SYS_LINKAT, uintptr(fdcwd), uintptr(unsafe.Pointer(from)),
		uintptr(fdcwd), uintptr(unsafe.Pointer(to)), atSymlinkFollow, 0); e != 0 {
		return e
	}
	return nil
}

// dialJail connects to a socket in a jail dir: as root, a planted symlink would
// otherwise point boatd at any socket on the host.
func dialJail(ctx context.Context, path string) (net.Conn, error) {
	fd, err := syscall.Open(path, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFSOCK {
		return nil, fmt.Errorf("%s: not a socket", path)
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d", fd))
}

// enableSimpleQuota: BTRFS_IOC_QUOTA_CTL. An ioctl, because btrfs-progs had no option
// for it before 6.7 and the kernel is what counts. Already enabled: not an error.
func enableSimpleQuota(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	args := struct{ cmd, status uint64 }{cmd: btrfsQuotaEnableSimple}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), btrfsIocQuotaCtl, uintptr(unsafe.Pointer(&args))); e != 0 {
		return e
	}
	return nil
}
