package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// extDir holds checkpoints of directories outside ~/work, which are copied
// (cp --reflink=always) instead of snapshotted, so they never move.
const extDir = "_ext"

// discoverDepth: git repos at most this many levels below the home.
const discoverDepth = 4

// skipDirs are never searched for repos: caches, toolchains, our own dirs.
var skipDirs = map[string]bool{
	".cache": true, ".checkpoints": true, ".npm": true, ".cargo": true, ".rustup": true,
	".gradle": true, ".m2": true, ".nix-defexpr": true, ".nix-profile": true,
	".local/share/Trash": true, "node_modules": true,
}

// ExtRepo is a directory outside ~/work with (or eligible for) external checkpoints.
type ExtRepo struct {
	Path        string `json:"path"`
	Slug        string `json:"slug"`
	Checkpoints int    `json:"checkpoints"`
	Latest      string `json:"latest,omitempty"`
}

// Targets is everything `list` without a workspace reports.
type Targets struct {
	Workspaces []Workspace `json:"workspaces"`
	External   []ExtRepo   `json:"external"`
}

// extSlug names the checkpoint dir of a home-relative path: readable, valid
// for validName, and unique thanks to the hash of the exact path.
func extSlug(rel string) string {
	b := []byte(rel)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			b[i] = '_'
		}
	}
	s := string(b)
	if len(s) > 50 {
		s = s[len(s)-50:]
	}
	s = strings.TrimLeft(s, "._-")
	if s == "" {
		s = "x"
	}
	sum := sha256.Sum256([]byte(rel))
	return s + "-" + hex.EncodeToString(sum[:4])
}

// extTarget accepts an absolute or ~/ path to an existing directory inside
// the home, outside ~/work and ~/.checkpoints, reached without symlinks.
func (m *Manager) extTarget(p string) (target, error) {
	if strings.HasPrefix(p, "~/") {
		p = filepath.Join(m.Home, p[2:])
	}
	if !filepath.IsAbs(p) {
		return target{}, fmt.Errorf("%q: give a workspace name or an absolute (or ~/) path", p)
	}
	abs := filepath.Clean(p)
	rel, err := filepath.Rel(m.Home, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return target{}, fmt.Errorf("%s is not inside your home %s", abs, m.Home)
	}
	switch strings.SplitN(rel, "/", 2)[0] {
	case "work":
		return target{}, fmt.Errorf("%s is under ~/work: use its workspace name", abs)
	case ".checkpoints":
		return target{}, fmt.Errorf("%s is inside ~/.checkpoints", abs)
	}
	home, err := filepath.EvalSymlinks(m.Home)
	if err != nil {
		return target{}, err
	}
	if real, err := filepath.EvalSymlinks(abs); err != nil || real != filepath.Join(home, rel) {
		return target{}, fmt.Errorf("%s must be an existing directory reached without symlinks", abs)
	}
	return target{name: abs, live: abs, dir: filepath.Join(m.ckptRoot(), extDir, extSlug(rel)), ext: true}, nil
}

// Discover finds git repos (a .git dir or file) up to discoverDepth below
// the home, outside ~/work, without following symlinks or entering repos.
func (m *Manager) Discover() ([]ExtRepo, error) {
	var out []ExtRepo
	err := filepath.WalkDir(m.Home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == m.Home {
				return err
			}
			return fs.SkipDir // unreadable: not ours to checkpoint
		}
		if !d.IsDir() || p == m.Home {
			return nil
		}
		rel, _ := filepath.Rel(m.Home, p)
		if rel == "work" || skipDirs[rel] || skipDirs[d.Name()] || d.Name() == ".git" {
			return fs.SkipDir
		}
		if exists(filepath.Join(p, ".git")) {
			t, terr := m.extTarget(p)
			if terr == nil {
				r := ExtRepo{Path: p, Slug: filepath.Base(t.dir)}
				if list, lerr := m.list(t); lerr == nil && len(list) > 0 {
					r.Checkpoints, r.Latest = len(list), list[len(list)-1].ID
				}
				out = append(out, r)
			}
			return fs.SkipDir
		}
		if strings.Count(rel, "/")+1 >= discoverDepth {
			return fs.SkipDir
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if out == nil {
		out = []ExtRepo{}
	}
	return out, err
}

func (m *Manager) Targets() (Targets, error) {
	ws, err := m.Workspaces()
	if err != nil {
		return Targets{}, err
	}
	ext, err := m.Discover()
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return Targets{Workspaces: ws, External: ext}, err
}

// extDirs lists the external checkpoint dirs that exist (for gc).
func (m *Manager) extDirs() []string {
	ents, err := os.ReadDir(filepath.Join(m.ckptRoot(), extDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && validName("slug", e.Name()) == nil {
			out = append(out, filepath.Join(m.ckptRoot(), extDir, e.Name()))
		}
	}
	return out
}
