package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func r40Server(t *testing.T, createBody string, extra func(mux *http.ServeMux)) *httptest.Server {
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, code int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
	mux.HandleFunc("POST /v1/vms", func(w http.ResponseWriter, r *http.Request) { js(w, 502, createBody) })
	if extra != nil {
		extra(mux)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestR40CreateEnvStartFailedNamesStoppedVMs(t *testing.T) {
	single := `{"error":{"code":"env_start_failed","message":"push failed"},"vm":{"id":"vm9","state":"stopped"}}`
	batch := `{"error":{"code":"env_start_failed","message":"push failed"},"vms":[{"id":"vm7","state":"stopped"},{"id":"vm8","state":"running"}]}`
	cases := []struct {
		name string
		args []string
		body string
		want []string
	}{
		{"new", []string{"new"}, single, []string{"vm9 stopped (fresh boot env push failed"}},
		{"run", []string{"run", "--", "true"}, single, []string{"vm9 stopped (fresh boot env push failed"}},
		{"batch", []string{"new", "--count", "2"}, batch, []string{"vm7 stopped", "vm8 running"}},
	}
	for _, c := range cases {
		srv := r40Server(t, c.body, nil)
		a, errw := r35App(t, srv.URL)
		if code := a.run(c.args); code != 1 {
			t.Fatalf("%s exit %d err %q", c.name, code, errw)
		}
		for _, w := range c.want {
			if !strings.Contains(errw.String(), w) {
				t.Errorf("%s human missing %q: %q", c.name, w, errw)
			}
		}
		if strings.Contains(errw.String(), "all or none") {
			t.Errorf("%s still says all or none: %q", c.name, errw)
		}
		a, _ = r35App(t, srv.URL)
		var out bytes.Buffer
		a.out = &out
		if code := a.run(append([]string{"--json"}, c.args...)); code != 1 {
			t.Fatalf("%s json exit %d", c.name, code)
		}
		o := out.String()
		if !strings.Contains(o, `"code":"env_start_failed"`) || !strings.Contains(o, `"state":"stopped"`) || !strings.Contains(o, `"vms":[`) {
			t.Errorf("%s json: %q", c.name, o)
		}
	}
}

func TestR40ForkTypeFailureReportsChild(t *testing.T) {
	for _, withVM := range []bool{false, true} {
		deleted := 0
		start := `{"error":{"code":"env_start_failed","message":"wipe failed"}}`
		if withVM {
			start = `{"error":{"code":"env_start_failed","message":"wipe failed"},"vm":{"id":"vm9","state":"stopped"}}`
		}
		srv := r40Server(t, `{}`, func(mux *http.ServeMux) {
			js := func(w http.ResponseWriter, code int, body string) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				io.WriteString(w, body)
			}
			mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
				js(w, 200, `{"id":"vm1","name":"dev","size":"default","state":"running"}`)
			})
			mux.HandleFunc("POST /v1/vms/vm1/fork", func(w http.ResponseWriter, r *http.Request) {
				js(w, 200, `{"id":"vm9","size":"default","state":"stopped"}`)
			})
			mux.HandleFunc("POST /v1/vms/vm9/resize", func(w http.ResponseWriter, r *http.Request) { js(w, 200, `{"id":"vm9"}`) })
			mux.HandleFunc("POST /v1/vms/vm9/start", func(w http.ResponseWriter, r *http.Request) { js(w, 502, start) })
			mux.HandleFunc("DELETE /v1/vms/vm9", func(w http.ResponseWriter, r *http.Request) { deleted++; js(w, 200, `{}`) })
		})
		for _, verb := range []string{"fork", "clone"} {
			deleted = 0
			a, errw := r35App(t, srv.URL)
			if code := a.run([]string{verb, "vm1", "--type", "large"}); code != 1 {
				t.Fatalf("%s exit %d", verb, code)
			}
			e := errw.String()
			if !strings.Contains(e, "child vm9 was deleted") || strings.Contains(e, "was stopped") || deleted != 1 {
				t.Errorf("%s withVM=%v: deleted=%d err %q", verb, withVM, deleted, e)
			}
		}
	}
}

func TestR41CreateStartFailureDedupesVM(t *testing.T) {
	body := `{"error":{"code":"env_start_failed","message":"push failed"},"vm":{"id":"A","state":"stopped"},"vms":[{"id":"A","state":"stopped"},{"id":"B","state":"running"}]}`
	srv := r40Server(t, body, nil)
	a, errw := r35App(t, srv.URL)
	if code := a.run([]string{"new", "--count", "2"}); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if n := strings.Count(errw.String(), "A stopped"); n != 1 {
		t.Errorf("A printed %d times: %q", n, errw)
	}
	a, _ = r35App(t, srv.URL)
	var out bytes.Buffer
	a.out = &out
	if code := a.run([]string{"--json", "new", "--count", "2"}); code != 1 {
		t.Fatalf("json exit %d", code)
	}
	if n := strings.Count(out.String(), `"id":"A"`); n != 2 { // top-level id + one vms entry
		t.Errorf("json A count %d: %q", n, out.String())
	}
}
