package main

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"unicode/utf8"
)

const (
	maxDiffBytes = 64 << 10 // unified diffs only for text files up to this size
	maxDiffFiles = 100      // and for at most this many files per diff
)

type entry struct {
	mode  fs.FileMode
	size  int64
	mtime int64
	link  string
}

// Change is one path that differs between two trees.
type Change struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // added | deleted | modified
	Diff string `json:"diff,omitempty"`
}

// scan lstat-walks root (WalkDir never follows symlinks) and returns its
// entries plus the apparent size of its regular files.
func scan(root string) (map[string]entry, int64, error) {
	out := map[string]entry{}
	var total int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		e := entry{mode: fi.Mode()}
		switch {
		case fi.Mode().IsRegular():
			e.size, e.mtime = fi.Size(), fi.ModTime().UnixNano()
			total += fi.Size()
		case fi.Mode()&fs.ModeSymlink != 0:
			if e.link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		out[rel] = e
		return nil
	})
	return out, total, err
}

// compareTrees uses rsync-style quick check (type, mode, size, mtime, link
// target); directory mtimes are ignored since they only mirror their entries.
func compareTrees(a, b string, withDiff bool) ([]Change, error) {
	ea, _, err := scan(a)
	if err != nil {
		return nil, err
	}
	eb, _, err := scan(b)
	if err != nil {
		return nil, err
	}
	changes := []Change{}
	for p, x := range ea {
		y, ok := eb[p]
		switch {
		case !ok:
			changes = append(changes, Change{Path: p, Kind: "deleted"})
		case x != y:
			changes = append(changes, Change{Path: p, Kind: "modified"})
		}
	}
	for p := range eb {
		if _, ok := ea[p]; !ok {
			changes = append(changes, Change{Path: p, Kind: "added"})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	if !withDiff {
		return changes, nil
	}
	n := 0
	for i, c := range changes {
		if n >= maxDiffFiles {
			break
		}
		x, y := sidePath(a, ea, c.Path), sidePath(b, eb, c.Path)
		if (x == "" && y == "") || !isText(x) || !isText(y) {
			continue
		}
		d, err := unifiedDiff(c.Path, x, y)
		if err != nil {
			return nil, err
		}
		changes[i].Diff, n = d, n+1
	}
	return changes, nil
}

// sidePath is root/p for a regular file, /dev/null when p is absent, and ""
// (no text diff) for anything else.
func sidePath(root string, es map[string]entry, p string) string {
	e, ok := es[p]
	if !ok {
		return os.DevNull
	}
	if !e.mode.IsRegular() || e.size > maxDiffBytes {
		return ""
	}
	return filepath.Join(root, p)
}

func isText(p string) bool {
	if p == os.DevNull {
		return true
	}
	if p == "" {
		return false
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxDiffBytes+1))
	return err == nil && len(b) <= maxDiffBytes && bytes.IndexByte(b, 0) < 0 && utf8.Valid(b)
}

func unifiedDiff(rel, x, y string) (string, error) {
	out, err := exec.Command("diff", "-u", "--label", "a/"+rel, "--label", "b/"+rel, x, y).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 { // 1 = files differ
		err = nil
	}
	return string(out), err
}
