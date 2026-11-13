package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// r12: a compat route must never run a native handler whose scope its caller
// lacks (POST …/host with vms:write reached the exec-scoped port URL).

// scopeProbeBodies are bodies that get each route as far as its native calls.
func scopeProbeBodies(bx string) map[string]any {
	return map[string]any{
		"POST /api/v1/sandboxes":                  map[string]any{"hostedPorts": []any{map[string]any{"port": 8080}}},
		"POST /api/v1/sandboxes/{id}/host":        map[string]any{"port": 8080},
		"POST /api/v1/saved-snapshots":            map[string]any{"sandboxId": bx, "name": "probe2"},
		"POST /api/v1/named-snapshots":            map[string]any{"sandboxId": bx, "name": "probe3"},
		"POST /api/v1/sandboxes/{id}/commands":    map[string]any{"command": "true"},
		"POST /api/v1/sandboxes/{id}/prompt":      map[string]any{"prompt": "hi"},
		"POST /api/v1/sandboxes/{id}/files/move":  map[string]any{"from": "/tmp/a", "to": "/tmp/b"},
		"POST /api/v1/sandboxes/{id}/files/folder": map[string]any{"path": "/tmp/d"},
	}
}

func TestCompatNativeCallsStayInScope(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMDomain = "vm.example"
	bx := e.newSandbox(nil)
	e.ok("POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": bx, "name": "probe"}, 202)
	var mu sync.Mutex
	var holes []string
	var route string
	e.srv.onNative = func(pattern string, p *principal) {
		need, known := e.srv.routeScopes[pattern]
		if !known || (need != "" && !p.has(need)) {
			mu.Lock()
			holes = append(holes, route+" ("+strings.Join(p.Scopes, ",")+") ran "+pattern+" needing "+need)
			mu.Unlock()
		}
	}
	params := strings.NewReplacer("{id}", bx, "{name}", "probe", "{port}", "8080", "{sid}", "snap-x",
		"{pid}", "p1", "{cid}", "c1", "{stopid}", "s1", "{opid}", "o1", "{share}", "sh1",
		"{key}", "K", "{provider}", "anthropic", "{token}", "t")
	bodies := scopeProbeBodies(bx)
	keys := map[string]string{}
	for _, sc := range allScopes {
		keys[sc] = e.newKey(sc)
	}
	var patterns []string
	for p := range e.srv.routeScopes {
		patterns = append(patterns, p)
	}
	slices.Sort(patterns)
	n := 0
	for _, pattern := range patterns {
		method, path, _ := strings.Cut(pattern, " ")
		if !strings.HasPrefix(path, compatPrefix) || strings.HasSuffix(path, "/stream") || strings.HasSuffix(path, "/events") {
			continue
		}
		need := e.srv.routeScopes[pattern]
		for _, sc := range allScopes {
			if need != "" && need != sc {
				continue // the route itself refuses this key
			}
			route = pattern
			e.compat(keys[sc], method, params.Replace(path), bodies[pattern], nil)
			n++
		}
	}
	if n < 100 {
		t.Fatalf("probed only %d route×key pairs", n)
	}
	for _, h := range holes {
		t.Error(h)
	}
}

// Every pattern handed to callNative is a registered route, so its scope
// lookup never misses (callNative refuses an unknown one).
func TestCallNativePatternsRegistered(t *testing.T) {
	e := setup(t)
	re := regexp.MustCompile(`callNative\([^,]+, [^,]+, "([A-Z]+ [^"]+)"`)
	files, _ := filepath.Glob("*.go")
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			seen++
			if _, ok := e.srv.routeScopes[m[1]]; !ok {
				t.Errorf("%s: callNative(%q) is no registered route", f, m[1])
			}
		}
	}
	if seen < 40 {
		t.Fatalf("found only %d callNative patterns", seen)
	}
}
