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

func validWorkspace(s string) error {
	if s == extDir {
		return fmt.Errorf("workspace name %q is reserved", s)
	}
	return validName("workspace", s)
}

// Meta is the JSON sidecar stored next to each snapshot as <id>.json.
type Meta struct {
	ID           string    `json:"id"`
	Created      time.Time `json:"created"`
	Label        string    `json:"label"`
	Reason       string    `json:"reason"`
	Parent       string    `json:"parent"`
	SizeEstimate int64     `json:"sizeEstimate"`     // apparent bytes of regular files, not exclusive extents
	Source       string    `json:"source,omitempty"` // external checkpoints: the directory copied
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

// Manager does everything as the calling user under Home. Run executes
// btrfs and cp (tests swap in a fake); IsSubvol reports btrfs subvolume roots;
// Proc is /proc (tests point it at a fake tree).
type Manager struct {
	Home     string
	Run      func(argv ...string) error
	IsSubvol func(path string) bool
	Now      func() time.Time
	Proc     string
}

func NewManager(home string) *Manager {
	return &Manager{Home: home, Run: execRun, IsSubvol: isSubvol, Now: time.Now, Proc: "/proc"}
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

// target is what checkpoints are taken of: a ~/work subvolume, or (ext) any
// other directory in the home, copied by reflink into ~/.checkpoints/_ext.
type target struct {
	name string // workspace name, or the absolute path for ext
	live string // the tree being checkpointed (may not exist any more)
	dir  string // ~/.checkpoints/<ws> or ~/.checkpoints/_ext/<slug>
	ext  bool
}

// target resolves a workspace name or, if it contains '/' or starts with
// '~', a directory path inside the home (see extTarget).
func (m *Manager) target(name string) (target, error) {
	if strings.ContainsRune(name, '/') || strings.HasPrefix(name, "~") {
		return m.extTarget(name)
	}
	if err := validWorkspace(name); err != nil {
		return target{}, err
	}
	return target{name: name, live: filepath.Join(m.workDir(), name), dir: filepath.Join(m.ckptRoot(), name)}, nil
}

// liveDir checks the live tree is a real directory (and ~/work too).
func (m *Manager) liveDir(t target) error {
	if !t.ext {
		if err := realDir(m.workDir()); err != nil {
			return err
		}
	}
	if err := realDir(t.live); err != nil {
		return fmt.Errorf("%s: %w", t.name, err)
	}
	return nil
}

func (m *Manager) wsPath(ws string) (string, error) {
	t, err := m.target(ws)
	if err != nil {
		return "", err
	}
	if t.ext {
		return "", fmt.Errorf("%s is not a ~/work workspace name", ws)
	}
	return t.live, m.liveDir(t)
}

// snapDir returns t.dir, creating it (and its parents) when create is set.
func (m *Manager) snapDir(t target, create bool) (string, error) {
	dirs := []string{m.ckptRoot()}
	if t.ext {
		dirs = append(dirs, filepath.Join(m.ckptRoot(), extDir))
	}
	for _, d := range append(dirs, t.dir) {
		var err error
		if create {
			err = mkdirReal(d, 0o700)
		} else {
			err = realDir(d)
		}
		if err != nil {
			return "", err
		}
	}
	return t.dir, nil
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

// head is what the tree currently derives from: last checkpoint, the
// restored id, or "<ws>/<id>" for a fork. Stored in <dir>/.head.
func (m *Manager) head(t target) string {
	b, err := os.ReadFile(filepath.Join(t.dir, ".head"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (m *Manager) setHead(t target, h string) error {
	dir, err := m.snapDir(t, true)
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
	if err := validWorkspace(ws); err != nil {
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
// new subvolume, reflink copy, atomic swap, drop the old tree. Processes
// using the directory would keep the old (deleted) tree, so that needs confirm.
func (m *Manager) Adopt(path string, confirm bool) (Workspace, error) {
	unlock, err := m.lock()
	if err != nil {
		return Workspace{}, err
	}
	defer unlock()
	if strings.HasPrefix(path, "~/") {
		path = filepath.Join(m.Home, path[2:])
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Workspace{}, err
	}
	name := filepath.Base(abs)
	if filepath.Dir(abs) != m.workDir() {
		return Workspace{}, fmt.Errorf("adopt: %s must be directly inside %s; to checkpoint a directory elsewhere without moving it, pass its path as the workspace", abs, m.workDir())
	}
	if _, err := m.wsPath(name); err != nil {
		return Workspace{}, err
	}
	if m.IsSubvol(abs) {
		return Workspace{}, fmt.Errorf("adopt: %s is already a subvolume", abs)
	}
	if pids := m.busyPids(abs); len(pids) > 0 && !confirm {
		return Workspace{}, fmt.Errorf("adopt: processes %v have their cwd or open files in %s; after the swap they would keep using the old, deleted tree. Stop them or cd out, or pass confirm:true (--confirm) and have them cd back in", pids, abs)
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

func (m *Manager) Create(name, label string) (Meta, error) {
	unlock, err := m.lock()
	if err != nil {
		return Meta{}, err
	}
	defer unlock()
	t, err := m.target(name)
	if err != nil {
		return Meta{}, err
	}
	if label == "" {
		label = reasonManual
	}
	return m.create(t, label, reasonManual)
}

func (m *Manager) create(t target, label, reason string) (Meta, error) {
	if err := m.liveDir(t); err != nil {
		return Meta{}, err
	}
	dir, err := m.snapDir(t, true)
	if err != nil {
		return Meta{}, err
	}
	_, size, err := scan(t.live)
	if err != nil {
		return Meta{}, err
	}
	now := m.Now().UTC()
	base := now.Format("20060102-150405")
	id := base
	for i := 1; exists(filepath.Join(dir, id)) || exists(filepath.Join(dir, id+".json")); i++ {
		id = fmt.Sprintf("%s-%d", base, i)
	}
	if err := m.snapshot(t, filepath.Join(dir, id)); err != nil {
		return Meta{}, err
	}
	meta := Meta{ID: id, Created: now, Label: label, Reason: reason, Parent: m.head(t), SizeEstimate: size}
	if t.ext {
		meta.Source = t.live
	}
	if err := writeJSONFile(filepath.Join(dir, id+".json"), meta); err != nil {
		return Meta{}, err
	}
	return meta, m.setHead(t, id)
}

// snapshot makes the read-only checkpoint dst of t: a btrfs snapshot for a
// workspace, a new subvolume filled by reflink copy for an external tree.
func (m *Manager) snapshot(t target, dst string) error {
	if !t.ext {
		return m.btrfs("subvolume", "snapshot", "-r", t.live, dst)
	}
	if err := m.btrfs("subvolume", "create", dst); err != nil {
		return err
	}
	if err := m.Run("cp", "-a", "--reflink=always", t.live+"/.", dst+"/"); err != nil {
		return errors.Join(err, m.deleteSubvol(dst))
	}
	if err := m.btrfs("property", "set", "-ts", dst, "ro", "true"); err != nil {
		return errors.Join(err, m.deleteSubvol(dst))
	}
	return nil
}

// List returns the checkpoints of name, oldest first. Sidecars without a
// snapshot (or unreadable ones) are left for gc.
func (m *Manager) List(name string) ([]Meta, error) {
	t, err := m.target(name)
	if err != nil {
		return nil, err
	}
	return m.list(t)
}

func (m *Manager) list(t target) ([]Meta, error) {
	dir, err := m.snapDir(t, false)
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

// lastN keeps the newest n entries of an oldest-first list (n <= 0: all).
func lastN(list []Meta, n int) []Meta {
	if n > 0 && len(list) > n {
		return list[len(list)-n:]
	}
	return list
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
		if e.IsDir() && validWorkspace(e.Name()) == nil {
			out = append(out, Workspace{e.Name(), filepath.Join(m.workDir(), e.Name())})
		}
	}
	return out, nil
}

func (m *Manager) snapPath(t target, id string) (string, error) {
	if err := validName("checkpoint id", id); err != nil {
		return "", err
	}
	dir, err := m.snapDir(t, false)
	if err != nil {
		return "", fmt.Errorf("no checkpoints for %s: %w", t.name, err)
	}
	p := filepath.Join(dir, id)
	if err := realDir(p); err != nil {
		return "", fmt.Errorf("checkpoint %s %s: %w", t.name, id, err)
	}
	return p, nil
}

// Diff compares checkpoint id with id2, or with the live tree when id2 is
// "" or "current".
func (m *Manager) Diff(name, id, id2 string) ([]Change, error) {
	t, err := m.target(name)
	if err != nil {
		return nil, err
	}
	a, err := m.snapPath(t, id)
	if err != nil {
		return nil, err
	}
	b := t.live
	if id2 == "" || id2 == "current" {
		err = m.liveDir(t)
	} else {
		b, err = m.snapPath(t, id2)
	}
	if err != nil {
		return nil, err
	}
	return compareTrees(a, b, true)
}

// Restore checkpoints the current state, then atomically swaps a writable
// copy of id into place. Processes inside the old tree must cd again.
func (m *Manager) Restore(name, id string) (RestoreResult, error) {
	unlock, err := m.lock()
	if err != nil {
		return RestoreResult{}, err
	}
	defer unlock()
	t, err := m.target(name)
	if err != nil {
		return RestoreResult{}, err
	}
	snap, err := m.snapPath(t, id)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := m.liveDir(t); err != nil {
		return RestoreResult{}, err
	}
	tmp := filepath.Join(filepath.Dir(t.live), "."+filepath.Base(t.live)+".restore")
	if exists(tmp) {
		return RestoreResult{}, fmt.Errorf("restore: leftover %s, run gc (or remove it)", tmp)
	}
	safety, err := m.create(t, "before restore to "+id, reasonPreRestore)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("safety checkpoint: %w", err)
	}
	res := RestoreResult{Workspace: t.name, Restored: id, Safety: safety.ID}
	drop := m.deleteSubvol
	if t.ext {
		// Plain directory next to the original, filled by reflink copy.
		drop = os.RemoveAll
		if err := os.Mkdir(tmp, 0o700); err != nil {
			return res, err
		}
		err = m.Run("cp", "-a", "--reflink=always", snap+"/.", tmp+"/")
	} else {
		err = m.btrfs("subvolume", "snapshot", snap, tmp)
	}
	if err != nil {
		return res, errors.Join(err, drop(tmp))
	}
	if err := swap(t.live, tmp); err != nil {
		return res, errors.Join(err, drop(tmp))
	}
	if err := m.setHead(t, id); err != nil {
		return res, err
	}
	if err := drop(tmp); err != nil {
		res.Warning = "restored, but the old tree is left at " + tmp + ": " + err.Error()
	}
	return res, nil
}

// Fork makes a writable copy of name@id as a new ~/work workspace.
func (m *Manager) Fork(name, id, newName string) (Workspace, error) {
	unlock, err := m.lock()
	if err != nil {
		return Workspace{}, err
	}
	defer unlock()
	t, err := m.target(name)
	if err != nil {
		return Workspace{}, err
	}
	nt, err := m.target(newName)
	if err != nil {
		return Workspace{}, err
	}
	if nt.ext {
		return Workspace{}, fmt.Errorf("fork: %q must be a new workspace name in ~/work, not a path", newName)
	}
	snap, err := m.snapPath(t, id)
	if err != nil {
		return Workspace{}, err
	}
	if err := mkdirReal(m.workDir(), 0o755); err != nil {
		return Workspace{}, err
	}
	if exists(nt.live) {
		return Workspace{}, fmt.Errorf("workspace %s already exists", newName)
	}
	if err := m.btrfs("subvolume", "snapshot", snap, nt.live); err != nil {
		return Workspace{}, err
	}
	return Workspace{newName, nt.live}, m.setHead(nt, t.name+"/"+id)
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

func (m *Manager) deleteCheckpoint(t target, id string) error {
	if err := m.deleteSubvol(filepath.Join(t.dir, id)); err != nil {
		return err
	}
	return os.Remove(filepath.Join(t.dir, id+".json"))
}

// Prune deletes checkpoints that are beyond the newest keep AND older than
// olderThan (0 = any age).
func (m *Manager) Prune(name string, keep int, olderThan time.Duration) ([]string, error) {
	if keep <= 0 && olderThan <= 0 {
		return nil, errors.New("prune: need --keep N and/or --older-than D")
	}
	unlock, err := m.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	t, err := m.target(name)
	if err != nil {
		return nil, err
	}
	list, err := m.list(t)
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
	return m.deleteAll(t, victims)
}

func (m *Manager) deleteAll(t target, ids []string) ([]string, error) {
	done := []string{}
	for _, id := range ids {
		if err := m.deleteCheckpoint(t, id); err != nil {
			return done, fmt.Errorf("delete %s %s: %w", t.name, id, err)
		}
		done = append(done, id)
	}
	return done, nil
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
