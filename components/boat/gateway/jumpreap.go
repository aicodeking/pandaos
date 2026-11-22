package main

// Jump-session revocation (r28).
//
// sshd authorizes the `jump` user once per connection (AuthorizedKeysCommand),
// so an open `ssh -J` session would outlive grant revocation, key deletion,
// member removal, VM deletion and IP reuse. Three cooperating pieces fix that:
//
//  1. `boat-gateway jump-keys` (run by sshd as boat-ssh) records, for the sshd
//     connection that invoked it (its parent pid + that pid's start time), the
//     exact authorized_keys lines the API granted for every offered key.
//  2. `boat-gateway jump-reaper` (root, systemd service) re-asks the API for
//     each recorded key every few seconds (identical keys are looked up once
//     per pass, a few in parallel). The connection's sshd processes are killed
//     when the API answers 204 (authorization gone: key revoked, member removed,
//     VM deleted), or when a permitopen target it was granted is no longer
//     offered AND boatd's routes.json (-routes) lists a running VM at that
//     address (or quarantines it): the API only offers running, policy-current
//     VMs the key is entitled to, so an address that is still occupied but no
//     longer offered means the grant was revoked or the IP now belongs to
//     something else. A target that merely vanished (its VM stopped or was
//     deleted: it is in no routes.json entry) and targets that were only added
//     never end a session, so a teammate stopping an unrelated VM cannot kill
//     it. If the API cannot be reached or answers garbage for longer than
//     -max-stale the connection is killed as well, and the same applies while
//     routes.json cannot be read for a removed target.
//  3. The reaper stamps a heartbeat file (reaper.alive in the state dir) at the
//     start and end of every pass. The helper refuses to authorize (and so to
//     print keys) while it is older than jumpHeartbeatMaxAge or missing, so a
//     dead or wedged reaper cannot leave new sessions unrevocable.
//
// Records are written by an unprivileged helper and read by root, so the
// reaper treats them as untrusted: strict file names and size caps, the pid
// must still be the process the helper saw (same start time), must be an sshd
// process that is not the listener (parent pid 1), and must own a child
// process running as the jump user, so a forged record can never be used to
// kill another user's ssh session.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	jumpStateDir       = "/run/boat-jump"
	jumpMaxRecordKeys  = 16
	jumpMaxRecordBytes = 1 << 20

	jumpRoutesPath    = "/run/boat/routes.json"
	jumpHeartbeatName = "reaper.alive"
	// jumpHeartbeatMaxAge must exceed the slowest healthy gap between stamps: a
	// pass (<= jumpDefaultBudget + one lookup timeout) plus the interval.
	jumpHeartbeatMaxAge = 30 * time.Second

	jumpUser            = "jump"
	jumpLookupWorkers   = 4
	jumpDefaultBudget   = 15 * time.Second
	jumpAllowUntrackEnv = "BOAT_JUMP_ALLOW_UNTRACKED"
)

// errJumpNoAuth is runJumpKeys' answer for HTTP 204: boat-api no longer
// authorizes the key at all (as opposed to being unreachable or malformed).
var errJumpNoAuth = errors.New("authorization gone (204)")

// jumpAllowUntracked is the explicit opt-out of fail-closed recording for
// manual runs and tests. sshd runs the helper with an empty environment and
// the connecting client cannot influence it, so it is not an attacker lever.
func jumpAllowUntracked() bool { return os.Getenv(jumpAllowUntrackEnv) == "1" }

// jumpRecordDir is where the helper records sessions (sshd gives it an empty
// environment, so the override exists for tests and manual runs only).
func jumpRecordDir() string {
	if d := os.Getenv("BOAT_JUMP_STATE_DIR"); d != "" {
		return d
	}
	return jumpStateDir
}

var jumpRecordNameRe = regexp.MustCompile(`\A([1-9][0-9]{0,9})\.json\z`)

type jumpRecordKey struct {
	Type  string   `json:"type"`
	Blob  string   `json:"blob"`
	Lines []string `json:"lines"`
}

type jumpRecord struct {
	PID   int             `json:"pid"`
	Start string          `json:"start"`
	Keys  []jumpRecordKey `json:"keys"`
}

// procfs reads the few /proc facts the reaper needs; root is injectable for tests.
type procfs struct{ root string }

func (p procfs) read(pid int, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(p.root, strconv.Itoa(pid), name))
}

// stat returns (ppid, starttime) from /proc/<pid>/stat.
func (p procfs) stat(pid int) (ppid int, start string, err error) {
	b, err := p.read(pid, "stat")
	if err != nil {
		return 0, "", err
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, "", errors.New("bad stat")
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return 0, "", errors.New("short stat")
	}
	ppid, err = strconv.Atoi(f[1])
	if err != nil {
		return 0, "", err
	}
	return ppid, f[19], nil
}

// uid returns the real uid from /proc/<pid>/status.
func (p procfs) uid(pid int) (int, error) {
	b, err := p.read(pid, "status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			f := strings.Fields(rest)
			if len(f) == 0 {
				break
			}
			return strconv.Atoi(f[0])
		}
	}
	return 0, errors.New("no Uid in status")
}

func (p procfs) isSSHD(pid int) bool {
	b, err := p.read(pid, "cmdline")
	if err != nil || len(b) == 0 {
		return false
	}
	argv0, _, _ := bytes.Cut(b, []byte{0})
	return strings.HasPrefix(filepath.Base(strings.TrimSpace(string(argv0))), "sshd")
}

// childMap scans /proc once and returns every process's children by parent
// pid. The reaper builds one per pass instead of one scan per session.
func (p procfs) childMap() map[int][]int {
	ents, err := os.ReadDir(p.root)
	if err != nil {
		return nil
	}
	out := make(map[int][]int)
	for _, e := range ents {
		c, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if pp, _, err := p.stat(c); err == nil {
			out[pp] = append(out[pp], c)
		}
	}
	return out
}

// checkJumpHeartbeat denies while the reaper is not demonstrably running: a
// session recorded with no reaper to read it could never be revoked.
func checkJumpHeartbeat(dir string, now time.Time) error {
	fi, err := os.Lstat(filepath.Join(dir, jumpHeartbeatName))
	if err != nil {
		return fmt.Errorf("jump reaper heartbeat missing: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return errors.New("jump reaper heartbeat is not a regular file")
	}
	if age := now.Sub(fi.ModTime()); age > jumpHeartbeatMaxAge || age < -jumpHeartbeatMaxAge {
		return fmt.Errorf("jump reaper heartbeat is %v old (max %v): reaper is not running", age.Round(time.Second), jumpHeartbeatMaxAge)
	}
	return nil
}

// recordJumpSession is called by the AuthorizedKeysCommand helper. It does
// nothing (nil) when nothing was authorized. A failure to record must deny the
// authentication: an untracked session could never be revoked. That includes a
// parent that cannot be positively identified as a connection sshd the reaper
// can later signal (an sshd process whose own parent is not the listener), so
// the helper fails closed; allowUntracked is the explicit opt-out for manual
// runs and tests (BOAT_JUMP_ALLOW_UNTRACKED=1).
func recordJumpSession(proc procfs, dir string, ppid int, keyType, keyBlob string, lines []string, allowUntracked bool) error {
	if len(lines) == 0 {
		return nil
	}
	if !allowUntracked {
		if err := checkJumpHeartbeat(dir, time.Now()); err != nil {
			return err
		}
	}
	if ppid <= 1 || !proc.isSSHD(ppid) {
		if allowUntracked {
			return nil
		}
		return errors.New("parent is not an identifiable sshd connection; refusing untracked session")
	}
	pp, start, err := proc.stat(ppid)
	if err != nil {
		return err
	}
	if pp <= 1 && !allowUntracked {
		return errors.New("parent sshd has no listener; refusing untracked session")
	}
	path := filepath.Join(dir, strconv.Itoa(ppid)+".json")
	rec := jumpRecord{PID: ppid, Start: start}
	if b, err := os.ReadFile(path); err == nil {
		var old jumpRecord
		if json.Unmarshal(b, &old) == nil && old.PID == ppid && old.Start == start {
			rec = old
		}
	}
	keys := make([]jumpRecordKey, 0, len(rec.Keys)+1)
	for _, k := range rec.Keys {
		if k.Type != keyType || k.Blob != keyBlob {
			keys = append(keys, k)
		}
	}
	keys = append(keys, jumpRecordKey{Type: keyType, Blob: keyBlob, Lines: slices.Clone(lines)})
	if len(keys) > jumpMaxRecordKeys {
		return errors.New("too many keys offered on one connection")
	}
	rec.Keys = keys
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if len(b) > jumpMaxRecordBytes {
		// The reaper refuses to read a record this large; authorizing would
		// leave the session untrackable.
		return errors.New("jump session record too large; refusing untracked session")
	}
	tmp, err := os.CreateTemp(dir, ".rec-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Not secret (public key + VM addresses); the root reaper reads it without
	// needing CAP_DAC_READ_SEARCH.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

type jumpReaper struct {
	proc procfs
	dir  string
	// lookup returns the lines the API grants now. (nil, nil) means the API
	// answered 204: the authorization is gone. Any error (transport, 5xx,
	// malformed) is "cannot verify" and only matters after maxStale. It must be
	// safe for concurrent use.
	lookup     func(keyType, keyBlob string) ([]string, error)
	kill       func(pid int, sig syscall.Signal) error
	now        func() time.Time
	maxStale   time.Duration
	jumpUID    int           // uid the authenticated session child runs as
	passBudget time.Duration // 0 = unbounded; lookups not started in time count as errors

	// occupied returns the addresses boatd's routes.json currently gives to a
	// running VM (or quarantines). nil = unknown: removed targets never kill.
	occupied func() (map[string]bool, error)

	lastOK map[int]time.Time // pid -> last successful verification (or first sight)
	termed map[int]bool      // pid -> SIGTERM already sent
	scans  int               // /proc scans performed (tests)
	kids   map[int][]int     // ppid -> children, from one /proc scan per pass
}

// jumpOccupied reads routes.json (boatd's /run/boat/routes.json) for the IPs a
// running VM holds, plus quarantined running VMs' IPs.
func jumpOccupied(path string) func() (map[string]bool, error) {
	return func() (map[string]bool, error) {
		vms, quarantined, err := readRoutes(path)
		if err != nil {
			return nil, err
		}
		out := make(map[string]bool, len(vms)+len(quarantined))
		for _, v := range vms {
			if ip := normalizeIP(v.IP); ip != "" {
				out[ip] = true
			}
		}
		for ip := range quarantined {
			out[ip] = true
		}
		return out, nil
	}
}

// occupancy loads the occupied set at most once per pass, and only when a
// removed target needs it.
type occupancy struct {
	load func() (map[string]bool, error)
	done bool
	set  map[string]bool
	err  error
}

func (o *occupancy) get() (map[string]bool, error) {
	if !o.done {
		o.done = true
		if o.load == nil {
			o.set = map[string]bool{}
		} else {
			o.set, o.err = o.load()
		}
	}
	return o.set, o.err
}

// beat stamps the heartbeat file the helper checks (atomic, world-readable).
func (r *jumpReaper) beat() {
	tmp, err := os.CreateTemp(r.dir, ".beat-*")
	if err == nil {
		defer os.Remove(tmp.Name())
		_, err = tmp.WriteString(strconv.FormatInt(r.now().Unix(), 10) + "\n")
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(tmp.Name(), 0o644)
		}
		if err == nil {
			err = os.Rename(tmp.Name(), filepath.Join(r.dir, jumpHeartbeatName))
		}
	}
	if err != nil {
		log.Printf("jump-reaper: cannot stamp heartbeat (new logins will be denied): %v", err)
	}
}

// jumpAPILookup asks boat-api like sshd's AuthorizedKeysCommand does, but maps
// the API's 204 (authorization gone) to no lines instead of an error.
func jumpAPILookup(api string, timeout time.Duration) func(keyType, keyBlob string) ([]string, error) {
	return func(t, b string) ([]string, error) {
		lines, err := runJumpKeys([]string{api, t, b, jumpUser}, timeout)
		if errors.Is(err, errJumpNoAuth) {
			return nil, nil
		}
		return lines, err
	}
}

func newJumpReaper(dir, api, routesPath string, maxStale time.Duration, jumpUID int) *jumpReaper {
	return &jumpReaper{
		proc: procfs{root: "/proc"}, dir: dir, maxStale: maxStale, now: time.Now,
		jumpUID: jumpUID, passBudget: jumpDefaultBudget,
		lookup:   jumpAPILookup(api, 2*time.Second),
		occupied: jumpOccupied(routesPath),
		kill:     func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) },
		lastOK:   map[int]time.Time{}, termed: map[int]bool{},
	}
}

func readJumpRecord(path string) (jumpRecord, bool) {
	var rec jumpRecord
	f, err := os.Open(path)
	if err != nil {
		return rec, false
	}
	defer f.Close()
	var buf bytes.Buffer
	if n, err := buf.ReadFrom(&limitedReader{f, jumpMaxRecordBytes + 1}); err != nil || n > jumpMaxRecordBytes {
		return rec, false
	}
	if json.Unmarshal(buf.Bytes(), &rec) != nil || rec.PID <= 1 || rec.Start == "" || len(rec.Keys) == 0 || len(rec.Keys) > jumpMaxRecordKeys {
		return rec, false
	}
	return rec, true
}

type limitedReader struct {
	f *os.File
	n int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errors.New("limit")
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.f.Read(p)
	l.n -= int64(n)
	return n, err
}

// jumpSession is one verified record found during a pass.
type jumpSession struct {
	pid int
	rec jumpRecord
}

// children returns the parent->children map, scanning /proc on first use in a
// pass: a pass with no live record never scans.
func (r *jumpReaper) children() map[int][]int {
	if r.kids == nil {
		r.scans++
		r.kids = r.proc.childMap()
		if r.kids == nil {
			r.kids = map[int][]int{}
		}
	}
	return r.kids
}

// ownsJumpSession reports whether pid is an sshd connection that belongs to the
// jump user: its parent is not 1 (that is the listener, never ours to signal)
// and one of its children runs as the jump uid. Records are written by an
// unprivileged helper, so without this a forged record could name any other
// user's sshd. Pre-authentication there is no such child yet: not ours (yet).
func (r *jumpReaper) ownsJumpSession(pid int) bool {
	ppid, _, err := r.proc.stat(pid)
	if err != nil || ppid <= 1 {
		return false
	}
	for _, c := range r.children()[pid] {
		if uid, err := r.proc.uid(c); err == nil && uid == r.jumpUID {
			return true
		}
	}
	return false
}

type jumpAnswer struct {
	lines []string
	err   error
}

// lookupAll asks the API once per distinct key (not once per session) with at
// most jumpLookupWorkers requests in flight, and stops starting new ones when
// the pass budget is spent (those keys count as errors: the stale path).
func (r *jumpReaper) lookupAll(sessions []jumpSession) map[string]jumpAnswer {
	var keys []jumpRecordKey
	seen := map[string]bool{}
	for _, s := range sessions {
		for _, k := range s.rec.Keys {
			if id := k.Type + " " + k.Blob; !seen[id] {
				seen[id] = true
				keys = append(keys, k)
			}
		}
	}
	answers := make(map[string]jumpAnswer, len(keys))
	var mu sync.Mutex
	var deadline time.Time
	if r.passBudget > 0 {
		deadline = time.Now().Add(r.passBudget)
	}
	work := make(chan jumpRecordKey)
	var wg sync.WaitGroup
	for i := 0; i < min(jumpLookupWorkers, len(keys)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range work {
				var a jumpAnswer
				if !deadline.IsZero() && !time.Now().Before(deadline) {
					a.err = errors.New("pass budget exhausted")
				} else {
					a.lines, a.err = r.lookup(k.Type, k.Blob)
				}
				mu.Lock()
				answers[k.Type+" "+k.Blob] = a
				mu.Unlock()
			}
		}()
	}
	for _, k := range keys {
		work <- k
	}
	close(work)
	wg.Wait()
	return answers
}

// reapOnce checks every record once and returns the pids it signalled.
func (r *jumpReaper) reapOnce() []int {
	r.beat()
	defer r.beat()
	r.kids = nil // scanned lazily, only if a live record needs it
	ents, err := os.ReadDir(r.dir)
	if err != nil {
		return nil
	}
	var sessions []jumpSession
	var killed []int
	seen := map[int]bool{}
	for _, e := range ents {
		m := jumpRecordNameRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		path := filepath.Join(r.dir, e.Name())
		pid, _ := strconv.Atoi(m[1])
		rec, ok := readJumpRecord(path)
		_, start, serr := r.proc.stat(pid)
		if (!ok || rec.PID != pid) && serr == nil && r.proc.isSSHD(pid) && r.ownsJumpSession(pid) {
			// Unreadable/oversized/corrupt record, but the connection is a live
			// jump session: it can no longer be verified, so end it. The record
			// stays until the process is gone (SIGKILL escalation next pass).
			seen[pid] = true
			if r.terminate(pid, start) {
				killed = append(killed, pid)
			}
			continue
		}
		if !ok || rec.PID != pid || serr != nil || start != rec.Start {
			_ = os.Remove(path) // process gone, pid reused, or garbage
			continue
		}
		if !r.proc.isSSHD(pid) {
			_ = os.Remove(path)
			continue
		}
		seen[pid] = true // alive: keep its bookkeeping even if not (yet) ours
		if !r.ownsJumpSession(pid) {
			continue
		}
		if _, ok := r.lastOK[pid]; !ok {
			r.lastOK[pid] = r.now()
		}
		sessions = append(sessions, jumpSession{pid: pid, rec: rec})
	}
	answers := r.lookupAll(sessions)
	occ := &occupancy{load: r.occupied}
	for _, s := range sessions {
		if r.revoked(s.rec, answers, occ) || r.now().Sub(r.lastOK[s.pid]) > r.maxStale {
			if r.terminate(s.pid, s.rec.Start) {
				killed = append(killed, s.pid)
			}
		}
	}
	for pid := range r.lastOK {
		if !seen[pid] {
			delete(r.lastOK, pid)
			delete(r.termed, pid)
		}
	}
	return killed
}

// jumpTargets returns the permitopen targets named by authorized_keys lines.
func jumpTargets(lines []string) map[string]bool {
	out := map[string]bool{}
	for _, l := range lines {
		for _, m := range jumpTargetRe.FindAllStringSubmatch(l, -1) {
			out[m[1]] = true
		}
	}
	return out
}

// revoked reports whether any key lost authorization: the API answered 204 (no
// lines), or no longer offers a permitopen target the connection was granted
// while a running (or quarantined) VM still holds that address, so the grant
// was revoked or the IP now belongs to something the key is not entitled to.
// A target whose VM merely stopped or was deleted (no routes.json entry), and
// targets that were added, do not end the session. Errors are not revocations
// (they age lastOK instead), and neither is a routes.json that cannot be read.
func (r *jumpReaper) revoked(rec jumpRecord, answers map[string]jumpAnswer, occ *occupancy) bool {
	allOK := true
	for _, k := range rec.Keys {
		a, ok := answers[k.Type+" "+k.Blob]
		if !ok || a.err != nil {
			allOK = false
			continue
		}
		if len(a.lines) == 0 {
			return true
		}
		now := jumpTargets(a.lines)
		var removed []string
		for t := range jumpTargets(k.Lines) {
			if !now[normalizeIP(t)] && !now[t] {
				removed = append(removed, t)
			}
		}
		if len(removed) == 0 {
			continue
		}
		held, err := occ.get()
		if err != nil {
			allOK = false // cannot tell a stopped VM from a reused IP
			continue
		}
		for _, t := range removed {
			if held[normalizeIP(t)] {
				return true
			}
		}
	}
	if allOK {
		r.lastOK[rec.PID] = r.now()
	}
	return false
}

// terminate signals the connection's unprivileged children and the monitor:
// SIGTERM first, SIGKILL on the next pass if it is still there. It re-checks
// the start time right before signalling so a recycled pid is never hit.
func (r *jumpReaper) terminate(pid int, start string) bool {
	if _, now, err := r.proc.stat(pid); err != nil || now != start {
		return false
	}
	sig := syscall.SIGTERM
	if r.termed[pid] {
		sig = syscall.SIGKILL
	}
	r.termed[pid] = true
	for _, c := range r.children()[pid] {
		// The map is a few seconds old at most: a child that exited and whose
		// pid was recycled must not be signalled.
		if pp, _, err := r.proc.stat(c); err == nil && pp == pid {
			_ = r.kill(c, sig)
		}
	}
	_ = r.kill(pid, sig)
	log.Printf("jump-reaper: terminated sshd connection pid %d (%v): authorization gone, target removed or unverifiable", pid, sig)
	return true
}

func runJumpReaper(args []string) error {
	var api, dir string
	interval, maxStale := 3*time.Second, 2*time.Minute
	dir, routesPath := jumpStateDir, jumpRoutesPath
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			return fmt.Errorf("flag %s needs a value", args[i])
		}
		v := args[i+1]
		var err error
		switch args[i] {
		case "-api":
			api = v
		case "-dir":
			dir = v
		case "-routes":
			routesPath = v
		case "-interval":
			interval, err = time.ParseDuration(v)
		case "-max-stale":
			maxStale, err = time.ParseDuration(v)
		default:
			return fmt.Errorf("unknown flag %s", args[i])
		}
		if err != nil {
			return err
		}
		i++
	}
	// The heartbeat must survive a few passes, or healthy logins would be denied.
	if api == "" || interval <= 0 || maxStale <= 0 || interval > jumpHeartbeatMaxAge/3 || !jumpURLRe.MatchString(api) {
		return errors.New("usage: jump-reaper -api http://127.0.0.1:PORT/internal/ssh/authorize [-dir D] [-routes /run/boat/routes.json] [-interval 3s (max 10s)] [-max-stale 2m]")
	}
	u, err := user.Lookup(jumpUser)
	if err != nil {
		return fmt.Errorf("user %s: %w", jumpUser, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("user %s: bad uid %q", jumpUser, u.Uid)
	}
	r := newJumpReaper(dir, api, routesPath, maxStale, uid)
	for {
		r.reapOnce()
		time.Sleep(interval)
	}
}
