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
	srv       *httptest.Server
	reqs      []string
	bodies    map[string][]string
	upload    []byte
	execLines func(w http.ResponseWriter, r *http.Request, cmd []string)
	created   int
	failFrom  int // creates after this many get 409 (0 = never)
}

func newJobAPI(t *testing.T) *jobAPI {
	api := &jobAPI{bodies: map[string][]string{}}
	mux := http.NewServeMux()
	j := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		api.Lock()
		api.created++
		n := api.created
		over := api.failFrom > 0 && n > api.failFrom
		api.Unlock()
		if over {
			j(w, 409, obj{"error": obj{"code": "quota", "message": "over quota"}})
			return
		}
		j(w, 201, obj{"id": fmt.Sprintf("vm-%d", n), "name": "", "size": "large", "state": "running"})
	})
	mux.HandleFunc("GET /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !strings.HasPrefix(id, "vm-") {
			j(w, 404, obj{"error": obj{"code": "not_found", "message": "vm not found"}})
			return
		}
		j(w, 200, obj{"id": id, "name": "dev", "size": "large", "state": "running"})
	})
	mux.HandleFunc("GET /v1/vms", func(w http.ResponseWriter, r *http.Request) { j(w, 200, obj{"vms": []obj{}}) })
	mux.HandleFunc("DELETE /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) { j(w, 200, obj{"ok": true}) })
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
	if h.a.run([]string{"run", "--push", "/no/such/dir", "--", "ls"}) != 1 {
		t.Fatal("a missing --push dir should fail")
	}
	if api.count("POST /v1/vms") != 0 {
		t.Errorf("created a VM: %v", api.reqs)
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
			!strings.Contains(got, "ProxyCommand=ssh -o StrictHostKeyChecking=accept-new -W %h:%p -p 2222 boat@gw.example") {
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
		"  ProxyCommand ssh -o StrictHostKeyChecking=accept-new -W %h:%p -p 2222 boat@gw.example\n"
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
	if code := h.a.run([]string{"new", "--from", "ci-base", "--count", "3", "--json"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if n := strings.Count(h.out.String(), "\n"); n != 3 || api.count("POST /v1/vms") != 3 {
		t.Errorf("%d lines, %d creates", n, api.count("POST /v1/vms"))
	}
	if !strings.Contains(api.lastBody("POST /v1/vms"), `"snapshot":"snap-1"`) {
		t.Errorf("body %s", api.lastBody("POST /v1/vms"))
	}

	// One share import, however many VMs.
	api = newJobAPI(t)
	h = newHarness(t, api)
	if code := h.a.run([]string{"new", "--from", "bsh_a", "--count", "2"}); code != 0 || api.count("POST /v1/snapshots/import-share") != 1 {
		t.Errorf("share: exit %d, %v", code, api.reqs)
	}
	if !strings.Contains(api.lastBody("POST /v1/vms"), `"snapshot":"snap-9"`) {
		t.Errorf("body %s", api.lastBody("POST /v1/vms"))
	}

	// All or none.
	api = newJobAPI(t)
	api.failFrom = 2
	h = newHarness(t, api)
	if code := h.a.run([]string{"new", "--from", "ci-base", "--count", "4"}); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.err.String(), "rolled back") || h.out.Len() != 0 {
		t.Errorf("out %q err %q", h.out, h.err)
	}
	deletes := 0
	for _, r := range api.reqs {
		if strings.HasPrefix(r, "DELETE /v1/vms/") {
			deletes++
		}
	}
	if deletes != 2 {
		t.Errorf("%d rollbacks, %v", deletes, api.reqs)
	}

	h = newHarness(t, newJobAPI(t))
	for _, args := range [][]string{{"new", "--count", "2"}, {"new", "--from", "s", "--count", "0"}, {"new", "--from", "ci-base", "--count", "2", "--name", "x"}} {
		if code := h.a.run(args); code != 2 {
			t.Errorf("%v: exit %d: %s", args, code, h.err)
		}
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
