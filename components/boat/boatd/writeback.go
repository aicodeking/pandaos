package main

import (
	"os"
	"sync"
	"time"
)

// The memory dump Firecracker writes (3.8 GiB in the e2e perf VM) is left to
// writeback when the snapshot is not fsynced. boat-e2e 43328227157 saw the next
// new-from-snapshot wait 6.2 s in its boot (7.39 s, gate 5 s) behind those dirty
// pages, with quotas on; 0.56-3.19 s in the 15 runs before. Starting writeback
// while the dump is still being written leaves fewer dirty pages behind it.
const writebackEvery = 250 * time.Millisecond

// startWriteback starts writeback of path (once it exists) every writebackEvery
// until stop, which kicks it once more. Best effort: errors only mean no head start.
func startWriteback(path string) (stop func()) {
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(writebackEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				kickFile(path)
				return
			case <-t.C:
				kickFile(path)
			}
		}
	}()
	return func() { close(done); <-finished }
}

func kickFile(path string) {
	f, err := openJail(path, os.O_RDONLY)
	if err != nil {
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Mode().IsRegular() {
		kickWriteback(f)
	}
}

// A dump's writeback (2 GiB+, the merge then fsyncs it) fills a slow disk's queue, and a VM
// that boots meanwhile waits for its boot reads behind it: a cold create took 7.7-12.6 s
// instead of 2.3-4.3 on the CI runner (loop file on a throttled cloud disk; r11, r12). While
// any VM boots, every dumping VM's writes are capped with io.max on stateDir's disk. r12's
// probe, a create 1 s into a 2 GiB dump, 8 tries each: uncapped 2.92-8.47 s (3 slow), the
// dump capped at 150 MiB/s for all of it 3.14-3.50 s (dump 16.1-16.7 s instead of 13.1-18.0),
// at 50 MiB/s 2.88-3.29 s but a 44 s dump. Capping only during boots keeps a dump with nobody
// booting at full speed. On the AX42's NVMe an 8 GiB writeback moved another cgroup's 4k reads
// from 56 to 62 us (bench/boatbench/results/ax42-r12-2026-11-13): there the cap costs a dump a
// few hundred ms per boot and changes nothing else.
const dumpWbpsWhileBooting = 150 << 20

// dumpIO tracks dumping VMs and booting ones, under its own lock (taken inside m.mu never).
type dumpIO struct {
	mu      sync.Mutex
	booting int
	next    uint64
	dumping map[string]uint64
}

// dumpStarted: v's memory is being dumped (liftMemLimits). Capped at once if a VM boots.
func (m *Manager) dumpStarted(id string) uint64 {
	m.dio.mu.Lock()
	defer m.dio.mu.Unlock()
	if m.dio.dumping == nil {
		m.dio.dumping = map[string]uint64{}
	}
	m.dio.next++
	m.dio.dumping[id] = m.dio.next
	if m.dio.booting > 0 {
		m.capDump(id, true)
	}
	return m.dio.next
}

func (m *Manager) dumpGeneration(id string) uint64 {
	m.dio.mu.Lock()
	defer m.dio.mu.Unlock()
	return m.dio.dumping[id]
}

// dumpDone: v's dump is on disk (setMemLimits, or a stop's flush). Its cap goes.
func (m *Manager) dumpDone(id string) {
	m.dio.mu.Lock()
	defer m.dio.mu.Unlock()
	if m.dio.dumping[id] != 0 {
		delete(m.dio.dumping, id)
		m.capDump(id, false)
	}
}

// bootIO marks a VM boot until the returned func: dumps are capped meanwhile.
func (m *Manager) bootIO() (done func()) {
	m.dio.mu.Lock()
	defer m.dio.mu.Unlock()
	if m.dio.booting++; m.dio.booting == 1 {
		for id := range m.dio.dumping {
			m.capDump(id, true)
		}
	}
	return func() {
		m.dio.mu.Lock()
		defer m.dio.mu.Unlock()
		if m.dio.booting--; m.dio.booting == 0 {
			for id := range m.dio.dumping {
				m.capDump(id, false)
			}
		}
	}
}

func (m *Manager) capDump(id string, on bool) {
	limit := int64(0)
	if on {
		limit = dumpWbpsWhileBooting
	}
	if val, _ := m.cfg.ioMax(limit); val != "" {
		m.writeCgroup(VM{ID: id}, "io.max", val)
	}
}
