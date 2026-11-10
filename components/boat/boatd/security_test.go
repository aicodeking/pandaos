package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// plant replaces a jail-owned file the way a compromised VMM can.
func plant(t *testing.T, path, target string) {
	t.Helper()
	os.Remove(path)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func hostSecret(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(p, []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertUntouched(t *testing.T, p string) {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("host file's mode changed: %v %v", st, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "host secret" {
		t.Fatalf("host file changed: %q", b)
	}
}

func TestJailFilesNeverFollowed(t *testing.T) {
	secret := hostSecret(t)
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	plant(t, link, secret)
	if err := sealMem(link); err == nil {
		t.Error("sealMem followed a symlink")
	}
	if err := truncateJail(link, 0); err == nil {
		t.Error("truncate followed a symlink")
	}
	if err := fsyncFile(link); err == nil {
		t.Error("fsync followed a symlink")
	}
	if err := cloneFile(link, filepath.Join(dir, "copy"), "auto"); err == nil {
		t.Error("clone read through a symlink")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "copy")); strings.Contains(string(b), "secret") {
		t.Error("clone holds the host file")
	}
	assertUntouched(t, secret)
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- fsyncFile(fifo) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO accepted as a memory file")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a planted FIFO hangs boatd")
	}
}

func TestSnapshotRefusesPlantedFiles(t *testing.T) {
	secret := hostSecret(t)
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	// The VMM swaps its base for a link to a host file: the merged memory must not
	// become a copy of that file, and no chmod may reach it.
	if _, err := m.CreateSnapshot(ctx, v.ID, ""); err != nil {
		t.Fatal(err)
	}
	root := m.root(v.ID)
	plant(t, filepath.Join(root, memFile), secret)
	if _, err := m.CreateSnapshot(ctx, v.ID, ""); err == nil {
		t.Fatal("snapshot merged onto a planted base")
	}
	assertUntouched(t, secret)
	if st, _ := os.Stat(filepath.Join(m.vmDir(v.ID), memStage)); st != nil {
		t.Fatal("stage left behind")
	}
	// A planted disk: a read-through-symlink would put the host file into the fork.
	plant(t, filepath.Join(root, diskFile), secret)
	if _, err := m.Fork(ctx, v.ID, "", ""); err == nil {
		t.Fatal("fork cloned a planted disk")
	}
	if _, err := m.Resize(ctx, v.ID, ResizeReq{DiskGiB: 9}); err == nil {
		t.Fatal("resize truncated through a planted disk")
	}
	assertUntouched(t, secret)
}

func TestSocketsNeverFollowed(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "bdsock")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	real := filepath.Join(dir, "boatd.sock")
	ln, err := net.Listen("unix", real)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if c, err := dialJail(context.Background(), real); err != nil {
		t.Fatalf("control: a real socket: %v", err)
	} else {
		c.Close()
	}
	link := filepath.Join(dir, "v.sock")
	plant(t, link, real)
	if _, err := dialJail(context.Background(), link); err == nil {
		t.Fatal("boatd dialled a host socket through a link in the jail")
	}
	if _, err := agentCall(context.Background(), link, map[string]string{"op": "health"}); err == nil {
		t.Fatal("agent call through a planted link")
	}
}

func TestHungGuestBoundsStart(t *testing.T) {
	agentCallTimeout = 200 * time.Millisecond
	defer func() { agentCallTimeout = 30 * time.Second }()
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stop(ctx, v.ID, ""); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.hangOps = map[string]bool{"set_env": true}
	h.mu.Unlock()
	t0 := time.Now()
	got, _ := m.Start(ctx, v.ID, map[string]string{"A": "b"})
	if d := time.Since(t0); d > 3*time.Second {
		t.Fatalf("start waited %s for a hung guest", d)
	}
	if got.AgentReady {
		t.Fatal("a guest that hung set_env counted as ready")
	}
}

func TestReapDoesNotWaitForOneVM(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	var skew atomic.Int64
	m.now = func() time.Time { return time.Now().UTC().Add(time.Duration(skew.Load())) }
	stuck, err := m.Create(ctx, CreateReq{Name: "stuck", TTLS: 10})
	if err != nil {
		t.Fatal(err)
	}
	other, err := m.Create(ctx, CreateReq{Name: "other", TTLS: 10})
	if err != nil {
		t.Fatal(err)
	}
	unlock := m.lock(stuck.ID) // a start still waiting for its guest holds this
	skew.Store(int64(time.Minute))
	done := make(chan struct{})
	go func() { m.reap(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reap waited for a VM's lock")
	}
	eventually(t, "the other VM's TTL stop", func() bool {
		v, _ := m.get(other.ID)
		return v.State == Stopped
	})
	m.reapWG.Wait() // the busy VM holds no reap slot: it is simply tried again
	if v, _ := m.get(stuck.ID); v.State != Running {
		t.Fatalf("stopped through a held lock: %s", v.State)
	}
	unlock()
	m.reap(ctx)
	m.reapWG.Wait()
	if v, _ := m.get(stuck.ID); v.State != Stopped {
		t.Fatalf("stuck VM not stopped once its lock was free: %s", v.State)
	}
}

func TestStorageReservedAcrossVMs(t *testing.T) {
	m, _ := newTestManager(t, func(c *Config) { c.DefaultTeam.MaxDiskGiB = 4; c.DefaultTeam.MaxSnapshots = 2 })
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Team: "t"}) // 1 GiB disk + 512 MiB + logs
	if err != nil {
		t.Fatal(err)
	}
	// What a snapshot of another VM holds while its dump is being written.
	res, err := m.reserve("t", 1, 2<<30)
	if err != nil {
		t.Fatal(err)
	}
	var ae *apiErr
	if _, err := m.CreateSnapshot(ctx, v.ID, ""); !errors.As(err, &ae) || ae.code != 429 {
		t.Fatalf("second snapshot passed the same check: %v", err)
	}
	if st, _ := filepath.Glob(filepath.Join(m.cfg.StateDir, "snapshots", "*")); len(st) != 0 {
		t.Fatal("a refused snapshot left a dir")
	}
	res.release()
	if m.reservedBytes["t"] != 0 || m.reservedSnaps["t"] != 0 {
		t.Fatal("reservation not returned")
	}
	if _, err := m.CreateSnapshot(ctx, v.ID, ""); err != nil {
		t.Fatal(err)
	}
	if m.reservedBytes["t"] != 0 || m.reservedSnaps["t"] != 0 {
		t.Fatal("a finished snapshot still holds its reservation")
	}
}

func TestHostStorageAdmission(t *testing.T) {
	m, _ := newTestManager(t, func(c *Config) { c.StorageOvercommit = 1 })
	m.statfs = func(string) (uint64, uint64, error) { return 1 << 40, 4 << 30, nil }
	ctx := context.Background()
	a, err := m.Create(ctx, CreateReq{Team: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, CreateReq{Team: "b"}); err != nil {
		t.Fatal(err)
	}
	var ae *apiErr
	if _, err := m.Create(ctx, CreateReq{Team: "c"}); !errors.As(err, &ae) || ae.code != 507 {
		t.Fatalf("a third team past the host's disk: %v", err)
	}
	if _, err := m.Delete(ctx, a.ID); err != nil { // shrinking is never refused
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, CreateReq{Team: "c"}); err != nil {
		t.Fatal(err)
	}
}

func TestImportChargedWhileExtracting(t *testing.T) {
	m, _ := newTestManager(t, func(c *Config) { c.DefaultTeam.MaxDiskGiB = 2 })
	m.unpackArgv = []string{"cat"}
	ctx := context.Background()
	if _, err := m.Create(ctx, CreateReq{Team: "t"}); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: diskFile, Mode: 0o600, Size: 1 << 30, Typeflag: tar.TypeReg})
	// No data: a refusal must come from the header, before anything is written.
	var ae *apiErr
	_, err := m.ImportSnapshot(ctx, &b, "t", "", "", "")
	if !errors.As(err, &ae) || ae.code != 429 {
		t.Fatalf("over-quota member reached the disk: %v", err)
	}
	if st, _ := filepath.Glob(filepath.Join(m.cfg.StateDir, "snapshots", "*")); len(st) != 0 {
		t.Fatalf("a refused import left %v", st)
	}
	if m.reservedBytes["t"] != 0 || m.reservedSnaps["t"] != 0 {
		t.Fatal("a refused import still holds its reservation")
	}
}

func TestUnpackerDropsRoot(t *testing.T) {
	if a := unprivileged(0); a == nil || a.Credential.Uid != nobodyID || a.Credential.Gid != nobodyID || a.Credential.Groups == nil {
		t.Fatalf("root's decompressor keeps privileges: %+v", a)
	}
	if unprivileged(1000) != nil {
		t.Fatal("no credential to change when not root")
	}
}

func TestHostAdmissionCountsOverhead(t *testing.T) {
	m, _ := newTestManager(t, func(c *Config) { c.HostMemMiB, c.HostReserveMiB, c.MemOvercommit, c.VMOverheadMiB = 1500, 0, 1, 256 })
	ctx := context.Background()
	if _, err := m.Create(ctx, CreateReq{}); err != nil { // 512 + 256
		t.Fatal(err)
	}
	var ae *apiErr
	if _, err := m.Create(ctx, CreateReq{Name: "b"}); !errors.As(err, &ae) || ae.code != 503 {
		t.Fatalf("1536 MiB of cgroup limits admitted on 1500 MiB: %v", err)
	}
}

func TestLiftedMemoryLimitIsBounded(t *testing.T) {
	cgroupRoot = t.TempDir()
	defer func() { cgroupRoot = "/sys/fs/cgroup" }()
	m, _ := newTestManager(t, func(c *Config) { c.CgroupParent = "p" })
	m.cfg.cgroupLimits = true
	v := VM{ID: "vm-x", MemMiB: 1024}
	dir := filepath.Join(cgroupRoot, "p", v.ID)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "memory.max"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "memory.high"), nil, 0o644)
	m.liftMemLimits(v)
	b, _ := os.ReadFile(filepath.Join(dir, "memory.max"))
	want := (1024 + m.cfg.VMOverheadMiB + m.cfg.VMPageCacheMiB + 1024) << 20
	if got, err := strconv.Atoi(string(b)); err != nil || got != want {
		t.Fatalf("memory.max %q during a dump, want %d", b, want)
	}
}

func TestCgroupsFailClosed(t *testing.T) {
	cgroupRoot = t.TempDir()
	defer func() { cgroupRoot = "/sys/fs/cgroup" }()
	os.Mkdir(filepath.Join(cgroupRoot, "cgroup.subtree_control"), 0o755) // any write fails
	c := Config{Jailer: "/bin/jailer", CgroupParent: "p"}
	if err := setupCgroups(&c); err == nil || c.cgroupLimits {
		t.Fatalf("started without limits: %v", err)
	}
	c = Config{Jailer: "/bin/jailer"}
	if err := setupCgroups(&c); err == nil {
		t.Fatal("no cgroup parent accepted")
	}
	c = Config{Jailer: "/bin/jailer", CgroupParent: "p", CgroupsOptional: true}
	if err := setupCgroups(&c); err != nil || c.cgroupLimits {
		t.Fatalf("cgroupsOptional: %v, limits %v", err, c.cgroupLimits)
	}
}

func TestUntrustedTenantsConfig(t *testing.T) {
	ok := defaultConfig()
	ok.UntrustedTenants, ok.Jailer, ok.StorageOvercommit = true, "/bin/jailer", 1
	ok.EgressMbit, ok.IngressMbit, ok.IOMaxMBps, ok.IOMaxIOPS = 1, 1, 1, 1
	ok.IOCost, ok.IOCostModel = true, "rbps=1000000000 rrandiops=100000"
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for name, spoil := range map[string]func(*Config){
		"ksm":       func(c *Config) { c.KSMTeam = "x" },
		"ksm all":   func(c *Config) { c.KSMTeam = "*" },
		"warm":      func(c *Config) { c.WarmPool = true },
		"no jailer": func(c *Config) { c.Jailer = "" },
		"cgroups":   func(c *Config) { c.CgroupsOptional = true },
		"qgroups":   func(c *Config) { c.BtrfsQgroups = false },
		"storage":   func(c *Config) { c.StorageOvercommit = 2 },
		"egress":    func(c *Config) { c.EgressMbit = 0 },
		"io":        func(c *Config) { c.IOMaxIOPS = 0 },
		"ct":        func(c *Config) { c.CtPerVM = 0 },
		"no ioCost": func(c *Config) { c.IOCost = false },
		"no model":  func(c *Config) { c.IOCostModel = "" },
	} {
		c := ok
		spoil(&c)
		if c.validate() == nil {
			t.Errorf("%s accepted with untrustedTenants", name)
		}
	}
}

func TestHostHardened(t *testing.T) {
	sys := t.TempDir()
	smt := filepath.Join(sys, "devices/system/cpu/smt/control")
	ksm := filepath.Join(sys, "kernel/mm/ksm/run")
	os.MkdirAll(filepath.Dir(smt), 0o755)
	os.MkdirAll(filepath.Dir(ksm), 0o755)
	os.WriteFile(smt, []byte("on\n"), 0o644)
	os.WriteFile(ksm, []byte("0\n"), 0o644)
	if hostHardened(sys) == nil {
		t.Error("SMT on accepted")
	}
	os.WriteFile(smt, []byte("forceoff\n"), 0o644)
	if err := hostHardened(sys); err != nil {
		t.Error(err)
	}
	os.WriteFile(ksm, []byte("1\n"), 0o644)
	if hostHardened(sys) == nil {
		t.Error("KSM on accepted")
	}
}

func TestQuotaRefusalKeepsWarmTemplate(t *testing.T) {
	m, _ := newTestManager(t, func(c *Config) { c.WarmPool = true; c.DefaultTeam.MaxVMs = 1 })
	m.warmSettle = 0
	ctx := context.Background()
	if _, err := m.Create(ctx, CreateReq{Team: "t"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "template", func() bool { return m.warmTemplate(m.cfg.DefaultImage, m.cfg.DefaultSize) != "" })
	var ae *apiErr
	if _, err := m.Create(ctx, CreateReq{Team: "t", Name: "over"}); !errors.As(err, &ae) || ae.code != 429 {
		t.Fatal(err)
	}
	if m.warmTemplate(m.cfg.DefaultImage, m.cfg.DefaultSize) == "" {
		t.Fatal("a tenant's quota refusal deleted the shared template")
	}
}

func TestExportKeyFromCredential(t *testing.T) {
	m, _ := newTestManager(t, nil)
	d := t.TempDir()
	key := bytes.Repeat([]byte{7}, 32)
	os.WriteFile(filepath.Join(d, "boat-export-key"), key, 0o600)
	t.Setenv("CREDENTIALS_DIRECTORY", d)
	got, err := m.exportKey()
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("%x %v", got, err)
	}
	if exists(filepath.Join(m.cfg.StateDir, "export.key")) {
		t.Fatal("a plaintext key was written beside a sealed one")
	}
}

func TestQuotaFullDoesNotBlockStop(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{})
	if err != nil {
		t.Fatal(err)
	}
	m.saveState = func(string, VM) error { return &os.PathError{Op: "write", Path: "x", Err: syscall.EDQUOT} }
	if got, err := m.Stop(ctx, v.ID, ""); err != nil || got.State != Stopped {
		t.Fatalf("a team at its disk quota cannot stop a VM: %+v %v", got, err)
	}
}
