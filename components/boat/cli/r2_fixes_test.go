package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestSnapshotDeleteRequiresConfirmation(t *testing.T) {
	type tc struct {
		name       string
		args       []string
		isTerm     bool
		input      string
		wantOK     bool
		wantPrompt string
		wantHdrs   map[string]string
	}
	cases := []tc{
		{
			name:   "single sid non-tty refuses without yes",
			args:   []string{"snapshot", "delete", "snap_1"},
			isTerm: false,
			wantOK: false,
		},
		{
			name:   "multi sid non-tty refuses without yes",
			args:   []string{"snapshot", "delete", "snap_1", "snap_2"},
			isTerm: false,
			wantOK: false,
		},
		{
			name:   "all vm non-tty refuses without yes",
			args:   []string{"snapshot", "delete", "--all", "vm1"},
			isTerm: false,
			wantOK: false,
		},
		{
			name:   "single sid json on tty refuses without yes",
			args:   []string{"snapshot", "delete", "snap_1", "--json"},
			isTerm: true,
			input:  "yes\n",
			wantOK: false,
		},
		{
			name:   "all vm json on tty refuses without yes",
			args:   []string{"snapshot", "delete", "--all", "vm1", "--json"},
			isTerm: true,
			input:  "yes\n",
			wantOK: false,
		},
		{
			name:       "single sid tty declined",
			args:       []string{"snapshot", "delete", "snap_1"},
			isTerm:     true,
			input:      "no\n",
			wantOK:     false,
			wantPrompt: "Permanently delete snapshot snap_1? [y/N]",
		},
		{
			name:       "single sid tty confirmed",
			args:       []string{"snapshot", "delete", "snap_1"},
			isTerm:     true,
			input:      "y\n",
			wantOK:     true,
			wantPrompt: "Permanently delete snapshot snap_1? [y/N]",
			wantHdrs:   map[string]string{"DELETE /api/v1/snapshots/snap_1": "snap_1"},
		},
		{
			name:       "all vm tty confirmed resolves name to id",
			args:       []string{"snapshot", "delete", "--all", "dev"},
			isTerm:     true,
			input:      "yes\n",
			wantOK:     true,
			wantPrompt: "Permanently delete all snapshots of vm1? [y/N]",
			wantHdrs:   map[string]string{"DELETE /api/v1/sandboxes/vm1/snapshots": "vm1"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			gotDeletes := map[string]string{}
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"vm1","name":"dev","state":"running"}`)
			})
			mux.HandleFunc("GET /v1/vms", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"vms":[{"id":"vm1","name":"dev","state":"running"}]}`)
			})
			mux.HandleFunc("GET /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(404)
				io.WriteString(w, `{"error":{"code":"not_found","message":"no"}}`)
			})
			mux.HandleFunc("DELETE /api/v1/snapshots/{sid}", func(w http.ResponseWriter, r *http.Request) {
				sid := r.PathValue("sid")
				hdr := r.Header.Get("X-Ascii-Confirm-Delete")
				mu.Lock()
				gotDeletes["DELETE "+r.URL.Path] = hdr
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if hdr != sid {
					w.WriteHeader(409)
					io.WriteString(w, `{"error":{"code":"confirmation_required","message":"set X-Ascii-Confirm-Delete to the snapshot id"}}`)
					return
				}
				w.WriteHeader(202)
				io.WriteString(w, `{"ok":true,"type":"snapshot.deleting"}`)
			})
			mux.HandleFunc("DELETE /api/v1/sandboxes/{id}/snapshots", func(w http.ResponseWriter, r *http.Request) {
				id := r.PathValue("id")
				hdr := r.Header.Get("X-Ascii-Confirm-Delete")
				mu.Lock()
				gotDeletes["DELETE "+r.URL.Path] = hdr
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if hdr != id {
					w.WriteHeader(409)
					io.WriteString(w, `{"error":{"code":"confirmation_required","message":"set X-Ascii-Confirm-Delete to the sandbox id"}}`)
					return
				}
				w.WriteHeader(202)
				io.WriteString(w, `{"ok":true,"type":"snapshot.deleting","operations":[]}`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			a, out, errw := upgradeDeleteApp(t, srv.URL)
			a.in = strings.NewReader(c.input)
			if c.isTerm {
				a.isTerminal = func() bool { return true }
			}
			code := a.run(c.args)
			if c.wantOK {
				if code != 0 {
					t.Fatalf("expected exit 0, got %d; out=%q err=%q", code, out, errw)
				}
				if len(gotDeletes) != len(c.wantHdrs) {
					t.Fatalf("deletes=%v want %v", gotDeletes, c.wantHdrs)
				}
				for k, v := range c.wantHdrs {
					if gotDeletes[k] != v {
						t.Errorf("%s header=%q want %q", k, gotDeletes[k], v)
					}
				}
			} else {
				if code == 0 {
					t.Fatalf("expected refusal, got exit 0; out=%q err=%q", out, errw)
				}
				if len(gotDeletes) != 0 {
					t.Fatalf("refused command sent DELETEs: %v", gotDeletes)
				}
			}
			if c.wantPrompt != "" && !strings.Contains(errw.String(), c.wantPrompt) {
				t.Errorf("prompt missing %q in stderr=%q", c.wantPrompt, errw.String())
			}
		})
	}
}

func TestSnapshotDeleteConfirmHeaderWithYes(t *testing.T) {
	var mu sync.Mutex
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"vm1","name":"dev","state":"running"}`)
	})
	mux.HandleFunc("GET /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"vms":[{"id":"vm1","name":"dev","state":"running"}]}`)
	})
	mux.HandleFunc("GET /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		io.WriteString(w, `{"error":{"code":"not_found","message":"no"}}`)
	})
	mux.HandleFunc("DELETE /api/v1/snapshots/{sid}", func(w http.ResponseWriter, r *http.Request) {
		sid := r.PathValue("sid")
		hdr := r.Header.Get("X-Ascii-Confirm-Delete")
		mu.Lock()
		got = append(got, r.URL.Path+"="+hdr)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if hdr != sid {
			w.WriteHeader(409)
			io.WriteString(w, `{"error":{"code":"confirmation_required","message":"bad header"}}`)
			return
		}
		w.WriteHeader(202)
		io.WriteString(w, `{"ok":true}`)
	})
	mux.HandleFunc("DELETE /api/v1/sandboxes/{id}/snapshots", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		hdr := r.Header.Get("X-Ascii-Confirm-Delete")
		mu.Lock()
		got = append(got, r.URL.Path+"="+hdr)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if hdr != id {
			w.WriteHeader(409)
			io.WriteString(w, `{"error":{"code":"confirmation_required","message":"bad header"}}`)
			return
		}
		w.WriteHeader(202)
		io.WriteString(w, `{"ok":true,"operations":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Single sid
	a, out, errw := upgradeDeleteApp(t, srv.URL)
	if code := a.run([]string{"snapshot", "delete", "snap_a", "--yes"}); code != 0 {
		t.Fatalf("single: exit %d err=%s", code, errw)
	}
	// Multi sid with -y --json
	a, out, errw = upgradeDeleteApp(t, srv.URL)
	if code := a.run([]string{"snapshot", "delete", "snap_b", "snap_c", "-y", "--json"}); code != 0 {
		t.Fatalf("multi: exit %d err=%s", code, errw)
	}
	var res obj
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res["ok"] != true {
		t.Fatalf("multi json=%q err=%v", out.String(), err)
	}
	// --all <vm> by name
	a, out, errw = upgradeDeleteApp(t, srv.URL)
	if code := a.run([]string{"snapshot", "delete", "--all", "dev", "--yes"}); code != 0 {
		t.Fatalf("--all: exit %d err=%s", code, errw)
	}

	want := []string{
		"/api/v1/snapshots/snap_a=snap_a",
		"/api/v1/snapshots/snap_b=snap_b",
		"/api/v1/snapshots/snap_c=snap_c",
		"/api/v1/sandboxes/vm1/snapshots=vm1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("headers sent = %v, want %v", got, want)
	}
}

func TestSSHConfigHostInjectionAndIDValidation(t *testing.T) {
	runSSHConfig := func(t *testing.T, vmID, vmName string) (int, string, string, string) {
		t.Helper()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(obj{"id": vmID, "name": vmName, "state": "running"})
		})
		mux.HandleFunc("POST /v1/vms/{id}/ssh", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"user":"dev","host":"10.0.0.5","port":22}`)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		home := t.TempDir()
		os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
		os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519.pub"), []byte("ssh-ed25519 AAAA k\n"), 0o600)
		var out, errw bytes.Buffer
		env := map[string]string{
			"HOME":         home,
			"BOAT_CONFIG":  filepath.Join(home, "config.json"),
			"BOAT_URL":     srv.URL,
			"BOAT_API_KEY": "boat_k",
		}
		a := &app{out: &out, errw: &errw, in: strings.NewReader(""), getenv: func(k string) string { return env[k] }}
		code := a.run([]string{"ssh-config", "target", "--print"})
		return code, out.String(), errw.String(), home
	}

	unsafeNames := []string{
		"*",
		"github.com *",
		"api worker",
		"API-Worker",
		"web?1",
		"!prod",
		"foo%h",
		"bar#baz",
		"-leading",
		"trailing-",
		"bad\nProxyCommand sh -c evil",
	}
	for _, name := range unsafeNames {
		t.Run("reject_name_"+name, func(t *testing.T) {
			code, out, errw, _ := runSSHConfig(t, "vm_1", name)
			if code != 0 {
				t.Fatalf("name=%q: exit %d err=%s", name, code, errw)
			}
			firstLine := strings.SplitN(out, "\n", 2)[0]
			if firstLine != "Host vm_1" {
				t.Fatalf("name=%q leaked into Host line: %q", name, firstLine)
			}
		})
	}

	t.Run("accept_valid_slug", func(t *testing.T) {
		for _, slug := range []string{"api-worker", "a", "web1"} {
			code, out, errw, _ := runSSHConfig(t, "vm_1", slug)
			if code != 0 {
				t.Fatalf("slug=%q: exit %d err=%s", slug, code, errw)
			}
			firstLine := strings.SplitN(out, "\n", 2)[0]
			want := "Host vm_1 " + slug
			if firstLine != want {
				t.Fatalf("slug=%q: Host line = %q, want %q", slug, firstLine, want)
			}
		}
	})

	t.Run("reject_invalid_vm_id", func(t *testing.T) {
		for _, badID := range []string{"*", "../evil", "vm 1", "-flag", "vm\nHost *", ""} {
			code, out, errw, home := runSSHConfig(t, badID, "api-worker")
			if code == 0 {
				t.Fatalf("badID=%q should fail; out=%q", badID, out)
			}
			if !strings.Contains(errw, "invalid VM id") {
				t.Errorf("badID=%q stderr=%q", badID, errw)
			}
			if entries, _ := os.ReadDir(filepath.Join(home, ".ssh", "boat")); len(entries) != 0 {
				t.Errorf("badID=%q wrote ssh config files: %v", badID, entries)
			}
		}
	})
}

func TestOpenURLValidationAndDesktopRejectsUnsafe(t *testing.T) {
	badURLs := []string{
		"file:///System/Applications/Calculator.app",
		"smb://attacker/share",
		`\\attacker\share\x.exe`,
		"-flag",
		"--args",
		"javascript:alert(1)",
		"http:///no-host",
		"",
	}
	for _, raw := range badURLs {
		if err := openURL(raw); err == nil {
			t.Errorf("openURL(%q) succeeded, want error", raw)
		}
		if err := validateOpenURL(raw); err == nil {
			t.Errorf("validateOpenURL(%q) succeeded, want error", raw)
		}
	}
	for _, good := range []string{
		"http://127.0.0.1:8080/desktop",
		"https://boat.example/desktop/vm1?exp=1&sig=ab",
	} {
		if err := validateOpenURL(good); err != nil {
			t.Errorf("validateOpenURL(%q) failed: %v", good, err)
		}
	}

	// Verify cmdDesktop rejects unsafe URLs from server and never invokes a.open.
	for _, raw := range badURLs {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"vm1","name":"dev","state":"running"}`)
		})
		mux.HandleFunc("GET /v1/vms/vm1/desktop", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(obj{"url": raw})
		})
		srv := httptest.NewServer(mux)
		a, out, errw := upgradeDeleteApp(t, srv.URL)
		opened := ""
		a.open = func(u string) error { opened = u; return nil }
		code := a.run([]string{"desktop", "vm1"})
		srv.Close()
		if code == 0 || opened != "" {
			t.Errorf("desktop %q: code=%d opened=%q out=%q err=%q", raw, code, opened, out, errw)
		}
	}
}

func TestCompleteVMsFiltersUnsafeNames(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(obj{
			"vms": []obj{
				{"id": "vm1", "name": "api-worker"},
				{"id": "vm2", "name": "api worker"},
				{"id": "vm3", "name": "*"},
				{"id": "vm4", "name": "github.com *"},
				{"id": "vm5", "name": "$(whoami)"},
				{"id": "vm6", "name": "-rf"},
				{"id": "vm7", "name": "vm7"},
				{"id": "vm8", "name": "web_1.prod"},
				{"id": "bad id", "name": "ok-name"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a, out, errw := upgradeDeleteApp(t, srv.URL)
	if code := a.run([]string{"__vms"}); code != 0 {
		t.Fatalf("__vms exit %d: %s", code, errw)
	}
	got := strings.Fields(out.String())
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	want := []string{"vm1", "api-worker", "vm2", "vm3", "vm4", "vm5", "vm6", "vm7", "vm8", "web_1.prod", "ok-name"}
	if !slices.Equal(lines, want) || !slices.Equal(got, want) {
		t.Fatalf("__vms lines=%q fields=%q want=%q", lines, got, want)
	}
}

func TestResumeRetypeStartFailureRollsBackSize(t *testing.T) {
	for _, initialState := range []string{"running", "stopped"} {
		t.Run(initialState, func(t *testing.T) {
			var mu sync.Mutex
			state := initialState
			size := "small"
			var ops []string
			var startCount int

			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				st, sz := state, size
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(obj{"id": "vm1", "name": "dev", "size": sz, "state": st, "ttl_s": 600, "no_env_run": false})
			})
			mux.HandleFunc("GET /v1/images", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"images":[],"sizes":{"small":{},"xlarge":{}}}`)
			})
			mux.HandleFunc("POST /v1/vms/vm1/stop", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				state = "stopped"
				ops = append(ops, "stop")
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"vm1","state":"stopped"}`)
			})
			mux.HandleFunc("POST /v1/vms/vm1/resize", func(w http.ResponseWriter, r *http.Request) {
				var body obj
				json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				size = s(body, "size")
				ops = append(ops, "resize:"+size)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(obj{"id": "vm1", "size": size, "state": "stopped"})
			})
			mux.HandleFunc("POST /v1/vms/vm1/start", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				startCount++
				curSize := size
				ops = append(ops, "start:"+curSize)
				if curSize == "xlarge" {
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(503)
					io.WriteString(w, `{"error":{"code":"no_ready_machine","message":"host full for xlarge"}}`)
					return
				}
				state = "running"
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(obj{"id": "vm1", "size": curSize, "state": "running"})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			a, errw := r35App(t, srv.URL)
			verb := "resume"
			if initialState == "stopped" {
				verb = "start"
			}
			if code := a.run([]string{verb, "vm1", "--type", "xlarge"}); code == 0 {
				t.Fatalf("expected failure when xlarge start fails")
			}
			if !strings.Contains(errw.String(), "host full for xlarge") {
				t.Errorf("missing original error: %s", errw.String())
			}

			mu.Lock()
			defer mu.Unlock()
			if size != "small" {
				t.Fatalf("size not rolled back: got %q want small (ops=%v)", size, ops)
			}
			if initialState == "running" {
				wantOps := []string{"stop", "resize:xlarge", "start:xlarge", "resize:small", "start:small"}
				if !slices.Equal(ops, wantOps) {
					t.Fatalf("running ops=%v, want %v", ops, wantOps)
				}
				if state != "running" || startCount != 2 {
					t.Fatalf("running VM not restored: state=%q startCount=%d", state, startCount)
				}
			} else {
				wantOps := []string{"resize:xlarge", "start:xlarge", "resize:small"}
				if !slices.Equal(ops, wantOps) {
					t.Fatalf("stopped ops=%v, want %v", ops, wantOps)
				}
				if state != "stopped" || startCount != 1 {
					t.Fatalf("stopped VM should stay stopped on small: state=%q startCount=%d", state, startCount)
				}
			}
		})
	}
}

func TestResumeRetypeRollbackResizeFailureReported(t *testing.T) {
	for _, initialState := range []string{"running", "stopped"} {
		t.Run(initialState, func(t *testing.T) {
			resizes := 0
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(obj{"id": "vm1", "name": "dev", "size": "small", "state": initialState})
			})
			mux.HandleFunc("POST /v1/vms/vm1/stop", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"vm1","state":"stopped"}`)
			})
			mux.HandleFunc("POST /v1/vms/vm1/resize", func(w http.ResponseWriter, r *http.Request) {
				resizes++
				w.Header().Set("Content-Type", "application/json")
				if resizes == 2 {
					w.WriteHeader(500)
					io.WriteString(w, `{"error":{"code":"internal","message":"rollback-resize-failed"}}`)
					return
				}
				io.WriteString(w, `{"id":"vm1","size":"xlarge","state":"stopped"}`)
			})
			mux.HandleFunc("POST /v1/vms/vm1/start", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(503)
				io.WriteString(w, `{"error":{"code":"no_ready_machine","message":"start-failed"}}`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			a, errw := r35App(t, srv.URL)
			if code := a.run([]string{"resume", "vm1", "--type", "xlarge"}); code == 0 {
				t.Fatal("expected failure")
			}
			e := errw.String()
			if !strings.Contains(e, "start-failed") || !strings.Contains(e, "rollback-resize-failed") {
				t.Fatalf("expected both start and rollback errors, got: %s", e)
			}
		})
	}
}

func TestSnapshotLatestNullAndPresent(t *testing.T) {
	var snapResp string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"vm1","name":"dev","state":"running"}`)
	})
	mux.HandleFunc("GET /api/v1/sandboxes/vm1/snapshots/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, snapResp)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Null snapshot in text mode
	snapResp = `{"ok":true,"type":"snapshot.latest","snapshot":null}`
	a, out, errw := upgradeDeleteApp(t, srv.URL)
	if code := a.run([]string{"snapshot", "latest", "vm1"}); code != 0 {
		t.Fatalf("latest null text: exit %d err=%s", code, errw)
	}
	if got := out.String(); got != "no snapshots\n" {
		t.Fatalf("latest null text = %q, want %q", got, "no snapshots\n")
	}

	// Null snapshot in --json mode
	a, out, errw = upgradeDeleteApp(t, srv.URL)
	if code := a.run([]string{"snapshot", "latest", "vm1", "--json"}); code != 0 {
		t.Fatalf("latest null json: exit %d err=%s", code, errw)
	}
	if got := strings.TrimSpace(out.String()); got != "null" {
		t.Fatalf("latest null json = %q, want null", got)
	}

	// Present snapshot in text and --json mode
	snapResp = `{"ok":true,"type":"snapshot.latest","snapshot":{"id":"sn_1","status":"ready","createdAt":"2026-01-01T00:00:00Z","sizeBytes":1024}}`
	a, out, errw = upgradeDeleteApp(t, srv.URL)
	if code := a.run([]string{"snapshot", "latest", "vm1"}); code != 0 {
		t.Fatalf("latest present text: exit %d err=%s", code, errw)
	}
	if !strings.Contains(out.String(), "sn_1") || !strings.Contains(out.String(), "ID") {
		t.Fatalf("latest present text = %q", out.String())
	}
	a, out, errw = upgradeDeleteApp(t, srv.URL)
	if code := a.run([]string{"snapshot", "latest", "vm1", "--json"}); code != 0 {
		t.Fatalf("latest present json: exit %d err=%s", code, errw)
	}
	var got obj
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got["id"] != "sn_1" {
		t.Fatalf("latest present json = %q err=%v", out.String(), err)
	}
}
