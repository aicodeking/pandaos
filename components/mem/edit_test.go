package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOverwriteNeedsFlag(t *testing.T) {
	s := newStore(t)
	p, err := s.Write("semantic", "note", "v1", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write("semantic", "note", "v2", nil, false); err == nil || !strings.Contains(err.Error(), "overwrite:true") || !strings.Contains(err.Error(), "mem_append") {
		t.Fatalf("overwrite without flag: %v", err)
	}
	if got, _ := s.Read(p); !strings.HasSuffix(got, "v1") {
		t.Fatalf("content changed: %q", got)
	}
}

func TestAppendDelete(t *testing.T) {
	s := newStore(t)
	p, _ := s.Write("procedural", "deploy", "step 1", nil, false)
	if got, err := s.Append("", "deploy", "step 2"); err != nil || got != p {
		t.Fatalf("append by title %q %v", got, err)
	}
	if _, err := s.Append(p, "", "step 3"); err != nil {
		t.Fatal(err)
	}
	body, _ := s.Read(p)
	if strings.Count(body, "\n### ") != 2 || !strings.Contains(body, "step 1") || !strings.HasSuffix(body, "step 3\n") {
		t.Fatalf("appended body %q", body)
	}
	for _, c := range [][3]string{{"", "missing", "x"}, {"semantic/none.md", "", "x"}, {p, "", "  "}, {"", "", "x"}} {
		if _, err := s.Append(c[0], c[1], c[2]); err == nil {
			t.Errorf("append %v accepted", c)
		}
	}
	if _, err := s.Delete(p, false); err == nil || !strings.Contains(err.Error(), "confirm") {
		t.Fatalf("delete without confirm: %v", err)
	}
	if _, err := s.Delete("../x.md", true); err == nil {
		t.Fatal("traversal delete")
	}
	if _, err := s.Delete(p, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(p); err == nil {
		t.Fatal("still readable")
	}
	if h, _ := s.History(p, 10); !strings.Contains(h, "mem: delete "+p) {
		t.Fatalf("history %q", h)
	}
	// a symlink planted as a memory is refused, not followed or deleted through
	outside := filepath.Join(t.TempDir(), "keep.md")
	os.WriteFile(outside, []byte("x"), 0o644)
	os.Symlink(outside, filepath.Join(s.Dir, "procedural", "link.md"))
	if _, err := s.Delete("procedural/link.md", true); err == nil {
		t.Fatal("deleted a symlink")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal(err)
	}
}

func TestLogEpisode(t *testing.T) {
	s := newStore(t)
	p, err := s.LogEpisode("Fix flaky login test", "Retried the race; added a lock.", "success", []string{"auth/login.go"})
	if err != nil || !strings.HasPrefix(p, "episodic/20") || !strings.Contains(p, "-fix-flaky-login-test-") {
		t.Fatalf("episode %q %v", p, err)
	}
	body, _ := s.Read(p)
	e := parse(p, []byte(body))
	if e.Kind != "episodic" || e.Title != "Fix flaky login test" || strings.Join(e.Tags, ",") != "episode,success" ||
		!strings.Contains(e.Body, "- auth/login.go") || !strings.Contains(e.Body, "added a lock") {
		t.Fatalf("episode entry %+v", e)
	}
	p2, err := s.LogEpisode("Fix flaky login test", "again", "partial", nil)
	if err != nil || p2 == p || !strings.HasSuffix(p2, "-2.md") {
		t.Fatalf("second episode %q %v", p2, err)
	}
	for _, c := range [][3]string{{"", "s", "success"}, {"t\nx", "s", "success"}, {"t", "", "success"}, {"t", "s", "great"}} {
		if _, err := s.LogEpisode(c[0], c[1], c[2], nil); err == nil {
			t.Errorf("episode %v accepted", c)
		}
	}
	if _, err := s.LogEpisode("t", "s", "success", []string{"a\nb"}); err == nil {
		t.Error("multi-line file accepted")
	}
}

func TestCLIEditCommands(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mem")
	var out strings.Builder
	cli := func(stdin string, args ...string) error {
		out.Reset()
		return run(append(args[:1:1], append([]string{"-dir", dir}, args[1:]...)...), strings.NewReader(stdin), &out)
	}
	if err := cli("", "write", "-kind", "semantic", "-title", "t", "-body", "b"); err != nil {
		t.Fatal(err)
	}
	p := strings.TrimSpace(out.String())
	if err := cli("", "write", "-kind", "semantic", "-title", "t", "-body", "b2"); err == nil {
		t.Fatal("cli overwrite without -overwrite")
	}
	if err := cli("more", "append", "-path", p); err != nil {
		t.Fatal(err)
	}
	if err := cli("did it", "episode", "-task", "x", "-outcome", "failure", "-files", "a,b"); err != nil || !strings.HasPrefix(out.String(), "episodic/") {
		t.Fatalf("episode %q %v", out.String(), err)
	}
	if err := cli("", "delete", "-path", p, "-confirm"); err != nil {
		t.Fatal(err)
	}
}
