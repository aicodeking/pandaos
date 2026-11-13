package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// ---- btrfs NOCOW VM disks ----
//
// CoW turns random guest writes into one extent each (about 450k after fio's
// 15 s randwrite), and `cp --reflink` is O(extents): 4.5 s with the source VM
// paused (run 41573512017). NOCOW overwrites in place, so extents stay large.
// A reflink needs both files to agree on the flag, and it only sticks on an
// empty file, so the image is converted once and cp() carries the flag along.

const (
	fsIocGetFlags  = 0x80086601
	fsIocSetFlags  = 0x40086602
	fsIocFiemap    = 0xc020660b
	fsNoCowFlag    = 0x00800000
	fsNoCompFlag   = 0x00000400
	maxImageExtent = 1 << 20 // bytes per extent below which an image counts as fragmented
)

func fsFlags(f *os.File) (int32, error) {
	var v int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocGetFlags, uintptr(unsafe.Pointer(&v))); e != 0 {
		return 0, e
	}
	return v, nil
}

func isNoCow(path string) bool {
	f, err := openJail(path, os.O_RDONLY)
	if err != nil {
		return false
	}
	defer f.Close()
	v, err := fsFlags(f)
	return err == nil && v&fsNoCowFlag != 0
}

func setFsFlags(f *os.File, v int32) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocSetFlags, uintptr(unsafe.Pointer(&v))); e != 0 {
		return e
	}
	return nil
}

// createNoCow makes an empty NOCOW file at path. False where the filesystem has
// no such flag (ext4, tmpfs, APFS): the caller then keeps plain copies.
func createNoCow(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	v, err := fsFlags(f)
	if err != nil {
		return false
	}
	// Inherited from a noCompress dir. btrfs refuses NOCOW next to any compression
	// flag (EINVAL, checked against the old flags too), so it goes in its own call.
	if v&fsNoCompFlag != 0 {
		v &^= fsNoCompFlag
		if setFsFlags(f, v) != nil {
			return false
		}
	}
	if setFsFlags(f, v|fsNoCowFlag) != nil {
		return false
	}
	v, err = fsFlags(f)
	return err == nil && v&fsNoCowFlag != 0
}

// ---- no compression for VM and snapshot dirs ----
//
// A store mounted compress=zstd (the PandaOS install) compresses every memory dump
// on writeback: 128 KiB extents (16,814 for an 8 GiB VM's mem on the AX42, 109
// without), each one more for the next merge's reflink and the restore's reads to
// walk. FS_NOCOMP_FL on a dir is inherited by what is created in it. Not NOCOW:
// that drops checksums and would have to match every reflink source, while
// NOCOMP files reflink from and to anything. Best effort: other filesystems have
// no such flag and nothing to gain.
func noCompress(dir string) {
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	defer f.Close()
	v, err := fsFlags(f)
	if err != nil || v&(fsNoCompFlag|fsNoCowFlag) != 0 {
		return // done, or a NOCOW dir, which btrfs does not compress anyway
	}
	setFsFlags(f, v|fsNoCompFlag)
}

// extentCount: FIEMAP with no room for extents returns how many there are.
func extentCount(f *os.File) (uint32, error) {
	var m struct {
		start, length            uint64
		flags, mapped, count, rs uint32
	}
	m.length = ^uint64(0)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocFiemap, uintptr(unsafe.Pointer(&m))); e != 0 {
		return 0, e
	}
	return m.mapped, nil
}

// fragmented: an image whose reflink costs O(extents) for nothing: a hole at every
// zero block. 22,567 extents in a 3 GiB rootfs made a 152 ms FICLONE on every create
// and disk snapshot (AX42); a dense copy has 4 and clones in 0.1 ms. Holes are not
// kept even where the source has them: zstd -d writes one per zero run, and
// `cp --sparse=auto` of that gave the runner's image a 31 ms clone (boat-e2e
// 43264949029; 15 extents with the AX42's cp). The cost is the zeros: 0.2 GiB of 3.
// ponytail: an image with more real holes than that is converted again at each start.
func fragmented(extents uint32, size int64) bool {
	return extents > 64 && int64(extents) > size/maxImageExtent
}

func isFragmented(path string) bool {
	n, size := extentsOf(path)
	return fragmented(n, size)
}

func extentsOf(path string) (uint32, int64) {
	f, err := openJail(path, os.O_RDONLY)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, 0
	}
	n, _ := extentCount(f)
	return n, st.Size()
}

// copyDense writes all of src into dst, holes as zeros, through plain buffered
// writes: no copy_file_range or splice, whose extents depend on the kernel and cp.
func copyDense(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer out.Close()
	// The wrappers hide ReaderFrom/WriterTo, which would take the copy_file_range path.
	if _, err := io.CopyBuffer(struct{ io.Writer }{out}, struct{ io.Reader }{in}, make([]byte, 8<<20)); err != nil {
		return err
	}
	return out.Close()
}

// convertImages rewrites each image's rootfs as a dense NOCOW file, once per image
// (and once more for one an older boatd left fragmented, see fragmented).
// Failure keeps the CoW image: copies of it stay CoW and still work.
//
// Each copy is flushed before it replaces the image, and creates wait for that
// (imagesReady): left to writeback, 8.5 GiB of fresh image data made the first VM
// stops after startup 6-7 s instead of under 1 s (boat-e2e 41652556991, 41669516359:
// stop_cli right after the conversion), and a crash before writeback could leave a
// renamed, unwritten image.
func (m *Manager) convertImages() {
	dirs, _ := filepath.Glob(filepath.Join(m.cfg.StateDir, "images", "*", diskFile))
	for _, disk := range dirs {
		if isNoCow(disk) && !isFragmented(disk) {
			continue
		}
		tmp := disk + ".nocow"
		if !createNoCow(tmp) {
			os.Remove(tmp)
			log.Printf("images: %s stays CoW: no NOCOW on this filesystem", disk)
			return
		}
		t0 := time.Now()
		before, _ := extentsOf(disk)
		st, err := os.Stat(disk)
		if err == nil {
			err = copyDense(disk, tmp)
		}
		if err == nil {
			err = os.Chmod(tmp, st.Mode().Perm())
		}
		t1 := time.Now()
		if err == nil {
			err = syncPath(tmp)
		}
		if err == nil {
			err = os.Rename(tmp, disk)
		}
		if err == nil {
			err = syncPath(filepath.Dir(disk))
		}
		if err != nil {
			os.Remove(tmp)
			log.Printf("images: %s stays CoW: %v", disk, err)
			continue
		}
		after, _ := extentsOf(disk)
		log.Printf("images: %s converted to NOCOW in %dms (flush %dms), extents %d -> %d", disk,
			time.Since(t0).Milliseconds(), time.Since(t1).Milliseconds(), before, after)
	}
}

// ---- cgroup memory limits ----

func (c Config) memLimits(v VM) (high, hard int64) {
	high = int64(v.MemMiB+c.VMOverheadMiB) << 20
	return high, high + int64(c.VMPageCacheMiB)<<20
}

func (m *Manager) writeCgroup(v VM, file, val string) {
	err := os.WriteFile(filepath.Join(cgroupRoot, m.cfg.CgroupParent, v.ID, file), []byte(val), 0)
	if err != nil && !errors.Is(err, fs.ErrNotExist) { // deleted meanwhile
		log.Printf("%s: %s: %v", v.ID, file, err)
	}
}

// liftMemLimits for a memory dump. Its page cache is charged to the paused VM, which
// sits near memory.high: dumping 3.8 GiB took 22 s of reclaim, throttled to the
// disk's speed (run 41584046201). A dump is at most the VM's RAM, so the limit moves up
// by that and no further: a resumed guest may write to its disk during the window, and
// a tenant can keep the window open by snapshotting in a loop.
func (m *Manager) liftMemLimits(v VM) {
	m.dumpStarted(v.ID)
	if m.cfg.cgroupLimits {
		_, hard := m.cfg.memLimits(v)
		limit := strconv.FormatInt(hard+int64(v.MemMiB)<<20, 10)
		m.writeCgroup(v, "memory.max", limit)
		m.writeCgroup(v, "memory.high", limit)
	}
}

// memMaxWait bounds how long setMemLimits waits for a VM's usage to fit under memory.max.
var memMaxWait = 30 * time.Second

// setMemLimits gives a VM its limits back after a dump. memory.high first: it reclaims
// and throttles, never kills. memory.max only once the usage fits under it: written
// over a higher memory.current, the kernel OOM-kills the VMM as soon as reclaim stalls
// (the dump's page cache in writeback), which is how a VM died right after a refused
// fork (boat-e2e 43224143615: firecracker gone without a word, no guest panic).
// memory.reclaim pushes the usage down meanwhile. Past memMaxWait the max stays lifted
// (by at most the VM's RAM, see liftMemLimits) and memory.high still bounds the VM.
func (m *Manager) setMemLimits(v VM) {
	m.dumpDone(v.ID)
	if !m.cfg.cgroupLimits {
		return
	}
	high, hard := m.cfg.memLimits(v)
	m.writeCgroup(v, "memory.high", strconv.FormatInt(high, 10))
	cg := filepath.Join(cgroupRoot, m.cfg.CgroupParent, v.ID)
	for deadline := time.Now().Add(memMaxWait); ; time.Sleep(200 * time.Millisecond) {
		cur, err := readInt(filepath.Join(cg, "memory.current"))
		if err != nil || cur <= hard {
			break // no memory.current: deleted meanwhile, or no memory controller to wait for
		}
		if time.Now().After(deadline) {
			log.Printf("%s: memory.max stays lifted: %d MiB in use over its %d MiB", v.ID, cur>>20, hard>>20)
			return
		}
		os.WriteFile(filepath.Join(cg, "memory.reclaim"), []byte(strconv.FormatInt(cur-hard, 10)), 0) // EAGAIN when short
	}
	m.writeCgroup(v, "memory.max", strconv.FormatInt(hard, 10))
}

func readInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// oomKills is the VM cgroup's oom_kill count, -1 when unknown.
func (m *Manager) oomKills(id string) int64 {
	b, err := os.ReadFile(filepath.Join(cgroupRoot, m.cfg.CgroupParent, id, "memory.events"))
	if err != nil {
		return -1
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "oom_kill" {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			return n
		}
	}
	return -1
}

// ---- memory density ----

// meminfoMiB reads one /proc/meminfo field; 0 where unavailable.
func meminfoMiB(key string) int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == key+":" {
			kb, _ := strconv.Atoi(f[1])
			return kb / 1024
		}
	}
	return 0
}

func hostMemMiB() int { return meminfoMiB("MemTotal") }

// realMemCheck is the overcommit half of host admission: committed RAM is capped
// by memOvercommit, and a new VM also needs real free memory for its share of a
// fully used host (RAM / overcommit), on top of the reserve. At 1.0 RAM is fully
// reserved already, so there is nothing to add.
// ponytail: VMs still booting are not yet in MemAvailable; the committed cap bounds
// that burst. Count "creating" VMs here if bursts ever trip the host OOM killer.
func (m *Manager) realMemCheck(s Size) error {
	if m.cfg.MemOvercommit <= 1 || m.memAvail == nil {
		return nil
	}
	avail := m.memAvail()
	if avail == 0 {
		return nil // unknown (non-Linux)
	}
	need := m.cfg.HostReserveMiB + int(float64(s.MemMiB+m.cfg.VMOverheadMiB)/m.cfg.MemOvercommit)
	if avail < need {
		return errCapacity("host capacity: %d MiB really free, %d MiB needed", avail, need)
	}
	return nil
}

// memoryPressure inflates balloons when the host runs short, so guests give back
// page cache before the host OOM killer has to pick a VM. Each tick asks for a
// further balloonStepPct of RAM, and lets them deflate once memory is back.
// deflate_on_oom keeps an inflated balloon from killing guest processes.
func (m *Manager) memoryPressure(ctx context.Context) {
	if !m.cfg.Balloon || m.memAvail == nil {
		return
	}
	avail := m.memAvail()
	if avail == 0 {
		return
	}
	low := avail < m.cfg.HostReserveMiB
	if !low && (!m.ballooned || avail < 2*m.cfg.HostReserveMiB) {
		return // hysteresis: hold the balloons until there is twice the reserve
	}
	m.ballooned = low
	m.balloonPct = min(balloonMaxPct, m.balloonPct+balloonStepPct)
	if !low {
		m.balloonPct = 0
	}
	log.Printf("memory: %d MiB available, reserve %d MiB: balloons at %d%%", avail, m.cfg.HostReserveMiB, m.balloonPct)
	var wg sync.WaitGroup
	for _, v := range m.list("") {
		if v.State == Running {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// This ticker also rotates the logs: a hung VMM must not stop that.
				ctx, cancel := context.WithTimeout(ctx, balloonCallTimeout)
				defer cancel()
				err := newFC(filepath.Join(m.root(v.ID), apiSock)).call(ctx, "PATCH", "/balloon",
					map[string]int{"amount_mib": v.MemMiB * m.balloonPct / 100})
				if err != nil {
					log.Printf("%s: balloon: %v", v.ID, err) // VM restored from a snapshot without one
				}
			}()
		}
	}
	wg.Wait()
}

const (
	balloonStepPct = 10
	balloonMaxPct  = 50
	pressureEvery  = 5 // log-rotation ticks (seconds) between memoryPressure checks

	balloonCallTimeout = 3 * time.Second
)

// ksmExecArg re-executes boatd as the launcher of a VM whose memory may merge:
// PR_SET_MEMORY_MERGE survives fork and exec, and Firecracker has no such option.
const ksmExecArg = "-ksm-exec"

// ksmExec never returns on success. Without kernel support (< 6.4) the VM runs unmerged.
func ksmExec(argv []string) error {
	if e := setMemoryMerge(); e != nil {
		log.Printf("ksm: prctl: %v (host kernel < 6.4?)", e)
	}
	return syscall.Exec(argv[0], argv, os.Environ())
}

// ksmFor: is this team's memory mergeable? Merging leaks across its scope, so
// the scope is one team, or "*" on a host with a single trust domain.
func (c Config) ksmFor(team string) bool {
	return c.KSMTeam != "" && (c.KSMTeam == "*" || c.KSMTeam == team)
}

func (c Config) enableKSM() {
	if c.KSMTeam == "" {
		return
	}
	for f, v := range map[string]string{"pages_to_scan": "1000", "run": "1"} {
		if err := os.WriteFile("/sys/kernel/mm/ksm/"+f, []byte(v), 0); err != nil {
			log.Printf("ksm: %s: %v", f, err)
		}
	}
}

// ---- warm templates (warmPool) ----
//
// A template is one booted, settled VM of an image + size, saved as a memory
// snapshot. A create is then a restore: reflinks, ~1 s of guest init, and no CPU
// for a boot (10 cold boots on 4 host CPUs took 28 s). It holds no RAM while
// nobody creates, unlike a pool of running VMs, which is what density needs.
// It never saw a tenant, and every create gets fresh identity entropy and VMGenID.

const warmTeam = "_warm"

// warmStamp identifies what a template was built from.
func (m *Manager) warmStamp(image string) string {
	s := ""
	for _, f := range []string{filepath.Join(m.imageDir(image), diskFile), filepath.Join(m.imageDir(image), kernFile), m.cfg.Firecracker} {
		if st, err := os.Stat(f); err == nil {
			s += fmt.Sprintf("%d-%d;", st.Size(), st.ModTime().UnixNano())
		}
	}
	// The VM config a restore inherits: diskCache Writeback on a host with templates
	// from before kept handing out Unsafe drives (AX42 r11: one NVMe flush in 20 s
	// of guest fsyncs).
	c := m.cfg
	return s + fmt.Sprintf("drive=%s/%s/%d/%d;args=%s;balloon=%t;diff=%t", c.DiskCache, c.DiskEngine, c.DiskMBps, c.DiskIOPS,
		c.BootArgs, c.Balloon, c.DiffSnapshots)
}

func (m *Manager) warmDir(image, size string) string {
	return filepath.Join(m.cfg.StateDir, "warm", image+"@"+size)
}

// warmTemplate returns the usable template dir for a create, or "".
func (m *Manager) warmTemplate(image, size string) string {
	if !m.cfg.WarmPool {
		return ""
	}
	d := m.warmDir(image, size)
	var meta struct {
		Stamp string `json:"stamp"`
	}
	if readJSON(filepath.Join(d, "warm.json"), &meta) != nil || meta.Stamp != m.warmStamp(image) {
		return ""
	}
	return d
}

// ensureWarm builds a missing template in the background, one per key at a time.
func (m *Manager) ensureWarm(ctx context.Context, image, size string) {
	if !m.cfg.WarmPool || m.warmTemplate(image, size) != "" {
		return
	}
	key := image + "@" + size
	m.mu.Lock()
	busy := m.warming[key]
	m.warming[key] = true
	m.mu.Unlock()
	if busy {
		return
	}
	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.warming, key)
			m.mu.Unlock()
		}()
		t0 := time.Now()
		if err := m.buildWarm(ctx, image, size); err != nil {
			log.Printf("warm %s: %v", key, err)
			return
		}
		log.Printf("warm %s: built in %dms", key, time.Since(t0).Milliseconds())
	}()
}

func (m *Manager) buildWarm(ctx context.Context, image, size string) error {
	v, err := m.createFrom(ctx, CreateReq{Team: warmTeam, Image: image, Size: size}, "")
	if err != nil {
		return err
	}
	defer m.Delete(context.WithoutCancel(ctx), v.ID)
	sock := filepath.Join(m.root(v.ID), vsockUDS)
	// Settled guest, no page cache, and its free pages reported: a small memory file.
	a, err := dialAgent(ctx, sock)
	if err != nil {
		return err
	}
	if err := a.send(map[string]any{"op": "exec", "argv": []string{"systemctl", "is-system-running", "--wait"}, "user": "root", "timeout_s": 30}); err == nil {
		for {
			if _, r, err := a.recv(); err != nil || r.final() {
				break
			}
		}
	}
	a.Close()
	if _, err := agentCall(ctx, sock, map[string]any{"op": "sync", "drop_caches": true}); err != nil {
		log.Printf("warm: sync: %v", err)
	}
	select { // the kernel reports free pages about 2 s after they are freed
	case <-time.After(m.warmSettle):
	case <-ctx.Done():
		return ctx.Err()
	}
	unlock := m.lock(v.ID)
	defer unlock()
	v, err = m.get(v.ID)
	if err != nil {
		return err
	}
	dst := m.warmDir(image, size)
	tmp := dst + ".tmp"
	os.RemoveAll(tmp)
	withMem, err := m.freeze(ctx, v, tmp)
	if err == nil && !withMem {
		err = fmt.Errorf("no memory snapshot")
	}
	if err == nil {
		err = fsyncFile(filepath.Join(tmp, memFile)) // creates clone it: flushed now, not by each of them
	}
	if err == nil {
		err = writeJSONAtomic(filepath.Join(tmp, "warm.json"), map[string]string{"stamp": m.warmStamp(image)}, 0o600)
	}
	if err == nil {
		os.RemoveAll(dst)
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.RemoveAll(tmp)
	}
	return err
}

// ksmArgv wraps a launch in the prctl re-exec.
func ksmArgv(self string, argv []string) []string {
	return append([]string{self, ksmExecArg, "--"}, argv...)
}

// syncPath fsyncs a boatd-owned file or directory (not one in a jail: see fsyncFile).
func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
