package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// A VM's root dir belongs to its jail uid, so a compromised VMM can replace any
// file in it with a symlink, a FIFO or a device between two of boatd's calls. boatd
// is root: everything it does there goes through a descriptor opened by openJail,
// never by path. The dirs above root stay root-owned, so only the last component
// can be planted, and a memory file is built outside root (memStage) and renamed in.

// openJail opens a regular file without following a symlink. O_NONBLOCK: a planted
// FIFO must not hang a goroutine holding the VM's lock.
func openJail(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = fmt.Errorf("%s: not a regular file (%s)", path, st.Mode().Type())
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func truncateJail(path string, size int64) error {
	f, err := openJail(path, os.O_WRONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}

// sealFile: root-owned and read-only, so that VMs of one team can share the inode.
func sealFile(f *os.File) error {
	if os.Geteuid() == 0 {
		if err := f.Chown(0, 0); err != nil {
			return err
		}
	}
	return f.Chmod(0o444)
}

// cloneFile makes dst a copy of src: a reflink, or with reflink "auto" a sparse copy
// where the filesystem has none. dst must be in a dir the VMM cannot reach (yet).
func cloneFile(src, dst, reflink string) error {
	in, err := openJail(src, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	noCow := false
	if v, err := fsFlags(in); err == nil {
		noCow = v&fsNoCowFlag != 0
	}
	if noCow && !createNoCow(dst) { // a clone has to match its source's NOCOW flag
		return fmt.Errorf("cp: cannot create NOCOW %s", dst)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, st.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	if err := out.Truncate(0); err != nil { // reused: a stale file must not survive a sparse copy
		return err
	}
	if err := out.Chmod(st.Mode().Perm()); err != nil { // an existing NOCOW file, or the umask
		return err
	}
	err = ficlone(out, in)
	if err != nil && reflink == "always" {
		return fmt.Errorf("reflink %s: %w", src, err)
	}
	if err != nil {
		if err := copySparse(in, out, st.Size()); err != nil {
			return err
		}
		if err := out.Truncate(st.Size()); err != nil { // a hole at the end
			return err
		}
	}
	return out.Close()
}

// copySparse copies every data region of in (SEEK_DATA/SEEK_HOLE) to the same
// offsets of out. Without hole support it copies the whole file.
func copySparse(in, out *os.File, size int64) error { return copyData(in, out, size, false) }

// copyData copies the data regions of in to the same offsets in out; with clone, as
// reflinks where the filesystem can (no second write of data already on disk).
func copyData(in, out *os.File, size int64, clone bool) error {
	for off := int64(0); off < size; {
		data, err := in.Seek(off, seekData)
		if errors.Is(err, syscall.ENXIO) {
			break // no data after off
		}
		if err != nil {
			data = off // no SEEK_DATA: treat the rest as data
		}
		hole, err := in.Seek(data, seekHole)
		if err != nil {
			hole = size
		}
		if clone && cloneRange(out, in, data, hole-data) == nil {
			off = hole
			continue
		}
		clone = false // not this filesystem, or an unaligned range: copy from here on
		if _, err := io.Copy(io.NewOffsetWriter(out, data), io.NewSectionReader(in, data, hole-data)); err != nil {
			return err
		}
		off = hole
	}
	return nil
}
