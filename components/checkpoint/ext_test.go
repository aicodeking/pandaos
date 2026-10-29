package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gitRepo(t *testing.T, p string) {
	t.Helper()
	write(t, filepath.Join(p, ".git", "HEAD"), "ref: refs/heads/main\n")
}

func TestExtSlug(t *testing.T) {
	seen := map[string]bool{}
	for _, rel := range []string{"src/app", "src_app", ".hidden/x", strings.Repeat("deep/", 30) + "x", "a b/ü"} {
		s := extSlug(rel)
		if validName("slug", s) != nil || seen[s] {
			t.Errorf("slug %q for %q invalid or duplicate", s, rel)
		}
		seen[s] = true
	}
}

func TestExtTargetValidation(t *testing.T) {
	m, _ := newTest(t)
	must(m.Init("w"))
	os.MkdirAll(filepath.Join(m.Home, "src", "app"), 0o755)
	os.Symlink(filepath.Join(m.Home, "src"), filepath.Join(m.Home, "link"))
	outside := t.TempDir()
	if tg, err := m.target("~/src/app"); err != nil || !tg.ext || tg.live != filepath.Join(m.Home, "src", "app") {
		t.Fatalf("~/ path: %+v %v", tg, err)
	}
	for _, bad := range []string{"src/app", outside, m.Home, filepath.Join(m.Home, "work", "w"), filepath.Join(m.Home, ".checkpoints"),
		"~/link/app", "~/missing", "~/src/../../etc", "_ext"} {
		if _, err := m.target(bad); err == nil {
			t.Errorf("target %q accepted", bad)
		}
	}
}

func TestDiscover(t *testing.T) {
	m, _ := newTest(t)
	must(m.Init("w"))
	gitRepo(t, filepath.Join(m.Home, "work", "w"))                        // in ~/work: not external
	gitRepo(t, filepath.Join(m.Home, "src", "app"))                       // depth 2
	gitRepo(t, filepath.Join(m.Home, "src", "app", "vendor", "sub"))      // inside a repo: skipped
	gitRepo(t, filepath.Join(m.Home, "a", "b", "c", "d"))                 // depth 4
	gitRepo(t, filepath.Join(m.Home, "a", "b", "c", "d2", "e"))           // depth 5: too deep
	gitRepo(t, filepath.Join(m.Home, ".cache", "x"))                      // skipped dir
	gitRepo(t, filepath.Join(m.Home, ".local", "share", "pc", "co"))      // hidden but searched
	write(t, filepath.Join(m.Home, "wt", ".git"), "gitdir: /elsewhere\n") // worktree: .git file
	got := must(m.Discover())
	var paths []string
	for _, r := range got {
		rel, _ := filepath.Rel(m.Home, r.Path)
		paths = append(paths, rel)
	}
	want := "[.local/share/pc/co a/b/c/d src/app wt]"
	if fmt.Sprint(paths) != want {
		t.Fatalf("discovered %v, want %s", paths, want)
	}
}

func TestExternalCreateDiffRestoreAuto(t *testing.T) {
	m, f := newTest(t)
	repo := filepath.Join(m.Home, "src", "app")
	gitRepo(t, repo)
	write(t, filepath.Join(repo, "main.go"), "v1\n")
	c1 := must(m.Create(repo, "before"))
	if c1.Source != repo {
		t.Fatalf("source %q", c1.Source)
	}
	snap := filepath.Join(m.ckptRoot(), extDir, extSlug("src/app"), c1.ID)
	if !f.ro[snap] || read(t, filepath.Join(snap, "main.go")) != "v1\n" || read(t, filepath.Join(snap, ".git", "HEAD")) == "" {
		t.Fatal("external checkpoint is not a read-only full copy")
	}
	var sawAlways bool
	for _, c := range f.calls {
		sawAlways = sawAlways || (c[0] == "cp" && c[2] == "--reflink=always")
	}
	if !sawAlways {
		t.Fatalf("no reflink=always copy: %v", f.calls)
	}

	write(t, filepath.Join(repo, "main.go"), "v2\n")
	write(t, filepath.Join(repo, "junk.txt"), "x")
	if d := must(m.Diff("~/src/app", c1.ID, "")); len(d) != 2 {
		t.Fatalf("diff %+v", d)
	}
	res := must(m.Restore(repo, c1.ID))
	if read(t, filepath.Join(repo, "main.go")) != "v1\n" || exists(filepath.Join(repo, "junk.txt")) || exists(filepath.Join(m.Home, "src", ".app.restore")) {
		t.Fatal("external restore")
	}
	if d := must(m.Diff(repo, res.Safety, "")); len(d) != 2 {
		t.Fatalf("safety copy lost the pre-restore state: %+v", d)
	}

	// auto: the repo is discovered; unchanged since restore (head = c1) -> skipped
	r := must(m.Auto(5, true))
	if len(r) != 1 || r[0].Workspace != repo || r[0].Skipped != "unchanged" {
		t.Fatalf("auto %+v", r)
	}
	write(t, filepath.Join(repo, "main.go"), "v3\n")
	if r = must(m.Auto(5, true)); r[0].Created == "" {
		t.Fatalf("auto did not checkpoint the changed repo: %+v", r)
	}
	if r = must(m.Auto(5, false)); len(r) != 0 {
		t.Fatalf("auto without external: %+v", r)
	}
	fork := must(m.Fork(repo, c1.ID, "app-try"))
	if read(t, filepath.Join(fork.Path, "main.go")) != "v1\n" {
		t.Fatal("fork from external checkpoint")
	}
	if n := len(must(m.GC())); n != 0 {
		t.Fatalf("gc removed %d healthy entries", n)
	}
	if got := must(m.Prune(repo, 1, 0)); len(got) != 2 {
		t.Fatalf("prune external %v", got)
	}
}

func TestAdoptRefusesBusyDir(t *testing.T) {
	m, _ := newTest(t)
	p := filepath.Join(m.workDir(), "old")
	write(t, filepath.Join(p, "f"), "x")
	m.Proc = t.TempDir()
	real, _ := filepath.EvalSymlinks(p)
	os.MkdirAll(filepath.Join(m.Proc, "4242", "fd"), 0o755)
	os.Symlink(real+"/f", filepath.Join(m.Proc, "4242", "fd", "3"))
	os.MkdirAll(filepath.Join(m.Proc, "77", "fd"), 0o755)
	os.Symlink("/tmp", filepath.Join(m.Proc, "77", "cwd"))
	if pids := m.busyPids(p); len(pids) != 1 || pids[0] != 4242 {
		t.Fatalf("busy %v", pids)
	}
	if _, err := m.Adopt(p, false); err == nil || !strings.Contains(err.Error(), "confirm") {
		t.Fatalf("busy adopt: %v", err)
	}
	if _, err := m.Adopt("~/work/old", true); err != nil {
		t.Fatal(err)
	}
}

func TestMCPInitAdoptListLimit(t *testing.T) {
	m, _ := newTest(t)
	m.Proc = t.TempDir()
	os.MkdirAll(filepath.Join(m.workDir(), "plain"), 0o755)
	call := func(name string, args map[string]any) (string, bool) {
		req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args}})
		var out bytes.Buffer
		if err := m.ServeMCP(bytes.NewReader(append(req, '\n')), &out); err != nil {
			t.Fatal(err)
		}
		var r struct {
			Result struct {
				Content []struct{ Text string }
				IsError bool
			}
		}
		json.Unmarshal(out.Bytes(), &r)
		return r.Result.Content[0].Text, r.Result.IsError
	}
	if txt, isErr := call("ckpt_init", map[string]any{"workspace": "fresh"}); isErr || !strings.Contains(txt, "fresh") {
		t.Fatalf("init %s", txt)
	}
	if txt, isErr := call("ckpt_adopt", map[string]any{"path": "~/work/plain"}); isErr {
		t.Fatalf("adopt %s", txt)
	}
	for i := 0; i < 25; i++ {
		must(m.Create("fresh", ""))
	}
	txt, _ := call("ckpt_list", map[string]any{"workspace": "fresh"})
	var list []Meta
	json.Unmarshal([]byte(txt), &list)
	all := must(m.List("fresh"))
	if len(list) != 20 || list[19].ID != all[24].ID {
		t.Fatalf("default limit: %d", len(list))
	}
	txt, _ = call("ckpt_list", map[string]any{"workspace": "fresh", "limit": 3})
	json.Unmarshal([]byte(txt), &list)
	if len(list) != 3 || list[0].ID != all[22].ID {
		t.Fatalf("limit 3: %v", ids(list))
	}
	if txt, _ = call("ckpt_list", nil); !strings.Contains(txt, `"external"`) || !strings.Contains(txt, `"fresh"`) {
		t.Fatalf("targets %s", txt)
	}
	if _, isErr := call("ckpt_discover", nil); isErr {
		t.Fatal("discover")
	}
}
