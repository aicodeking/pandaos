package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeProc builds a /proc tree: pid -> (ppid, start, argv0).
func fakeProc(t *testing.T, procs map[int][3]string) procfs {
	t.Helper()
	root := t.TempDir()
	for pid, v := range procs {
		d := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		// 22 fields: pid (comm) state ppid ... starttime is field 22.
		fields := make([]string, 20)
		for i := range fields {
			fields[i] = "0"
		}
		fields[0] = "S"
		fields[1] = v[0]
		fields[19] = v[1]
		stat := fmt.Sprintf("%d (sshd-session) %s", pid, strings.Join(fields, " "))
		if err := os.WriteFile(filepath.Join(d, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
		uid := "0"
		if strings.Contains(v[2], "jump@") {
			uid = strconv.Itoa(testJumpUID)
		}
		if err := os.WriteFile(filepath.Join(d, "status"), []byte("Name:\tx\nUid:\t"+uid+"\t"+uid+"\t"+uid+"\t"+uid+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "cmdline"), []byte(v[2]+"\x00jump [priv]\x00"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return procfs{root: root}
}

const testJumpUID = 995

const (
	lineA = `restrict,port-forwarding,permitopen="10.0.0.5:22" ssh-ed25519 AAAA`
	lineB = `restrict,port-forwarding,permitopen="10.0.0.6:22" ssh-ed25519 AAAA`
	// Both VMs granted: a team VM was added.
	lineAB = `restrict,port-forwarding,permitopen="10.0.0.5:22",permitopen="10.0.0.6:22" ssh-ed25519 AAAA`
)

// touchBeat stamps a fresh reaper heartbeat (what a running reaper does), so
// recordJumpSession's fail-closed liveness check passes.
func touchBeat(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, jumpHeartbeatName), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(filepath.Join(dir, jumpHeartbeatName), now, now); err != nil {
		t.Fatal(err)
	}
}

type reaperEnv struct {
	r       *jumpReaper
	answer  map[string][]string // type+" "+blob -> lines
	fail    bool
	held    map[string]bool // IPs routes.json gives to a running VM
	heldErr error
	calls   atomic.Int32
	sent    map[int][]syscall.Signal
	clock   time.Time
}

func newReaperEnv(t *testing.T, proc procfs) *reaperEnv {
	t.Helper()
	e := &reaperEnv{answer: map[string][]string{}, sent: map[int][]syscall.Signal{}, clock: time.Unix(1000, 0)}
	e.r = &jumpReaper{
		proc: proc, dir: t.TempDir(), maxStale: time.Minute, jumpUID: testJumpUID,
		now: func() time.Time { return e.clock },
		lookup: func(typ, blob string) ([]string, error) {
			e.calls.Add(1)
			if e.fail {
				return nil, fmt.Errorf("api down")
			}
			return e.answer[typ+" "+blob], nil
		},
		kill: func(pid int, sig syscall.Signal) error {
			e.sent[pid] = append(e.sent[pid], sig)
			return nil
		},
		occupied: func() (map[string]bool, error) { return e.held, e.heldErr },
		lastOK:   map[int]time.Time{}, termed: map[int]bool{},
	}
	touchBeat(t, e.r.dir)
	return e
}

// jumpProcs is a listener (50, parent 1), the monitor pid and its jump-user child (pid+1).
func jumpProcs(pid int, start string) map[int][3]string {
	return map[int][3]string{
		50:      {"1", "4444", "sshd: /usr/sbin/sshd -D [listener]"},
		pid:     {"50", start, "sshd-session: jump [priv]"},
		pid + 1: {strconv.Itoa(pid), start + "1", "sshd-session: jump@notty"},
	}
}

func TestJumpRecordAndReap(t *testing.T) {
	proc := fakeProc(t, map[int][3]string{
		50:  {"1", "4444", "sshd: /usr/sbin/sshd -D [listener]"},
		100: {"50", "5555", "sshd-session: jump [priv]"},
		101: {"100", "5556", "sshd-session: jump@notty"},
		200: {"1", "6666", "/usr/bin/other"},
	})
	e := newReaperEnv(t, proc)
	// Not an sshd parent: fail closed (error) unless explicitly allowed; nothing recorded either way.
	if err := recordJumpSession(proc, e.r.dir, 200, "ssh-ed25519", "AAAA", []string{lineA}, false); err == nil {
		t.Fatal("unidentifiable parent must deny")
	}
	if err := recordJumpSession(proc, e.r.dir, 200, "ssh-ed25519", "AAAA", []string{lineA}, true); err != nil {
		t.Fatal(err)
	}
	// No lines: nothing to track, no error.
	if err := recordJumpSession(proc, e.r.dir, 100, "ssh-ed25519", "AAAA", nil, false); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(e.r.dir); len(ents) != 1 || ents[0].Name() != jumpHeartbeatName {
		t.Fatalf("unexpected records: %v", ents)
	}
	if err := recordJumpSession(proc, e.r.dir, 100, "ssh-ed25519", "AAAA", []string{lineA}, false); err != nil {
		t.Fatal(err)
	}
	// Recording the same key again (second lookup of one auth) replaces, not appends.
	if err := recordJumpSession(proc, e.r.dir, 100, "ssh-ed25519", "AAAA", []string{lineA}, false); err != nil {
		t.Fatal(err)
	}
	rec, ok := readJumpRecord(filepath.Join(e.r.dir, "100.json"))
	if !ok || len(rec.Keys) != 1 || rec.Start != "5555" {
		t.Fatalf("record = %+v ok=%v", rec, ok)
	}
	e.answer["ssh-ed25519 AAAA"] = []string{lineA}

	if k := e.r.reapOnce(); len(k) != 0 {
		t.Fatalf("still authorized, killed %v", k)
	}
	// Target gone and another VM now holds its address (IP reuse): terminated, children first.
	e.answer["ssh-ed25519 AAAA"] = []string{lineB}
	e.held = map[string]bool{"10.0.0.5": true}
	if k := e.r.reapOnce(); !slices.Equal(k, []int{100}) {
		t.Fatalf("killed %v", k)
	}
	if !slices.Equal(e.sent[101], []syscall.Signal{syscall.SIGTERM}) || !slices.Equal(e.sent[100], []syscall.Signal{syscall.SIGTERM}) {
		t.Fatalf("signals %v", e.sent)
	}
	// Still alive next pass: escalates to SIGKILL.
	e.r.reapOnce()
	if !slices.Equal(e.sent[100], []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
		t.Fatalf("no escalation: %v", e.sent[100])
	}
	// Process gone: record removed.
	_ = os.RemoveAll(filepath.Join(proc.root, "100"))
	e.r.reapOnce()
	if _, err := os.Stat(filepath.Join(e.r.dir, "100.json")); !os.IsNotExist(err) {
		t.Fatal("stale record kept")
	}
}

func TestJumpReapRevokedEmpty(t *testing.T) {
	proc := fakeProc(t, jumpProcs(100, "5555"))
	e := newReaperEnv(t, proc)
	if err := recordJumpSession(proc, e.r.dir, 100, "ssh-ed25519", "AAAA", []string{lineA}, false); err != nil {
		t.Fatal(err)
	}
	e.answer["ssh-ed25519 AAAA"] = nil // 204: grant revoked / VM deleted / member removed
	if k := e.r.reapOnce(); !slices.Equal(k, []int{100}) {
		t.Fatalf("revoked key not terminated: %v", k)
	}
}

func TestJumpReapAPIErrorFailsClosedAfterMaxStale(t *testing.T) {
	proc := fakeProc(t, jumpProcs(100, "5555"))
	e := newReaperEnv(t, proc)
	_ = recordJumpSession(proc, e.r.dir, 100, "ssh-ed25519", "AAAA", []string{lineA}, false)
	e.fail = true
	if k := e.r.reapOnce(); len(k) != 0 {
		t.Fatalf("transient API error killed session: %v", k)
	}
	e.clock = e.clock.Add(30 * time.Second)
	if k := e.r.reapOnce(); len(k) != 0 {
		t.Fatalf("killed before max-stale: %v", k)
	}
	e.clock = e.clock.Add(31 * time.Second)
	if k := e.r.reapOnce(); !slices.Equal(k, []int{100}) {
		t.Fatalf("unverifiable session survived max-stale: %v", k)
	}
	// A successful verification resets the clock.
	proc2 := fakeProc(t, jumpProcs(101, "7"))
	e2 := newReaperEnv(t, proc2)
	_ = recordJumpSession(proc2, e2.r.dir, 101, "ssh-ed25519", "AAAA", []string{lineA}, false)
	e2.answer["ssh-ed25519 AAAA"] = []string{lineA}
	e2.clock = e2.clock.Add(50 * time.Second)
	e2.r.reapOnce()
	e2.fail = true
	e2.clock = e2.clock.Add(50 * time.Second)
	if k := e2.r.reapOnce(); len(k) != 0 {
		t.Fatalf("fresh verification ignored: %v", k)
	}
}

func TestJumpReapIgnoresReusedPidAndGarbage(t *testing.T) {
	proc := fakeProc(t, map[int][3]string{
		100: {"1", "9999", "sshd-session: jump [priv]"}, // pid reused: different start time
		300: {"1", "1", "/sbin/init"},                   // not sshd
	})
	e := newReaperEnv(t, proc)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(e.r.dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("100.json", `{"pid":100,"start":"5555","keys":[{"type":"ssh-ed25519","blob":"AAAA","lines":["x"]}]}`)
	write("300.json", `{"pid":300,"start":"1","keys":[{"type":"ssh-ed25519","blob":"AAAA","lines":["x"]}]}`)
	write("1.json", `{"pid":1,"start":"1","keys":[{"type":"ssh-ed25519","blob":"AAAA","lines":["x"]}]}`)
	write("200.json", `not json`)
	write("../x.json", `x`)
	write("notes.txt", `x`)
	if k := e.r.reapOnce(); len(k) != 0 || len(e.sent) != 0 {
		t.Fatalf("signalled %v %v", k, e.sent)
	}
	for _, n := range []string{"100.json", "300.json", "1.json", "200.json"} {
		if _, err := os.Stat(filepath.Join(e.r.dir, n)); !os.IsNotExist(err) {
			t.Errorf("%s should be cleaned", n)
		}
	}
	if _, err := os.Stat(filepath.Join(e.r.dir, "notes.txt")); err != nil {
		t.Error("unrelated file removed")
	}
}

func TestRunJumpReaperRejectsBadArgs(t *testing.T) {
	for _, a := range [][]string{nil, {"-api"}, {"-api", "http://example.com/x"}, {"-bogus", "1"}, {"-api", "http://127.0.0.1:1/internal/ssh/authorize", "-interval", "0s"},
		// The heartbeat (30s) must outlive three passes.
		{"-api", "http://127.0.0.1:1/internal/ssh/authorize", "-interval", "11s"}} {
		if err := runJumpReaper(a); err == nil {
			t.Errorf("args %v accepted", a)
		}
	}
}

// realKey returns a key type/blob accepted by runJumpKeys' validation.
func realKey(seed byte) (typ, blob string) {
	typ = "ssh-ed25519"
	return typ, makeSSHWireBlob(typ, append([]byte{0, 0, 0, 32}, bytes.Repeat([]byte{seed}, 32)...))
}

func realLine(typ, blob string, ips ...string) string {
	opts := "restrict,port-forwarding"
	for _, ip := range ips {
		opts += fmt.Sprintf(`,permitopen="%s:22"`, ip)
	}
	return opts + " " + typ + " " + blob
}

// The reaper's lookup is the REAL one against an API stand-in: 200 then 204
// kills on the very next pass; 5xx and garbage only after max-stale.
func TestJumpReapRealLookupStatusMapping(t *testing.T) {
	typ, blob := realKey('r')
	line := realLine(typ, blob, "10.200.0.2")
	var mode atomic.Value
	mode.Store("200")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch mode.Load().(string) {
		case "200":
			_, _ = w.Write([]byte(`{"authorized_keys":[` + strconv.Quote(line) + `]}`))
		case "204":
			w.WriteHeader(http.StatusNoContent)
		case "500":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "garbage":
			_, _ = w.Write([]byte(`<html>`))
		}
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	api := "http://127.0.0.1:" + port + "/internal/ssh/authorize"

	newEnv := func(t *testing.T) (*jumpReaper, *time.Time, map[int][]syscall.Signal) {
		proc := fakeProc(t, jumpProcs(100, "5555"))
		r := newJumpReaper(t.TempDir(), api, filepath.Join(t.TempDir(), "routes.json"), time.Minute, testJumpUID)
		touchBeat(t, r.dir)
		clock := time.Unix(1000, 0)
		sent := map[int][]syscall.Signal{}
		r.proc, r.now = proc, func() time.Time { return clock }
		r.kill = func(pid int, sig syscall.Signal) error { sent[pid] = append(sent[pid], sig); return nil }
		if err := recordJumpSession(proc, r.dir, 100, typ, blob, []string{line}, false); err != nil {
			t.Fatal(err)
		}
		return r, &clock, sent
	}

	t.Run("204 kills on the next pass", func(t *testing.T) {
		mode.Store("200")
		r, _, sent := newEnv(t)
		if k := r.reapOnce(); len(k) != 0 {
			t.Fatalf("authorized session killed: %v", k)
		}
		mode.Store("204")
		if k := r.reapOnce(); !slices.Equal(k, []int{100}) {
			t.Fatalf("204 did not kill on first pass: %v", k)
		}
		if !slices.Equal(sent[100], []syscall.Signal{syscall.SIGTERM}) {
			t.Fatalf("signals %v", sent)
		}
	})
	for _, m := range []string{"500", "garbage"} {
		t.Run(m+" waits for max-stale", func(t *testing.T) {
			mode.Store("200")
			r, clock, _ := newEnv(t)
			r.reapOnce()
			mode.Store(m)
			*clock = clock.Add(30 * time.Second)
			if k := r.reapOnce(); len(k) != 0 {
				t.Fatalf("%s killed before max-stale: %v", m, k)
			}
			*clock = clock.Add(31 * time.Second)
			if k := r.reapOnce(); !slices.Equal(k, []int{100}) {
				t.Fatalf("%s survived max-stale: %v", m, k)
			}
		})
	}
	t.Run("transport error waits for max-stale", func(t *testing.T) {
		mode.Store("200")
		r, clock, _ := newEnv(t)
		r.lookup = jumpAPILookup("http://127.0.0.1:1/internal/ssh/authorize", 500*time.Millisecond)
		if k := r.reapOnce(); len(k) != 0 {
			t.Fatalf("unreachable API killed immediately: %v", k)
		}
		*clock = clock.Add(2 * time.Minute)
		if k := r.reapOnce(); !slices.Equal(k, []int{100}) {
			t.Fatalf("unreachable API survived max-stale: %v", k)
		}
	})
}

func TestJumpReapOnlyRemovedTargetsKill(t *testing.T) {
	proc := fakeProc(t, jumpProcs(100, "5555"))
	e := newReaperEnv(t, proc)
	if err := recordJumpSession(proc, e.r.dir, 100, "ssh-ed25519", "AAAA", []string{lineA}, false); err != nil {
		t.Fatal(err)
	}
	// Another team VM started: target added, session survives (and keeps its grant).
	e.answer["ssh-ed25519 AAAA"] = []string{lineAB}
	if k := e.r.reapOnce(); len(k) != 0 {
		t.Fatalf("added target killed session: %v", k)
	}
	// ... and again after that VM stopped (back to the original target).
	e.answer["ssh-ed25519 AAAA"] = []string{lineA}
	if k := e.r.reapOnce(); len(k) != 0 {
		t.Fatalf("unrelated change killed session: %v", k)
	}
}

// A teammate stopping an unrelated VM removes that VM's target from the
// lines; its address is in no routes.json entry, so nothing is killed. Only
// the stopped VM's own forwards die with it.
func TestJumpReapStoppedVMDoesNotKill(t *testing.T) {
	proc := fakeProc(t, jumpProcs(100, "5555"))
	e := newReaperEnv(t, proc)
	if err := recordJumpSession(proc, e.r.dir, 100, "ssh-ed25519", "AAAA", []string{lineAB}, false); err != nil {
		t.Fatal(err)
	}
	e.answer["ssh-ed25519 AAAA"] = []string{lineA} // 10.0.0.6 stopped
	e.held = map[string]bool{"10.0.0.5": true}     // only the still-running VM is routed
	for range 3 {
		if k := e.r.reapOnce(); len(k) != 0 {
			t.Fatalf("stopped VM killed an unrelated session: %v", k)
		}
	}
	if len(e.sent) != 0 {
		t.Fatalf("signals sent: %v", e.sent)
	}
}

// Revoking one key's grant (no lines at all) kills, whatever routes.json says.
func TestJumpReapRevokedKeyKillsRegardlessOfRoutes(t *testing.T) {
	proc := fakeProc(t, jumpProcs(100, "5555"))
	e := newReaperEnv(t, proc)
	if err := recordJumpSession(proc, e.r.dir, 100, "ssh-ed25519", "AAAA", []string{lineA}, false); err != nil {
		t.Fatal(err)
	}
	e.answer["ssh-ed25519 AAAA"] = nil // 204
	e.heldErr = fmt.Errorf("routes unreadable")
	if k := e.r.reapOnce(); !slices.Equal(k, []int{100}) {
		t.Fatalf("revoked key survived: %v", k)
	}
}

// The address of a granted target now holds a VM the key is not offered (IP
// reused by another VM, or this VM's grant was revoked while the key keeps
// other grants): killed. A quarantined VM there counts too.
func TestJumpReapReusedOrRevokedTargetKills(t *testing.T) {
	for name, held := range map[string]map[string]bool{
		"another VM runs there now": {"10.0.0.5": true, "10.0.0.9": true},
		"routes lists it":           {"10.0.0.5": true},
	} {
		t.Run(name, func(t *testing.T) {
			proc := fakeProc(t, jumpProcs(100, "5555"))
			e := newReaperEnv(t, proc)
			if err := recordJumpSession(proc, e.r.dir, 100, "ssh-ed25519", "AAAA", []string{lineAB}, false); err != nil {
				t.Fatal(err)
			}
			e.answer["ssh-ed25519 AAAA"] = []string{lineB} // 10.0.0.5 no longer offered
			e.held = held
			if k := e.r.reapOnce(); !slices.Equal(k, []int{100}) {
				t.Fatalf("reused/revoked target survived: %v", k)
			}
		})
	}
}

// If routes.json cannot be read the reaper cannot tell a stopped VM from a
// reused IP: it keeps the session until max-stale, then fails closed.
func TestJumpReapUnreadableRoutesFailsClosedAfterMaxStale(t *testing.T) {
	proc := fakeProc(t, jumpProcs(100, "5555"))
	e := newReaperEnv(t, proc)
	if err := recordJumpSession(proc, e.r.dir, 100, "ssh-ed25519", "AAAA", []string{lineAB}, false); err != nil {
		t.Fatal(err)
	}
	e.answer["ssh-ed25519 AAAA"] = []string{lineA}
	e.heldErr = fmt.Errorf("no routes.json")
	if k := e.r.reapOnce(); len(k) != 0 {
		t.Fatalf("killed at once on unreadable routes: %v", k)
	}
	e.clock = e.clock.Add(2 * time.Minute)
	if k := e.r.reapOnce(); !slices.Equal(k, []int{100}) {
		t.Fatalf("unreadable routes survived max-stale: %v", k)
	}
}

func TestJumpOccupiedReadsRoutesJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "routes.json")
	if err := os.WriteFile(p, []byte(`{"vms":{"a":{"ip":"10.0.0.5"},"b":{"ip":"::ffff:10.0.0.6"}},"quarantined":["10.0.0.7"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := jumpOccupied(p)()
	if err != nil || !got["10.0.0.5"] || !got["10.0.0.6"] || !got["10.0.0.7"] || len(got) != 3 {
		t.Fatalf("occupied = %v, %v", got, err)
	}
	if _, err := jumpOccupied(p + ".missing")(); err == nil {
		t.Fatal("missing routes.json must be an error")
	}
}

// The helper refuses to authorize unless the reaper stamped its heartbeat
// recently; the explicit opt-out still works.
func TestJumpHelperDeniesWithoutReaperHeartbeat(t *testing.T) {
	proc := fakeProc(t, jumpProcs(100, "5555"))
	dir := t.TempDir()
	record := func(allow bool) error {
		return recordJumpSession(proc, dir, 100, "ssh-ed25519", "AAAA", []string{lineA}, allow)
	}
	if err := record(false); err == nil {
		t.Fatal("no heartbeat file: must deny")
	}
	if err := record(true); err != nil {
		t.Fatalf("opt-out must not need a heartbeat: %v", err)
	}
	_ = os.Remove(filepath.Join(dir, "100.json"))

	touchBeat(t, dir)
	if err := record(false); err != nil {
		t.Fatalf("fresh heartbeat denied: %v", err)
	}
	old := time.Now().Add(-jumpHeartbeatMaxAge - 5*time.Second)
	if err := os.Chtimes(filepath.Join(dir, jumpHeartbeatName), old, old); err != nil {
		t.Fatal(err)
	}
	if err := record(false); err == nil {
		t.Fatal("stale heartbeat (dead reaper): must deny")
	}
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(dir, jumpHeartbeatName), future, future)
	if err := record(false); err == nil {
		t.Fatal("heartbeat from the future: must deny")
	}
	// Not a regular file (symlink): refused.
	_ = os.Remove(filepath.Join(dir, jumpHeartbeatName))
	touchBeat(t, t.TempDir())
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, jumpHeartbeatName)); err != nil {
		t.Fatal(err)
	}
	if err := record(false); err == nil {
		t.Fatal("symlinked heartbeat: must deny")
	}
	// No lines: nothing to track, never denied for a stale heartbeat.
	if err := recordJumpSession(proc, dir, 100, "ssh-ed25519", "AAAA", nil, false); err != nil {
		t.Fatalf("no lines: %v", err)
	}
}

// Each pass stamps the heartbeat, which makes the helper allow again.
func TestJumpReaperPassStampsHeartbeat(t *testing.T) {
	proc := fakeProc(t, jumpProcs(100, "5555"))
	e := newReaperEnv(t, proc)
	hb := filepath.Join(e.r.dir, jumpHeartbeatName)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(hb, old, old); err != nil {
		t.Fatal(err)
	}
	if err := checkJumpHeartbeat(e.r.dir, time.Now()); err == nil {
		t.Fatal("stale heartbeat accepted")
	}
	e.r.reapOnce()
	if err := checkJumpHeartbeat(e.r.dir, time.Now()); err != nil {
		t.Fatalf("pass did not refresh the heartbeat: %v", err)
	}
	if fi, err := os.Stat(hb); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("heartbeat mode %v %v", fi, err)
	}
	if ents, _ := os.ReadDir(e.r.dir); len(ents) != 1 {
		t.Fatalf("temp files left behind: %v", ents)
	}
	// The record reader ignores it, and it is never mistaken for a session.
	if _, ok := readJumpRecord(hb); ok {
		t.Fatal("heartbeat parsed as a record")
	}
}

// One /proc scan builds the whole parent->children map, and terminate never
// signals a child whose parent changed since the scan (recycled pid).
func TestJumpChildMapAndTerminateSkipsReparentedChildren(t *testing.T) {
	procs := jumpProcs(100, "5555")
	procs[102] = [3]string{"100", "5557", "sshd-session: jump@pts/0"}
	proc := fakeProc(t, procs)
	m := proc.childMap()
	slices.Sort(m[100])
	if !slices.Equal(m[100], []int{101, 102}) || !slices.Equal(m[50], []int{100}) {
		t.Fatalf("childMap = %v", m)
	}
	e := newReaperEnv(t, proc)
	e.r.kids = m
	// 102 exits and its pid is reused by an unrelated process under pid 1.
	stat := filepath.Join(proc.root, "102", "stat")
	b, _ := os.ReadFile(stat)
	_ = os.WriteFile(stat, []byte(strings.Replace(string(b), " 100 ", " 1 ", 1)), 0o644)
	if !e.r.terminate(100, "5555") {
		t.Fatal("terminate refused")
	}
	if _, hit := e.sent[102]; hit {
		t.Fatalf("recycled child signalled: %v", e.sent)
	}
	if !slices.Equal(e.sent[101], []syscall.Signal{syscall.SIGTERM}) || !slices.Equal(e.sent[100], []syscall.Signal{syscall.SIGTERM}) {
		t.Fatalf("signals %v", e.sent)
	}
}

func TestJumpReapRefusesForeignOrListenerPids(t *testing.T) {
	procs := jumpProcs(100, "5555")
	procs[300] = [3]string{"50", "7777", "sshd-session: admin [priv]"} // someone else's connection
	procs[301] = [3]string{"300", "7778", "sshd-session: admin@pts/0"}
	procs[50] = [3]string{"1", "4444", "sshd: /usr/sbin/sshd -D [listener]"}
	procs[60] = [3]string{"1", "8888", "sshd-session: jump [priv]"} // parent 1, even with a jump child
	procs[61] = [3]string{"60", "8889", "sshd-session: jump@notty"}
	procs[70] = [3]string{"50", "9999", "sshd-session: jump [priv]"} // no jump child yet (pre-auth)
	proc := fakeProc(t, procs)
	e := newReaperEnv(t, proc)
	for _, pid := range []int{100, 300, 50, 60, 70} {
		path := filepath.Join(e.r.dir, strconv.Itoa(pid)+".json")
		start := map[int]string{100: "5555", 300: "7777", 50: "4444", 60: "8888", 70: "9999"}[pid]
		body := fmt.Sprintf(`{"pid":%d,"start":%q,"keys":[{"type":"ssh-ed25519","blob":"AAAA","lines":[%q]}]}`, pid, start, lineA)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Every key is unauthorized (204): only the genuine jump connection may die.
	if k := e.r.reapOnce(); !slices.Equal(k, []int{100}) {
		t.Fatalf("killed %v, want only 100", k)
	}
	for _, pid := range []int{300, 301, 50, 60, 61, 70} {
		if len(e.sent[pid]) != 0 {
			t.Errorf("signalled foreign/listener pid %d: %v", pid, e.sent[pid])
		}
	}
	// Records of live non-owned processes are kept (pre-auth may become ours).
	if _, err := os.Stat(filepath.Join(e.r.dir, "70.json")); err != nil {
		t.Error("pre-auth record dropped")
	}
	// Pre-auth process gets its jump child: now it is killable.
	child := filepath.Join(proc.root, "71")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	// reuse fakeProc layout for the new child
	src := fakeProc(t, map[int][3]string{71: {"70", "10000", "sshd-session: jump@notty"}})
	for _, f := range []string{"stat", "cmdline", "status"} {
		b, _ := os.ReadFile(filepath.Join(src.root, "71", f))
		_ = os.WriteFile(filepath.Join(child, f), b, 0o644)
	}
	if k := e.r.reapOnce(); !slices.Contains(k, 70) {
		t.Fatalf("session did not become killable: %v", k)
	}
}

func TestJumpReapDedupsKeyLookupsAndBoundsParallelism(t *testing.T) {
	procs := map[int][3]string{50: {"1", "4444", "sshd: listener"}}
	for i := 0; i < 6; i++ {
		pid := 100 + 10*i
		procs[pid] = [3]string{"50", fmt.Sprint(5000 + i), "sshd-session: jump [priv]"}
		procs[pid+1] = [3]string{strconv.Itoa(pid), fmt.Sprint(6000 + i), "sshd-session: jump@notty"}
	}
	proc := fakeProc(t, procs)
	e := newReaperEnv(t, proc)
	var mu sync.Mutex
	inflight, peak := 0, 0
	e.r.lookup = func(typ, blob string) ([]string, error) {
		e.calls.Add(1)
		mu.Lock()
		inflight++
		peak = max(peak, inflight)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inflight--
		mu.Unlock()
		return []string{lineA}, nil
	}
	for i := 0; i < 6; i++ {
		// Sessions 0-2 share key AAAA, 3-4 share BBBB, 5 uses CCCC (+ AAAA again).
		blob := []string{"AAAA", "AAAA", "AAAA", "BBBB", "BBBB", "CCCC"}[i]
		if err := recordJumpSession(proc, e.r.dir, 100+10*i, "ssh-ed25519", blob, []string{lineA}, false); err != nil {
			t.Fatal(err)
		}
	}
	if k := e.r.reapOnce(); len(k) != 0 {
		t.Fatalf("killed %v", k)
	}
	if got := e.calls.Load(); got != 3 {
		t.Fatalf("lookups = %d, want 3 (one per distinct key)", got)
	}
	if peak > jumpLookupWorkers {
		t.Fatalf("parallelism %d exceeds %d", peak, jumpLookupWorkers)
	}
}

func TestJumpReapPassBudgetCountsUnstartedLookupsAsErrors(t *testing.T) {
	procs := map[int][3]string{50: {"1", "4444", "sshd: listener"}}
	for i := 0; i < 8; i++ {
		pid := 100 + 10*i
		procs[pid] = [3]string{"50", fmt.Sprint(5000 + i), "sshd-session: jump [priv]"}
		procs[pid+1] = [3]string{strconv.Itoa(pid), fmt.Sprint(6000 + i), "sshd-session: jump@notty"}
	}
	proc := fakeProc(t, procs)
	e := newReaperEnv(t, proc)
	e.r.passBudget = 30 * time.Millisecond
	var started atomic.Int32
	e.r.lookup = func(typ, blob string) ([]string, error) {
		started.Add(1)
		time.Sleep(60 * time.Millisecond)
		return nil, nil // would be "revoked" if it were asked
	}
	for i := 0; i < 8; i++ {
		if err := recordJumpSession(proc, e.r.dir, 100+10*i, "ssh-ed25519", fmt.Sprintf("K%d", i), []string{lineA}, false); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	k := e.r.reapOnce()
	if started.Load() >= 8 {
		t.Fatalf("budget did not stop new lookups (%d started)", started.Load())
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("pass not bounded")
	}
	// Only the keys actually answered (204) are killed; skipped ones wait for max-stale.
	if len(k) != int(started.Load()) {
		t.Fatalf("killed %d, started %d", len(k), started.Load())
	}
}

// A pass with no session records must not scan /proc; one with a live record
// scans exactly once.
func TestJumpReapScansProcLazily(t *testing.T) {
	proc := fakeProc(t, jumpProcs(100, "5555"))
	e := newReaperEnv(t, proc)
	e.r.reapOnce()
	if e.r.scans != 0 {
		t.Fatalf("empty pass scanned /proc %d times", e.r.scans)
	}
	if e.r.kids != nil {
		t.Fatal("kids populated without a record")
	}
	// Garbage records are removed without needing ownership.
	if err := os.WriteFile(filepath.Join(e.r.dir, "999.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.r.reapOnce()
	if e.r.scans != 0 {
		t.Fatalf("pass with only a stale record scanned /proc %d times", e.r.scans)
	}
	if !e.r.ownsJumpSession(100) || !e.r.ownsJumpSession(100) || e.r.scans != 1 {
		t.Fatalf("ownership checks should share one scan, got %d", e.r.scans)
	}
}

func TestJumpRecordRefusesOversizedRecord(t *testing.T) {
	proc := fakeProc(t, jumpProcs(100, "5555"))
	e := newReaperEnv(t, proc)
	big := lineA + strings.Repeat("x", jumpMaxRecordBytes)
	if err := recordJumpSession(proc, e.r.dir, 100, "ssh-ed25519", "AAAA", []string{big}, false); err == nil {
		t.Fatal("oversized record must refuse authorization")
	}
	if _, err := os.Stat(filepath.Join(e.r.dir, "100.json")); err == nil {
		t.Fatal("no record should be written")
	}
}

func TestJumpReapTerminatesLiveSessionWithUnreadableRecord(t *testing.T) {
	procs := jumpProcs(100, "5555")
	procs[200] = [3]string{"1", "6666", "/usr/bin/other"}
	procs[300] = [3]string{"50", "7777", "sshd-session: jump [priv]"}
	proc := fakeProc(t, procs)
	e := newReaperEnv(t, proc)
	write := func(name string, b []byte) string {
		p := filepath.Join(e.r.dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	live := write("100.json", append([]byte(`{"pid":100,"start":"5555","keys":[{"type":"t","blob":"`), make([]byte, jumpMaxRecordBytes+10)...))
	notJump := write("200.json", []byte("garbage"))
	noJumpChild := write("300.json", []byte("garbage"))
	dead := write("400.json", []byte("garbage"))
	k := e.r.reapOnce()
	if len(k) != 1 || k[0] != 100 {
		t.Fatalf("killed %v, want [100]", k)
	}
	if got := e.sent[100]; len(got) != 1 || got[0] != syscall.SIGTERM {
		t.Fatalf("sent %v", e.sent[100])
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal("live record must stay for SIGKILL escalation")
	}
	for _, p := range []string{notJump, noJumpChild, dead} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s should be deleted", p)
		}
	}
	e.r.reapOnce()
	if got := e.sent[100]; len(got) < 2 || got[len(got)-1] != syscall.SIGKILL {
		t.Fatalf("no escalation: %v", got)
	}
}
