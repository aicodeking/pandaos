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
	"log"
	mrand "math/rand/v2"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	version      = "0.1.0"
	maxBackoff   = 60 * time.Second
	codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no I/O/0/1; 32 symbols so b%32 is unbiased
)

type config struct {
	relay    string
	slots    int
	stateDir string
	runDir   string
}

func parseConfig(args []string, getenv func(string) string) (config, error) {
	env := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}
	slots, err := strconv.Atoi(env("PANDA_AGENTD_SLOTS", "2"))
	if err != nil {
		return config{}, fmt.Errorf("PANDA_AGENTD_SLOTS: %w", err)
	}
	var c config
	fs := flag.NewFlagSet("panda-agentd", flag.ContinueOnError)
	fs.StringVar(&c.relay, "relay", getenv("PANDA_AGENTD_RELAY"), "relay base URL, http(s) (env PANDA_AGENTD_RELAY)")
	fs.IntVar(&c.slots, "slots", slots, "max concurrent jobs (env PANDA_AGENTD_SLOTS)")
	fs.StringVar(&c.stateDir, "state-dir", env("PANDA_AGENTD_STATE_DIR", "/var/lib/panda-agentd"), "persistent state (env PANDA_AGENTD_STATE_DIR)")
	fs.StringVar(&c.runDir, "run-dir", env("PANDA_AGENTD_RUN_DIR", "/run/panda-agentd"), "runtime files (env PANDA_AGENTD_RUN_DIR)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	u, err := url.Parse(c.relay)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return config{}, fmt.Errorf("invalid -relay %q: want http(s)://host[:port]", c.relay)
	}
	if c.slots < 1 {
		return config{}, fmt.Errorf("invalid -slots %d: must be >= 1", c.slots)
	}
	return c, nil
}

type daemon struct {
	cfg            config
	run            Runner
	sem            chan struct{} // one token per running job
	heartbeatEvery time.Duration
	pairPollEvery  time.Duration
}

func newDaemon(cfg config, r Runner) *daemon {
	return &daemon{
		cfg:            cfg,
		run:            r,
		sem:            make(chan struct{}, cfg.slots),
		heartbeatEvery: 30 * time.Second,
		pairPollEvery:  2 * time.Second,
	}
}

func (d *daemon) tokenPath() string { return filepath.Join(d.cfg.stateDir, "token") }
func (d *daemon) codePath() string  { return filepath.Join(d.cfg.runDir, "pairing-code") }

// Run pairs if needed, then serves until ctx ends. A 401 drops the token and re-pairs.
func (d *daemon) Run(ctx context.Context) error {
	if err := os.MkdirAll(d.cfg.stateDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(d.cfg.runDir, 0o755); err != nil {
		return err
	}
	for {
		token, err := d.loadToken()
		if err != nil {
			if token, err = d.pair(ctx); err != nil {
				return err
			}
		}
		err = d.serve(ctx, newClient(d.cfg.relay, token))
		if !errors.Is(err, errUnauthorized) {
			return err
		}
		log.Print("relay rejected token; re-pairing")
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
	f, err := os.CreateTemp(d.cfg.stateDir, ".token-*") // created 0600
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(token + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), d.tokenPath())
}

func (d *daemon) pair(ctx context.Context) (string, error) {
	c := newClient(d.cfg.relay, "")
	for {
		code, err := pairingCode()
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(d.codePath(), []byte(code+"\n"), 0o644); err != nil {
			log.Printf("write pairing code: %v", err)
		}
		log.Printf("pairing code: %s", code)
		token, err := d.pairWith(ctx, c, code)
		if errors.Is(err, errPairExpired) {
			log.Print("pairing code expired; generating a new one")
			continue
		}
		if err != nil {
			return "", err
		}
		os.Remove(d.codePath())
		if err := d.saveToken(token); err != nil {
			return "", fmt.Errorf("save token: %w", err)
		}
		log.Print("paired")
		return token, nil
	}
}

func (d *daemon) pairWith(ctx context.Context, c *client, code string) (string, error) {
	caps := d.caps()
	if err := retry(ctx, "pair start", func() error { return c.pairStart(ctx, code, caps) }); err != nil {
		return "", err
	}
	for {
		var token string
		err := retry(ctx, "pair status", func() (err error) {
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

// serve heartbeats and pulls jobs until ctx ends or the relay returns 401.
func (d *daemon) serve(parent context.Context, c *client) error {
	ctx, cancel := context.WithCancelCause(parent)
	fail := func(err error) {
		if errors.Is(err, errUnauthorized) {
			cancel(err)
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); d.heartbeat(ctx, c, fail) }()

	for ctx.Err() == nil {
		select {
		case d.sem <- struct{}{}: // wait for a free slot before asking for work
		case <-ctx.Done():
			continue
		}
		var job *Job
		err := retry(ctx, "next job", func() (err error) {
			job, err = c.nextJob(ctx)
			return err
		})
		if err != nil || job == nil {
			<-d.sem
			fail(err)
			continue
		}
		wg.Add(1)
		go func(j Job) {
			defer wg.Done()
			defer func() { <-d.sem }()
			d.handle(ctx, c, j, fail)
		}(*job)
	}
	cancel(nil)
	wg.Wait()
	return context.Cause(ctx)
}

func (d *daemon) heartbeat(ctx context.Context, c *client, fail func(error)) {
	t := time.NewTicker(d.heartbeatEvery)
	defer t.Stop()
	for {
		if err := c.heartbeat(ctx, d.caps()); err != nil && ctx.Err() == nil {
			log.Printf("heartbeat: %v", err)
			fail(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (d *daemon) handle(ctx context.Context, c *client, j Job, fail func(error)) {
	log.Printf("job %s: start %q", j.ID, j.Argv)
	res := Result{ExitCode: -1, Stderr: "panda-agentd: empty argv"}
	if len(j.Argv) > 0 {
		res = d.run(ctx, j)
	}
	res = Result{ExitCode: res.ExitCode, Stdout: truncate(res.Stdout), Stderr: truncate(res.Stderr)}
	log.Printf("job %s: exit %d", j.ID, res.ExitCode)
	fail(retry(ctx, "post result "+j.ID, func() error { return c.postResult(ctx, j.ID, res) }))
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
func retry(ctx context.Context, what string, f func() error) error {
	for attempt := 0; ; attempt++ {
		err := f()
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if err == nil || errors.Is(err, errUnauthorized) || errors.Is(err, errPairExpired) {
			return err
		}
		log.Printf("%s: %v (retrying)", what, err)
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

func main() {
	cfg, err := parseConfig(os.Args[1:], os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	runner, kind := defaultRunner()
	log.Printf("panda-agentd %s: relay=%s slots=%d runner=%s", version, cfg.relay, cfg.slots, kind)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newDaemon(cfg, runner).Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
