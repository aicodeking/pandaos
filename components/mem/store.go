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
	"time"
)

const maxBody = 256 << 10

var kinds = []string{"episodic", "semantic", "procedural"}

// Store is a git repo of markdown memories.
type Store struct {
	Dir string
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
	Terms   int    `json:"terms"`   // distinct query terms matched
	Matches int    `json:"matches"` // total term occurrences
}

func validKind(k string) error {
	for _, v := range kinds {
		if k == v {
			return nil
		}
	}
	return fmt.Errorf("invalid kind %q (want episodic|semantic|procedural)", k)
}

// gitHarden neuters every repo-controlled way to make git run code: hooks,
// fsmonitor, pagers, editors, attribute files, external diff and transports.
// System/global config is ignored via env; local config is allow-listed by checkRepo.
var gitHarden = []string{
	"--no-pager",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "core.pager=cat",
	"-c", "core.editor=true",
	"-c", "core.attributesFile=/dev/null",
	"-c", "diff.external=",
	"-c", "protocol.allow=never",
}

func gitEnv() []string {
	env := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return env
}

func (s *Store) rawGit(args []string) (string, error) {
	argv := append(append([]string{"-C", s.Dir}, gitHarden...), args...)
	cmd := exec.Command("git", argv...)
	cmd.Env = gitEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// driverOverrides blanks every repo-local filter/diff driver (names are
// repo-chosen, so they must be read first); an empty command disables it.
func (s *Store) driverOverrides() []string {
	out, _ := s.rawGit([]string{"config", "--local", "--name-only", "--get-regexp", `^(filter|diff)\.`})
	var o []string
	for _, k := range strings.Fields(out) {
		sect, rest, _ := strings.Cut(k, ".")
		if i := strings.LastIndex(rest, "."); i > 0 {
			k = sect + "." + rest[:i]
		}
		if sect == "filter" {
			o = append(o, "-c", k+".clean=", "-c", k+".smudge=", "-c", k+".process=", "-c", k+".required=false")
		} else {
			o = append(o, "-c", k+".textconv=", "-c", k+".command=")
		}
	}
	return o
}

func (s *Store) git(args ...string) (string, error) {
	return s.rawGit(append(s.driverOverrides(), args...))
}

// Init creates dir and the git repo if missing. Idempotent.
func (s *Store) Init() error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(s.Dir, ".git")); err == nil {
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

// allowedConfig is everything `git init` + Init write. Anything else in
// .git/config (filter.*, diff.*, include.*, core.sshCommand, ...) means tampering.
var allowedConfig = map[string]bool{
	"core.repositoryformatversion": true, "core.filemode": true, "core.bare": true,
	"core.logallrefupdates": true, "core.ignorecase": true, "core.precomposeunicode": true,
	"core.symlinks": true, "extensions.objectformat": true, "extensions.refstorage": true,
	"user.name": true, "user.email": true,
}

// checkRepo refuses to touch a repo with planted hooks or unexpected config.
func (s *Store) checkRepo() error {
	gitDir := filepath.Join(s.Dir, ".git")
	if st, err := os.Lstat(gitDir); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a git directory", gitDir)
	}
	hooks, err := os.ReadDir(filepath.Join(gitDir, "hooks"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, h := range hooks {
		if !strings.HasSuffix(h.Name(), ".sample") {
			return fmt.Errorf("refusing repo: unexpected hook %s", h.Name())
		}
	}
	out, err := s.git("config", "--local", "--name-only", "--list")
	if err != nil {
		return err
	}
	for _, k := range strings.Fields(out) {
		if !allowedConfig[strings.ToLower(k)] {
			return fmt.Errorf("refusing repo: unexpected git config key %s", k)
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

// resolve validates a user-supplied path lexically and splits it into
// kind and file name. Symlinks are handled at open time by openIn.
func resolve(p string) (kind, name string, err error) {
	if p == "" || filepath.IsAbs(p) || strings.ContainsRune(p, 0) {
		return "", "", fmt.Errorf("invalid path %q", p)
	}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(p)), "/")
	if len(parts) != 2 || validKind(parts[0]) != nil || !strings.HasSuffix(parts[1], ".md") || strings.HasPrefix(parts[1], ".") {
		return "", "", fmt.Errorf("invalid path %q (want <kind>/<file>.md)", p)
	}
	return parts[0], parts[1], nil
}

func (s *Store) root() (string, error) { return filepath.EvalSymlinks(s.Dir) }

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

// Write creates the memory for title and commits it; replacing an existing
// one needs overwrite. Returns the relative path.
func (s *Store) Write(kind, title, body string, tags []string, overwrite bool) (string, error) {
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
	return s.mutate("write", func(root string) (string, error) {
		if err := os.Mkdir(filepath.Join(root, kind), 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		name := slug(title) + ".md"
		rel := kind + "/" + name
		created := time.Now().UTC().Format(time.RFC3339)
		if old, err := readIn(root, kind, name); err == nil {
			if !overwrite {
				return "", fmt.Errorf("a memory titled %q already exists at %s: pass overwrite:true to replace it, or use mem_append to add to it", title, rel)
			}
			if c := parse(rel, old).Created; c != "" {
				created = c
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		e := Entry{Title: title, Kind: kind, Created: created, Tags: clean, Body: body}
		return rel, writeIn(root, kind, name, render(e))
	})
}

// mutate runs fn under the repo lock, then stages (incl. deletion) and
// commits the path fn returns. An unchanged file makes no commit.
func (s *Store) mutate(verb string, fn func(root string) (string, error)) (string, error) {
	if err := s.Init(); err != nil {
		return "", err
	}
	if err := s.checkRepo(); err != nil {
		return "", err
	}
	unlock, err := lock(filepath.Join(s.Dir, ".git"))
	if err != nil {
		return "", err
	}
	defer unlock()
	root, err := s.root()
	if err != nil {
		return "", err
	}
	rel, err := fn(root)
	if err != nil {
		return "", err
	}
	if _, err := s.git("add", "-A", "--", rel); err != nil {
		return "", err
	}
	if _, err := s.git("diff", "--cached", "--quiet", "--no-ext-diff", "--no-textconv", "--", rel); err == nil {
		return rel, nil // unchanged
	}
	if _, err := s.git("commit", "-q", "--no-verify", "-m", "mem: "+verb+" "+rel, "--", rel); err != nil {
		return "", err
	}
	return rel, nil
}

func (s *Store) Read(p string) (string, error) {
	kind, name, err := resolve(p)
	if err != nil {
		return "", err
	}
	root, err := s.root()
	if err != nil {
		return "", err
	}
	b, err := readIn(root, kind, name)
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
	root, err := s.root()
	if err != nil {
		return err
	}
	for _, k := range ks {
		ents, err := os.ReadDir(filepath.Join(root, k))
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
			b, err := readIn(root, k, d.Name())
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
	terms := []string{}
	seen := map[string]bool{}
	for _, t := range strings.Fields(strings.ToLower(query)) {
		if !seen[t] {
			seen[t] = true
			terms = append(terms, t)
		}
	}
	if len(terms) == 0 {
		return nil, errors.New("empty query")
	}
	hits := []Hit{}
	err := s.walk(kind, func(rel string, data []byte) {
		lower := strings.ToLower(string(data))
		h := Hit{Path: rel}
		for _, t := range terms {
			if n := strings.Count(lower, t); n > 0 {
				h.Terms++
				h.Matches += n
			}
		}
		if h.Terms == 0 {
			return
		}
		h.Title = parse(rel, data).Title
		h.Snippet = snippet(string(data), terms)
		hits = append(hits, h)
	})
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.Terms != b.Terms {
			return a.Terms > b.Terms
		}
		if a.Matches != b.Matches {
			return a.Matches > b.Matches
		}
		return a.Path < b.Path
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, err
}

// snippet is the line matching the most distinct terms (first wins), capped at 200 bytes.
func snippet(data string, terms []string) string {
	best, bestN := "", 0
	for _, line := range strings.Split(data, "\n") {
		lower, n := strings.ToLower(line), 0
		for _, t := range terms {
			if strings.Contains(lower, t) {
				n++
			}
		}
		if n > bestN {
			best, bestN = strings.TrimSpace(line), n
		}
	}
	if len(best) > 200 {
		best = best[:200]
	}
	return best
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
	if _, err := os.Lstat(filepath.Join(s.Dir, ".git")); err != nil {
		return "", nil
	}
	if err := s.checkRepo(); err != nil {
		return "", err
	}
	if _, err := s.git("rev-parse", "-q", "--verify", "HEAD"); err != nil {
		return "", nil // no commits yet
	}
	args := []string{"log", "-n", strconv.Itoa(limit), "--pretty=format:%h %cI %s"}
	if p != "" {
		kind, name, err := resolve(p)
		if err != nil {
			return "", err
		}
		args = append(args, "--", kind+"/"+name)
	}
	return s.git(args...)
}
