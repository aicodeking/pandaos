package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLinuxDevIDAndMountSource(t *testing.T) {
	// Classic major 8 minor 1 -> 0x0801.
	if got := linuxDevID(0x0801); got != "8:1" {
		t.Fatalf("linuxDevID(0x0801) = %q, want 8:1", got)
	}
	// NVMe major 259, minor 3 -> (259 << 8) | 3 = 0x10303.
	if got := linuxDevID((259 << 8) | 3); got != "259:3" {
		t.Fatalf("linuxDevID(nvme0n1p3) = %q, want 259:3", got)
	}
	// Extended minor > 255: major 259, minor 0x104 -> low8=0x04, high=((0x100)<<12)=0x100000.
	if got := linuxDevID((259 << 8) | 0x04 | (0x100 << 12)); got != "259:260" {
		t.Fatalf("linuxDevID(259:260) = %q, want 259:260", got)
	}

	mi := filepath.Join(t.TempDir(), "mountinfo")
	content := strings.Join([]string{
		"29 1 253:0 / / rw,relatime shared:1 - ext4 /dev/mapper/vg0-root rw",
		"44 29 0:31 /@boat /var/lib/boat rw,relatime shared:2 - btrfs /dev/loop0[/@boat] rw,ssd",
		"45 29 8:2 / /var/lib/other rw,relatime - ext4 /dev/sda2 rw",
	}, "\n")
	if err := os.WriteFile(mi, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mountSource(mi, "/var/lib/boat/vms"); got != "/dev/loop0" {
		t.Fatalf("mountSource(/var/lib/boat/vms) = %q, want /dev/loop0", got)
	}
	if got := mountSource(mi, "/srv/state"); got != "/dev/mapper/vg0-root" {
		t.Fatalf("mountSource(/srv/state) = %q, want /dev/mapper/vg0-root", got)
	}
}

func TestResolveBlockDiskTopologies(t *testing.T) {
	root := t.TempDir()
	sysBlock := filepath.Join(root, "sys", "dev", "block")
	devs := filepath.Join(root, "sys", "devices")
	mustMkdir := func(p string) {
		t.Helper()
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(p, s string) {
		t.Helper()
		mustMkdir(filepath.Dir(p))
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustSymlink := func(target, link string) {
		t.Helper()
		mustMkdir(filepath.Dir(link))
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}

	// Physical NVMe disk 259:0 with partitions 259:1, 259:2, 259:3.
	nvme0 := filepath.Join(devs, "pci0000:00", "nvme0", "nvme0n1")
	mustWrite(filepath.Join(nvme0, "dev"), "259:0\n")
	mustMkdir(filepath.Join(nvme0, "device"))
	mustSymlink(nvme0, filepath.Join(sysBlock, "259:0"))
	for i := 1; i <= 3; i++ {
		part := filepath.Join(nvme0, "nvme0n1p"+strconv.Itoa(i))
		id := "259:" + strconv.Itoa(i)
		mustWrite(filepath.Join(part, "dev"), id+"\n")
		mustWrite(filepath.Join(part, "partition"), strconv.Itoa(i)+"\n")
		mustSymlink(part, filepath.Join(sysBlock, id))
	}

	// Second physical NVMe disk 259:4 for striped multi-disk rejection test.
	nvme1 := filepath.Join(devs, "pci0000:00", "nvme1", "nvme1n1")
	mustWrite(filepath.Join(nvme1, "dev"), "259:4\n")
	mustMkdir(filepath.Join(nvme1, "device"))
	mustSymlink(nvme1, filepath.Join(sysBlock, "259:4"))

	// Loop device 7:0 with direct-IO enabled (dio=1) backed by /var/lib/boat.btrfs on partition 259:2 -> 259:0.
	loop0 := filepath.Join(devs, "virtual", "block", "loop0")
	mustWrite(filepath.Join(loop0, "dev"), "7:0\n")
	mustWrite(filepath.Join(loop0, "loop", "backing_file"), "/var/lib/boat.btrfs\n")
	mustWrite(filepath.Join(loop0, "loop", "dio"), "1\n")
	mustSymlink(loop0, filepath.Join(sysBlock, "7:0"))

	// Buffered loop device 7:2 (dio=0) and 7:3 (missing dio) keep the loop block device ID.
	loop2 := filepath.Join(devs, "virtual", "block", "loop2")
	mustWrite(filepath.Join(loop2, "dev"), "7:2\n")
	mustWrite(filepath.Join(loop2, "loop", "backing_file"), "/var/lib/boat.btrfs\n")
	mustWrite(filepath.Join(loop2, "loop", "dio"), "0\n")
	mustSymlink(loop2, filepath.Join(sysBlock, "7:2"))

	loop3 := filepath.Join(devs, "virtual", "block", "loop3")
	mustWrite(filepath.Join(loop3, "dev"), "7:3\n")
	mustWrite(filepath.Join(loop3, "loop", "backing_file"), "/var/lib/boat.btrfs\n")
	mustSymlink(loop3, filepath.Join(sysBlock, "7:3"))

	// Unbacked loop device 7:1 -> "".
	loop1 := filepath.Join(devs, "virtual", "block", "loop1")
	mustWrite(filepath.Join(loop1, "dev"), "7:1\n")
	mustMkdir(filepath.Join(loop1, "loop"))
	mustSymlink(loop1, filepath.Join(sysBlock, "7:1"))

	// Single-slave LVM dm-0 (253:0) -> partition 259:3 -> 259:0.
	dm0 := filepath.Join(devs, "virtual", "block", "dm-0")
	mustWrite(filepath.Join(dm0, "dev"), "253:0\n")
	mustMkdir(filepath.Join(dm0, "dm"))
	mustSymlink(filepath.Join(nvme0, "nvme0n1p3"), filepath.Join(dm0, "slaves", "nvme0n1p3"))
	mustSymlink(dm0, filepath.Join(sysBlock, "253:0"))

	// Multi-slave dm-1 (253:1) whose slaves are two partitions on the SAME disk (259:1, 259:2) -> 259:0.
	dm1 := filepath.Join(devs, "virtual", "block", "dm-1")
	mustWrite(filepath.Join(dm1, "dev"), "253:1\n")
	mustMkdir(filepath.Join(dm1, "dm"))
	mustSymlink(filepath.Join(nvme0, "nvme0n1p1"), filepath.Join(dm1, "slaves", "nvme0n1p1"))
	mustSymlink(filepath.Join(nvme0, "nvme0n1p2"), filepath.Join(dm1, "slaves", "nvme0n1p2"))
	mustSymlink(dm1, filepath.Join(sysBlock, "253:1"))

	// Multi-disk striped dm-2 (253:2) across 259:0 and 259:4 -> "" (ambiguous).
	dm2 := filepath.Join(devs, "virtual", "block", "dm-2")
	mustWrite(filepath.Join(dm2, "dev"), "253:2\n")
	mustMkdir(filepath.Join(dm2, "dm"))
	mustSymlink(nvme0, filepath.Join(dm2, "slaves", "nvme0n1"))
	mustSymlink(nvme1, filepath.Join(dm2, "slaves", "nvme1n1"))
	mustSymlink(dm2, filepath.Join(sysBlock, "253:2"))

	// Unslaved dm-9 (253:9) -> "".
	dm9 := filepath.Join(devs, "virtual", "block", "dm-9")
	mustWrite(filepath.Join(dm9, "dev"), "253:9\n")
	mustMkdir(filepath.Join(dm9, "dm"))
	mustSymlink(dm9, filepath.Join(sysBlock, "253:9"))

	// Cycle guard: loop8 -> /cycleA -> 7:9 -> /cycleB -> 7:8 -> terminates at depth 8 with "".
	loop8 := filepath.Join(devs, "virtual", "block", "loop8")
	mustWrite(filepath.Join(loop8, "dev"), "7:8\n")
	mustWrite(filepath.Join(loop8, "loop", "backing_file"), "/cycleA\n")
	mustWrite(filepath.Join(loop8, "loop", "dio"), "1\n")
	mustSymlink(loop8, filepath.Join(sysBlock, "7:8"))
	loop9 := filepath.Join(devs, "virtual", "block", "loop9")
	mustWrite(filepath.Join(loop9, "dev"), "7:9\n")
	mustWrite(filepath.Join(loop9, "loop", "backing_file"), "/cycleB\n")
	mustWrite(filepath.Join(loop9, "loop", "dio"), "1\n")
	mustSymlink(loop9, filepath.Join(sysBlock, "7:9"))

	statMap := map[string]string{
		"/var/lib/boat.btrfs": "259:2",
		"/cycleA":             "7:9",
		"/cycleB":             "7:8",
	}
	statFn := func(path string) (string, error) {
		if id, ok := statMap[path]; ok {
			return id, nil
		}
		return "", errors.New("not found")
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"whole_nvme", "259:0", "259:0"},
		{"nvme_partition", "259:1", "259:0"},
		{"loop_dio_on_partition", "7:0", "259:0"},
		{"loop_buffered_dio_0", "7:2", "7:2"},
		{"loop_missing_dio", "7:3", "7:3"},
		{"lvm_dm_on_partition", "253:0", "259:0"},
		{"dm_same_disk_slaves", "253:1", "259:0"},
		{"dm_multi_disk_striped_rejected", "253:2", ""},
		{"unbacked_loop_rejected", "7:1", ""},
		{"unslaved_dm_rejected", "253:9", ""},
		{"cycle_depth_guard", "7:8", ""},
		{"missing_device", "999:0", ""},
	}
	for _, tc := range cases {
		if got := resolveBlockDisk(sysBlock, tc.in, statFn); got != tc.want {
			t.Errorf("%s: resolveBlockDisk(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestUntrustedTenantsSetupCgroupsFailClosed(t *testing.T) {
	origRoot, origDiskFn := cgroupRoot, stateDiskFn
	defer func() {
		cgroupRoot = origRoot
		stateDiskFn = origDiskFn
	}()

	makeUntrustedCfg := func() Config {
		c := defaultConfig()
		c.UntrustedTenants = true
		c.Jailer = "/usr/bin/jailer"
		c.CgroupParent = "boat"
		c.CgroupsOptional = false
		c.IOCost = true
		c.IOCostModel = "rbps=3000000000 rrandiops=400000"
		return c
	}

	t.Run("missing_io_controller_fails", func(t *testing.T) {
		cg := t.TempDir()
		cgroupRoot = cg
		stateDiskFn = func(string) string { return "259:0" }
		// Make cgroup.subtree_control overwrite end with only memory/pids/cpu when read back:
		// simulate a kernel where "+io" is rejected by pre-creating boat/cgroup.subtree_control as a directory.
		if err := os.MkdirAll(filepath.Join(cg, "boat", "cgroup.subtree_control"), 0o755); err != nil {
			t.Fatal(err)
		}
		c := makeUntrustedCfg()
		if err := setupCgroups(&c); err == nil {
			t.Fatal("setupCgroups succeeded despite cgroup controller failure")
		}
	})

	t.Run("io_controller_inactive_in_subtree_control_fails", func(t *testing.T) {
		// In regular files on tmpfs, writing "+io" overwrites file contents with "+io" (which satisfies "+io"),
		// whereas writing "+memory +pids +cpu" without "+io" happens if "+io" fails. Verify enableControllers directly
		// when cgroup.subtree_control is read-only or lacks io.
		dir := t.TempDir()
		ctl := filepath.Join(dir, "cgroup.subtree_control")
		if err := os.WriteFile(ctl, []byte("memory pids cpu"), 0o400); err != nil {
			t.Fatal(err)
		}
		if err := enableControllers(dir, true); err == nil {
			t.Fatal("enableControllers succeeded on unwritable subtree_control")
		}
	})

	t.Run("empty_state_disk_fails", func(t *testing.T) {
		cg := t.TempDir()
		cgroupRoot = cg
		stateDiskFn = func(string) string { return "" }
		c := makeUntrustedCfg()
		err := setupCgroups(&c)
		if err == nil || !strings.Contains(err.Error(), "resolvable block device") {
			t.Fatalf("setupCgroups with empty stateDisk = %v, want resolvable block device error", err)
		}
	})

	t.Run("iocost_model_failure_fails", func(t *testing.T) {
		cg := t.TempDir()
		cgroupRoot = cg
		stateDiskFn = func(string) string { return "259:0" }
		// Force io.cost.model write to fail by making it a directory.
		if err := os.MkdirAll(filepath.Join(cg, "io.cost.model"), 0o755); err != nil {
			t.Fatal(err)
		}
		c := makeUntrustedCfg()
		err := setupCgroups(&c)
		if err == nil || !strings.Contains(err.Error(), "io.cost.model") {
			t.Fatalf("setupCgroups with failing io.cost.model = %v", err)
		}
	})

	t.Run("iocost_empty_model_succeeds_with_resolved_disk", func(t *testing.T) {
		cg := t.TempDir()
		cgroupRoot = cg
		stateDiskFn = func(string) string { return "259:0" }
		c := makeUntrustedCfg()
		c.IOCostModel = ""
		if err := setupCgroups(&c); err != nil {
			t.Fatalf("setupCgroups with empty ioCostModel failed: %v", err)
		}
		if c.stateDisk != "259:0" || !c.cgroupLimits {
			t.Fatalf("unexpected cgroup state: stateDisk=%q cgroupLimits=%v", c.stateDisk, c.cgroupLimits)
		}
	})

	t.Run("full_untrusted_cgroups_succeeds", func(t *testing.T) {
		cg := t.TempDir()
		cgroupRoot = cg
		stateDiskFn = func(string) string { return "259:0" }
		c := makeUntrustedCfg()
		if err := setupCgroups(&c); err != nil {
			t.Fatalf("setupCgroups failed: %v", err)
		}
		if b, _ := os.ReadFile(filepath.Join(cg, "io.cost.model")); !strings.HasPrefix(string(b), "259:0 ctrl=user model=linear ") {
			t.Fatalf("unexpected io.cost.model: %q", b)
		}
	})
}

func TestUntrustedTenantsStartKillsVMWhenIOMaxFails(t *testing.T) {
	origRoot := cgroupRoot
	defer func() { cgroupRoot = origRoot }()

	tmp := t.TempDir()
	cgroupRoot = filepath.Join(tmp, "cgroup")

	// Fake jailer spawns a background sleep process, writes its PID to <root>/firecracker.pid, and exits 0.
	fakeJailer := filepath.Join(tmp, "fake-jailer.sh")
	script := `#!/bin/sh
set -eu
id=""
base=""
while [ $# -gt 0 ]; do
  case "$1" in
    --id) id="$2"; shift 2 ;;
    --chroot-base-dir) base="$2"; shift 2 ;;
    *) shift ;;
  esac
done
root="$base/firecracker/$id/root"
mkdir -p "$root"
sleep 60 >/dev/null 2>&1 &
pid=$!
printf '%s\n' "$pid" > "$root/firecracker.pid"
exit 0
`
	if err := os.WriteFile(fakeJailer, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		stateDisk string
	}{
		{"io_max_write_error", "259:0"},
		{"missing_state_disk", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vmID := "vm-iomax-" + tc.name
			vmDir := filepath.Join(tmp, tc.name, "vms", vmID)
			root := filepath.Join(vmDir, "firecracker", vmID, "root")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}

			c := defaultConfig()
			c.UntrustedTenants = true
			c.Jailer = fakeJailer
			c.Firecracker = "/usr/bin/firecracker"
			c.CgroupParent = "boat"
			c.cgroupLimits = true
			c.stateDisk = tc.stateDisk
			c.IOMaxMBps = 250
			c.IOMaxIOPS = 10000
			// Deliberately do NOT create filepath.Join(cgroupRoot, "boat", vmID), so writing io.max fails!

			p := realProcs{cfg: c}
			v := VM{ID: vmID, Index: 2, VCPU: 1, MemMiB: 256}
			_, _, err := p.Start(v, root, 900002, 900002)
			if err == nil || !strings.Contains(err.Error(), "io.max") {
				t.Fatalf("Start error = %v, want io.max failure", err)
			}

			b, rerr := os.ReadFile(filepath.Join(root, "firecracker.pid"))
			if rerr != nil {
				t.Fatalf("pid file not written by fake jailer: %v", rerr)
			}
			pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
			if perr != nil || pid <= 0 {
				t.Fatalf("invalid pid %q: %v", b, perr)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

			dead := false
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if err := syscall.Kill(pid, 0); err != nil {
					dead = true
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if !dead {
				t.Fatalf("spawned firecracker pid %d survived io.max failure under untrustedTenants", pid)
			}
		})
	}
}
