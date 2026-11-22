package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- A. nftables ruleset excludes Stopped/Error and Stop runs a single full syncNet ---

func TestR4RulesetExcludesStoppedAndErrorAndStopSingleSyncNet(t *testing.T) {
	t.Run("ruleset_omits_stopped_and_error_keeps_active", func(t *testing.T) {
		c := defaultConfig()
		c.IPv6Prefix = "2001:db8:1::/64"
		c.Teams = map[string]TeamPolicy{"blue": {AllowIntraTeam: true}}
		running := make([]VM, 0, 10)
		for i := 0; i < 10; i++ {
			idx := 2 + i
			running = append(running, VM{
				ID: fmt.Sprintf("vm-run-%d", i), State: Running, SSHBootEpoch: "ep", SSHPolicyReady: true,
				Index: idx, Team: "blue", Tap: tapFor(idx), IP: c.ipFor(idx), MAC: macFor(idx), IPv6: c.ipv6For(idx),
				Ports: []Port{{GuestPort: 8080, HostPort: 20000 + i}},
			})
		}
		mixed := append([]VM(nil), running...)
		for i := 0; i < 50; i++ {
			idx := 100 + i
			st := Stopped
			if i%2 == 1 {
				st = Error
			}
			mixed = append(mixed, VM{
				ID: fmt.Sprintf("vm-dead-%d", i), State: st, SSHBootEpoch: "ep",
				Index: idx, Team: "blue", Tap: tapFor(idx), IP: c.ipFor(idx), MAC: macFor(idx), IPv6: c.ipv6For(idx),
				Ports: []Port{{GuestPort: 8080, HostPort: 21000 + i}},
			})
		}
		rsRun := ruleset(c, running)
		rsMixed := ruleset(c, mixed)
		if rsRun != rsMixed {
			t.Fatalf("ruleset with Stopped/Error VMs differs from Running-only ruleset:\nlen(run)=%d len(mixed)=%d", len(rsRun), len(rsMixed))
		}
		if strings.Contains(rsMixed, "bt100") || strings.Contains(rsMixed, c.ipFor(100)) {
			t.Fatalf("stopped/error VM leaked into ruleset")
		}
		// Active transitional states (Creating, Starting, Stopping, Forking) MUST remain quarantined in ruleset.
		for i, st := range []State{Creating, Starting, Stopping, Forking} {
			idx := 300 + i
			v := VM{
				ID: fmt.Sprintf("vm-tr-%s", st), State: st, SSHBootEpoch: "ep",
				Index: idx, Team: "blue", Tap: tapFor(idx), IP: c.ipFor(idx), MAC: macFor(idx), IPv6: c.ipv6For(idx),
			}
			rs := ruleset(c, []VM{v})
			if !strings.Contains(rs, "ip saddr "+v.IP+" drop") || !strings.Contains(rs, fmt.Sprintf(`oifname %q drop`, v.Tap)) {
				t.Fatalf("active transitional state %s missing quarantine rules:\n%s", st, rs)
			}
		}
	})

	t.Run("stop_uses_incremental_quarantine_before_delVMNet_and_single_full_syncNet", func(t *testing.T) {
		m, _ := newTestManager(t, func(c *Config) { c.IPv6Prefix = "2001:db8:1::/64" })
		ctx := context.Background()
		v, err := m.Create(ctx, CreateReq{Team: "blue"})
		if err != nil {
			t.Fatal(err)
		}
		// Promote v to SSHPolicyReady so Stop must quarantine it before teardown.
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
		if _, err := m.installSSHPolicy(ctx, v.ID, req); err != nil {
			t.Fatal(err)
		}

		var mu sync.Mutex
		var events []string
		var fullReloads int
		var lastFull string
		origRun := m.run
		m.run = func(stdin, name string, args ...string) error {
			mu.Lock()
			if name == "nft" {
				if len(args) >= 2 && args[0] == "-f" && args[1] == "-" {
					fullReloads++
					lastFull = stdin
					events = append(events, "nft-full")
				} else if len(args) >= 2 && args[0] == "add" && args[1] == "element" {
					events = append(events, "nft-add:"+strings.Join(args, " "))
				}
			} else if name == "ip" && strings.Contains(stdin, "netns del "+netnsFor(v.Index)) {
				events = append(events, "delVMNet")
			}
			mu.Unlock()
			return origRun(stdin, name, args...)
		}

		stopped, err := m.Stop(ctx, v.ID, "api")
		if err != nil || stopped.State != Stopped {
			t.Fatalf("Stop: %+v %v", stopped, err)
		}

		mu.Lock()
		gotFull := fullReloads
		gotEvents := append([]string(nil), events...)
		gotLastFull := lastFull
		mu.Unlock()

		if gotFull != 1 {
			t.Fatalf("Stop triggered %d full nft -f - reloads, want 1 (events=%v)", gotFull, gotEvents)
		}
		addIdx, delIdx, fullIdx := -1, -1, -1
		for i, ev := range gotEvents {
			switch {
			case strings.HasPrefix(ev, "nft-add:") && addIdx < 0:
				addIdx = i
				if !strings.Contains(ev, "quarantine_v4 { "+v.IP+" }") ||
					!strings.Contains(ev, fmt.Sprintf(`quarantine_tap { %q }`, v.Tap)) {
					t.Fatalf("incremental quarantine add missing VM addr/tap: %s", ev)
				}
			case ev == "delVMNet" && delIdx < 0:
				delIdx = i
			case ev == "nft-full" && fullIdx < 0:
				fullIdx = i
			}
		}
		if addIdx < 0 || delIdx < 0 || fullIdx < 0 || !(addIdx < delIdx && delIdx < fullIdx) {
			t.Fatalf("wrong Stop fence/teardown order (want nft-add < delVMNet < nft-full): %v", gotEvents)
		}
		if strings.Contains(gotLastFull, v.IP) || strings.Contains(gotLastFull, fmt.Sprintf("%q", v.Tap)) {
			t.Fatalf("stopped VM rules still present after Stop:\n%s", gotLastFull)
		}
	})
}

// --- B. Idle reaper skips health within idleLimit + bounded reap & memoryPressure ---

func TestR4ReapSkipsHealthWithinIdleLimitAndBoundsConcurrency(t *testing.T) {
	t.Run("skips_vsock_health_within_host_idle_limit", func(t *testing.T) {
		m, h := newTestManager(t, func(c *Config) { c.IdleStopS = 60 })
		ctx := context.Background()
		var skew atomic.Int64
		m.now = func() time.Time { return time.Now().UTC().Add(time.Duration(skew.Load())) }

		v, err := m.Create(ctx, CreateReq{Name: "idle-skip", Team: "t1"})
		if err != nil {
			t.Fatal(err)
		}
		fc := h.fcOf(t, m, v.ID)
		countHealth := func() int {
			fc.mu.Lock()
			defer fc.mu.Unlock()
			n := 0
			for _, c := range fc.calls {
				if c == "agent health" {
					n++
				}
			}
			return n
		}
		baseHealth := countHealth()

		// Within idleLimit (30s <= 60s): reap MUST NOT contact guest agent.
		skew.Store(int64(30 * time.Second))
		m.reap(ctx)
		m.reapWG.Wait()
		if got := countHealth() - baseHealth; got != 0 {
			t.Fatalf("reap called guest health %d times within idleLimit, want 0", got)
		}

		// Host activity at t=50s resets the host timer: at t=90s (40s since touch <= 60s), still 0 health calls.
		skew.Store(int64(50 * time.Second))
		m.touch(v.ID)
		skew.Store(int64(90 * time.Second))
		m.reap(ctx)
		m.reapWG.Wait()
		if got := countHealth() - baseHealth; got != 0 {
			t.Fatalf("reap called guest health %d times after host touch, want 0", got)
		}

		// Past idleLimit (t=120s, 70s > 60s): now reap MUST query guest health once.
		// Guest reports idle_s=0 (active SSH/desktop), so VM stays Running.
		h.mu.Lock()
		h.idleS = 0
		h.mu.Unlock()
		skew.Store(int64(120 * time.Second))
		m.reap(ctx)
		m.reapWG.Wait()
		if got := countHealth() - baseHealth; got != 1 {
			t.Fatalf("reap called guest health %d times past host idleLimit, want 1", got)
		}
		if cur, _ := m.get(v.ID); cur.State != Running {
			t.Fatalf("guest active (idle_s=0) but VM stopped: %s", cur.State)
		}

		// Guest now reports idle_s=120 (> 60s): reap queries health and stops VM.
		h.mu.Lock()
		h.idleS = 120
		h.mu.Unlock()
		m.reap(ctx)
		m.reapWG.Wait()
		if got := countHealth() - baseHealth; got != 2 {
			t.Fatalf("reap health calls = %d, want 2", got)
		}
		if cur, _ := m.get(v.ID); cur.State != Stopped {
			t.Fatalf("idle VM did not stop: %s", cur.State)
		}
	})

	t.Run("reap_bounds_concurrent_goroutines_to_reapSlots", func(t *testing.T) {
		m, _ := newTestManager(t, nil)
		m.reapSlots = make(chan struct{}, 2)
		ctx := context.Background()
		var skew atomic.Int64
		m.now = func() time.Time { return time.Now().UTC().Add(time.Duration(skew.Load())) }

		ids := make([]string, 0, 6)
		for i := 0; i < 6; i++ {
			v, err := m.Create(ctx, CreateReq{Name: fmt.Sprintf("rb-%d", i), Team: "t1", TTLS: 10})
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, v.ID)
		}

		var cur, maxSeen atomic.Int32
		m.mu.Lock()
		origSave := m.saveState
		m.saveState = func(dir string, v VM) error {
			if v.State == Stopping {
				n := cur.Add(1)
				for {
					prev := maxSeen.Load()
					if n <= prev || maxSeen.CompareAndSwap(prev, n) {
						break
					}
				}
				time.Sleep(25 * time.Millisecond)
				cur.Add(-1)
			}
			return origSave(dir, v)
		}
		m.mu.Unlock()

		skew.Store(int64(2 * time.Minute))
		m.reap(ctx)
		m.reapWG.Wait()

		if got := maxSeen.Load(); got > 2 {
			t.Fatalf("reap concurrency = %d, exceeded reapSlots cap 2", got)
		}
		for _, id := range ids {
			if v, _ := m.get(id); v.State != Stopped {
				t.Fatalf("vm %s state = %s, want stopped", id, v.State)
			}
		}
	})
}

func TestR4MemoryPressureBoundedAndReusesTransport(t *testing.T) {
	m, h := newTestManager(t, func(c *Config) {
		c.HostReserveMiB = 1024
		c.HostMemMiB = 65536
		c.Teams = map[string]TeamPolicy{"t1": {MaxVMs: 20, MaxTotalVMs: 20, MaxVCPU: 64, MaxMemMiB: 32768}}
	})
	ctx := context.Background()
	const nVMs = 10
	ids := make([]string, 0, nVMs)
	for i := 0; i < nVMs; i++ {
		v, err := m.Create(ctx, CreateReq{Name: fmt.Sprintf("bp-%d", i), Team: "t1"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, v.ID)
	}

	c1 := m.balloonFC()
	tr1, ok := c1.Transport.(*http.Transport)
	if !ok || !tr1.DisableKeepAlives {
		t.Fatalf("balloon transport must be *http.Transport with DisableKeepAlives=true")
	}

	m.memAvail = func() int { return 512 }
	m.memoryPressure(ctx)
	m.memoryPressure(ctx)

	if c2 := m.balloonFC(); c2 != c1 || c2.Transport != tr1 {
		t.Fatalf("memoryPressure did not reuse a single shared http.Transport")
	}
	for _, id := range ids {
		if !h.fcOf(t, m, id).called("PATCH /balloon") {
			t.Fatalf("vm %s did not receive PATCH /balloon", id)
		}
	}
}

// --- C. conntrack dirty tracking + bounded concurrent batch flushes ---

func TestR4ConntrackDirtySkipAndBatchParallel(t *testing.T) {
	t.Run("skip_flush_for_never_run_and_already_flushed_slot", func(t *testing.T) {
		m, h := newTestManager(t, func(c *Config) { c.IndexQuarantineS = 0 })
		ctx := context.Background()

		ctCalls := func() int {
			h.mu.Lock()
			defer h.mu.Unlock()
			n := 0
			for _, c := range h.cmds {
				if strings.HasPrefix(c, "conntrack -D") {
					n++
				}
			}
			return n
		}

		// 1. Allocate a VM that NEVER boots, then delete it:
		//    alloc flushes unknown slot once (2 calls: -s, -d), deleteVM skips (0 calls).
		neverRun, err := m.alloc(VM{Name: "never", Team: "t1", Size: "default", VCPU: 2, MemMiB: 512, DiskGiB: 1}, Creating)
		if err != nil {
			t.Fatal(err)
		}
		afterAlloc := ctCalls()
		if afterAlloc == 0 {
			t.Fatal("initial alloc of unknown slot should flush conntrack")
		}
		if _, err := m.Delete(ctx, neverRun.ID); err != nil {
			t.Fatal(err)
		}
		if got := ctCalls() - afterAlloc; got != 0 {
			t.Fatalf("deleteVM of never-run VM ran %d conntrack calls, want 0", got)
		}

		// 2. Next Create reuses slot 2 (already ctClean from step 1): alloc skips flush,
		//    boot marks slot dirty, Delete flushes once and marks slot clean again.
		beforeCreate := ctCalls()
		v, err := m.Create(ctx, CreateReq{Name: "ran", Team: "t1"})
		if err != nil {
			t.Fatal(err)
		}
		if v.IP != neverRun.IP {
			t.Fatalf("expected slot reuse of %s, got %s", neverRun.IP, v.IP)
		}
		if got := ctCalls() - beforeCreate; got != 0 {
			t.Fatalf("alloc reusing already-clean slot ran %d conntrack calls, want 0", got)
		}
		beforeDel := ctCalls()
		if _, err := m.Delete(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
		if got := ctCalls() - beforeDel; got != 2 {
			t.Fatalf("deleteVM of booted VM ran %d conntrack calls, want 2 (-s and -d)", got)
		}
		// Calling flushConntrack again on the already-flushed IP is a 0-call no-op.
		beforeRepeat := ctCalls()
		m.flushConntrack(v.IP)
		if got := ctCalls() - beforeRepeat; got != 0 {
			t.Fatalf("repeat flushConntrack on clean IP ran %d calls, want 0", got)
		}

		// 3. A failed conntrack flush leaves the IP dirty so the next flush retries.
		m.markConntrackDirty(v.IP)
		h.mu.Lock()
		h.ctErr = errors.New("conntrack: failed")
		h.mu.Unlock()
		beforeFail := ctCalls()
		m.flushConntrack(v.IP)
		if got := ctCalls() - beforeFail; got != 2 {
			t.Fatalf("dirty IP flush ran %d calls, want 2", got)
		}
		h.mu.Lock()
		h.ctErr = nil
		h.mu.Unlock()
		beforeRetry := ctCalls()
		m.flushConntrack(v.IP)
		if got := ctCalls() - beforeRetry; got != 2 {
			t.Fatalf("retry after failed flush ran %d calls, want 2", got)
		}
	})

	t.Run("allocBatch_flushes_conntrack_concurrently_with_bound", func(t *testing.T) {
		m, _ := newTestManager(t, nil)
		origRun := m.run
		var active, maxActive, total atomic.Int32
		m.run = func(stdin, name string, args ...string) error {
			if name == "conntrack" {
				total.Add(1)
				n := active.Add(1)
				for {
					prev := maxActive.Load()
					if n <= prev || maxActive.CompareAndSwap(prev, n) {
						break
					}
				}
				time.Sleep(25 * time.Millisecond)
				active.Add(-1)
			}
			return origRun(stdin, name, args...)
		}

		bases := make([]VM, 12)
		for i := range bases {
			bases[i] = VM{Name: fmt.Sprintf("b-%d", i), Team: "t1", Size: "default", VCPU: 2, MemMiB: 512, DiskGiB: 1}
		}
		vms, err := m.allocBatch(bases)
		if err != nil || len(vms) != len(bases) {
			t.Fatalf("allocBatch: len=%d err=%v", len(vms), err)
		}
		if got := maxActive.Load(); got < 2 || got > conntrackParallel {
			t.Fatalf("allocBatch max concurrent conntrack calls = %d, want in [2, %d]", got, conntrackParallel)
		}
		if got := total.Load(); got != int32(len(bases)*2) {
			t.Fatalf("total conntrack calls = %d, want %d", got, len(bases)*2)
		}
	})
}

// --- D. Reserved host ports, aiProxyPort validation, TCP bind probe, loopback output_nat exclusion ---

func TestR4ReservedPortsAndLoopbackExclusion(t *testing.T) {
	t.Run("config_validates_aiProxyPort_and_reservedPorts", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "boatd.json")
		valid := `{"portMin":20000,"portMax":40000,"aiProxyPort":8744,"hostHttpsPort":443,"reservedPorts":[8740,2222,8744,8742,8743]}`
		if err := os.WriteFile(p, []byte(valid), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfig(p)
		if err != nil || len(cfg.ReservedPorts) != 5 || cfg.AIProxyPort != 8744 {
			t.Fatalf("valid config rejected: %+v %v", cfg, err)
		}

		for _, bad := range []string{
			`{"portMin":20000,"portMax":40000,"aiProxyPort":20000}`,
			`{"portMin":20000,"portMax":40000,"aiProxyPort":30000}`,
			`{"portMin":20000,"portMax":40000,"aiProxyPort":40000}`,
			`{"portMin":20000,"portMax":40000,"aiProxyPort":65536}`,
			`{"portMin":20000,"portMax":40000,"aiProxyPort":-1}`,
			`{"portMin":20000,"portMax":40000,"reservedPorts":[0]}`,
			`{"portMin":20000,"portMax":40000,"reservedPorts":[-5]}`,
			`{"portMin":20000,"portMax":40000,"reservedPorts":[65536]}`,
			`{"portMin":20000,"portMax":40000,"reservedPorts":[20000]}`,
			`{"portMin":20000,"portMax":40000,"reservedPorts":[25000]}`,
			`{"portMin":20000,"portMax":40000,"reservedPorts":[40000]}`,
		} {
			if err := os.WriteFile(p, []byte(bad), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadConfig(p); err == nil {
				t.Errorf("invalid config accepted: %s", bad)
			}
		}
	})

	t.Run("freePortLocked_skips_reserved_and_bound_tcp_ports", func(t *testing.T) {
		// Bind a real local TCP listener on loopback and set PortMin..PortMax around it.
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		boundPort := ln.Addr().(*net.TCPAddr).Port
		if boundPort < 1026 || boundPort > 65533 {
			t.Skipf("ephemeral port %d near range boundary", boundPort)
		}
		// Reserve boundPort-1; boundPort is actively listening; boundPort+1 is free.
		m, _ := newTestManager(t, func(c *Config) {
			c.PortMin = boundPort - 1
			c.PortMax = boundPort + 1
			c.ReservedPorts = []int{boundPort - 1}
		})
		v, err := m.Create(context.Background(), CreateReq{Team: "t1"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := m.AddPort(v.ID, 8080, true)
		if err != nil {
			t.Fatalf("AddPort: %v", err)
		}
		if got.HostPort != boundPort+1 {
			t.Fatalf("AddPort allocated %d (reserved=%d bound=%d), want %d", got.HostPort, boundPort-1, boundPort, boundPort+1)
		}
		// No more free ports in range -> 503.
		var ae *apiErr
		if _, err := m.AddPort(v.ID, 8081, true); !errors.As(err, &ae) || ae.code != 503 {
			t.Fatalf("expected 503 when only reserved/bound ports remain, got %v", err)
		}
	})

	t.Run("output_nat_and_prerouting_exclude_loopback", func(t *testing.T) {
		c := defaultConfig()
		vms := []VM{{
			ID: "vm-1", State: Running, SSHBootEpoch: "ep", SSHPolicyReady: true,
			Index: 2, Team: "t1", Tap: "bt2", IP: c.ipFor(2), MAC: macFor(2),
			Ports: []Port{{GuestPort: 8080, HostPort: 20000}},
		}}
		rs := ruleset(c, vms)
		want := "ip daddr != 127.0.0.0/8 fib daddr type local tcp dport 20000 dnat ip to 10.200.0.2:8080"
		outNat := inetChain(t, rs, "output_nat")
		if !strings.Contains(outNat, want) {
			t.Fatalf("output_nat missing loopback exclusion %q:\n%s", want, outNat)
		}
		preNat := inetChain(t, rs, "prerouting")
		if !strings.Contains(preNat, want) {
			t.Fatalf("prerouting missing loopback exclusion %q:\n%s", want, preNat)
		}
	})
}

// --- E. Protected export provenance & export registry ---

func TestR4ProtectedExportProvenanceAndRegistry(t *testing.T) {
	t.Setenv("COPYFILE_DISABLE", "1")
	m, _ := newTestManager(t, nil)
	testExporter(m)
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	ctx := context.Background()

	// Helper to unpack tar members.
	unpackTar := func(arch []byte) ([]string, map[string][]byte) {
		t.Helper()
		tr := tar.NewReader(bytes.NewReader(arch))
		var order []string
		members := map[string][]byte{}
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			order = append(order, hdr.Name)
			members[hdr.Name] = b
		}
		return order, members
	}

	// 1. Protected round trip via GET /v1/snapshots/{sid}/export?team=...&protected=1.
	v, err := m.Create(ctx, CreateReq{Team: "team-a", Labels: map[string]string{"boat.no_env": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.CreateSnapshot(ctx, v.ID, "prot-snap")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.snapDir(s.ID), diskFile), []byte("protected-noenv-disk-v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Wrong team or missing team on /export returns 404.
	if c := call(t, srv, "GET", "/v1/snapshots/"+s.ID+"/export?team=team-b&protected=1", nil, &[]byte{}); c != 404 {
		t.Fatalf("wrong team export: status=%d, want 404", c)
	}
	if c := call(t, srv, "GET", "/v1/snapshots/"+s.ID+"/export?protected=1", nil, &[]byte{}); c != 404 {
		t.Fatalf("missing team export: status=%d, want 404", c)
	}

	var arch []byte
	if c := call(t, srv, "GET", "/v1/snapshots/"+s.ID+"/export?team=team-a&protected=1", nil, &arch); c != 200 || len(arch) == 0 {
		t.Fatalf("protected export: status=%d len=%d", c, len(arch))
	}

	// Verify manifest has protected:true covered by HMAC.
	order, members := unpackTar(arch)
	var mf exportManifest
	if err := json.Unmarshal(members[manifestFile], &mf); err != nil {
		t.Fatal(err)
	}
	if !mf.Protected || !mf.NoEnv || mf.Sig == "" {
		t.Fatalf("manifest missing protected/no_env/sig: %+v", mf)
	}
	// Flipping Protected breaks HMAC verification.
	tamperedMF := mf
	tamperedMF.Protected = false
	if ok, _ := m.verifyManifest(tamperedMF, m.snapDir(s.ID), "team-a"); ok {
		t.Fatal("HMAC did not cover Protected field in manifest")
	}

	// Verify exports.json exists with mode 0600.
	st, err := os.Stat(filepath.Join(m.cfg.StateDir, exportRegistryFile))
	if err != nil {
		t.Fatalf("exports.json missing: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("exports.json mode=%o, want 0600", st.Mode().Perm())
	}

	// Import via HTTP endpoint: response JSON gets top-level "protected": true.
	req, err := http.NewRequest("POST", srv.URL+"/v1/snapshots/import?team=team-a&name=rt", bytes.NewReader(arch))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rawImp, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("protected import status=%d body=%s", resp.StatusCode, rawImp)
	}
	var impMap map[string]any
	if err := json.Unmarshal(rawImp, &impMap); err != nil {
		t.Fatal(err)
	}
	if impMap["protected"] != true || impMap["no_env"] != true || impMap["has_mem"] != true {
		t.Fatalf("protected round trip JSON mismatch: %s", rawImp)
	}

	// 2. Stripped-manifest import of a no_env + protected export:
	//    drops vmstate/mem/kernel, restores NoEnv, and restores Protected for same team.
	strippedTar := tarOf(t, func(tw *tar.Writer) {
		for _, name := range order {
			if name == manifestFile {
				continue
			}
			tarFile(tw, name, string(members[name]))
		}
	})
	strippedSame, err := m.ImportSnapshot(ctx, bytes.NewReader(strippedTar.Bytes()), "team-a", "stripped-same", "", "")
	if err != nil {
		t.Fatalf("stripped same-team import: %v", err)
	}
	if strippedSame.HasMem {
		t.Fatalf("stripped import kept memory: %+v", strippedSame)
	}
	if !strippedSame.NoEnv || strippedSame.Labels["boat.no_env"] != "1" {
		t.Fatalf("stripped import dropped sticky no_env: %+v", strippedSame)
	}
	if !strippedSame.Protected {
		t.Fatalf("stripped same-team import did not restore protected from registry: %+v", strippedSame)
	}

	// Cross-team stripped import: keeps sticky no_env, MUST NOT grant protected.
	strippedCross, err := m.ImportSnapshot(ctx, bytes.NewReader(strippedTar.Bytes()), "team-b", "stripped-cross", "", "")
	if err != nil {
		t.Fatalf("stripped cross-team import: %v", err)
	}
	if !strippedCross.NoEnv || strippedCross.Labels["boat.no_env"] != "1" {
		t.Fatalf("stripped cross-team import dropped sticky no_env: %+v", strippedCross)
	}
	if strippedCross.Protected {
		t.Fatalf("stripped cross-team import falsely granted protected: %+v", strippedCross)
	}

	// 3. Modified disk is not recognized by registry.
	modDiskTar := tarOf(t, func(tw *tar.Writer) {
		mod := append([]byte(nil), members[diskFile]...)
		mod[0] ^= 0xff
		tarFile(tw, diskFile, string(mod))
	})
	modImp, err := m.ImportSnapshot(ctx, bytes.NewReader(modDiskTar.Bytes()), "team-a", "mod-disk", "", "")
	if err != nil {
		t.Fatalf("modified disk import: %v", err)
	}
	if modImp.Protected || modImp.NoEnv {
		t.Fatalf("modified disk falsely matched registry: %+v", modImp)
	}

	// 4. Untrusted archive (forged manifest claiming protected:true and boat.protected_lineage label) NEVER gets protected.
	forgedMF := exportManifest{
		Snapshot: Snapshot{
			ID: "snap-fake", Team: "team-a", Image: m.cfg.DefaultImage, Size: m.cfg.DefaultSize,
			VCPU: 2, MemMiB: 512, DiskGiB: 1,
			Labels: map[string]string{"boat.protected_lineage": "1"},
		},
		Protected: true,
		Sig:       "deadbeef",
	}
	forgedBytes, _ := json.Marshal(forgedMF)
	untrustedTar := tarOf(t, func(tw *tar.Writer) {
		tarFile(tw, manifestFile, string(forgedBytes))
		tarFile(tw, diskFile, "arbitrary-tenant-uploaded-disk")
	})
	untrustedImp, err := m.ImportSnapshot(ctx, bytes.NewReader(untrustedTar.Bytes()), "team-a", "untrusted", "", "")
	if err != nil {
		t.Fatalf("untrusted import: %v", err)
	}
	if untrustedImp.Protected || untrustedImp.HasMem {
		t.Fatalf("untrusted archive got protected or memory: %+v", untrustedImp)
	}

	// 5. Export without protected=1 even when snapshot carries boat.protected_lineage label does NOT set protected.
	vPlain, err := m.Create(ctx, CreateReq{Team: "team-a", Labels: map[string]string{"boat.protected_lineage": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	sPlain, err := m.CreateSnapshot(ctx, vPlain.ID, "plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.snapDir(sPlain.ID), diskFile), []byte("unprotected-disk-v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	var plainArch []byte
	if c := call(t, srv, "GET", "/v1/vms/"+vPlain.ID+"/snapshots/"+sPlain.ID+"/download", nil, &plainArch); c != 200 {
		t.Fatalf("plain download: %d", c)
	}
	plainImp, err := m.ImportSnapshot(ctx, bytes.NewReader(plainArch), "team-a", "plain-imp", "", "")
	if err != nil {
		t.Fatalf("plain import: %v", err)
	}
	if plainImp.Protected {
		t.Fatalf("plain export without protected=1 got protected from labels: %+v", plainImp)
	}

	// 6. Export registry persists across reload and stays bounded at maxExportRegistryEntries (2048).
	m2, _ := newTestManager(t, func(c *Config) { c.StateDir = m.cfg.StateDir })
	rec, ok := m2.lookupExport(mf.Disk)
	if !ok || !rec.NoEnv || !rec.Protected || rec.Team != "team-a" {
		t.Fatalf("reloaded export registry lost record: ok=%v rec=%+v", ok, rec)
	}

	mBound, _ := newTestManager(t, nil)
	base := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	mBound.exportMu.Lock()
	mBound.exportReg = make(map[string]exportRecord, maxExportRegistryEntries+5)
	for i := 0; i < maxExportRegistryEntries+4; i++ {
		mBound.exportReg[fmt.Sprintf("hash-%04d", i)] = exportRecord{
			Team: "t1", NoEnv: true, At: base.Add(time.Duration(i) * time.Second),
		}
	}
	mBound.exportMu.Unlock()
	mBound.now = func() time.Time { return base.Add(10 * time.Hour) }
	if err := mBound.recordExport("hash-newest", "t1", true, true); err != nil {
		t.Fatalf("recordExport bound: %v", err)
	}
	mBound.exportMu.Lock()
	gotLen := len(mBound.exportReg)
	_, hasOldest := mBound.exportReg["hash-0000"]
	_, hasNewest := mBound.exportReg["hash-newest"]
	mBound.exportMu.Unlock()
	if gotLen != maxExportRegistryEntries || hasOldest || !hasNewest {
		t.Fatalf("export registry bound failed: len=%d hasOldest=%v hasNewest=%v", gotLen, hasOldest, hasNewest)
	}
}

// --- F. Structured guest error codes ---

func TestR4StructuredGuestErrorMapping(t *testing.T) {
	cases := []struct {
		name         string
		err          *agentErr
		wantHTTP     int
		wantCode     string
		wantNotFound bool
	}{
		// New R4 structured codes (must be authoritative even if message contains "No such").
		{"invalid_user", &agentErr{msg: "no such user: ghost", code: "invalid_user"}, 400, "invalid_user", false},
		{"spawn_failed_enoent_msg", &agentErr{msg: "spawn: [Errno 2] No such file or directory: '/missing'", code: "spawn_failed"}, 400, "spawn_failed", false},
		{"unsupported", &agentErr{msg: "desktop is not installed in this image (slim variant)", code: "unsupported"}, 422, "unsupported", false},
		{"not_supported", &agentErr{msg: "desktop is not installed", code: "not_supported"}, 422, "not_supported", false},
		// Legacy compatibility: old images sending not_found or empty code with "no such user".
		{"legacy_not_found_no_such_user", &agentErr{msg: "no such user: ghost", code: "not_found"}, 400, "invalid_user", false},
		{"legacy_empty_no_such_user", &agentErr{msg: "No such user: ghost", code: ""}, 400, "invalid_user", false},
		// 404 codes.
		{"not_found", &agentErr{msg: "No such command: c_1234", code: "not_found"}, 404, "not_found", true},
		{"no_such_file", &agentErr{msg: "missing", code: "no_such_file"}, 404, "no_such_file", true},
		{"no_such_command", &agentErr{msg: "missing", code: "no_such_command"}, 404, "no_such_command", true},
		{"enoent", &agentErr{msg: "missing", code: "enoent"}, 404, "enoent", true},
		// 409 codes (including is_a_directory).
		{"conflict", &agentErr{msg: "not found conflict text", code: "conflict"}, 409, "conflict", false},
		{"already_exists", &agentErr{msg: "dst exists", code: "already_exists"}, 409, "already_exists", false},
		{"is_a_directory", &agentErr{msg: "/tmp is a directory", code: "is_a_directory"}, 409, "is_a_directory", false},
		{"not_empty", &agentErr{msg: "dir not empty", code: "not_empty"}, 409, "not_empty", false},
		{"not_running", &agentErr{msg: "command exited", code: "not_running"}, 409, "not_running", false},
		// 403 codes.
		{"permission_denied", &agentErr{msg: "cannot open /root/secret", code: "permission_denied"}, 403, "permission_denied", false},
		{"eacces", &agentErr{msg: "denied", code: "eacces"}, 403, "eacces", false},
		{"eperm", &agentErr{msg: "denied", code: "eperm"}, 403, "eperm", false},
		// 400 codes (authoritative even if msg says "not found").
		{"invalid_argument_code_first", &agentErr{msg: "not found in schema", code: "invalid_argument"}, 400, "invalid_argument", false},
		{"protected_path", &agentErr{msg: "refusing to delete /", code: "protected_path"}, 400, "protected_path", false},
		{"not_a_directory", &agentErr{msg: "open /etc/hosts", code: "not_a_directory"}, 400, "not_a_directory", false},
		// Unknown code -> 502.
		{"unknown_code", &agentErr{msg: "internal panic", code: "totally_unknown_code"}, 502, "totally_unknown_code", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.notFound(); got != tc.wantNotFound {
				t.Fatalf("notFound()=%v, want %v", got, tc.wantNotFound)
			}
			rec := httptest.NewRecorder()
			writeErr(rec, tc.err)
			if rec.Code != tc.wantHTTP {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.wantHTTP, rec.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			wantErr := "guest agent: " + tc.err.msg
			if body["error"] != wantErr {
				t.Fatalf("error=%q want=%q", body["error"], wantErr)
			}
			if body["code"] != tc.wantCode {
				t.Fatalf("code=%q want=%q", body["code"], tc.wantCode)
			}
		})
	}
}
