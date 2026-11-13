package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestHelpWithoutLogin(t *testing.T) {
	env := map[string]string{"BOAT_CONFIG": filepath.Join(t.TempDir(), "none.json")}
	for _, args := range [][]string{{"ssh", "--help"}, {"help", "ssh"}, {"help"}, {"snapshots", "-h"}} {
		a, out, errw := acctApp(env, "")
		if code := a.run(args); code != 0 || !strings.Contains(out.String(), "boat ") || strings.Contains(errw.String(), "not logged in") {
			t.Errorf("%v: exit %d out %q err %q", args, code, out, errw)
		}
	}
	a, out, _ := acctApp(env, "")
	a.run([]string{"ssh-keys", "--help"})
	if !strings.Contains(out.String(), "ssh-keys ls") || strings.Contains(out.String(), "boat whoami") {
		t.Errorf("verb help should be just the verb's lines: %q", out)
	}
	// a remote command's own --help is not ours
	a, _, errw := acctApp(env, "")
	if code := a.run([]string{"exec", "vm1", "--", "ls", "--help"}); code != 1 || !strings.Contains(errw.String(), "not logged in") {
		t.Errorf("exec -- --help: %d %q", code, errw)
	}
}

func TestNoLoginVerbs(t *testing.T) {
	env := map[string]string{"BOAT_CONFIG": filepath.Join(t.TempDir(), "none.json")}
	for _, c := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"feedback"}, 0, "github.com/aicodeking/pandaos/issues"},
		{[]string{"wallet"}, 1, "not supported on self-hosted"},
		{[]string{"data-retention", "show"}, 1, "not supported on self-hosted"},
	} {
		a, out, errw := acctApp(env, "")
		if code := a.run(c.args); code != c.code || !strings.Contains(out.String()+errw.String(), c.out) {
			t.Errorf("%v: exit %d out %q err %q", c.args, code, out, errw)
		}
	}
	a, out, _ := acctApp(env, "")
	if code := a.run([]string{"--json", "nosuchverb"}); code != 2 {
		t.Errorf("unknown verb exit %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var ev map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &ev); err != nil || ev["event"] != "error" || !strings.Contains(ev["message"].(string), "nosuchverb") {
		t.Errorf("--json unknown verb must end with a JSON error line: %q", out)
	}
}

func TestSSHKeysVerb(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.EscapedPath())
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.EscapedPath() {
		case "GET /v1/vms/vm1":
			io.WriteString(w, `{"id":"vm1","name":"dev"}`)
		case "GET /v1/vms/vm1/ssh":
			io.WriteString(w, `{"keys":[{"fingerprint":"SHA256:ab+/c","public_key":"ssh-ed25519 AAAA","key_id":null,"user_id":null,"unowned":true}]}`)
		case "DELETE /v1/vms/vm1/ssh/ab-_c":
			io.WriteString(w, `{"ok":true,"fingerprint":"SHA256:ab+/c"}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"code":"not_found","message":"no"}}`)
		}
	}))
	defer srv.Close()
	env := map[string]string{"BOAT_URL": srv.URL, "BOAT_API_KEY": "boat_k", "BOAT_CONFIG": filepath.Join(t.TempDir(), "c.json")}
	a, out, errw := acctApp(env, "")
	if code := a.run([]string{"ssh-keys", "ls", "vm1"}); code != 0 || !strings.Contains(out.String(), "unowned") {
		t.Errorf("ls: %d %q %q", code, out, errw)
	}
	a, out, errw = acctApp(env, "")
	if code := a.run([]string{"ssh-keys", "rm", "vm1", "SHA256:ab+/c"}); code != 0 || !strings.Contains(out.String(), "revoked") {
		t.Errorf("rm: %d %q %q %v", code, out, errw, got)
	}
}
