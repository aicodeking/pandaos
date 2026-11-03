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
	done     chan struct{}
	once     sync.Once
}

func (h *fakeHost) Start(v VM, root string, uid, gid int) (int, <-chan struct{}, error) {
	f := &fakeFC{h: h, id: v.ID, root: root, done: make(chan struct{})}
	h.mu.Lock()
	if h.nftAtStart == nil {
		h.nftAtStart = map[string]string{}
	}
	h.nftAtStart[v.ID] = h.nft
	h.mu.Unlock()
	ln, err := net.Listen("unix", filepath.Join(root, apiSock))
	if err != nil {
		return 0, nil, err
	}
	f.lns = append(f.lns, ln)
	go http.Serve(ln, f)
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

func (f *fakeFC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	send := func(v any) { b, _ := json.Marshal(v); c.Write(append(b, '\n')) }
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
		case "exec":
			var argv []string
			for _, a := range q["argv"].([]any) {
				argv = append(argv, a.(string))
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
			p := q["path"].(string)
			old, _ := base64.StdEncoding.DecodeString(d[p])
			chunk, _ := base64.StdEncoding.DecodeString(q["data_b64"].(string))
			if int(q["offset"].(float64)) == 0 {
				old = nil
			}
			d[p] = base64.StdEncoding.EncodeToString(append(old, chunk...))
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
