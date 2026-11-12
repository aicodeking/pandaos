package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// agentOps are the guest ops POST /v1/vms/{id}/agent may pass through. Nothing
// else: exec, put_file, set_identity, reseed... have their own endpoints or are
// boatd's alone. See ../PROTOCOL.md.
var agentOps = map[string]bool{
	"cmd_start": true, "cmd_list": true, "cmd_status": true, "cmd_logs": true, "cmd_signal": true,
	"file_list": true, "file_delete": true, "file_move": true, "file_search": true,
}

func (m *Manager) agentHandler(r *http.Request) (any, error) {
	var req map[string]any
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	op, _ := req["op"].(string)
	if !agentOps[op] {
		return nil, errf(400, "op %q is not allowed", op)
	}
	return m.agentPass(r, req)
}

// ---- resize ----

type ResizeReq struct {
	Size          string `json:"size"`
	DiskGiB       int    `json:"disk_gib"`
	DiscardMemory bool   `json:"discard_memory"`
}

// Resize applies a new size. vCPU and RAM change only on a stopped VM, and only
// through a cold boot: a memory snapshot cannot change shape, so a stop snapshot
// is dropped, which needs discard_memory:true. The disk grows at any time and
// never shrinks: a smaller size keeps the larger disk.
func (m *Manager) Resize(ctx context.Context, id string, r ResizeReq) (VM, error) {
	unlock := m.lock(id)
	defer unlock()
	v, err := m.get(id)
	if err != nil {
		return v, err
	}
	s := Size{v.VCPU, v.MemMiB, v.DiskGiB}
	if r.Size != "" {
		var ok bool
		if s, ok = m.cfg.Sizes[r.Size]; !ok {
			return v, errf(400, "unknown size %q", r.Size)
		}
	}
	if r.DiskGiB < 0 || (r.DiskGiB > 0 && r.DiskGiB < v.DiskGiB) {
		return v, errf(400, "disk_gib must not shrink the disk (%d GiB)", v.DiskGiB)
	}
	disk := max(v.DiskGiB, s.DiskGiB, r.DiskGiB)
	shape := s.VCPU != v.VCPU || s.MemMiB != v.MemMiB
	switch {
	case v.State != Running && v.State != Stopped:
		return v, errf(http.StatusConflict, "vm is %s", v.State)
	case shape && v.State != Stopped:
		return v, errf(http.StatusConflict, "stop the VM to change vCPU or RAM; only the disk can grow while it runs")
	case shape && v.HasSnapshot && !r.DiscardMemory:
		return v, errf(http.StatusConflict, "the stop snapshot cannot change shape: pass discard_memory:true to cold-boot next start")
	}
	res, err := m.reserve(v.Team, 0, int64(disk-v.DiskGiB)<<30+int64(s.MemMiB-v.MemMiB)<<20)
	if err != nil {
		return v, err
	}
	defer res.release()
	root := m.root(id)
	if disk > v.DiskGiB {
		if err := truncateJail(filepath.Join(root, diskFile), int64(disk)<<30); err != nil {
			return v, err
		}
	}
	if shape && v.HasSnapshot {
		os.Remove(filepath.Join(root, vmstate))
		os.Remove(filepath.Join(root, memFile))
	}
	nv, err := m.update(id, "", func(x *VM) {
		if r.Size != "" {
			x.Size = r.Size
		}
		x.VCPU, x.MemMiB, x.DiskGiB = s.VCPU, s.MemMiB, disk
		if shape {
			x.HasSnapshot, x.SnapSynced = false, false
		}
		// A restored guest still believes the old size (a cold boot sees the new one).
		x.DiskGrown = (x.DiskGrown || disk > v.DiskGiB) && x.HasSnapshot
	})
	if err != nil {
		return nv, err
	}
	m.ev.publish(Event{Type: "vm.resized", VMID: id, Team: v.Team, Size: nv.Size, State: nv.State})
	if disk > v.DiskGiB && v.State == Running {
		if err := m.growGuestDisk(ctx, nv); err != nil {
			return nv, err
		}
	}
	return nv, nil
}

// growGuestDisk makes a running guest see the larger disk and grows its filesystem.
func (m *Manager) growGuestDisk(ctx context.Context, v VM) error {
	root := m.root(v.ID)
	fc := newFC(filepath.Join(root, apiSock))
	if err := fc.call(ctx, "PATCH", "/drives/rootfs", map[string]string{"drive_id": "rootfs", "path_on_host": diskFile}); err != nil {
		return err
	}
	return execRoot(ctx, filepath.Join(root, vsockUDS), []string{"resize2fs", "/dev/vda"})
}

// execRoot runs argv as root in the guest and requires exit 0.
func execRoot(ctx context.Context, sock string, argv []string) error {
	c, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	a, err := dialAgent(c, sock)
	if err != nil {
		return err
	}
	defer a.Close()
	if err := a.send(map[string]any{"op": "exec", "argv": argv, "user": "root", "timeout_s": 50}); err != nil {
		return err
	}
	for {
		_, r, err := a.recv()
		if err != nil {
			return err
		}
		if r.Error != "" {
			return &agentErr{r.Error}
		}
		if r.Exit != nil {
			if *r.Exit != 0 {
				return &agentErr{fmt.Sprintf("%s exited %d", argv[0], *r.Exit)}
			}
			return nil
		}
	}
}

// ---- snapshot export / import ----

// Firecracker does not validate vmstate ("treat snapshot files as trusted"), so
// memory crosses an export/import only under an HMAC from this host's key. Any
// other archive is a tenant's upload: its disk is kept (only the guest kernel
// ever parses it), its vmstate, memory and kernel are dropped, and the snapshot
// cold-boots on the local image's kernel.
type exportManifest struct {
	Snapshot Snapshot `json:"snapshot"`
	VMState  string   `json:"vmstate_sha256,omitempty"`
	Mem      string   `json:"mem_sha256,omitempty"`
	Kernel   string   `json:"kernel_sha256,omitempty"`
	Sig      string   `json:"sig,omitempty"`
}

const manifestFile = "export.json"

// exportKey comes from the sealed systemd credential on a NixOS host, so a backup of
// stateDir holds no usable key. The plain file is for a boatd outside that module.
func (m *Manager) exportKey() ([]byte, error) {
	if d := os.Getenv("CREDENTIALS_DIRECTORY"); d != "" {
		if b, err := os.ReadFile(filepath.Join(d, "boat-export-key")); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	p := filepath.Join(m.cfg.StateDir, "export.key")
	if b, err := os.ReadFile(p); err == nil && len(b) == 32 {
		return b, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, os.WriteFile(p, b, 0o600)
}

func manifestSig(key []byte, mf exportManifest) (string, error) {
	mf.Sig = ""
	b, err := json.Marshal(mf)
	if err != nil {
		return "", err
	}
	h := hmac.New(sha256.New, key)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// exportHandler streams a snapshot as tar.zst (GNU sparse, so a 51 GiB disk with
// little data stays cheap), its manifest first.
func (m *Manager) exportHandler(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	s, ok := m.snaps[r.PathValue("sid")]
	m.mu.Unlock()
	if !ok || s.VMID != r.PathValue("id") {
		writeErr(w, errf(404, "snapshot not found"))
		return
	}
	dir := m.snapDir(s.ID)
	files := []string{manifestFile, diskFile, kernFile}
	mf := exportManifest{Snapshot: s}
	var err error
	if mf.Kernel, err = sha256File(filepath.Join(dir, kernFile)); err == nil && s.HasMem {
		files = append(files, vmstate, memFile)
		if mf.VMState, err = sha256File(filepath.Join(dir, vmstate)); err == nil {
			mf.Mem, err = sha256File(filepath.Join(dir, memFile))
		}
	}
	var key []byte
	if err == nil {
		key, err = m.exportKey()
	}
	if err == nil {
		mf.Sig, err = manifestSig(key, mf)
	}
	if err == nil {
		err = writeJSONAtomic(filepath.Join(dir, manifestFile), mf, 0o600)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	argv := m.exportArgv(dir, files)
	cmd := exec.CommandContext(r.Context(), argv[0], argv[1:]...)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &stderr
	w.Header().Set("Content-Type", "application/zstd")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", s.ID+".tar.zst"))
	if err := cmd.Run(); err != nil {
		log.Printf("%s: export: %v: %s", s.ID, err, bytes.TrimSpace(stderr.Bytes()))
		panic(http.ErrAbortHandler) // never let a truncated stream look complete
	}
}

func tarZstdArgv(dir string, files []string) []string {
	return append([]string{"tar", "--create", "--sparse", "--zstd", "--directory", dir, "--file", "-"}, files...)
}

// importCaps: the most each archive member may hold. A disk may have been grown
// past every size, so its cap is what the team may commit at all.
func (m *Manager) importCaps(team string) map[string]int64 {
	var disk, mem int64
	for _, s := range m.cfg.Sizes {
		disk, mem = max(disk, int64(s.DiskGiB)<<30), max(mem, int64(s.MemMiB)<<20)
	}
	disk = max(disk, int64(m.cfg.team(team).MaxDiskGiB)<<30)
	return map[string]int64{manifestFile: 1 << 20, diskFile: disk, kernFile: 512 << 20, vmstate: 256 << 20, memFile: mem}
}

// ImportSnapshot stores an uploaded tar.zst as a new snapshot owned by team.
func (m *Manager) ImportSnapshot(ctx context.Context, body io.Reader, team, name, image, size string) (Snapshot, error) {
	if !reTeam.MatchString(team) {
		return Snapshot{}, errf(400, "invalid team")
	}
	if len(name) > 128 {
		return Snapshot{}, errf(400, "name too long")
	}
	if err := m.checkFloor(0); err != nil {
		return Snapshot{}, err
	}
	res, err := m.reserve(team, 1, 0)
	if err != nil {
		return Snapshot{}, err
	}
	defer res.release()
	s := Snapshot{ID: newID("snap-"), Name: name, Team: team, CreatedAt: m.now()}
	if err := m.makeStoreDir("snapshots", team, s.ID); err != nil {
		return s, err
	}
	dir := m.snapDir(s.ID)
	s, err = m.importInto(ctx, body, dir, s, image, size, res)
	if err != nil {
		m.removeStoreDir(dir)
		return s, err
	}
	m.mu.Lock()
	res.releaseLocked()
	m.snaps[s.ID] = s
	m.mu.Unlock()
	m.ev.publish(Event{Type: "snapshot.created", SnapID: s.ID, Team: team, Reason: "import"})
	return s, nil
}

// unprivileged: the decompressor parses a tenant's bytes, so it gets no root and, in a
// network namespace of its own, no network (boat's output chain drops nobody's packets too).
func unprivileged(euid int) *syscall.SysProcAttr {
	if euid != 0 {
		return nil
	}
	return noNetwork(&syscall.SysProcAttr{Credential: &syscall.Credential{Uid: nobodyID, Gid: nobodyID, Groups: []uint32{}}})
}

// unpackSandboxProps: the transient unit the decompressor runs in under unpackSandbox. It
// reads a tar.zst on stdin and writes a tar on stdout, nothing else.
var unpackSandboxProps = []string{
	"User=nobody", "PrivateNetwork=yes", "IPAddressDeny=any",
	"SystemCallFilter=@system-service", "SystemCallFilter=~@privileged @resources @mount", "SystemCallFilter=~@network-io",
	"SystemCallArchitectures=native", "SystemCallErrorNumber=EPERM", "NoNewPrivileges=yes",
	"CapabilityBoundingSet=", "ProtectSystem=strict", "ProtectHome=yes", "PrivateTmp=yes",
	"PrivateDevices=yes", "PrivateIPC=yes", "ProtectKernelTunables=yes", "ProtectKernelModules=yes",
	"ProtectKernelLogs=yes", "ProtectControlGroups=yes", "ProtectClock=yes", "ProtectHostname=yes",
	"ProtectProc=invisible", "RestrictNamespaces=yes", "RestrictRealtime=yes", "RestrictSUIDSGID=yes",
	"LockPersonality=yes", "MemoryDenyWriteExecute=yes", "MemoryMax=1G", "TasksMax=16",
}

// unpackCommand: the decompressor's argv and credentials. unpackSandbox (root only:
// systemd-run starts a system unit) wraps it in systemd-run with unpackSandboxProps.
func unpackCommand(c Config, argv []string, euid int) ([]string, *syscall.SysProcAttr, error) {
	if !c.UnpackSandbox || euid != 0 {
		return argv, unprivileged(euid), nil
	}
	bin, err := exec.LookPath(argv[0]) // the unit gets systemd's PATH, not boatd's
	if err != nil {
		return nil, nil, fmt.Errorf("unpack: %w", err)
	}
	run := []string{"systemd-run", "--quiet", "--pipe", "--wait", "--collect", "--service-type=exec"}
	for _, p := range unpackSandboxProps {
		run = append(run, "-p", p)
	}
	return append(append(run, "--", bin), argv[1:]...), nil, nil
}

const nobodyID = 65534

func (m *Manager) importInto(ctx context.Context, body io.Reader, dir string, s Snapshot, image, size string, res *reservation) (Snapshot, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	argv, attr, err := unpackCommand(m.cfg, m.unpackArgv, os.Geteuid())
	if err != nil {
		return s, err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = body
	cmd.Dir, cmd.Env, cmd.SysProcAttr = "/", []string{}, attr
	stderr := &cappedBuffer{max: 4096} // zstd's or systemd-run's complaint, for the log
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return s, err
	}
	if err := cmd.Start(); err != nil {
		return s, err
	}
	err = m.extract(tar.NewReader(out), dir, s.Team, res)
	cancel()
	cmd.Wait() // a corrupt stream already surfaced as a tar error
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			log.Printf("import %s: decompressor: %s", s.ID, msg)
		}
		return s, err
	}
	var mf exportManifest
	trusted := false
	if b, rerr := os.ReadFile(filepath.Join(dir, manifestFile)); rerr == nil && json.Unmarshal(b, &mf) == nil {
		trusted, err = m.verifyManifest(mf, dir)
		if err != nil {
			return s, err
		}
	}
	os.Remove(filepath.Join(dir, manifestFile))
	return m.finishImport(s, mf, trusted, dir, image, size, res)
}

// verifyManifest: signed by this host's key, with matching hashes.
func (m *Manager) verifyManifest(mf exportManifest, dir string) (bool, error) {
	key, err := m.exportKey()
	if err != nil {
		return false, err
	}
	want, err := manifestSig(key, mf)
	if err != nil || mf.Sig == "" || !hmac.Equal([]byte(want), []byte(mf.Sig)) {
		return false, err
	}
	for f, sum := range map[string]string{vmstate: mf.VMState, memFile: mf.Mem, kernFile: mf.Kernel} {
		if sum == "" && f != kernFile {
			continue
		}
		got, err := sha256File(filepath.Join(dir, f))
		if err != nil || got != sum {
			return false, nil
		}
	}
	return true, nil
}

func (m *Manager) finishImport(s Snapshot, mf exportManifest, trusted bool, dir, image, size string, res *reservation) (Snapshot, error) {
	st, err := os.Stat(filepath.Join(dir, diskFile))
	if err != nil {
		return s, errf(400, "archive has no %s", diskFile)
	}
	if trusted {
		o := mf.Snapshot
		s.Image, s.Size, s.VCPU, s.MemMiB, s.DiskGiB, s.HasMem = o.Image, o.Size, o.VCPU, o.MemMiB, o.DiskGiB, o.HasMem && mf.Mem != ""
	} else {
		for _, f := range []string{vmstate, memFile, kernFile} {
			os.Remove(filepath.Join(dir, f))
		}
		if image == "" {
			image = m.cfg.DefaultImage
		}
		if size == "" {
			size = m.cfg.DefaultSize
		}
		sz, ok := m.cfg.Sizes[size]
		if !reImage.MatchString(image) || !ok {
			return s, errf(400, "invalid image or size")
		}
		kern := filepath.Join(m.imageDir(image), kernFile)
		if !exists(kern) {
			return s, errf(400, "image %q is not installed", image)
		}
		if err := m.cp(kern, filepath.Join(dir, kernFile)); err != nil {
			return s, err
		}
		s.Image, s.Size, s.VCPU, s.MemMiB = image, size, sz.VCPU, sz.MemMiB
		s.DiskGiB = max(sz.DiskGiB, int((st.Size()+1<<30-1)>>30))
	}
	if err := res.set(snapCommit(s)); err != nil { // what it commits now, not what the upload declared
		return s, err
	}
	return s, writeJSONAtomic(filepath.Join(dir, "snap.json"), s, 0o600)
}

// extract writes only the expected regular files, each once and within its cap:
// no paths from the archive reach the filesystem, so traversal, symlinks and
// device nodes are refused rather than sanitized. Each member's declared size is
// charged to the team before a byte of it is written (writeSparse holds it to that size).
func (m *Manager) extract(tr *tar.Reader, dir, team string, res *reservation) error {
	caps, seen := m.importCaps(team), map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			if len(seen) == 0 {
				return errf(400, "empty or corrupt archive")
			}
			return nil
		}
		if err != nil {
			return errf(400, "bad archive: %v", err)
		}
		name := path.Clean(h.Name)
		lim, ok := caps[name]
		if !ok || seen[name] || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeGNUSparse) {
			return errf(400, "unexpected archive member %q", h.Name)
		}
		if h.Size > lim {
			return errf(400, "%s exceeds %d bytes", name, lim)
		}
		if err := res.grow(h.Size); err != nil {
			return err
		}
		seen[name] = true
		if err := m.writeSparse(filepath.Join(dir, name), tr, h.Size); err != nil {
			return err
		}
	}
}

// writeSparse skips all-zero blocks (the tar expands holes), and stops if the
// upload would take stateDir below its free-space floor.
func (m *Manager) writeSparse(p string, r io.Reader, size int64) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	var off, sinceCheck int64
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			if !allZero(buf[:n]) {
				if _, err := f.WriteAt(buf[:n], off); err != nil {
					return err
				}
				if sinceCheck += int64(n); sinceCheck >= importFloorEvery {
					sinceCheck = 0
					if err := m.checkFloor(0); err != nil {
						return err
					}
				}
			}
			off += int64(n)
		}
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
			break
		}
		if rerr != nil {
			return errf(400, "bad archive: %v", rerr)
		}
	}
	if off != size {
		return errf(400, "truncated archive member")
	}
	if err := f.Truncate(size); err != nil {
		return err
	}
	return f.Sync()
}

const importFloorEvery = 256 << 20 // bytes really written between free-space checks

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func (m *Manager) snapshotsOfTeam(team string) []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Snapshot{}
	for _, s := range m.snaps {
		if team == "" || s.Team == team {
			out = append(out, s)
		}
	}
	return out
}

// cappedBuffer keeps the first max bytes written to it and drops the rest.
type cappedBuffer struct {
	bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
