package main

import (
	"encoding/json"
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
		"POST /api/v1/sandboxes":                   map[string]any{"hostedPorts": []any{map[string]any{"port": 8080}}},
		"POST /api/v1/sandboxes/{id}/host":         map[string]any{"port": 8080},
		"POST /api/v1/saved-snapshots":             map[string]any{"sandboxId": bx, "name": "probe2"},
		"POST /api/v1/named-snapshots":             map[string]any{"sandboxId": bx, "name": "probe3"},
		"POST /api/v1/sandboxes/{id}/commands":     map[string]any{"command": "true"},
		"POST /api/v1/sandboxes/{id}/prompt":       map[string]any{"prompt": "hi"},
		"POST /api/v1/sandboxes/{id}/files/move":   map[string]any{"from": "/tmp/a", "to": "/tmp/b"},
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

// A download URL dies when its minting key no longer holds the scope minting
// needed (exec), not only when the key is gone.
func TestDownloadURLRechecksScope(t *testing.T) {
	e := setup(t)
	id := e.newSandbox(nil)
	base := "/api/v1/sandboxes/" + id
	e.ok("PUT", base+"/files", map[string]any{"path": "r.txt", "content": "x"}, 200)
	tok := e.newKey("exec", "vms:read")
	st, d, _ := e.compat(tok, "POST", base+"/files/download-url", map[string]any{"path": "r.txt"}, nil)
	if st != 200 {
		t.Fatalf("mint: %d %v", st, d)
	}
	u := strings.TrimPrefix(d["url"].(string), e.api.URL)
	if st, _ := e.raw("", "GET", u, nil); st != 200 {
		t.Fatalf("live: %d", st)
	}
	kid := keyID(e, tok)
	e.srv.store.Update(func(st *State) error {
		k := *st.Keys[kid]
		k.Scopes = []string{"vms:read"}
		st.Keys[kid] = &k
		return nil
	})
	if st, _ := e.raw("", "GET", u, nil); st != 404 {
		t.Errorf("URL outlived its key's exec scope: %d", st)
	}
}

// Links a VM's own token minted die when that token rotates (fork, share, start).
func TestSelfTokenLinksDieOnRotate(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example/"
	e.srv.cfg.VMDomain = "vm.example"
	vm := e.createVM()
	self := e.selfTokenOf(vm)
	if st, _ := e.do(self, "POST", "/v1/vms/"+vm+"/ports", map[string]any{"port": 8081}); st != 201 {
		t.Fatalf("port: %d", st)
	}
	if st, out := e.do(self, "GET", "/v1/vms/"+vm+"/ports/8081/url", nil); st != 200 || out["token"] == nil {
		t.Fatalf("url: %d %v", st, out)
	}
	e.do(self, "PUT", "/v1/vms/"+vm+"/files?path=/tmp/r.txt", "x")
	st, d := e.do(self, "POST", "/api/v1/sandboxes/"+vm+"/files/download-url", map[string]any{"path": "/tmp/r.txt"})
	if st != 200 {
		t.Fatalf("download-url: %d %v", st, d)
	}
	u := strings.TrimPrefix(d["url"].(string), e.api.URL)
	gen := e.srv.store.Snapshot().RouteGens[vm]
	e.srv.rotateSelf(vm)
	if g := e.srv.store.Snapshot().RouteGens[vm]; g == gen {
		t.Error("port links the old self token minted survive its rotation")
	}
	if st, _ := e.raw("", "GET", u, nil); st != 404 {
		t.Errorf("download URL survives the self token's rotation: %d", st)
	}
}

// boat.dev's spec is served where its tools look: /api/v1/openapi.json and
// the API root's /openapi.json and /openapi.yaml, paths relative to /api/v1.
func TestCompatOpenAPIServed(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/api/v1/openapi.json", "/openapi.json", "/openapi.yaml"} {
		st, b := e.raw("", "GET", p, nil)
		var spec struct {
			Servers []struct{ URL string }
			Paths   map[string]any
		}
		if st != 200 || json.Unmarshal(b, &spec) != nil {
			t.Fatalf("%s: %d", p, st)
		}
		if len(spec.Servers) != 1 || spec.Servers[0].URL != "/api/v1" || spec.Paths["/sandboxes"] == nil || spec.Paths["/sandboxes/{id}/host"] == nil {
			t.Errorf("%s: servers %v, %d paths", p, spec.Servers, len(spec.Paths))
		}
		for k := range spec.Paths {
			if strings.HasPrefix(k, "/v1/") || strings.HasPrefix(k, "/api/") {
				t.Errorf("%s: native or unstripped path %s", p, k)
			}
		}
	}
}
