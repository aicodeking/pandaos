package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s := &Store{Dir: filepath.Join(t.TempDir(), "mem")}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	return s
}

func commitCount(t *testing.T, s *Store) string {
	t.Helper()
	out, err := s.git("rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func TestWriteReadUpdate(t *testing.T) {
	s := newStore(t)
	p, err := s.Write("semantic", "Nix flakes", "use flakes", []string{"nix"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, "semantic/nix-flakes-") || commitCount(t, s) != "1" {
		t.Fatalf("path %q commits %s", p, commitCount(t, s))
	}
	got, err := s.Read(p)
	if err != nil || !strings.Contains(got, "use flakes") || !strings.Contains(got, `title: "Nix flakes"`) {
		t.Fatalf("read %q %v", got, err)
	}
	created := parse(p, []byte(got)).Created

	p2, err := s.Write("semantic", "Nix flakes", "use flakes v2", nil)
	if err != nil || p2 != p {
		t.Fatalf("update %q %v", p2, err)
	}
	got, _ = s.Read(p)
	e := parse(p, []byte(got))
	if e.Body != "use flakes v2" || e.Created != created || commitCount(t, s) != "2" {
		t.Fatalf("after update: %+v commits %s", e, commitCount(t, s))
	}
	// identical rewrite: no new commit, no error
	if _, err := s.Write("semantic", "Nix flakes", "use flakes v2", nil); err != nil || commitCount(t, s) != "2" {
		t.Fatalf("noop write: %v commits %s", err, commitCount(t, s))
	}
	if h, _ := s.History(p, 20); strings.Count(h, "\n") != 1 {
		t.Fatalf("history %q", h)
	}
	if _, err := s.Write("semantic", "big", strings.Repeat("x", maxBody+1), nil); err == nil {
		t.Fatal("oversize body accepted")
	}
}

func TestSearchRanking(t *testing.T) {
	s := newStore(t)
	s.Write("semantic", "one", "golang once", nil)
	s.Write("procedural", "three", "Golang GOLANG golang", nil)
	s.Write("episodic", "none", "rust", nil)
	hits, err := s.Search("golang", "", 10)
	if err != nil || len(hits) != 2 || hits[0].Title != "three" || hits[0].Matches != 3 || hits[1].Title != "one" {
		t.Fatalf("hits %+v %v", hits, err)
	}
	if hits, _ := s.Search("golang", "semantic", 10); len(hits) != 1 {
		t.Fatalf("kind filter %+v", hits)
	}
	if l, _ := s.List("", 50); len(l) != 3 {
		t.Fatalf("list %+v", l)
	}
}

func TestPathTraversal(t *testing.T) {
	s := newStore(t)
	outside := filepath.Join(filepath.Dir(s.Dir), "secret.md")
	os.WriteFile(outside, []byte("secret"), 0o644)
	os.MkdirAll(filepath.Join(s.Dir, "semantic"), 0o755)
	os.Symlink(outside, filepath.Join(s.Dir, "semantic", "link.md"))
	for _, p := range []string{"../secret.md", "semantic/../../secret.md", outside, ".git/config", "semantic/link.md", "semantic/x.txt"} {
		if _, err := s.Read(p); err == nil {
			t.Errorf("read %q allowed", p)
		}
	}
	// symlinked kind dir pointing outside
	s2 := newStore(t)
	os.Symlink(filepath.Dir(s.Dir), filepath.Join(s2.Dir, "episodic"))
	if _, err := s2.Write("episodic", "x", "y", nil); err == nil {
		t.Error("write through symlinked dir allowed")
	}
	if _, err := s2.Read("episodic/secret.md"); err == nil {
		t.Error("read through symlinked dir allowed")
	}
}

type client struct {
	t   *testing.T
	w   io.Writer
	r   *bufio.Scanner
	ids int
}

func (c *client) call(method string, params any) map[string]any {
	c.t.Helper()
	c.ids++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.ids, "method": method, "params": params})
	c.w.Write(append(b, '\n'))
	if !c.r.Scan() {
		c.t.Fatalf("no response to %s", method)
	}
	var resp map[string]any
	if err := json.Unmarshal(c.r.Bytes(), &resp); err != nil {
		c.t.Fatal(err)
	}
	return resp
}

func (c *client) tool(name string, args any) (string, bool) {
	c.t.Helper()
	res := c.call("tools/call", map[string]any{"name": name, "arguments": args})["result"].(map[string]any)
	return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"].(bool)
}

func TestMCPSession(t *testing.T) {
	s := newStore(t)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { s.ServeMCP(inR, outW); outW.Close() }()
	defer inW.Close()
	c := &client{t: t, w: inW, r: bufio.NewScanner(outR)}

	init := c.call("initialize", map[string]any{"protocolVersion": "2025-03-26"})["result"].(map[string]any)
	if init["protocolVersion"] != "2025-03-26" || init["capabilities"].(map[string]any)["tools"] == nil {
		t.Fatalf("initialize %+v", init)
	}
	inW.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	if r := c.call("ping", nil); r["result"] == nil {
		t.Fatalf("ping %+v", r)
	}
	tl := c.call("tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	if len(tl) != 5 {
		t.Fatalf("tools %+v", tl)
	}
	path, isErr := c.tool("mem_write", map[string]any{"kind": "procedural", "title": "deploy", "body": "run nixos-rebuild switch", "tags": []string{"ops"}})
	if isErr || !strings.HasPrefix(path, "procedural/deploy-") {
		t.Fatalf("write %q", path)
	}
	out, isErr := c.tool("mem_search", map[string]any{"query": "NIXOS-REBUILD"})
	if isErr || !strings.Contains(out, path) {
		t.Fatalf("search %q", out)
	}
	if out, isErr := c.tool("mem_write", map[string]any{"kind": "bogus", "title": "x", "body": "y"}); !isErr || !strings.Contains(out, "invalid kind") {
		t.Fatalf("bad kind %q %v", out, isErr)
	}
	if out, isErr := c.tool("mem_read", map[string]any{"path": "../etc/passwd"}); !isErr {
		t.Fatalf("traversal %q", out)
	}
	if r := c.call("nope", nil); r["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("unknown method %+v", r)
	}
}
