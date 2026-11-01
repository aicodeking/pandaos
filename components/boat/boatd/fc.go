package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Paths inside a VM's root dir. Firecracker runs with cwd = root (plain mode)
// or chrooted into root (jailer), and every path we hand it is relative, so a
// snapshot's vmstate never names a host path: a copy of root/ restores anywhere.
const (
	apiSock  = "fc.sock"
	vsockUDS = "v.sock"
	diskFile = "rootfs.ext4"
	kernFile = "vmlinux"
	memFile  = "mem"
	vmstate  = "vmstate"
)

// fcClient talks to one Firecracker API socket.
type fcClient struct{ hc *http.Client }

func newFC(sock string) *fcClient {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
	return &fcClient{hc: &http.Client{Transport: tr, Timeout: 10 * time.Minute}} // mem dumps of big VMs take a while
}

func (c *fcClient) call(ctx context.Context, method, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://fc"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("firecracker %s %s: HTTP %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

func (c *fcClient) pause(ctx context.Context) error {
	return c.call(ctx, "PATCH", "/vm", map[string]string{"state": "Paused"})
}

func (c *fcClient) resume(ctx context.Context) error {
	return c.call(ctx, "PATCH", "/vm", map[string]string{"state": "Resumed"})
}

// snapshot writes a "Full" or "Diff" (pages dirtied since the last snapshot,
// restore or boot; a sparse file) snapshot of a paused VM.
func (c *fcClient) snapshot(ctx context.Context, typ, statePath, memPath string) error {
	return c.call(ctx, "PUT", "/snapshot/create", map[string]string{
		"snapshot_type": typ, "snapshot_path": statePath, "mem_file_path": memPath,
	})
}

// restore loads root/{vmstate,mem} (mem is mmapped MAP_PRIVATE, so it stays
// the VM's backing file and the base for its next diff snapshot), points eth0
// at tap0 of the VM's own netns and resumes. VMGenID (Firecracker >= 1.10,
// x86_64) is bumped on every load, so a guest kernel with the vmgenid driver
// reseeds its CRNG on its own as well.
func (c *fcClient) restore(ctx context.Context, trackDirty bool) error {
	return c.call(ctx, "PUT", "/snapshot/load", map[string]any{
		"snapshot_path":     vmstate,
		"mem_backend":       map[string]string{"backend_type": "File", "backend_path": memFile},
		"resume_vm":         true,
		"track_dirty_pages": trackDirty,
		"network_overrides": []map[string]string{{"iface_id": "eth0", "host_dev_name": guestTap}},
	})
}

// tokenBucket is a Firecracker rate limiter refilled every second; nil when unlimited.
func rateLimiter(bytesPerS, opsPerS int64) map[string]any {
	rl := map[string]any{}
	if bytesPerS > 0 {
		rl["bandwidth"] = map[string]int64{"size": bytesPerS, "refill_time": 1000}
	}
	if opsPerS > 0 {
		rl["ops"] = map[string]int64{"size": opsPerS, "refill_time": 1000}
	}
	if len(rl) == 0 {
		return nil
	}
	return rl
}

func (c *fcClient) boot(ctx context.Context, v VM, bootArgs string, trackDirty bool, cfg Config) error {
	drive := map[string]any{"drive_id": "rootfs", "path_on_host": diskFile, "is_root_device": true, "is_read_only": false}
	if rl := rateLimiter(int64(cfg.DiskMBps)<<20, int64(cfg.DiskIOPS)); rl != nil {
		drive["rate_limiter"] = rl
	}
	nic := map[string]any{"iface_id": "eth0", "guest_mac": v.MAC, "host_dev_name": guestTap}
	if rl := rateLimiter(int64(cfg.IngressMbit)*125000, 0); rl != nil {
		nic["rx_rate_limiter"] = rl
	}
	steps := []struct {
		path string
		body any
	}{
		{"/boot-source", map[string]string{"kernel_image_path": kernFile, "boot_args": bootArgs}},
		{"/drives/rootfs", drive},
		{"/machine-config", map[string]any{"vcpu_count": v.VCPU, "mem_size_mib": v.MemMiB, "smt": false, "track_dirty_pages": trackDirty}},
		{"/network-interfaces/eth0", nic},
		{"/vsock", map[string]any{"guest_cid": 3, "uds_path": vsockUDS}},
		{"/actions", map[string]string{"action_type": "InstanceStart"}},
	}
	for _, s := range steps {
		if err := c.call(ctx, "PUT", s.path, s.body); err != nil {
			return err
		}
	}
	return nil
}

// procs starts and watches hypervisor processes; tests swap in a fake.
type procs interface {
	// Start launches firecracker for v rooted at root. done closes when it exits.
	Start(v VM, root string, uid, gid int) (pid int, done <-chan struct{}, err error)
	// Adopt watches a process boatd did not start (after a boatd restart).
	Adopt(pid int, id string) <-chan struct{}
	Alive(pid int, id string) bool
	Kill(pid int)
}

type realProcs struct{ cfg Config }

func (p realProcs) Start(v VM, root string, uid, gid int) (int, <-chan struct{}, error) {
	vmDir := filepath.Dir(root)
	if p.cfg.Jailer != "" {
		vmDir = filepath.Dir(filepath.Dir(filepath.Dir(root))) // <vmdir>/<exec>/<id>/root
	}
	logf, err := os.OpenFile(filepath.Join(vmDir, "firecracker.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 0, nil, err
	}
	defer logf.Close()
	exe := filepath.Base(p.cfg.Firecracker)
	var cmd *exec.Cmd
	if p.cfg.Jailer != "" {
		// The jailer re-creates these each launch and fails if they exist
		// (<exec>.pid too: "Failed to open file /firecracker.pid: File exists").
		for _, n := range []string{"dev", "run", exe, exe + ".pid"} {
			os.RemoveAll(filepath.Join(root, n))
		}
		cmd = exec.Command(p.cfg.Jailer, jailerArgs(p.cfg, v, uid, gid, vmDir)...)
	} else {
		// Dev/test mode: one shared uid, no chroot/cgroup; still in the VM's netns.
		args := []string{"--net=" + netnsPath(v.Index)}
		if uid != 0 {
			args = append(args, "--setuid", strconv.Itoa(uid), "--setgid", strconv.Itoa(gid))
		}
		args = append(args, "--", p.cfg.Firecracker, "--id", v.ID, "--api-sock", filepath.Join(root, apiSock))
		cmd = exec.Command("nsenter", args...)
		cmd.Dir = root
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	if p.cfg.Jailer == "" {
		done := make(chan struct{})
		go func() { <-exited; close(done) }()
		setOOMScore(cmd.Process.Pid)
		return cmd.Process.Pid, done, nil
	}
	// --new-pid-ns: the jailer forks firecracker as pid 1 of a new pid
	// namespace, writes its host pid to <chroot>/<exe>.pid and exits.
	pid, err := waitPidFile(filepath.Join(root, exe+".pid"), exited)
	if err != nil {
		return 0, nil, err
	}
	p.setIOMax(v.ID)
	return pid, p.Adopt(pid, v.ID), nil
}

// setIOMax writes the VM cgroup's io.max. It is not a --cgroup flag because the
// jailer rejects values that contain "=".
func (p realProcs) setIOMax(id string) {
	c := p.cfg
	if !c.cgroupLimits || c.stateDisk == "" || (c.IOMaxMBps == 0 && c.IOMaxIOPS == 0) {
		return
	}
	val := c.stateDisk
	if c.IOMaxMBps > 0 {
		b := int64(c.IOMaxMBps) << 20
		val += fmt.Sprintf(" rbps=%d wbps=%d", b, b)
	}
	if c.IOMaxIOPS > 0 {
		val += fmt.Sprintf(" riops=%d wiops=%d", c.IOMaxIOPS, c.IOMaxIOPS)
	}
	if err := os.WriteFile(filepath.Join("/sys/fs/cgroup", c.CgroupParent, id, "io.max"), []byte(val), 0); err != nil {
		log.Printf("%s: io.max: %v", id, err)
	}
}

// jailerArgs: per-VM uid/gid, chroot, netns, new pid ns and a cgroup with
// memory/cpu/pids limits (when the controllers are delegated to us).
func jailerArgs(c Config, v VM, uid, gid int, vmDir string) []string {
	args := []string{"--id", v.ID, "--exec-file", c.Firecracker,
		"--uid", strconv.Itoa(uid), "--gid", strconv.Itoa(gid),
		"--chroot-base-dir", vmDir, "--netns", netnsPath(v.Index), "--new-pid-ns", "--cgroup-version", "2"}
	if c.CgroupParent != "" {
		args = append(args, "--parent-cgroup", c.CgroupParent)
	}
	if c.cgroupLimits {
		args = append(args,
			"--cgroup", fmt.Sprintf("memory.max=%d", int64(v.MemMiB+c.VMOverheadMiB)<<20),
			"--cgroup", fmt.Sprintf("cpu.max=%d 100000", (v.VCPU+1)*100000), // vCPUs + 1 for the VMM threads
			"--cgroup", "pids.max=256")
	}
	args = append(args, c.JailerArgs...)
	return append(args, "--", "--api-sock", "/"+apiSock)
}

func waitPidFile(path string, exited <-chan error) (int, error) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid, nil
			}
		}
		select {
		case err := <-exited:
			if err != nil {
				return 0, fmt.Errorf("jailer: %w (see firecracker.log)", err)
			}
			exited = nil // exited cleanly; the pid file is (about to be) there
		case <-time.After(10 * time.Millisecond):
		}
	}
	return 0, fmt.Errorf("jailer wrote no pid file %s", path)
}

// vmOOMScoreAdj: boatd.service runs at OOMScoreAdjust=-900 and children inherit
// it. Under memory pressure a VM must die before boatd or the host.
const vmOOMScoreAdj = "500"

func setOOMScore(pid int) {
	if err := os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), []byte(vmOOMScoreAdj), 0); err != nil {
		log.Printf("pid %d: oom_score_adj: %v", pid, err)
	}
}

func (p realProcs) Adopt(pid int, id string) <-chan struct{} {
	setOOMScore(pid) // also re-applied to VMs a restarted boatd takes over
	done := make(chan struct{})
	go func() {
		for p.Alive(pid, id) {
			time.Sleep(100 * time.Millisecond)
		}
		close(done)
	}()
	return done
}

// Alive: pid exists and its cmdline names this VM (guards against pid reuse).
func (realProcs) Alive(pid int, id string) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	return err == nil && cmdlineAlive(b, stat, id)
}

// cmdlineAlive judges /proc/<pid>/{cmdline,stat}. An empty cmdline is not death:
// it is also what a process shows mid-exec, between the old image and the new
// one's arguments, and the jailer's child is exactly there when Start polls it.
// Treating that as an exit marked healthy VMs stopped. Only a zombie or a dead
// process counts (its stat state is Z, X or x).
func cmdlineAlive(cmdline, stat []byte, id string) bool {
	if len(cmdline) > 0 {
		return bytes.Contains(cmdline, []byte("\x00"+id+"\x00"))
	}
	// stat is "pid (comm) S ...": comm may hold spaces and parens, so use the last ')'.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 || i+2 >= len(stat) {
		return false
	}
	switch stat[i+2] {
	case 'Z', 'X', 'x':
		return false
	}
	return true
}

func (realProcs) Kill(pid int) { syscall.Kill(pid, syscall.SIGKILL) }

// waitSock waits until the API socket accepts, or the process dies.
func waitSock(path string, done <-chan struct{}, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return nil
		}
		select {
		case <-done:
			return fmt.Errorf("firecracker exited during startup (see firecracker.log)")
		case <-time.After(20 * time.Millisecond):
		}
	}
	return fmt.Errorf("firecracker API socket %s not ready after %s", path, d)
}
