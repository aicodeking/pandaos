// Byte-identical in components/mem, components/checkpoint and components/sysmcp
// (see mcpserve.go). Each component defines mcpTools.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
)

func TestSharedMCPFilesIdentical(t *testing.T) {
	for _, f := range []string{"mcpserve.go", "mcpserve_test.go"} {
		mine, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, sib := range []string{"../mem/", "../checkpoint/", "../sysmcp/"} {
			other, err := os.ReadFile(sib + f)
			if errors.Is(err, fs.ErrNotExist) {
				continue // Nix sandbox: only this component's source
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(mine, other) {
				t.Errorf("%s%s differs from ./%s; keep the copies identical", sib, f, f)
			}
		}
	}
}

func TestNegotiate(t *testing.T) {
	for in, want := range map[string]string{
		"2025-06-18": "2025-06-18", "2025-03-26": "2025-03-26", "2024-11-05": "2024-11-05",
		"": protocolVersions[0], "1999-01-01": protocolVersions[0], "2099-01-01": protocolVersions[0],
	} {
		if got := negotiate(in); got != want {
			t.Errorf("negotiate(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestToolsDocumented: every tool has a description, MCP annotations with
// all three hints, and a description on every parameter.
func TestToolsDocumented(t *testing.T) {
	var tools []struct {
		Name        string
		Description string
		InputSchema struct {
			Type       string
			Required   []string
			Properties map[string]struct{ Description string }
		}
		Annotations map[string]any
	}
	if err := json.Unmarshal(mcpTools, &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) == 0 {
		t.Fatal("no tools")
	}
	for _, tl := range tools {
		if len(tl.Description) < 20 || tl.InputSchema.Type != "object" {
			t.Errorf("%s: short description or non-object schema", tl.Name)
		}
		for _, h := range []string{"readOnlyHint", "destructiveHint", "idempotentHint"} {
			if _, ok := tl.Annotations[h].(bool); !ok {
				t.Errorf("%s: annotations.%s missing", tl.Name, h)
			}
		}
		if ro, _ := tl.Annotations["readOnlyHint"].(bool); ro && tl.Annotations["destructiveHint"] == true {
			t.Errorf("%s: read-only and destructive", tl.Name)
		}
		for p, s := range tl.InputSchema.Properties {
			if strings.TrimSpace(s.Description) == "" {
				t.Errorf("%s.%s: no description", tl.Name, p)
			}
		}
		for _, r := range tl.InputSchema.Required {
			if _, ok := tl.InputSchema.Properties[r]; !ok {
				t.Errorf("%s: required %q not in properties", tl.Name, r)
			}
		}
	}
}

func TestServeProtocol(t *testing.T) {
	s := &mcpServer{Name: "t", Version: "0", Tools: mcpTools, Call: func(name string, _ json.RawMessage) (string, error) {
		if name == "boom" {
			return "", errors.New("bad")
		}
		return "ok:" + name, nil
	}}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"9999-01-01"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`not json`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"x"}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"boom","arguments":null}}`,
		`{"jsonrpc":"2.0","id":5,"method":"nope"}`,
		`{"jsonrpc":"2.0","id":6,"method":"ping"}`,
	}, "\n")
	var out bytes.Buffer
	if err := s.Serve(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 7 {
		t.Fatalf("%d replies: %s", len(lines), out.String())
	}
	for i, want := range []string{
		`"protocolVersion":"2024-11-05"`, `"protocolVersion":"` + protocolVersions[0] + `"`, `"code":-32700`,
		`"text":"ok:x"`, `"isError":true`, `"code":-32601`, `"id":6,"result":{}`,
	} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("reply %d %s lacks %s", i, lines[i], want)
		}
	}
}
