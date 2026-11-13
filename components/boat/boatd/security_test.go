package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
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

// Writing memory.max under memory.current OOM-kills the VMM: the max waits for the usage.
func TestMemoryMaxNotLoweredUnderUsage(t *testing.T) {
	cgroupRoot = t.TempDir()
	defer func() { cgroupRoot = "/sys/fs/cgroup"; memMaxWait = 30 * time.Second }()
	memMaxWait = 300 * time.Millisecond
	m, _ := newTestManager(t, func(c *Config) { c.CgroupParent = "p" })
	m.cfg.cgroupLimits = true
	v := VM{ID: "vm-x", MemMiB: 1024}
	dir := filepath.Join(cgroupRoot, "p", v.ID)
	os.MkdirAll(dir, 0o755)
	high, hard := m.cfg.memLimits(v)
	for _, f := range []string{"memory.max", "memory.high", "memory.reclaim"} {
		os.WriteFile(filepath.Join(dir, f), nil, 0o644)
	}
	read := func(f string) string { b, _ := os.ReadFile(filepath.Join(dir, f)); return string(b) }
	os.WriteFile(filepath.Join(dir, "memory.current"), []byte(strconv.FormatInt(hard+(1<<20), 10)+"\n"), 0o644)
	m.setMemLimits(v)
	if read("memory.max") != "" || read("memory.high") != strconv.FormatInt(high, 10) || read("memory.reclaim") != "1048576" {
		t.Fatalf("over the limit: max %q high %q reclaim %q", read("memory.max"), read("memory.high"), read("memory.reclaim"))
	}
	os.WriteFile(filepath.Join(dir, "memory.current"), []byte(strconv.FormatInt(hard, 10)+"\n"), 0o644)
	m.setMemLimits(v)
	if read("memory.max") != strconv.FormatInt(hard, 10) {
		t.Fatalf("usage fits, memory.max %q", read("memory.max"))
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

// untrustedConfig: the defaults with untrusted.json (what the NixOS module merges in) on top.
func untrustedConfig(t *testing.T) Config {
	t.Helper()
	b, err := os.ReadFile("untrusted.json")
	if err != nil {
		t.Fatal(err)
	}
	c := defaultConfig()
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	c.Jailer, c.IOCostModel = "/bin/jailer", "rbps=1000000000 rrandiops=100000"
	return c
}

func TestUntrustedTenantsConfig(t *testing.T) {
	ok := untrustedConfig(t)
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for name, spoil := range map[string]func(*Config){
		"ksm":        func(c *Config) { c.KSMTeam = "x" },
		"no jailer":  func(c *Config) { c.Jailer = "" },
		"cgroups":    func(c *Config) { c.CgroupsOptional = true },
		"storage 0":  func(c *Config) { c.StorageOvercommit = 0 },
		"dirty 0":    func(c *Config) { c.HostDirtyRatio = 0 },
		"share 101":  func(c *Config) { c.UplinkSharePct = 101 },
		"huge pages": func(c *Config) { c.HugePages, c.Balloon, c.DiffSnapshots = true, false, false },
	} {
		c := ok
		spoil(&c)
		if c.validate() == nil {
			t.Errorf("%s accepted with untrustedTenants", name)
		}
	}
	// No disk model yet: iocost stays off, the io.max ceilings (checked above) still bound each VM.
	if c := ok; func() error { c.IOCostModel = ""; return c.validate() }() != nil {
		t.Error("untrustedTenants without an ioCostModel refused; it falls back to io.max")
	}
	// Raised limits stay accepted; explicit caps replace the uplink share.
	c := ok
	c.IOMaxMBps, c.CtPerVM, c.UplinkSharePct, c.EgressMbit, c.IngressMbit = 8000, 9000, 0, 300, 300
	if err := c.validate(); err != nil {
		t.Errorf("raised limits refused: %v", err)
	}
}

// Every key of untrusted.json has a rule, and its weak value is refused (naming the key).
func TestUntrustedEveryKeyChecked(t *testing.T) {
	b, err := os.ReadFile("untrusted.json")
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}
	rules := map[string]untrustedRule{}
	for _, r := range untrustedRules {
		rules[r.key] = r
		if _, ok := keys[r.key]; !ok {
			t.Errorf("rule %s is not in untrusted.json", r.key)
		}
	}
	for k := range keys {
		if k == "untrustedTenants" {
			continue
		}
		r, ok := rules[k]
		if !ok {
			t.Errorf("untrusted.json key %s has no rule in untrustedRules", k)
			continue
		}
		c := untrustedConfig(t)
		weak, _ := json.Marshal(map[string]any{k: r.weak})
		if err := json.Unmarshal(weak, &c); err != nil {
			t.Fatal(err)
		}
		if err := c.validate(); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s = %v: got %v, want a refusal naming %s", k, r.weak, err, k)
		}
	}
}

func TestHostHardened(t *testing.T) {
	sys, procSys := t.TempDir(), t.TempDir()
	smt := filepath.Join(sys, "devices/system/cpu/smt/control")
	ksm := filepath.Join(sys, "kernel/mm/ksm/run")
	iou := filepath.Join(procSys, "kernel/io_uring_disabled")
	group := filepath.Join(procSys, "kernel/io_uring_group")
	os.MkdirAll(filepath.Dir(smt), 0o755)
	os.MkdirAll(filepath.Dir(ksm), 0o755)
	os.MkdirAll(filepath.Dir(iou), 0o755)
	os.WriteFile(smt, []byte("on\n"), 0o644)
	os.WriteFile(ksm, []byte("0\n"), 0o644)
	os.WriteFile(iou, []byte("2\n"), 0o644)
	c := Config{JailGID: 899999}
	if hostHardened(sys, procSys, c) == nil {
		t.Error("SMT on accepted")
	}
	os.WriteFile(smt, []byte("forceoff\n"), 0o644)
	if err := hostHardened(sys, procSys, c); err != nil {
		t.Error(err)
	}
	os.WriteFile(ksm, []byte("1\n"), 0o644)
	if hostHardened(sys, procSys, c) == nil {
		t.Error("KSM on accepted")
	}
	os.WriteFile(ksm, []byte("0\n"), 0o644)
	for _, tc := range []struct {
		disabled, group string
		ok              bool
	}{
		{"2", "", true}, {"1", "899999", true}, {"1", "-1", true},
		{"1", "100", false}, {"0", "899999", false}, {"", "", false},
	} {
		os.Remove(iou)
		os.Remove(group)
		if tc.disabled != "" {
			os.WriteFile(iou, []byte(tc.disabled+"\n"), 0o644)
		}
		if tc.group != "" {
			os.WriteFile(group, []byte(tc.group+"\n"), 0o644)
		}
		if err := hostHardened(sys, procSys, c); (err == nil) != tc.ok {
			t.Errorf("io_uring_disabled=%q group=%q: %v, want ok=%v", tc.disabled, tc.group, err, tc.ok)
		}
	}
}

func TestJailGIDShared(t *testing.T) {
	m := &Manager{cfg: Config{Jailer: "/bin/jailer", JailUIDBase: 900000}}
	if u, g := m.ids(VM{Index: 7}); u != 900007 || g != 900007 {
		t.Errorf("per-VM ids: %d %d", u, g)
	}
	m.cfg.JailGID = 899999
	if u, g := m.ids(VM{Index: 7}); u != 900007 || g != 899999 {
		t.Errorf("shared gid: %d %d", u, g)
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

// A dump that fails (team quota: EDQUOT on the memory file) keeps the VM and its
// memory: the stop answers 507, the guest runs on, and a retry snapshots it.
func TestFailedDumpKeepsVMRunning(t *testing.T) {
	m, h := newTestManager(t, nil)
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	var v VM
	call(t, srv, "POST", "/v1/vms", map[string]any{"team": "t1"}, &v)
	h.mu.Lock()
	h.failSnap = 1
	h.mu.Unlock()
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/stop", nil, nil); c != http.StatusInsufficientStorage {
		t.Fatalf("stop with a failed dump: %d, want 507", c)
	}
	call(t, srv, "GET", "/v1/vms/"+v.ID, nil, &v)
	fc := h.fcOf(t, m, v.ID)
	if v.State != Running || fc.called("agent shutdown") {
		t.Fatalf("a failed dump shut the VM down: %+v", v)
	}
	fc.mu.Lock()
	paused := fc.paused
	fc.mu.Unlock()
	if paused {
		t.Fatal("a failed dump left the VM paused")
	}
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/stop", nil, &v); c != 200 || v.State != Stopped || !v.HasSnapshot {
		t.Fatalf("retry: %d %+v", c, v)
	}
	if c := call(t, srv, "POST", "/v1/vms/"+v.ID+"/start", nil, &v); c != 200 || !h.fcOf(t, m, v.ID).called("PUT /snapshot/load") {
		t.Fatalf("start after the retried stop did not resume: %d %+v", c, v)
	}
}

// boatd exiting cannot wait for a retry: a failed dump falls back to a cold shutdown.
func TestFailedDumpOnShutdownStopsCold(t *testing.T) {
	m, h := newTestManager(t, nil)
	v, err := m.Create(context.Background(), CreateReq{})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.failSnap = 1
	h.mu.Unlock()
	if got, err := m.Stop(context.Background(), v.ID, shutdownReason); err != nil || got.State != Stopped || got.HasSnapshot {
		t.Fatalf("%+v %v", got, err)
	}
}
