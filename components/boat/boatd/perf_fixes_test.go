package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Item 1: Stop must not block on synchronous Firecracker sync_snapshot_files or
// synchronous fsyncFile(memFile); durability is finalized asynchronously by syncSnapshot.
func TestStopSkipsSyncFsyncAndSyncsInBackground(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Name: "stop-async", Team: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	fc := h.fcOf(t, m, v.ID)

	// Hold bootIO active across Stop so background syncSnapshot pauses in waitBootQuiet
	// until we release it: proves Stop does not wait for syncSnapshot or fsyncFile(memFile).
	bootDone := m.bootIO()
	stopped, err := m.Stop(ctx, v.ID, "api")
	if err != nil {
		bootDone()
		t.Fatalf("stop: %v", err)
	}
	if stopped.State != Stopped || !stopped.HasSnapshot {
		bootDone()
		t.Fatalf("unexpected stopped state: %+v", stopped)
	}
	if stopped.SnapSynced {
		bootDone()
		t.Fatalf("Stop returned SnapSynced=true while background syncSnapshot was paused")
	}

	fc.mu.Lock()
	syncs := append([]bool(nil), fc.snapSyncs...)
	fc.mu.Unlock()
	if len(syncs) != 1 || syncs[0] {
		t.Fatalf("Stop passed sync_snapshot_files=%v, want [false]", syncs)
	}

	// Releasing bootIO lets syncSnapshot finish and mark SnapSynced=true.
	bootDone()
	eventually(t, "background syncSnapshot", func() bool {
		cur, err := m.get(v.ID)
		return err == nil && cur.SnapSynced
	})
}

// Item 2: Fast exponential backoff polling in adoptPoll, killWait, waitPidFile,
// waitSock, waitAgent avoids 20-100ms fixed polling latency.
type slowDoneProcs struct {
	mu    sync.Mutex
	alive map[int]bool
	dones map[int]chan struct{}
}

func (p *slowDoneProcs) Start(VM, string, int, int) (int, <-chan struct{}, error) {
	return 0, nil, errors.New("unused")
}
func (p *slowDoneProcs) Adopt(pid int, id string) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dones[pid]
}
func (p *slowDoneProcs) Alive(pid int, id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.alive[pid]
}
func (p *slowDoneProcs) Kill(pid int) {
	// Process exits 2ms after SIGKILL, while done channel (simulating fallback
	// Adopt at max backoff) would not close for 200ms.
	time.AfterFunc(2*time.Millisecond, func() {
		p.mu.Lock()
		p.alive[pid] = false
		p.mu.Unlock()
	})
	time.AfterFunc(200*time.Millisecond, func() {
		p.mu.Lock()
		ch := p.dones[pid]
		p.mu.Unlock()
		if ch != nil {
			close(ch)
		}
	})
}

func TestFastBackoffPollingAndKillWait(t *testing.T) {
	// Verify pollBackoff doubles and caps cleanly.
	bo := newPollBackoff(time.Millisecond, 10*time.Millisecond)
	want := []time.Duration{
		1 * time.Millisecond,
		2 * time.Millisecond,
		4 * time.Millisecond,
		8 * time.Millisecond,
		10 * time.Millisecond,
		10 * time.Millisecond,
	}
	for i, w := range want {
		if got := bo.next(); got != w {
			t.Fatalf("step %d: got %v, want %v", i, got, w)
		}
	}

	// adoptPoll detects quick exit well below the old 100ms sleep.
	var running atomic.Bool
	running.Store(true)
	time.AfterFunc(3*time.Millisecond, func() { running.Store(false) })
	t0 := time.Now()
	<-adoptPoll(running.Load)
	if elapsed := time.Since(t0); elapsed > 60*time.Millisecond {
		t.Fatalf("adoptPoll took %v, expected < 60ms", elapsed)
	}

	// killWait detects exit in ~2-5ms even when done lags by 200ms.
	sp := &slowDoneProcs{
		alive: map[int]bool{42: true},
		dones: map[int]chan struct{}{42: make(chan struct{})},
	}
	m := &Manager{
		cfg:   Config{StateDir: t.TempDir()},
		procs: sp,
		dones: map[string]<-chan struct{}{"vm-1": sp.dones[42]},
	}
	t0 = time.Now()
	m.killWait("vm-1", 42)
	if elapsed := time.Since(t0); elapsed > 80*time.Millisecond {
		t.Fatalf("killWait took %v (waited for slow done instead of fast Alive backoff)", elapsed)
	}

	// waitPidFile detects a pid file written after 2ms promptly.
	pidPath := filepath.Join(t.TempDir(), "fc.pid")
	time.AfterFunc(2*time.Millisecond, func() {
		_ = os.WriteFile(pidPath, []byte("12345\n"), 0o600)
	})
	t0 = time.Now()
	pid, err := waitPidFile(pidPath, make(chan error))
	if err != nil || pid != 12345 {
		t.Fatalf("waitPidFile: pid=%d err=%v", pid, err)
	}
	if elapsed := time.Since(t0); elapsed > 50*time.Millisecond {
		t.Fatalf("waitPidFile took %v, expected fast backoff", elapsed)
	}

	// waitSock detects a socket appearing after 2ms promptly.
	sockDir, err := os.MkdirTemp("/tmp", "boatsock")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "fc.sock")
	var lnMu sync.Mutex
	var ln net.Listener
	time.AfterFunc(2*time.Millisecond, func() {
		l, lerr := net.Listen("unix", sockPath)
		if lerr == nil {
			lnMu.Lock()
			ln = l
			lnMu.Unlock()
		}
	})
	defer func() {
		lnMu.Lock()
		if ln != nil {
			ln.Close()
		}
		lnMu.Unlock()
	}()
	t0 = time.Now()
	if err := waitSock(sockPath, make(chan struct{}), 2*time.Second); err != nil {
		t.Fatalf("waitSock: %v", err)
	}
	if elapsed := time.Since(t0); elapsed > 80*time.Millisecond {
		t.Fatalf("waitSock took %v, expected fast backoff", elapsed)
	}
}

// Item 3a: SSH policy exec/verify reuses the pooled vsock connection instead of
// dialing a fresh unpooled connection on every call.
func TestSSHPolicyUsesPooledVsock(t *testing.T) {
	m, v, r := sshPolicyFixture(t)
	if err := os.MkdirAll(m.root(v.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(m.root(v.ID), vsockUDS)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var accepts atomic.Int32
	var execs atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				if _, err := br.ReadString('\n'); err != nil {
					return
				}
				fmt.Fprint(c, "OK 1024\n")
				dec := json.NewDecoder(br)
				for {
					var q map[string]any
					if dec.Decode(&q) != nil {
						return
					}
					switch q["op"] {
					case "health":
						json.NewEncoder(c).Encode(map[string]any{"ok": true, "trusted_root_exec": true})
					case "put_file":
						json.NewEncoder(c).Encode(map[string]any{"ok": true})
					case "exec":
						execs.Add(1)
						for _, rep := range sshAttest(r) {
							json.NewEncoder(c).Encode(rep)
						}
					default:
						json.NewEncoder(c).Encode(map[string]any{"ok": true})
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		closeAgentConns(sock)
		ln.Close()
		<-done
	})

	// Install runs health + put_file + install + verify: all 4 ops must ride 1 pooled conn.
	if _, err := m.installSSHPolicy(context.Background(), v.ID, r); err != nil {
		t.Fatalf("installSSHPolicy: %v", err)
	}
	if got := accepts.Load(); got != 1 {
		t.Fatalf("installSSHPolicy accepted %d vsock conns, want 1 (pooled)", got)
	}
	if got := execs.Load(); got != 2 {
		t.Fatalf("installSSHPolicy ran %d execs, want 2 (install+verify)", got)
	}

	// Follow-up verify call (as in restoreSSHAfterPause) reuses the exact same pooled conn.
	_, digest, _ := canonicalSSHConfig(r.Config)
	cfgBytes, _, _ := canonicalSSHConfig(r.Config)
	if err := runSSHPolicy(context.Background(), sock, "verify", cfgBytes, digest, r.BootEpoch); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got := accepts.Load(); got != 1 {
		t.Fatalf("verify after install opened new conn (accepts=%d, want 1)", got)
	}
}

// Item 3b: quarantineSSH / restoreSSHAfterPause use incremental `nft add/delete element`
// on dedicated @quarantine_{v4,v6,tap} sets when tables are loaded, and cleanly fall back
// to full `nft -f -` if incremental updates fail or tables were reloaded mid-pause.
func TestIncrementalQuarantineAndFallback(t *testing.T) {
	t.Run("ruleset_has_quarantine_sets_before_established_and_teams", func(t *testing.T) {
		c := defaultConfig()
		c.IPv6Prefix = "2001:db8:1::/64"
		c.Teams = map[string]TeamPolicy{"blue": {AllowIntraTeam: true}}
		vms := []VM{
			{State: Running, SSHBootEpoch: "ok", SSHPolicyReady: true, Index: 2, Team: "blue", Tap: "bt2", IP: c.ipFor(2), MAC: macFor(2), IPv6: c.ipv6For(2)},
		}
		rs := ruleset(c, vms)
		for _, want := range []string{
			"set quarantine_v4 { type ipv4_addr; }",
			"set quarantine_v6 { type ipv6_addr; }",
			"set quarantine_tap { type ifname; }",
		} {
			if !strings.Contains(rs, want) {
				t.Fatalf("ruleset missing %q:\n%s", want, rs)
			}
		}
		fwd := inetChain(t, rs, "forward")
		if qIdx, estIdx := strings.Index(fwd, "ip daddr @quarantine_v4 drop"), strings.Index(fwd, "ct state established,related accept"); qIdx < 0 || qIdx > estIdx {
			t.Fatalf("@quarantine_v4 not before established:\n%s", fwd)
		}
		_, bridge, _ := strings.Cut(rs, "table bridge boat {")
		if qIdx, teamIdx := strings.Index(bridge, "oifname @quarantine_tap drop"), strings.Index(bridge, "iifname @team0 oifname @team0 accept"); qIdx < 0 || qIdx > teamIdx {
			t.Fatalf("@quarantine_tap not before @team0 accept:\n%s", bridge)
		}
	})

	t.Run("pause_resume_uses_incremental_set_updates_without_full_reload", func(t *testing.T) {
		m, _ := newTestManager(t, func(c *Config) { c.IPv6Prefix = "2001:db8:1::/64" })
		v, err := m.Create(context.Background(), CreateReq{Team: "blue"})
		if err != nil {
			t.Fatal(err)
		}
		sock := filepath.Join(m.root(v.ID), vsockUDS)
		if err := os.Rename(sock, sock+".old"); err != nil {
			t.Fatal(err)
		}
		cfg := sshPolicyConfig{
			Version: 1, URL: "https://api.example/v1/vms/" + v.ID + "/ssh/authorize",
			Token: "tok", VMID: v.ID, TeamID: v.Team, Generation: 0, BootEpoch: v.SSHBootEpoch, Users: []string{"dev"},
		}
		req := sshPolicyRequest{Version: 1, BootEpoch: v.SSHBootEpoch, Config: cfg, HelperB64: b64("helper")}
		sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
			if q["op"] != "exec" {
				return []map[string]any{{"ok": true}}
			}
			return sshAttest(req)
		})
		if _, err := m.installSSHPolicy(context.Background(), v.ID, req); err != nil {
			t.Fatal(err)
		}

		var mu sync.Mutex
		var fullReloads int
		var addCmds, delCmds []string
		origRun := m.run
		m.run = func(stdin, name string, args ...string) error {
			if name == "nft" {
				cmd := strings.Join(args, " ")
				mu.Lock()
				if len(args) >= 2 && args[0] == "-f" && args[1] == "-" {
					fullReloads++
				} else if len(args) >= 2 && args[0] == "add" && args[1] == "element" {
					addCmds = append(addCmds, cmd)
				} else if len(args) >= 2 && args[0] == "delete" && args[1] == "element" {
					delCmds = append(delCmds, cmd)
				}
				mu.Unlock()
			}
			return origRun(stdin, name, args...)
		}

		if _, err := m.CreateSnapshot(context.Background(), v.ID, "snap-inc"); err != nil {
			t.Fatalf("CreateSnapshot: %v", err)
		}

		mu.Lock()
		gotFull, gotAdds, gotDels := fullReloads, append([]string(nil), addCmds...), append([]string(nil), delCmds...)
		mu.Unlock()
		if gotFull != 0 {
			t.Fatalf("snapshot pause/resume triggered %d full nft -f - reloads, want 0", gotFull)
		}
		if len(gotAdds) != 1 || !strings.Contains(gotAdds[0], "quarantine_v4 { "+v.IP+" }") ||
			!strings.Contains(gotAdds[0], "quarantine_v6 { "+v.IPv6+" }") ||
			!strings.Contains(gotAdds[0], fmt.Sprintf(`quarantine_tap { %q }`, v.Tap)) {
			t.Fatalf("unexpected incremental add commands: %v", gotAdds)
		}
		if len(gotDels) != 1 || !strings.Contains(gotDels[0], "quarantine_v4 { "+v.IP+" }") ||
			!strings.Contains(gotDels[0], "quarantine_v6 { "+v.IPv6+" }") ||
			!strings.Contains(gotDels[0], fmt.Sprintf(`quarantine_tap { %q }`, v.Tap)) {
			t.Fatalf("unexpected incremental delete commands: %v", gotDels)
		}
		now, _ := m.get(v.ID)
		if !now.SSHPolicyReady {
			t.Fatalf("VM not restored to SSHPolicyReady: %+v", now)
		}
		var rf routeFile
		if err := readJSON(filepath.Join(m.cfg.RunDir, "routes.json"), &rf); err != nil {
			t.Fatal(err)
		}
		if _, ok := rf.VMs[v.ID]; !ok || len(rf.Quarantined) != 0 {
			t.Fatalf("routes.json not restored after incremental unquarantine: %+v", rf)
		}
	})

	t.Run("incremental_error_falls_back_to_full_syncNet", func(t *testing.T) {
		m, _ := newTestManager(t, nil)
		v, err := m.Create(context.Background(), CreateReq{Team: "blue"})
		if err != nil {
			t.Fatal(err)
		}
		sock := filepath.Join(m.root(v.ID), vsockUDS)
		if err := os.Rename(sock, sock+".old"); err != nil {
			t.Fatal(err)
		}
		cfg := sshPolicyConfig{
			Version: 1, URL: "https://api.example/v1/vms/" + v.ID + "/ssh/authorize",
			Token: "tok", VMID: v.ID, TeamID: v.Team, Generation: 0, BootEpoch: v.SSHBootEpoch, Users: []string{"dev"},
		}
		req := sshPolicyRequest{Version: 1, BootEpoch: v.SSHBootEpoch, Config: cfg, HelperB64: b64("helper")}
		sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
			if q["op"] != "exec" {
				return []map[string]any{{"ok": true}}
			}
			return sshAttest(req)
		})
		if _, err := m.installSSHPolicy(context.Background(), v.ID, req); err != nil {
			t.Fatal(err)
		}

		var fullReloads atomic.Int32
		origRun := m.run
		m.run = func(stdin, name string, args ...string) error {
			if name == "nft" {
				if len(args) >= 2 && (args[0] == "add" || args[0] == "delete") && args[1] == "element" {
					return errors.New("nft: No such file or directory")
				}
				if len(args) >= 2 && args[0] == "-f" && args[1] == "-" {
					fullReloads.Add(1)
				}
			}
			return origRun(stdin, name, args...)
		}

		// Quarantine fails incremental add -> falls back to full syncNet (1);
		// Restore sees VM not in dynQuarantined -> full syncNet (2).
		if _, err := m.CreateSnapshot(context.Background(), v.ID, "snap-fb"); err != nil {
			t.Fatalf("CreateSnapshot with incremental fallback: %v", err)
		}
		if got := fullReloads.Load(); got != 2 {
			t.Fatalf("expected 2 full syncNet fallback reloads, got %d", got)
		}
		now, _ := m.get(v.ID)
		if !now.SSHPolicyReady {
			t.Fatalf("VM not ready after fallback restore: %+v", now)
		}
	})

	t.Run("mid_pause_full_syncNet_clears_dynQuarantined_and_restore_falls_back", func(t *testing.T) {
		m, _ := newTestManager(t, nil)
		v, err := m.Create(context.Background(), CreateReq{Team: "blue"})
		if err != nil {
			t.Fatal(err)
		}
		sock := filepath.Join(m.root(v.ID), vsockUDS)
		if err := os.Rename(sock, sock+".old"); err != nil {
			t.Fatal(err)
		}
		cfg := sshPolicyConfig{
			Version: 1, URL: "https://api.example/v1/vms/" + v.ID + "/ssh/authorize",
			Token: "tok", VMID: v.ID, TeamID: v.Team, Generation: 0, BootEpoch: v.SSHBootEpoch, Users: []string{"dev"},
		}
		req := sshPolicyRequest{Version: 1, BootEpoch: v.SSHBootEpoch, Config: cfg, HelperB64: b64("helper")}
		sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
			if q["op"] != "exec" {
				return []map[string]any{{"ok": true}}
			}
			return sshAttest(req)
		})
		if _, err := m.installSSHPolicy(context.Background(), v.ID, req); err != nil {
			t.Fatal(err)
		}
		readyVM, _ := m.get(v.ID)
		if err := m.quarantineSSH(readyVM); err != nil {
			t.Fatalf("quarantineSSH: %v", err)
		}
		// Full syncNet while dynamically quarantined must reload tables (migrating v
		// to static quarantine rules) and clear dynQuarantined.
		var fullReloads atomic.Int32
		origRun := m.run
		m.run = func(stdin, name string, args ...string) error {
			if name == "nft" && len(args) >= 2 && args[0] == "-f" && args[1] == "-" {
				fullReloads.Add(1)
			}
			return origRun(stdin, name, args...)
		}
		if err := m.syncNet(); err != nil {
			t.Fatalf("mid-pause syncNet: %v", err)
		}
		if got := fullReloads.Load(); got != 1 {
			t.Fatalf("mid-pause syncNet reloads=%d, want 1", got)
		}
		// Restore must fall back to full syncNet because v is now statically quarantined.
		if err := m.restoreSSHAfterPause(context.Background(), readyVM); err != nil {
			t.Fatalf("restoreSSHAfterPause: %v", err)
		}
		if got := fullReloads.Load(); got != 2 {
			t.Fatalf("restore after mid-pause syncNet reloads=%d, want 2", got)
		}
	})
}

func TestWriteTombstoneSkipsRedundantStateDirSync(t *testing.T) {
	m, _ := newTestManager(t, nil)
	var stateDirSyncs atomic.Int32
	m.syncDirFn = func(p string) error {
		if p == m.cfg.StateDir {
			stateDirSyncs.Add(1)
		}
		return syncDir(p)
	}

	// newManager already pre-created and synced tombstones/ at startup:
	// deleting multiple VMs must NOT call syncDir(StateDir) again.
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("vm-del-%d", i)
		if err := m.writeTombstone(id, VM{ID: id}); err != nil {
			t.Fatalf("writeTombstone(%s): %v", id, err)
		}
	}
	if got := stateDirSyncs.Load(); got != 0 {
		t.Fatalf("writeTombstone synced StateDir %d times after startup, want 0", got)
	}

	// If tombstones/ is removed externally, writeTombstone recreates & syncs StateDir once, then skips again.
	if err := os.RemoveAll(filepath.Join(m.cfg.StateDir, tombstoneSub)); err != nil {
		t.Fatal(err)
	}
	if err := m.writeTombstone("vm-recreate-1", VM{ID: "vm-recreate-1"}); err != nil {
		t.Fatalf("writeTombstone after dir removal: %v", err)
	}
	if err := m.writeTombstone("vm-recreate-2", VM{ID: "vm-recreate-2"}); err != nil {
		t.Fatalf("writeTombstone after recreate: %v", err)
	}
	if got := stateDirSyncs.Load(); got != 1 {
		t.Fatalf("expected exactly 1 StateDir sync after tombstones/ recreation, got %d", got)
	}
}

func TestAddVMNetBatchingAndCleanup(t *testing.T) {
	type callRec struct {
		stdin string
		cmd   string
	}
	var calls []callRec
	run := func(stdin, name string, args ...string) error {
		calls = append(calls, callRec{stdin: stdin, cmd: name + " " + strings.Join(args, " ")})
		return nil
	}
	c := defaultConfig()
	c.IPv6Prefix = "2001:db8:1::/64"
	v := VM{Index: 5, Tap: "bt5", IP: c.ipFor(5), MAC: macFor(5), IPv6: c.ipv6For(5)}
	if err := addVMNet(c, run, v, 900005, 899999); err != nil {
		t.Fatalf("addVMNet: %v", err)
	}
	// Expect 5 total subprocesses: delVMNet batch, host ip batch, sysctl, netns ip batch, bridge batch.
	if len(calls) != 5 {
		t.Fatalf("addVMNet spawned %d subprocesses, want 5: %+v", len(calls), calls)
	}
	if calls[0].cmd != "ip -force -batch -" || !strings.Contains(calls[0].stdin, "netns del boat5") {
		t.Fatalf("call 0 = %+v", calls[0])
	}
	if calls[1].cmd != "ip -force -batch -" ||
		!strings.Contains(calls[1].stdin, "netns add boat5") ||
		!strings.Contains(calls[1].stdin, "link add bt5 type veth peer name veth0 netns boat5") ||
		!strings.Contains(calls[1].stdin, "link set bt5 master boatbr0 up") ||
		!strings.Contains(calls[1].stdin, "neigh replace "+v.IP+" lladdr "+v.MAC+" dev boatbr0 nud permanent") ||
		!strings.Contains(calls[1].stdin, "neigh replace "+v.IPv6+" lladdr "+v.MAC+" dev boatbr0 nud permanent") {
		t.Fatalf("call 1 = %+v", calls[1])
	}
	if calls[2].cmd != "ip netns exec boat5 sysctl -qw net.ipv6.conf.all.disable_ipv6=1" {
		t.Fatalf("call 2 = %+v", calls[2])
	}
	if calls[3].cmd != "ip -n boat5 -force -batch -" ||
		!strings.Contains(calls[3].stdin, "tuntap add dev tap0 mode tap user 900005 group 899999") ||
		!strings.Contains(calls[3].stdin, "link set br0 up") {
		t.Fatalf("call 3 = %+v", calls[3])
	}
	if calls[4].cmd != "bridge -force -batch -" ||
		!strings.Contains(calls[4].stdin, "fdb replace "+v.MAC+" dev bt5 master static") ||
		!strings.Contains(calls[4].stdin, "link set dev bt5 learning off flood off") {
		t.Fatalf("call 4 = %+v", calls[4])
	}

	// Failure mid-setup cleans up via delVMNet batch and surfaces the error.
	var cleanupSeen bool
	failRun := func(stdin, name string, args ...string) error {
		cmd := name + " " + strings.Join(args, " ")
		if cmd == "ip -n boat5 -force -batch -" {
			return errors.New("Command failed -:2")
		}
		if cmd == "ip -force -batch -" && strings.Contains(stdin, "netns del boat5") {
			cleanupSeen = true
		}
		return nil
	}
	if err := addVMNet(c, failRun, v, 900005, 899999); err == nil || !strings.Contains(err.Error(), "Command failed -:2") {
		t.Fatalf("expected netns batch error, got %v", err)
	}
	if !cleanupSeen {
		t.Fatal("failed addVMNet did not invoke delVMNet cleanup")
	}

	// delNeighbours batches IPv4 + IPv6 into one `ip -force -batch -` call.
	calls = nil
	delNeighbours(c, run, v)
	if len(calls) != 1 || calls[0].cmd != "ip -force -batch -" ||
		!strings.Contains(calls[0].stdin, "neigh del "+v.IP+" dev boatbr0") ||
		!strings.Contains(calls[0].stdin, "neigh del "+v.IPv6+" dev boatbr0") {
		t.Fatalf("delNeighbours calls = %+v", calls)
	}
}

func TestSyncNetGroupCommitCoalescing(t *testing.T) {
	m, _ := newTestManager(t, nil)
	inFirst := make(chan struct{})
	releaseFirst := make(chan struct{})
	var nftCalls atomic.Int32
	origRun := m.run
	m.run = func(stdin, name string, args ...string) error {
		if name == "nft" && len(args) >= 2 && args[0] == "-f" && args[1] == "-" {
			if nftCalls.Add(1) == 1 {
				close(inFirst)
				<-releaseFirst
			}
		}
		return origRun(stdin, name, args...)
	}

	// Force a real ruleset change for pass 1.
	m.netMu.Lock()
	m.lastNftRuleset = ""
	m.netMu.Unlock()

	err1 := make(chan error, 1)
	go func() { err1 <- m.syncNet() }()
	<-inFirst

	// While pass 1 is inside nft -f -, mutate state (force a different ruleset) and queue 8 concurrent callers.
	m.cfg.CtPerVM = 123
	const waiters = 8
	errs := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		go func() { errs <- m.syncNet() }()
	}
	// Wait until all 8 waiters are parked on syncNetWait.
	eventually(t, "syncNetWait populated", func() bool {
		m.syncNetMu.Lock()
		defer m.syncNetMu.Unlock()
		return m.syncNetWait != nil
	})
	time.Sleep(25 * time.Millisecond)

	// Release pass 1: all 8 concurrent waiters must coalesce into exactly 1 follow-up nft reload (total 2).
	close(releaseFirst)
	if err := <-err1; err != nil {
		t.Fatalf("pass 1 syncNet: %v", err)
	}
	for i := 0; i < waiters; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("waiter %d syncNet: %v", i, err)
		}
	}
	if got := nftCalls.Load(); got != 2 {
		t.Fatalf("8 concurrent syncNet callers triggered %d nft -f - reloads, want 2 (1 active + 1 coalesced)", got)
	}
}

func TestParentCgroupMemCeilingAndDumpBounds(t *testing.T) {
	t.Run("parent_cgroup_memory_ceiling_applied_at_setup", func(t *testing.T) {
		origRoot := cgroupRoot
		origStateDisk := stateDiskFn
		origHostMem := hostMemFn
		defer func() {
			cgroupRoot = origRoot
			stateDiskFn = origStateDisk
			hostMemFn = origHostMem
		}()
		cg := t.TempDir()
		cgroupRoot = cg
		stateDiskFn = func(string) string { return "259:0" }
		hostMemFn = func() int { return 16384 }

		c := defaultConfig()
		c.Jailer = "/bin/jailer"
		c.CgroupParent = "boat-vms"
		c.HostReserveMiB = 2048
		c.UntrustedTenants = true
		c.IOCost = false
		if err := setupCgroups(&c); err != nil {
			t.Fatalf("setupCgroups: %v", err)
		}
		wantHard := int64(16384-2048) << 20
		wantHigh := wantHard - (512 << 20)
		gotMax, err := readInt(filepath.Join(cg, "boat-vms", "memory.max"))
		if err != nil || gotMax != wantHard {
			t.Fatalf("parent memory.max=%d err=%v, want %d", gotMax, err, wantHard)
		}
		gotHigh, err := readInt(filepath.Join(cg, "boat-vms", "memory.high"))
		if err != nil || gotHigh != wantHigh {
			t.Fatalf("parent memory.high=%d err=%v, want %d", gotHigh, err, wantHigh)
		}
	})

	t.Run("concurrent_dumps_bounded_by_semaphore", func(t *testing.T) {
		m, _ := newTestManager(t, nil)
		if cap(m.dumpSlots) != maxConcurrentDumps {
			t.Fatalf("dumpSlots cap=%d, want %d", cap(m.dumpSlots), maxConcurrentDumps)
		}
		var active, maxActive atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rel, err := m.acquireDumpSlot(context.Background())
				if err != nil {
					t.Errorf("acquireDumpSlot: %v", err)
					return
				}
				cur := active.Add(1)
				for {
					prev := maxActive.Load()
					if cur <= prev || maxActive.CompareAndSwap(prev, cur) {
						break
					}
				}
				time.Sleep(25 * time.Millisecond)
				active.Add(-1)
				rel()
			}()
		}
		wg.Wait()
		if got := maxActive.Load(); got > maxConcurrentDumps {
			t.Fatalf("max concurrent dumps=%d exceeded cap %d", got, maxConcurrentDumps)
		}
		// Cancelled context must fail promptly while slots are full.
		r1, _ := m.acquireDumpSlot(context.Background())
		r2, _ := m.acquireDumpSlot(context.Background())
		defer r1()
		defer r2()
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		if _, err := m.acquireDumpSlot(cctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected DeadlineExceeded when dumpSlots full, got %v", err)
		}
	})

	t.Run("background_retry_restores_memory_max_after_page_cache_drains", func(t *testing.T) {
		origRoot := cgroupRoot
		origWait := memMaxWait
		origInt := memMaxRetryInterval
		origAtt := memMaxRetryAttempts
		defer func() {
			cgroupRoot = origRoot
			memMaxWait = origWait
			memMaxRetryInterval = origInt
			memMaxRetryAttempts = origAtt
		}()
		cgroupRoot = t.TempDir()
		memMaxWait = 40 * time.Millisecond
		memMaxRetryInterval = 20 * time.Millisecond
		memMaxRetryAttempts = 20

		m, _ := newTestManager(t, func(c *Config) { c.CgroupParent = "boat-vms" })
		m.cfg.cgroupLimits = true
		v := VM{ID: "vm-bg-max", MemMiB: 1024}
		dir := filepath.Join(cgroupRoot, "boat-vms", v.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		_, hard := m.cfg.memLimits(v)
		for _, f := range []string{"memory.max", "memory.high", "memory.reclaim"} {
			os.WriteFile(filepath.Join(dir, f), nil, 0o644)
		}
		os.WriteFile(filepath.Join(dir, "memory.current"), []byte(strconv.FormatInt(hard+(2<<20), 10)+"\n"), 0o644)

		m.setMemLimits(v)
		if b, _ := os.ReadFile(filepath.Join(dir, "memory.max")); string(b) != "" {
			t.Fatalf("memory.max lowered prematurely while cur > hard: %q", b)
		}

		// Simulate page cache draining below hard: background retry must restore memory.max automatically.
		os.WriteFile(filepath.Join(dir, "memory.current"), []byte(strconv.FormatInt(hard, 10)+"\n"), 0o644)
		want := strconv.FormatInt(hard, 10)
		eventually(t, "background memory.max restore", func() bool {
			b, _ := os.ReadFile(filepath.Join(dir, "memory.max"))
			return string(b) == want
		})
	})

	t.Run("background_retry_is_bounded_and_aborts_on_new_dump", func(t *testing.T) {
		origRoot := cgroupRoot
		origWait := memMaxWait
		origInt := memMaxRetryInterval
		origAtt := memMaxRetryAttempts
		defer func() {
			cgroupRoot = origRoot
			memMaxWait = origWait
			memMaxRetryInterval = origInt
			memMaxRetryAttempts = origAtt
		}()
		cgroupRoot = t.TempDir()
		memMaxWait = 20 * time.Millisecond
		memMaxRetryInterval = 15 * time.Millisecond
		memMaxRetryAttempts = 3

		m, _ := newTestManager(t, func(c *Config) { c.CgroupParent = "boat-vms" })
		m.cfg.cgroupLimits = true
		v := VM{ID: "vm-bg-bound", MemMiB: 512}
		dir := filepath.Join(cgroupRoot, "boat-vms", v.ID)
		os.MkdirAll(dir, 0o755)
		_, hard := m.cfg.memLimits(v)
		for _, f := range []string{"memory.max", "memory.high", "memory.reclaim"} {
			os.WriteFile(filepath.Join(dir, f), nil, 0o644)
		}
		os.WriteFile(filepath.Join(dir, "memory.current"), []byte(strconv.FormatInt(hard+(4<<20), 10)+"\n"), 0o644)

		m.setMemLimits(v)
		// Retry loop must terminate after 3 bounded attempts and clear m.memMaxRetries[v.ID].
		eventually(t, "bounded retry exit", func() bool {
			m.mu.Lock()
			defer m.mu.Unlock()
			return !m.memMaxRetries[v.ID]
		})
		if b, _ := os.ReadFile(filepath.Join(dir, "memory.max")); string(b) != "" {
			t.Fatalf("memory.max changed despite cur > hard: %q", b)
		}
	})
}

func TestDeleteVMSkipsRedundantVMJSONFsyncAndRecordsBilling(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()

	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	m.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}

	v, err := m.Create(ctx, CreateReq{Name: "del-no-fsync", Team: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	if v.State != Running || v.StartedAt == nil {
		t.Fatalf("unexpected created state: %+v", v)
	}

	clockMu.Lock()
	now = now.Add(45 * time.Second)
	clockMu.Unlock()

	_, evCh, unsub := m.ev.subscribe(0)
	defer unsub()

	var saveCalls atomic.Int32
	m.mu.Lock()
	realSave := m.saveState
	m.saveState = func(dir string, vm VM) error {
		if vm.ID == v.ID {
			saveCalls.Add(1)
		}
		return realSave(dir, vm)
	}
	m.mu.Unlock()

	deleted, err := m.Delete(ctx, v.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if deleted.State != "deleted" {
		t.Fatalf("deleted.State = %q, want deleted", deleted.State)
	}
	if got := saveCalls.Load(); got != 0 {
		t.Fatalf("deleteVM called saveState (vm.json write+fsync) %d times, want 0", got)
	}
	if exists(m.vmDir(v.ID)) || m.tombstoned(v.ID) {
		t.Fatalf("vmDir or tombstone left behind after Delete")
	}

	// Billing interval must be recorded in usage.jsonl.
	if err := m.usage.flush(); err != nil {
		t.Fatalf("usage flush: %v", err)
	}
	recs, err := m.usage.read(time.Time{})
	if err != nil {
		t.Fatalf("usage read: %v", err)
	}
	var foundBilling bool
	for _, r := range recs {
		if r.Kind == "vm" && r.ID == v.ID && r.Team == "blue" {
			foundBilling = true
			if !r.Start.Equal(*v.StartedAt) || !r.End.Equal(now) {
				t.Fatalf("unexpected billing interval: start=%v end=%v want [%v, %v]", r.Start, r.End, *v.StartedAt, now)
			}
		}
	}
	if !foundBilling {
		t.Fatalf("billing record for deleted VM %s missing from usage ledger (%+v)", v.ID, recs)
	}

	// Both vm.state (reason=deleted) and vm.deleted events must be published.
	var sawStateDeleted, sawVMDeleted bool
	deadline := time.After(2 * time.Second)
	for !sawStateDeleted || !sawVMDeleted {
		select {
		case ev := <-evCh:
			if ev.VMID != v.ID {
				continue
			}
			if ev.Type == "vm.state" && ev.Prev == Running && ev.State == Stopped && ev.Reason == "deleted" {
				sawStateDeleted = true
			}
			if ev.Type == "vm.deleted" {
				sawVMDeleted = true
			}
		case <-deadline:
			t.Fatalf("missing events: sawStateDeleted=%v sawVMDeleted=%v", sawStateDeleted, sawVMDeleted)
		}
	}
}

func TestStopAllBoundedConcurrencyAndNoDuplicateSyncSnapshot(t *testing.T) {
	t.Run("bounded_concurrency_and_single_fsync_per_vm", func(t *testing.T) {
		m, _ := newTestManager(t, func(c *Config) {
			c.Teams = map[string]TeamPolicy{"t1": {MaxVMs: 16, MaxTotalVMs: 16, MaxVCPU: 64, MaxMemMiB: 32768}}
			c.HostMemMiB = 65536
			c.HostCPUs = 64
		})
		ctx := context.Background()
		const nVMs = 6
		ids := make([]string, 0, nVMs)
		for i := 0; i < nVMs; i++ {
			v, err := m.Create(ctx, CreateReq{Name: fmt.Sprintf("sa-%d", i), Team: "t1"})
			if err != nil {
				t.Fatalf("Create %d: %v", i, err)
			}
			ids = append(ids, v.ID)
		}

		var (
			mu            sync.Mutex
			activeVMs     = map[string]bool{}
			maxConcurrent int
			fsyncCounts   = map[string]int{} // "<vmID>/<file>" -> count
			stoppedWrites = map[string]int{} // vmID -> Stopped saveState writes
		)
		m.mu.Lock()
		realSave := m.saveState
		m.saveState = func(dir string, vm VM) error {
			mu.Lock()
			if vm.State == Stopping {
				activeVMs[vm.ID] = true
				if len(activeVMs) > maxConcurrent {
					maxConcurrent = len(activeVMs)
				}
			}
			if vm.State == Stopped {
				stoppedWrites[vm.ID]++
			}
			if vm.SnapSynced {
				delete(activeVMs, vm.ID)
			}
			mu.Unlock()
			return realSave(dir, vm)
		}
		m.fsyncSnapFile = func(path string) (os.FileInfo, error) {
			vmID := filepath.Base(filepath.Dir(filepath.Dir(path)))
			file := filepath.Base(path)
			key := vmID + "/" + file
			mu.Lock()
			fsyncCounts[key]++
			mu.Unlock()
			time.Sleep(25 * time.Millisecond)
			return fsyncFileInfo(path)
		}
		m.mu.Unlock()

		m.stopAll(ctx, shutdownReason)

		limit := cap(m.dumpSlots)
		mu.Lock()
		gotMax := maxConcurrent
		countsCopy := make(map[string]int, len(fsyncCounts))
		for k, v := range fsyncCounts {
			countsCopy[k] = v
		}
		writesCopy := make(map[string]int, len(stoppedWrites))
		for k, v := range stoppedWrites {
			writesCopy[k] = v
		}
		mu.Unlock()

		if gotMax > limit {
			t.Fatalf("stopAll max concurrent stops/syncs = %d, exceeded dumpSlots cap %d", gotMax, limit)
		}
		if gotMax < limit {
			t.Fatalf("stopAll max concurrent stops/syncs = %d, expected bounded parallelism up to %d", gotMax, limit)
		}
		for _, id := range ids {
			v, err := m.get(id)
			if err != nil {
				t.Fatalf("get(%s): %v", id, err)
			}
			if v.State != Stopped || !v.HasSnapshot || !v.SnapSynced || !v.ResumeOnBoot {
				t.Fatalf("vm %s unexpected post-stopAll state: %+v", id, v)
			}
			for _, f := range []string{memFile, vmstate} {
				key := id + "/" + f
				if got := countsCopy[key]; got != 1 {
					t.Fatalf("syncSnapshot fsyncs for %s = %d, want 1", key, got)
				}
			}
			// 1 Stopped write (with ResumeOnBoot=true) + 1 SnapSynced=true write (no separate ResumeOnBoot write).
			if got := writesCopy[id]; got != 2 {
				t.Fatalf("vm %s Stopped vm.json writes = %d, want 2", id, got)
			}
		}
		recs, err := m.usage.read(time.Time{})
		if err != nil {
			t.Fatalf("usage read: %v", err)
		}
		if len(recs) != nVMs {
			t.Fatalf("usage ledger records after stopAll = %d, want %d", len(recs), nVMs)
		}
	})

	t.Run("joins_inflight_and_already_synced_without_duplicate_fsync", func(t *testing.T) {
		m, _ := newTestManager(t, func(c *Config) {
			c.Teams = map[string]TeamPolicy{"t1": {MaxVMs: 8, MaxTotalVMs: 8, MaxVCPU: 32, MaxMemMiB: 16384}}
			c.HostMemMiB = 32768
			c.HostCPUs = 32
		})
		ctx := context.Background()
		alreadySynced, err := m.Create(ctx, CreateReq{Name: "already", Team: "t1"})
		if err != nil {
			t.Fatal(err)
		}
		inFlight, err := m.Create(ctx, CreateReq{Name: "inflight", Team: "t1"})
		if err != nil {
			t.Fatal(err)
		}
		live, err := m.Create(ctx, CreateReq{Name: "live", Team: "t1"})
		if err != nil {
			t.Fatal(err)
		}

		var (
			mu          sync.Mutex
			fsyncCounts = map[string]int{}
		)
		inFlightEntered := make(chan struct{})
		releaseInFlight := make(chan struct{})
		var enterOnce sync.Once

		m.mu.Lock()
		m.fsyncSnapFile = func(path string) (os.FileInfo, error) {
			vmID := filepath.Base(filepath.Dir(filepath.Dir(path)))
			file := filepath.Base(path)
			key := vmID + "/" + file
			mu.Lock()
			fsyncCounts[key]++
			mu.Unlock()
			if vmID == inFlight.ID && file == memFile {
				enterOnce.Do(func() { close(inFlightEntered) })
				<-releaseInFlight
			}
			return fsyncFileInfo(path)
		}
		m.mu.Unlock()

		// 1) alreadySynced finishes its background syncSnapshot before stopAll.
		if _, err := m.Stop(ctx, alreadySynced.ID, "api"); err != nil {
			t.Fatalf("Stop alreadySynced: %v", err)
		}
		eventually(t, "alreadySynced SnapSynced", func() bool {
			v, err := m.get(alreadySynced.ID)
			return err == nil && v.SnapSynced
		})
		// Explicit re-sync of an already-synced VM must be a no-op (0 extra fsyncs).
		m.syncSnapshot(alreadySynced.ID)

		// 2) inFlight is mid-syncSnapshot when stopAll starts.
		if _, err := m.Stop(ctx, inFlight.ID, "api"); err != nil {
			t.Fatalf("Stop inFlight: %v", err)
		}
		select {
		case <-inFlightEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("inFlight background syncSnapshot did not start")
		}

		// Concurrent direct syncSnapshot callers must join the in-flight sync too.
		joinDone := make(chan struct{})
		go func() {
			defer close(joinDone)
			m.syncSnapshot(inFlight.ID)
		}()

		stopAllDone := make(chan struct{})
		go func() {
			defer close(stopAllDone)
			m.stopAll(ctx, shutdownReason)
		}()

		// stopAll must wait for inFlight's background syncSnapshot to finish before returning.
		select {
		case <-stopAllDone:
			t.Fatal("stopAll returned while inFlight syncSnapshot was still blocked")
		case <-time.After(60 * time.Millisecond):
		}

		close(releaseInFlight)
		select {
		case <-stopAllDone:
		case <-time.After(10 * time.Second):
			t.Fatal("stopAll did not finish after releasing inFlight syncSnapshot")
		}
		select {
		case <-joinDone:
		case <-time.After(5 * time.Second):
			t.Fatal("joining syncSnapshot did not return")
		}

		for _, tc := range []struct {
			id           string
			resumeOnBoot bool
		}{
			{alreadySynced.ID, false},
			{inFlight.ID, false},
			{live.ID, true},
		} {
			v, err := m.get(tc.id)
			if err != nil {
				t.Fatalf("get(%s): %v", tc.id, err)
			}
			if v.State != Stopped || !v.HasSnapshot || !v.SnapSynced || v.ResumeOnBoot != tc.resumeOnBoot {
				t.Fatalf("vm %s state=%+v want Stopped+HasSnapshot+SnapSynced ResumeOnBoot=%v", tc.id, v, tc.resumeOnBoot)
			}
			for _, f := range []string{memFile, vmstate} {
				key := tc.id + "/" + f
				mu.Lock()
				got := fsyncCounts[key]
				mu.Unlock()
				if got != 1 {
					t.Fatalf("syncSnapshot fsyncs for %s = %d, want 1", key, got)
				}
			}
		}
	})
}
