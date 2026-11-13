package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
