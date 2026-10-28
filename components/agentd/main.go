// panda-agentd pairs a PandaOS box with PandaCode and runs agent jobs.
// It only dials out; it never listens.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	version      = "0.2.0"
	maxBackoff   = 60 * time.Second
	codeGroup    = "wheel"                            // may read the pairing code
	codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no I/O/0/1; 32 symbols so b%32 is unbiased
)

type config struct {
	relay         string
	allowInsecure bool
	slots         int
	stateDir      string
	runDir        string
	cpuQuota      string   // per-job CPUQuota=, "" for none
	jobRWPaths    []string // extra writable paths for jobs, e.g. /mem
	relayFile     string   // if it holds a URL, it overrides relay
	minFreeMB     int64    // refuse jobs below this much free space on stateDir; 0 = off
}

const maxRelayFile = 4 << 10

var cpuQuotaRe = regexp.MustCompile(`^[1-9][0-9]*%$`)

func envOr(getenv func(string) string, k, def string) string {
	if v := getenv(k); v != "" {
		return v
	}
	return def
}

func parseConfig(args []string, getenv func(string) string) (config, error) {
	env := func(k, def string) string { return envOr(getenv, k, def) }
	slots, err := strconv.Atoi(env("PANDA_AGENTD_SLOTS", "2"))
	if err != nil {
		return config{}, fmt.Errorf("PANDA_AGENTD_SLOTS: %w", err)
	}
	var c config
	fs := flag.NewFlagSet("panda-agentd", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: panda-agentd [flags]\n       panda-agentd status [-json] [-run-dir DIR]")
		fs.PrintDefaults()
	}
	fs.StringVar(&c.relay, "relay", getenv("PANDA_AGENTD_RELAY"), "relay base URL, https (env PANDA_AGENTD_RELAY)")
	fs.BoolVar(&c.allowInsecure, "allow-insecure", env("PANDA_AGENTD_ALLOW_INSECURE", "") == "1", "allow http to loopback, private, .local or single-label hosts (env PANDA_AGENTD_ALLOW_INSECURE=1)")
	fs.IntVar(&c.slots, "slots", slots, "max concurrent jobs (env PANDA_AGENTD_SLOTS)")
	fs.StringVar(&c.stateDir, "state-dir", env("PANDA_AGENTD_STATE_DIR", "/var/lib/panda-agentd"), "persistent state (env PANDA_AGENTD_STATE_DIR)")
	fs.StringVar(&c.runDir, "run-dir", env("PANDA_AGENTD_RUN_DIR", "/run/panda-agentd"), "runtime files (env PANDA_AGENTD_RUN_DIR)")
	fs.StringVar(&c.relayFile, "relay-file", env("PANDA_AGENTD_RELAY_FILE", ""), "file whose URL overrides -relay, written by panda-install --relay (default <state-dir>/relay)")
	fs.Int64Var(&c.minFreeMB, "min-free-mb", 1024, "refuse jobs when the state dir has less free space (MiB); 0 disables")
	fs.StringVar(&c.cpuQuota, "cpu-quota", env("PANDA_AGENTD_CPU_QUOTA", "200%"), `per-job CPUQuota, e.g. "200%"; "" for none`)
	fs.Func("job-rw-path", "extra absolute path jobs may write (repeatable)", func(p string) error {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, " \t\n") {
			return fmt.Errorf("want an absolute path without whitespace, got %q", p)
		}
		c.jobRWPaths = append(c.jobRWPaths, filepath.Clean(p))
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if c.relayFile == "" {
		c.relayFile = filepath.Join(c.stateDir, "relay")
	}
	switch url, err := readRelayFile(c.relayFile); {
	case err != nil:
		return config{}, err
	case url != "":
		if err := checkRelayURL(url, c.allowInsecure); err != nil {
			return config{}, fmt.Errorf("%s: %w", c.relayFile, err)
		}
		c.relay = url
	default:
		if err := checkRelayURL(c.relay, c.allowInsecure); err != nil {
			return config{}, err
		}
	}
	if c.minFreeMB < 0 {
		return config{}, fmt.Errorf("invalid -min-free-mb %d: must be >= 0", c.minFreeMB)
	}
	if c.slots < 1 {
		return config{}, fmt.Errorf("invalid -slots %d: must be >= 1", c.slots)
	}
	if c.cpuQuota != "" && !cpuQuotaRe.MatchString(c.cpuQuota) {
		return config{}, fmt.Errorf(`invalid -cpu-quota %q: want e.g. "200%%"`, c.cpuQuota)
	}
	return c, nil
}

// readRelayFile returns the URL in path, or "" if the file is missing or empty.
func readRelayFile(path string) (string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("relay file: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxRelayFile+1))
	if err != nil {
		return "", fmt.Errorf("relay file: %w", err)
	}
	if len(b) > maxRelayFile {
		return "", fmt.Errorf("relay file %s: larger than %d bytes", path, maxRelayFile)
	}
	return string(bytes.TrimSpace(b)), nil
}

type daemon struct {
	cfg            config
	run            Runner
	sem            chan struct{} // one token per running job
	heartbeatEvery time.Duration
	pairPollEvery  time.Duration
	shutdownGrace  time.Duration // how long SIGTERM waits for running jobs
	postGrace      time.Duration // then how long cancelled jobs get to report
	owner          *owner        // chown job workdirs to this user; nil = leave as is
	codeGID        int           // group of the pairing-code file; -1 = leave as is
	systemctl      systemctlFunc // nil when jobs do not run in systemd units
	freeMB         func(path string) (int64, error)
	interrupted    []string // job ids whose units outlived a previous agentd

	mu      sync.Mutex
	st      Status
	running map[string]context.CancelCauseFunc // job id -> cancel
}

type owner struct{ uid, gid int }

func newDaemon(cfg config, r Runner) *daemon {
	return &daemon{
		cfg:            cfg,
		run:            r,
		sem:            make(chan struct{}, cfg.slots),
		heartbeatEvery: 30 * time.Second,
		pairPollEvery:  2 * time.Second,
		shutdownGrace:  30 * time.Second,
		postGrace:      15 * time.Second,
		codeGID:        -1,
		freeMB:         freeMB,
		st:             Status{Relay: cfg.relay, Slots: cfg.slots, Version: version, Since: time.Now().UTC().Truncate(time.Second)},
		running:        map[string]context.CancelCauseFunc{},
	}
}

func (d *daemon) tokenPath() string  { return filepath.Join(d.cfg.stateDir, "token") }
func (d *daemon) jobsDir() string    { return filepath.Join(d.cfg.stateDir, "jobs") }
func (d *daemon) codePath() string   { return filepath.Join(d.cfg.runDir, "pairing-code") }
func (d *daemon) statusPath() string { return filepath.Join(d.cfg.runDir, "status.json") }

// setStatus applies f to a copy of the status and publishes it.
func (d *daemon) setStatus(f func(*Status)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.setStatusLocked(f)
}

func (d *daemon) setStatusLocked(f func(*Status)) {
	next := d.st
	f(&next)
	d.st = next
	if err := writeStatus(d.statusPath(), next); err != nil {
		slog.Warn("write status failed", "err", err)
	}
}

// noteErr records the outcome of a relay call in status.json: err sets
// lastError, nil clears it. Only changes are written.
func (d *daemon) noteErr(what string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		if d.st.LastError != "" {
			d.setStatusLocked(func(s *Status) { s.LastError, s.LastErrorAt = "", nil })
		}
		return
	}
	msg := what + ": " + err.Error()
	if msg == d.st.LastError {
		return
	}
	now := time.Now().UTC().Truncate(time.Second)
	d.setStatusLocked(func(s *Status) { s.LastError, s.LastErrorAt = msg, &now })
}

func (d *daemon) status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.st
}

func (d *daemon) setup() error {
	if err := os.MkdirAll(d.cfg.stateDir, 0o700); err != nil {
		return err
	}
	if d.owner != nil {
		// The job user must traverse stateDir to reach its workdir; the token stays 0600.
		if err := os.Chmod(d.cfg.stateDir, 0o711); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(d.jobsDir(), 0o711); err != nil {
		return err
	}
	return os.MkdirAll(d.cfg.runDir, 0o755)
}

// Run pairs if needed, then serves until ctx ends. A 401 drops the token and re-pairs.
func (d *daemon) Run(ctx context.Context) error {
	if err := d.setup(); err != nil {
		return err
	}
	if d.systemctl != nil {
		ids, err := cleanupUnits(ctx, d.systemctl)
		if err != nil {
			slog.Warn("cleaning up leftover job units", "err", err)
		}
		if len(ids) > 0 {
			slog.Warn("stopped job units left by a previous agentd", "jobs", ids)
		}
		d.interrupted = ids
	}
	for {
		token, err := d.loadToken()
		if err != nil {
			if token, err = d.pair(ctx); err != nil {
				return err
			}
		}
		d.setStatus(func(s *Status) { s.Paired, s.Pairing = true, false })
		err = d.serve(ctx, newClient(d.cfg.relay, token))
		if !errors.Is(err, errUnauthorized) {
			return err
		}
		slog.Warn("relay rejected token; re-pairing")
		d.setStatus(func(s *Status) { s.Paired = false })
		if err := os.Remove(d.tokenPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
}

func (d *daemon) loadToken() (string, error) {
	b, err := os.ReadFile(d.tokenPath())
	if t := string(bytes.TrimSpace(b)); err == nil && t != "" {
		return t, nil
	}
	return "", errors.New("no token")
}

func (d *daemon) saveToken(token string) error {
	return writeFileAtomic(d.tokenPath(), []byte(token+"\n"), 0o600, -1)
}

func (d *daemon) pair(ctx context.Context) (string, error) {
	c := newClient(d.cfg.relay, "")
	for {
		code, err := pairingCode()
		if err != nil {
			return "", err
		}
		// Whoever reads the code can claim the box: root and wheel only.
		if err := writeFileAtomic(d.codePath(), []byte(code+"\n"), 0o640, d.codeGID); err != nil {
			slog.Warn("write pairing code failed", "err", err)
		}
		d.setStatus(func(s *Status) { s.Paired, s.Pairing = false, true })
		// The journal is readable by the same audience (root, wheel,
		// systemd-journal). "pairing code:" is grepped by the NixOS VM tests.
		slog.Info("pairing code: "+code, "code", code)
		token, err := d.pairWith(ctx, c, code)
		if errors.Is(err, errPairExpired) {
			slog.Info("pairing code expired; generating a new one")
			continue
		}
		if err != nil {
			return "", err
		}
		os.Remove(d.codePath())
		d.setStatus(func(s *Status) { s.Pairing = false })
		if err := d.saveToken(token); err != nil {
			return "", fmt.Errorf("save token: %w", err)
		}
		slog.Info("paired")
		return token, nil
	}
}

func (d *daemon) pairWith(ctx context.Context, c *client, code string) (string, error) {
	caps := d.caps()
	if err := d.retry(ctx, "pair start", func() error { return c.pairStart(ctx, code, caps) }); err != nil {
		return "", err
	}
	for {
		var token string
		err := d.retry(ctx, "pair status", func() (err error) {
			token, err = c.pairStatus(ctx, code)
			return err
		})
		if err != nil || token != "" {
			return token, err
		}
		if !sleep(ctx, d.pairPollEvery) {
			return "", ctx.Err()
		}
	}
}

func pairingCode() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	return string(b), nil
}

func (d *daemon) caps() Caps {
	host, _ := os.Hostname()
	total, avail := meminfo("/proc/meminfo")
	return Caps{
		Hostname:   host,
		CPUs:       runtime.NumCPU(),
		MemTotalMB: total,
		MemAvailMB: avail,
		Slots:      d.cfg.slots,
		FreeSlots:  d.cfg.slots - len(d.sem),
		Version:    version,
	}
}

// meminfo returns MemTotal and MemAvailable in MiB; 0 for anything missing.
func meminfo(path string) (totalMB, availMB int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		var key string
		var kb int
		if _, err := fmt.Sscanf(s.Text(), "%s %d", &key, &kb); err != nil {
			continue
		}
		switch key {
		case "MemTotal:":
			totalMB = kb / 1024
		case "MemAvailable:":
			availMB = kb / 1024
		}
	}
	return totalMB, availMB
}

// retry calls f until it succeeds, the relay says 401 / unknown code, or ctx ends.
func (d *daemon) retry(ctx context.Context, what string, f func() error) error {
	for attempt := 0; ; attempt++ {
		err := f()
		if err == nil {
			d.noteErr(what, nil)
			return nil // even if ctx just ended: a job we were handed must not be dropped
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if errors.Is(err, errPairExpired) {
			return err
		}
		d.noteErr(what, err)
		if errors.Is(err, errUnauthorized) {
			return err
		}
		slog.Warn(what+" failed; retrying", "err", err, "attempt", attempt+1)
		if !sleep(ctx, backoff(attempt)) {
			return context.Cause(ctx)
		}
	}
}

// backoff is exponential from 1s, capped at maxBackoff, with jitter in [d/2, d].
func backoff(attempt int) time.Duration {
	d := min(time.Second<<min(attempt, 6), maxBackoff)
	return d/2 + mrand.N(d/2+1)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// newLogger logs JSON (for journald) unless w is a terminal.
func newLogger(w *os.File) *slog.Logger {
	if fi, err := w.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return slog.New(slog.NewTextHandler(w, nil))
	}
	return slog.New(slog.NewJSONHandler(w, nil))
}

func lookupOwner(name string) (*owner, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err := errors.Join(err1, err2); err != nil {
		return nil, err
	}
	return &owner{uid, gid}, nil
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "status" {
		if err := statusCmd(os.Args[2:], os.Getenv, os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "panda-agentd status:", err)
			os.Exit(1)
		}
		return
	}
	slog.SetDefault(newLogger(os.Stderr))
	cfg, err := parseConfig(os.Args[1:], os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fatal("bad config", err)
	}
	runner, kind, sc := defaultRunner(cfg)
	d := newDaemon(cfg, runner)
	d.systemctl = sc
	if sc != nil {
		if d.owner, err = lookupOwner(jobUser); err != nil {
			fatal("look up job user "+jobUser, err)
		}
	}
	if os.Geteuid() == 0 {
		if g, err := user.LookupGroup(codeGroup); err == nil {
			d.codeGID, _ = strconv.Atoi(g.Gid)
		} else {
			slog.Warn("pairing code will be root-only", "group", codeGroup, "err", err)
		}
	}
	slog.Info("panda-agentd starting", "version", version, "relay", cfg.relay, "slots", cfg.slots, "runner", kind, "insecure", cfg.allowInsecure)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := d.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fatal("panda-agentd stopped", err)
	}
	slog.Info("panda-agentd stopped")
}
