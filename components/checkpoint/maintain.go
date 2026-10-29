package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type AutoResult struct {
	Workspace string   `json:"workspace"`
	Created   string   `json:"created,omitempty"`
	Pruned    []string `json:"pruned,omitempty"`
	Skipped   string   `json:"skipped,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// changed reports whether t differs from its head checkpoint (or the latest
// one if head is gone). find-new would be cheaper but needs CAP_SYS_ADMIN.
func (m *Manager) changed(t target, list []Meta) (bool, error) {
	if len(list) == 0 {
		return true, nil
	}
	base := list[len(list)-1].ID
	for _, c := range list {
		if c.ID == m.head(t) {
			base = c.ID
		}
	}
	// ponytail: full metadata walk per tick; fine for agent workspaces, use a
	// root-helper find-new generation check if huge trees make this slow.
	ch, err := compareTrees(filepath.Join(t.dir, base), t.live, false)
	return len(ch) > 0, err
}

// Auto checkpoints every changed ~/work subvolume and, with external, every
// changed git repo Discover finds; keeps the newest keepAuto auto
// checkpoints of each.
func (m *Manager) Auto(keepAuto int, external bool) ([]AutoResult, error) {
	unlock, err := m.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	wss, err := m.Workspaces()
	if err != nil {
		return nil, err
	}
	var ts []target
	for _, w := range wss {
		ts = append(ts, target{name: w.Name, live: w.Path, dir: filepath.Join(m.ckptRoot(), w.Name)})
	}
	var errs []error
	if external {
		repos, err := m.Discover()
		if err != nil {
			errs = append(errs, fmt.Errorf("discover: %w", err))
		}
		for _, r := range repos {
			if t, err := m.extTarget(r.Path); err == nil {
				ts = append(ts, t)
			}
		}
	}
	results := []AutoResult{}
	for _, t := range ts {
		r, err := m.autoOne(t, keepAuto)
		if err != nil {
			r.Error = err.Error()
			errs = append(errs, fmt.Errorf("%s: %w", t.name, err))
		}
		results = append(results, r)
	}
	return results, errors.Join(errs...)
}

func (m *Manager) autoOne(t target, keepAuto int) (AutoResult, error) {
	r := AutoResult{Workspace: t.name}
	if !t.ext && !m.IsSubvol(t.live) {
		r.Skipped = "not a subvolume (run panda-ckpt adopt)"
		return r, nil
	}
	list, err := m.list(t)
	if err != nil {
		return r, err
	}
	ch, err := m.changed(t, list)
	if err != nil {
		return r, err
	}
	if !ch {
		r.Skipped = "unchanged"
	} else {
		meta, err := m.create(t, reasonAuto, reasonAuto)
		if err != nil {
			return r, err
		}
		r.Created = meta.ID
		list = append(list, meta)
	}
	var autos []string
	for _, c := range list {
		if c.Reason == reasonAuto {
			autos = append(autos, c.ID)
		}
	}
	if len(autos) > keepAuto {
		r.Pruned, err = m.deleteAll(t, autos[:len(autos)-keepAuto])
	}
	return r, err
}

// GC removes leftovers of interrupted operations: temp subvolumes in ~/work,
// snapshots without a sidecar, and sidecars without a snapshot. Checkpoints
// of deleted workspaces are kept (they can still be forked).
func (m *Manager) GC() ([]string, error) {
	unlock, err := m.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	removed := []string{}
	var errs []error
	rm := func(p string, f func(string) error) {
		if err := f(p); err != nil {
			errs = append(errs, err)
		} else {
			removed = append(removed, p)
		}
	}
	if ents, err := os.ReadDir(m.workDir()); err == nil {
		for _, e := range ents {
			n := e.Name()
			if e.IsDir() && strings.HasPrefix(n, ".") && (strings.HasSuffix(n, ".restore") || strings.HasSuffix(n, ".adopt")) {
				rm(filepath.Join(m.workDir(), n), m.deleteSubvol)
			}
		}
	}
	wsDirs, err := os.ReadDir(m.ckptRoot())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return removed, err
	}
	dirs := m.extDirs()
	for _, wd := range wsDirs {
		if wd.IsDir() && validWorkspace(wd.Name()) == nil {
			dirs = append(dirs, filepath.Join(m.ckptRoot(), wd.Name()))
		}
	}
	for _, dir := range dirs {
		ents, err := os.ReadDir(dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range ents {
			p := filepath.Join(dir, e.Name())
			if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && e.Type().IsRegular() && !exists(filepath.Join(dir, id)) {
				rm(p, os.Remove)
			} else if e.IsDir() && validName("id", e.Name()) == nil && !exists(p+".json") {
				rm(p, m.deleteSubvol)
			}
		}
	}
	return removed, errors.Join(errs...)
}
