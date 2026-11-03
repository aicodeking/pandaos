package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func putFile(t *testing.T, m *Manager, id, path, data string) {
	t.Helper()
	sock, err := m.sock(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentCall(context.Background(), sock, map[string]any{"op": "put_file", "path": path,
		"offset": 0, "last": true, "data_b64": b64(data)}); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycle(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	_, events, cancel := m.ev.subscribe(0)
	defer cancel()

	v, err := m.Create(ctx, CreateReq{Name: "a", Team: "t1", Env: map[string]string{"K": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	if v.State != Running || !v.AgentReady || v.IP != "10.200.0.2" || v.Tap != "bt2" || v.MAC != "06:00:0a:c8:00:02" {
		t.Fatalf("created: %+v", v)
	}
	fc := h.fcOf(t, m, v.ID)
	if st, _ := os.Stat(filepath.Join(m.root(v.ID), diskFile)); st.Size() != 1<<30 {
		t.Fatalf("disk not grown to quota: %d", st.Size())
	}
	if len(fc.idents) != 1 || fc.idents[0]["ipv4_cidr"] != "10.200.0.2/16" || fc.idents[0]["hostname"] != "a" || fc.env["K"] != "v" {
		t.Fatalf("guest init: %v %v", fc.idents, fc.env)
	}
	if !fc.called("PUT /actions") || fc.tap != "tap0" || !h.ran("ip link add bt2 type veth peer name veth0 netns boat2") ||
		!h.ran("ip -n boat2 tuntap add dev tap0 mode tap") || !h.ran("ip link set bt2 master boatbr0 up") {
		t.Fatal("not cold booted on tap0 in its netns")
	}
	putFile(t, m, v.ID, "/home/dev/x", "hello")

	// stop: pause + snapshot + kill; files land atomically.
	v, err = m.Stop(ctx, v.ID, "api")
	if err != nil || v.State != Stopped || !v.HasSnapshot || v.PID != 0 {
		t.Fatalf("stop: %+v %v", v, err)
	}
	for _, f := range []string{vmstate, memFile} {
		if !exists(filepath.Join(m.root(v.ID), f)) {
			t.Fatalf("snapshot file %s not in place", f)
		}
	}
	for _, f := range []string{snapState, snapDiff, memNew} {
		if exists(filepath.Join(m.root(v.ID), f)) {
			t.Fatalf("temp file %s left", f)
		}
	}
	if !fc.called("PATCH /vm") || !fc.called("snapshot Diff") || !fc.called("agent sync") {
		t.Fatal("stop did not sync + diff-snapshot")
	}
	if st, _ := os.Stat(filepath.Join(m.root(v.ID), memFile)); st.Size() != 512<<20 {
		t.Fatalf("merged mem is %d bytes, want the VM's RAM", st.Size())
	}

	// start: restore, delete the now-stale snapshot, fix the guest clock.
	v, err = m.Start(ctx, v.ID, nil)
	if err != nil || v.State != Running {
		t.Fatalf("start: %+v %v", v, err)
	}
	fc = h.fcOf(t, m, v.ID)
	if !fc.called("PUT /snapshot/load") || !fc.called("load tracking") || fc.called("PUT /actions") || !fc.clockSet {
		t.Fatal("start did not restore from snapshot")
	}
	if exists(filepath.Join(m.root(v.ID), vmstate)) || !exists(filepath.Join(m.root(v.ID), memFile)) {
		t.Fatal("after restore: vmstate must be consumed, mem kept as the backing/diff base")
	}

	// fork a running VM: parent resumed, child restored with new identity + disk copy.
	c, err := m.Fork(ctx, v.ID, "b", "t1")
	if err != nil || c.State != Running || c.IP != "10.200.0.3" || c.ParentID != v.ID {
		t.Fatalf("fork: %+v %v", c, err)
	}
	if fc.paused {
		t.Fatal("parent left paused")
	}
	cfc := h.fcOf(t, m, c.ID)
	if !cfc.called("PUT /snapshot/load") || cfc.tap != "tap0" || !h.ran("ip netns add boat3") {
		t.Fatalf("child not restored in its own netns: %v %s", cfc.calls, cfc.tap)
	}
	if i, j := indexOf(cfc.calls, "agent reseed"), indexOf(cfc.calls, "agent set_identity"); i < 0 || j < i {
		t.Fatalf("reseed must precede set_identity: %v", cfc.calls)
	}
	if len(cfc.idents) != 1 || cfc.idents[0]["ipv4_cidr"] != "10.200.0.3/16" || cfc.idents[0]["mac"] != "06:00:0a:c8:00:03" {
		t.Fatalf("child identity: %v", cfc.idents)
	}
	if cfc.disk()["/home/dev/x"] != b64("hello") {
		t.Fatal("child disk is not a copy of the parent's")
	}
	if _, err := m.Fork(ctx, v.ID, "c", "other-team"); !errors.Is(err, errNotFound) {
		t.Fatalf("cross-team fork: %v", err)
	}

	// named snapshot of the child, then a new VM from it.
	s, err := m.CreateSnapshot(ctx, c.ID, "snap1")
	if err != nil || !s.HasMem {
		t.Fatalf("snapshot: %+v %v", s, err)
	}
	d, err := m.Create(ctx, CreateReq{Team: "t1", Snapshot: s.ID})
	if err != nil || d.State != Running || d.FromSnapshot != s.ID {
		t.Fatalf("from snapshot: %+v %v", d, err)
	}
	if h.fcOf(t, m, d.ID).disk()["/home/dev/x"] != b64("hello") {
		t.Fatal("snapshot lost the disk")
	}

	// fork a stopped VM (from its stop snapshot).
	if _, err := m.Stop(ctx, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	e, err := m.Fork(ctx, d.ID, "", "")
	if err != nil || e.State != Running || !h.fcOf(t, m, e.ID).called("PUT /snapshot/load") {
		t.Fatalf("fork stopped: %+v %v", e, err)
	}

	rep, err := m.Usage(time.Time{})
	if err != nil || len(rep.VMs) < 4 || len(rep.Snapshots) != 1 {
		t.Fatalf("usage: %+v %v", rep, err)
	}
	for _, x := range m.list("") {
		if _, err := m.Delete(ctx, x.ID); err != nil {
			t.Fatal(err)
		}
		if exists(m.vmDir(x.ID)) {
			t.Fatal("vm dir left behind")
		}
	}
	if exists(m.snapDir(s.ID)) {
		t.Fatal("snapshot of deleted vm left behind")
	}
	seen := map[string]bool{}
	for len(events) > 0 {
		ev := <-events
		seen[ev.Type+":"+string(ev.State)] = true
	}
	for _, k := range []string{"vm.state:creating", "vm.state:running", "vm.state:stopping", "vm.state:stopped",
		"vm.state:starting", "vm.state:forking", "snapshot.created:", "snapshot.deleted:", "vm.deleted:"} {
		if !seen[k] {
			t.Errorf("no %s event (saw %v)", k, seen)
		}
	}
}

func indexOf(xs []string, x string) int {
	for i, y := range xs {
		if y == x {
			return i
		}
	}
	return -1
}

func TestQuota(t *testing.T) {
	m, _ := newTestManager(t, func(c *Config) {
		c.Teams = map[string]TeamPolicy{"t": {MaxVMs: 1}}
		c.DefaultTeam = TeamPolicy{MaxVCPU: 2}
	})
	ctx := context.Background()
	a, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	var ae *apiErr
	if _, err := m.Create(ctx, CreateReq{Team: "t"}); !errors.As(err, &ae) || ae.code != 429 {
		t.Fatalf("want 429, got %v", err)
	}
	if _, err := m.Fork(ctx, a.ID, "", ""); !errors.As(err, &ae) || ae.code != 429 {
		t.Fatalf("fork past quota: %v", err)
	}
	m.Stop(ctx, a.ID, "")
	b, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ctx, a.ID, nil); !errors.As(err, &ae) || ae.code != 429 {
		t.Fatalf("start past quota: %v", err)
	}
	m.Delete(ctx, b.ID)
	if _, err := m.Start(ctx, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, CreateReq{Team: "x", Size: "large"}); !errors.As(err, &ae) || ae.code != 429 {
		t.Fatalf("vcpu quota: %v", err)
	}
}

func TestReconcile(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	a, _ := m.Create(ctx, CreateReq{Name: "a"})
	b, _ := m.Create(ctx, CreateReq{Name: "b"})
	// b looks like boatd died mid-stop with a half-written snapshot.
	m.update(b.ID, "", func(v *VM) { v.State = Stopping })
	os.WriteFile(filepath.Join(m.root(b.ID), memNew), []byte("x"), 0o600)
	h.Kill(b.PID)

	m2, err := newManager(m.cfg, h.run, h)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := m2.get(a.ID); v.State != Running || v.PID != a.PID {
		t.Fatalf("running vm not adopted: %+v", v)
	}
	if v, _ := m2.get(b.ID); v.State != Stopped || v.HasSnapshot || exists(filepath.Join(m.root(b.ID), memNew)) {
		t.Fatalf("dead vm not reconciled: %+v", v)
	}
	// the adopted process exits on its own (guest poweroff) -> stopped, cold next time.
	h.Kill(a.PID)
	eventually(t, "adopted exit noticed", func() bool { v, _ := m2.get(a.ID); return v.State == Stopped })
	if v, _ := m2.get(b.ID); v.State != Stopped {
		t.Fatal(v.State)
	}
	if _, err := m2.Start(ctx, b.ID, nil); err != nil || !h.fcOf(t, m2, b.ID).called("PUT /actions") {
		t.Fatalf("cold start after crash: %v", err)
	}
}

func TestReap(t *testing.T) {
	m, h := newTestManager(t, func(c *Config) { c.IdleStopS = 60 })
	ctx := context.Background()
	var skew atomic.Int64
	m.now = func() time.Time { return time.Now().UTC().Add(time.Duration(skew.Load())) }
	ttl, _ := m.Create(ctx, CreateReq{Name: "ttl", TTLS: 30})
	busy, _ := m.Create(ctx, CreateReq{Name: "busy"})
	never, _ := m.Create(ctx, CreateReq{Name: "never", IdleStopS: -1})
	reap := func() { m.reap(ctx); m.reapWG.Wait() }
	reap() // nothing due yet
	skew.Store(int64(45 * time.Second))
	reap() // agent reports idle_s=0: only the TTL fires
	if v, _ := m.get(ttl.ID); v.State != Stopped {
		t.Fatalf("ttl: %s", v.State)
	}
	if v, _ := m.get(busy.ID); v.State != Running {
		t.Fatal("busy vm stopped")
	}
	h.mu.Lock()
	h.idleS = 3600
	h.mu.Unlock()
	skew.Store(int64(2 * time.Hour))
	reap()
	if v, _ := m.get(busy.ID); v.State != Stopped {
		t.Fatalf("idle: %s", v.State)
	}
	if v, _ := m.get(never.ID); v.State != Running {
		t.Fatal("idle_stop_s=-1 vm stopped")
	}
}

func TestValidate(t *testing.T) {
	m, _ := newTestManager(t, nil)
	for _, r := range []CreateReq{
		{Name: "Bad_Name"}, {Image: "../etc"}, {Size: "huge"}, {Team: "a b"},
		{Env: map[string]string{"1X": "y"}}, {Labels: map[string]string{"a b": "c"}}, {Image: "missing"},
	} {
		var ae *apiErr
		if _, err := m.Create(context.Background(), r); !errors.As(err, &ae) || ae.code != 400 {
			t.Errorf("%+v: want 400, got %v", r, err)
		}
	}
	if len(m.list("")) != 0 {
		t.Fatal("invalid request allocated a vm")
	}
	if err := (Config{Subnet: "10.0.0.0/8"}).validate(); err == nil || !strings.Contains(err.Error(), "/16") {
		t.Fatal(err)
	}
}

func TestStopAll(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	a, _ := m.Create(ctx, CreateReq{})
	b, _ := m.Create(ctx, CreateReq{})
	m.stopAll(ctx, "boatd shutdown")
	for _, id := range []string{a.ID, b.ID} {
		if v, _ := m.get(id); v.State != Stopped || !v.HasSnapshot || !v.ResumeOnBoot {
			t.Fatalf("%+v", v)
		}
	}
	m.resumeAll(ctx)
	eventually(t, "resumed", func() bool { v, _ := m.get(b.ID); return v.State == Running && v.AgentReady })
	eventually(t, "resumed", func() bool { v, _ := m.get(a.ID); return v.State == Running && !v.ResumeOnBoot })
}

func TestStorageQuotaAndAdmission(t *testing.T) {
	m, _ := newTestManager(t, func(c *Config) {
		c.Teams = map[string]TeamPolicy{"t": {MaxTotalVMs: 1, MaxSnapshots: 1}, "d": {MaxDiskGiB: 2}}
		c.HostMemMiB, c.HostReserveMiB, c.MemOvercommit = 2048, 512, 1.0 // 1536 MiB for VMs
	})
	ctx := context.Background()
	var ae *apiErr
	a, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	m.Stop(ctx, a.ID, "")
	if _, err := m.Create(ctx, CreateReq{Team: "t"}); !errors.As(err, &ae) || ae.code != 429 {
		t.Fatalf("maxTotalVMs counts stopped VMs: %v", err)
	}
	if _, err := m.CreateSnapshot(ctx, a.ID, "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateSnapshot(ctx, a.ID, "2"); !errors.As(err, &ae) || ae.code != 429 {
		t.Fatalf("maxSnapshots: %v", err)
	}
	// d: 2 GiB committed = one 1 GiB disk + 512 MiB mem; a second VM does not fit.
	if _, err := m.Create(ctx, CreateReq{Team: "d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, CreateReq{Team: "d"}); !errors.As(err, &ae) || ae.code != 429 {
		t.Fatalf("maxDiskGiB: %v", err)
	}
	// host: 1536 MiB, one 512 MiB VM running; two more fit, the third does not.
	m.Create(ctx, CreateReq{Team: "x"})
	m.Create(ctx, CreateReq{Team: "x"})
	if _, err := m.Create(ctx, CreateReq{Team: "x"}); !errors.As(err, &ae) || ae.code != 503 {
		t.Fatalf("host admission: %v", err)
	}
	rep, err := m.Usage(time.Time{})
	if err != nil || len(rep.Storage) == 0 || rep.Disk == nil {
		t.Fatalf("usage storage: %+v %v", rep.Storage, err)
	}
	for _, s := range rep.Storage {
		if s.Team == "t" && (s.Snapshots != 1 || s.VMs != 1 || s.CommittedBytes <= 0 || s.VMApparentBytes <= 0) {
			t.Fatalf("storage: %+v", s)
		}
	}
}

func TestDiskFloor(t *testing.T) {
	m, h := newTestManager(t, func(c *Config) { c.MinFreePct = 10 })
	ctx := context.Background()
	m.statfs = func(string) (uint64, uint64, error) { return 50 << 30, 100 << 30, nil }
	v, err := m.Create(ctx, CreateReq{})
	if err != nil {
		t.Fatal(err)
	}
	m.statfs = func(string) (uint64, uint64, error) { return 5 << 30, 100 << 30, nil } // 5% free
	var ae *apiErr
	if _, err := m.Create(ctx, CreateReq{}); !errors.As(err, &ae) || ae.code != 507 {
		t.Fatalf("create below floor: %v", err)
	}
	if _, err := m.Fork(ctx, v.ID, "", ""); !errors.As(err, &ae) || ae.code != 507 {
		t.Fatalf("fork below floor: %v", err)
	}
	if _, err := m.CreateSnapshot(ctx, v.ID, ""); !errors.As(err, &ae) || ae.code != 507 {
		t.Fatalf("snapshot below floor: %v", err)
	}
	fc := h.fcOf(t, m, v.ID)
	v, err = m.Stop(ctx, v.ID, "")
	if err != nil || v.State != Stopped || v.HasSnapshot || !fc.called("agent shutdown") || fc.called("PUT /snapshot/create") {
		t.Fatalf("stop below floor must shut down cleanly: %+v %v %v", v, err, fc.calls)
	}
	if exists(filepath.Join(m.root(v.ID), memFile)) {
		t.Fatal("stale mem kept after a cold stop")
	}
}

func TestNoQuotasWithoutBtrfs(t *testing.T) {
	quotaCtl = func(string) error { return syscall.ENOTTY }
	defer func() { quotaCtl = func(string) error { return nil } }()
	m, h := newTestManager(t, nil)
	if _, err := m.Create(context.Background(), CreateReq{}); err != nil {
		t.Fatal(err)
	}
	if h.ran("btrfs") || m.cfg.BtrfsQgroups {
		t.Fatal("quotas where stateDir has none")
	}
	c := m.cfg
	c.BtrfsQgroups, c.UntrustedTenants = true, true
	if _, err := newManager(c, h.run, h); err == nil {
		t.Fatal("untrustedTenants went on without quotas")
	}
}

func TestBtrfsQgroups(t *testing.T) {
	m, h := newTestManager(t, func(c *Config) {
		c.BtrfsQgroups, c.IPv6Prefix = true, "fd42::/64"
		c.DefaultTeam.MaxDiskGiB = 100
	})
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	vol := filepath.Join(m.cfg.StateDir, "teams", "t_t1")
	if !h.ran("btrfs subvolume create "+vol) ||
		!h.ran(fmt.Sprintf("btrfs qgroup limit -e %d %s", int64(100)<<30/100*125, vol)) {
		t.Fatal(h.cmds)
	}
	if real, _ := filepath.EvalSymlinks(m.vmDir(v.ID)); !strings.HasSuffix(real, "/teams/t_t1/vms/"+v.ID) {
		t.Fatalf("VM dir at %s", real)
	}
	// A team name reTeam allows must stay one path component.
	if _, err := m.Create(ctx, CreateReq{Team: ".."}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(m.cfg.StateDir, "teams", "t_..")) {
		t.Fatal("team .. escaped teams/")
	}
	s1, err := m.CreateSnapshot(ctx, v.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	// A memory file stays one inode across the team's VM, snapshot and fork.
	f, err := m.Fork(ctx, v.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	st1, _ := os.Stat(filepath.Join(m.snapDir(s1.ID), memFile))
	st2, _ := os.Stat(filepath.Join(m.root(v.ID), memFile))
	st3, _ := os.Stat(filepath.Join(m.root(f.ID), memFile))
	if st1 == nil || st3 == nil || st2 == nil || !os.SameFile(st2, st3) {
		t.Fatal("fork did not link the memory file within the team's subvolume")
	}
	if _, st := m.storage(); st.Total == 0 {
		t.Fatal("no disk totals")
	}
	if rep, _ := m.storage(); rep["t1"].VMApparentBytes == 0 {
		t.Fatal("usage does not walk through the VM dir's symlink")
	}
	m.Delete(ctx, f.ID)
	m.Delete(ctx, v.ID)
	if exists(m.vmDir(v.ID)) || exists(filepath.Join(vol, "vms", v.ID)) || exists(filepath.Join(vol, "snapshots", s1.ID)) || !h.ran("conntrack -D -f ipv4 -s "+v.IP) {
		t.Fatal("VM dir, its data or conntrack not cleaned up")
	}
	if !h.ran("conntrack -D -f ipv6 -d " + v.IPv6) {
		t.Fatal("ipv6 conntrack not flushed")
	}
}

func TestCopyExtents(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "diff"), filepath.Join(dir, "base")
	f, _ := os.Create(src)
	f.Truncate(4 << 20)
	f.WriteAt([]byte("AAAA"), 1<<20)
	f.WriteAt([]byte("BBBB"), 3<<20)
	f.Close()
	os.WriteFile(dst, bytes.Repeat([]byte("x"), 4<<20), 0o600)
	if err := copyExtents(src, dst); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dst)
	if string(b[1<<20:1<<20+4]) != "AAAA" || string(b[3<<20:3<<20+4]) != "BBBB" || len(b) != 4<<20 {
		t.Fatal("dirty pages not merged")
	}
	// Holes keep the base where the filesystem reports them (APFS/btrfs/ext4 do at 4K+).
	if b[0] != 'x' && b[0] != 0 {
		t.Fatalf("byte 0 = %q", b[0])
	}
}

func TestFullDiskKeepsControlPlane(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{})
	if err != nil {
		t.Fatal(err)
	}
	real := m.saveState
	m.mu.Lock()
	m.saveState = func(string, VM) error { return fmt.Errorf("write: %w", syscall.ENOSPC) }
	m.mu.Unlock()
	st, err := m.Stop(ctx, v.ID, "")
	if err != nil || st.State != Stopped {
		t.Fatalf("stop on a full disk: %+v %v", st, err)
	}
	var onDisk VM
	readJSON(filepath.Join(m.vmDir(v.ID), "vm.json"), &onDisk)
	if onDisk.State == Stopped {
		t.Fatal("test proves nothing: the file was written")
	}
	m.flushDirty() // still full
	m.mu.Lock()
	dirty := m.dirty[v.ID]
	m.saveState = real
	m.mu.Unlock()
	if !dirty {
		t.Fatal("failed write not remembered")
	}
	m.flushDirty()
	readJSON(filepath.Join(m.vmDir(v.ID), "vm.json"), &onDisk)
	if onDisk.State != Stopped {
		t.Fatalf("state not flushed: %s", onDisk.State)
	}
	// A non-ENOSPC failure still reports.
	m.mu.Lock()
	m.saveState = func(string, VM) error { return errors.New("boom") }
	m.mu.Unlock()
	if _, err := m.update(v.ID, "", func(x *VM) { x.Error = "x" }); err == nil {
		t.Fatal("real write errors must surface")
	}
	m.mu.Lock()
	m.saveState = real
	m.mu.Unlock()
	if _, err := m.Delete(ctx, v.ID); err != nil || exists(m.vmDir(v.ID)) {
		t.Fatalf("delete: %v", err)
	}
}

func TestCmdlineAlive(t *testing.T) {
	const id = "vm-1"
	run := []byte("1 (fire) cracker) S 1 2 3")
	for _, c := range []struct {
		name, cmdline, stat string
		want                bool
	}{
		{"named", "firecracker\x00--id\x00vm-1\x00", "1 (firecracker) S 1", true},
		{"pid reused", "bash\x00-c\x00x\x00", "1 (bash) S 1", false},
		{"mid exec: empty cmdline, still running", "", string(run), true},
		{"zombie", "", "1 (firecracker) Z 1", false},
		{"dead", "", "1 (firecracker) X 1", false},
		{"unreadable stat", "", "", false},
	} {
		if got := cmdlineAlive([]byte(c.cmdline), []byte(c.stat), id); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

func TestRotateLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "firecracker.log")
	if err := rotateLog(p); err != nil {
		t.Fatal(err) // no file yet
	}
	os.WriteFile(p, []byte("small"), 0o600)
	if rotateLog(p); !exists(p) || exists(p+".1") {
		t.Fatal("rotated a small log")
	}
	b := append(bytes.Repeat([]byte("x"), logCapBytes), []byte("TAIL")...)
	os.WriteFile(p, b, 0o600)
	if err := rotateLog(p); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Size() != 0 {
		t.Fatalf("not truncated: %d", st.Size())
	}
	tail, _ := os.ReadFile(p + ".1")
	if len(tail) != logTailBytes || !bytes.HasSuffix(tail, []byte("TAIL")) {
		t.Fatalf("tail %d bytes", len(tail))
	}
}

func TestRulesBeforeBoot(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	a, _ := m.Create(ctx, CreateReq{Team: "t"})
	s, err := m.CreateSnapshot(ctx, a.ID, "s")
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.Create(ctx, CreateReq{Team: "t", Snapshot: s.ID})
	if err != nil {
		t.Fatal(err)
	}
	f, err := m.Fork(ctx, a.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []VM{a, c, f} {
		if !strings.Contains(h.nftAtStart[v.ID], `iifname "`+v.Tap+`" ether saddr != `+v.MAC) {
			t.Errorf("%s started before its rules were loaded", v.ID)
		}
	}
}

func TestPortsCapsAndPrivate(t *testing.T) {
	m, h := newTestManager(t, func(c *Config) {
		c.MaxPortsPerVM = 2
		c.Teams = map[string]TeamPolicy{"t": {MaxPorts: 3}}
	})
	ctx := context.Background()
	a, _ := m.Create(ctx, CreateReq{Team: "t"})
	b, _ := m.Create(ctx, CreateReq{Team: "t"})
	var ae *apiErr
	p, err := m.AddPort(a.ID, 80, true)
	if err != nil || p.HostPort == 0 {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := m.AddPort(a.ID, 81, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddPort(a.ID, 82, false); !errors.As(err, &ae) || ae.code != 429 {
		t.Fatalf("per-VM cap: %v", err)
	}
	if _, err := m.AddPort(b.ID, 80, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddPort(b.ID, 81, false); !errors.As(err, &ae) || ae.code != 429 {
		t.Fatalf("team cap: %v", err)
	}
	// Re-adding is not a new port, and public:false really closes it.
	q, err := m.AddPort(a.ID, 80, false)
	if err != nil || q.HostPort != 0 {
		t.Fatalf("not private: %+v %v", q, err)
	}
	if strings.Contains(h.nft, fmt.Sprintf("dport %d dnat", p.HostPort)) {
		t.Fatal("DNAT survived")
	}
	if !h.ran(fmt.Sprintf("conntrack -D -f ipv4 -p tcp --dport %d", p.HostPort)) {
		t.Fatal("flows of the closed port not flushed")
	}
	h.mu.Lock()
	h.ctErr = errors.New("conntrack: not found")
	h.mu.Unlock()
	m.Delete(ctx, b.ID)
	if m.ctFlushErrs.Load() == 0 {
		t.Fatal("conntrack failure not counted")
	}
	h.mu.Lock()
	h.ctErr = errors.New("conntrack v1.4: 0 flow entries have been deleted.")
	h.mu.Unlock()
	before := m.ctFlushErrs.Load()
	m.Delete(ctx, a.ID)
	if m.ctFlushErrs.Load() != before {
		t.Fatal("nothing-to-delete counted as a failure")
	}
}

func TestResumeAfterCrash(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	v, _ := m.Create(ctx, CreateReq{Name: "crashed"})
	stopped, _ := m.Create(ctx, CreateReq{Name: "stopped"})
	m.Stop(ctx, stopped.ID, "")
	// As under KillMode=mixed without an outside cgroup: boatd and firecracker die together.
	cfg := m.cfg
	dead := newFakeHost()
	dead.next = h.next + 100
	m2, err := newManager(cfg, dead.run, dead)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := m2.get(v.ID)
	if x.State != Stopped || !x.ResumeOnBoot || x.HasSnapshot {
		t.Fatalf("%+v", x)
	}
	if y, _ := m2.get(stopped.ID); y.ResumeOnBoot {
		t.Fatal("a VM the user stopped must stay stopped")
	}
	m2.resumeAll(ctx)
	eventually(t, "cold resume", func() bool { y, _ := m2.get(v.ID); return y.State == Running && y.AgentReady })
	if y, _ := m2.get(stopped.ID); y.State != Stopped {
		t.Fatal("stopped VM resumed")
	}
	h.Kill(v.PID)
}

func TestSnapshotDurability(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	a, _ := m.Create(ctx, CreateReq{Name: "synced"})
	b, _ := m.Create(ctx, CreateReq{Name: "unsynced"})
	m.Stop(ctx, a.ID, "")
	m.Stop(ctx, b.ID, "")
	eventually(t, "background fsync", func() bool {
		x, _ := m.get(a.ID)
		y, _ := m.get(b.ID)
		return x.SnapSynced && y.SnapSynced
	})
	// As if the host died before b's fsync finished.
	m.update(b.ID, "", func(x *VM) { x.SnapSynced = false })
	m2, err := newManager(m.cfg, h.run, h)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := m2.get(a.ID); !v.HasSnapshot {
		t.Fatal("durable snapshot discarded")
	}
	if v, _ := m2.get(b.ID); v.HasSnapshot || exists(filepath.Join(m2.root(b.ID), memFile)) {
		t.Fatalf("unsynced snapshot trusted: %+v", v)
	}
	if v, err := m2.Start(ctx, b.ID, nil); err != nil || !h.fcOf(t, m2, v.ID).called("PUT /actions") {
		t.Fatalf("cold boot: %v", err)
	}
}
