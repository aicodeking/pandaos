package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// jobAPI fakes what run/push/host/forward/ssh-config touch. Exec answers per
// test through execLines; an exec of the unpack script prints the directory.
type jobAPI struct {
	sync.Mutex
	srv         *httptest.Server
	reqs        []string
	bodies      map[string][]string
	upload      []byte
	execLines   func(w http.ResponseWriter, r *http.Request, cmd []string)
	created     int
	vmExtra     obj            // merged into the stop/start/fork answers
	createExtra obj            // merged into the single-VM create answer
	startClosed bool           // start answers 502 env_start_failed (no_env cold start failed closed)
	startLegacy bool           // start answers the legacy bare 502 env_update_failed
	listVMs     []obj          // GET /v1/vms answer (default: none)
	batchEnvErr map[int]string // batch member (1-based) -> env_error
	failFrom    int            // creates after this many get 409 (0 = never, -1 = all)
}

func newJobAPI(t *testing.T) *jobAPI {
	api := &jobAPI{bodies: map[string][]string{}}
	mux := http.NewServeMux()
	j := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /api/v1/ai-usage", func(w http.ResponseWriter, r *http.Request) {
		api.Lock()
		api.reqs = append(api.reqs, "QUERY "+r.URL.RawQuery)
		api.Unlock()
		j(w, 200, obj{"usage": []obj{}, "sandboxes": []obj{}})
	})
	mux.HandleFunc("POST /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		var in obj
		json.NewDecoder(r.Body).Decode(&in)
		api.Lock()
		api.created++
		n := api.created
		over := api.failFrom != 0 && n > api.failFrom
		api.Unlock()
		if over {
			j(w, 409, obj{"error": obj{"code": "quota", "message": "over quota"}})
			return
		}
		if c, _ := in["count"].(float64); c > 1 { // the server's batch
			vms := []obj{}
			for i := 1; i <= int(c); i++ {
				vm := obj{"id": fmt.Sprintf("vm-%d-%d", n, i), "name": "", "size": "large", "state": "running"}
				if e := api.batchEnvErr[i]; e != "" {
					vm["env_error"] = e
				}
				vms = append(vms, vm)
			}
			j(w, 201, obj{"vms": vms})
			return
		}
		vm := obj{"id": fmt.Sprintf("vm-%d", n), "name": "", "size": "large", "state": "running"}
		for k, v := range api.createExtra {
			vm[k] = v
		}
		j(w, 201, vm)
	})
	mux.HandleFunc("GET /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !strings.HasPrefix(id, "vm-") {
			j(w, 404, obj{"error": obj{"code": "not_found", "message": "vm not found"}})
			return
		}
		j(w, 200, obj{"id": id, "name": "dev", "size": "large", "state": "running"})
	})
	mux.HandleFunc("GET /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		api.Lock()
		defer api.Unlock()
		vms := api.listVMs
		if vms == nil {
			vms = []obj{}
		}
		j(w, 200, obj{"vms": vms})
	})
	mux.HandleFunc("DELETE /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) { j(w, 200, obj{"ok": true}) })
	mux.HandleFunc("PATCH /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) { j(w, 200, obj{"id": r.PathValue("id")}) })
	mux.HandleFunc("GET /v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		j(w, 200, obj{"snapshots": []obj{{"id": "snap-1", "name": "ci-base"}}})
	})
	mux.HandleFunc("POST /v1/snapshots/import-share", func(w http.ResponseWriter, r *http.Request) {
		j(w, 201, obj{"id": "snap-9"})
	})
	mux.HandleFunc("PUT /v1/vms/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		api.Lock()
		api.upload = b
		api.Unlock()
		j(w, 200, obj{"ok": true})
	})
	mux.HandleFunc("POST /v1/vms/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Cmd []string `json:"cmd"`
		}
		json.Unmarshal([]byte(api.lastBody("POST /v1/vms/"+r.PathValue("id")+"/exec")), &body)
		if len(body.Cmd) > 2 && body.Cmd[2] == unpackScript {
			enc := json.NewEncoder(w)
			enc.Encode(obj{"stream": "stdout", "data_b64": base64.StdEncoding.EncodeToString([]byte("/home/dev/" + body.Cmd[3] + "\n"))})
			enc.Encode(obj{"exit": 0})
			return
		}
		api.execLines(w, r, body.Cmd)
	})
	for _, act := range []string{"stop", "start", "fork"} {
		mux.HandleFunc("POST /v1/vms/{id}/"+act, func(w http.ResponseWriter, r *http.Request) {
			api.Lock()
			legacy := act == "start" && api.startLegacy
			api.Unlock()
			if act == "start" && api.startClosed {
				j(w, 502, obj{"error": obj{"code": "env_start_failed", "message": "env wipe failed"}, "vm": obj{"id": r.PathValue("id"), "state": "stopped"}})
				return
			}
			if legacy {
				j(w, 502, obj{"error": obj{"code": "env_update_failed", "message": "env push failed"}})
				return
			}
			vm := obj{"id": r.PathValue("id"), "name": "dev", "size": "large", "state": "running"}
			api.Lock()
			for k, v := range api.vmExtra {
				vm[k] = v
			}
			api.Unlock()
			j(w, 200, vm)
		})
	}
	mux.HandleFunc("GET /v1/usage", func(w http.ResponseWriter, r *http.Request) {
		api.Lock()
		api.reqs = append(api.reqs, "QUERY "+r.URL.RawQuery)
		api.Unlock()
		j(w, 200, obj{"team": "t"})
	})
	mux.HandleFunc("POST /v1/vms/{id}/ports", func(w http.ResponseWriter, r *http.Request) { j(w, 201, obj{"guest_port": 3000}) })
	mux.HandleFunc("GET /v1/vms/{id}/ports/{port}/url", func(w http.ResponseWriter, r *http.Request) {
		j(w, 200, obj{"url": "https://3000-vm.example/?boat_sig=ab", "expires_at": "2026-01-01T00:00:00Z"})
	})
	mux.HandleFunc("POST /v1/vms/{id}/ssh", func(w http.ResponseWriter, r *http.Request) {
		j(w, 200, obj{"host": "10.0.0.5", "port": 22, "user": "dev", "jump": "boat@gw.example:2222"})
	})
	api.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		k := r.Method + " " + r.URL.Path
		api.Lock()
		api.reqs = append(api.reqs, k)
		api.bodies[k] = append(api.bodies[k], string(b))
		api.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(api.srv.Close)
	return api
}

func (api *jobAPI) lastBody(k string) string {
	api.Lock()
	defer api.Unlock()
	l := api.bodies[k]
	if len(l) == 0 {
		return ""
	}
	return l[len(l)-1]
}

func (api *jobAPI) count(k string) int {
	api.Lock()
	defer api.Unlock()
	return len(api.bodies[k])
}

func exitWith(code int, extra obj) func(http.ResponseWriter, *http.Request, []string) {
	return func(w http.ResponseWriter, r *http.Request, cmd []string) {
		enc := json.NewEncoder(w)
		enc.Encode(obj{"stream": "stdout", "data_b64": base64.StdEncoding.EncodeToString([]byte("job output\n"))})
		last := obj{"exit": code}
		for k, v := range extra {
			last[k] = v
		}
		enc.Encode(last)
	}
}

type harness struct {
	a        *app
	out, err *bytes.Buffer
	home     string
	env      map[string]string
}

func newHarness(t *testing.T, api *jobAPI) *harness {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519.pub"), []byte("ssh-ed25519 AAAA k\n"), 0o600)
	h := &harness{out: &bytes.Buffer{}, err: &bytes.Buffer{}, home: home, env: map[string]string{
		"HOME": home, "BOAT_CONFIG": filepath.Join(home, "cfg.json"), "BOAT_URL": api.srv.URL, "BOAT_API_KEY": "k",
		"BOAT_DIR": filepath.Join(home, "not-a-guest")}}
	h.a = &app{out: h.out, errw: h.err, in: strings.NewReader(""), shellID: 4242,
		getenv: func(k string) string { return h.env[k] }}
	return h
}

func TestRunDeletesAndExitsWithCommandCode(t *testing.T) {
	api := newJobAPI(t)
	api.execLines = exitWith(7, nil)
	h := newHarness(t, api)
	if code := h.a.run([]string{"run", "--type", "large", "-e", "A=b", "--no-env", "--", "npm", "test"}); code != 7 {
		t.Fatalf("exit %d, stderr %s", code, h.err)
	}
	if h.out.String() != "job output\n" {
		t.Errorf("stdout %q", h.out)
	}
	if api.count("DELETE /v1/vms/vm-1") != 1 {
		t.Errorf("not deleted: %v", api.reqs)
	}
	if got := h.err.String(); !strings.HasPrefix(got, "exit 7 · ") || !strings.HasSuffix(got, " · large · vm-1 deleted\n") {
		t.Errorf("summary %q", got)
	}
	var create obj
	json.Unmarshal([]byte(api.lastBody("POST /v1/vms")), &create)
	if create["size"] != "large" || create["idle_stop_s"] != float64(runIdleStopS) || fmt.Sprint(create["env"]) != "map[A:b]" || fmt.Sprint(create["secrets"]) != "[]" {
		t.Errorf("create body %v", create)
	}
	var ex obj
	json.Unmarshal([]byte(api.lastBody("POST /v1/vms/vm-1/exec")), &ex)
	if fmt.Sprint(ex["cmd"]) != "[npm test]" || ex["timeout_s"] != float64(runNoLimitS) {
		t.Errorf("exec body %v", ex)
	}
}

func TestRunTimeoutExits124(t *testing.T) {
	api := newJobAPI(t)
	api.execLines = exitWith(143, obj{"timed_out": true})
	h := newHarness(t, api)
	if code := h.a.run([]string{"run", "--timeout", "5", "--", "sleep", "9"}); code != 124 {
		t.Fatalf("exit %d", code)
	}
}

func TestRunUnknownFlagCreatesNothing(t *testing.T) {
	api := newJobAPI(t)
	h := newHarness(t, api)
	if code := h.a.run([]string{"run", "--personal", "--", "ls"}); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if code := h.a.run([]string{"run", "--timeout", "-1", "--", "ls"}); code != 2 {
		t.Fatalf("negative --timeout exit %d, want 2", code)
	}
	if h.a.run([]string{"run", "--push", "/no/such/dir", "--", "ls"}) != 1 {
		t.Fatal("a missing --push dir should fail")
	}
	if api.count("POST /v1/vms") != 0 {
		t.Errorf("created a VM: %v", api.reqs)
	}
}

func TestRunEndToEndDeadlineAndSetupReconnect(t *testing.T) {
	pushDir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(pushDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pushDir, "hello.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("reconnect_resume_and_remaining_exec_timeout", func(t *testing.T) {
		var mu sync.Mutex
		var followQueries []string
		var execTimeoutS int
		var deleted bool
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/vms", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(obj{"id": "vm-1", "size": "small", "state": "running", "setup_cmd_id": "setup-1"})
		})
		mux.HandleFunc("GET /v1/vms/vm-1/commands/setup-1/logs", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			idx := len(followQueries)
			followQueries = append(followQueries, r.URL.RawQuery)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/x-ndjson")
			enc := json.NewEncoder(w)
			switch idx {
			case 0:
				time.Sleep(400 * time.Millisecond)
				enc.Encode(obj{"stream": "stdout", "data_b64": base64.StdEncoding.EncodeToString([]byte("step1\n")), "next_offset": 42})
				enc.Encode(obj{"reconnect": true, "next_offset": 42})
			case 1:
				time.Sleep(400 * time.Millisecond)
				enc.Encode(obj{"reconnect": true}) // preserve offset 42
			case 2:
				time.Sleep(400 * time.Millisecond)
				enc.Encode(obj{"stream": "stdout", "data_b64": base64.StdEncoding.EncodeToString([]byte("step2\n")), "next_offset": 99})
				enc.Encode(obj{"exit": 0})
			default:
				enc.Encode(obj{"exit": 0})
			}
		})
		mux.HandleFunc("PUT /v1/vms/vm-1/files", func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			json.NewEncoder(w).Encode(obj{"ok": true})
		})
		mux.HandleFunc("POST /v1/vms/vm-1/exec", func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Cmd      []string `json:"cmd"`
				TimeoutS int      `json:"timeout_s"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			enc := json.NewEncoder(w)
			if len(in.Cmd) > 2 && in.Cmd[2] == unpackScript {
				enc.Encode(obj{"stream": "stdout", "data_b64": base64.StdEncoding.EncodeToString([]byte("/home/dev/" + in.Cmd[3] + "\n"))})
				enc.Encode(obj{"exit": 0})
				return
			}
			mu.Lock()
			execTimeoutS = in.TimeoutS
			mu.Unlock()
			enc.Encode(obj{"exit": 0})
		})
		mux.HandleFunc("DELETE /v1/vms/vm-1", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			deleted = true
			mu.Unlock()
			json.NewEncoder(w).Encode(obj{"ok": true})
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		h := newHarness(t, &jobAPI{srv: srv})
		if code := h.a.run([]string{"run", "--timeout", "3", "--push", pushDir, "--", "make"}); code != 0 {
			t.Fatalf("exit %d, stderr %s", code, h.err)
		}
		mu.Lock()
		gotQueries := slices.Clone(followQueries)
		gotTimeout := execTimeoutS
		gotDeleted := deleted
		mu.Unlock()
		wantQueries := []string{"follow=1&offset=0", "follow=1&offset=42", "follow=1&offset=42"}
		if !slices.Equal(gotQueries, wantQueries) {
			t.Errorf("follow queries %v, want %v", gotQueries, wantQueries)
		}
		// ~1.3s elapsed out of 3s budget -> remaining whole seconds is 1 (not 3).
		if gotTimeout < 1 || gotTimeout >= 3 {
			t.Errorf("exec timeout_s = %d, want remaining whole seconds in [1, 2]", gotTimeout)
		}
		if !gotDeleted {
			t.Error("expected VM to be deleted")
		}
	})

	for _, phase := range []string{"setup", "push", "exec"} {
		t.Run("deadline_exceeded_during_"+phase, func(t *testing.T) {
			var mu sync.Mutex
			var deleted int
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/vms", func(w http.ResponseWriter, r *http.Request) {
				vm := obj{"id": "vm-1", "size": "small", "state": "running"}
				if phase == "setup" {
					vm["setup_cmd_id"] = "setup-1"
				}
				w.WriteHeader(201)
				json.NewEncoder(w).Encode(vm)
			})
			mux.HandleFunc("GET /v1/vms/vm-1/commands/setup-1/logs", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			mux.HandleFunc("PUT /v1/vms/vm-1/files", func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if phase == "push" {
					<-r.Context().Done()
					return
				}
				json.NewEncoder(w).Encode(obj{"ok": true})
			})
			mux.HandleFunc("POST /v1/vms/vm-1/exec", func(w http.ResponseWriter, r *http.Request) {
				var in struct {
					Cmd []string `json:"cmd"`
				}
				json.NewDecoder(r.Body).Decode(&in)
				enc := json.NewEncoder(w)
				if len(in.Cmd) > 2 && in.Cmd[2] == unpackScript {
					enc.Encode(obj{"stream": "stdout", "data_b64": base64.StdEncoding.EncodeToString([]byte("/home/dev/" + in.Cmd[3] + "\n"))})
					enc.Encode(obj{"exit": 0})
					return
				}
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			mux.HandleFunc("DELETE /v1/vms/vm-1", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				deleted++
				mu.Unlock()
				json.NewEncoder(w).Encode(obj{"ok": true})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			h := newHarness(t, &jobAPI{srv: srv})
			t0 := time.Now()
			code := h.a.run([]string{"run", "--timeout", "1", "--push", pushDir, "--", "sleep", "30"})
			elapsed := time.Since(t0)
			if code != 124 {
				t.Fatalf("phase %s: exit %d, want 124; stderr %s", phase, code, h.err)
			}
			if elapsed > 3*time.Second {
				t.Errorf("phase %s took %v, expected ~1s deadline", phase, elapsed)
			}
			mu.Lock()
			gotDeleted := deleted
			mu.Unlock()
			if gotDeleted != 1 {
				t.Errorf("phase %s: DELETE count %d, want 1", phase, gotDeleted)
			}
		})
	}
}

func TestRunKeepAndJSONSummary(t *testing.T) {
	api := newJobAPI(t)
	api.execLines = exitWith(0, nil)
	h := newHarness(t, api)
	if code := h.a.run([]string{"run", "--json", "--keep", "--", "true"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if api.count("DELETE /v1/vms/vm-1") != 0 {
		t.Error("--keep deleted the VM")
	}
	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	var sum obj
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &sum); err != nil {
		t.Fatal(err)
	}
	if sum["type"] != "summary" || sum["exitCode"] != float64(0) || sum["deleted"] != false || sum["sandboxType"] != "large" {
		t.Errorf("summary %v", sum)
	}
	if id, err := h.a.vmAlias("current"); err != nil || id != "vm-1" {
		t.Errorf("--keep should set current: %q %v", id, err)
	}
}

func TestRunInterruptDeletes(t *testing.T) {
	api := newJobAPI(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	api.execLines = func(w http.ResponseWriter, r *http.Request, cmd []string) {
		w.(http.Flusher).Flush()
		syscall.Kill(os.Getpid(), syscall.SIGINT)
		<-release
	}
	h := newHarness(t, api)
	if code := h.a.run([]string{"run", "--", "sleep", "99"}); code != 130 {
		t.Fatalf("exit %d", code)
	}
	if api.count("DELETE /v1/vms/vm-1") != 1 {
		t.Errorf("Ctrl-C did not delete: %v", api.reqs)
	}
}

func untar(t *testing.T, b []byte) map[string]*tar.Header {
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*tar.Header{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = h
	}
}

func git(t *testing.T, dir string, args ...string) {
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "protocol.file.allow=always"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestPushInGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	sub := t.TempDir()
	git(t, sub, "init", "-q")
	os.WriteFile(filepath.Join(sub, "inner.txt"), []byte("in"), 0o644)
	git(t, sub, "add", ".")
	git(t, sub, "commit", "-qm", "x")

	dir := filepath.Join(t.TempDir(), "proj")
	os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755)
	git(t, filepath.Dir(dir), "init", "-q", "proj")
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "gone.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "node_modules", "big.js"), []byte("x"), 0o644)
	os.Symlink("run.sh", filepath.Join(dir, "link"))
	git(t, dir, "add", ".")
	git(t, dir, "submodule", "add", "-q", sub, "vendored")
	git(t, dir, "commit", "-qm", "x")
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("n"), 0o644)
	os.Remove(filepath.Join(dir, "gone.txt"))

	api := newJobAPI(t)
	h := newHarness(t, api)
	if code := h.a.run([]string{"push", "vm-1", dir}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	got := untar(t, api.upload)
	for _, want := range []string{".gitignore", "run.sh", "link", "new.txt", "vendored/inner.txt"} {
		if got[want] == nil {
			t.Errorf("missing %s in %v", want, keys(got))
		}
	}
	for _, bad := range []string{"gone.txt", "node_modules/big.js", ".git/config", ".git"} {
		if got[bad] != nil {
			t.Errorf("should not send %s", bad)
		}
	}
	if h := got["run.sh"]; h != nil && h.Mode&0o111 == 0 {
		t.Errorf("exec bit lost: %o", h.Mode)
	}
	if l := got["link"]; l != nil && (l.Typeflag != tar.TypeSymlink || l.Linkname != "run.sh") {
		t.Errorf("link %v", l)
	}
	if h.out.String() != fmt.Sprintf("pushed %d files to vm-1:/home/dev/proj\n", len(got)) {
		t.Errorf("stdout %q", h.out)
	}
}

func keys(m map[string]*tar.Header) []string {
	var l []string
	for k := range m {
		l = append(l, k)
	}
	slices.Sort(l)
	return l
}

func TestPushOutsideGitSkipsDotGitOnly(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(dir, "a"), 0o755)
	os.WriteFile(filepath.Join(dir, "a", "b"), []byte("x"), 0o644)
	files, err := walkPushFiles(dir)
	if err != nil || fmt.Sprint(files) != "[a/b]" {
		t.Errorf("%v %v", files, err)
	}
}

// The script that runs on the VM, for real: a relative path is from home, the
// archive is removed, the directory is printed, and a bad archive fails.
func TestUnpackScript(t *testing.T) {
	src, home := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(src, "run.sh"), []byte("x"), 0o755)
	tmp := filepath.Join(t.TempDir(), "a.tgz")
	f, _ := os.Create(tmp)
	if err := packPush(f, src, []string{"run.sh"}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	run := func(to, archive string) (string, error) {
		cmd := exec.Command("bash", "-c", unpackScript, to, archive)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.Output()
		return string(out), err
	}
	out, err := run("a/proj", tmp)
	if want := filepath.Join(home, "a/proj") + "\n"; err != nil || out != want {
		t.Fatalf("%q %v, want %q", out, err, want)
	}
	if st, err := os.Stat(filepath.Join(home, "a/proj/run.sh")); err != nil || st.Mode()&0o100 == 0 {
		t.Errorf("unpacked: %v %v", st, err)
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Error("archive left behind")
	}
	bad := filepath.Join(t.TempDir(), "bad.tgz")
	os.WriteFile(bad, []byte("not a tar"), 0o644)
	if out, err := run("b", bad); err == nil || out != "" {
		t.Errorf("bad archive: %q %v", out, err)
	}
}

func TestRunPushRunsInPushedDir(t *testing.T) {
	api := newJobAPI(t)
	api.execLines = exitWith(0, nil)
	h := newHarness(t, api)
	dir := filepath.Join(t.TempDir(), "api")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644)
	if code := h.a.run([]string{"run", "--push", dir, "--", "make"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	var ex obj
	json.Unmarshal([]byte(api.lastBody("POST /v1/vms/vm-1/exec")), &ex)
	if ex["cwd"] != "/home/dev/api" {
		t.Errorf("exec body %v", ex)
	}
}

func TestRunRelativeCwdIsFromHome(t *testing.T) {
	api := newJobAPI(t)
	api.execLines = exitWith(0, nil)
	h := newHarness(t, api)
	if code := h.a.run([]string{"run", "--cwd", "sub dir", "--", "ls"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var ex struct{ Cmd []string }
	json.Unmarshal([]byte(api.lastBody("POST /v1/vms/vm-1/exec")), &ex)
	if len(ex.Cmd) != 5 || ex.Cmd[3] != "sub dir" || ex.Cmd[4] != "ls" {
		t.Errorf("cmd %q", ex.Cmd)
	}
}

func TestForwardArgv(t *testing.T) {
	api := newJobAPI(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--remote", "8080"}, "-L 127.0.0.1:8080:127.0.0.1:8080"},
		{[]string{"--remote", "3000", "--local", "9000", "--bind", "0.0.0.0"}, "-L 0.0.0.0:9000:127.0.0.1:3000"},
		{[]string{"--reverse", "--local", "7777"}, "-R 127.0.0.1:7777:127.0.0.1:7777"},
		{[]string{"--reverse", "--local", "11434", "--remote", "11500"}, "-R 127.0.0.1:11500:127.0.0.1:11434"},
	}
	for _, c := range cases {
		h := newHarness(t, api)
		var argv []string
		h.a.runProg = func(v []string) error { argv = v; return nil }
		if code := h.a.run(append([]string{"forward", "vm-1"}, c.args...)); code != 0 {
			t.Fatalf("%v: exit %d: %s", c.args, code, h.err)
		}
		got := strings.Join(argv, " ")
		if !strings.Contains(got, c.want+" dev@10.0.0.5") || !strings.Contains(got, "-N -o ExitOnForwardFailure=yes") ||
			!strings.Contains(got, "ProxyCommand=ssh -o StrictHostKeyChecking=accept-new -W %h:%p -p 2222 -- boat@gw.example") {
			t.Errorf("%v: argv %q", c.args, got)
		}
	}
	h := newHarness(t, api)
	h.a.runProg = func([]string) error { t.Error("no tunnel without a port"); return nil }
	if code := h.a.run([]string{"forward", "vm-1"}); code != 2 {
		t.Errorf("no --remote: exit %d", code)
	}
	if code := h.a.run([]string{"forward", "vm-1", "--reverse"}); code != 2 {
		t.Errorf("no --local: exit %d", code)
	}
}

func TestForwardRetries(t *testing.T) {
	defer func(a, b time.Duration) { forwardHold, forwardBackoff = a, b }(forwardHold, forwardBackoff)
	forwardBackoff = 0

	api := newJobAPI(t)
	boom := errors.New("exit status 255")

	// A failure at startup shows at once.
	forwardHold = time.Hour
	h := newHarness(t, api)
	runs := 0
	h.a.runProg = func([]string) error { runs++; return boom }
	if code := h.a.run([]string{"forward", "vm-1", "--remote", "80"}); code != 1 || runs != 1 {
		t.Errorf("startup failure: exit %d, %d runs", code, runs)
	}

	// A lost tunnel asks for the address again, and tries five more times.
	forwardHold = 0
	before := api.count("POST /v1/vms/vm-1/ssh")
	h = newHarness(t, api)
	runs = 0
	h.a.runProg = func([]string) error {
		runs++
		if runs == 1 {
			return boom // held long enough (forwardHold 0)
		}
		forwardHold = time.Hour // from here, fast failures
		if runs < 7 {
			return boom
		}
		t.Error("too many tries")
		return nil
	}
	if code := h.a.run([]string{"forward", "vm-1", "--remote", "80"}); code != 1 || runs != 6 {
		t.Errorf("lost: exit %d, %d runs: %s", code, runs, h.err)
	}
	if got := api.count("POST /v1/vms/vm-1/ssh") - before; got != runs {
		t.Errorf("%d ssh lookups for %d runs", got, runs)
	}
	forwardHold = 0
	h = newHarness(t, api)
	runs = 0
	h.a.runProg = func([]string) error {
		if runs++; runs == 1 {
			return boom
		}
		return nil // Ctrl+C or clean end
	}
	if code := h.a.run([]string{"forward", "vm-1", "--remote", "80"}); code != 0 || runs != 2 {
		t.Errorf("recovered: exit %d, %d runs", code, runs)
	}
}

func TestSSHConfig(t *testing.T) {
	api := newJobAPI(t)
	h := newHarness(t, api)
	if code := h.a.run([]string{"ssh-config", "vm-1"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	path := filepath.Join(h.home, ".ssh", "boat", "vm-1.conf")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "Host vm-1 dev\n  HostName 10.0.0.5\n  Port 22\n  User dev\n  HostKeyAlias boat-vm-1\n  StrictHostKeyChecking accept-new\n" +
		"  ProxyCommand ssh -o StrictHostKeyChecking=accept-new -W %h:%p -p 2222 -- boat@gw.example\n"
	if string(b) != want {
		t.Errorf("got\n%s", b)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", st.Mode().Perm())
	}
	if !strings.Contains(h.err.String(), "Include ~/.ssh/boat/*.conf") {
		t.Errorf("no Include hint: %s", h.err)
	}
	h.out.Reset()
	if code := h.a.run([]string{"ssh-config", "vm-1", "--print"}); code != 0 || h.out.String() != want {
		t.Errorf("--print exit %d: %q", code, h.out)
	}
}

func TestHost(t *testing.T) {
	api := newJobAPI(t)
	h := newHarness(t, api)
	if code := h.a.run([]string{"host", "vm-1", "3000"}); code != 0 || !strings.HasPrefix(h.out.String(), "https://3000-vm.example/") {
		t.Fatalf("exit %d: %q %s", code, h.out, h.err)
	}
	if !strings.Contains(api.lastBody("POST /v1/vms/vm-1/ports"), `"auth":"private"`) {
		t.Errorf("default must be private: %s", api.lastBody("POST /v1/vms/vm-1/ports"))
	}
	h.out.Reset()
	if code := h.a.run([]string{"host", "vm-1", "3000", "--public", "--json"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var o obj
	json.Unmarshal(h.out.Bytes(), &o)
	if o["sandboxId"] != "vm-1" || o["port"] != float64(3000) || o["access"] != "public" || o["isProtected"] != false || o["localhost"] != false {
		t.Errorf("json %v", o)
	}
	if code := h.a.run([]string{"host", "vm-1", "3000", "--public", "--private"}); code != 2 {
		t.Errorf("conflict: exit %d", code)
	}
	if code := h.a.run([]string{"host", "vm-1", "99999"}); code != 2 {
		t.Errorf("bad port: exit %d", code)
	}
}

func TestHostArgsInGuest(t *testing.T) {
	cases := map[string]string{
		"3000":              "host self 3000",
		"3000 --public":     "host self 3000 --public",
		"list":              "ports ls self",
		"url 3001":          "ports url self 3001",
		"url 3001 --public": "host self 3001 --public",
		"hide 3000":         "ports rm self 3000",
	}
	for in, want := range cases {
		if got := strings.Join(hostArgs(strings.Fields(in)), " "); got != want {
			t.Errorf("host %s = %q, want %q", in, got, want)
		}
	}
}

func TestCurrentAlias(t *testing.T) {
	api := newJobAPI(t)
	api.execLines = exitWith(0, nil)
	h := newHarness(t, api)
	if code := h.a.run([]string{"exec", "current", "--", "ls"}); code != 1 || !strings.Contains(h.err.String(), "no current VM") {
		t.Fatalf("no current: exit %d: %s", code, h.err)
	}
	if code := h.a.run([]string{"new"}); code != 0 {
		t.Fatalf("new: %s", h.err)
	}
	if code := h.a.run([]string{"exec", "current", "--", "ls"}); code != 0 {
		t.Errorf("exec current: exit %d: %s", code, h.err)
	}
	if code := h.a.run([]string{"exec", "--", "ls"}); code != 0 {
		t.Errorf("exec without an id: exit %d: %s", code, h.err)
	}
	if api.count("POST /v1/vms/vm-1/exec") != 2 {
		t.Errorf("%v", api.reqs)
	}
	// Another shell has its own.
	other := *h.a
	other.shellID = 1
	if _, err := other.vmAlias("current"); err == nil {
		t.Error("another shell saw this shell's current VM")
	}
	// Stale entries are ignored.
	old := time.Now().Add(-2 * currentMaxAge)
	os.Chtimes(h.a.currentFile(), old, old)
	if _, err := h.a.vmAlias("current"); err == nil {
		t.Error("stale current VM accepted")
	}
}

func TestSelfAlias(t *testing.T) {
	h := newHarness(t, newJobAPI(t))
	if _, err := h.a.vmAlias("self"); err == nil || !strings.Contains(err.Error(), "only inside") {
		t.Errorf("outside a VM: %v", err)
	}
	h.env["BOAT_ID"] = "vm-7"
	if got, err := h.a.vmAlias("self"); err != nil || got != "vm-7" {
		t.Errorf("BOAT_ID: %q %v", got, err)
	}
	delete(h.env, "BOAT_ID")
	h.env["BOAT_DIR"] = t.TempDir()
	want, _ := os.Hostname()
	if got, err := h.a.vmAlias("self"); err != nil || got != want {
		t.Errorf("hostname: %q %v", got, err)
	}
	if got, _ := h.a.vmAlias("my-vm"); got != "my-vm" {
		t.Errorf("plain ref changed: %q", got)
	}
}

func TestNoKeyInGuestSaysHow(t *testing.T) {
	h := newHarness(t, newJobAPI(t))
	h.env["BOAT_API_KEY"], h.env["BOAT_DIR"] = "", t.TempDir()
	if code := h.a.run([]string{"host", "self", "3000"}); code != 1 || !strings.Contains(h.err.String(), "boat secrets set BOAT_API_KEY") {
		t.Errorf("exit %d: %s", code, h.err)
	}
}

func TestNewCount(t *testing.T) {
	api := newJobAPI(t)
	h := newHarness(t, api)
	if code := h.a.run([]string{"new", "--from", "ci-base", "--count", "3", "--name", "w", "--json"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	// One request: the server creates the batch, all or none.
	if n := strings.Count(h.out.String(), "\n"); n != 3 || api.count("POST /v1/vms") != 1 {
		t.Errorf("%d lines, %d creates", n, api.count("POST /v1/vms"))
	}
	if b := api.lastBody("POST /v1/vms"); !strings.Contains(b, `"snapshot":"snap-1"`) || !strings.Contains(b, `"count":3`) || !strings.Contains(b, `"name":"w"`) {
		t.Errorf("body %s", b)
	}

	// A share code goes to the server as is; it imports it once for the batch.
	api = newJobAPI(t)
	h = newHarness(t, api)
	if code := h.a.run([]string{"new", "--from", "bsh_a", "--count", "2"}); code != 0 || api.count("POST /v1/snapshots/import-share") != 0 {
		t.Errorf("share: exit %d, %v", code, api.reqs)
	}
	if b := api.lastBody("POST /v1/vms"); !strings.Contains(b, `"from":"bsh_a"`) || !strings.Contains(b, `"count":2`) {
		t.Errorf("body %s", b)
	}

	// No --from needed any more.
	api = newJobAPI(t)
	h = newHarness(t, api)
	if code := h.a.run([]string{"new", "--count", "2"}); code != 0 || api.count("POST /v1/vms") != 1 {
		t.Errorf("plain batch: exit %d %s", code, h.err)
	}

	// All or none: a refused batch leaves nothing to roll back here.
	api = newJobAPI(t)
	api.failFrom = -1
	h = newHarness(t, api)
	if code := h.a.run([]string{"new", "--count", "4"}); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.err.String(), "all or none") || h.out.Len() != 0 || api.count("POST /v1/vms") != 1 {
		t.Errorf("out %q err %q reqs %v", h.out, h.err, api.reqs)
	}

	h = newHarness(t, newJobAPI(t))
	for _, args := range [][]string{{"new", "--count", "0"}, {"new", "--count", "17"}, {"new", "--count", "2", "--wait-setup"}} {
		if code := h.a.run(args); code != 2 {
			t.Errorf("%v: exit %d: %s", args, code, h.err)
		}
	}
}

func TestForceStopAndFailFast(t *testing.T) {
	api := newJobAPI(t)
	h := newHarness(t, api)
	for _, c := range []struct {
		args       []string
		route, has string
	}{
		{[]string{"stop", "vm-1", "--force"}, "POST /v1/vms/vm-1/stop", `"force":true`},
		{[]string{"new", "--fail-fast"}, "POST /v1/vms", `"fail_fast":true`},
		{[]string{"fork", "vm-1", "--fail-fast"}, "POST /v1/vms/vm-1/fork", `"fail_fast":true`},
		{[]string{"resume", "vm-1", "--fail-fast"}, "POST /v1/vms/vm-1/start", `"fail_fast":true`},
		{[]string{"start", "vm-1", "--fail-fast"}, "POST /v1/vms/vm-1/start", `"fail_fast":true`},
	} {
		if code := h.a.run(c.args); code != 0 {
			t.Errorf("%v: exit %d: %s", c.args, code, h.err)
			continue
		}
		if b := api.lastBody(c.route); !strings.Contains(b, c.has) {
			t.Errorf("%v: body %q, want %s", c.args, b, c.has)
		}
	}
	h.a.run([]string{"stop", "vm-1"})
	if b := api.lastBody("POST /v1/vms/vm-1/stop"); strings.Contains(b, "force") {
		t.Errorf("plain stop sent %q", b)
	}
}

func TestPromptProviderAlias(t *testing.T) {
	var reqs bytes.Buffer
	srv := fakeAPI(t, &reqs)
	home := t.TempDir()
	var out, errw bytes.Buffer
	a := &app{out: &out, errw: &errw, in: strings.NewReader(""), getenv: func(k string) string {
		return map[string]string{"HOME": home, "BOAT_CONFIG": filepath.Join(home, "c"), "BOAT_URL": srv.URL, "BOAT_API_KEY": "k"}[k]
	}}
	a.run([]string{"prompt", "vm1", "hi", "--provider", "codex"})
	if !strings.Contains(reqs.String(), `"agent":"codex"`) {
		t.Errorf("%s", reqs.String())
	}
}

func TestEnvSecretsAndCombinedSetFlags(t *testing.T) {
	mux := http.NewServeMux()
	var (
		mu     sync.Mutex
		reqs   []string
		bodies = map[string]string{}
	)
	record := func(r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		k := r.Method + " " + r.URL.Path
		mu.Lock()
		reqs = append(reqs, k)
		bodies[k] = string(b)
		mu.Unlock()
	}
	mux.HandleFunc("POST /v1/environments", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"env_1","name":"base","image":"ubuntu","size":"small","version":1,"is_default":false}`)
	})
	mux.HandleFunc("PUT /v1/environments/{id}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"env_1","name":"`+r.PathValue("id")+`","image":"ubuntu","size":"large","version":2,"is_default":false}`)
	})
	mux.HandleFunc("PUT /api/v1/environments/{id}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"success":true,"environment":{"id":"env_1","name":"`+r.PathValue("id")+`","passSecrets":true}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	runCLI := func(args ...string) (int, string, string) {
		home := t.TempDir()
		var out, errw bytes.Buffer
		a := &app{out: &out, errw: &errw, in: strings.NewReader(""), getenv: func(k string) string {
			return map[string]string{"HOME": home, "BOAT_CONFIG": filepath.Join(home, "c"), "BOAT_URL": srv.URL, "BOAT_API_KEY": "k"}[k]
		}}
		code := a.run(args)
		return code, out.String(), errw.String()
	}

	// 1. env new NAME --secrets false -> sends secrets: [] (not ["false"])
	mu.Lock()
	reqs = nil
	bodies = map[string]string{}
	mu.Unlock()
	if code, _, errStr := runCLI("env", "new", "no-sec", "--secrets", "false", "--json"); code != 0 {
		t.Fatalf("env new --secrets false exit %d: %s", code, errStr)
	}
	var newFalse map[string]any
	if err := json.Unmarshal([]byte(bodies["POST /v1/environments"]), &newFalse); err != nil {
		t.Fatalf("decode POST /v1/environments: %v (%s)", err, bodies["POST /v1/environments"])
	}
	secSlice, ok := newFalse["secrets"].([]any)
	if !ok || len(secSlice) != 0 {
		t.Fatalf("expected secrets: [], got %#v in %s", newFalse["secrets"], bodies["POST /v1/environments"])
	}

	// 2. env new NAME --secrets true -> omits secrets (all-secrets default)
	mu.Lock()
	reqs = nil
	bodies = map[string]string{}
	mu.Unlock()
	if code, _, errStr := runCLI("env", "new", "all-sec", "--secrets", "true", "--json"); code != 0 {
		t.Fatalf("env new --secrets true exit %d: %s", code, errStr)
	}
	var newTrue map[string]any
	if err := json.Unmarshal([]byte(bodies["POST /v1/environments"]), &newTrue); err != nil {
		t.Fatalf("decode POST /v1/environments: %v", err)
	}
	if _, exists := newTrue["secrets"]; exists {
		t.Fatalf("expected secrets omitted for --secrets true, got %s", bodies["POST /v1/environments"])
	}

	// 3. env set ENV --secrets true --image ubuntu --var K=V -> never drops accompanying flags
	mu.Lock()
	reqs = nil
	bodies = map[string]string{}
	mu.Unlock()
	if code, _, errStr := runCLI("env", "set", "base", "--secrets", "true", "--image", "ubuntu", "--size", "large", "--var", "FOO=bar", "--json"); code != 0 {
		t.Fatalf("env set --secrets true with flags exit %d: %s", code, errStr)
	}
	var setNative map[string]any
	if err := json.Unmarshal([]byte(bodies["PUT /v1/environments/base"]), &setNative); err != nil {
		t.Fatalf("expected PUT /v1/environments/base with accompanying flags, got reqs=%v body=%q err=%v", reqs, bodies["PUT /v1/environments/base"], err)
	}
	if setNative["image"] != "ubuntu" || setNative["size"] != "large" {
		t.Fatalf("accompanying flags lost: %#v", setNative)
	}
	if envMap, _ := setNative["env"].(map[string]any); envMap["FOO"] != "bar" {
		t.Fatalf("accompanying --var lost: %#v", setNative)
	}
	if setNative["reset_secrets"] != true {
		t.Fatalf("expected reset_secrets: true on PUT /v1/environments/base, got %#v", setNative)
	}

	// 3b. env set ENV --secrets true alone -> sends {"reset_secrets": true} on PUT /v1/environments/base
	mu.Lock()
	reqs = nil
	bodies = map[string]string{}
	mu.Unlock()
	if code, _, errStr := runCLI("env", "set", "base", "--secrets", "true", "--json"); code != 0 {
		t.Fatalf("env set --secrets true exit %d: %s", code, errStr)
	}
	var setResetOnly map[string]any
	if err := json.Unmarshal([]byte(bodies["PUT /v1/environments/base"]), &setResetOnly); err != nil {
		t.Fatalf("decode PUT /v1/environments/base: %v", err)
	}
	if setResetOnly["reset_secrets"] != true {
		t.Fatalf("expected reset_secrets: true, got %#v", setResetOnly)
	}
	if _, hasSecrets := setResetOnly["secrets"]; hasSecrets {
		t.Fatalf("expected secrets omitted when reset_secrets is true, got %#v", setResetOnly)
	}

	// 4. env set ENV --secrets false --var A=1 -> sends secrets: [] alongside --var
	mu.Lock()
	reqs = nil
	bodies = map[string]string{}
	mu.Unlock()
	if code, _, errStr := runCLI("env", "set", "base", "--secrets", "false", "--var", "A=1", "--json"); code != 0 {
		t.Fatalf("env set --secrets false --var exit %d: %s", code, errStr)
	}
	var setFalse map[string]any
	if err := json.Unmarshal([]byte(bodies["PUT /v1/environments/base"]), &setFalse); err != nil {
		t.Fatalf("decode PUT /v1/environments/base: %v", err)
	}
	if s, ok := setFalse["secrets"].([]any); !ok || len(s) != 0 {
		t.Fatalf("expected secrets: [], got %#v", setFalse["secrets"])
	}
	if envMap, _ := setFalse["env"].(map[string]any); envMap["A"] != "1" {
		t.Fatalf("expected env A=1 preserved, got %#v", setFalse)
	}
}

func TestForkCloneAndResumeTTLJSONOutput(t *testing.T) {
	patchCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"`+r.PathValue("id")+`","name":"parent","size":"small","state":"stopped","ttl_s":3600,"expires_at":"2026-01-01T01:00:00Z"}`)
	})
	mux.HandleFunc("POST /v1/vms/{id}/fork", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			TTLS *int64 `json:"ttl_s"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if body.TTLS != nil && *body.TTLS == 0 {
			io.WriteString(w, `{"id":"vm_forked","name":"child","size":"small","state":"running","ttl_s":0}`)
			return
		}
		ttl := int64(3600)
		exp := "2026-01-01T01:00:00Z"
		if body.TTLS != nil {
			ttl = *body.TTLS
			exp = "2026-01-01T05:00:00Z"
		}
		fmt.Fprintf(w, `{"id":"vm_forked","name":"child","size":"small","state":"running","ttl_s":%d,"expires_at":%q}`, ttl, exp)
	})
	mux.HandleFunc("POST /v1/vms/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			TTLS *int64 `json:"ttl_s"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if body.TTLS != nil && *body.TTLS == 0 {
			io.WriteString(w, `{"id":"`+r.PathValue("id")+`","name":"parent","size":"small","state":"running","ttl_s":0}`)
			return
		}
		ttl := int64(3600)
		exp := "2026-01-01T01:00:00Z"
		if body.TTLS != nil {
			ttl = *body.TTLS
			exp = "2026-01-01T05:00:00Z"
		}
		fmt.Fprintf(w, `{"id":%q,"name":"parent","size":"small","state":"running","ttl_s":%d,"expires_at":%q}`, r.PathValue("id"), ttl, exp)
	})
	mux.HandleFunc("PATCH /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		patchCalled = true
		http.Error(w, "unexpected follow-up PATCH", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	runJSON := func(args ...string) map[string]any {
		t.Helper()
		home := t.TempDir()
		var out, errw bytes.Buffer
		a := &app{out: &out, errw: &errw, in: strings.NewReader(""), getenv: func(k string) string {
			return map[string]string{"HOME": home, "BOAT_CONFIG": filepath.Join(home, "c"), "BOAT_URL": srv.URL, "BOAT_API_KEY": "k"}[k]
		}}
		args = append(args, "--json")
		if code := a.run(args); code != 0 {
			t.Fatalf("%v exit %d: %s", args, code, errw.String())
		}
		var got map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &got); err != nil {
			t.Fatalf("decode %q: %v", out.String(), err)
		}
		return got
	}

	forked := runJSON("fork", "vm_parent", "--ttl", "1800")
	if forked["id"] != "vm_forked" || forked["ttl_s"] != float64(1800) || forked["expires_at"] != "2026-01-01T05:00:00Z" {
		t.Fatalf("fork --ttl --json returned stale TTL: %#v", forked)
	}

	cloned := runJSON("clone", "vm_parent", "--ttl", "900")
	if cloned["id"] != "vm_forked" || cloned["ttl_s"] != float64(900) || cloned["expires_at"] != "2026-01-01T05:00:00Z" {
		t.Fatalf("clone --ttl --json returned stale TTL: %#v", cloned)
	}

	forkInf := runJSON("fork", "vm_parent", "--ttl", "infinite")
	if forkInf["ttl_s"] != float64(0) {
		t.Fatalf("fork --ttl infinite --json ttl_s: %#v", forkInf)
	}

	resumed := runJSON("resume", "vm_parent", "--ttl", "7200")
	if resumed["id"] != "vm_parent" || resumed["ttl_s"] != float64(7200) || resumed["expires_at"] != "2026-01-01T05:00:00Z" {
		t.Fatalf("resume --ttl --json returned stale TTL: %#v", resumed)
	}

	noStop := runJSON("resume", "vm_parent", "--no-auto-stop")
	if noStop["ttl_s"] != float64(0) {
		t.Fatalf("resume --no-auto-stop --json ttl_s: %#v", noStop)
	}
	if _, hasExp := noStop["expires_at"]; hasExp {
		t.Fatalf("expected expires_at cleared on --no-auto-stop, got %#v", noStop)
	}

	resumedInf := runJSON("resume", "vm_parent", "--ttl", "infinite")
	if resumedInf["ttl_s"] != float64(0) {
		t.Fatalf("resume --ttl infinite --json ttl_s: %#v", resumedInf)
	}

	if patchCalled {
		t.Fatal("expected atomic ttl_s in fork/start without follow-up PATCH")
	}
}

func TestVMSnapshotsTextColumns(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"vm1","name":"dev","state":"running"}`)
	})
	mux.HandleFunc("GET /v1/vms/vm1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"snapshots":[`+
			`{"id":"snap_1","name":"pre-deploy","created_at":"2026-03-01T12:00:00Z","disk_gib":20,"mem_mib":2048},`+
			`{"id":"snap_2","name":"legacy","created":"2026-03-02T13:00:00Z","size_bytes":104857600}`+
			`]}`)
	})
	mux.HandleFunc("POST /v1/vms/vm1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		io.WriteString(w, `{"id":"snap_3","name":"fresh","created_at":"2026-03-03T14:00:00Z","disk_gib":40,"mem_mib":4096}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	runText := func(args ...string) string {
		t.Helper()
		home := t.TempDir()
		var out, errw bytes.Buffer
		a := &app{out: &out, errw: &errw, in: strings.NewReader(""), getenv: func(k string) string {
			return map[string]string{"HOME": home, "BOAT_CONFIG": filepath.Join(home, "c"), "BOAT_URL": srv.URL, "BOAT_API_KEY": "k"}[k]
		}}
		if code := a.run(args); code != 0 {
			t.Fatalf("%v exit %d: %s", args, code, errw.String())
		}
		return out.String()
	}

	lsOut := runText("snapshots", "vm1")
	for _, want := range []string{"CREATED_AT", "SIZE", "snap_1", "2026-03-01T12:00:00Z", "20 GiB / 2048 MiB", "snap_2", "2026-03-02T13:00:00Z", "104857600"} {
		if !strings.Contains(lsOut, want) {
			t.Fatalf("snapshots ls text missing %q in:\n%s", want, lsOut)
		}
	}

	createOut := runText("snapshots", "vm1", "create", "--name", "fresh")
	for _, want := range []string{"CREATED_AT", "SIZE", "snap_3", "fresh", "2026-03-03T14:00:00Z", "40 GiB / 4096 MiB"} {
		if !strings.Contains(createOut, want) {
			t.Fatalf("snapshots create text missing %q in:\n%s", want, createOut)
		}
	}
}

func TestRound24CLIParityLifecycleAndVisibility(t *testing.T) {
	var reqs []string
	var failResize bool
	vmState := "running"
	vmSize := "default"

	mux := http.NewServeMux()
	rec := func(r *http.Request) string {
		b, _ := io.ReadAll(r.Body)
		s := strings.TrimSpace(string(b))
		line := r.Method + " " + r.URL.RequestURI()
		if s != "" {
			line += " " + s
		}
		reqs = append(reqs, line)
		return s
	}
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"vm1","name":"dev","size":%q,"state":%q,"image":"ubuntu-24.04"}`, vmSize, vmState)
	})
	mux.HandleFunc("POST /v1/vms/vm1/stop", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		vmState = "stopped"
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"vm1","name":"dev","size":%q,"state":"stopped","image":"ubuntu-24.04"}`, vmSize)
	})
	mux.HandleFunc("POST /v1/vms/vm1/resize", func(w http.ResponseWriter, r *http.Request) {
		body := rec(r)
		w.Header().Set("Content-Type", "application/json")
		if vmState == "running" {
			w.WriteHeader(409)
			io.WriteString(w, `{"error":{"code":"conflict","message":"vm is running"}}`)
			return
		}
		if failResize {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"code":"invalid_request","message":"unknown size"}}`)
			return
		}
		var req struct {
			Size string `json:"size"`
		}
		json.Unmarshal([]byte(body), &req)
		vmSize = req.Size
		fmt.Fprintf(w, `{"id":"vm1","name":"dev","size":%q,"state":"stopped","image":"ubuntu-24.04"}`, vmSize)
	})
	mux.HandleFunc("POST /v1/vms/vm1/start", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		vmState = "running"
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"vm1","name":"dev","size":%q,"state":"running","image":"ubuntu-24.04"}`, vmSize)
	})
	mux.HandleFunc("POST /v1/vms/vm1/fork", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		io.WriteString(w, `{"id":"vm_child","name":"child","size":"default","state":"running","image":"ubuntu-24.04"}`)
	})
	childState := "running"
	mux.HandleFunc("POST /v1/vms/vm_child/stop", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		childState = "stopped"
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"vm_child","name":"child","size":"default","state":"stopped","image":"ubuntu-24.04"}`)
	})
	mux.HandleFunc("POST /v1/vms/vm_child/resize", func(w http.ResponseWriter, r *http.Request) {
		body := rec(r)
		w.Header().Set("Content-Type", "application/json")
		if childState == "running" {
			w.WriteHeader(409)
			io.WriteString(w, `{"error":{"code":"conflict","message":"vm is running"}}`)
			return
		}
		if failResize {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"code":"invalid_request","message":"unknown size"}}`)
			return
		}
		var req struct {
			Size string `json:"size"`
		}
		json.Unmarshal([]byte(body), &req)
		fmt.Fprintf(w, `{"id":"vm_child","name":"child","size":%q,"state":"stopped","image":"ubuntu-24.04"}`, req.Size)
	})
	mux.HandleFunc("POST /v1/vms/vm_child/start", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		childState = "running"
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"vm_child","name":"child","size":"large","state":"running","image":"ubuntu-24.04"}`)
	})
	mux.HandleFunc("DELETE /v1/vms/vm_child", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
	mux.HandleFunc("POST /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		io.WriteString(w, `{"id":"vm_run","name":"run","size":"default","state":"running","image":"ubuntu-24.04"}`)
	})
	mux.HandleFunc("POST /v1/vms/vm_run/exec", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, `{"exit":0}`+"\n")
	})
	mux.HandleFunc("DELETE /v1/vms/vm_run", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
	mux.HandleFunc("GET /v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"snapshots":[{"id":"snap_sh","name":"shared-snap","vm_id":"vm1","created_at":"2026-01-01T00:00:00Z"}]}`)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	execCLI := func(args ...string) (int, string, string) {
		home := t.TempDir()
		var out, errw bytes.Buffer
		a := &app{out: &out, errw: &errw, in: strings.NewReader(""), getenv: func(k string) string {
			return map[string]string{"HOME": home, "BOAT_CONFIG": filepath.Join(home, "c"), "BOAT_URL": srv.URL, "BOAT_API_KEY": "k"}[k]
		}}
		code := a.run(args)
		return code, out.String(), errw.String()
	}

	// 1. resume --type on an already-running VM stops -> resizes -> starts with atomic ttl_s & no_env
	reqs = nil
	if code, _, errStr := execCLI("resume", "vm1", "--type", "large", "--no-env", "--ttl", "1200"); code != 0 {
		t.Fatalf("resume running VM with --type failed (%d): %s", code, errStr)
	}
	wantResumeReqs := []string{
		"GET /v1/vms/vm1",
		"POST /v1/vms/vm1/stop",
		`POST /v1/vms/vm1/resize {"discard_memory":true,"size":"large"}`,
		`POST /v1/vms/vm1/start {"no_env":true,"ttl_s":1200}`,
	}
	if strings.Join(reqs, "\n") != strings.Join(wantResumeReqs, "\n") {
		t.Fatalf("resume requests:\ngot:  %v\nwant: %v", reqs, wantResumeReqs)
	}

	// 2. resume --type when resize fails on a previously running VM restores running state
	reqs = nil
	failResize = true
	if code, _, _ := execCLI("resume", "vm1", "--type", "xlarge"); code == 0 {
		t.Fatal("expected resume with rejected resize to fail")
	}
	if got := reqs[len(reqs)-1]; got != `POST /v1/vms/vm1/start {"ttl_s":0}` {
		t.Fatalf("restore start: %v", reqs)
	}
	reqs = nil
	failResize, vmState = true, "running"
	if code, _, _ := execCLI("resume", "vm1", "--type", "xlarge", "--no-env", "-e", "A=b"); code == 0 {
		t.Fatal("expected failure")
	}
	if got := reqs[len(reqs)-1]; got != `POST /v1/vms/vm1/start {"ttl_s":0}` {
		t.Fatalf("restore is the previous run (no new --no-env/env): %v", reqs)
	}
	reqs = nil
	failResize, vmState = true, "running"
	if code, _, _ := execCLI("resume", "vm1", "--type", "xlarge"); code == 0 {
		t.Fatal("expected failure")
	}
	if vmState != "running" {
		t.Fatalf("expected previously running VM to be restarted after failed resize, got state %q (reqs: %v)", vmState, reqs)
	}

	// 3. fork --type --no-env --ttl succeeds via fork -> stop -> resize -> start
	failResize = false
	childState = "running"
	reqs = nil
	if code, _, errStr := execCLI("fork", "vm1", "--name", "child", "--type", "large", "--no-env", "--ttl", "900"); code != 0 {
		t.Fatalf("fork --type failed (%d): %s", code, errStr)
	}
	wantForkReqs := []string{
		"GET /v1/vms/vm1",
		`POST /v1/vms/vm1/fork {"name":"child","no_env":true,"ttl_s":900}`,
		"POST /v1/vms/vm_child/stop",
		`POST /v1/vms/vm_child/resize {"discard_memory":true,"size":"large"}`,
		`POST /v1/vms/vm_child/start {"no_env":true,"ttl_s":900}`,
	}
	if strings.Join(reqs, "\n") != strings.Join(wantForkReqs, "\n") {
		t.Fatalf("fork --type requests:\ngot:  %v\nwant: %v", reqs, wantForkReqs)
	}

	// 4. fork --type cleans up forked VM on resize failure
	failResize = true
	childState = "running"
	reqs = nil
	if code, _, _ := execCLI("fork", "vm1", "--type", "xlarge"); code == 0 {
		t.Fatal("expected fork --type with rejected resize to fail")
	}
	if reqs[len(reqs)-1] != "DELETE /v1/vms/vm_child" {
		t.Fatalf("expected cleanup DELETE /v1/vms/vm_child on failed fork retype, got %v", reqs)
	}

	// 5. boat run --no-env sets BOTH no_env: true AND secrets: []
	reqs = nil
	if code, _, errStr := execCLI("run", "--no-env", "--", "true"); code != 0 {
		t.Fatalf("run --no-env failed (%d): %s", code, errStr)
	}
	if len(reqs) == 0 || !strings.Contains(reqs[0], `"no_env":true`) || !strings.Contains(reqs[0], `"secrets":[]`) {
		t.Fatalf("expected POST /v1/vms with no_env:true and secrets:[], got %v", reqs)
	}

	// 6. boat snapshots team --visibility shared|all and invalid value
	reqs = nil
	if code, _, errStr := execCLI("snapshots", "team", "--visibility", "shared"); code != 0 {
		t.Fatalf("snapshots team --visibility shared failed (%d): %s", code, errStr)
	}
	if code, _, errStr := execCLI("snapshots", "team", "--visibility", "all"); code != 0 {
		t.Fatalf("snapshots team --visibility all failed (%d): %s", code, errStr)
	}
	wantSnapReqs := []string{"GET /v1/snapshots?visibility=shared", "GET /v1/snapshots?visibility=all"}
	if strings.Join(reqs, "\n") != strings.Join(wantSnapReqs, "\n") {
		t.Fatalf("snapshots team --visibility requests: got %v, want %v", reqs, wantSnapReqs)
	}
	if code, _, _ := execCLI("snapshots", "team", "--visibility", "private"); code != 2 {
		t.Fatalf("expected exit 2 for invalid --visibility, got %d", code)
	}
}

func TestUsageWindowFlags(t *testing.T) {
	api := newJobAPI(t)
	h := newHarness(t, api)
	if code := h.a.run([]string{"usage", "--from", "2026-01-01", "--to", "2026-02-01T00:00:00Z"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	want := "QUERY from=2026-01-01&to=2026-02-01T00%3A00%3A00Z"
	if !slices.Contains(api.reqs, want) {
		t.Errorf("want %q in %v", want, api.reqs)
	}
	api = newJobAPI(t)
	h = newHarness(t, api)
	if code := h.a.run([]string{"usage"}); code != 0 || !slices.Contains(api.reqs, "QUERY ") {
		t.Errorf("plain usage: exit %d %v", code, api.reqs)
	}
	if code := h.a.run([]string{"usage", "extra"}); code != 2 {
		t.Errorf("positional: exit %d", code)
	}
}

func TestNewFailsOnEnvError(t *testing.T) {
	api := newJobAPI(t)
	api.createExtra = obj{"env_error": "push failed", "agent_ready": false}
	h := newHarness(t, api)
	if code := h.a.run([]string{"new"}); code != 1 || !strings.Contains(h.err.String(), "push failed") || h.out.Len() != 0 {
		t.Errorf("exit %d out %q err %q", code, h.out, h.err)
	}
	h = newHarness(t, api)
	if code := h.a.run([]string{"new", "--json"}); code != 1 {
		t.Errorf("json exit %d", code)
	}
	if o := h.out.String(); !strings.Contains(o, `"event":"error"`) || strings.Contains(o, `"event":"ready"`) || !strings.Contains(o, "push failed") {
		t.Errorf("json out %q", o)
	}
}

func TestRunFailsOnEnvError(t *testing.T) {
	api := newJobAPI(t)
	api.createExtra = obj{"env_error": "push failed"}
	h := newHarness(t, api)
	if code := h.a.run([]string{"run", "--", "true"}); code == 0 || !strings.Contains(h.err.String(), "push failed") || api.count("POST /v1/vms/vm-1/exec") != 0 {
		t.Errorf("exit %d err %q reqs %v", code, h.err, api.reqs)
	}
}

func TestAIUsageDaysAlwaysFiltered(t *testing.T) {
	api := newJobAPI(t)
	h := newHarness(t, api)
	if code := h.a.run([]string{"ai", "usage"}); code != 0 {
		t.Fatalf("exit %d err %q", code, h.err)
	}
	if !slices.Contains(api.reqs, "QUERY days=30") {
		t.Errorf("default must filter: %v", api.reqs)
	}
	for _, r := range api.reqs {
		if strings.HasPrefix(r, "GET /v1/ai/usage") {
			t.Errorf("unfiltered route used: %v", api.reqs)
		}
	}
	h = newHarness(t, api)
	if code := h.a.run([]string{"ai", "usage", "--days", "0"}); code != 2 {
		t.Errorf("--days 0: exit %d", code)
	}
}

func TestForkResumeStartFailOnEnvError(t *testing.T) {
	for _, args := range [][]string{{"fork", "vm-1"}, {"resume", "vm-1"}, {"start", "vm-1"}} {
		api := newJobAPI(t)
		api.vmExtra = obj{"env_error": "push failed", "agent_ready": false}
		h := newHarness(t, api)
		if code := h.a.run(args); code != 1 || !strings.Contains(h.err.String(), "push failed") || !strings.Contains(h.err.String(), "boat delete vm-1") || h.out.Len() != 0 {
			t.Errorf("%v: exit %d out %q err %q", args, code, h.out, h.err)
		}
		h = newHarness(t, api)
		if code := h.a.run(append(args, "--json")); code != 1 {
			t.Errorf("%v json exit %d", args, code)
		}
		if o := h.out.String(); !strings.Contains(o, `"event":"error"`) || !strings.Contains(o, `"code":"env_error"`) || !strings.Contains(o, "push failed") {
			t.Errorf("%v json out %q", args, o)
		}
	}
}

func TestUsageHelpDocumentsInclusiveTo(t *testing.T) {
	h := newHarness(t, newJobAPI(t))
	h.a.run([]string{"usage", "--help"})
	if !strings.Contains(h.err.String()+h.out.String(), "inclusive") {
		t.Errorf("usage help: %q %q", h.out, h.err)
	}
}

func TestStartEnvStartFailedReportsStopped(t *testing.T) {
	for _, args := range [][]string{{"resume", "vm-1"}, {"start", "vm-1"}} {
		api := newJobAPI(t)
		api.startClosed = true
		h := newHarness(t, api)
		e := h.err.String()
		code := h.a.run(args)
		e = h.err.String()
		if code != 1 || !strings.Contains(e, "stopped") || !strings.Contains(e, "discarded") || strings.Contains(e, "still running") {
			t.Errorf("%v: exit %d err %q", args, code, e)
		}
		h = newHarness(t, api)
		if code := h.a.run(append(args, "--json")); code != 1 || !strings.Contains(h.out.String(), `"code":"env_start_failed"`) || !strings.Contains(h.out.String(), `"state":"stopped"`) {
			t.Errorf("%v json: exit %d out %q", args, code, h.out)
		}
	}
}

func TestStartLegacyEnvUpdateFailed(t *testing.T) {
	for _, args := range [][]string{{"resume", "vm-1"}, {"start", "vm-1"}} {
		api := newJobAPI(t)
		api.startLegacy = true
		h := newHarness(t, api)
		if code := h.a.run(args); code != 1 || !strings.Contains(h.err.String(), "env push failed") || !strings.Contains(h.err.String(), "update") || !strings.Contains(h.err.String(), "previous environment") || strings.Contains(h.err.String(), "boat delete") || h.out.Len() != 0 {
			t.Errorf("%v: exit %d out %q err %q", args, code, h.out, h.err)
		}
		h = newHarness(t, api)
		if code := h.a.run(append(args, "--json")); code != 1 {
			t.Errorf("%v json exit %d", args, code)
		}
		if o := h.out.String(); !strings.Contains(o, `"event":"error"`) || !strings.Contains(o, `"id":"vm-1"`) || !strings.Contains(o, `"code":"env_update_failed"`) {
			t.Errorf("%v json out %q", args, o)
		}
	}
}

func TestLsShowsEnvFailure(t *testing.T) {
	api := newJobAPI(t)
	api.listVMs = []obj{
		{"id": "vm-ok", "name": "a", "size": "small", "state": "running"},
		{"id": "vm-bad", "name": "b", "size": "small", "state": "running", "env_error": "push failed"},
	}
	h := newHarness(t, api)
	if code := h.a.run([]string{"ls"}); code != 0 {
		t.Fatalf("exit %d err %q", code, h.err)
	}
	out := h.out.String()
	if !strings.Contains(out, "running (env failed)") || strings.Count(out, "env failed") != 1 {
		t.Errorf("text out %q", out)
	}
	h = newHarness(t, api)
	if code := h.a.run([]string{"ls", "--json"}); code != 0 {
		t.Fatalf("json exit %d", code)
	}
	if o := h.out.String(); strings.Contains(o, "env failed") || !strings.Contains(o, `"state":"running"`) || !strings.Contains(o, `"env_error":"push failed"`) {
		t.Errorf("json out %q", o)
	}
}

func TestNewCountEnvErrorReportsEverySibling(t *testing.T) {
	for _, jsonMode := range []bool{true, false} {
		api := newJobAPI(t)
		api.batchEnvErr = map[int]string{2: "push failed"}
		h := newHarness(t, api)
		args := []string{"new", "--count", "3"}
		if jsonMode {
			args = append(args, "--json")
		}
		if code := h.a.run(args); code != 1 {
			t.Fatalf("json=%v exit %d", jsonMode, code)
		}
		out, errs := h.out.String(), h.err.String()
		if jsonMode {
			if strings.Count(out, `"event":"ready"`) != 2 || strings.Count(out, `"event":"error"`) != 1 ||
				!strings.Contains(out, `"id":"vm-1-1"`) || !strings.Contains(out, `"id":"vm-1-3"`) || !strings.Contains(out, `"id":"vm-1-2"`) {
				t.Errorf("events: %s", out)
			}
			continue
		}
		for _, id := range []string{"vm-1-1", "vm-1-2", "vm-1-3"} {
			if !strings.Contains(out, id) {
				t.Errorf("%s missing from list: %s", id, out)
			}
		}
		if !strings.Contains(out, "env failed") || !strings.Contains(errs, "vm-1-2") {
			t.Errorf("out %q err %q", out, errs)
		}
	}
}

// r32b: a failed retype restarts a run that reports no_env_run:true no_env,
// even without --no-env, and never with the new -e env.
func TestR32bRetypeFailureRestartsNoEnvRunWithoutSecrets(t *testing.T) {
	var starts []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"vm1","name":"dev","size":"default","state":"running","no_env_run":true}`)
	})
	mux.HandleFunc("POST /v1/vms/vm1/stop", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"vm1","state":"stopped"}`)
	})
	mux.HandleFunc("POST /v1/vms/vm1/resize", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"code":"invalid_request","message":"unknown size"}}`)
	})
	mux.HandleFunc("POST /v1/vms/vm1/start", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		starts = append(starts, strings.TrimSpace(string(b)))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"vm1","state":"running"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for _, args := range [][]string{
		{"resume", "vm1", "--type", "xlarge"},
		{"resume", "vm1", "--type", "xlarge", "-e", "A=b"},
	} {
		starts = nil
		home := t.TempDir()
		var out, errw bytes.Buffer
		a := &app{out: &out, errw: &errw, in: strings.NewReader(""), getenv: func(k string) string {
			return map[string]string{"HOME": home, "BOAT_CONFIG": filepath.Join(home, "c"), "BOAT_URL": srv.URL, "BOAT_API_KEY": "k"}[k]
		}}
		if code := a.run(args); code == 0 {
			t.Fatalf("%v: expected failure", args)
		}
		if len(starts) != 1 || starts[0] != `{"no_env":true,"ttl_s":0}` {
			t.Fatalf("%v: restart bodies = %q, want one {\"no_env\":true,\"ttl_s\":0}", args, starts)
		}
	}
}

// r33: a successful retype of a no_env run starts it no_env too, keeping the
// user's env and ttl.
func TestR33RetypeSuccessKeepsNoEnvRun(t *testing.T) {
	var starts []string
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		js(w, `{"id":"vm1","name":"dev","size":"default","state":"running","no_env_run":true}`)
	})
	mux.HandleFunc("POST /v1/vms/vm1/stop", func(w http.ResponseWriter, r *http.Request) { js(w, `{"id":"vm1","state":"stopped"}`) })
	mux.HandleFunc("POST /v1/vms/vm1/resize", func(w http.ResponseWriter, r *http.Request) { js(w, `{"id":"vm1","size":"large"}`) })
	mux.HandleFunc("POST /v1/vms/vm1/start", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		starts = append(starts, string(b))
		js(w, `{"id":"vm1","state":"running"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	home := t.TempDir()
	var out, errw bytes.Buffer
	a := &app{out: &out, errw: &errw, in: strings.NewReader(""), getenv: func(k string) string {
		return map[string]string{"HOME": home, "BOAT_CONFIG": filepath.Join(home, "c"), "BOAT_URL": srv.URL, "BOAT_API_KEY": "k"}[k]
	}}
	if code := a.run([]string{"resume", "vm1", "--type", "large", "-e", "A=b"}); code != 0 {
		t.Fatalf("code %d: %s", code, errw.String())
	}
	if len(starts) != 1 {
		t.Fatalf("starts = %q", starts)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(starts[0]), &got); err != nil {
		t.Fatal(err)
	}
	env, _ := got["env"].(map[string]any)
	if got["no_env"] != true || env["A"] != "b" {
		t.Fatalf("start body = %s", starts[0])
	}
}

func r35App(t *testing.T, srvURL string) (*app, *bytes.Buffer) {
	home := t.TempDir()
	var out, errw bytes.Buffer
	return &app{out: &out, errw: &errw, in: strings.NewReader(""), getenv: func(k string) string {
		return map[string]string{"HOME": home, "BOAT_CONFIG": filepath.Join(home, "c"), "BOAT_URL": srvURL, "BOAT_API_KEY": "k"}[k]
	}}, &errw
}

// r35: a fork --type of a no_env run starts the retyped child no_env.
func TestR35ForkRetypeKeepsNoEnvRun(t *testing.T) {
	var starts []string
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		js(w, `{"id":"vm1","name":"dev","size":"default","state":"running","no_env_run":true}`)
	})
	mux.HandleFunc("POST /v1/vms/vm1/fork", func(w http.ResponseWriter, r *http.Request) {
		js(w, `{"id":"vmc","name":"c","size":"default","state":"running","no_env_run":true}`)
	})
	mux.HandleFunc("POST /v1/vms/vmc/stop", func(w http.ResponseWriter, r *http.Request) { js(w, `{"id":"vmc","state":"stopped"}`) })
	mux.HandleFunc("POST /v1/vms/vmc/resize", func(w http.ResponseWriter, r *http.Request) { js(w, `{"id":"vmc","size":"large"}`) })
	mux.HandleFunc("POST /v1/vms/vmc/start", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		starts = append(starts, strings.TrimSpace(string(b)))
		js(w, `{"id":"vmc","size":"large","state":"running"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	a, errw := r35App(t, srv.URL)
	if code := a.run([]string{"fork", "vm1", "--type", "large", "--ttl", "900"}); code != 0 {
		t.Fatalf("code %d: %s", code, errw.String())
	}
	if len(starts) != 1 || starts[0] != `{"no_env":true,"ttl_s":900}` {
		t.Fatalf("start bodies = %q", starts)
	}
}

// r35: resume --type whose post-resize start fails issues one restore start
// and returns the original error.
func TestR35ResumeRetypeStartFailureRestores(t *testing.T) {
	var starts []string
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		js(w, `{"id":"vm1","name":"dev","size":"default","state":"running","no_env_run":true,"ttl_s":600}`)
	})
	mux.HandleFunc("POST /v1/vms/vm1/stop", func(w http.ResponseWriter, r *http.Request) { js(w, `{"id":"vm1","state":"stopped"}`) })
	mux.HandleFunc("POST /v1/vms/vm1/resize", func(w http.ResponseWriter, r *http.Request) { js(w, `{"id":"vm1","size":"large"}`) })
	mux.HandleFunc("POST /v1/vms/vm1/start", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		starts = append(starts, strings.TrimSpace(string(b)))
		if len(starts) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(500)
			io.WriteString(w, `{"error":{"code":"internal","message":"boom-original"}}`)
			return
		}
		js(w, `{"id":"vm1","state":"running"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	a, errw := r35App(t, srv.URL)
	if code := a.run([]string{"resume", "vm1", "--type", "large", "-e", "A=b", "--ttl", "60"}); code == 0 {
		t.Fatal("expected failure")
	}
	if len(starts) != 2 || starts[1] != `{"no_env":true,"ttl_s":600}` {
		t.Fatalf("start bodies = %q", starts)
	}
	if !strings.Contains(errw.String(), "boom-original") {
		t.Fatalf("original error missing: %s", errw.String())
	}
}

// r35b helper: retype server whose first start fails and (optionally) the restore too.
func r35bServer(t *testing.T, vmJSON string, failRestore bool, starts *[]string) *httptest.Server {
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
	fail := func(w http.ResponseWriter, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		io.WriteString(w, `{"error":{"code":"internal","message":"`+msg+`"}}`)
	}
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) { js(w, vmJSON) })
	mux.HandleFunc("POST /v1/vms/vm1/stop", func(w http.ResponseWriter, r *http.Request) { js(w, `{"id":"vm1","state":"stopped"}`) })
	mux.HandleFunc("POST /v1/vms/vm1/resize", func(w http.ResponseWriter, r *http.Request) { js(w, `{"id":"vm1","size":"large"}`) })
	mux.HandleFunc("POST /v1/vms/vm1/start", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*starts = append(*starts, strings.TrimSpace(string(b)))
		switch {
		case len(*starts) == 1:
			fail(w, "boom-original")
		case failRestore:
			fail(w, "boom-restore")
		default:
			js(w, `{"id":"vm1","state":"running"}`)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestR35bStoppedVMRetypeNoRestore(t *testing.T) {
	var starts []string
	srv := r35bServer(t, `{"id":"vm1","name":"dev","size":"default","state":"stopped"}`, false, &starts)
	a, errw := r35App(t, srv.URL)
	if code := a.run([]string{"resume", "vm1", "--type", "large", "--no-env", "-e", "A=b"}); code == 0 {
		t.Fatal("expected failure")
	}
	if len(starts) != 1 {
		t.Fatalf("stopped VM must not be restored/started again: %q", starts)
	}
	if !strings.Contains(errw.String(), "boom-original") {
		t.Fatalf("original error missing: %s", errw.String())
	}
}

func TestR35bRunningRetypeRestoreHonorsNoEnvFlag(t *testing.T) {
	var starts []string
	srv := r35bServer(t, `{"id":"vm1","name":"dev","size":"default","state":"running","no_env_run":false,"ttl_s":600}`, false, &starts)
	a, _ := r35App(t, srv.URL)
	if code := a.run([]string{"resume", "vm1", "--type", "large", "--no-env", "-e", "A=b"}); code == 0 {
		t.Fatal("expected failure")
	}
	if len(starts) != 2 || starts[1] != `{"ttl_s":600}` {
		t.Fatalf("start bodies = %q", starts)
	}
}

func TestR35bRestoreFailureReportsBoth(t *testing.T) {
	var starts []string
	srv := r35bServer(t, `{"id":"vm1","name":"dev","size":"default","state":"running","no_env_run":true}`, true, &starts)
	a, errw := r35App(t, srv.URL)
	if code := a.run([]string{"resume", "vm1", "--type", "large"}); code == 0 {
		t.Fatal("expected failure")
	}
	e := errw.String()
	if !strings.Contains(e, "boom-original") || !strings.Contains(e, "boom-restore") {
		t.Fatalf("both errors expected: %s", e)
	}
}

func TestR36RetypeSizeValidatedBeforeStopAndTTLZeroRestore(t *testing.T) {
	var reqs []string
	mux := http.NewServeMux()
	rec := func(r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		line := r.Method + " " + r.URL.RequestURI()
		if s := strings.TrimSpace(string(b)); s != "" {
			line += " " + s
		}
		reqs = append(reqs, line)
	}
	reply := func(w http.ResponseWriter, code int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
	// boatd omits ttl_s when it is 0.
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		reply(w, 200, `{"id":"vm1","name":"dev","size":"default","state":"running","image":"ubuntu-24.04"}`)
	})
	mux.HandleFunc("GET /v1/images", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		reply(w, 200, `{"images":[],"sizes":{"default":{},"large":{}}}`)
	})
	mux.HandleFunc("POST /v1/vms/vm1/stop", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		reply(w, 200, `{"id":"vm1","state":"stopped"}`)
	})
	mux.HandleFunc("POST /v1/vms/vm1/resize", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		reply(w, 200, `{"id":"vm1","size":"large","state":"stopped"}`)
	})
	starts := 0
	mux.HandleFunc("POST /v1/vms/vm1/start", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		starts++
		if starts == 1 {
			reply(w, 500, `{"error":{"code":"internal","message":"boom"}}`)
			return
		}
		reply(w, 200, `{"id":"vm1","state":"running"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	run := func(args ...string) int {
		home := t.TempDir()
		var out, errw bytes.Buffer
		a := &app{out: &out, errw: &errw, in: strings.NewReader(""), getenv: func(k string) string {
			return map[string]string{"HOME": home, "BOAT_CONFIG": filepath.Join(home, "c"), "BOAT_URL": srv.URL, "BOAT_API_KEY": "k"}[k]
		}}
		return a.run(args)
	}

	if code := run("resume", "vm1", "--type", "nosuch"); code == 0 {
		t.Fatal("unknown size must fail")
	}
	if got, want := strings.Join(reqs, "\n"), "GET /v1/vms/vm1\nGET /v1/images"; got != want {
		t.Fatalf("unknown size must not stop the VM:\ngot:  %s\nwant: %s", got, want)
	}

	reqs = nil
	if code := run("resume", "vm1", "--type", "large"); code == 0 {
		t.Fatal("failing start must fail the command")
	}
	if last := reqs[len(reqs)-1]; last != `POST /v1/vms/vm1/start {"ttl_s":0}` {
		t.Fatalf("restore must send ttl_s:0 explicitly: %v", reqs)
	}
}

// r37: a failed resize restores the previous run (its ttl, no secrets flag
// only if that run had it), never the request's --ttl/--no-env/--fail-fast.
func TestR37RetypeResizeFailureRestoresPreviousRun(t *testing.T) {
	var starts []string
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, code int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		js(w, 200, `{"id":"vm1","name":"dev","size":"default","state":"running","ttl_s":3600,"no_env_run":false}`)
	})
	mux.HandleFunc("POST /v1/vms/vm1/stop", func(w http.ResponseWriter, r *http.Request) { js(w, 200, `{"id":"vm1","state":"stopped"}`) })
	mux.HandleFunc("POST /v1/vms/vm1/resize", func(w http.ResponseWriter, r *http.Request) {
		js(w, 400, `{"error":{"code":"invalid_request","message":"unknown size"}}`)
	})
	mux.HandleFunc("POST /v1/vms/vm1/start", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		starts = append(starts, strings.TrimSpace(string(b)))
		js(w, 200, `{"id":"vm1","state":"running"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	home := t.TempDir()
	var out, errw bytes.Buffer
	a := &app{out: &out, errw: &errw, in: strings.NewReader(""), getenv: func(k string) string {
		return map[string]string{"HOME": home, "BOAT_CONFIG": filepath.Join(home, "c"), "BOAT_URL": srv.URL, "BOAT_API_KEY": "k"}[k]
	}}
	if code := a.run([]string{"resume", "vm1", "--type", "xlarge", "--ttl", "60", "--no-env", "--fail-fast", "-e", "A=b"}); code == 0 {
		t.Fatal("expected failure")
	}
	if len(starts) != 1 || starts[0] != `{"ttl_s":3600}` {
		t.Fatalf("restore bodies = %q, want one {\"ttl_s\":3600}", starts)
	}
}

// r39: a no_env fork whose fresh boot failed closed names the stopped child.
func TestR39ForkNoEnvStartFailedNamesChild(t *testing.T) {
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, code int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		js(w, 200, `{"id":"vm1","name":"dev","size":"default","state":"running"}`)
	})
	mux.HandleFunc("POST /v1/vms/vm1/fork", func(w http.ResponseWriter, r *http.Request) {
		js(w, 502, `{"error":{"code":"env_start_failed","message":"wipe failed"},"vm":{"id":"vm9","state":"stopped"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for _, verb := range []string{"fork", "clone"} {
		a, errw := r35App(t, srv.URL)
		if code := a.run([]string{verb, "vm1", "--no-env"}); code != 1 {
			t.Fatalf("%s exit %d", verb, code)
		}
		if e := errw.String(); !strings.Contains(e, "child vm9 was stopped (no_env fresh boot failed") {
			t.Fatalf("%s human: %q", verb, e)
		}
	}
	a, errw := r35App(t, srv.URL)
	var out bytes.Buffer
	a.out = &out
	if code := a.run([]string{"fork", "vm1", "--no-env", "--json"}); code != 1 {
		t.Fatalf("json exit %d", code)
	}
	if o := out.String(); !strings.Contains(o, `"id":"vm9"`) || !strings.Contains(o, `"code":"env_start_failed"`) {
		t.Fatalf("json: %q err %q", o, errw)
	}
}

func r39RetypeServer(t *testing.T, restoreBody string) *httptest.Server {
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, code int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
	n := 0
	mux.HandleFunc("GET /v1/vms/vm1", func(w http.ResponseWriter, r *http.Request) {
		js(w, 200, `{"id":"vm1","name":"dev","size":"default","state":"running","no_env_run":false}`)
	})
	mux.HandleFunc("POST /v1/vms/vm1/stop", func(w http.ResponseWriter, r *http.Request) { js(w, 200, `{"id":"vm1","state":"stopped"}`) })
	mux.HandleFunc("POST /v1/vms/vm1/resize", func(w http.ResponseWriter, r *http.Request) { js(w, 200, `{"id":"vm1","size":"large"}`) })
	mux.HandleFunc("POST /v1/vms/vm1/start", func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			js(w, 502, `{"error":{"code":"env_start_failed","message":"wipe failed"},"vm":{"id":"vm1","state":"stopped"}}`)
			return
		}
		js(w, 200, restoreBody)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestR39ResumeRetypeRestoredReportsRestartedNotStopped(t *testing.T) {
	srv := r39RetypeServer(t, `{"id":"vm1","state":"running"}`)
	a, errw := r35App(t, srv.URL)
	if code := a.run([]string{"resume", "vm1", "--type", "large", "--no-env"}); code != 1 {
		t.Fatalf("exit %d", code)
	}
	e := errw.String()
	if !strings.Contains(e, "wipe failed") || !strings.Contains(e, "restarted with team secrets") || strings.Contains(e, "was stopped") {
		t.Fatalf("err %q", e)
	}
}

func TestR39ResumeRetypeRestoredEnvErrorSurfaced(t *testing.T) {
	srv := r39RetypeServer(t, `{"id":"vm1","state":"running","env_error":"push broke"}`)
	a, errw := r35App(t, srv.URL)
	if code := a.run([]string{"resume", "vm1", "--type", "large", "--no-env"}); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if e := errw.String(); !strings.Contains(e, "push broke") || strings.Contains(e, "was stopped") {
		t.Fatalf("err %q", e)
	}
}
