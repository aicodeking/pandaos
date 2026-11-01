// Command boat-api is the public Boat API server (TLS is terminated by Caddy).
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed openapi.json
var openapiSpec []byte

const usageText = `usage:
  boat-api [serve] [flags]          run the API server
  boat-api admin init [flags]       create the operator (first owner) + team, print its key
  boat-api admin gen-master-key     print a new 32-byte master key (hex) for systemd-creds encrypt
run "boat-api serve -h" for flags`

type common struct {
	dataDir, masterKey string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.dataDir, "data", "/var/lib/boat-api", "state directory")
	fs.StringVar(&c.masterKey, "master-key-file", "", "32-byte master key (default $CREDENTIALS_DIRECTORY/boat-master-key, else <data>/master.key, created 0600)")
}

// masterKeyPath picks the master key source. Preferred: the systemd
// credential "boat-master-key" (LoadCredentialEncrypted=, TPM2-sealed when
// the host has one), which systemd decrypts into $CREDENTIALS_DIRECTORY.
// Fallback: <data>/master.key (created 0600), with a warning, because a
// plain file on the data disk is readable from backups and disk images.
func (c *common) masterKeyPath() (path string, create bool, warn string) {
	if c.masterKey != "" {
		return c.masterKey, false, ""
	}
	if d := os.Getenv("CREDENTIALS_DIRECTORY"); d != "" {
		p := filepath.Join(d, "boat-master-key")
		if _, err := os.Stat(p); err == nil {
			return p, false, ""
		}
	}
	return filepath.Join(c.dataDir, "master.key"), true,
		"WARNING: master key is a plain file on the data disk; use the systemd credential boat-master-key " +
			"(LoadCredentialEncrypted=, see README) so secrets at rest are not readable from disk images or backups"
}

func (c *common) open() (*Store, *Keyring, error) {
	path, create, warn := c.masterKeyPath()
	if warn != "" {
		log.Print(warn + " (" + path + ")")
	}
	master, err := LoadMasterKey(path, create)
	if err != nil {
		return nil, nil, fmt.Errorf("master key: %w", err)
	}
	kr, err := NewKeyring(master)
	if err != nil {
		return nil, nil, err
	}
	st, err := OpenStore(c.dataDir)
	return st, kr, err
}

func main() {
	args := os.Args[1:]
	switch {
	case len(args) >= 2 && args[0] == "admin" && args[1] == "gen-master-key":
		fmt.Println(randHex(32))
		os.Exit(0)
	case len(args) >= 2 && args[0] == "admin" && args[1] == "init":
		os.Exit(adminInit(args[2:]))
	case len(args) == 0 || strings.HasPrefix(args[0], "-"):
		os.Exit(serve(args))
	case args[0] == "serve":
		os.Exit(serve(args[1:]))
	default:
		fmt.Fprintln(os.Stderr, usageText)
		os.Exit(2)
	}
}

func adminInit(args []string) int {
	fs := flag.NewFlagSet("admin init", flag.ExitOnError)
	var c common
	c.register(fs)
	user := fs.String("user", "owner", "owner user name")
	team := fs.String("team", "default", "first team name")
	fs.Parse(args)
	store, kr, err := c.open()
	if err != nil {
		fmt.Fprintln(os.Stderr, "boat-api:", err)
		return 1
	}
	k, err := bootstrap(store, kr, *user, *team)
	if err != nil {
		fmt.Fprintln(os.Stderr, "boat-api:", err)
		return 1
	}
	json.NewEncoder(os.Stdout).Encode(k)
	fmt.Fprintln(os.Stderr, "Store this key now; it is not shown again. Restart boat-api if it is running.")
	return 0
}

func serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var c common
	c.register(fs)
	listen := fs.String("listen", "127.0.0.1:8740", "listen address")
	boatd := fs.String("boatd", "/run/boat/boatd.sock", "boatd unix socket path or http:// URL")
	rate := fs.Float64("rate", 10, "requests/second per API key (0 = unlimited)")
	burst := fs.Float64("burst", 40, "rate limit burst per API key")
	auditPath := fs.String("audit-log", "", "audit JSON-lines file (default <data>/audit.jsonl, - = stderr)")
	sshUser := fs.String("ssh-user", "dev", "VM login user (the guest image's user)")
	sshJump := fs.String("ssh-jump", "", "ssh jump host (user@host:port) returned to clients")
	desktopURL := fs.String("desktop-url", "", "desktop gateway base URL, e.g. https://boat.example/desktop")
	desktopKey := fs.String("desktop-key-file", "", "shared HMAC key file for desktop URLs (0600)")
	desktopTTL := fs.Duration("desktop-ttl", 5*time.Minute, "desktop URL lifetime")
	images := fs.String("images", "ubuntu-24.04", "comma-separated image ids; first is the default")
	allowPrivate := fs.Bool("allow-private-webhooks", false, "allow http:// and private-network webhook targets (dev only)")
	jumpKeys := fs.String("ssh-jump-keys-file", "", "authorized_keys file for the jump host user, rewritten on ssh grants (e.g. /var/lib/boat/ssh/jump_authorized_keys)")
	auditMax := fs.Int64("audit-max-bytes", 100<<20, "rotate the audit log at this size (0 = never)")
	auditKeep := fs.Int("audit-keep", 20, "rotated audit files to keep (0 = unlimited)")
	auditAge := fs.Duration("audit-max-age", 90*24*time.Hour, "delete rotated audit files older than this (0 = never)")
	teamRate := fs.Float64("team-rate", 30, "requests/second per team, all keys together (0 = unlimited)")
	teamBurst := fs.Float64("team-burst", 120, "rate limit burst per team")
	maxKeys := fs.Int("max-keys-per-team", 100, "API keys per team (0 = unlimited)")
	maxHooks := fs.Int("max-webhooks-per-team", 20, "webhooks per team (0 = unlimited)")
	portsBase := fs.String("ports-base-url", "", "gateway base URL for port routes, e.g. https://vm.example.com (routes are <port>-<vmid>.<host>)")
	portTTL := fs.Duration("port-url-ttl", time.Hour, "default lifetime of private port URLs")
	publicPorts := fs.String("public-ports-file", "", "JSON file listing open port routes for the gateway (others need a token)")
	maxFile := fs.Int64("max-file-bytes", 256<<20, "max upload size for PUT files")
	fs.Parse(args)

	store, kr, err := c.open()
	if err != nil {
		log.Printf("boat-api: %v", err)
		return 1
	}
	cfg := Config{SSHUser: *sshUser, SSHJump: *sshJump, DesktopURL: *desktopURL, DesktopTTL: *desktopTTL,
		AllowPrivateHook: *allowPrivate, MaxFileBytes: *maxFile, JumpKeysFile: *jumpKeys,
		MaxKeysPerTeam: *maxKeys, MaxHooksPerTeam: *maxHooks, PortsBaseURL: *portsBase, PortURLTTL: *portTTL,
		PublicPortsFile: *publicPorts}
	for _, im := range strings.Split(*images, ",") {
		if im = strings.TrimSpace(im); im != "" {
			cfg.Images = append(cfg.Images, im)
		}
	}
	if *desktopKey != "" {
		if cfg.DesktopKey, err = LoadMasterKey(*desktopKey, false); err != nil {
			log.Printf("boat-api: desktop key: %v", err)
			return 1
		}
	}
	var auditW io.Writer = os.Stderr
	if *auditPath != "-" {
		p := *auditPath
		if p == "" {
			p = filepath.Join(c.dataDir, "audit.jsonl")
		}
		f, err := openRotating(p, *auditMax, *auditKeep, *auditAge)
		if err != nil {
			log.Printf("boat-api: audit log: %v", err)
			return 1
		}
		defer f.Close()
		auditW = f
	}
	s := newServer(store, kr, newBoatd(*boatd), cfg, newLimiter(*rate, *burst), &auditLog{w: auditW})
	s.teamLim = newLimiter(*teamRate, *teamBurst)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go s.hooks.watch(ctx)
	go s.syncJumpKeys(ctx)
	go s.syncPublicPorts(ctx)
	srv := &http.Server{Addr: *listen, Handler: s, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Printf("boat-api listening on %s (boatd %s)", *listen, *boatd)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("boat-api: %v", err)
		return 1
	}
	return 0
}

func newServer(store *Store, kr *Keyring, bd *boatdClient, cfg Config, lim *limiter, audit *auditLog) *server {
	if cfg.MaxFileBytes == 0 {
		cfg.MaxFileBytes = 256 << 20
	}
	if cfg.SSHUser == "" {
		cfg.SSHUser = "dev"
	}
	s := &server{store: store, keys: kr, boatd: bd, limiter: lim, audit: audit, cfg: cfg, mux: http.NewServeMux()}
	s.hooks = newDispatcher(s, cfg.AllowPrivateHook)
	s.routes()
	return s
}

func (s *server) routes() {
	s.handle("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	s.handle("GET /v1/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(openapiSpec)
	})
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { writeErr(w, r, 404, "not_found", "no such endpoint") })

	s.route("GET /v1/me", "", s.me)
	s.route("GET /v1/images", "", s.images)
	s.route("GET /v1/usage", "vms:read", s.usage)

	s.route("GET /v1/vms", "vms:read", s.listVMs)
	s.route("POST /v1/vms", "vms:write", s.createVM)
	s.route("GET /v1/vms/{id}", "vms:read", s.getVM)
	s.route("DELETE /v1/vms/{id}", "vms:write", s.deleteVM)
	s.route("POST /v1/vms/{id}/start", "vms:write", s.startVM)
	s.route("POST /v1/vms/{id}/stop", "vms:write", s.stopVM)
	s.route("POST /v1/vms/{id}/fork", "vms:write", s.forkVM)
	s.route("POST /v1/vms/{id}/exec", "exec", s.execVM)
	s.route("POST /v1/vms/{id}/prompt", "exec", s.promptVM)
	s.route("GET /v1/vms/{id}/files", "exec", s.getFile)
	s.route("PUT /v1/vms/{id}/files", "exec", s.putFile)
	s.route("GET /v1/vms/{id}/ports", "vms:read", s.listPorts)
	s.route("POST /v1/vms/{id}/ports", "vms:write", s.addPort)
	s.route("DELETE /v1/vms/{id}/ports/{port}", "vms:write", s.removePort)
	s.route("GET /v1/vms/{id}/ports/{port}/url", "exec", s.portURL)
	s.route("GET /v1/vms/{id}/snapshots", "vms:read", s.proxySnapshots)
	s.route("POST /v1/vms/{id}/snapshots", "vms:write", s.proxySnapshots)
	s.route("DELETE /v1/vms/{id}/snapshots/{sid}", "vms:write", s.proxySnapshots)
	s.route("POST /v1/vms/{id}/ssh", "exec", s.sshVM)
	s.route("GET /v1/vms/{id}/desktop", "exec", s.desktopVM)

	s.route("GET /v1/teams", "", s.listTeams)
	s.route("POST /v1/teams", "admin", s.createTeam)
	s.route("GET /v1/teams/{id}", "", s.getTeam)
	s.route("DELETE /v1/teams/{id}", "admin", s.deleteTeam)
	s.route("POST /v1/teams/{id}/members", "admin", s.addMember)
	s.route("DELETE /v1/teams/{id}/members/{user}", "admin", s.removeMember)

	s.route("GET /v1/keys", "", s.listKeys)
	s.route("POST /v1/keys", "", s.createKey)
	s.route("DELETE /v1/keys/{id}", "", s.deleteKey)

	s.route("GET /v1/secrets", "vms:read", s.listSecrets)
	s.route("PUT /v1/secrets/{name}", "admin", s.putSecret)
	s.route("DELETE /v1/secrets/{name}", "admin", s.deleteSecret)

	s.route("GET /v1/webhooks", "admin", s.listHooks)
	s.route("POST /v1/webhooks", "admin", s.createHook)
	s.route("DELETE /v1/webhooks/{id}", "admin", s.deleteHook)
	s.route("GET /v1/webhooks/{id}/deliveries", "admin", s.hookDeliveries)
	s.route("POST /v1/webhooks/{id}/test", "admin", s.testHook)
}
