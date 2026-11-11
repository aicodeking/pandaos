package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---- vCPU threads apart from the VMM thread ----
//
// Firecracker runs all block and net emulation on one VMM thread (fc_vmm). In
// the VM's cgroup that thread is one of vCPUs+1 equal threads, so on a host
// with fewer CPUs than busy threads (SMT off: 2 CPUs for a 4 vCPU VM) it gets
// 1/(vCPUs+1) of them while each guest I/O waits on it. boatd moves the vCPU
// threads (fc_vcpu N) into a threaded child cgroup, <vm>/vcpu, with
// cpu.weight vcpuCPUWeight. The VMM, API and other threads stay in the VM's
// cgroup, where each competes with the vCPUs as a group (a nice-0 thread
// weighs about cpu.weight 100), as libvirt splits emulator from vcpu threads.
// The VM's cpu.max still caps the total; memory, io and pids stay on the VM's
// cgroup, the threaded domain.

const vcpuCgroup = "vcpu"

var procRoot = "/proc"

// vmThreads lists the threads of pid as tid -> comm.
func vmThreads(pid int) (map[int]string, error) {
	dir := filepath.Join(procRoot, strconv.Itoa(pid), "task")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[int]string{}
	for _, e := range ents {
		tid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "comm"))
		if err != nil {
			continue // exited meanwhile
		}
		out[tid] = strings.TrimSpace(string(b))
	}
	return out, nil
}

// splitVCPUs moves pid's vCPU threads into <vm>/vcpu. Errors are logged by the
// caller: a VM whose threads stay together still runs, as before.
func (m *Manager) splitVCPUs(v VM, pid int) error {
	w := m.cfg.VCPUCPUWeight
	if w == 0 || !m.cfg.cgroupLimits || m.cfg.CgroupParent == "" || pid <= 0 {
		return nil
	}
	vmcg := filepath.Join(cgroupRoot, m.cfg.CgroupParent, v.ID)
	cg := filepath.Join(vmcg, vcpuCgroup)
	threads, err := vmThreads(pid)
	if err != nil {
		return err
	}
	var vcpus []int
	for tid, comm := range threads {
		if strings.HasPrefix(comm, "fc_vcpu") {
			vcpus = append(vcpus, tid)
		}
	}
	if len(vcpus) == 0 {
		return fmt.Errorf("no fc_vcpu threads in pid %d", pid)
	}
	sort.Ints(vcpus)
	if err := os.Mkdir(cg, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	steps := [][2]string{
		{filepath.Join(cg, "cgroup.type"), "threaded"}, // the VM's cgroup becomes its threaded domain
		{filepath.Join(vmcg, "cgroup.subtree_control"), "+cpu"},
		{filepath.Join(cg, "cpu.weight"), strconv.Itoa(w)},
	}
	for _, s := range steps {
		if err := os.WriteFile(s[0], []byte(s[1]), 0); err != nil {
			return fmt.Errorf("%s: %w", strings.TrimPrefix(s[0], cgroupRoot+"/"), err)
		}
	}
	for _, tid := range vcpus {
		if err := os.WriteFile(filepath.Join(cg, "cgroup.threads"), []byte(strconv.Itoa(tid)), 0); err != nil {
			return fmt.Errorf("vcpu thread %d: %w", tid, err)
		}
	}
	return nil
}

// rmVCPUCgroup removes <vm>/vcpu once the VM's threads are gone, so that the
// jailer's cgroup can go too.
func (m *Manager) rmVCPUCgroup(id string) {
	if m.cfg.CgroupParent == "" {
		return
	}
	os.Remove(filepath.Join(cgroupRoot, m.cfg.CgroupParent, id, vcpuCgroup))
}

// ---- cpuTraceS: where a VM's CPU time goes ----
//
// Every cpuTraceS seconds boatd logs, per running VM, each Firecracker thread
// kind's share of one CPU over the interval and the VM cgroup's pressure (PSI
// "some": the share of time at least one of its threads waited for CPU,
// memory or I/O). A diagnostic, off by default.

type cpuSample struct {
	at      time.Time
	ticks   map[string]float64 // thread kind -> CPU seconds
	psi     map[string]float64 // cpu/memory/io -> "some" total, seconds
	throttl float64            // cpu.stat throttled_usec, seconds
}

var clkTck = 100.0 // USER_HZ on every Linux boatd runs on

func threadKind(comm string) string {
	switch {
	case strings.HasPrefix(comm, "fc_vcpu"):
		return strings.ReplaceAll(comm, " ", "")
	case comm == "fc_vmm", comm == "fc_api":
		return comm
	case comm == "firecracker":
		return "fc_vmm" // the main thread is the VMM thread
	}
	return "other"
}

func sampleVM(pid int, cg string) (cpuSample, error) {
	s := cpuSample{at: time.Now(), ticks: map[string]float64{}, psi: map[string]float64{}}
	threads, err := vmThreads(pid)
	if err != nil {
		return s, err
	}
	for tid, comm := range threads {
		b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "task", strconv.Itoa(tid), "stat"))
		if err != nil {
			continue
		}
		f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
		if len(f) < 13 { // state is f[0]; utime, stime are fields 14, 15 of stat
			continue
		}
		u, _ := strconv.ParseFloat(f[11], 64)
		st, _ := strconv.ParseFloat(f[12], 64)
		s.ticks[threadKind(comm)] += (u + st) / clkTck
	}
	for _, r := range []string{"cpu", "memory", "io"} {
		s.psi[r] = psiSomeTotal(filepath.Join(cg, r+".pressure"))
	}
	s.throttl = statField(filepath.Join(cg, "cpu.stat"), "throttled_usec") / 1e6
	return s, nil
}

func psiSomeTotal(path string) float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, kv := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(kv, "total="); ok {
				n, _ := strconv.ParseFloat(v, 64)
				return n / 1e6
			}
		}
	}
	return 0
}

func statField(path, key string) float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, " "); ok && k == key {
			n, _ := strconv.ParseFloat(v, 64)
			return n
		}
	}
	return 0
}

// traceLine renders the interval a..b: "fc_vmm=91% fc_vcpu0=40% ... psi cpu=35% memory=0% io=12% throttled=0%".
func traceLine(a, b cpuSample) string {
	dt := b.at.Sub(a.at).Seconds()
	if dt <= 0 {
		return ""
	}
	pct := func(x float64) string { return strconv.Itoa(int(x/dt*100+0.5)) + "%" }
	kinds := make([]string, 0, len(b.ticks))
	for k := range b.ticks {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	var sb strings.Builder
	for _, k := range kinds {
		fmt.Fprintf(&sb, "%s=%s ", k, pct(b.ticks[k]-a.ticks[k]))
	}
	fmt.Fprintf(&sb, "psi cpu=%s memory=%s io=%s throttled=%s",
		pct(b.psi["cpu"]-a.psi["cpu"]), pct(b.psi["memory"]-a.psi["memory"]), pct(b.psi["io"]-a.psi["io"]), pct(b.throttl-a.throttl))
	return sb.String()
}

// cpuTrace logs traceLine for each running VM every cpuTraceS seconds, until stop closes.
func (m *Manager) cpuTrace(stop <-chan struct{}) {
	if m.cfg.CPUTraceS <= 0 {
		return
	}
	prev := map[string]cpuSample{}
	t := time.NewTicker(time.Duration(m.cfg.CPUTraceS) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		next := map[string]cpuSample{}
		for _, v := range m.list("") {
			if v.State != Running || v.PID <= 0 {
				continue
			}
			s, err := sampleVM(v.PID, filepath.Join(cgroupRoot, m.cfg.CgroupParent, v.ID))
			if err != nil {
				continue
			}
			key := v.ID + "/" + strconv.Itoa(v.PID)
			if p, ok := prev[key]; ok {
				log.Printf("cputrace %s: %s", v.ID, traceLine(p, s))
			}
			next[key] = s
		}
		prev = next
	}
}
