package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func r26App(env map[string]string, out io.Writer) *app {
	return &app{out: out, errw: io.Discard, in: strings.NewReader(""), getenv: func(k string) string { return env[k] }}
}

func TestR26APIOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"http://h:1/": "http://h:1", "http://h/api/v1": "http://h", "http://h/api/v1/": "http://h",
		"http://h/v1": "http://h", "http://h": "http://h", "http://h/x/v1/": "http://h/x",
	} {
		if got := apiOrigin(in); got != want {
			t.Errorf("apiOrigin(%q)=%q want %q", in, got, want)
		}
	}
	got, _ := loadConfig(func(k string) string {
		return map[string]string{"BOAT_CONFIG": "/nonexistent", "BOAT_URL": "http://h/v1/"}[k]
	})
	if got.URL != "http://h" {
		t.Errorf("BOAT_URL not normalized: %q", got.URL)
	}
}

func TestR26APIURLOverridesConfig(t *testing.T) {
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{}, "vms": []any{}})
	}))
	defer srv.Close()
	cfg := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(cfg, []byte(`{"url":"http://127.0.0.1:1","key":"k"}`), 0o600)
	env := map[string]string{"BOAT_CONFIG": cfg}
	a := r26App(env, io.Discard)
	a.apiURL = srv.URL + "/api/v1"
	if c, _ := a.loadCfg(); c.URL != srv.URL {
		t.Fatalf("loadCfg url %q", c.URL)
	}
	if code := a.run([]string{"--api-url", srv.URL + "/api/v1/", "ls"}); code != 0 || gotHost == "" {
		t.Fatalf("exit %d path %q", code, gotHost)
	}
}

func TestR26StatusSignedOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	env := map[string]string{"BOAT_CONFIG": filepath.Join(t.TempDir(), "none.json")}
	var out bytes.Buffer
	a := r26App(env, &out)
	if code := a.run([]string{"--api-url", srv.URL + "/api/v1", "status", "--json"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var v struct {
		API struct {
			URL string
			OK  bool
		} `json:"api"`
		Account struct{ SignedIn bool } `json:"account"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatal(err, out.String())
	}
	if v.API.URL != srv.URL || !v.API.OK || v.Account.SignedIn {
		t.Errorf("status %s", out.String())
	}
	// no URL at all still works, and --check exits 1
	out.Reset()
	if code := r26App(env, &out).run([]string{"status", "--check"}); code != 1 {
		t.Errorf("--check exit %d", code)
	}
}

func TestR26RequestID(t *testing.T) {
	for name, body := range map[string]string{
		"top":    `{"error":"nope","requestId":"req_top"}`,
		"obj":    `{"error":{"code":"c","message":"nope","requestId":"req_top"}}`,
		"nested": `{"error":{"code":"c","message":"nope","request_id":"req_top"}}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			w.Write([]byte(body))
		}))
		c := &client{base: srv.URL, key: "k", hc: &http.Client{}}
		err := c.call("GET", "/x", nil, nil)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "req_top") {
			t.Errorf("%s: error %v lacks request id", name, err)
		}
	}
}

func TestR26NoEnvSendsEmptySecrets(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			raw, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(raw), `"secrets":[]`) {
				t.Errorf("body %s lacks explicit empty secrets", raw)
			}
			json.Unmarshal(raw, &body)
		}
		w.Write([]byte(`{"id":"vm1","name":"x","state":"running"}`))
	}))
	defer srv.Close()
	env := map[string]string{"BOAT_CONFIG": "/nonexistent", "BOAT_URL": srv.URL, "BOAT_API_KEY": "k"}
	r26App(env, io.Discard).run([]string{"new", "x", "--no-env", "--json"})
	if s, ok := body["secrets"].([]any); !ok || len(s) != 0 {
		t.Errorf("secrets %#v", body["secrets"])
	}
}
