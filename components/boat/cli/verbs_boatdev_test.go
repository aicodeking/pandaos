package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func boatdevAPI(t *testing.T) (*httptest.Server, *[]string) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/vms/bx_23456789", "GET /v1/vms/vm1":
			io.WriteString(w, `{"id":"vm1","name":"dev","state":"running"}`)
		case "GET /api/v1/sandboxes/vm1/events":
			io.WriteString(w, `{"ok":true,"events":[{"id":"p.prompt","type":"prompt","timestamp":0,"data":{"prompt":"hi"}}]}`)
		case "POST /api/v1/sandboxes/vm1/recover":
			io.WriteString(w, `{"ok":true,"message":"power-cycled"}`)
		case "POST /api/v1/sandboxes/vm1/share":
			io.WriteString(w, `{"sandbox":{"id":"bx_23456789"}}`)
		case "POST /api/v1/sandboxes/vm1/unshare":
			w.WriteHeader(501)
			io.WriteString(w, `{"error":{"code":"not_supported","message":"unshare is not supported on self-hosted boat"}}`)
		case "GET /api/v1/deletion-operations/bdop_1":
			io.WriteString(w, `{"ok":true,"operation":{"id":"bdop_1","targetId":"bx_23456789","status":"completed"}}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"code":"not_found","message":"no"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestBoatDevVerbs(t *testing.T) {
	srv, got := boatdevAPI(t)
	env := map[string]string{"BOAT_URL": "http://unused.invalid", "BOAT_API_KEY": "boat_k", "BOAT_CONFIG": filepath.Join(t.TempDir(), "c.json")}
	for _, c := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"--api-url", srv.URL, "status", "bx_23456789"}, 0, "vm1\tdev\trunning"},
		{[]string{"--api-url=" + srv.URL, "events", "vm1"}, 0, "prompt\thi"},
		{[]string{"--api-url", srv.URL, "recover", "vm1"}, 0, "power-cycled"},
		{[]string{"--api-url", srv.URL, "share", "vm1"}, 0, "shared"},
		{[]string{"--api-url", srv.URL, "unshare", "vm1"}, 1, "not supported on self-hosted"},
		{[]string{"--api-url", srv.URL, "deletion", "status", "bdop_1"}, 0, "bdop_1\tbx_23456789\tcompleted"},
		{[]string{"--api-url", srv.URL, "dashboard", "--no-open"}, 0, srv.URL + "/"},
		{[]string{"orgs", "list"}, 1, "not supported on self-hosted"},
		{[]string{"deletion", "vm1"}, 2, ""},
	} {
		a, out, errw := acctApp(env, "")
		if code := a.run(c.args); code != c.code || !strings.Contains(out.String()+errw.String(), c.out) {
			t.Errorf("%v: exit %d, out %q err %q", c.args, code, out, errw)
		}
	}
	if (*got)[0] != "GET /v1/vms/bx_23456789" {
		t.Errorf("--api-url not used: %v", *got)
	}
}

func TestAPIURLOnLogin(t *testing.T) {
	srv, _ := acctAPI(t)
	env := map[string]string{"BOAT_CONFIG": filepath.Join(t.TempDir(), "c.json")}
	a, _, errw := acctApp(env, "")
	if code := a.run([]string{"--api-url", srv.URL, "login", "boat_k"}); code != 0 {
		t.Fatalf("login: %d %s", code, errw)
	}
	if cfg, _ := loadConfig(a.getenv); cfg.URL != srv.URL {
		t.Errorf("saved url %q", cfg.URL)
	}
}
