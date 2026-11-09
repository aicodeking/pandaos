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
	st, err := OpenSealedStore(c.dataDir, kr)
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

// headerFlag collects repeated k=v flags.
type headerFlag map[string]string

func (h *headerFlag) String() string { return "" }
func (h *headerFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("want k=v")
	}
	if *h == nil {
		*h = headerFlag{}
	}
	(*h)[k] = val
	return nil
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
	vmDomain := fs.String("vm-domain", "", "domain of gateway port routes <port>-<vmid>.<vm-domain> (https; a scheme://host[:port] value is used as is)")
	portTTL := fs.Duration("port-url-ttl", time.Hour, "default lifetime of private port URLs")
	aiListen := fs.String("ai-proxy-listen", "", "address serving ONLY the AI proxy to VMs, e.g. 10.200.0.1:8744")
	aiURL := fs.String("ai-proxy-url", "", "proxy URL as VMs see it (default http://<ai-proxy-listen>)")
	aiRate := fs.Float64("ai-rate", 20, "AI proxy requests/second per VM (0 = unlimited)")
	publicURL := fs.String("public-url", "", "this API's public base URL, for signed snapshot links")
	maxImport := fs.Int64("max-import-bytes", 200<<30, "max snapshot archive size for import")
	otlp := fs.String("otlp-endpoint", "", "OTLP/HTTP base URL for request traces + VM lifecycle logs")
	var otlpHeaders headerFlag
	fs.Var(&otlpHeaders, "otlp-header", "k=v header for -otlp-endpoint (repeatable)")
	hookTeam := fs.Int("webhook-team-inflight", 8, "webhook HTTP attempts in flight per team")
	hookPending := fs.Int("webhook-team-pending", 1000, "webhook deliveries pending per team; more are dropped and logged")
	maxFile := fs.Int64("max-file-bytes", 256<<20, "max upload size for PUT files")
	teamStreams := fs.Int("team-streams", defaultTeamStreams, "exec, SSE, log and download streams open per team (0 = unlimited)")
	teamUploads := fs.Int("team-uploads", defaultTeamUploads, "file uploads and snapshot imports in flight per team (0 = unlimited)")
	aiInflight := fs.Int("ai-vm-inflight", defaultAIVMInflight, "AI proxy requests in flight per VM (0 = unlimited)")
	fs.Parse(args)

	store, kr, err := c.open()
	if err != nil {
		log.Printf("boat-api: %v", err)
		return 1
	}
	cfg := Config{SSHUser: *sshUser, SSHJump: *sshJump, DesktopURL: *desktopURL, DesktopTTL: *desktopTTL,
		AllowPrivateHook: *allowPrivate, MaxFileBytes: *maxFile, JumpKeysFile: *jumpKeys,
		MaxKeysPerTeam: *maxKeys, MaxHooksPerTeam: *maxHooks, VMDomain: *vmDomain, PortURLTTL: *portTTL, AIProxyURL: *aiURL, PublicURL: *publicURL, MaxImportBytes: *maxImport}
	if cfg.AIProxyURL == "" && *aiListen != "" {
		cfg.AIProxyURL = "http://" + *aiListen
	}
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
	s.aiLim = newLimiter(*aiRate, *aiRate*5)
	s.hooks.teamInflight, s.hooks.teamPending = *hookTeam, *hookPending
	s.streams, s.uploads, s.aiInflight = newInflight(*teamStreams), newInflight(*teamUploads), newInflight(*aiInflight)
	if *otlp != "" {
		s.otel = newOTel(*otlp, otlpHeaders)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	s.ctx = ctx
	go s.hooks.watch(ctx)
	go s.otel.run(ctx)
	go s.aiMeterLoop(ctx)
	if *aiListen != "" {
		go s.serveAIProxy(ctx, *aiListen)
	}
	go s.syncJumpKeys(ctx)
	srv := &http.Server{Addr: *listen, Handler: s, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Printf("boat-api listening on %s (boatd %s)", *listen, *boatd)
	err = srv.ListenAndServe()
	if ferr := store.Flush(); ferr != nil { // what Apply left in memory
		log.Printf("boat-api: write state: %v", ferr)
	}
	if err != nil && err != http.ErrServerClosed {
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
	if cfg.MaxImportBytes == 0 {
		cfg.MaxImportBytes = 200 << 30
	}
	s := &server{store: store, keys: kr, boatd: bd, limiter: lim, audit: audit, cfg: cfg, mux: http.NewServeMux(),
		aiMeter: newAIMeter(), aiClient: newAIClient(),
		streams: newInflight(defaultTeamStreams), uploads: newInflight(defaultTeamUploads), aiInflight: newInflight(defaultAIVMInflight)}
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
	s.mux.HandleFunc("GET /internal/route-gens", s.routeGens) // boat-gateway; Caddy never publishes /internal/

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
	s.route("POST /v1/vms/{id}/port-tokens/revoke", "exec", s.revokePortTokens)
	s.route("GET /v1/vms/{id}/snapshots", "vms:read", s.proxySnapshots)
	s.route("POST /v1/vms/{id}/snapshots", "vms:write", s.proxySnapshots)
	s.route("DELETE /v1/vms/{id}/snapshots/{sid}", "vms:write", s.proxySnapshots)
	s.route("POST /v1/vms/{id}/resize", "vms:write", s.resizeVM)
	s.route("GET /v1/vms/{id}/usage", "vms:read", s.vmUsage)
	s.route("GET /v1/events", "vms:read", s.streamEvents)

	s.route("POST /v1/vms/{id}/commands", "exec", s.startCommand)
	s.route("GET /v1/vms/{id}/commands", "exec", s.listCommands)
	s.route("GET /v1/vms/{id}/commands/{cid}", "exec", s.commandStatus)
	s.route("GET /v1/vms/{id}/commands/{cid}/logs", "exec", s.commandLogs)
	s.route("POST /v1/vms/{id}/commands/{cid}/signal", "exec", s.signalCommand)
	s.route("DELETE /v1/vms/{id}/commands/{cid}", "exec", s.stopCommand)
	s.route("GET /v1/vms/{id}/command-history", "exec", s.commandHistory)
	s.route("GET /v1/vms/{id}/processes", "exec", s.listProcesses)

	s.route("GET /v1/vms/{id}/files/list", "exec", s.listFiles)
	s.route("DELETE /v1/vms/{id}/files", "exec", s.deleteFile)
	s.route("POST /v1/vms/{id}/files/move", "exec", s.moveFile)
	s.route("GET /v1/vms/{id}/files/search", "exec", s.searchFiles)
	s.route("GET /v1/vms/{id}/files/info", "exec", s.fileInfo)
	s.route("POST /v1/vms/{id}/files/folder", "exec", s.makeFolder)
	s.route("POST /v1/vms/{id}/files/permissions", "exec", s.chmodFile)

	s.route("POST /v1/vms/{id}/prompts", "exec", s.startPrompt)
	s.route("GET /v1/vms/{id}/prompts", "exec", s.listPrompts)
	s.route("GET /v1/vms/{id}/prompts/{pid}", "exec", s.getPrompt)
	s.route("GET /v1/vms/{id}/prompts/{pid}/logs", "exec", s.promptLogs)
	s.route("POST /v1/vms/{id}/prompts/{pid}/steer", "exec", s.steerPrompt)
	s.route("POST /v1/vms/{id}/prompts/{pid}/interrupt", "exec", s.interruptPrompt)
	s.route("GET /v1/vms/{id}/conversations", "exec", s.listConversations)

	s.route("GET /v1/vms/{id}/snapshots/{sid}/download", "exec", s.downloadSnapshot)
	s.route("POST /v1/vms/{id}/snapshots/{sid}/shares", "admin", s.createShare)
	s.route("GET /v1/vms/{id}/snapshots/{sid}/shares", "admin", s.listShares)
	s.route("DELETE /v1/vms/{id}/snapshots/{sid}/shares/{share}", "admin", s.revokeShare)
	s.route("GET /v1/snapshots", "vms:read", s.listTeamSnapshots)
	s.route("DELETE /v1/snapshots/{sid}", "vms:write", s.deleteTeamSnapshot)
	s.route("POST /v1/snapshots/import", "vms:write", s.importSnapshot)
	s.route("POST /v1/snapshots/import-share", "vms:write", s.importShare)
	s.route("GET /v1/shared-snapshots", "vms:read", s.sharedWithMe)
	s.handle("GET /v1/snapshot-links/{code}/download", s.linkDownload)

	s.route("GET /v1/environments", "vms:read", s.listEnvs)
	s.route("POST /v1/environments", "admin", s.createEnv)
	s.route("GET /v1/environments/{id}", "vms:read", s.getEnv)
	s.route("PUT /v1/environments/{id}", "admin", s.updateEnv)
	s.route("DELETE /v1/environments/{id}", "admin", s.deleteEnv)
	s.route("POST /v1/environments/{id}/build", "admin", s.buildEnv)

	s.route("GET /v1/ai/credentials", "admin", s.listAICreds)
	s.route("PUT /v1/ai/credentials/{provider}", "admin", s.putAICred)
	s.route("DELETE /v1/ai/credentials/{provider}", "admin", s.deleteAICred)
	s.route("GET /v1/ai/proxy", "vms:read", s.getAIProxy)
	s.route("PUT /v1/ai/proxy", "admin", s.putAIProxy)
	s.route("GET /v1/ai/usage", "vms:read", s.teamAIUsage)
	s.route("GET /v1/vms/{id}/ai-usage", "vms:read", s.vmAIUsage)
	s.route("PUT /v1/vms/{id}/ai-access", "admin", s.setAIAccess)

	s.route("GET /v1/telemetry", "admin", s.getTelemetry)
	s.route("PUT /v1/telemetry", "admin", s.putTelemetry)
	s.route("DELETE /v1/telemetry", "admin", s.deleteTelemetry)

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
	s.route("POST /v1/keys/{id}/rotate", "", s.rotateKey)
	s.route("GET /v1/keys/{id}/usage", "", s.keyUsageView)

	s.route("GET /v1/secrets", "vms:read", s.listSecrets)
	s.route("PUT /v1/secrets/{name}", "admin", s.putSecret)
	s.route("DELETE /v1/secrets/{name}", "admin", s.deleteSecret)

	s.route("GET /v1/webhooks", "admin", s.listHooks)
	s.route("POST /v1/webhooks", "admin", s.createHook)
	s.route("GET /v1/webhooks/{id}", "admin", s.getHook)
	s.route("PATCH /v1/webhooks/{id}", "admin", s.updateHook)
	s.route("DELETE /v1/webhooks/{id}", "admin", s.deleteHook)
	s.route("POST /v1/webhooks/{id}/rotate", "admin", s.rotateHook)
	s.route("GET /v1/webhooks/{id}/deliveries", "admin", s.hookDeliveries)
	s.route("POST /v1/webhooks/{id}/test", "admin", s.testHook)
}
