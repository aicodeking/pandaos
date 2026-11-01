package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// snapshot writes a full snapshot to root/<prefix>vmstate and root/<prefix>mem.
func (c *fcClient) snapshot(ctx context.Context, prefix string) error {
	return c.call(ctx, "PUT", "/snapshot/create", map[string]string{
		"snapshot_type": "Full", "snapshot_path": prefix + vmstate, "mem_file_path": prefix + memFile,
	})
}

// restore loads root/{vmstate,mem}, swaps the tap (Firecracker >= 1.12) and resumes.
func (c *fcClient) restore(ctx context.Context, tap string) error {
	return c.call(ctx, "PUT", "/snapshot/load", map[string]any{
		"snapshot_path":     vmstate,
		"mem_backend":       map[string]string{"backend_type": "File", "backend_path": memFile},
		"resume_vm":         true,
		"network_overrides": []map[string]string{{"iface_id": "eth0", "host_dev_name": tap}},
	})
}

func (c *fcClient) boot(ctx context.Context, v VM, bootArgs string) error {
	steps := []struct {
		path string
		body any
	}{
		{"/boot-source", map[string]string{"kernel_image_path": kernFile, "boot_args": bootArgs}},
		{"/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": diskFile, "is_root_device": true, "is_read_only": false}},
		{"/machine-config", map[string]any{"vcpu_count": v.VCPU, "mem_size_mib": v.MemMiB, "smt": false}},
		{"/network-interfaces/eth0", map[string]string{"iface_id": "eth0", "guest_mac": v.MAC, "host_dev_name": v.Tap}},
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
	var cmd *exec.Cmd
	if p.cfg.Jailer != "" {
		// The jailer re-creates these each launch and fails if they exist
		// (<exec>.pid too: "Failed to open file /firecracker.pid: File exists").
		exe := filepath.Base(p.cfg.Firecracker)
		for _, n := range []string{"dev", "run", exe, exe + ".pid"} {
			os.RemoveAll(filepath.Join(root, n))
		}
		args := []string{"--id", v.ID, "--exec-file", p.cfg.Firecracker,
			"--uid", strconv.Itoa(uid), "--gid", strconv.Itoa(gid),
			"--chroot-base-dir", vmDir, "--cgroup-version", "2"}
		args = append(append(args, p.cfg.JailerArgs...), "--", "--api-sock", "/"+apiSock)
		cmd = exec.Command(p.cfg.Jailer, args...)
	} else {
		cmd = exec.Command(p.cfg.Firecracker, "--id", v.ID, "--api-sock", filepath.Join(root, apiSock))
		cmd.Dir = root
		cmd.SysProcAttr = &syscall.SysProcAttr{}
		if uid != 0 {
			cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
		}
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true // survive a boatd restart (unit has KillMode=process)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	return cmd.Process.Pid, done, nil
}

func (p realProcs) Adopt(pid int, id string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for p.Alive(pid, id) {
			time.Sleep(500 * time.Millisecond)
		}
		close(done)
	}()
	return done
}

// Alive: pid exists and its cmdline names this VM (guards against pid reuse).
func (realProcs) Alive(pid int, id string) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && bytes.Contains(b, []byte("\x00"+id+"\x00"))
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
