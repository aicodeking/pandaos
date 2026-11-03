// Command boatd is the privileged Firecracker VM manager behind boat-api.
// It serves an internal HTTP+JSON API on a unix socket; see ../PROTOCOL.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const version = "0.1.0"

func main() {
	if len(os.Args) > 3 && os.Args[1] == ksmExecArg { // before flag parsing: argv belongs to the jailer
		log.Fatal(ksmExec(os.Args[3:]))
	}
	cfgPath := flag.String("config", "/etc/boat/boatd.json", "config file")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if err := serve(*cfgPath); err != nil {
		log.Fatalf("boatd: %v", err)
	}
}

func serve(cfgPath string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	log.Printf("boatd %s starting: bridge %s, stateDir %s", version, cfg.Bridge, cfg.StateDir)
	if cfg.UntrustedTenants {
		if err := hostHardened("/sys"); err != nil {
			return fmt.Errorf("untrustedTenants: %w", err)
		}
	}
	if err := setupBridge(cfg, execRunner); err != nil {
		return fmt.Errorf("bridge: %w", err)
	}
	if err := setupCgroups(&cfg); err != nil {
		return err
	}
	if cfg.HostDirtyRatio > 0 {
		if err := os.WriteFile("/proc/sys/vm/dirty_ratio", []byte(strconv.Itoa(cfg.HostDirtyRatio)), 0); err != nil {
			log.Printf("vm.dirty_ratio: %v", err)
		}
	}
	log.Printf("cgroups: parent %q, limits %v", cfg.CgroupParent, cfg.cgroupLimits)
	m, err := newManager(cfg, execRunner, realProcs{cfg})
	if err != nil {
		return err
	}
	if err := m.syncNet(); err != nil {
		return fmt.Errorf("firewall rules not loaded, refusing to start: %w", err)
	}
	m.resumeAll(context.Background())
	ln, err := listenSocket(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		t := time.NewTicker(time.Duration(cfg.ReapS) * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.reap(ctx)
			}
		}
	}()
	go func() {
		pressureTick := 0
		t := time.NewTicker(logRotateEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.rotateLogs()
				m.flushDirty()
				if pressureTick++; pressureTick%pressureEvery == 0 {
					m.memoryPressure(ctx)
				}
			}
		}
	}()
	m.ensureWarm(ctx, cfg.DefaultImage, cfg.DefaultSize)
	srv := &http.Server{Handler: m.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(c)
	}()
	log.Printf("boatd %s listening on %s (%d VMs)", version, cfg.Socket, len(m.list("")))
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// The unit (KillMode=mixed) kills the whole cgroup once we exit, so
	// snapshot every running VM first; they resume on the next start.
	m.stopAll(context.Background(), "boatd shutdown")
	return nil
}

// cgroupRoot is a variable for the tests.
var cgroupRoot = "/sys/fs/cgroup"

// setupCgroups enables memory/pids/cpu/io for the per-VM children of cgroupParent (or
// a legacy --parent-cgroup in jailerArgs). A VM without them can take the host's
// memory, so this fails boatd's start unless cgroupsOptional says limits may be absent.
func setupCgroups(c *Config) error {
	if c.Jailer == "" {
		return nil
	}
	if i := slices.Index(c.JailerArgs, "--parent-cgroup"); i >= 0 && i+1 < len(c.JailerArgs) {
		c.CgroupParent = c.JailerArgs[i+1]
		c.JailerArgs = slices.Delete(slices.Clone(c.JailerArgs), i, i+2)
	}
	if err := delegateCgroups(c); err != nil {
		if !c.CgroupsOptional {
			return fmt.Errorf("cgroups: %w (VMs would run without limits; cgroupsOptional allows that)", err)
		}
		log.Printf("cgroups: %v; VMs run without limits", err)
		return nil
	}
	c.cgroupLimits = true
	c.stateDisk = stateDisk(c.StateDir)
	return nil
}

func delegateCgroups(c *Config) error {
	if c.CgroupParent == "" {
		return errors.New("no cgroup parent")
	}
	// The parent lives outside boatd's unit: a crash of boatd then kills neither
	// the VMs nor their memory (KillMode=mixed kills only the unit's cgroup), and
	// the next boatd re-adopts them. Every ancestor must enable the controllers.
	dir := cgroupRoot
	for _, part := range strings.Split(strings.Trim(c.CgroupParent, "/"), "/") {
		if err := enableControllers(dir); err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		dir = filepath.Join(dir, part)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := enableControllers(dir); err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	return nil
}

// enableControllers: io is optional (only io.max needs it), the others are not.
func enableControllers(dir string) error {
	ctl := filepath.Join(dir, "cgroup.subtree_control")
	if err := os.WriteFile(ctl, []byte("+memory +pids +cpu"), 0); err != nil {
		return err
	}
	if err := os.WriteFile(ctl, []byte("+io"), 0); err != nil {
		log.Printf("cgroups: %s: no io controller: %v", dir, err)
	}
	return nil
}

// stateDisk returns "major:minor" of the whole disk under dir, as io.max wants
// it ("" if unknown): btrfs's st_dev is anonymous, so go through the mount source.
func stateDisk(dir string) string {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	best, source := "", ""
	for _, l := range strings.Split(string(b), "\n") {
		pre, post, ok := strings.Cut(l, " - ")
		f, g := strings.Fields(pre), strings.Fields(post)
		if !ok || len(f) < 5 || len(g) < 2 {
			continue
		}
		if mp := f[4]; (dir == mp || mp == "/" || strings.HasPrefix(dir, mp+"/")) && len(mp) >= len(best) {
			best, source = mp, g[1]
		}
	}
	var st syscall.Stat_t
	if source == "" || syscall.Stat(source, &st) != nil {
		return ""
	}
	dev := uint64(st.Rdev)
	id := fmt.Sprintf("%d:%d", (dev>>8)&0xfff, (dev&0xff)|((dev>>12)&0xfff00))
	sys := "/sys/dev/block/" + id
	if _, err := os.Stat(sys + "/partition"); err == nil { // io.max takes whole disks only
		if p, err := os.ReadFile(sys + "/../dev"); err == nil {
			return strings.TrimSpace(string(p))
		}
	}
	return id
}

// hostHardened checks what only the kernel command line and sysctls can give. SMT
// shares a core's caches and buffers between guests; KSM merges their pages.
func hostHardened(sys string) error {
	if b, err := os.ReadFile(filepath.Join(sys, "devices/system/cpu/smt/control")); err == nil && strings.TrimSpace(string(b)) == "on" {
		return errors.New("SMT is on: boot with nosmt=force")
	}
	if b, err := os.ReadFile(filepath.Join(sys, "kernel/mm/ksm/run")); err == nil && strings.TrimSpace(string(b)) != "0" {
		return errors.New("KSM is running: echo 0 >/sys/kernel/mm/ksm/run")
	}
	return nil
}

// listenSocket: mode 0660, group socketGroup (boat-api's), so only root and
// that group can drive boatd.
func listenSocket(cfg Config) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.Socket), 0o755); err != nil {
		return nil, err
	}
	os.Remove(cfg.Socket)
	old := syscall.Umask(0o117)
	ln, err := net.Listen("unix", cfg.Socket)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if cfg.SocketGroup != "" {
		g, err := user.LookupGroup(cfg.SocketGroup)
		if err != nil {
			ln.Close()
			return nil, err
		}
		gid, _ := strconv.Atoi(g.Gid)
		if err := os.Chown(cfg.Socket, 0, gid); err != nil {
			ln.Close()
			return nil, err
		}
	}
	return ln, os.Chmod(cfg.Socket, 0o660)
}
