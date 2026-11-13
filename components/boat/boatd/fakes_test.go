package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost implements procs and runner. Each "process" is an in-process fake
// Firecracker API on root/fc.sock; InstanceStart/snapshot-load bring up a fake
// guest agent on root/v.sock whose filesystem is a JSON map stored in the
// VM's rootfs.ext4, so reflink copies (fork, snapshots) are really exercised.
type fakeHost struct {
	mu         sync.Mutex
	next       int
	vms        map[int]*fakeFC // by pid
	cmds       []string
	nft        string
	noSetClock bool // old agent
	noAsync    bool // Firecracker refuses the Async engine
	nftErr     error
	ctErr      error
	nftAtStart map[string]string // ruleset in force when each VM's process started
	idleS      float64
	hangOps    map[string]bool // agent ops the guest never answers
	starts     int             // processes started
	failStart  int             // fail the failStart-th process start (1-based; 0 = never)
	failResume int             // refuse this many resumes (PATCH /vm Resumed) with 503
	failSnap   int             // refuse this many snapshot creates like a full team quota (EDQUOT)
	slowGo     chan struct{}   // "slow" execs (sessions) send their end once this is closed
	loseExec   int             // ignore this many exec requests (lost in a vsock reset)
	execKilled map[string]bool // execs (by argv[0]) the agent reports timed out and killed
	sockDelay  time.Duration   // the API socket appears this long after the start (a stalled fs)
}

func init() { quotaCtl = func(string) error { return nil } }

func newFakeHost() *fakeHost { return &fakeHost{next: 1000, vms: map[int]*fakeFC{}} }

func (h *fakeHost) run(stdin, name string, args ...string) error {
	h.mu.Lock()
	h.cmds = append(h.cmds, name+" "+strings.Join(args, " "))
	if name == "nft" {
		h.nft = stdin
		if h.nftErr != nil {
			defer h.mu.Unlock()
			return h.nftErr
		}
	}
	h.mu.Unlock()
	if name == "conntrack" {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.ctErr
	}
	if name == "btrfs" && len(args) == 3 && args[0] == "subvolume" {
		if args[1] == "create" {
			return os.Mkdir(args[2], 0o711)
		}
		return os.RemoveAll(args[2])
	}
	if name != "cp" {
		return nil
	}
	src, dst := args[len(args)-2], args[len(args)-1]
	flag := "--reflink=auto"
	if runtime.GOOS == "darwin" {
		flag = "-c" // APFS clone: the test disks are 1 GiB sparse
	}
	if out, err := exec.Command("cp", flag, src, dst).CombinedOutput(); err != nil {
		return fmt.Errorf("cp: %v %s", err, out)
	}
	return nil
}

func (h *fakeHost) ran(prefix string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.cmds {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

type fakeFC struct {
	h        *fakeHost
	id, root string
	pid      int
	mu       sync.Mutex
	calls    []string
	paused   bool
	tap      string
	idents   []map[string]any
	env      map[string]any
	clockSet bool
	lns      []net.Listener
	apiConns int // open API connections, capped like Firecracker's server
	epoch    int // vsock resets so far: one per snapshot, like Firecracker
	sessions map[string]*fakeSession
	done     chan struct{}
	once     sync.Once
}

func (h *fakeHost) Start(v VM, root string, uid, gid int) (int, <-chan struct{}, error) {
	f := &fakeFC{h: h, id: v.ID, root: root, done: make(chan struct{})}
	h.mu.Lock()
	h.starts++
	if h.starts == h.failStart {
		h.mu.Unlock()
		return 0, nil, fmt.Errorf("fake: start %d fails", h.starts)
	}
	if h.nftAtStart == nil {
		h.nftAtStart = map[string]string{}
	}
	h.nftAtStart[v.ID] = h.nft
	h.mu.Unlock()
	listen := func() error {
		ln, err := net.Listen("unix", filepath.Join(root, apiSock))
		if err != nil {
			return err
		}
		f.mu.Lock()
		f.lns = append(f.lns, ln)
		f.mu.Unlock()
		go (&http.Server{Handler: f, ConnState: f.connState}).Serve(ln)
		return nil
	}
	if h.sockDelay > 0 {
		time.AfterFunc(h.sockDelay, func() { listen() })
	} else if err := listen(); err != nil {
		return 0, nil, err
	}
	h.mu.Lock()
	h.next++
	pid := h.next
	f.pid = pid
	h.vms[pid] = f
	h.mu.Unlock()
	return pid, f.done, nil
}

func (h *fakeHost) fc(pid int) *fakeFC {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.vms[pid]
}

func (h *fakeHost) Adopt(pid int, id string) <-chan struct{} {
	if f := h.fc(pid); f != nil {
		return f.done
	}
	c := make(chan struct{})
	close(c)
	return c
}

func (h *fakeHost) Alive(pid int, id string) bool {
	f := h.fc(pid)
	if f == nil || f.id != id {
		return false
	}
	select {
	case <-f.done:
		return false
	default:
		return true
	}
}

func (h *fakeHost) Kill(pid int) {
	if f := h.fc(pid); f != nil {
		f.once.Do(func() {
			f.mu.Lock()
			for _, l := range f.lns {
				l.Close()
			}
			f.mu.Unlock()
			close(f.done)
		})
	}
}

func (f *fakeFC) record(c string) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
}

func (f *fakeFC) called(c string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.calls {
		if x == c {
			return true
		}
	}
	return false
}

func fault(w http.ResponseWriter, msg string) {
	w.WriteHeader(400)
	fmt.Fprintf(w, `{"fault_message":%q}`, msg)
}

// fcMaxConns is Firecracker's API server cap (micro_http MAX_CONNECTIONS): past it
// a request gets 503 "Too many open connections" (boatbench 42818758694).
const fcMaxConns = 10

func (f *fakeFC) connState(_ net.Conn, st http.ConnState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch st {
	case http.StateNew:
		f.apiConns++
	case http.StateClosed, http.StateHijacked:
		f.apiConns--
	}
}

func (f *fakeFC) openConns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.apiConns
}

func (f *fakeFC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.openConns() > fcMaxConns {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{ "error": "Too many open connections" }`)
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.record(r.Method + " " + r.URL.Path)
	str := func(k string) string { s, _ := body[k].(string); return s }
	if r.URL.Path == "/snapshot/create" {
		f.record("snapshot " + str("snapshot_type"))
	}
	if r.URL.Path == "/snapshot/load" && body["track_dirty_pages"] == true {
		f.record("load tracking")
	}
	switch r.Method + " " + r.URL.Path {
	case "PUT /boot-source":
		if !exists(filepath.Join(f.root, str("kernel_image_path"))) {
			fault(w, "no kernel")
			return
		}
	case "PUT /drives/rootfs":
		if !exists(filepath.Join(f.root, str("path_on_host"))) {
			fault(w, "no disk")
			return
		}
		f.record("engine " + str("io_engine"))
		f.record("cache " + str("cache_type"))
		if f.h.noAsync && str("io_engine") == "Async" {
			fault(w, "no io_uring")
			return
		}
	case "PUT /network-interfaces/eth0":
		f.mu.Lock()
		f.tap = str("host_dev_name")
		f.mu.Unlock()
	case "PUT /actions":
		if err := f.startAgent(); err != nil {
			fault(w, err.Error())
			return
		}
	case "PATCH /vm":
		f.h.mu.Lock()
		refuse := str("state") == "Resumed" && f.h.failResume > 0
		if refuse {
			f.h.failResume--
		}
		f.h.mu.Unlock()
		if refuse {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{ "error": "refused" }`)
			return
		}
		f.mu.Lock()
		f.paused = str("state") == "Paused"
		f.mu.Unlock()
	case "PUT /snapshot/create":
		f.mu.Lock()
		paused := f.paused
		f.mu.Unlock()
		if !paused {
			fault(w, "not paused")
			return
		}
		f.h.mu.Lock()
		quota := f.h.failSnap > 0
		if quota {
			f.h.failSnap--
		}
		f.h.mu.Unlock()
		if quota {
			fault(w, "Create snapshot error: Cannot perform open on the snapshot backing file: Quota exceeded (os error 122)")
			return
		}
		f.mu.Lock()
		f.epoch++ // the guest drops its vsock connections; the host side stays open
		f.mu.Unlock()
		os.WriteFile(filepath.Join(f.root, str("snapshot_path")), []byte("vmstate"), 0o600)
		os.WriteFile(filepath.Join(f.root, str("mem_file_path")), []byte("mem"), 0o600)
	case "PUT /snapshot/load":
		mb, _ := body["mem_backend"].(map[string]any)
		if !exists(filepath.Join(f.root, str("snapshot_path"))) || !exists(filepath.Join(f.root, mb["backend_path"].(string))) {
			fault(w, "no snapshot files")
			return
		}
		ov, _ := body["network_overrides"].([]any)
		if len(ov) == 1 {
			f.mu.Lock()
			f.tap = ov[0].(map[string]any)["host_dev_name"].(string)
			f.mu.Unlock()
		}
		if err := f.startAgent(); err != nil {
			fault(w, err.Error())
			return
		}
	}
	w.WriteHeader(204)
}

func (f *fakeFC) startAgent() error {
	ln, err := net.Listen("unix", filepath.Join(f.root, vsockUDS))
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.lns = append(f.lns, ln)
	f.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.agentConn(c)
		}
	}()
	return nil
}

// disk is the guest filesystem: a JSON map path -> base64, NUL-padded by truncate.
func (f *fakeFC) disk() map[string]string {
	var b []byte
	if fh, err := os.Open(filepath.Join(f.root, diskFile)); err == nil {
		b, _ = io.ReadAll(io.LimitReader(fh, 16<<20)) // the rest is the sparse 1 GiB tail
		fh.Close()
	}
	m := map[string]string{}
	json.Unmarshal(bytes.TrimRight(b, "\x00"), &m)
	return m
}

func (f *fakeFC) agentConn(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	if l, _ := r.ReadString('\n'); l != "CONNECT 1024\n" {
		return
	}
	fmt.Fprint(c, "OK 1073741824\n")
	f.mu.Lock()
	epoch := f.epoch
	f.mu.Unlock()
	var wmu sync.Mutex
	send := func(v any) {
		f.mu.Lock()
		alive := f.epoch == epoch
		f.mu.Unlock()
		if !alive {
			return // reset in the guest: the line never reaches the host
		}
		b, _ := json.Marshal(v)
		wmu.Lock()
		c.Write(append(b, '\n'))
		wmu.Unlock()
	}
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var q map[string]any
		json.Unmarshal(line, &q)
		f.record("agent " + fmt.Sprint(q["op"]))
		f.h.mu.Lock()
		hang := f.h.hangOps[fmt.Sprint(q["op"])]
		f.h.mu.Unlock()
		if hang {
			io.Copy(io.Discard, r)
			return
		}
		switch q["op"] {
		case "health":
			f.h.mu.Lock()
			idle := f.h.idleS
			f.h.mu.Unlock()
			send(map[string]any{"ok": true, "idle_s": idle, "version": "1"})
		case "exec_attach":
			f.mu.Lock()
			fs := f.sessions[fmt.Sprint(q["session"])]
			f.mu.Unlock()
			if fs == nil && q["seen"].(float64) == 0 && q["exec"] != nil { // the exec was lost: start it here
				send(map[string]any{"attached": q["session"], "started": true})
				fs = f.startSlow(q["session"].(string), send)
				continue
			}
			if fs == nil {
				send(map[string]string{"error": "no such exec session"})
				continue
			}
			send(map[string]any{"attached": q["session"]})
			fs.attach(int(q["seen"].(float64)), send)
		case "exec":
			f.h.mu.Lock()
			lose := f.h.loseExec > 0
			if lose {
				f.h.loseExec--
			}
			f.h.mu.Unlock()
			if lose {
				continue
			}
			if sid, ok := q["session"].(string); ok && fmt.Sprint(q["argv"].([]any)[0]) == "slow" {
				if f.startSlow(sid, send) == nil {
					send(map[string]string{"error": "session exists: " + sid})
				}
				continue
			}
			var argv []string
			for _, a := range q["argv"].([]any) {
				argv = append(argv, a.(string))
			}
			f.h.mu.Lock()
			killed := f.h.execKilled[argv[0]]
			f.h.mu.Unlock()
			if killed {
				send(map[string]any{"exit": 137, "signal": "SIGKILL", "timed_out": true})
				continue
			}
			if argv[0] == "date" {
				f.mu.Lock()
				f.clockSet = true
				f.mu.Unlock()
			}
			send(map[string]any{"stream": "stdout", "data_b64": base64.StdEncoding.EncodeToString([]byte(strings.Join(argv, " ")))})
			send(map[string]any{"stream": "stderr", "data_b64": base64.StdEncoding.EncodeToString([]byte("err"))})
			send(map[string]any{"exit": 0})
		case "put_file":
			d := f.disk()
			p := q["path"].(string) + ".boat-part" // like the agent: renamed on last
			old, _ := base64.StdEncoding.DecodeString(d[p])
			chunk, _ := base64.StdEncoding.DecodeString(q["data_b64"].(string))
			if int(q["offset"].(float64)) == 0 {
				old = nil
			}
			d[p] = base64.StdEncoding.EncodeToString(append(old, chunk...))
			if q["last"] == true {
				d[q["path"].(string)] = d[p]
				delete(d, p)
			}
			b, _ := json.Marshal(d)
			os.WriteFile(filepath.Join(f.root, diskFile), b, 0o600)
			send(map[string]any{"ok": true, "size": len(old) + len(chunk)})
		case "get_file":
			d, ok := f.disk()[q["path"].(string)]
			if !ok {
				send(map[string]string{"error": "open: No such file or directory"})
				continue
			}
			send(map[string]any{"offset": 0, "data_b64": d})
			send(map[string]any{"eof": true})
		case "set_identity":
			f.mu.Lock()
			f.idents = append(f.idents, q)
			f.mu.Unlock()
			send(map[string]bool{"ok": true})
		case "set_env":
			f.mu.Lock()
			f.env = q["vars"].(map[string]any)
			f.mu.Unlock()
			send(map[string]bool{"ok": true})
		case "desktop":
			send(map[string]any{"ok": true, "port": 6080})
		case "cmd_start":
			send(map[string]any{"ok": true, "cmd_id": "c1"})
		case "cmd_status":
			send(map[string]any{"error": "No such command: 'nope'"})
		case "file_delete":
			d := f.disk()
			delete(d, q["path"].(string))
			b, _ := json.Marshal(d)
			os.WriteFile(filepath.Join(f.root, diskFile), b, 0o600)
			send(map[string]bool{"ok": true})
		case "file_list":
			send(map[string]any{"error": "open /nope: No such file or directory"})
		case "set_clock":
			if f.h.noSetClock {
				send(map[string]any{"error": "unknown op"})
			} else {
				f.mu.Lock()
				f.clockSet = true
				f.mu.Unlock()
				send(map[string]bool{"ok": true})
			}
		case "sync":
			send(map[string]bool{"ok": true})
		case "reseed":
			if len(q["entropy_b64"].(string)) != 88 {
				send(map[string]string{"error": "want 64 bytes"})
				continue
			}
			send(map[string]bool{"ok": true})
		case "shutdown":
			send(map[string]bool{"ok": true})
			go f.h.Kill(f.pid)
		default:
			send(map[string]string{"error": "unknown op"})
		}
	}
}

// startSlow registers exec session sid ("slow": begin, then end + exit 7 once
// slowGo is closed); nil if it exists.
func (f *fakeFC) startSlow(sid string, send func(any)) *fakeSession {
	f.mu.Lock()
	if f.sessions == nil {
		f.sessions = map[string]*fakeSession{}
	}
	if f.sessions[sid] != nil {
		f.mu.Unlock()
		return nil
	}
	fs := &fakeSession{target: send, done: make(chan struct{})}
	f.sessions[sid] = fs
	f.mu.Unlock()
	fs.emit(map[string]any{"stream": "stdout", "data_b64": b64("begin\n")})
	go func() {
		<-f.h.slowGo
		fs.emit(map[string]any{"stream": "stdout", "data_b64": b64("end\n")})
		fs.emit(map[string]any{"exit": 7})
	}()
	return fs
}

// fakeSession: the agent's exec session (boat-agent.py Session).
type fakeSession struct {
	mu     sync.Mutex
	lines  []any
	target func(any)
	done   chan struct{}
}

func (s *fakeSession) emit(v any) {
	s.mu.Lock()
	s.lines = append(s.lines, v)
	s.target(v)
	if m, ok := v.(map[string]any); ok && m["exit"] != nil {
		close(s.done)
	}
	s.mu.Unlock()
}

func (s *fakeSession) attach(seen int, send func(any)) {
	s.mu.Lock()
	s.target = send
	for _, v := range s.lines[seen:] {
		send(v)
	}
	s.mu.Unlock()
	<-s.done
}

// newTestManager: state under /tmp (unix socket paths must stay < 104 bytes on macOS).
func newTestManager(t *testing.T, mod func(*Config)) (*Manager, *fakeHost) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cfg := defaultConfig()
	cfg.StateDir, cfg.RunDir, cfg.Reflink, cfg.BootTimeoutS = dir, filepath.Join(dir, "run"), "auto", 5
	cfg.MinFreeGiB, cfg.MinFreePct, cfg.HostMemMiB, cfg.HostCPUs = 0, 0, 1<<20, 64
	cfg.Sizes = map[string]Size{"default": {2, 512, 1}, "large": {4, 1024, 1}}
	if mod != nil {
		mod(&cfg)
	}
	img := filepath.Join(dir, "images", cfg.DefaultImage)
	os.MkdirAll(img, 0o755)
	os.WriteFile(filepath.Join(img, kernFile), []byte("kernel"), 0o644)
	os.WriteFile(filepath.Join(img, diskFile), []byte(`{}`), 0o644)
	h := newFakeHost()
	m, err := newManager(cfg, h.run, h)
	if err != nil {
		t.Fatal(err)
	}
	return m, h
}

func (h *fakeHost) fcOf(t *testing.T, m *Manager, id string) *fakeFC {
	t.Helper()
	v, err := m.get(id)
	if err != nil || v.PID == 0 {
		t.Fatalf("vm %s has no process: %v", id, err)
	}
	return h.fc(v.PID)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
