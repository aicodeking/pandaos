package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func r45Server(envs string, mutating *int32) *httptest.Server {
	var calls int32
	mux, _, _ := r44Mux(`{"id":"vm1","name":"dev","size":"default","state":"running"}`, &calls)
	mux.HandleFunc("GET /v1/environments", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(envs))
	})
	count := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				atomic.AddInt32(mutating, 1)
			}
			h.ServeHTTP(w, r)
		})
	}
	return httptest.NewServer(count(mux))
}

const r45Envs = `{"environments":[{"id":"env_1","name":"prod"}]}`

func TestR45UnknownEnvironmentMakesNoStateChange(t *testing.T) {
	for _, verb := range []string{"resume", "fork"} {
		var mut int32
		srv := r45Server(r45Envs, &mut)
		a, errw := r35App(t, srv.URL)
		code := a.run([]string{verb, "vm1", "--type", "large", "--environment", "nope"})
		srv.Close()
		if code == 0 || !strings.Contains(errw.String(), "unknown environment") {
			t.Fatalf("%s: code %d err %q", verb, code, errw)
		}
		if n := atomic.LoadInt32(&mut); n != 0 {
			t.Fatalf("%s made %d state-changing calls", verb, n)
		}
	}
}

func TestR45KnownEnvironmentByIDNameAndBase(t *testing.T) {
	for _, env := range []string{"env_1", "prod", "base"} {
		for _, verb := range []string{"resume", "fork"} {
			var mut int32
			srv := r45Server(r45Envs, &mut)
			a, errw := r35App(t, srv.URL)
			code := a.run([]string{verb, "vm1", "--type", "large", "--environment", env})
			srv.Close()
			if code != 0 {
				t.Fatalf("%s %s: code %d err %q", verb, env, code, errw)
			}
			if atomic.LoadInt32(&mut) == 0 {
				t.Fatalf("%s %s: expected state changes", verb, env)
			}
		}
	}
}

func TestR45SSHFieldValidation(t *testing.T) {
	good := obj{"user": "root", "host": "10.0.0.1", "port": float64(2222), "jump": "boat@jump.example.com:2200"}
	if err := validateSSHInfo(good); err != nil {
		t.Fatalf("good rejected: %v", err)
	}
	if err := validateSSHInfo(obj{"user": "a.b-c_d", "host": "fe80::1", "port": "22"}); err != nil {
		t.Fatalf("ipv6 rejected: %v", err)
	}
	if err := validateSSHInfo(obj{"user": "u", "host": "h", "port": "22", "jump": "u@[::1]:22"}); err != nil {
		t.Fatalf("bracket jump rejected: %v", err)
	}
	bad := map[string]string{
		"user": "-oProxyCommand=x",
		"host": "-oProxyCommand=x",
		"port": "0",
		"jump": "u@h -oProxyCommand=x",
	}
	for field, val := range bad {
		info := obj{"user": "u", "host": "h", "port": "22"}
		info[field] = val
		if err := validateSSHInfo(info); err == nil {
			t.Fatalf("%s=%q accepted", field, val)
		}
	}
	for _, v := range []string{"a b", "a;b", "a$(x)", "a`x`", "a|b", "a&b", "a\nb", "a'b", `a"b`, "", "-x", "a@b"} {
		if validSSHUser(v) {
			t.Errorf("user %q accepted", v)
		}
	}
	for _, v := range []string{"h;x", "h x", "-h", "h$x", "h|x", "", "h/", "h:22"} {
		if validSSHHost(v) {
			t.Errorf("host %q accepted", v)
		}
	}
	for _, v := range []string{"0", "65536", "-1", "1e3", "22 ", "022", "", "x"} {
		if validSSHPort(v) {
			t.Errorf("port %q accepted", v)
		}
	}
	for _, v := range []string{"", "host", "-u@h", "u@-h", "u@h:0", "u@h:99999", "u@h;x", "u@h:22:3x", "u@h -p1", "u@h%p", "u@[::1", "u@h:22$(x)"} {
		if validSSHJump(v) {
			t.Errorf("jump %q accepted", v)
		}
	}
}

func TestR45SSHVerbRejectsHostileTarget(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"vm1","name":"dev","state":"running"}`))
	})
	mux.HandleFunc("POST /v1/vms/vm1/ssh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"user":"root","host":"-oProxyCommand=touch /tmp/x","port":22}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	a, errw := r35App(t, srv.URL)
	home := a.getenv("HOME")
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519.pub"), []byte("ssh-ed25519 AAAA k\n"), 0o600)
	if code := a.run([]string{"ssh", "vm1", "--json"}); code == 0 || !strings.Contains(errw.String(), "invalid ssh host") {
		t.Fatalf("code %d err %q", code, errw)
	}
}
