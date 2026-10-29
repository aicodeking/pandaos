package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain lets the test binary act as the panda-mem CLI for cross-process tests.
func TestMain(m *testing.M) {
	if os.Getenv("PANDA_MEM_CHILD") == "1" {
		if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// plainGit runs unhardened git, like an attacker with write access would.
func plainGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func evil(t *testing.T, marker string) string {
	t.Helper()
	p := filepath.Join(filepath.Dir(marker), "evil.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertNoMarker(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("planted code executed")
	}
}

func TestPlantedHookDoesNotRun(t *testing.T) {
	s := newStore(t)
	marker := filepath.Join(filepath.Dir(s.Dir), "PWNED")
	script := evil(t, marker)
	for _, h := range []string{"pre-commit", "post-commit", "commit-msg"} {
		os.Symlink(script, filepath.Join(s.Dir, ".git", "hooks", h))
	}
	if _, err := s.Write("semantic", "a", "b", nil, false); err == nil || !strings.Contains(err.Error(), "unexpected hook") {
		t.Fatalf("write with planted hook: %v", err)
	}
	// Layer 2: even if the startup check were bypassed, hooksPath neuters hooks.
	os.MkdirAll(filepath.Join(s.Dir, "semantic"), 0o755)
	os.WriteFile(filepath.Join(s.Dir, "semantic", "x.md"), []byte("x"), 0o644)
	if _, err := s.git("add", "--", "semantic/x.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.git("commit", "-q", "-m", "x"); err != nil {
		t.Fatal(err)
	}
	assertNoMarker(t, marker)
}

func TestMaliciousConfigDoesNotRun(t *testing.T) {
	for _, kv := range [][2]string{
		{"core.fsmonitor", ""},
		{"core.pager", ""},
		{"filter.x.clean", ""},
		{"diff.external", ""},
	} {
		t.Run(kv[0], func(t *testing.T) {
			s := newStore(t)
			marker := filepath.Join(filepath.Dir(s.Dir), "PWNED")
			plainGit(t, s.Dir, "config", kv[0], evil(t, marker))
			if kv[0] == "filter.x.clean" {
				os.WriteFile(filepath.Join(s.Dir, ".gitattributes"), []byte("* filter=x\n"), 0o644)
			}
			if _, err := s.Write("semantic", "a", "b", nil, false); err == nil || !strings.Contains(err.Error(), "unexpected git config") {
				t.Fatalf("write with %s: %v", kv[0], err)
			}
			// Layer 2: hardened flags alone.
			os.WriteFile(filepath.Join(s.Dir, "f.md"), []byte("x"), 0o644)
			for _, a := range [][]string{{"status"}, {"add", "f.md"}, {"diff", "--cached", "--no-ext-diff", "--no-textconv"}, {"log"}} {
				s.git(a...)
			}
			assertNoMarker(t, marker)
		})
	}
}

func TestSymlinkedParentRejected(t *testing.T) {
	s := newStore(t)
	// kind dir -> another kind dir inside the store: still refused.
	os.MkdirAll(filepath.Join(s.Dir, "semantic"), 0o755)
	os.WriteFile(filepath.Join(s.Dir, "semantic", "x.md"), []byte("x"), 0o644)
	if err := os.Symlink("semantic", filepath.Join(s.Dir, "episodic")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read("episodic/x.md"); err == nil {
		t.Error("read through in-store symlinked dir allowed")
	}
	if _, err := s.Write("episodic", "y", "z", nil, false); err == nil {
		t.Error("write through in-store symlinked dir allowed")
	}
	if _, err := os.Lstat(filepath.Join(s.Dir, "semantic", slug("y")+".md")); err == nil {
		t.Error("file created via symlink")
	}
	// file symlink to .git/config inside the store
	os.Symlink("../.git/config", filepath.Join(s.Dir, "semantic", slug("cfg")+".md"))
	if _, err := s.Write("semantic", "cfg", "[core]\nfsmonitor=/tmp/x\n", nil, false); err == nil {
		t.Error("write through file symlink allowed")
	}
	if b, _ := os.ReadFile(filepath.Join(s.Dir, ".git", "config")); strings.Contains(string(b), "fsmonitor") {
		t.Fatal(".git/config overwritten")
	}
}

func TestConcurrentWriters(t *testing.T) {
	s := newStore(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const procs, inproc = 6, 6
	var wg sync.WaitGroup
	errs := make(chan error, procs+inproc)
	for i := 0; i < procs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(exe, "write", "-dir", s.Dir, "-kind", "episodic", "-title", fmt.Sprintf("proc %d", i), "-body", "x")
			cmd.Env = append(os.Environ(), "PANDA_MEM_CHILD=1")
			if out, err := cmd.CombinedOutput(); err != nil {
				errs <- fmt.Errorf("proc %d: %v %s", i, err, out)
			}
		}(i)
	}
	for i := 0; i < inproc; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			other := &Store{Dir: s.Dir} // separate instance, separate lock fd
			if _, err := other.Write("semantic", fmt.Sprintf("store %d", i), "y", nil, false); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := commitCount(t, s); n != fmt.Sprint(procs+inproc) {
		t.Fatalf("commits %s, want %d", n, procs+inproc)
	}
}

func TestMultiTermSearch(t *testing.T) {
	s := newStore(t)
	s.Write("semantic", "both", "nix flakes are nice", nil, false)
	s.Write("semantic", "spam", "nix nix nix nix nix", nil, false)
	s.Write("semantic", "both-twice", "NIX flakes\nnix FLAKES", nil, false)
	s.Write("semantic", "none", "rust", nil, false)
	hits, err := s.Search("Flakes  nix nix", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range hits {
		got = append(got, h.Title)
	}
	if strings.Join(got, ",") != "both-twice,both,spam" {
		t.Fatalf("order %v (%+v)", got, hits)
	}
	if hits[0].Terms != 2 || hits[2].Terms != 1 || hits[1].Snippet != "nix flakes are nice" {
		t.Fatalf("hits %+v", hits)
	}
}

func TestSocketServer(t *testing.T) {
	s := newStore(t)
	tmp, _ := os.MkdirTemp("", "pm") // short path: unix socket path limit
	defer os.RemoveAll(tmp)
	sock := filepath.Join(tmp, "sock")
	go s.Serve(sock)
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st, err := os.Stat(sock); err != nil || st.Mode().Perm() != 0o660 {
		t.Fatalf("socket %v %v", st, err)
	}
	p, err := remoteTool(sock, "mem_write", toolArgs{Kind: "procedural", Title: "via socket", Body: "hello"})
	if err != nil || !strings.HasPrefix(p, "procedural/via-socket-") {
		t.Fatalf("remote write %q %v", p, err)
	}
	if _, err := remoteTool(sock, "mem_write", toolArgs{Kind: "bad", Title: "x"}); err == nil {
		t.Fatal("bad kind accepted over socket")
	}
	var out strings.Builder
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mem_search","arguments":{"query":"hello"}}}` + "\n")
	if err := proxy(sock, in, &out); err != nil || !strings.Contains(out.String(), p) {
		t.Fatalf("proxy %q %v", out.String(), err)
	}
}
