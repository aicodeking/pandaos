package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestFailedStopReleasesDumpLimits(t *testing.T) {
	for _, phase := range []string{"write", "install"} {
		t.Run(phase, func(t *testing.T) {
			oldRoot := cgroupRoot
			cgroupRoot = t.TempDir()
			defer func() { cgroupRoot = oldRoot }()
			m, h := newTestManager(t, func(c *Config) { c.CgroupParent = "p" })
			v, err := m.Create(context.Background(), CreateReq{})
			if err != nil {
				t.Fatal(err)
			}
			m.cfg.cgroupLimits, m.cfg.stateDisk = true, "259:0"
			cg := filepath.Join(cgroupRoot, "p", v.ID)
			if err := os.MkdirAll(cg, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{"memory.max", "memory.high", "io.max"} {
				if err := os.WriteFile(filepath.Join(cg, f), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "write" {
				h.mu.Lock()
				h.failSnap = 1
				h.mu.Unlock()
			} else {
				stage := filepath.Join(m.vmDir(v.ID), memStage)
				if err := os.Mkdir(stage, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(stage, "block-rename"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			bootDone := m.bootIO()
			defer bootDone()
			if got, err := m.Stop(context.Background(), v.ID, ""); err == nil || got.State != Running {
				t.Fatalf("failed %s must keep the VM running: %+v %v", phase, got, err)
			}
			if generation := m.dumpGeneration(v.ID); generation != 0 {
				t.Errorf("failed %s still owns dump generation %d", phase, generation)
			}
			high, hard := m.cfg.memLimits(v)
			for f, want := range map[string]string{
				"memory.high": strconv.FormatInt(high, 10), "memory.max": strconv.FormatInt(hard, 10),
				"io.max": "259:0 rbps=max wbps=max riops=max wiops=max",
			} {
				if got, err := os.ReadFile(filepath.Join(cg, f)); err != nil || string(got) != want {
					t.Errorf("failed %s: %s=%q, want %q: %v", phase, f, got, want, err)
				}
			}
		})
	}
}

func TestConcurrentDumpCleanupOwnership(t *testing.T) {
	m := &Manager{cfg: Config{StateDir: t.TempDir()}, locks: map[string]*sync.Mutex{}}
	var wg sync.WaitGroup
	for i := range 8 {
		v := VM{ID: "vm-" + strconv.Itoa(i)}
		if err := os.MkdirAll(m.root(v.ID), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(m.root(v.ID), memFile), []byte("mem"), 0o600); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				unlock := m.lock(v.ID)
				first := m.liftMemLimits(v)
				m.setMemLimits(v) // completion must not allow this generation to be reused
				second := m.liftMemLimits(v)
				bootDone := m.bootIO()
				finished := make(chan struct{})
				go func() { m.settleAfterDump(v, first); close(finished) }()
				unlock()
				<-finished
				if got := m.dumpGeneration(v.ID); got != second || second <= first {
					t.Errorf("%s: stale generation %d reset current generation %d to %d", v.ID, first, second, got)
				}
				unlock = m.lock(v.ID)
				m.setMemLimits(v)
				unlock()
				bootDone()
			}
		}()
	}
	wg.Wait()
	if m.dio.booting != 0 || len(m.dio.dumping) != 0 {
		t.Fatalf("unfinished I/O tracking: %d boots, %v dumps", m.dio.booting, m.dio.dumping)
	}
}

func TestStaleDumpCleanupKeepsNewDumpLimits(t *testing.T) {
	for _, missingMem := range []bool{false, true} {
		t.Run(strconv.FormatBool(missingMem), func(t *testing.T) {
			oldRoot := cgroupRoot
			cgroupRoot = t.TempDir()
			defer func() { cgroupRoot = oldRoot }()
			m, _ := newTestManager(t, func(c *Config) { c.CgroupParent = "p" })
			<-m.imagesReady
			m.cfg.cgroupLimits, m.cfg.stateDisk = true, "259:0"
			v := VM{ID: "vm-overlap", MemMiB: 1024}
			cg := filepath.Join(cgroupRoot, "p", v.ID)
			if err := os.MkdirAll(cg, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{"memory.max", "memory.high", "io.max"} {
				if err := os.WriteFile(filepath.Join(cg, f), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if !missingMem {
				if err := os.MkdirAll(m.root(v.ID), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(m.root(v.ID), memFile), []byte("old dump"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			unlock := m.lock(v.ID)
			first := m.liftMemLimits(v)
			finished := make(chan struct{})
			go func() {
				m.settleAfterDump(v, first)
				close(finished)
			}()
			// The old cleanup cannot take the VM lock until a newer dump owns the limits.
			second := m.liftMemLimits(v)
			bootDone := m.bootIO()
			unlock()
			<-finished
			read := func(f string) string {
				b, err := os.ReadFile(filepath.Join(cg, f))
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			if got := read("io.max"); got != "259:0 rbps=max wbps=157286400 riops=max wiops=max" {
				t.Errorf("stale cleanup uncapped the active dump: %s", got)
			}
			_, hard := m.cfg.memLimits(v)
			lifted := strconv.FormatInt(hard+int64(v.MemMiB)<<20, 10)
			for _, f := range []string{"memory.high", "memory.max"} {
				if got := read(f); got != lifted {
					t.Errorf("stale cleanup lowered %s to %s, want %s", f, got, lifted)
				}
			}
			bootDone()
			m.settleAfterDump(v, second)
			if len(m.dio.dumping) != 0 || read("memory.max") != strconv.FormatInt(hard, 10) {
				t.Fatal("current dump did not clean up")
			}
		})
	}
}

// The dump appears after the snapshot call starts; a planted link is not followed;
// stop returns once the last kick is done.
func TestStartWriteback(t *testing.T) {
	dir := t.TempDir()
	mem := filepath.Join(dir, memNew)
	stop := startWriteback(mem)
	time.Sleep(writebackEvery + 50*time.Millisecond) // no file yet
	if err := os.WriteFile(mem, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(writebackEvery + 50*time.Millisecond)
	t0 := time.Now()
	stop()
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("stop took %v", d)
	}
	link := filepath.Join(dir, "link")
	os.Symlink(mem, link)
	if f, err := openJail(link, os.O_RDONLY); err == nil {
		f.Close()
		t.Fatal("followed a link")
	}
}

// A dump is capped only while some VM boots, from whichever starts second, and its
// configured io.max comes back after.
func TestDumpCappedWhileAVMBoots(t *testing.T) {
	cgroupRoot = t.TempDir()
	defer func() { cgroupRoot = "/sys/fs/cgroup" }()
	m := &Manager{cfg: Config{cgroupLimits: true, stateDisk: "259:0", CgroupParent: "p", IOMaxIOPS: 1000}}
	for _, id := range []string{"a", "b"} { // the kernel's files exist; writeCgroup does not create them readable
		os.MkdirAll(filepath.Join(cgroupRoot, "p", id), 0o755)
		os.WriteFile(filepath.Join(cgroupRoot, "p", id, "io.max"), nil, 0o644)
	}
	ioMax := func(id string) string {
		b, _ := os.ReadFile(filepath.Join(cgroupRoot, "p", id, "io.max"))
		return string(b)
	}
	const capped, open = "259:0 rbps=max wbps=157286400 riops=1000 wiops=1000", "259:0 rbps=max wbps=max riops=1000 wiops=1000"
	m.dumpStarted("a")
	if ioMax("a") != "" {
		t.Fatalf("capped with nobody booting: %q", ioMax("a"))
	}
	done1, done2 := m.bootIO(), m.bootIO()
	if ioMax("a") != capped {
		t.Fatalf("dump while booting: %q", ioMax("a"))
	}
	m.dumpStarted("b") // starts during a boot
	done1()
	if ioMax("a") != capped || ioMax("b") != capped {
		t.Fatalf("one boot still runs: %q %q", ioMax("a"), ioMax("b"))
	}
	m.dumpDone("a")
	if ioMax("a") != open {
		t.Fatalf("a's dump is done: %q", ioMax("a"))
	}
	done2()
	if ioMax("b") != open || len(m.dio.dumping) != 1 {
		t.Fatalf("no boot left: %q, dumping %v", ioMax("b"), m.dio.dumping)
	}
	m.dumpDone("b")
	m.dumpDone("never") // no dump: nothing written
	if _, err := os.Stat(filepath.Join(cgroupRoot, "p", "never")); err == nil || len(m.dio.dumping) != 0 {
		t.Fatalf("dumping %v", m.dio.dumping)
	}
}

func TestIOMaxLine(t *testing.T) {
	for _, c := range []struct {
		cfg     Config
		cap     int64
		want    string
		limited bool
	}{
		{Config{cgroupLimits: true, stateDisk: "8:0"}, 0, "8:0 rbps=max wbps=max riops=max wiops=max", false},
		{Config{cgroupLimits: true, stateDisk: "8:0", IOMaxMBps: 800}, 0, "8:0 rbps=838860800 wbps=838860800 riops=max wiops=max", true},
		{Config{cgroupLimits: true, stateDisk: "8:0", IOMaxMBps: 800}, 150 << 20, "8:0 rbps=838860800 wbps=157286400 riops=max wiops=max", true},
		{Config{cgroupLimits: true, stateDisk: "8:0", IOMaxMBps: 100}, 150 << 20, "8:0 rbps=104857600 wbps=104857600 riops=max wiops=max", true}, // the lower stays
		{Config{stateDisk: "8:0", IOMaxMBps: 100}, 0, "", false},
		{Config{cgroupLimits: true, IOMaxMBps: 100}, 0, "", false},
	} {
		if got, lim := c.cfg.ioMax(c.cap); got != c.want || lim != c.limited {
			t.Errorf("%+v cap %d: %q %v, want %q %v", c.cfg, c.cap, got, lim, c.want, c.limited)
		}
	}
}
