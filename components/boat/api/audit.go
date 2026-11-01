package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// rotatingFile is an append-only log that rotates by size and keeps at most
// keep rotated files, none older than maxAge. Rotated files are named
// <path>.<UTC timestamp>, so lexical order is age order. Not safe for
// concurrent use; auditLog serialises writes.
type rotatingFile struct {
	path     string
	maxBytes int64         // 0 = never rotate
	keep     int           // rotated files kept; 0 = unlimited
	maxAge   time.Duration // 0 = forever
	now      func() time.Time
	f        *os.File
	size     int64
}

func openRotating(path string, maxBytes int64, keep int, maxAge time.Duration) (*rotatingFile, error) {
	r := &rotatingFile{path: path, maxBytes: maxBytes, keep: keep, maxAge: maxAge, now: time.Now}
	if err := r.open(); err != nil {
		return nil, err
	}
	r.prune()
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *rotatingFile) Write(b []byte) (int, error) {
	if r.maxBytes > 0 && r.size > 0 && r.size+int64(len(b)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return 0, fmt.Errorf("rotate %s: %w", r.path, err)
		}
	}
	n, err := r.f.Write(b)
	r.size += int64(n)
	return n, err
}

func (r *rotatingFile) rotate() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	dst := r.path + "." + r.now().UTC().Format("20060102T150405.000000000Z")
	if err := os.Rename(r.path, dst); err != nil {
		return err
	}
	if err := r.open(); err != nil {
		return err
	}
	r.prune()
	return nil
}

// rotated lists rotated files, oldest first.
func (r *rotatingFile) rotated() []string {
	matches, _ := filepath.Glob(r.path + ".*")
	var out []string
	for _, m := range matches {
		if !strings.HasPrefix(filepath.Base(m), ".") {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

func (r *rotatingFile) prune() {
	files := r.rotated()
	for i, f := range files {
		tooMany := r.keep > 0 && len(files)-i > r.keep
		tooOld := false
		if r.maxAge > 0 {
			if st, err := os.Stat(f); err == nil && r.now().Sub(st.ModTime()) > r.maxAge {
				tooOld = true
			}
		}
		if tooMany || tooOld {
			os.Remove(f)
		}
	}
}

func (r *rotatingFile) Close() error { return r.f.Close() }
