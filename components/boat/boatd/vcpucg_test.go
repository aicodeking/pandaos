package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// fakeProc writes /proc/<pid>/task/<tid>/{comm,stat} under a temp procRoot.
func fakeProc(t *testing.T, pid int, threads map[string]string) {
	t.Helper()
	old := procRoot
	procRoot = t.TempDir()
	t.Cleanup(func() { procRoot = old })
	for tid, comm := range threads {
		d := filepath.Join(procRoot, strconv.Itoa(pid), "task", tid)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "comm"), []byte(comm+"\n"), 0o644)
		// utime 150, stime 50 ticks: 2 s of CPU
		os.WriteFile(filepath.Join(d, "stat"), []byte(tid+" ("+comm+") S 1 1 1 0 -1 0 0 0 0 0 150 50 0 0 20 0 1 0"), 0o644)
	}
}

func TestSplitVCPUs(t *testing.T) {
	old := cgroupRoot
	cgroupRoot = t.TempDir()
	defer func() { cgroupRoot = old }()
	fakeProc(t, 4242, map[string]string{"4242": "firecracker", "4243": "fc_api", "4244": "fc_vcpu 0", "4245": "fc_vcpu 1"})
	m, _ := newTestManager(t, func(c *Config) { c.CgroupParent, c.cgroupLimits = "boat", true })
	vmcg := filepath.Join(cgroupRoot, "boat", "vm-a")
	os.MkdirAll(filepath.Join(vmcg, "vcpu"), 0o755) // cgroupfs makes the files; the fake needs them writable
	for _, f := range []string{"vcpu/cgroup.type", "cgroup.subtree_control", "vcpu/cpu.weight", "vcpu/cgroup.threads"} {
		os.WriteFile(filepath.Join(vmcg, f), nil, 0o644)
	}
	if err := m.splitVCPUs(VM{ID: "vm-a"}, 4242); err != nil {
		t.Fatal(err)
	}
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(vmcg, p)); return string(b) }
	// A file write replaces: the fake keeps the last thread; the kernel takes each.
	if read("vcpu/cgroup.type") != "threaded" || read("cgroup.subtree_control") != "+cpu" || read("vcpu/cpu.weight") != "100" || read("vcpu/cgroup.threads") != "4245" {
		t.Fatalf("type %q subtree %q weight %q threads %q", read("vcpu/cgroup.type"), read("cgroup.subtree_control"), read("vcpu/cpu.weight"), read("vcpu/cgroup.threads"))
	}
	m.rmVCPUCgroup("vm-a") // not empty in the fake: rmdir fails like the kernel's on a populated cgroup
	m.cfg.VCPUCPUWeight = 0
	os.RemoveAll(filepath.Join(vmcg, "vcpu"))
	if err := m.splitVCPUs(VM{ID: "vm-a"}, 4242); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(vmcg, "vcpu")); err == nil {
		t.Fatal("vcpuCpuWeight 0 still split the threads")
	}
	m.cfg.VCPUCPUWeight = 100
	fakeProc(t, 4242, map[string]string{"4242": "firecracker"})
	if err := m.splitVCPUs(VM{ID: "vm-a"}, 4242); err == nil {
		t.Fatal("no vCPU threads: no error")
	}
}

func TestCPUTraceLine(t *testing.T) {
	fakeProc(t, 7, map[string]string{"7": "fc_vmm", "8": "fc_vcpu 0", "9": "fc_api"})
	cg := t.TempDir()
	os.WriteFile(filepath.Join(cg, "cpu.pressure"), []byte("some avg10=0.00 avg60=0.00 avg300=0.00 total=500000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n"), 0o644)
	os.WriteFile(filepath.Join(cg, "cpu.stat"), []byte("usage_usec 10\nthrottled_usec 0\n"), 0o644)
	a, err := sampleVM(7, cg)
	if err != nil || a.ticks["fc_vmm"] != 2 || a.ticks["fc_vcpu0"] != 2 || a.psi["cpu"] != 0.5 {
		t.Fatalf("%+v %v", a, err)
	}
	b := a
	b.at = a.at.Add(2 * time.Second)
	b.ticks = map[string]float64{"fc_vmm": 3.8, "fc_vcpu0": 2.5, "fc_api": 2}
	b.psi = map[string]float64{"cpu": 1.5, "memory": 0.5, "io": 0.5}
	got := traceLine(a, b)
	if got != "fc_api=0% fc_vcpu0=25% fc_vmm=90% psi cpu=50% memory=25% io=25% throttled=0%" {
		t.Fatal(got)
	}
	if threadKind("firecracker") != "fc_vmm" || threadKind("fc_vcpu 12") != "fc_vcpu12" || threadKind("kworker") != "other" {
		t.Fatal("threadKind")
	}
}
