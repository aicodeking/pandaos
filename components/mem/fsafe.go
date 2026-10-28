package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// openIn opens root/kind/name so that a symlink swapped in after validation
// cannot redirect the I/O: the kind dir must be a real directory (Lstat, then
// pinned by os.Root and re-checked with SameFile), and os.Root confines every
// open to that directory even under races. Symlinked files are refused too.
// root itself is admin-controlled.
func openIn(root, kind, name string, flag int, perm os.FileMode) (*os.File, error) {
	dir := filepath.Join(root, kind)
	want, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !want.IsDir() {
		return nil, fmt.Errorf("%s is not a directory (symlink?)", kind)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if got, err := r.Stat("."); err != nil || !os.SameFile(want, got) {
		return nil, fmt.Errorf("%s changed while opening", kind)
	}
	if st, err := r.Lstat(name); err == nil && !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s/%s is not a regular file", kind, name)
	}
	// O_NONBLOCK: a planted FIFO must not hang the reader.
	f, err := r.OpenFile(name, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, perm)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s/%s is not a regular file", kind, name)
	}
	return f, nil
}

func readIn(root, kind, name string) ([]byte, error) {
	f, err := openIn(root, kind, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 4*maxBody))
}

func writeIn(root, kind, name string, data []byte) error {
	f, err := openIn(root, kind, name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// lock takes an exclusive flock on .git/panda-mem.lock; it serialises
// write+commit across processes so they never race on git's index.lock.
func lock(gitDir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(gitDir, "panda-mem.lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil // close releases the flock
}
