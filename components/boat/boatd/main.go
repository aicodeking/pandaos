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
	if err := setupBridge(cfg, execRunner); err != nil {
		return fmt.Errorf("bridge: %w", err)
	}
	if cg := ownCgroup(); cfg.Jailer != "" && cg != "" && !slices.Contains(cfg.JailerArgs, "--parent-cgroup") {
		cfg.JailerArgs = append(cfg.JailerArgs, "--parent-cgroup", cg)
	}
	m, err := newManager(cfg, execRunner, realProcs{cfg})
	if err != nil {
		return err
	}
	m.syncNet()
	m.resumeAll(context.Background())
	ln, err := listenSocket(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		t := time.NewTicker(30 * time.Second)
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

// ownCgroup is boatd's cgroup v2 path (Delegate=yes); jailed VMs go below it.
func ownCgroup() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(l, "0::/"); ok && p != "" {
			return p
		}
	}
	return ""
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
