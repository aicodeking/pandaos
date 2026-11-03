package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestDiskEngine(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if f := h.fcOf(t, m, v.ID); !f.called("engine Async") || f.called("engine Sync") {
		t.Fatal("auto must ask for Async")
	}
	h.noAsync = true
	v, err = m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatalf("auto must fall back to Sync: %v", err)
	}
	if !h.fcOf(t, m, v.ID).called("engine Sync") {
		t.Fatal("no Sync after a refused Async")
	}
	m.cfg.DiskEngine = "Async"
	if _, err := m.Create(ctx, CreateReq{Team: "t"}); err == nil {
		t.Fatal("an explicit Async must not fall back")
	}
}

func TestDefaultsDoNotThrottleDisk(t *testing.T) {
	c := defaultConfig()
	if c.DiskMBps != 0 || c.DiskIOPS != 0 {
		t.Fatalf("drive limiter by default: %d MB/s, %d IOPS", c.DiskMBps, c.DiskIOPS)
	}
	if c.KSMTeam != "" || c.WarmPool {
		t.Fatal("KSM and warmPool must be opt-in")
	}
	if c.MemOvercommit > 1.5 {
		t.Fatalf("memOvercommit default %v", c.MemOvercommit)
	}
}

func TestBalloonDevice(t *testing.T) {
	m, h := newTestManager(t, nil)
	v, err := m.Create(context.Background(), CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if !h.fcOf(t, m, v.ID).called("PUT /balloon") {
		t.Fatal("no balloon on a cold boot")
	}
	m2, h2 := newTestManager(t, func(c *Config) { c.Balloon = false })
	v2, err := m2.Create(context.Background(), CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if h2.fcOf(t, m2, v2.ID).called("PUT /balloon") {
		t.Fatal("balloon although balloon=false")
	}
}

func TestRealMemoryAdmission(t *testing.T) {
	m, _ := newTestManager(t, func(c *Config) {
		c.HostMemMiB, c.HostReserveMiB, c.MemOvercommit = 16384, 1024, 2
	})
	ctx := context.Background()
	// test size: see newTestManager. need = reserve + RAM/2.
	s := m.cfg.Sizes[m.cfg.DefaultSize]
	need := 1024 + s.MemMiB/2
	m.memAvail = func() int { return need - 1 }
	var ae *apiErr
	if _, err := m.Create(ctx, CreateReq{Team: "t"}); !errors.As(err, &ae) || ae.code != 503 {
		t.Fatalf("below real memory: %v", err)
	}
	m.memAvail = func() int { return need }
	if _, err := m.Create(ctx, CreateReq{Team: "t"}); err != nil {
		t.Fatal(err)
	}
	m.cfg.MemOvercommit = 1 // RAM is fully reserved: nothing more to check
	m.memAvail = func() int { return 1 }
	if err := m.realMemCheck(s); err != nil {
		t.Fatal(err)
	}
}

func TestKSMScope(t *testing.T) {
	var c Config
	if c.ksmFor("a") {
		t.Fatal("KSM on by default")
	}
	c.KSMTeam = "a"
	if !c.ksmFor("a") || c.ksmFor("b") {
		t.Fatal("KSM must cover exactly one team")
	}
	c.KSMTeam = "*"
	if !c.ksmFor("b") {
		t.Fatal("* covers everyone")
	}
}

func TestWarmTemplate(t *testing.T) {
	m, h := newTestManager(t, func(c *Config) { c.WarmPool = true })
	m.warmSettle = 0
	ctx := context.Background()
	img, size := m.cfg.DefaultImage, m.cfg.DefaultSize
	a, err := m.Create(ctx, CreateReq{Team: "t", Env: map[string]string{"A": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if h.fcOf(t, m, a.ID).called("PUT /snapshot/load") {
		t.Fatal("no template yet: the first create boots cold")
	}
	eventually(t, "template", func() bool { return m.warmTemplate(img, size) != "" })
	eventually(t, "builder deleted", func() bool { return len(m.list(warmTeam)) == 0 })
	b, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	f := h.fcOf(t, m, b.ID)
	if !f.called("PUT /snapshot/load") || !f.called("agent reseed") || !f.called("agent set_identity") {
		t.Fatal("a warm create must restore, reseed and set identity")
	}
	if b.Team != "t" || b.FromSnapshot != "" {
		t.Fatalf("warm VM: %+v", b)
	}
	// A changed image makes the template stale.
	if err := os.Chtimes(filepath.Join(m.imageDir(img), diskFile), b.CreatedAt, b.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if m.warmTemplate(img, size) != "" {
		t.Fatal("stale template used")
	}
	// Off: no template is ever used, even if one exists on disk.
	m.cfg.WarmPool = false
	if m.warmTemplate(img, size) != "" {
		t.Fatal("warmPool off")
	}
}

func TestUnsyncedSnapshotRestoresCold(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.CreateSnapshot(ctx, v.ID, "x")
	if err != nil || !s.HasMem {
		t.Fatalf("%+v %v", s, err)
	}
	eventually(t, "synced", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.snaps[s.ID].Unsynced
	})
	var disk Snapshot
	if err := readJSON(filepath.Join(m.snapDir(s.ID), "snap.json"), &disk); err != nil || disk.Unsynced {
		t.Fatalf("snap.json after the sync: %+v %v", disk, err)
	}
	// As after a host crash before the fsync finished.
	disk.Unsynced = true
	if err := writeJSONAtomic(filepath.Join(m.snapDir(s.ID), "snap.json"), disk, 0o600); err != nil {
		t.Fatal(err)
	}
	m2, err := newManager(m.cfg, h.run, h)
	if err != nil {
		t.Fatal(err)
	}
	got := m2.snaps[s.ID]
	if got.HasMem || got.Unsynced || exists(filepath.Join(m.snapDir(s.ID), memFile)) {
		t.Fatalf("unsynced snapshot kept its memory: %+v", got)
	}
	if !exists(filepath.Join(m.snapDir(s.ID), diskFile)) {
		t.Fatal("the disk must survive")
	}
}

func TestMemoryPressure(t *testing.T) {
	m, h := newTestManager(t, func(c *Config) { c.HostReserveMiB = 1024; c.HostMemMiB = 65536 })
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	f := h.fcOf(t, m, v.ID)
	m.memAvail = func() int { return 4096 }
	m.memoryPressure(ctx)
	if f.called("PATCH /balloon") {
		t.Fatal("balloon without pressure")
	}
	m.memAvail = func() int { return 512 }
	m.memoryPressure(ctx)
	m.memoryPressure(ctx)
	if !f.called("PATCH /balloon") || m.balloonPct != 2*balloonStepPct {
		t.Fatalf("no inflation under pressure (%d%%)", m.balloonPct)
	}
	m.memAvail = func() int { return 1500 } // above the reserve, below the hysteresis
	m.memoryPressure(ctx)
	if m.balloonPct == 0 {
		t.Fatal("deflated too early")
	}
	m.memAvail = func() int { return 2048 }
	m.memoryPressure(ctx)
	if m.balloonPct != 0 || m.ballooned {
		t.Fatalf("not released: %d%%", m.balloonPct)
	}
}

func TestSetClockFallsBackToExec(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Stop(ctx, v.ID, ""); err != nil {
		t.Fatal(err)
	}
	h.noSetClock = true
	if _, err = m.Start(ctx, v.ID, nil); err != nil {
		t.Fatal(err)
	}
	f := h.fcOf(t, m, v.ID)
	if !f.called("agent set_clock") || !f.called("agent exec") {
		t.Fatal("an old agent must get the exec fallback")
	}
}

func TestInterruptedTemplateBuildIsRemoved(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()
	v, err := m.createFrom(ctx, CreateReq{Team: warmTeam}, "")
	if err != nil {
		t.Fatal(err)
	}
	m.resumeAll(ctx)
	if _, err := m.get(v.ID); !errors.Is(err, errNotFound) {
		t.Fatalf("builder left behind: %v", err)
	}
}

func TestMemFileHardLinkedWithinTeamOnly(t *testing.T) {
	m, _ := newTestManager(t, func(c *Config) { c.WarmPool = true })
	m.warmSettle = 0
	ctx := context.Background()
	p, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	ino := func(path string) uint64 {
		var st syscall.Stat_t
		if err := syscall.Stat(path, &st); err != nil {
			t.Fatal(err)
		}
		return uint64(st.Ino)
	}
	eventually(t, "template", func() bool { return m.warmTemplate(p.Image, p.Size) != "" })
	sn, err := m.CreateSnapshot(ctx, p.ID, "x")
	if err != nil {
		t.Fatal(err)
	}
	snapMem := filepath.Join(m.snapDir(sn.ID), memFile)
	if st, _ := os.Stat(snapMem); st.Mode().Perm() != 0o444 {
		t.Fatalf("mem not sealed: %v", st.Mode())
	}
	if ino(filepath.Join(m.root(p.ID), memFile)) != ino(snapMem) {
		t.Fatal("snapshot mem is not the parent's inode")
	}
	s1, err := m.Create(ctx, CreateReq{Team: "t", Snapshot: sn.ID})
	if err != nil {
		t.Fatal(err)
	}
	if ino(filepath.Join(m.root(s1.ID), memFile)) != ino(snapMem) {
		t.Fatal("new-from-snapshot must link within the team")
	}
	w1, err := m.Create(ctx, CreateReq{Team: "a"})
	if err != nil {
		t.Fatal(err)
	}
	w2, err := m.Create(ctx, CreateReq{Team: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if ino(filepath.Join(m.root(w1.ID), memFile)) == ino(filepath.Join(m.root(w2.ID), memFile)) {
		t.Fatal("two teams share a memory inode: page cache would cross the tenant boundary")
	}
	// A later diff replaces the file; the snapshot keeps the old inode, sealed and unchanged.
	if _, err := m.Fork(ctx, p.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if ino(filepath.Join(m.root(p.ID), memFile)) == ino(snapMem) {
		t.Fatal("a diff must replace root/mem, never write it in place")
	}
}
