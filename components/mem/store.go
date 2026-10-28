package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBody = 256 << 10

var kinds = []string{"episodic", "semantic", "procedural"}

// Store is a git repo of markdown memories.
type Store struct {
	Dir string
	mu  sync.Mutex // ponytail: in-process lock only; concurrent processes rely on git's index.lock
}

type Entry struct {
	Path    string   `json:"path"`
	Title   string   `json:"title"`
	Kind    string   `json:"kind"`
	Created string   `json:"created"`
	Tags    []string `json:"tags"`
	Body    string   `json:"-"`
}

type Hit struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
	Matches int    `json:"matches"`
}

func validKind(k string) error {
	for _, v := range kinds {
		if k == v {
			return nil
		}
	}
	return fmt.Errorf("invalid kind %q (want episodic|semantic|procedural)", k)
}

func (s *Store) git(args ...string) (string, error) {
	// safe.directory: the shared repo is root-owned; group members must still be allowed in.
	cmd := exec.Command("git", append([]string{"-c", "safe.directory=" + s.Dir, "-C", s.Dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Init creates dir and the git repo if missing. Idempotent.
func (s *Store) Init() error {
	if err := os.MkdirAll(s.Dir, 0o775); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(s.Dir, ".git")); err == nil {
		return nil
	}
	for _, a := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "panda-mem"},
		{"config", "user.email", "panda-mem@localhost"},
	} {
		if _, err := s.git(a...); err != nil {
			return err
		}
	}
	return nil
}

func slug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 60 {
			break
		}
	}
	sum := sha256.Sum256([]byte(title))
	return strings.Trim(b.String(), "-") + "-" + hex.EncodeToString(sum[:4])
}

// resolve maps a user-supplied relative path to an absolute one inside Dir,
// rejecting traversal, absolute paths, symlink escapes and non-memory files.
func (s *Store) resolve(p string) (string, error) {
	if p == "" || filepath.IsAbs(p) || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("invalid path %q", p)
	}
	clean := filepath.Clean(filepath.FromSlash(p))
	parts := strings.Split(clean, string(filepath.Separator))
	if len(parts) != 2 || validKind(parts[0]) != nil || !strings.HasSuffix(clean, ".md") {
		return "", fmt.Errorf("invalid path %q (want <kind>/<file>.md)", p)
	}
	root, err := filepath.EvalSymlinks(s.Dir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(root, clean)
	real, err := filepath.EvalSymlinks(full)
	if errors.Is(err, fs.ErrNotExist) {
		// New file: its parent must still resolve inside root.
		dir, derr := filepath.EvalSymlinks(filepath.Dir(full))
		if derr != nil && !errors.Is(derr, fs.ErrNotExist) {
			return "", derr
		}
		if derr == nil && dir != filepath.Join(root, parts[0]) {
			return "", fmt.Errorf("path %q escapes store", p)
		}
		return full, nil
	}
	if err != nil {
		return "", err
	}
	if real != full {
		return "", fmt.Errorf("path %q escapes store", p)
	}
	return full, nil
}

func render(e Entry) []byte {
	tags, _ := json.Marshal(e.Tags)
	return []byte(fmt.Sprintf("---\ntitle: %s\nkind: %s\ncreated: %s\ntags: %s\n---\n%s",
		strconv.Quote(e.Title), e.Kind, e.Created, tags, e.Body))
}

func parse(rel string, data []byte) Entry {
	e := Entry{Path: rel, Tags: []string{}, Body: string(data)}
	rest, ok := strings.CutPrefix(string(data), "---\n")
	if !ok {
		return e
	}
	head, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return e
	}
	e.Body = body
	for _, line := range strings.Split(head, "\n") {
		k, v, _ := strings.Cut(line, ": ")
		switch k {
		case "title":
			if t, err := strconv.Unquote(v); err == nil {
				e.Title = t
			} else {
				e.Title = v
			}
		case "kind":
			e.Kind = v
		case "created":
			e.Created = v
		case "tags":
			_ = json.Unmarshal([]byte(v), &e.Tags)
		}
	}
	return e
}

// Write creates or updates the memory for title and commits it. Returns the relative path.
func (s *Store) Write(kind, title, body string, tags []string) (string, error) {
	if err := validKind(kind); err != nil {
		return "", err
	}
	title = strings.TrimSpace(title)
	if title == "" || strings.ContainsAny(title, "\r\n") {
		return "", errors.New("title must be a non-empty single line")
	}
	if len(body) > maxBody {
		return "", fmt.Errorf("body exceeds %d bytes", maxBody)
	}
	clean := []string{}
	for _, t := range tags {
		if t = strings.TrimSpace(t); t != "" {
			clean = append(clean, t)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Init(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(s.Dir, kind), 0o775); err != nil {
		return "", err
	}
	rel := kind + "/" + slug(title) + ".md"
	full, err := s.resolve(rel)
	if err != nil {
		return "", err
	}
	created := time.Now().UTC().Format(time.RFC3339)
	if old, err := os.ReadFile(full); err == nil {
		if c := parse(rel, old).Created; c != "" {
			created = c
		}
	}
	e := Entry{Title: title, Kind: kind, Created: created, Tags: clean, Body: body}
	if err := os.WriteFile(full, render(e), 0o664); err != nil {
		return "", err
	}
	if _, err := s.git("add", "--", rel); err != nil {
		return "", err
	}
	if _, err := s.git("diff", "--cached", "--quiet", "--", rel); err == nil {
		return rel, nil // unchanged
	}
	if _, err := s.git("commit", "-q", "-m", "mem: "+rel, "--", rel); err != nil {
		return "", err
	}
	return rel, nil
}

func (s *Store) Read(p string) (string, error) {
	full, err := s.resolve(p)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(full)
	return string(b), err
}

// walk visits regular memory files (symlinks skipped) in the given kind, or all kinds.
func (s *Store) walk(kind string, fn func(rel string, data []byte)) error {
	ks := kinds
	if kind != "" {
		if err := validKind(kind); err != nil {
			return err
		}
		ks = []string{kind}
	}
	for _, k := range ks {
		ents, err := os.ReadDir(filepath.Join(s.Dir, k))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, d := range ents {
			if !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), ".md") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(s.Dir, k, d.Name()))
			if err != nil {
				return err
			}
			fn(k+"/"+d.Name(), b)
		}
	}
	return nil
}

// ponytail: grep search; add sqlite FTS when /mem > ~10k files
func (s *Store) Search(query, kind string, limit int) ([]Hit, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, errors.New("empty query")
	}
	hits := []Hit{}
	err := s.walk(kind, func(rel string, data []byte) {
		lower := strings.ToLower(string(data))
		n := strings.Count(lower, q)
		if n == 0 {
			return
		}
		e := parse(rel, data)
		snip := ""
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(strings.ToLower(line), q) {
				snip = strings.TrimSpace(line)
				break
			}
		}
		if len(snip) > 200 {
			snip = snip[:200]
		}
		hits = append(hits, Hit{Path: rel, Title: e.Title, Snippet: snip, Matches: n})
	})
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Matches != hits[j].Matches {
			return hits[i].Matches > hits[j].Matches
		}
		return hits[i].Path < hits[j].Path
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, err
}

func (s *Store) List(kind string, limit int) ([]Entry, error) {
	out := []Entry{}
	err := s.walk(kind, func(rel string, data []byte) { out = append(out, parse(rel, data)) })
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created > out[j].Created
		}
		return out[i].Path < out[j].Path
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, err
}

func (s *Store) History(p string, limit int) (string, error) {
	if _, err := os.Stat(filepath.Join(s.Dir, ".git")); err != nil {
		return "", nil
	}
	if _, err := s.git("rev-parse", "-q", "--verify", "HEAD"); err != nil {
		return "", nil // no commits yet
	}
	args := []string{"log", "-n", strconv.Itoa(limit), "--pretty=format:%h %cI %s"}
	if p != "" {
		if _, err := s.resolve(p); err != nil {
			return "", err
		}
		args = append(args, "--", filepath.ToSlash(filepath.Clean(p)))
	}
	return s.git(args...)
}
