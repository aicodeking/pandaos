package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Reasons recorded in Meta.Reason. Only "auto" checkpoints are subject to
// automatic retention; manual and pre-restore ones are kept until pruned.
const (
	reasonManual     = "manual"
	reasonAuto       = "auto"
	reasonPreRestore = "pre-restore"
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// validName also rejects a leading '.', which rules out "." and ".." and
// reserves dot-names in ~/work for our own temporaries.
func validName(kind, s string) error {
	if !nameRE.MatchString(s) || s[0] == '.' {
		return fmt.Errorf("invalid %s %q: want [A-Za-z0-9._-]{1,64}, not starting with '.'", kind, s)
	}
	return nil
}

// Meta is the JSON sidecar stored next to each snapshot as <id>.json.
type Meta struct {
	ID           string    `json:"id"`
	Created      time.Time `json:"created"`
	Label        string    `json:"label"`
	Reason       string    `json:"reason"`
	Parent       string    `json:"parent"`
	SizeEstimate int64     `json:"sizeEstimate"` // apparent bytes of regular files, not exclusive extents
}

type Workspace struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type RestoreResult struct {
	Workspace string `json:"workspace"`
	Restored  string `json:"restored"`
	Safety    string `json:"safety"`
	Warning   string `json:"warning,omitempty"`
}

type AutoResult struct {
	Workspace string   `json:"workspace"`
	Created   string   `json:"created,omitempty"`
	Pruned    []string `json:"pruned,omitempty"`
	Skipped   string   `json:"skipped,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// Manager does everything as the calling user under Home. Run executes
// btrfs and cp (tests swap in a fake); IsSubvol reports btrfs subvolume roots.
type Manager struct {
	Home     string
	Run      func(argv ...string) error
	IsSubvol func(path string) bool
	Now      func() time.Time
}

func NewManager(home string) *Manager {
	return &Manager{Home: home, Run: execRun, IsSubvol: isSubvol, Now: time.Now}
}

func execRun(argv ...string) error {
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *Manager) btrfs(args ...string) error { return m.Run(append([]string{"btrfs"}, args...)...) }

func (m *Manager) workDir() string  { return filepath.Join(m.Home, "work") }
func (m *Manager) ckptRoot() string { return filepath.Join(m.Home, ".checkpoints") }

// realDir fails unless p is a directory and not a symlink.
func realDir(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s: not a directory (symlinks are refused)", p)
	}
	return nil
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// mkdirReal creates p (one level) if missing and checks it is a real dir.
func mkdirReal(p string, mode os.FileMode) error {
	if err := os.Mkdir(p, mode); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return realDir(p)
}

func (m *Manager) wsPath(ws string) (string, error) {
	if err := validName("workspace", ws); err != nil {
		return "", err
	}
	if err := realDir(m.workDir()); err != nil {
		return "", err
	}
	p := filepath.Join(m.workDir(), ws)
	if err := realDir(p); err != nil {
		return "", fmt.Errorf("workspace %s: %w", ws, err)
	}
	return p, nil
}

func (m *Manager) snapDir(ws string, create bool) (string, error) {
	if err := validName("workspace", ws); err != nil {
		return "", err
	}
	root, dir := m.ckptRoot(), filepath.Join(m.ckptRoot(), ws)
	if create {
		if err := mkdirReal(root, 0o700); err != nil {
			return "", err
		}
		return dir, mkdirReal(dir, 0o700)
	}
	if err := realDir(root); err != nil {
		return "", err
	}
	return dir, realDir(dir)
}

// lock serialises mutating commands of one user (CLI, MCP and timer).
func (m *Manager) lock() (func(), error) {
	if err := mkdirReal(m.ckptRoot(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(m.ckptRoot(), ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func writeJSONFile(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// head is what the workspace currently derives from: last checkpoint, the
// restored id, or "<ws>/<id>" for a fork. Stored in .checkpoints/<ws>/.head.
func (m *Manager) head(ws string) string {
	b, err := os.ReadFile(filepath.Join(m.ckptRoot(), ws, ".head"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (m *Manager) setHead(ws, h string) error {
	dir, err := m.snapDir(ws, true)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".head.tmp")
	if err := os.WriteFile(tmp, []byte(h+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, ".head"))
}

// Init creates ~/work/<ws> as a new subvolume.
func (m *Manager) Init(ws string) (Workspace, error) {
	if err := validName("workspace", ws); err != nil {
		return Workspace{}, err
	}
	if err := mkdirReal(m.workDir(), 0o755); err != nil {
		return Workspace{}, err
	}
	p := filepath.Join(m.workDir(), ws)
	if exists(p) {
		return Workspace{}, fmt.Errorf("workspace %s already exists", ws)
	}
	return Workspace{ws, p}, m.btrfs("subvolume", "create", p)
}

// Adopt turns a plain directory ~/work/<name> into a subvolume in place:
// new subvolume, reflink copy, atomic swap, drop the old tree.
func (m *Manager) Adopt(path string) (Workspace, error) {
	unlock, err := m.lock()
	if err != nil {
		return Workspace{}, err
	}
	defer unlock()
	abs, err := filepath.Abs(path)
	if err != nil {
		return Workspace{}, err
	}
	name := filepath.Base(abs)
	if filepath.Dir(abs) != m.workDir() {
		return Workspace{}, fmt.Errorf("adopt: %s must be directly inside %s (move it there first)", abs, m.workDir())
	}
	if _, err := m.wsPath(name); err != nil {
		return Workspace{}, err
	}
	if m.IsSubvol(abs) {
		return Workspace{}, fmt.Errorf("adopt: %s is already a subvolume", abs)
	}
	tmp := filepath.Join(m.workDir(), "."+name+".adopt")
	if exists(tmp) {
		return Workspace{}, fmt.Errorf("adopt: leftover %s, run gc", tmp)
	}
	if err := m.btrfs("subvolume", "create", tmp); err != nil {
		return Workspace{}, err
	}
	if err := m.Run("cp", "-a", "--reflink=auto", abs+"/.", tmp+"/"); err != nil {
		return Workspace{}, errors.Join(err, m.deleteSubvol(tmp))
	}
	if err := swap(abs, tmp); err != nil {
		return Workspace{}, errors.Join(err, m.deleteSubvol(tmp))
	}
	if err := os.RemoveAll(tmp); err != nil {
		return Workspace{name, abs}, fmt.Errorf("adopted, but could not remove old tree %s: %w", tmp, err)
	}
	return Workspace{name, abs}, nil
}

func (m *Manager) Create(ws, label string) (Meta, error) {
	unlock, err := m.lock()
	if err != nil {
		return Meta{}, err
	}
	defer unlock()
	if label == "" {
		label = reasonManual
	}
	return m.create(ws, label, reasonManual)
}

func (m *Manager) create(ws, label, reason string) (Meta, error) {
	src, err := m.wsPath(ws)
	if err != nil {
		return Meta{}, err
	}
	dir, err := m.snapDir(ws, true)
	if err != nil {
		return Meta{}, err
	}
	_, size, err := scan(src)
	if err != nil {
		return Meta{}, err
	}
	now := m.Now().UTC()
	base := now.Format("20060102-150405")
	id := base
	for i := 1; exists(filepath.Join(dir, id)) || exists(filepath.Join(dir, id+".json")); i++ {
		id = fmt.Sprintf("%s-%d", base, i)
	}
	if err := m.btrfs("subvolume", "snapshot", "-r", src, filepath.Join(dir, id)); err != nil {
		return Meta{}, err
	}
	meta := Meta{ID: id, Created: now, Label: label, Reason: reason, Parent: m.head(ws), SizeEstimate: size}
	if err := writeJSONFile(filepath.Join(dir, id+".json"), meta); err != nil {
		return Meta{}, err
	}
	return meta, m.setHead(ws, id)
}

// List returns checkpoints oldest first. Sidecars without a snapshot (or
// unreadable ones) are left for gc.
func (m *Manager) List(ws string) ([]Meta, error) {
	dir, err := m.snapDir(ws, false)
	if errors.Is(err, fs.ErrNotExist) {
		return []Meta{}, nil
	}
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := []Meta{}
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() || validName("id", id) != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		var meta Meta
		if err != nil || json.Unmarshal(b, &meta) != nil || meta.ID != id || realDir(filepath.Join(dir, id)) != nil {
			continue
		}
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *Manager) Workspaces() ([]Workspace, error) {
	ents, err := os.ReadDir(m.workDir())
	if errors.Is(err, fs.ErrNotExist) {
		return []Workspace{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Workspace{}
	for _, e := range ents {
		if e.IsDir() && validName("workspace", e.Name()) == nil {
			out = append(out, Workspace{e.Name(), filepath.Join(m.workDir(), e.Name())})
		}
	}
	return out, nil
}

func (m *Manager) snapPath(ws, id string) (string, error) {
	if err := validName("checkpoint id", id); err != nil {
		return "", err
	}
	dir, err := m.snapDir(ws, false)
	if err != nil {
		return "", fmt.Errorf("no checkpoints for %s: %w", ws, err)
	}
	p := filepath.Join(dir, id)
	if err := realDir(p); err != nil {
		return "", fmt.Errorf("checkpoint %s/%s: %w", ws, id, err)
	}
	return p, nil
}

// Diff compares checkpoint id with id2, or with the live workspace when id2
// is "" or "current".
func (m *Manager) Diff(ws, id, id2 string) ([]Change, error) {
	a, err := m.snapPath(ws, id)
	if err != nil {
		return nil, err
	}
	var b string
	if id2 == "" || id2 == "current" {
		b, err = m.wsPath(ws)
	} else {
		b, err = m.snapPath(ws, id2)
	}
	if err != nil {
		return nil, err
	}
	return compareTrees(a, b, true)
}

// Restore checkpoints the current state, then atomically swaps a writable
// copy of id into place. Processes inside the old tree must cd again.
func (m *Manager) Restore(ws, id string) (RestoreResult, error) {
	unlock, err := m.lock()
	if err != nil {
		return RestoreResult{}, err
	}
	defer unlock()
	snap, err := m.snapPath(ws, id)
	if err != nil {
		return RestoreResult{}, err
	}
	live, err := m.wsPath(ws)
	if err != nil {
		return RestoreResult{}, err
	}
	tmp := filepath.Join(m.workDir(), "."+ws+".restore")
	if exists(tmp) {
		return RestoreResult{}, fmt.Errorf("restore: leftover %s, run gc", tmp)
	}
	safety, err := m.create(ws, "before restore to "+id, reasonPreRestore)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("safety checkpoint: %w", err)
	}
	res := RestoreResult{Workspace: ws, Restored: id, Safety: safety.ID}
	if err := m.btrfs("subvolume", "snapshot", snap, tmp); err != nil {
		return res, err
	}
	if err := swap(live, tmp); err != nil {
		return res, errors.Join(err, m.deleteSubvol(tmp))
	}
	if err := m.setHead(ws, id); err != nil {
		return res, err
	}
	if err := m.deleteSubvol(tmp); err != nil {
		res.Warning = "restored, but the old tree is left at " + tmp + " (gc removes it): " + err.Error()
	}
	return res, nil
}

// Fork makes a writable copy of ws@id as a new workspace.
func (m *Manager) Fork(ws, id, newName string) (Workspace, error) {
	unlock, err := m.lock()
	if err != nil {
		return Workspace{}, err
	}
	defer unlock()
	if err := validName("workspace", newName); err != nil {
		return Workspace{}, err
	}
	snap, err := m.snapPath(ws, id)
	if err != nil {
		return Workspace{}, err
	}
	if err := realDir(m.workDir()); err != nil {
		return Workspace{}, err
	}
	dst := filepath.Join(m.workDir(), newName)
	if exists(dst) {
		return Workspace{}, fmt.Errorf("workspace %s already exists", newName)
	}
	if err := m.btrfs("subvolume", "snapshot", snap, dst); err != nil {
		return Workspace{}, err
	}
	return Workspace{newName, dst}, m.setHead(newName, ws+"/"+id)
}

// deleteSubvol clears the read-only flag (allowed for the owner) and deletes.
// Without user_subvol_rm_allowed the ioctl is refused for non-root, so fall
// back to emptying the tree and rmdir'ing it (kernel >= 4.18 allows that).
func (m *Manager) deleteSubvol(p string) error {
	_ = m.btrfs("property", "set", "-ts", p, "ro", "false") // fails on plain dirs; RemoveAll will report
	err := m.btrfs("subvolume", "delete", p)
	if err == nil {
		return nil
	}
	if rerr := os.RemoveAll(p); rerr != nil {
		return fmt.Errorf("%v; fallback remove: %w", err, rerr)
	}
	return nil
}

func (m *Manager) deleteCheckpoint(ws, id string) error {
	dir := filepath.Join(m.ckptRoot(), ws)
	if err := m.deleteSubvol(filepath.Join(dir, id)); err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, id+".json"))
}

// Prune deletes checkpoints that are beyond the newest keep AND older than
// olderThan (0 = any age).
func (m *Manager) Prune(ws string, keep int, olderThan time.Duration) ([]string, error) {
	if keep <= 0 && olderThan <= 0 {
		return nil, errors.New("prune: need --keep N and/or --older-than D")
	}
	unlock, err := m.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	list, err := m.List(ws)
	if err != nil {
		return nil, err
	}
	now := m.Now()
	var victims []string
	for i := 0; i < len(list)-keep; i++ {
		if olderThan <= 0 || now.Sub(list[i].Created) > olderThan {
			victims = append(victims, list[i].ID)
		}
	}
	return m.deleteAll(ws, victims)
}

func (m *Manager) deleteAll(ws string, ids []string) ([]string, error) {
	done := []string{}
	for _, id := range ids {
		if err := m.deleteCheckpoint(ws, id); err != nil {
			return done, fmt.Errorf("delete %s/%s: %w", ws, id, err)
		}
		done = append(done, id)
	}
	return done, nil
}

// changed reports whether ws differs from its head checkpoint (or the latest
// one if head is gone). find-new would be cheaper but needs CAP_SYS_ADMIN.
func (m *Manager) changed(ws, live string, list []Meta) (bool, error) {
	if len(list) == 0 {
		return true, nil
	}
	base := list[len(list)-1].ID
	for _, c := range list {
		if c.ID == m.head(ws) {
			base = c.ID
		}
	}
	// ponytail: full metadata walk per tick; fine for agent workspaces, use a
	// root-helper find-new generation check if huge trees make this slow.
	ch, err := compareTrees(filepath.Join(m.ckptRoot(), ws, base), live, false)
	return len(ch) > 0, err
}

// Auto checkpoints every changed workspace subvolume and keeps the newest
// keepAuto auto checkpoints per workspace.
func (m *Manager) Auto(keepAuto int) ([]AutoResult, error) {
	unlock, err := m.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	wss, err := m.Workspaces()
	if err != nil {
		return nil, err
	}
	results := []AutoResult{}
	var errs []error
	for _, w := range wss {
		r, err := m.autoOne(w, keepAuto)
		if err != nil {
			r.Error = err.Error()
			errs = append(errs, fmt.Errorf("%s: %w", w.Name, err))
		}
		results = append(results, r)
	}
	return results, errors.Join(errs...)
}

func (m *Manager) autoOne(w Workspace, keepAuto int) (AutoResult, error) {
	r := AutoResult{Workspace: w.Name}
	if !m.IsSubvol(w.Path) {
		r.Skipped = "not a subvolume (run panda-ckpt adopt)"
		return r, nil
	}
	list, err := m.List(w.Name)
	if err != nil {
		return r, err
	}
	ch, err := m.changed(w.Name, w.Path, list)
	if err != nil {
		return r, err
	}
	if !ch {
		r.Skipped = "unchanged"
	} else {
		meta, err := m.create(w.Name, reasonAuto, reasonAuto)
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
		r.Pruned, err = m.deleteAll(w.Name, autos[:len(autos)-keepAuto])
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
	for _, wd := range wsDirs {
		if !wd.IsDir() || validName("workspace", wd.Name()) != nil {
			continue
		}
		dir := filepath.Join(m.ckptRoot(), wd.Name())
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

// swapRenames is the non-atomic fallback for swap: a -> .a.swap, b -> a, .a.swap -> b.
func swapRenames(a, b string) error {
	tmp := filepath.Join(filepath.Dir(a), "."+filepath.Base(a)+".swap")
	if err := os.Rename(a, tmp); err != nil {
		return err
	}
	if err := os.Rename(b, a); err != nil {
		return errors.Join(err, os.Rename(tmp, a))
	}
	return os.Rename(tmp, b)
}
