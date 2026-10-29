package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// maxFile caps a memory grown by mem_append (readIn reads at most 4*maxBody).
const maxFile = 4 * maxBody

var outcomes = []string{"success", "partial", "failure"}

func singleLine(field, v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || strings.ContainsAny(v, "\r\n") {
		return "", fmt.Errorf("%s must be a non-empty single line", field)
	}
	return v, nil
}

// locate finds an existing memory by path, or by title in any kind.
func locate(root, path, title string) (kind, name string, err error) {
	if path != "" {
		return resolve(path)
	}
	if title = strings.TrimSpace(title); title == "" {
		return "", "", errors.New("give path or title")
	}
	for _, k := range kinds {
		if _, err := os.Lstat(filepath.Join(root, k, slug(title)+".md")); err == nil {
			return k, slug(title) + ".md", nil
		}
	}
	return "", "", fmt.Errorf("no memory titled %q: create it with mem_write", title)
}

// Append adds a timestamped section to an existing memory.
func (s *Store) Append(path, title, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("text must not be empty")
	}
	if len(text) > maxBody {
		return "", fmt.Errorf("text exceeds %d bytes", maxBody)
	}
	return s.mutate("append", func(root string) (string, error) {
		kind, name, err := locate(root, path, title)
		if err != nil {
			return "", err
		}
		old, err := readIn(root, kind, name)
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%s/%s does not exist: create it with mem_write", kind, name)
		}
		if err != nil {
			return "", err
		}
		data := strings.TrimRight(string(old), "\n") + "\n\n### " + time.Now().UTC().Format(time.RFC3339) + "\n\n" + text + "\n"
		if len(data) > maxFile {
			return "", fmt.Errorf("%s/%s would exceed %d bytes; write a new memory instead", kind, name, maxFile)
		}
		return kind + "/" + name, writeIn(root, kind, name, []byte(data))
	})
}

// Delete removes a memory and commits the removal (git history keeps it).
func (s *Store) Delete(path string, confirm bool) (string, error) {
	if !confirm {
		return "", errors.New("mem_delete removes the memory for every agent; call again with confirm:true (git history keeps a copy)")
	}
	kind, name, err := resolve(path)
	if err != nil {
		return "", err
	}
	return s.mutate("delete", func(root string) (string, error) {
		if _, err := readIn(root, kind, name); err != nil { // real dir, regular file, no symlinks
			return "", err
		}
		r, err := os.OpenRoot(filepath.Join(root, kind))
		if err != nil {
			return "", err
		}
		defer r.Close()
		return kind + "/" + name, r.Remove(name)
	})
}

// LogEpisode records what a finished task did as episodic/<date>-<slug>.md.
func (s *Store) LogEpisode(task, summary, outcome string, files []string) (string, error) {
	task, err := singleLine("task", task)
	if err != nil {
		return "", err
	}
	if summary = strings.TrimSpace(summary); summary == "" || len(summary) > maxBody/2 {
		return "", fmt.Errorf("summary must be 1..%d bytes", maxBody/2)
	}
	if !slices.Contains(outcomes, outcome) {
		return "", fmt.Errorf("invalid outcome %q (want %s)", outcome, strings.Join(outcomes, "|"))
	}
	if len(files) > 200 {
		return "", errors.New("at most 200 files")
	}
	var body strings.Builder
	fmt.Fprintf(&body, "## Task\n\n%s\n\n## Outcome\n\n%s\n\n## Summary\n\n%s\n", task, outcome, summary)
	if len(files) > 0 {
		body.WriteString("\n## Files\n\n")
		for _, f := range files {
			f, err := singleLine("file", f)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&body, "- %s\n", f)
		}
	}
	now := time.Now().UTC()
	return s.mutate("episode", func(root string) (string, error) {
		if err := os.Mkdir(filepath.Join(root, "episodic"), 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		base := now.Format("2006-01-02") + "-" + slug(task)
		name := base + ".md"
		for i := 2; ; i++ { // same task twice a day: a new file each time
			if _, err := os.Lstat(filepath.Join(root, "episodic", name)); errors.Is(err, fs.ErrNotExist) {
				break
			}
			name = fmt.Sprintf("%s-%d.md", base, i)
		}
		e := Entry{Title: task, Kind: "episodic", Created: now.Format(time.RFC3339), Tags: []string{"episode", outcome}, Body: body.String()}
		return "episodic/" + name, writeIn(root, "episodic", name, render(e))
	})
}
