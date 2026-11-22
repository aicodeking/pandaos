package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func r44Mux(vmJSON string, calls *int32) (*http.ServeMux, *[]string, *[]string) {
	var starts, forks []string
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, body string) {
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) { js(w, vmJSON) })
	mux.HandleFunc("POST /v1/vms/vm1/stop", func(w http.ResponseWriter, r *http.Request) { js(w, `{"id":"vm1","state":"stopped"}`) })
	mux.HandleFunc("POST /v1/vms/vm1/resize", func(w http.ResponseWriter, r *http.Request) { js(w, `{"id":"vm1","size":"large"}`) })
	mux.HandleFunc("POST /v1/vms/vm1/start", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		starts = append(starts, strings.TrimSpace(string(b)))
		js(w, `{"id":"vm1","size":"large","state":"running"}`)
	})
	mux.HandleFunc("POST /v1/vms/vm1/fork", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		forks = append(forks, strings.TrimSpace(string(b)))
		js(w, `{"id":"vmc","name":"c","size":"default","state":"stopped"}`)
	})
	mux.HandleFunc("POST /v1/vms/vmc/resize", func(w http.ResponseWriter, r *http.Request) { js(w, `{"id":"vmc","size":"large"}`) })
	mux.HandleFunc("POST /v1/vms/vmc/start", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		starts = append(starts, strings.TrimSpace(string(b)))
		js(w, `{"id":"vmc","size":"large","state":"running"}`)
	})
	return mux, &starts, &forks
}

// r46: a no_env run is sticky; --type with --environment refuses before any
// stop, resize, fork or start (only the read-only GET happens).
func TestR46TypeEnvironmentOnNoEnvRunRefusedBeforeAnyChange(t *testing.T) {
	for _, verb := range []string{"resume", "fork"} {
		var calls int32
		mux, starts, forks := r44Mux(`{"id":"vm1","name":"dev","size":"default","state":"running","no_env_run":true}`, &calls)
		var writes int32
		wrapped := http.NewServeMux()
		wrapped.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				atomic.AddInt32(&writes, 1)
			}
			mux.ServeHTTP(w, r)
		})
		srv := httptest.NewServer(wrapped)
		a, errw := r35App(t, srv.URL)
		code := a.run([]string{verb, "vm1", "--type", "large", "--environment", "prod"})
		srv.Close()
		if code == 0 || !strings.Contains(errw.String(), "sticky") {
			t.Fatalf("%s: code %d err %q", verb, code, errw)
		}
		if atomic.LoadInt32(&writes) != 0 || len(*starts) != 0 || len(*forks) != 0 {
			t.Fatalf("%s made state-changing calls: writes=%d starts=%q forks=%q", verb, writes, *starts, *forks)
		}
	}
}

// r46: the API's no_env_sticky error maps to a clear message.
func TestR46StickyAPIErrorMapped(t *testing.T) {
	for _, verb := range []string{"resume", "fork"} {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"vm1","size":"default","state":"stopped"}`)
		})
		sticky := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"code":"no_env_sticky","message":"vm ran no_env"}}`)
		}
		mux.HandleFunc("POST /v1/vms/vm1/start", sticky)
		mux.HandleFunc("POST /v1/vms/vm1/fork", sticky)
		srv := httptest.NewServer(mux)
		a, errw := r35App(t, srv.URL)
		code := a.run([]string{verb, "vm1", "--environment", "prod"})
		srv.Close()
		if code == 0 || !strings.Contains(errw.String(), "no_env is sticky") {
			t.Fatalf("%s: code %d err %q", verb, code, errw)
		}
	}
}

// r46: a fork with neither --ttl nor --no-auto-stop sends no ttl_s, so the
// server applies its 1h default; no path sends no_env:false.
func TestR46ForkWithoutTTLSendsNoTTL(t *testing.T) {
	var calls int32
	mux, _, forks := r44Mux(`{"id":"vm1","name":"dev","size":"default","state":"running"}`, &calls)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	a, errw := r35App(t, srv.URL)
	if code := a.run([]string{"fork", "vm1"}); code != 0 {
		t.Fatalf("code %d: %s", code, errw)
	}
	if len(*forks) != 1 || strings.Contains((*forks)[0], "ttl_s") || strings.Contains((*forks)[0], "no_env") {
		t.Fatalf("fork bodies = %q", *forks)
	}
}

func TestR44NoEnvAndEnvironmentRefusedLocally(t *testing.T) {
	for _, verb := range []string{"resume", "fork"} {
		var calls int32
		mux, _, _ := r44Mux(`{"id":"vm1","size":"default","state":"running"}`, &calls)
		srv := httptest.NewServer(mux)
		a, errw := r35App(t, srv.URL)
		code := a.run([]string{verb, "vm1", "--type", "large", "--no-env", "--environment", "prod"})
		srv.Close()
		if code == 0 || !strings.Contains(errw.String(), "conflict") {
			t.Fatalf("%s: code %d err %q", verb, code, errw)
		}
		if n := atomic.LoadInt32(&calls); n != 0 {
			t.Fatalf("%s made %d API calls", verb, n)
		}
	}
}
