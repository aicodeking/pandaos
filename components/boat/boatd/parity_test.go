package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	if ok, _ := m.verifyManifest(mf, m.snapDir(s.ID), s.Team); ok {
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
	if guestTimeout(&agentErr{msg: "resize2fs exited 1"}) || !guestTimeout(fmt.Errorf("x: %w", os.ErrDeadlineExceeded)) {
		t.Fatal("guestTimeout")
	}
}

func TestSha256DiskSparseAndDenseEquivalence(t *testing.T) {
	dir := t.TempDir()

	check := func(name string, build func(f *os.File)) {
		t.Helper()
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		build(f)
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		want := hex.EncodeToString(sum[:])
		gotFile, err := sha256File(p)
		if err != nil || gotFile != want {
			t.Fatalf("%s: sha256File=%q want=%q err=%v", name, gotFile, want, err)
		}
		gotDisk, gotSize, err := sha256Disk(p)
		if err != nil {
			t.Fatalf("%s: sha256Disk err=%v", name, err)
		}
		if gotDisk != want || gotSize != int64(len(raw)) {
			t.Fatalf("%s: sha256Disk=(%q, %d), want (%q, %d)", name, gotDisk, gotSize, want, len(raw))
		}
	}

	check("empty", func(*os.File) {})
	check("dense", func(f *os.File) {
		buf := bytes.Repeat([]byte("boat-dense-block-"), 20000)
		if _, err := f.Write(buf); err != nil {
			t.Fatal(err)
		}
	})
	check("all-hole", func(f *os.File) {
		if err := f.Truncate(2<<20 + 137); err != nil {
			t.Fatal(err)
		}
	})
	check("multi-hole-sparse", func(f *os.File) {
		// Leading hole, first extent across chunk boundaries, middle hole, second extent, trailing hole.
		if _, err := f.WriteAt(bytes.Repeat([]byte{0x5a, 0xa5, 0x11}, 90000), 512<<10); err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte("second-extent-payload"), 2<<20+4096); err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(4<<20 + 777); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSnapshotExportImportDiskIntegrityAndQuotaClamp(t *testing.T) {
	t.Setenv("COPYFILE_DISABLE", "1")
	m, _ := newTestManager(t, func(c *Config) {
		c.Sizes["big"] = Size{VCPU: 2, MemMiB: 512, DiskGiB: 2}
		c.Teams = map[string]TeamPolicy{
			"qteam": {MaxDiskGiB: 3},
		}
	})
	testExporter(m)
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	ctx := context.Background()

	v, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.CreateSnapshot(ctx, v.ID, "signed")
	if err != nil || !s.HasMem {
		t.Fatalf("CreateSnapshot: %+v %v", s, err)
	}
	// Keep test export archive compact while exercising real disk bytes.
	if err := os.WriteFile(filepath.Join(m.snapDir(s.ID), diskFile), []byte("valid-ext4-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	var arch []byte
	if c := call(t, srv, "GET", "/v1/vms/"+v.ID+"/snapshots/"+s.ID+"/download", nil, &arch); c != 200 || len(arch) == 0 {
		t.Fatalf("download: %d %d", c, len(arch))
	}

	// (a) Tampering with rootfs.ext4 inside a signed archive causes verifyManifest to return false
	// and downgrades ImportSnapshot to untrusted disk-only (dropping vmstate and mem).
	unpackDir := t.TempDir()
	tr := tar.NewReader(bytes.NewReader(arch))
	var memberOrder []string
	members := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		memberOrder = append(memberOrder, hdr.Name)
		members[hdr.Name] = b
		if err := os.WriteFile(filepath.Join(unpackDir, hdr.Name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var exportedMF exportManifest
	if err := json.Unmarshal(members[manifestFile], &exportedMF); err != nil {
		t.Fatal(err)
	}
	if exportedMF.Disk == "" || exportedMF.DiskSize == 0 {
		t.Fatalf("expected non-empty disk_sha256 and disk_size in manifest, got %+v", exportedMF)
	}
	if ok, err := m.verifyManifest(exportedMF, unpackDir, "t"); err != nil || !ok {
		t.Fatalf("untampered manifest failed verifyManifest: ok=%v err=%v", ok, err)
	}

	tamperedDisk := append([]byte(nil), members[diskFile]...)
	if len(tamperedDisk) == 0 {
		tamperedDisk = []byte("tampered")
	} else {
		tamperedDisk[0] ^= 0xff
	}
	if err := os.WriteFile(filepath.Join(unpackDir, diskFile), tamperedDisk, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.verifyManifest(exportedMF, unpackDir, "t"); err != nil || ok {
		t.Fatalf("tampered rootfs.ext4 accepted by verifyManifest: ok=%v err=%v", ok, err)
	}

	tamperedTar := tarOf(t, func(tw *tar.Writer) {
		for _, name := range memberOrder {
			data := members[name]
			if name == diskFile {
				data = tamperedDisk
			}
			tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg})
			tw.Write(data)
		}
	})
	downgraded, err := m.ImportSnapshot(ctx, bytes.NewReader(tamperedTar.Bytes()), "t", "tampered-disk", "", "")
	if err != nil {
		t.Fatalf("ImportSnapshot on tampered disk failed instead of downgrading: %v", err)
	}
	if downgraded.HasMem {
		t.Fatalf("tampered disk archive kept trusted memory: %+v", downgraded)
	}
	for _, f := range []string{vmstate, memFile} {
		if exists(filepath.Join(m.snapDir(downgraded.ID), f)) {
			t.Fatalf("tampered archive retained %s", f)
		}
	}

	// (b) Signed archive claiming disk_gib: 1 with a 2 GiB sparse rootfs.ext4 clamps DiskGiB >= 2
	// even when trusted == true, and charges the full 2 GiB against team quota.
	craftDir := t.TempDir()
	for _, name := range []string{kernFile, vmstate, memFile} {
		if err := os.WriteFile(filepath.Join(craftDir, name), members[name], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sparseDisk := filepath.Join(craftDir, diskFile)
	df, err := os.OpenFile(sparseDisk, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := df.Write([]byte("ext4-header")); err != nil {
		t.Fatal(err)
	}
	if err := df.Truncate(2 << 30); err != nil {
		t.Fatal(err)
	}
	if err := df.Close(); err != nil {
		t.Fatal(err)
	}

	craftedMF := exportedMF
	craftedMF.Snapshot.Team = "qteam"
	craftedMF.Snapshot.DiskGiB = 1 // deliberately under-declared
	craftedMF.Disk, craftedMF.DiskSize, err = sha256Disk(sparseDisk)
	if err != nil {
		t.Fatal(err)
	}
	key, err := m.exportKey()
	if err != nil {
		t.Fatal(err)
	}
	craftedMF.Sig, err = manifestSig(key, craftedMF)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := m.verifyManifest(craftedMF, craftDir, "qteam"); err != nil || !ok {
		t.Fatalf("crafted signed manifest failed verifyManifest: ok=%v err=%v", ok, err)
	}
	if err := writeJSONAtomic(filepath.Join(craftDir, manifestFile), craftedMF, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(craftDir, manifestFile))
	if err != nil {
		t.Fatal(err)
	}

	// Stream a standard USTAR tar archive carrying the 2 GiB logical rootfs.ext4
	// (zero heap bloat; writeSparse skips zero blocks and truncates to 2 GiB).
	const logical2GiB = int64(2) << 30
	headerPrefix := []byte("ext4-header")
	streamCraftedArchive := func() io.Reader {
		pr, pw := io.Pipe()
		go func() {
			tw := tar.NewWriter(pw)
			tarFile(tw, manifestFile, string(manifestBytes))
			if err := tw.WriteHeader(&tar.Header{
				Name:     diskFile,
				Mode:     0o600,
				Size:     logical2GiB,
				Typeflag: tar.TypeReg,
			}); err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := tw.Write(headerPrefix); err != nil {
				pw.CloseWithError(err)
				return
			}
			if err := writeZeros(tw, logical2GiB-int64(len(headerPrefix))); err != nil {
				pw.CloseWithError(err)
				return
			}
			for _, name := range []string{kernFile, vmstate, memFile} {
				tarFile(tw, name, string(members[name]))
			}
			pw.CloseWithError(tw.Close())
		}()
		return pr
	}

	clamped, err := m.ImportSnapshot(ctx, streamCraftedArchive(), "qteam", "clamped-1", "", "")
	if err != nil {
		t.Fatalf("ImportSnapshot(clamped-1): %v", err)
	}
	if !clamped.HasMem {
		t.Fatalf("expected trusted import with HasMem=true, got %+v", clamped)
	}
	if clamped.DiskGiB < 2 {
		t.Fatalf("trusted import under-accounted DiskGiB=%d for 2 GiB sparse disk", clamped.DiskGiB)
	}
	// qteam has MaxDiskGiB=3: a second 2 GiB import must fail quota (2+2 > 3), proving quota accounting uses clamped DiskGiB.
	if _, err := m.ImportSnapshot(ctx, streamCraftedArchive(), "qteam", "clamped-2", "", ""); err == nil {
		t.Fatal("second 2 GiB import into MaxDiskGiB=3 team succeeded; quota under-accounted")
	}
}

func TestVerifyManifestTeamImageSizeAndNoEnvPropagation(t *testing.T) {
	t.Setenv("COPYFILE_DISABLE", "1")
	m, _ := newTestManager(t, nil)
	testExporter(m)
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	ctx := context.Background()

	// Create a sticky no_env VM and snapshot it.
	v, err := m.Create(ctx, CreateReq{Team: "team-a", Labels: map[string]string{"boat.no_env": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.CreateSnapshot(ctx, v.ID, "noenv-snap")
	if err != nil || !s.HasMem || !s.NoEnv || s.Labels["boat.no_env"] != "1" {
		t.Fatalf("expected snapshot to carry no_env: %+v %v", s, err)
	}
	if err := os.WriteFile(filepath.Join(m.snapDir(s.ID), diskFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var arch []byte
	if c := call(t, srv, "GET", "/v1/vms/"+v.ID+"/snapshots/"+s.ID+"/download", nil, &arch); c != 200 || len(arch) == 0 {
		t.Fatalf("download: %d %d", c, len(arch))
	}

	// 1. Same-team import is trusted (keeps memory) AND preserves no_env / boat.no_env.
	sameTeam, err := m.ImportSnapshot(ctx, bytes.NewReader(arch), "team-a", "same-team", "", "")
	if err != nil || !sameTeam.HasMem || !sameTeam.NoEnv || sameTeam.Labels["boat.no_env"] != "1" {
		t.Fatalf("same-team no_env import: %+v %v", sameTeam, err)
	}
	// Creating a VM from that imported snapshot propagates boat.no_env=1 automatically.
	child, err := m.Create(ctx, CreateReq{Team: "team-a", Snapshot: sameTeam.ID})
	if err != nil || child.Labels["boat.no_env"] != "1" {
		t.Fatalf("create from imported no_env snapshot did not propagate boat.no_env: %+v %v", child, err)
	}

	// 2. Cross-team import of a signed archive MUST downgrade to untrusted (strip memory/vmstate),
	//    while STILL preserving sticky no_env.
	crossTeam, err := m.ImportSnapshot(ctx, bytes.NewReader(arch), "team-b", "cross-team", "", "")
	if err != nil {
		t.Fatalf("cross-team import: %v", err)
	}
	if crossTeam.HasMem {
		t.Fatalf("cross-team import trusted another team's signed manifest: %+v", crossTeam)
	}
	for _, f := range []string{vmstate, memFile} {
		if exists(filepath.Join(m.snapDir(crossTeam.ID), f)) {
			t.Fatalf("cross-team import retained %s", f)
		}
	}
	if !crossTeam.NoEnv || crossTeam.Labels["boat.no_env"] != "1" {
		t.Fatalf("cross-team import dropped sticky no_env: %+v", crossTeam)
	}

	// 3. Signed archive whose size or image is no longer valid on this host MUST downgrade to untrusted.
	// Export a fresh archive from a custom size, then remove that size from host config before importing.
	m.cfg.Sizes["ephemeral"] = Size{VCPU: 2, MemMiB: 512, DiskGiB: 1}
	v2, err := m.Create(ctx, CreateReq{Team: "team-a", Size: "ephemeral"})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := m.CreateSnapshot(ctx, v2.ID, "eph-snap")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.snapDir(s2.ID), diskFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var arch2 []byte
	if c := call(t, srv, "GET", "/v1/vms/"+v2.ID+"/snapshots/"+s2.ID+"/download", nil, &arch2); c != 200 {
		t.Fatalf("download arch2: %d", c)
	}
	delete(m.cfg.Sizes, "ephemeral")
	badSizeImport, err := m.ImportSnapshot(ctx, bytes.NewReader(arch2), "team-a", "removed-size", "", "default")
	if err != nil {
		t.Fatalf("import with removed manifest size: %v", err)
	}
	if badSizeImport.HasMem || badSizeImport.Size != "default" {
		t.Fatalf("removed size manifest must downgrade to untrusted default size: %+v", badSizeImport)
	}

	// Invalid/uninstalled image in manifest must also be rejected by verifyManifest.
	mfBadImg := exportManifest{Snapshot: s}
	mfBadImg.Snapshot.Image = "not-installed-image"
	key, _ := m.exportKey()
	mfBadImg.Sig, _ = manifestSig(key, mfBadImg)
	if ok, err := m.verifyManifest(mfBadImg, m.snapDir(s.ID), "team-a"); err != nil || ok {
		t.Fatalf("verifyManifest accepted uninstalled image: ok=%v err=%v", ok, err)
	}
}
