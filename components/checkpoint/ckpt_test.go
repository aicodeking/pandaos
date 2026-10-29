package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBtrfs interprets the btrfs/cp argv the Manager issues, using plain
// directories: subvolumes are tracked in a set, snapshots are deep copies.
type fakeBtrfs struct {
	mu      sync.Mutex
	subvols map[string]bool
	ro      map[string]bool
	calls   [][]string
	noRm    bool // simulate a mount without user_subvol_rm_allowed
}

func (f *fakeBtrfs) isSubvol(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subvols[p]
}

func (f *fakeBtrfs) run(argv ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, argv)
	a := strings.Join(argv[:min(3, len(argv))], " ")
	last := argv[len(argv)-1]
	switch {
	case a == "btrfs subvolume create":
		if exists(last) {
			return fmt.Errorf("exists: %s", last)
		}
		f.subvols[last] = true
		return os.Mkdir(last, 0o755)
	case a == "btrfs subvolume snapshot":
		src, ro := argv[len(argv)-2], argv[3] == "-r"
		if !f.subvols[src] {
			return fmt.Errorf("not a subvolume: %s", src)
		}
		if exists(last) {
			return fmt.Errorf("exists: %s", last)
		}
		f.subvols[last], f.ro[last] = true, ro
		return copyTree(src, last)
	case a == "btrfs subvolume delete":
		if f.noRm {
			return fmt.Errorf("EPERM")
		}
		if !f.subvols[last] || f.ro[last] {
			return fmt.Errorf("cannot delete %s", last)
		}
		delete(f.subvols, last)
		return os.RemoveAll(last)
	case a == "btrfs property set":
		if !f.subvols[argv[4]] {
			return fmt.Errorf("not a subvolume")
		}
		f.ro[argv[4]] = false
		return nil
	case argv[0] == "cp":
		return copyTree(strings.TrimSuffix(argv[len(argv)-2], "/."), strings.TrimSuffix(last, "/"))
	}
	return fmt.Errorf("fake: unexpected %v", argv)
}

// copyTree copies src into dst (created if needed), keeping modes and mtimes.
func copyTree(src, dst string) error {
	type dirTime struct {
		p string
		t time.Time
	}
	var dirs []dirTime
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if err := os.MkdirAll(to, fi.Mode().Perm()); err != nil {
				return err
			}
			dirs = append(dirs, dirTime{to, fi.ModTime()})
		case fi.Mode()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, to)
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := os.WriteFile(to, b, fi.Mode().Perm()); err != nil {
				return err
			}
			return os.Chtimes(to, fi.ModTime(), fi.ModTime())
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Chtimes(dirs[i].p, dirs[i].t, dirs[i].t)
	}
	return err
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Minute)
	return c.t
}

func newTest(t *testing.T) (*Manager, *fakeBtrfs) {
	t.Helper()
	f := &fakeBtrfs{subvols: map[string]bool{}, ro: map[string]bool{}}
	c := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	m := &Manager{Home: t.TempDir(), Run: f.run, IsSubvol: f.isSubvol, Now: c.now}
	return m, f
}

// must panics on error, which fails the test with a stack trace.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func ids(list []Meta) []string {
	out := []string{}
	for _, c := range list {
		out = append(out, c.ID)
	}
	return out
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "my-ws_1.2", strings.Repeat("x", 64)} {
		if validName("ws", ok) != nil {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", ".hidden", "a/b", "../x", "a b", strings.Repeat("x", 65), "ü", "a\n"} {
		if validName("ws", bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCreateListDiffRestoreFork(t *testing.T) {
	m, _ := newTest(t)
	ws := must(m.Init("proj"))
	write(t, filepath.Join(ws.Path, "a.txt"), "one\n")
	write(t, filepath.Join(ws.Path, "sub/b.txt"), "keep\n")
	c1 := must(m.Create("proj", "first"))
	if c1.Reason != reasonManual || c1.Label != "first" || c1.SizeEstimate != 9 || c1.Parent != "" {
		t.Fatalf("meta %+v", c1)
	}

	write(t, filepath.Join(ws.Path, "a.txt"), "two\n")
	write(t, filepath.Join(ws.Path, "new.bin"), "\x00\x01")
	os.Remove(filepath.Join(ws.Path, "sub/b.txt"))

	ch := must(m.Diff("proj", c1.ID, "current"))
	kinds := map[string]string{}
	for _, c := range ch {
		kinds[c.Path] = c.Kind
	}
	want := map[string]string{"a.txt": "modified", "new.bin": "added", "sub/b.txt": "deleted"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("diff kinds %v", kinds)
	}
	for _, c := range ch {
		switch c.Path {
		case "a.txt":
			if !strings.Contains(c.Diff, "-one") || !strings.Contains(c.Diff, "+two") || !strings.Contains(c.Diff, "a/a.txt") {
				t.Fatalf("a.txt diff %q", c.Diff)
			}
		case "new.bin":
			if c.Diff != "" {
				t.Fatalf("binary got a diff %q", c.Diff)
			}
		case "sub/b.txt":
			if !strings.Contains(c.Diff, "-keep") {
				t.Fatalf("deleted diff %q", c.Diff)
			}
		}
	}

	c2 := must(m.Create("proj", ""))
	if c2.Parent != c1.ID || c2.Label != reasonManual {
		t.Fatalf("c2 %+v", c2)
	}
	if got := must(m.Diff("proj", c1.ID, c2.ID)); len(got) != 3 {
		t.Fatalf("ckpt-to-ckpt diff %v", got)
	}

	res := must(m.Restore("proj", c1.ID))
	if read(t, filepath.Join(ws.Path, "a.txt")) != "one\n" || read(t, filepath.Join(ws.Path, "sub/b.txt")) != "keep\n" || exists(filepath.Join(ws.Path, "new.bin")) {
		t.Fatal("restore did not bring content back")
	}
	if res.Safety == "" || res.Warning != "" || exists(filepath.Join(m.workDir(), ".proj.restore")) {
		t.Fatalf("restore %+v", res)
	}
	list := must(m.List("proj"))
	if len(list) != 3 || list[2].ID != res.Safety || list[2].Reason != reasonPreRestore {
		t.Fatalf("list after restore %+v", list)
	}
	if got := must(m.Diff("proj", res.Safety, "")); len(got) != 3 {
		t.Fatalf("safety checkpoint lost the pre-restore state: %v", got)
	}
	if m.head("proj") != c1.ID {
		t.Fatalf("head %q", m.head("proj"))
	}

	fork := must(m.Fork("proj", c2.ID, "proj-b"))
	write(t, filepath.Join(fork.Path, "a.txt"), "fork\n")
	if read(t, filepath.Join(ws.Path, "a.txt")) != "one\n" {
		t.Fatal("fork is not independent")
	}
	if c := must(m.Create("proj-b", "x")); c.Parent != "proj/"+c2.ID {
		t.Fatalf("fork parent %q", c.Parent)
	}
	if _, err := m.Fork("proj", c2.ID, "proj-b"); err == nil {
		t.Fatal("fork over an existing workspace")
	}
}

func TestRejectsBadInputAndSymlinks(t *testing.T) {
	m, _ := newTest(t)
	must(m.Init("ws"))
	c := must(m.Create("ws", ""))
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(m.workDir(), "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("link", ""); err == nil {
		t.Fatal("checkpointed a symlinked workspace")
	}
	if err := os.Symlink(outside, filepath.Join(m.ckptRoot(), "ws", "evil")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Diff("ws", "evil", ""); err == nil {
		t.Fatal("followed a symlinked checkpoint")
	}
	for _, bad := range [][2]string{{"../ws", c.ID}, {"ws", "../../x"}, {"ws", ".head"}} {
		if _, err := m.Restore(bad[0], bad[1]); err == nil {
			t.Fatalf("restore %v accepted", bad)
		}
	}
	if _, err := m.Fork("ws", c.ID, "../escape"); err == nil {
		t.Fatal("fork name accepted")
	}
	if _, err := m.Init("ws"); err == nil {
		t.Fatal("init over existing")
	}
	if _, err := m.Adopt(outside); err == nil {
		t.Fatal("adopted a dir outside ~/work")
	}
}

func TestAutoRetention(t *testing.T) {
	m, _ := newTest(t)
	ws := must(m.Init("w"))
	must(m.Create("w", "manual-1"))
	for i := 0; i < 5; i++ {
		write(t, filepath.Join(ws.Path, "f"), fmt.Sprint(i))
		os.Chtimes(filepath.Join(ws.Path, "f"), time.Unix(int64(1000+i), 0), time.Unix(int64(1000+i), 0))
		r := must(m.Auto(3))
		if len(r) != 1 || r[0].Created == "" {
			t.Fatalf("auto %d: %+v", i, r)
		}
	}
	r := must(m.Auto(3))
	if r[0].Created != "" || r[0].Skipped != "unchanged" {
		t.Fatalf("unchanged workspace checkpointed: %+v", r)
	}
	list := must(m.List("w"))
	var autos, manual int
	for _, c := range list {
		if c.Reason == reasonAuto {
			autos++
		} else {
			manual++
		}
	}
	if autos != 3 || manual != 1 {
		t.Fatalf("retention: %d auto %d manual: %v", autos, manual, ids(list))
	}
	// plain directories are skipped, not failed
	os.Mkdir(filepath.Join(m.workDir(), "plain"), 0o755)
	r = must(m.Auto(3))
	if len(r) != 2 || r[0].Workspace != "plain" || !strings.Contains(r[0].Skipped, "subvolume") {
		t.Fatalf("plain dir: %+v", r)
	}
}

func TestPrune(t *testing.T) {
	m, _ := newTest(t)
	must(m.Init("w"))
	for i := 0; i < 5; i++ {
		must(m.Create("w", ""))
	}
	all := ids(must(m.List("w")))
	if _, err := m.Prune("w", 0, 0); err == nil {
		t.Fatal("prune without limits")
	}
	// clock advances 1 min per call; nothing is older than a day
	if got := must(m.Prune("w", 1, 24*time.Hour)); len(got) != 0 {
		t.Fatalf("pruned young %v", got)
	}
	got := must(m.Prune("w", 2, 0))
	if !reflect.DeepEqual(got, all[:3]) || !reflect.DeepEqual(ids(must(m.List("w"))), all[3:]) {
		t.Fatalf("pruned %v left %v", got, ids(must(m.List("w"))))
	}
	if exists(filepath.Join(m.ckptRoot(), "w", all[0])) || exists(filepath.Join(m.ckptRoot(), "w", all[0]+".json")) {
		t.Fatal("pruned files remain")
	}
}

func TestDeleteFallbackWithoutRmOption(t *testing.T) {
	m, f := newTest(t)
	must(m.Init("w"))
	must(m.Create("w", ""))
	f.noRm = true
	if got := must(m.Prune("w", 0, time.Nanosecond)); len(got) != 1 {
		t.Fatalf("fallback delete %v", got)
	}
	if len(must(m.List("w"))) != 0 {
		t.Fatal("checkpoint still listed")
	}
}

func TestAdopt(t *testing.T) {
	m, f := newTest(t)
	p := filepath.Join(m.workDir(), "old")
	write(t, filepath.Join(p, "x/y.txt"), "data")
	ws := must(m.Adopt(p))
	// the fake does not follow renames, so check the subvolume+copy calls
	if fmt.Sprint(f.calls[0]) != "[btrfs subvolume create "+filepath.Join(m.workDir(), ".old.adopt")+"]" || f.calls[1][0] != "cp" ||
		ws.Path != p || read(t, filepath.Join(p, "x/y.txt")) != "data" || exists(filepath.Join(m.workDir(), ".old.adopt")) {
		t.Fatal("adopt")
	}
	m.IsSubvol = func(string) bool { return true }
	if _, err := m.Adopt(p); err == nil {
		t.Fatal("adopted a subvolume")
	}
}

func TestGC(t *testing.T) {
	m, f := newTest(t)
	must(m.Init("w"))
	c := must(m.Create("w", ""))
	dir := filepath.Join(m.ckptRoot(), "w")
	os.WriteFile(filepath.Join(dir, "ghost.json"), []byte("{}"), 0o600)
	f.run("btrfs", "subvolume", "snapshot", "-r", filepath.Join(m.workDir(), "w"), filepath.Join(dir, "half"))
	f.run("btrfs", "subvolume", "create", filepath.Join(m.workDir(), ".w.restore"))
	got := must(m.GC())
	if len(got) != 3 {
		t.Fatalf("gc removed %v", got)
	}
	if l := ids(must(m.List("w"))); !reflect.DeepEqual(l, []string{c.ID}) {
		t.Fatalf("gc touched good checkpoint: %v", l)
	}
}

func TestConcurrentCreatesGetDistinctIDs(t *testing.T) {
	m, _ := newTest(t)
	must(m.Init("w"))
	m.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) } // same second for all
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Create("w", "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := len(must(m.List("w"))); n != 8 {
		t.Fatalf("%d checkpoints", n)
	}
}

func TestRealArgv(t *testing.T) {
	m, f := newTest(t)
	must(m.Init("w"))
	c := must(m.Create("w", ""))
	must(m.Prune("w", 0, time.Nanosecond))
	snap := filepath.Join(m.ckptRoot(), "w", c.ID)
	want := [][]string{
		{"btrfs", "subvolume", "create", filepath.Join(m.workDir(), "w")},
		{"btrfs", "subvolume", "snapshot", "-r", filepath.Join(m.workDir(), "w"), snap},
		{"btrfs", "property", "set", "-ts", snap, "ro", "false"},
		{"btrfs", "subvolume", "delete", snap},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("argv\n%v\nwant\n%v", f.calls, want)
	}
}

func TestCLIAndMCP(t *testing.T) {
	m, _ := newTest(t)
	var out bytes.Buffer
	cli := func(args ...string) string {
		t.Helper()
		out.Reset()
		if err := runWith(m, args, nil, &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}
	cli("init", "w", "--json")
	var meta Meta
	if err := json.Unmarshal([]byte(cli("create", "w", "-m", "pre refactor", "--json")), &meta); err != nil || meta.Label != "pre refactor" {
		t.Fatalf("create --json: %v %+v", err, meta)
	}
	if !strings.Contains(cli("list", "w"), "pre refactor") {
		t.Fatal("list")
	}
	for _, bad := range [][]string{{"prune", "w", "--older-than", "x"}, {"create"}, {"nope"}, {"auto", "--keep-auto", "0"}} {
		if err := runWith(m, bad, nil, &out); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
	write(t, filepath.Join(m.workDir(), "w", "f.txt"), "hi\n")
	if s := cli("diff", "w", meta.ID); !strings.Contains(s, "added    f.txt") || !strings.Contains(s, "+hi") {
		t.Fatalf("diff output %q", s)
	}
	if s := cli("auto"); !strings.Contains(s, "created=2") {
		t.Fatalf("auto output %q", s)
	}
	cli("fork", "w", meta.ID, "w2")
	if s := cli("list"); !strings.Contains(s, "w2\t") {
		t.Fatalf("workspaces %q", s)
	}
	if s := cli("restore", "w", meta.ID); !strings.Contains(s, "safety checkpoint") {
		t.Fatalf("restore output %q", s)
	}
	cli("prune", "w", "--keep", "3", "--older-than", "0d")
	cli("gc", "--json")
	cli("version")

	reqs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ckpt_create","arguments":{"workspace":"w","label":"mcp"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"ckpt_restore","arguments":{"workspace":"w","id":"` + meta.ID + `"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"ckpt_restore","arguments":{"workspace":"w","id":"` + meta.ID + `","confirm":true}}}`,
	}
	out.Reset()
	if err := m.ServeMCP(strings.NewReader(strings.Join(reqs, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var r map[string]any
		json.Unmarshal(sc.Bytes(), &r)
		resps = append(resps, r)
	}
	if len(resps) != 5 {
		t.Fatalf("%d responses", len(resps))
	}
	toolsList := fmt.Sprint(resps[1]["result"])
	for _, name := range []string{"ckpt_create", "ckpt_list", "ckpt_diff", "ckpt_restore", "ckpt_fork", "BEFORE risky"} {
		if !strings.Contains(toolsList, name) {
			t.Fatalf("tools/list lacks %q", name)
		}
	}
	isErr := func(i int) bool { return resps[i]["result"].(map[string]any)["isError"].(bool) }
	if isErr(2) || !isErr(3) || isErr(4) {
		t.Fatalf("create/restore(no confirm)/restore: %v", resps[2:])
	}
}
