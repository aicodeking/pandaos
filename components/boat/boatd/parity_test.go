package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentAllowlist(t *testing.T) {
	m, h := newTestManager(t, nil)
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	v, _ := m.Create(context.Background(), CreateReq{})
	var out map[string]any
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/agent", map[string]any{"op": "cmd_start", "argv": []string{"x"}}, &out); c != 200 || out["cmd_id"] != "c1" {
		t.Fatalf("%d %v", c, out)
	}
	for _, op := range []string{"exec", "set_identity", "reseed", "shutdown", "put_file", ""} {
		if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/agent", map[string]any{"op": op}, &out); c != 400 {
			t.Errorf("op %q: %d", op, c)
		}
	}
	if h.fcOf(t, m, v.ID).called("agent shutdown") {
		t.Fatal("a refused op reached the guest")
	}
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/agent", map[string]any{"op": "file_list", "path": "/nope"}, &out); c != 404 {
		t.Fatalf("missing path: %d", c)
	}
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/agent", map[string]any{"op": "cmd_status", "cmd_id": "nope"}, &out); c != 404 {
		t.Fatalf("unknown cmd_id: %d", c)
	}
	m.Stop(context.Background(), v.ID, "")
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/agent", map[string]any{"op": "cmd_list"}, &out); c != 409 {
		t.Fatalf("stopped VM: %d", c)
	}
}

func TestResize(t *testing.T) {
	m, h := newTestManager(t, nil) // sizes: default 2/512/1, large 4/1024/1
	ctx := context.Background()
	v, _ := m.Create(ctx, CreateReq{})
	disk := filepath.Join(m.root(v.ID), diskFile)
	var ae *apiErr
	if _, err := m.Resize(ctx, v.ID, ResizeReq{Size: "large"}); !errors.As(err, &ae) || ae.code != 409 {
		t.Fatalf("shape change on a running VM: %v", err)
	}
	if _, err := m.Resize(ctx, v.ID, ResizeReq{Size: "nope"}); !errors.As(err, &ae) || ae.code != 400 {
		t.Fatalf("unknown size: %v", err)
	}
	got, err := m.Resize(ctx, v.ID, ResizeReq{DiskGiB: 2}) // disk grow while running
	if err != nil || got.DiskGiB != 2 || got.VCPU != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	if st, _ := os.Stat(disk); st.Size() != 2<<30 {
		t.Fatalf("disk %d", st.Size())
	}
	fc := h.fcOf(t, m, v.ID)
	if !fc.called("PATCH /drives/rootfs") {
		t.Fatal("guest not told about the new disk")
	}
	if _, err := m.Resize(ctx, v.ID, ResizeReq{DiskGiB: 1}); !errors.As(err, &ae) || ae.code != 400 {
		t.Fatalf("shrink: %v", err)
	}
	// Stopped with a snapshot: shape change needs an explicit discard.
	m.Stop(ctx, v.ID, "")
	if _, err := m.Resize(ctx, v.ID, ResizeReq{Size: "large"}); !errors.As(err, &ae) || ae.code != 409 {
		t.Fatalf("silent memory loss: %v", err)
	}
	// Disk grow on a stopped VM keeps the snapshot and tells the guest after the restore.
	got, err = m.Resize(ctx, v.ID, ResizeReq{DiskGiB: 3})
	if err != nil || !got.HasSnapshot || !got.DiskGrown {
		t.Fatalf("%+v %v", got, err)
	}
	started, err := m.Start(ctx, v.ID, nil)
	if err != nil || started.DiskGrown || !h.fcOf(t, m, v.ID).called("PATCH /drives/rootfs") {
		t.Fatalf("restore did not apply the grown disk: %+v %v", started, err)
	}
	m.Stop(ctx, v.ID, "")
	got, err = m.Resize(ctx, v.ID, ResizeReq{Size: "large", DiscardMemory: true})
	if err != nil || got.VCPU != 4 || got.MemMiB != 1024 || got.Size != "large" || got.HasSnapshot || got.DiskGiB != 3 {
		t.Fatalf("%+v %v", got, err) // the smaller size keeps the larger disk
	}
	if exists(filepath.Join(m.root(v.ID), vmstate)) {
		t.Fatal("stale snapshot kept")
	}
	started, err = m.Start(ctx, v.ID, nil)
	if err != nil || !h.fcOf(t, m, v.ID).called("PUT /actions") {
		t.Fatalf("not a cold boot: %+v %v", started, err)
	}
}

func testExporter(m *Manager) {
	m.exportArgv = func(dir string, files []string) []string {
		return append([]string{"tar", "-cf", "-", "-C", dir}, files...)
	}
	m.unpackArgv = []string{"cat"}
}

func tarOf(t *testing.T, add func(*tar.Writer)) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	add(tw)
	tw.Close()
	return &b
}

func tarFile(tw *tar.Writer, name, data string) {
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg})
	tw.Write([]byte(data))
}

func TestSnapshotExportImport(t *testing.T) {
	t.Setenv("COPYFILE_DISABLE", "1") // macOS tar: no ._ members
	m, _ := newTestManager(t, nil)
	testExporter(m)
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	ctx := context.Background()
	v, _ := m.Create(ctx, CreateReq{Team: "t"})
	s, err := m.CreateSnapshot(ctx, v.ID, "s1")
	if err != nil || !s.HasMem {
		t.Fatalf("%+v %v", s, err)
	}
	var arch []byte
	if c := call(t, srv, "GET", "/v1/vms/"+v.ID+"/snapshots/"+s.ID+"/download", nil, &arch); c != 200 || len(arch) == 0 {
		t.Fatalf("%d %d", c, len(arch))
	}
	if c := call(t, srv, "GET", "/v1/vms/other/snapshots/"+s.ID+"/download", nil, &[]byte{}); c != 404 {
		t.Fatalf("wrong vm: %d", c)
	}

	// Signed by this host: memory survives, and a VM restores from it.
	in, err := m.ImportSnapshot(ctx, bytes.NewReader(arch), "t", "copy", "", "")
	if err != nil || !in.HasMem || in.VMID != "" || in.DiskGiB != s.DiskGiB {
		t.Fatalf("signed import: %+v %v", in, err)
	}
	c, err := m.Create(ctx, CreateReq{Team: "t", Snapshot: in.ID})
	if err != nil || !c.AgentReady {
		t.Fatalf("create from import: %+v %v", c, err)
	}

	// Another host's key (or none): disk only, local kernel, no vmstate.
	mustRemove(t, filepath.Join(m.cfg.StateDir, "export.key"))
	un, err := m.ImportSnapshot(ctx, bytes.NewReader(arch), "t", "foreign", "", "")
	if err != nil || un.HasMem {
		t.Fatalf("foreign key: %+v %v", un, err)
	}
	for _, f := range []string{vmstate, memFile} {
		if exists(filepath.Join(m.snapDir(un.ID), f)) {
			t.Fatalf("untrusted %s kept", f)
		}
	}
	if k, _ := os.ReadFile(filepath.Join(m.snapDir(un.ID), kernFile)); string(k) != "kernel" {
		t.Fatalf("kernel not from the local image: %q", k)
	}

	// Tampered memory under a valid signature is refused too.
	key, _ := m.exportKey()
	mf := exportManifest{Snapshot: s, VMState: "aa", Mem: "bb", Kernel: "cc"}
	mf.Sig, _ = manifestSig(key, mf)
	if ok, _ := m.verifyManifest(mf, m.snapDir(s.ID)); ok {
		t.Fatal("hash mismatch accepted")
	}
}

func TestImportRejects(t *testing.T) {
	m, _ := newTestManager(t, nil)
	testExporter(m)
	ctx := context.Background()
	big := strings.Repeat("x", 2<<20)
	for name, tc := range map[string]func(*tar.Writer){
		"traversal":      func(w *tar.Writer) { tarFile(w, "../../etc/passwd", "x") },
		"absolute":       func(w *tar.Writer) { tarFile(w, "/etc/passwd", "x") },
		"unknown member": func(w *tar.Writer) { tarFile(w, "evil.sh", "x") },
		"symlink": func(w *tar.Writer) {
			w.WriteHeader(&tar.Header{Name: diskFile, Typeflag: tar.TypeSymlink, Linkname: "/etc/shadow"})
		},
		"device": func(w *tar.Writer) { w.WriteHeader(&tar.Header{Name: diskFile, Typeflag: tar.TypeChar}) },
		"hardlink": func(w *tar.Writer) {
			w.WriteHeader(&tar.Header{Name: memFile, Typeflag: tar.TypeLink, Linkname: "/etc/shadow"})
		},
		"duplicate": func(w *tar.Writer) { tarFile(w, diskFile, "x"); tarFile(w, diskFile, "y") },
		"oversized": func(w *tar.Writer) { tarFile(w, manifestFile, big) },
		"no disk":   func(w *tar.Writer) { tarFile(w, kernFile, "x") },
	} {
		before, _ := filepath.Glob(filepath.Join(m.cfg.StateDir, "snapshots", "*"))
		var ae *apiErr
		if _, err := m.ImportSnapshot(ctx, tarOf(t, tc), "t", "", "", ""); !errors.As(err, &ae) || ae.code != 400 {
			t.Errorf("%s: %v", name, err)
		}
		after, _ := filepath.Glob(filepath.Join(m.cfg.StateDir, "snapshots", "*"))
		if len(after) != len(before) {
			t.Errorf("%s: left files behind", name)
		}
	}
	if len(m.snapshotsOfTeam("")) != 0 {
		t.Fatal("a rejected import registered a snapshot")
	}
	if _, err := m.ImportSnapshot(ctx, strings.NewReader("garbage"), "t", "", "", ""); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := m.ImportSnapshot(ctx, tarOf(t, func(w *tar.Writer) { tarFile(w, diskFile, "x") }), "../t", "", "", ""); err == nil {
		t.Fatal("bad team accepted")
	}
	// A plain disk upload works and is sized to hold it.
	s, err := m.ImportSnapshot(ctx, tarOf(t, func(w *tar.Writer) { tarFile(w, "./"+diskFile, "{}") }), "t", "up", "", "")
	if err != nil || s.HasMem || s.DiskGiB != 1 {
		t.Fatalf("%+v %v", s, err)
	}
	v, err := m.Create(ctx, CreateReq{Team: "t", Snapshot: s.ID})
	if err != nil || v.State != Running {
		t.Fatalf("%+v %v", v, err)
	}
}

func mustRemove(t *testing.T, p string) {
	t.Helper()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
}

// A guest resize2fs that times out: 504 resize_timeout (not a 502), the host side
// stays grown and recorded, and the same disk_gib again retries the guest grow.
func TestResizeGuestTimeout(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	v, _ := m.Create(ctx, CreateReq{})
	h.mu.Lock()
	h.execKilled = map[string]bool{"resize2fs": true}
	h.mu.Unlock()
	var ae *apiErr
	if _, err := m.Resize(ctx, v.ID, ResizeReq{DiskGiB: 2}); !errors.As(err, &ae) || ae.code != 504 || ae.kind != codeResizeTimeout {
		t.Fatalf("want 504 %s, got %v", codeResizeTimeout, err)
	}
	if got, _ := m.get(v.ID); got.DiskGiB != 2 || got.State != Running {
		t.Fatalf("after the timeout: %+v", got)
	}
	if st, _ := os.Stat(filepath.Join(m.root(v.ID), diskFile)); st.Size() != 2<<30 {
		t.Fatalf("disk %d", st.Size())
	}
	h.mu.Lock()
	h.execKilled = nil
	h.mu.Unlock()
	got, err := m.Resize(ctx, v.ID, ResizeReq{DiskGiB: 2})
	if err != nil || got.DiskGiB != 2 {
		t.Fatalf("retry: %+v %v", got, err)
	}
	// Other failures keep their own status.
	if guestTimeout(&agentErr{"resize2fs exited 1"}) || !guestTimeout(fmt.Errorf("x: %w", os.ErrDeadlineExceeded)) {
		t.Fatal("guestTimeout")
	}
}
