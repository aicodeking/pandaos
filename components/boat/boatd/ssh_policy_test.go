package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSSHBootIdentity(t *testing.T) {
	m, _ := newTestManager(t, nil)
	v, err := m.Create(context.Background(), CreateReq{Team: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	fields := func(v VM) map[string]any {
		b, _ := json.Marshal(v)
		var x map[string]any
		json.Unmarshal(b, &x)
		return x
	}
	epoch, _ := fields(v)["ssh_boot_epoch"].(string)
	if len(epoch) < 64 {
		t.Fatalf("missing unpredictable boot identity: %q", epoch)
	}
	if fields(v)["ssh_policy_ready"] == true {
		t.Fatal("new boot trusted before install")
	}
	if _, err = m.Stop(context.Background(), v.ID, "api"); err != nil {
		t.Fatal(err)
	}
	v, err = m.Start(context.Background(), v.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fields(v)["ssh_boot_epoch"] == epoch {
		t.Fatal("restore reused boot identity")
	}
	child, err := m.Fork(context.Background(), v.ID, "child", "blue", nil)
	if err != nil {
		t.Fatal(err)
	}
	if fields(child)["ssh_boot_epoch"] == fields(v)["ssh_boot_epoch"] || fields(child)["ssh_policy_ready"] == true {
		t.Fatal("fork inherited boot policy identity")
	}

}

func TestSSHUnreadyNetworkBarrier(t *testing.T) {
	c := defaultConfig()
	c.IPv6Prefix = "2001:db8:1::/64"
	c.RunDir = t.TempDir()
	v := VM{ID: "vm-a", State: Running, Index: 2, IP: c.ipFor(2), IPv6: c.ipv6For(2), MAC: macFor(2), Tap: "bt2", Ports: []Port{{GuestPort: 22, HostPort: 20000}}}
	rs := ruleset(c, []VM{v})
	for _, rule := range []string{`ip daddr 10.200.0.2 drop`, `ip6 daddr 2001:db8:1::2 drop`, `oifname "bt2" drop`} {
		if !strings.Contains(rs, rule) {
			t.Errorf("missing quarantine %s", rule)
		}
	}
	if strings.Contains(rs, "dnat ip to 10.200.0.2:22") {
		t.Error("unready DNAT published")
	}
	if err := writeRoutes(c, []VM{v}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(c.RunDir, "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "vm-a") {
		t.Fatal("unready route published")
	}
	var rf routeFile
	if err := json.Unmarshal(b, &rf); err != nil {
		t.Fatal(err)
	}
	if len(rf.VMs) != 0 || strings.Join(rf.Quarantined, ",") != "10.200.0.2" {
		t.Fatalf("unexpected routes.json for quarantined VM: %+v", rf)
	}
}

func TestSSHRoutesFailurePropagates(t *testing.T) {
	m, _ := newTestManager(t, nil)
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0600); err != nil {
		t.Fatal(err)
	}
	m.cfg.RunDir = filepath.Join(f, "bad")
	if err := m.syncNet(); err == nil {
		t.Fatal("routes write failure ignored")
	}
	m.run = func(string, string, ...string) error { return errors.New("firewall failed") }
	if err := m.syncNet(); err == nil {
		t.Fatal("firewall failure ignored")
	}
}

// Dedicated unprivileged vsock-protocol peer. It attests policy installation,
// without running guest commands or changing host networking.
func sshPolicyPeer(t *testing.T, m *Manager, v VM, respond func(map[string]any) []map[string]any) *atomic.Int32 {
	t.Helper()
	var healthCalls atomic.Int32
	if err := os.MkdirAll(m.root(v.ID), 0700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(m.root(v.ID), vsockUDS))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { // the real guest serves each conn on its own thread (pooled conns idle open)
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
					if q["op"] == "health" {
						healthCalls.Add(1)
						json.NewEncoder(c).Encode(map[string]any{"ok": true, "trusted_root_exec": true})
						continue
					}
					for _, r := range respond(q) {
						json.NewEncoder(c).Encode(r)
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); <-done })
	return &healthCalls
}
func sshPolicyFixture(t *testing.T) (*Manager, VM, sshPolicyRequest) {
	t.Helper()
	m, _ := newTestManager(t, nil)
	v, err := m.alloc(VM{Team: "blue", Size: "default", VCPU: 1, MemMiB: 512, DiskGiB: 1}, Running)
	if err != nil {
		t.Fatal(err)
	}
	v, err = m.resetSSHBoot(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	v, err = m.update(v.ID, "", func(x *VM) { x.PID = 123; x.AgentReady = true })
	if err != nil {
		t.Fatal(err)
	}
	c := sshPolicyConfig{Version: 1, URL: "https://api.example/v1/vms/" + v.ID + "/ssh/authorize", Token: "lookup-only", VMID: v.ID, TeamID: v.Team, Generation: 0, BootEpoch: v.SSHBootEpoch, Users: []string{"dev"}}
	return m, v, sshPolicyRequest{Version: 1, BootEpoch: v.SSHBootEpoch, Config: c, HelperB64: b64("helper")}
}
func sshAttest(r sshPolicyRequest) []map[string]any {
	_, digest, _ := canonicalSSHConfig(r.Config)
	proof, _ := json.Marshal(sshPolicyGuestProof{Version: 1, ConfigDigest: digest, BootEpoch: r.BootEpoch, Readiness: true})
	return []map[string]any{{"stream": "stdout", "data_b64": b64(string(proof))}, {"exit": 0}}
}
func TestSSHInstallAndFailure(t *testing.T) {
	for _, mode := range []string{"success", "exit", "proof", "trailing", "pid", "team", "epoch", "route", "firewall"} {
		t.Run(mode, func(t *testing.T) {
			m, v, r := sshPolicyFixture(t)
			var ops []string
			healthCalls := sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
				if q["op"] == "put_file" {
					if q["owner"] != "root" || q["mode"] != "0555" || q["path"] != sshPolicyHelper {
						t.Error("helper ownership/path")
					}
					ops = append(ops, "put")
					return []map[string]any{{"ok": true}}
				}
				if q["user"] != "root" || q["trusted_root_exec"] != true || q["stdin_b64"] == "" {
					t.Error("installer not root/config absent")
				}
				argv := q["argv"].([]any)
				ops = append(ops, fmt.Sprint(argv[3]))
				if argv[3] == "verify" {
					switch mode {
					case "exit":
						return []map[string]any{{"exit": 1}}
					case "proof":
						return []map[string]any{{"stream": "stdout", "data_b64": b64(`{"version":1}`)}, {"exit": 0}}
					case "trailing":
						rs := sshAttest(r)
						rs[0]["data_b64"] = b64(`{} {}`)
						return rs
					case "pid", "team", "epoch":
						m.update(v.ID, "", func(x *VM) {
							if mode == "pid" {
								x.PID++
							}
							if mode == "team" {
								x.Team = "red"
							}
							if mode == "epoch" {
								x.SSHBootEpoch = "different"
							}
						})
					case "route":
						m.cfg.RunDir = filepath.Join(m.cfg.StateDir, "images", m.cfg.DefaultImage, kernFile, "invalid")
					case "firewall":
						m.run = func(string, string, ...string) error { return errors.New("firewall") }
					}
				}
				return sshAttest(r)
			})
			result, err := m.installSSHPolicy(context.Background(), v.ID, r)
			now, _ := m.get(v.ID)
			if mode == "success" {
				if err != nil || !result.Readiness || !now.sshNetworkReady() {
					t.Fatalf("install: %+v %v", result, err)
				}
				b, _ := os.ReadFile(filepath.Join(m.vmDir(v.ID), "vm.json"))
				if strings.Contains(string(b), "lookup-only") {
					t.Fatal("credential persisted")
				}
				if strings.Join(ops, ",") != "put,install,verify" {
					t.Fatal(ops)
				}
				if got := healthCalls.Load(); got != 1 {
					t.Fatalf("installSSHPolicy made %d health checks, want 1", got)
				}
			} else if err == nil || now.SSHPolicyReady {
				t.Fatalf("failure released ingress: %+v %v", now, err)
			}
		})
	}
}
func TestSSHStaleInstallAndSerialization(t *testing.T) {
	m, v, r := sshPolicyFixture(t)
	stale := r
	stale.BootEpoch = "stale"
	if _, err := m.installSSHPolicy(context.Background(), v.ID, stale); err == nil {
		t.Fatal("stale accepted")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
		if q["op"] == "put_file" {
			once.Do(func() { close(entered); <-release })
			return []map[string]any{{"ok": true}}
		}
		return sshAttest(r)
	})
	done := make(chan error, 1)
	go func() { _, err := m.installSSHPolicy(context.Background(), v.ID, r); done <- err }()
	<-entered
	if !m.lockBusy(v.ID) {
		t.Error("installer lacks lifecycle lock")
	}
	if _, err := m.get(v.ID); err != nil {
		t.Error("manager lock held across agent IO")
	}
	if now, _ := m.get(v.ID); now.SSHPolicyReady {
		t.Error("published before proof")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestSSHAdoptionResetsPersistedReadiness(t *testing.T) {
	m, h := newTestManager(t, nil)
	v, err := m.Create(context.Background(), CreateReq{Team: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	v, err = m.update(v.ID, "", func(x *VM) { x.SSHPolicyReady = true; x.SSHPolicyDigest = "old" })
	if err != nil {
		t.Fatal(err)
	}
	m.closeWatches()
	other, err := newManager(m.cfg, h.run, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.closeWatches)
	adopted, err := other.get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.SSHBootEpoch == v.SSHBootEpoch || adopted.SSHPolicyReady || adopted.SSHPolicyDigest != "" {
		t.Fatalf("persisted readiness adopted: %+v", adopted)
	}
}

func TestSSHSnapshotSourceContinuation(t *testing.T) {
	for _, mode := range []string{"success", "verify", "resume", "quarantine"} {
		t.Run(mode, func(t *testing.T) {
			m, h := newTestManager(t, nil)
			v, err := m.Create(context.Background(), CreateReq{Team: "blue"})
			if err != nil {
				t.Fatal(err)
			}
			// Replace only the fake agent endpoint; the fake VMM snapshot API stays live.
			sock := filepath.Join(m.root(v.ID), vsockUDS)
			if err := os.Rename(sock, sock+".old"); err != nil {
				t.Fatal(err)
			}
			c := sshPolicyConfig{Version: 1, URL: "https://api.example/v1/vms/" + v.ID + "/ssh/authorize", Token: "lookup", VMID: v.ID, TeamID: v.Team, Generation: 0, BootEpoch: v.SSHBootEpoch, Users: []string{"dev"}}
			r := sshPolicyRequest{Version: 1, BootEpoch: v.SSHBootEpoch, Config: c, HelperB64: b64("helper")}
			var snapshot atomic.Bool
			sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
				if q["op"] != "exec" {
					return []map[string]any{{"ok": true}}
				}
				if snapshot.Load() {
					now, _ := m.get(v.ID)
					if now.SSHPolicyReady {
						t.Error("source ready before verify")
					}
					var rf routeFile
					if err := readJSON(filepath.Join(m.cfg.RunDir, "routes.json"), &rf); err != nil {
						t.Error(err)
					}
					if _, ok := rf.VMs[v.ID]; ok {
						t.Error("source route released before verify")
					}
					if mode == "verify" {
						return []map[string]any{{"exit": 1}}
					}
				}
				return sshAttest(r)
			})
			if _, err := m.installSSHPolicy(context.Background(), v.ID, r); err != nil {
				t.Fatal(err)
			}
			snapshot.Store(true)
			if mode == "resume" {
				h.mu.Lock()
				h.failResume = 4
				h.mu.Unlock()
			}
			if mode == "quarantine" {
				h.mu.Lock()
				h.nftErr = errors.New("nft unavailable")
				h.mu.Unlock()
			}
			_, err = m.CreateSnapshot(context.Background(), v.ID, "source")
			now, _ := m.get(v.ID)
			if now.SSHBootEpoch != v.SSHBootEpoch {
				t.Fatal("same VMM snapshot rotated epoch")
			}
			if mode == "success" {
				if err != nil || !now.SSHPolicyReady {
					t.Fatalf("source continuation: %v %+v", err, now)
				}
			} else if err == nil || now.SSHPolicyReady {
				t.Fatalf("failed snapshot released ingress: %v %+v", err, now)
			}
		})
	}
}

func inetChain(t *testing.T, rs, hook string) string {
	t.Helper()
	inet, _, ok := strings.Cut(rs, "table bridge boat {")
	if !ok {
		t.Fatalf("missing bridge table in ruleset:\n%s", rs)
	}
	marker := "  chain " + hook + " {\n"
	a := strings.Index(inet, marker)
	if a < 0 {
		t.Fatalf("missing inet chain %s:\n%s", hook, inet)
	}
	tail := inet[a:]
	b := strings.Index(tail, "\n  }")
	if b < 0 {
		t.Fatalf("unterminated inet chain %s:\n%s", hook, tail)
	}
	return tail[:b]
}

func bridgeOutputChain(t *testing.T, rs string) string {
	t.Helper()
	_, bridge, ok := strings.Cut(rs, "table bridge boat {")
	if !ok {
		t.Fatalf("missing bridge table in ruleset:\n%s", rs)
	}
	marker := "  chain output {"
	a := strings.Index(bridge, marker)
	if a < 0 {
		t.Fatalf("missing bridge output chain:\n%s", bridge)
	}
	tail := bridge[a:]
	b := strings.Index(tail, "\n  }")
	if b < 0 {
		t.Fatalf("unterminated bridge output chain:\n%s", tail)
	}
	return tail[:b]
}

func TestSSHQuarantineRuleMatrix(t *testing.T) {
	c := defaultConfig()
	c.IPv6Prefix = "2001:db8:1::/64"
	c.HostHTTPSPort = 8443
	c.Teams = map[string]TeamPolicy{"blue": {AllowIntraTeam: true}}
	v := VM{ID: "vm-a", Team: "blue", State: Running, Index: 2, IP: c.ipFor(2), MAC: macFor(2), Tap: "bt2", IPv6: c.ipv6For(2), PublicIP: "203.0.113.2", SSHBootEpoch: "epoch", Ports: []Port{{GuestPort: 22, HostPort: 20000}}}
	rs := ruleset(c, []VM{v})
	for _, hook := range []string{"input", "forward", "output"} {
		chain := inetChain(t, rs, hook)
		rule := "ip saddr " + v.IP + " drop"
		v6rule := "ip6 saddr " + v.IPv6 + " drop"
		if hook == "output" {
			rule = "ip daddr " + v.IP + " drop"
			v6rule = "ip6 daddr " + v.IPv6 + " drop"
		}
		if !strings.Contains(chain, rule) || !strings.Contains(chain, v6rule) {
			t.Errorf("%s lacks %s or %s:\n%s", hook, rule, v6rule, chain)
		}
		if i, j := strings.Index(chain, "ip daddr "+v.IP+" drop"), strings.Index(chain, "ct state established,related accept"); hook == "forward" && (i < 0 || i > j) {
			t.Error("forward quarantine behind established")
		}
	}
	inChain := inetChain(t, rs, "input")
	fwdChain := inetChain(t, rs, "forward")
	outChain := inetChain(t, rs, "output")
	brOut := bridgeOutputChain(t, rs)

	if strings.Count(inChain, "10.200.0.2") != 1 || strings.Count(inChain, "2001:db8:1::2") != 1 || strings.Contains(inChain[:strings.Index(inChain, "ip6 saddr 2001:db8:1::2 drop")], " accept\n") {
		t.Errorf("input granted HTTPS or DNS exception to quarantined VM:\n%s", inChain)
	}
	if strings.Contains(outChain, "sport") || strings.Contains(brOut, "sport") || strings.Contains(brOut, "arp accept") {
		t.Errorf("output or bridge output granted exception to quarantined VM:\noutput:\n%s\nbridge:\n%s", outChain, brOut)
	}
	if strings.Contains(fwdChain[:strings.Index(fwdChain, "ct state established,related accept")], " accept\n") {
		t.Errorf("forward quarantine emitted accept:\n%s", fwdChain)
	}
	for _, rule := range []string{
		`iifname "bt2" ether type ip6 drop`,
		`oifname "bt2" drop`,
	} {
		if !strings.Contains(rs, rule) {
			t.Errorf("missing %s\n%s", rule, rs)
		}
	}
	if strings.Contains(rs, "dnat ip to 10.200.0.2") || strings.Contains(rs, "set team0") {
		t.Error("quarantine published DNAT/team")
	}
	v.SSHPolicyReady = true
	rs = ruleset(c, []VM{v})
	if !strings.Contains(rs, "dnat ip to 10.200.0.2") || !strings.Contains(rs, "set team0") || strings.Contains(rs, "ip daddr 10.200.0.2 drop") {
		t.Error("verified VM remains quarantined")
	}
	v.PendingCopy = true
	if strings.Contains(ruleset(c, []VM{v}), "dnat ip to 10.200.0.2") {
		t.Error("copy barrier bypassed by ready flag")
	}
}

func TestSSHQuarantineControlModes(t *testing.T) {
	newQuarantined := func(c Config) VM {
		return VM{
			ID: "vm-q", Team: "blue", State: Running, Index: 2,
			IP: c.ipFor(2), MAC: macFor(2), Tap: "bt2", IPv6: c.ipv6For(2),
			SSHBootEpoch: "epoch",
		}
	}

	t.Run("zero_exception_quarantine_with_hostHttpsPort443", func(t *testing.T) {
		c := defaultConfig()
		c.IPv6Prefix = "2001:db8:1::/64"
		c.HostHTTPSPort = 443
		v := newQuarantined(c)
		rs := ruleset(c, []VM{v})

		inChain := inetChain(t, rs, "input")
		fwdChain := inetChain(t, rs, "forward")
		outChain := inetChain(t, rs, "output")
		brOut := bridgeOutputChain(t, rs)

		s4 := strings.Index(inChain, "ip saddr 10.200.0.2 drop")
		s6 := strings.Index(inChain, "ip6 saddr 2001:db8:1::2 drop")
		privV4Drop := strings.Index(inChain, `iifname "boatbr0" ip daddr { `+strings.Join(privateV4, ", ")+` } tcp dport 443 drop`)
		privV6Drop := strings.Index(inChain, `iifname "boatbr0" ip6 daddr { `+strings.Join(privateV6, ", ")+` } tcp dport 443 drop`)
		fibLocal := strings.Index(inChain, `iifname "boatbr0" fib daddr type local tcp dport 443 accept`)
		if !(s4 >= 0 && s6 > s4 && privV4Drop > s6 && privV6Drop > privV4Drop && fibLocal > privV6Drop) {
			t.Fatalf("input quarantine drops must precede privateV4/privateV6 drops and fib-local accept:\n%s", inChain)
		}
		if strings.Count(inChain, "10.200.0.2") != 1 || strings.Count(inChain, "2001:db8:1::2") != 1 || strings.Contains(inChain[:s6], " accept\n") {
			t.Fatalf("input leaked DNS or HTTPS exception for quarantined VM:\n%s", inChain)
		}
		est := "ct state established,related accept"
		if s, d, v6s, v6d, e := strings.Index(fwdChain, "ip saddr 10.200.0.2 drop"), strings.Index(fwdChain, "ip daddr 10.200.0.2 drop"), strings.Index(fwdChain, "ip6 saddr 2001:db8:1::2 drop"), strings.Index(fwdChain, "ip6 daddr 2001:db8:1::2 drop"), strings.Index(fwdChain, est); !(s >= 0 && s < d && d < v6s && v6s < v6d && v6d < e) {
			t.Fatalf("forward quarantine drops out of order:\n%s", fwdChain)
		}
		if strings.Contains(fwdChain[:strings.Index(fwdChain, est)], " accept\n") {
			t.Fatalf("forward quarantine emitted an accept:\n%s", fwdChain)
		}
		if !strings.Contains(outChain, "ip daddr 10.200.0.2 drop") || !strings.Contains(outChain, "ip6 daddr 2001:db8:1::2 drop") || strings.Contains(outChain, "sport") {
			t.Fatalf("output must drop quarantined IPv4/IPv6 with zero sport exceptions:\n%s", outChain)
		}
		if !strings.Contains(brOut, `oifname "bt2" drop`) || strings.Contains(brOut, "sport") || strings.Contains(brOut, "arp accept") {
			t.Fatalf("bridge output must unconditionally drop quarantined tap:\n%s", brOut)
		}
	})

	t.Run("hostHttpsPort0_closes_local_https", func(t *testing.T) {
		c := defaultConfig()
		c.HostHTTPSPort = 0
		v := newQuarantined(c)
		rs := ruleset(c, []VM{v})

		inChain := inetChain(t, rs, "input")
		fwdChain := inetChain(t, rs, "forward")
		outChain := inetChain(t, rs, "output")
		brOut := bridgeOutputChain(t, rs)

		for name, ch := range map[string]string{"input": inChain, "forward": fwdChain, "output": outChain, "bridge output": brOut} {
			if strings.Contains(ch, "443") || strings.Contains(ch, "tcp dport") && name == "input" || strings.Contains(ch, "tcp sport") {
				t.Errorf("%s opened unexpected TCP control port when hostHttpsPort=0:\n%s", name, ch)
			}
		}
	})

	t.Run("hostHttpsAddrs_exact_allowlist_and_zero_sshControlEndpoints_bypass", func(t *testing.T) {
		c := defaultConfig()
		c.IPv6Prefix = "2001:db8:1::/64"
		c.HostHTTPSPort = 443
		c.HostHTTPSAddrs = []string{"203.0.113.10", "198.51.100.0/24"}
		c.SSHControlEndpoints = []string{"198.51.100.10:8443", "203.0.113.10"}
		if err := c.validate(); err != nil {
			t.Fatalf("valid config rejected: %v", err)
		}
		v := newQuarantined(c)
		rs := ruleset(c, []VM{v})

		inChain := inetChain(t, rs, "input")
		fwdChain := inetChain(t, rs, "forward")
		outChain := inetChain(t, rs, "output")
		brOut := bridgeOutputChain(t, rs)

		s4 := strings.Index(inChain, "ip saddr 10.200.0.2 drop")
		s6 := strings.Index(inChain, "ip6 saddr 2001:db8:1::2 drop")
		allowIdx := strings.Index(inChain, `iifname "boatbr0" ip daddr { 203.0.113.10, 198.51.100.0/24 } tcp dport 443 accept`)
		if !(s4 >= 0 && s6 > s4 && allowIdx > s6) {
			t.Fatalf("input missing quarantine drops before HostHTTPSAddrs allowlist:\n%s", inChain)
		}
		if strings.Contains(inChain, "10.200.0.1 } tcp dport 443") || strings.Contains(inChain, "{ 10.200.0.1,") {
			t.Fatalf("HostHTTPSAddrs unexpectedly injected 10.200.0.1 on :443:\n%s", inChain)
		}
		if strings.Contains(inChain, "fib daddr type local") {
			t.Fatalf("HostHTTPSAddrs must replace fib daddr type local:\n%s", inChain)
		}
		if strings.Contains(fwdChain, "8443") || strings.Contains(fwdChain, "198.51.100.10") || strings.Contains(outChain, "sport") || strings.Contains(brOut, "8443") {
			t.Fatalf("SSHControlEndpoints must not punch quarantine holes:\nfwd:\n%s\nout:\n%s\nbr:\n%s", fwdChain, outChain, brOut)
		}
	})
}

func TestSyncNetPreservesPublishedRoutesAndDeduplicates(t *testing.T) {
	m, _ := newTestManager(t, nil)
	ctx := context.Background()

	var (
		mu       sync.Mutex
		nftCalls int
		midRoute []routeFile
	)
	baseRun := m.run
	m.run = func(stdin, name string, args ...string) error {
		if name == "nft" {
			var rf routeFile
			b, err := os.ReadFile(filepath.Join(m.cfg.RunDir, "routes.json"))
			if err != nil {
				t.Errorf("routes.json missing during nft -f -: %v", err)
			} else if err := json.Unmarshal(b, &rf); err != nil {
				t.Errorf("invalid routes.json during nft -f -: %v", err)
			}
			mu.Lock()
			nftCalls++
			midRoute = append(midRoute, rf)
			mu.Unlock()
		}
		return baseRun(stdin, name, args...)
	}
	resetObs := func() {
		mu.Lock()
		nftCalls = 0
		midRoute = nil
		mu.Unlock()
	}
	readCurrentRoutes := func() routeFile {
		t.Helper()
		var rf routeFile
		if err := readJSON(filepath.Join(m.cfg.RunDir, "routes.json"), &rf); err != nil {
			t.Fatal(err)
		}
		return rf
	}
	attachAttester := func(v VM) *[]string {
		t.Helper()
		sock := filepath.Join(m.root(v.ID), vsockUDS)
		_ = os.Remove(sock)
		var ops []string
		c := sshPolicyConfig{Version: 1, URL: "https://api.example/v1/vms/" + v.ID + "/ssh/authorize", Token: "tok", VMID: v.ID, TeamID: v.Team, Generation: 0, BootEpoch: v.SSHBootEpoch, Users: []string{"dev"}}
		req := sshPolicyRequest{Version: 1, BootEpoch: v.SSHBootEpoch, Config: c, HelperB64: b64("helper")}
		sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
			if q["op"] == "put_file" {
				ops = append(ops, "put")
				return []map[string]any{{"ok": true}}
			}
			if q["op"] != "exec" {
				return []map[string]any{{"ok": true}}
			}
			argv := q["argv"].([]any)
			ops = append(ops, fmt.Sprint(argv[3]))
			return sshAttest(req)
		})
		res, err := m.installSSHPolicy(ctx, v.ID, req)
		if err != nil || !res.Readiness {
			t.Fatalf("installSSHPolicy(%s): %+v %v", v.ID, res, err)
		}
		return &ops
	}

	// 1. Create vmA: exactly 1 nft reload, vmA is in quarantined.
	resetObs()
	vmA, err := m.Create(ctx, CreateReq{Team: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	cCalls := nftCalls
	mu.Unlock()
	if cCalls != 1 {
		t.Fatalf("Create(vmA) ran %d nft reloads, want 1", cCalls)
	}
	if rf := readCurrentRoutes(); len(rf.VMs) != 0 || strings.Join(rf.Quarantined, ",") != vmA.IP {
		t.Fatalf("after Create(vmA) routes = %+v", rf)
	}

	// 2. Promote vmA via installSSHPolicy: already fenced in nft, so skip pre-fence syncNet -> 1 nft reload total;
	// during nft -f -, vmA is still withheld from VMs and remains in Quarantined; after nft -f -, vmA is live!
	resetObs()
	opsA := attachAttester(vmA)
	if strings.Join(*opsA, ",") != "put,install,verify" {
		t.Fatalf("unexpected vsock ops: %v", *opsA)
	}
	mu.Lock()
	pCalls, pMid := nftCalls, append([]routeFile(nil), midRoute...)
	mu.Unlock()
	if pCalls != 1 {
		t.Fatalf("installSSHPolicy(vmA) ran %d nft reloads, want 1", pCalls)
	}
	if _, premature := pMid[0].VMs[vmA.ID]; premature || strings.Join(pMid[0].Quarantined, ",") != vmA.IP {
		t.Fatalf("newly promoted vmA leaked into routes.json before nft finished: %+v", pMid[0])
	}
	if rf := readCurrentRoutes(); rf.VMs[vmA.ID].IP != vmA.IP || len(rf.Quarantined) != 0 {
		t.Fatalf("after installSSHPolicy(vmA) routes = %+v", rf)
	}

	// 3. Create vmB while vmA is live: zero blackout of vmA during or after nft -f -!
	resetObs()
	vmB, err := m.Create(ctx, CreateReq{Team: "red"})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	bCalls, bMid := nftCalls, append([]routeFile(nil), midRoute...)
	mu.Unlock()
	if bCalls != 1 {
		t.Fatalf("Create(vmB) ran %d nft reloads, want 1", bCalls)
	}
	if bMid[0].VMs[vmA.ID].IP != vmA.IP {
		t.Fatalf("BLACKOUT: vmA disappeared from routes.json during Create(vmB) nft reload: %+v", bMid[0])
	}
	if rf := readCurrentRoutes(); rf.VMs[vmA.ID].IP != vmA.IP || len(rf.VMs) != 1 || strings.Join(rf.Quarantined, ",") != vmB.IP {
		t.Fatalf("after Create(vmB) routes = %+v", rf)
	}

	// 4. Promote vmB: vmA stays live during nft -f -, vmB withheld until nft -f - finishes.
	resetObs()
	_ = attachAttester(vmB)
	mu.Lock()
	pbCalls, pbMid := nftCalls, append([]routeFile(nil), midRoute...)
	mu.Unlock()
	if pbCalls != 1 {
		t.Fatalf("installSSHPolicy(vmB) ran %d nft reloads, want 1", pbCalls)
	}
	if pbMid[0].VMs[vmA.ID].IP != vmA.IP || pbMid[0].VMs[vmB.ID].IP != "" || strings.Join(pbMid[0].Quarantined, ",") != vmB.IP {
		t.Fatalf("during installSSHPolicy(vmB) nft reload: %+v", pbMid[0])
	}
	if rf := readCurrentRoutes(); rf.VMs[vmA.ID].IP != vmA.IP || rf.VMs[vmB.ID].IP != vmB.IP || len(rf.Quarantined) != 0 {
		t.Fatalf("after installSSHPolicy(vmB) routes = %+v", rf)
	}

	// 5. Stop vmB: vmB removed from routes BEFORE nft -f -, while vmA stays live throughout!
	resetObs()
	if _, err := m.Stop(ctx, vmB.ID, "api"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	sMid := append([]routeFile(nil), midRoute...)
	mu.Unlock()
	if len(sMid) == 0 || sMid[0].VMs[vmA.ID].IP != vmA.IP || sMid[0].VMs[vmB.ID].IP != "" {
		t.Fatalf("during Stop(vmB) nft reload: %+v", sMid)
	}

	// 6. CreateBatch(Count: 8): coalesced to 1 nft reload for all 8 VMs, vmA stays live, Quarantined sorted!
	resetObs()
	batchVMs, err := m.CreateBatch(ctx, CreateReq{Count: 8, Team: "green"})
	if err != nil || len(batchVMs) != 8 {
		t.Fatalf("CreateBatch: len=%d err=%v", len(batchVMs), err)
	}
	mu.Lock()
	batchCalls, batchMid := nftCalls, append([]routeFile(nil), midRoute...)
	mu.Unlock()
	if batchCalls != 1 {
		t.Fatalf("CreateBatch(8) ran %d nft reloads, want 1", batchCalls)
	}
	if batchMid[0].VMs[vmA.ID].IP != vmA.IP {
		t.Fatalf("vmA blacked out during CreateBatch: %+v", batchMid[0])
	}
	rf := readCurrentRoutes()
	if rf.VMs[vmA.ID].IP != vmA.IP || len(rf.Quarantined) != 8 {
		t.Fatalf("after CreateBatch routes = %+v", rf)
	}
	for i := 1; i < len(rf.Quarantined); i++ {
		if rf.Quarantined[i-1] >= rf.Quarantined[i] {
			t.Fatalf("quarantined IPs not sorted: %v", rf.Quarantined)
		}
	}
}

func TestStandaloneSSHMode(t *testing.T) {
	m, h := newTestManager(t, func(c *Config) {
		c.StandaloneSSH = true
		c.Teams = map[string]TeamPolicy{"blue": {MaxVMs: 10}, "_warm": {MaxVMs: 4}}
	})
	ctx := context.Background()

	v, err := m.Create(ctx, CreateReq{Team: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	if !v.SSHPolicyReady {
		t.Fatalf("Create in standaloneSSH mode left SSHPolicyReady=false: %+v", v)
	}
	var rf routeFile
	if err := readJSON(filepath.Join(m.cfg.RunDir, "routes.json"), &rf); err != nil {
		t.Fatal(err)
	}
	if rf.VMs[v.ID].IP != v.IP || len(rf.Quarantined) != 0 {
		t.Fatalf("standaloneSSH VM missing from live routes.json: %+v", rf)
	}

	// Warm pool VMs remain quarantined until claimed even in standaloneSSH mode.
	warm, err := m.Create(ctx, CreateReq{Team: "_warm"})
	if err != nil {
		t.Fatal(err)
	}
	if warm.SSHPolicyReady {
		t.Fatalf("_warm VM must stay quarantined in standaloneSSH mode: %+v", warm)
	}
	_, _ = m.Delete(ctx, warm.ID)

	// Stop + Start keeps standaloneSSH readiness.
	if _, err := m.Stop(ctx, v.ID, "api"); err != nil {
		t.Fatal(err)
	}
	v, err = m.Start(ctx, v.ID, nil)
	if err != nil || !v.SSHPolicyReady {
		t.Fatalf("Start in standaloneSSH mode: %+v %v", v, err)
	}

	// CreateSnapshot and Fork keep source and child ready without boat-api sshConfig.
	if _, err := m.CreateSnapshot(ctx, v.ID, "snap1"); err != nil {
		t.Fatalf("CreateSnapshot in standaloneSSH mode: %v", err)
	}
	if now, _ := m.get(v.ID); !now.SSHPolicyReady {
		t.Fatalf("source lost SSHPolicyReady after snapshot in standaloneSSH mode: %+v", now)
	}
	child, err := m.Fork(ctx, v.ID, "child", "blue", nil)
	if err != nil || !child.SSHPolicyReady {
		t.Fatalf("Fork in standaloneSSH mode: %+v %v", child, err)
	}

	// Restart/adoption promotes live Running VMs when standaloneSSH=true.
	m.closeWatches()
	m2, err := newManager(m.cfg, h.run, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m2.closeWatches)
	adopted, err := m2.get(v.ID)
	if err != nil || !adopted.SSHPolicyReady {
		t.Fatalf("adopted VM in standaloneSSH mode lost SSHPolicyReady: %+v %v", adopted, err)
	}
}

func TestSSHFirewallFailureStopsUnverifiedGuest(t *testing.T) {
	m, h := newTestManager(t, nil)
	v, err := m.Create(context.Background(), CreateReq{Team: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	m.cfg.CtPerVM = 10
	h.mu.Lock()
	h.nftErr = errors.New("nft refused")
	h.mu.Unlock()
	if err := m.syncNet(); err == nil {
		t.Fatal("firewall error ignored")
	}
	if h.Alive(v.PID, v.ID) {
		t.Fatal("unfenced guest kept running")
	}
	now, _ := m.get(v.ID)
	if now.PID != 0 || now.SSHPolicyReady {
		t.Fatalf("unfenced guest published: %+v", now)
	}
}

func TestSSHOldAgentRefusesTrustedRootExecution(t *testing.T) {
	m, h := newTestManager(t, nil)
	v, err := m.Create(context.Background(), CreateReq{Team: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	err = runSSHPolicy(context.Background(), filepath.Join(m.root(v.ID), vsockUDS), "install", []byte(`{}`), "digest", v.SSHBootEpoch)
	if err == nil || !strings.Contains(err.Error(), "lacks trusted root execution") {
		t.Fatalf("old agent accepted trustedroot: %v", err)
	}
	if h.fcOf(t, m, v.ID).called("agent exec") {
		t.Fatal("launched root process before capability check")
	}
}

func TestSSHPolicyBatchCoalescingAndFailClosed(t *testing.T) {
	attachPeer := func(t *testing.T, m *Manager, v VM, req sshPolicyRequest) *atomic.Int32 {
		t.Helper()
		sock := filepath.Join(m.root(v.ID), vsockUDS)
		if err := os.Rename(sock, sock+".old"); err != nil {
			t.Fatal(err)
		}
		return sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
			if q["op"] != "exec" {
				return []map[string]any{{"ok": true}}
			}
			return sshAttest(req)
		})
	}
	makeItem := func(v VM) sshPolicyBatchItem {
		cfg := sshPolicyConfig{
			Version: 1, URL: "https://api.example/v1/vms/" + v.ID + "/ssh/authorize",
			Token: "lookup-only", VMID: v.ID, TeamID: v.Team,
			Generation: 0, BootEpoch: v.SSHBootEpoch, Users: []string{"dev"},
		}
		return sshPolicyBatchItem{
			ID: v.ID,
			sshPolicyRequest: sshPolicyRequest{
				Version:   1,
				BootEpoch: v.SSHBootEpoch,
				Config:    cfg,
				HelperB64: b64("helper"),
			},
		}
	}

	t.Run("coalesces_4_vms_into_single_nft_reload", func(t *testing.T) {
		m, _ := newTestManager(t, nil)
		srv := m.routes()
		vms, err := m.CreateBatch(context.Background(), CreateReq{Team: "blue", Count: 4})
		if err != nil {
			t.Fatal(err)
		}
		var items []sshPolicyBatchItem
		var healthCounters []*atomic.Int32
		for _, v := range vms {
			it := makeItem(v)
			items = append(items, it)
			healthCounters = append(healthCounters, attachPeer(t, m, v, it.sshPolicyRequest))
		}
		var nftCalls atomic.Int32
		origRun := m.run
		m.run = func(stdin, name string, args ...string) error {
			if name == "nft" {
				nftCalls.Add(1)
			}
			return origRun(stdin, name, args...)
		}

		body, _ := json.Marshal(sshPolicyBatchRequest{Items: items})
		req := httptest.NewRequest(http.MethodPost, "/v1/vms/ssh-policy-batch", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("batch status=%d body=%s", rec.Code, rec.Body.String())
		}
		var rep sshPolicyBatchReply
		if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
			t.Fatal(err)
		}
		if len(rep.Results) != 4 || len(rep.Errors) != 0 {
			t.Fatalf("unexpected batch reply: %+v", rep)
		}
		if got := nftCalls.Load(); got != 1 {
			t.Fatalf("4-VM batch triggered %d nft reloads, want 1", got)
		}
		for i, c := range healthCounters {
			if got := c.Load(); got != 1 {
				t.Fatalf("vm[%d] made %d health checks, want 1", i, got)
			}
		}
		var rf routeFile
		if err := readJSON(filepath.Join(m.cfg.RunDir, "routes.json"), &rf); err != nil {
			t.Fatal(err)
		}
		if len(rf.VMs) != 4 || len(rf.Quarantined) != 0 {
			t.Fatalf("expected all 4 VMs live and 0 quarantined in routes.json: %+v", rf)
		}
	})

	t.Run("partial_epoch_mismatch_fails_closed_for_bad_item", func(t *testing.T) {
		m, _ := newTestManager(t, nil)
		srv := m.routes()
		vms, err := m.CreateBatch(context.Background(), CreateReq{Team: "blue", Count: 4})
		if err != nil {
			t.Fatal(err)
		}
		var items []sshPolicyBatchItem
		for i, v := range vms {
			it := makeItem(v)
			attachPeer(t, m, v, sshPolicyRequest{
				Version: it.Version, BootEpoch: it.BootEpoch, Config: it.Config, HelperB64: it.HelperB64,
			})
			if i == 2 {
				it.BootEpoch = "stale-epoch"
			}
			items = append(items, it)
		}
		var nftCalls atomic.Int32
		origRun := m.run
		m.run = func(stdin, name string, args ...string) error {
			if name == "nft" {
				nftCalls.Add(1)
			}
			return origRun(stdin, name, args...)
		}

		body, _ := json.Marshal(sshPolicyBatchRequest{Items: items})
		req := httptest.NewRequest(http.MethodPost, "/v1/vms/ssh-policy-batch", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("batch status=%d body=%s", rec.Code, rec.Body.String())
		}
		var rep sshPolicyBatchReply
		if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
			t.Fatal(err)
		}
		if len(rep.Results) != 3 || len(rep.Errors) != 1 || rep.Errors[vms[2].ID] == "" {
			t.Fatalf("unexpected partial batch reply: %+v", rep)
		}
		if got := nftCalls.Load(); got != 1 {
			t.Fatalf("partial batch triggered %d nft reloads, want 1", got)
		}
		bad, _ := m.get(vms[2].ID)
		if bad.SSHPolicyReady {
			t.Fatalf("stale-epoch VM released ingress: %+v", bad)
		}
		var rf routeFile
		if err := readJSON(filepath.Join(m.cfg.RunDir, "routes.json"), &rf); err != nil {
			t.Fatal(err)
		}
		if len(rf.VMs) != 3 || strings.Join(rf.Quarantined, ",") != bad.IP {
			t.Fatalf("expected 3 live and %s quarantined in routes.json: %+v", bad.IP, rf)
		}
	})

	t.Run("firewall_error_rolls_back_all_items_closed", func(t *testing.T) {
		m, _ := newTestManager(t, nil)
		srv := m.routes()
		vms, err := m.CreateBatch(context.Background(), CreateReq{Team: "blue", Count: 4})
		if err != nil {
			t.Fatal(err)
		}
		var items []sshPolicyBatchItem
		for _, v := range vms {
			it := makeItem(v)
			items = append(items, it)
			attachPeer(t, m, v, sshPolicyRequest{
				Version: it.Version, BootEpoch: it.BootEpoch, Config: it.Config, HelperB64: it.HelperB64,
			})
		}
		m.run = func(_, name string, _ ...string) error {
			if name == "nft" {
				return errors.New("nft reload refused")
			}
			return nil
		}

		body, _ := json.Marshal(sshPolicyBatchRequest{Items: items})
		req := httptest.NewRequest(http.MethodPost, "/v1/vms/ssh-policy-batch", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 on firewall failure, got %d: %s", rec.Code, rec.Body.String())
		}
		for _, v := range vms {
			now, _ := m.get(v.ID)
			if now.SSHPolicyReady {
				t.Fatalf("VM %s left SSHPolicyReady after firewall error: %+v", v.ID, now)
			}
		}
	})
}

func TestReprovisionSSHSkipsRestoreAfterPause(t *testing.T) {
	m, _ := newTestManager(t, nil)
	srv := m.routes()
	v, err := m.Create(context.Background(), CreateReq{Team: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(m.root(v.ID), vsockUDS)
	if err := os.Rename(sock, sock+".old"); err != nil {
		t.Fatal(err)
	}
	mkReq := func(gen int64) sshPolicyRequest {
		cfg := sshPolicyConfig{
			Version: 1, URL: "https://api.example/v1/vms/" + v.ID + "/ssh/authorize",
			Token: fmt.Sprintf("lookup-gen-%d", gen), VMID: v.ID, TeamID: v.Team,
			Generation: gen, BootEpoch: v.SSHBootEpoch, Users: []string{"dev"},
		}
		return sshPolicyRequest{Version: 1, BootEpoch: v.SSHBootEpoch, Config: cfg, HelperB64: b64("helper")}
	}
	var currentReq atomic.Value
	currentReq.Store(mkReq(0))
	var verifyCalls, rotateCalls atomic.Int32
	sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
		if q["op"] != "exec" {
			return []map[string]any{{"ok": true}}
		}
		argv := q["argv"].([]any)
		if len(argv) > 3 {
			switch fmt.Sprint(argv[3]) {
			case "verify":
				verifyCalls.Add(1)
			case "rotate-token":
				rotateCalls.Add(1)
			}
		}
		return sshAttest(currentReq.Load().(sshPolicyRequest))
	})

	// Initial install makes source ready.
	if _, err := m.installSSHPolicy(context.Background(), v.ID, currentReq.Load().(sshPolicyRequest)); err != nil {
		t.Fatal(err)
	}
	verifyCalls.Store(0)
	rotateCalls.Store(0)

	var nftMu sync.Mutex
	var fullReloads int
	var addCmds, delCmds []string
	origRun := m.run
	m.run = func(stdin, name string, args ...string) error {
		if name == "nft" {
			nftMu.Lock()
			if len(args) >= 2 && args[0] == "-f" && args[1] == "-" {
				fullReloads++
			} else if len(args) >= 2 && args[0] == "add" && args[1] == "element" {
				addCmds = append(addCmds, strings.Join(args, " "))
			} else if len(args) >= 2 && args[0] == "delete" && args[1] == "element" {
				delCmds = append(delCmds, strings.Join(args, " "))
			}
			nftMu.Unlock()
		}
		return origRun(stdin, name, args...)
	}
	defer func() { m.run = origRun }()

	// snapshots/prepare with reprovision_ssh:true applies transient SSH-only pause-fence
	// (incremental @ssh_pause_* add, parent stays in routes.json, 0 full nft reloads, 0 verify).
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/vms/"+v.ID+"/snapshots/prepare", strings.NewReader(`{"name":"s1","reprovision_ssh":true}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("snapshots/prepare status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := verifyCalls.Load(); got != 0 {
		t.Fatalf("reprovision_ssh=true ran %d post-pause verify calls, want 0", got)
	}
	now, _ := m.get(v.ID)
	if !now.SSHPolicyReady || !now.sshPauseFenced || now.SSHBootEpoch != v.SSHBootEpoch || !m.isSSHPausedInNft(now) {
		t.Fatalf("expected source SSH-pause-fenced with ready state preserved after reprovision_ssh=true: %+v", now)
	}
	var rf routeFile
	if err := readJSON(filepath.Join(m.cfg.RunDir, "routes.json"), &rf); err != nil {
		t.Fatal(err)
	}
	if _, live := rf.VMs[v.ID]; !live || len(rf.Quarantined) != 0 {
		t.Fatalf("expected parent to remain live in routes.json during SSH pause-fence: %+v", rf)
	}

	// Subsequent boat-api reprovision via POST /v1/vms/{id}/ssh-policy hits rotate-token
	// and clears @ssh_pause_* via incremental nft delete element with 0 full nft -f - reloads.
	nextReq := mkReq(1)
	currentReq.Store(nextReq)
	reqBody, _ := json.Marshal(nextReq)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/vms/"+v.ID+"/ssh-policy", bytes.NewReader(reqBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("reprovision ssh-policy status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rotateCalls.Load(); got != 1 {
		t.Fatalf("reprovision after reprovision_ssh=true ran %d rotate-token calls, want 1", got)
	}
	nftMu.Lock()
	gotFull, gotAdds, gotDels := fullReloads, append([]string(nil), addCmds...), append([]string(nil), delCmds...)
	nftMu.Unlock()
	if gotFull != 0 {
		t.Fatalf("snapshot+reprovision triggered %d full nft -f - reloads, want 0", gotFull)
	}
	if len(gotAdds) != 1 || !strings.Contains(gotAdds[0], "ssh_pause_v4 { "+v.IP+" }") {
		t.Fatalf("unexpected incremental add commands: %v", gotAdds)
	}
	if len(gotDels) != 1 || !strings.Contains(gotDels[0], "ssh_pause_v4 { "+v.IP+" }") {
		t.Fatalf("unexpected incremental delete commands: %v", gotDels)
	}
	now, _ = m.get(v.ID)
	if !now.SSHPolicyReady || now.sshPauseFenced || m.isSSHPausedInNft(now) {
		t.Fatalf("expected pause-fence cleared after rotate-token: %+v", now)
	}

	// fork/prepare with reprovision_ssh:true also uses SSH-only pause-fence and rotate-token with 0 full reloads.
	verifyCalls.Store(0)
	rotateCalls.Store(0)
	nftMu.Lock()
	fullReloads, addCmds, delCmds = 0, nil, nil
	nftMu.Unlock()
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/vms/"+v.ID+"/fork/prepare", strings.NewReader(`{"name":"c1","reprovision_ssh":true}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("fork/prepare status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := verifyCalls.Load(); got != 0 {
		t.Fatalf("fork/prepare reprovision_ssh=true ran %d post-pause verify calls, want 0", got)
	}
	now, _ = m.get(v.ID)
	if !now.SSHPolicyReady || !now.sshPauseFenced {
		t.Fatalf("expected source SSH-pause-fenced after fork/prepare reprovision_ssh=true: %+v", now)
	}
	// Parent stays live in routes.json across fork/prepare pause-fence.
	rf = routeFile{}
	if err := readJSON(filepath.Join(m.cfg.RunDir, "routes.json"), &rf); err != nil {
		t.Fatal(err)
	}
	if _, live := rf.VMs[v.ID]; !live {
		t.Fatalf("expected parent live in routes.json after fork/prepare reprovision_ssh=true: %+v", rf)
	}

	// Reprovision parent via rotate-token (0 full reloads), then verify default reprovision_ssh=false still runs restoreSSHAfterPause.
	nextReq = mkReq(2)
	currentReq.Store(nextReq)
	if _, err := m.installSSHPolicy(context.Background(), v.ID, nextReq); err != nil {
		t.Fatal(err)
	}
	if got := rotateCalls.Load(); got != 1 {
		t.Fatalf("fork reprovision ran %d rotate-token calls, want 1", got)
	}
	nftMu.Lock()
	gotFull = fullReloads
	nftMu.Unlock()
	if gotFull != 0 {
		t.Fatalf("fork/prepare+reprovision triggered %d full nft -f - reloads, want 0", gotFull)
	}
	m.run = origRun

	verifyCalls.Store(0)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/vms/"+v.ID+"/snapshots/prepare", strings.NewReader(`{"name":"s2"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("default snapshots/prepare status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := verifyCalls.Load(); got != 1 {
		t.Fatalf("default snapshots/prepare ran %d post-pause verify calls, want 1", got)
	}
	now, _ = m.get(v.ID)
	if !now.SSHPolicyReady {
		t.Fatalf("expected source restored to SSHPolicyReady when reprovision_ssh omitted: %+v", now)
	}
}

func TestSSHPauseFenceRulesetAndFailClosed(t *testing.T) {
	t.Run("ruleset_blocks_tcp22_syn_only_for_pause_fenced_vm", func(t *testing.T) {
		c := defaultConfig()
		c.IPv6Prefix = "2001:db8:1::/64"
		c.Teams = map[string]TeamPolicy{"blue": {AllowIntraTeam: true}}
		v := VM{
			ID: "vm-p", Team: "blue", State: Running, Index: 2,
			IP: c.ipFor(2), MAC: macFor(2), Tap: "bt2", IPv6: c.ipv6For(2),
			SSHBootEpoch: "ep", SSHPolicyReady: true, sshPauseFenced: true,
			Ports: []Port{{GuestPort: 8080, HostPort: 20000}},
		}
		rs := ruleset(c, []VM{v})
		for _, want := range []string{
			"set ssh_pause_v4 { type ipv4_addr; elements = { " + v.IP + " }; }",
			"set ssh_pause_v6 { type ipv6_addr; elements = { " + v.IPv6 + " }; }",
			fmt.Sprintf(`set ssh_pause_tap { type ifname; elements = { %q }; }`, v.Tap),
			"ip daddr @ssh_pause_v4 tcp dport 22 tcp flags & (fin|syn|rst|ack) == syn drop",
			"ip6 daddr @ssh_pause_v6 tcp dport 22 tcp flags & (fin|syn|rst|ack) == syn drop",
			"oifname @ssh_pause_tap ether type ip tcp dport 22 tcp flags & (fin|syn|rst|ack) == syn drop",
			"oifname @ssh_pause_tap ether type ip6 tcp dport 22 tcp flags & (fin|syn|rst|ack) == syn drop",
			"dnat ip to " + v.IP + ":8080",
		} {
			if !strings.Contains(rs, want) {
				t.Fatalf("ruleset missing %q:\n%s", want, rs)
			}
		}
		// Forward rule must sit before established/related accept; output chain must also block host->VM TCP/22 SYN.
		fwd := inetChain(t, rs, "forward")
		if pIdx, estIdx := strings.Index(fwd, "ip daddr @ssh_pause_v4 tcp dport 22"), strings.Index(fwd, "ct state established,related accept"); pIdx < 0 || pIdx > estIdx {
			t.Fatalf("@ssh_pause_v4 rule not before established:\n%s", fwd)
		}
		out := inetChain(t, rs, "output")
		if !strings.Contains(out, "ip daddr @ssh_pause_v4 tcp dport 22 tcp flags & (fin|syn|rst|ack) == syn drop") {
			t.Fatalf("output chain missing @ssh_pause_v4 TCP/22 SYN drop:\n%s", out)
		}
		// Must NOT emit full quarantine drop for a pause-fenced ready VM.
		if strings.Contains(fwd, "ip daddr "+v.IP+" drop") {
			t.Fatalf("pause-fenced ready VM got full quarantine drop:\n%s", fwd)
		}
	})

	t.Run("fail_closed_when_rotate_token_fails_on_pause_fenced_vm", func(t *testing.T) {
		m, _ := newTestManager(t, nil)
		v, err := m.Create(context.Background(), CreateReq{Team: "blue"})
		if err != nil {
			t.Fatal(err)
		}
		sock := filepath.Join(m.root(v.ID), vsockUDS)
		if err := os.Rename(sock, sock+".old"); err != nil {
			t.Fatal(err)
		}
		mkReq := func(gen int64) sshPolicyRequest {
			cfg := sshPolicyConfig{
				Version: 1, URL: "https://api.example/v1/vms/" + v.ID + "/ssh/authorize",
				Token: fmt.Sprintf("tok-%d", gen), VMID: v.ID, TeamID: v.Team,
				Generation: gen, BootEpoch: v.SSHBootEpoch, Users: []string{"dev"},
			}
			return sshPolicyRequest{Version: 1, BootEpoch: v.SSHBootEpoch, Config: cfg, HelperB64: b64("#!/usr/bin/env python3\n# rotate-token install-and-verify\n")}
		}
		var failRotate atomic.Bool
		var cur atomic.Value
		cur.Store(mkReq(0))
		sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
			if q["op"] != "exec" {
				return []map[string]any{{"ok": true}}
			}
			argv := q["argv"].([]any)
			if len(argv) > 3 && fmt.Sprint(argv[3]) == "rotate-token" && failRotate.Load() {
				return []map[string]any{{"stream": "stderr", "data_b64": b64("tampered")}, {"exit": 1}}
			}
			return sshAttest(cur.Load().(sshPolicyRequest))
		})
		if _, err := m.installSSHPolicy(context.Background(), v.ID, mkReq(0)); err != nil {
			t.Fatal(err)
		}
		if _, err := m.createSnapshotOpts(context.Background(), v.ID, "s1", true, true); err != nil {
			t.Fatal(err)
		}
		now, _ := m.get(v.ID)
		if !now.sshPauseFenced || !now.SSHPolicyReady {
			t.Fatalf("expected pause-fenced ready VM: %+v", now)
		}

		// Tampered rotate-token must fail closed into full quarantine and remove from routes.json.
		failRotate.Store(true)
		req1 := mkReq(1)
		cur.Store(req1)
		if _, err := m.installSSHPolicy(context.Background(), v.ID, req1); err == nil {
			t.Fatal("expected rotate-token error to fail closed")
		}
		now, _ = m.get(v.ID)
		if now.SSHPolicyReady || now.sshPauseFenced || !m.isQuarantinedInNft(now) {
			t.Fatalf("failed rotate-token did not escalate to full quarantine: %+v", now)
		}
		var rf routeFile
		if err := readJSON(filepath.Join(m.cfg.RunDir, "routes.json"), &rf); err != nil {
			t.Fatal(err)
		}
		if _, live := rf.VMs[v.ID]; live || strings.Join(rf.Quarantined, ",") != v.IP {
			t.Fatalf("failed rotate-token left VM live in routes.json: %+v", rf)
		}
	})

	t.Run("helper_change_on_pause_fenced_vm_falls_back_to_full_quarantine_and_install", func(t *testing.T) {
		m, _ := newTestManager(t, nil)
		v, err := m.Create(context.Background(), CreateReq{Team: "blue"})
		if err != nil {
			t.Fatal(err)
		}
		sock := filepath.Join(m.root(v.ID), vsockUDS)
		if err := os.Rename(sock, sock+".old"); err != nil {
			t.Fatal(err)
		}
		mkReq := func(gen int64, helper string) sshPolicyRequest {
			cfg := sshPolicyConfig{
				Version: 1, URL: "https://api.example/v1/vms/" + v.ID + "/ssh/authorize",
				Token: fmt.Sprintf("tok-%d", gen), VMID: v.ID, TeamID: v.Team,
				Generation: gen, BootEpoch: v.SSHBootEpoch, Users: []string{"dev"},
			}
			return sshPolicyRequest{Version: 1, BootEpoch: v.SSHBootEpoch, Config: cfg, HelperB64: b64(helper)}
		}
		var ops []string
		var opsMu sync.Mutex
		var cur atomic.Value
		req0 := mkReq(0, "#!/usr/bin/env python3\n# v1 rotate-token install-and-verify\n")
		cur.Store(req0)
		sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
			if q["op"] != "exec" {
				return []map[string]any{{"ok": true}}
			}
			argv := q["argv"].([]any)
			opsMu.Lock()
			ops = append(ops, fmt.Sprint(argv[3]))
			opsMu.Unlock()
			return sshAttest(cur.Load().(sshPolicyRequest))
		})
		if _, err := m.installSSHPolicy(context.Background(), v.ID, req0); err != nil {
			t.Fatal(err)
		}
		if _, err := m.createSnapshotOpts(context.Background(), v.ID, "s1", true, true); err != nil {
			t.Fatal(err)
		}
		opsMu.Lock()
		ops = nil
		opsMu.Unlock()

		// Changed helper must NOT use rotate-token: must full-quarantine + install-and-verify + syncNet.
		req1 := mkReq(1, "#!/usr/bin/env python3\n# v2 rotate-token install-and-verify\n")
		cur.Store(req1)
		if _, err := m.installSSHPolicy(context.Background(), v.ID, req1); err != nil {
			t.Fatalf("install with changed helper: %v", err)
		}
		opsMu.Lock()
		gotOps := strings.Join(ops, ",")
		opsMu.Unlock()
		if strings.Contains(gotOps, "rotate-token") || !strings.Contains(gotOps, "install-and-verify") {
			t.Fatalf("expected install-and-verify fallback on helper change, got ops=%q", gotOps)
		}
		now, _ := m.get(v.ID)
		if !now.SSHPolicyReady || now.sshPauseFenced || m.isSSHPausedInNft(now) {
			t.Fatalf("VM not clean after fallback install: %+v", now)
		}
	})

	t.Run("unready_vm_never_uses_pause_fence_and_mid_pause_syncNet_preserves_fence", func(t *testing.T) {
		m, h := newTestManager(t, nil)
		v, err := m.Create(context.Background(), CreateReq{Team: "blue"})
		if err != nil {
			t.Fatal(err)
		}
		// Unready VM with reprovision_ssh:true must stay fully quarantined (never pause-fenced).
		if _, err := m.createSnapshotOpts(context.Background(), v.ID, "s-unready", true, true); err != nil {
			t.Fatal(err)
		}
		now, _ := m.get(v.ID)
		if now.SSHPolicyReady || now.sshPauseFenced || !m.isQuarantinedInNft(now) {
			t.Fatalf("unready VM must stay fully quarantined: %+v", now)
		}

		// Now install policy, pause-fence, trigger concurrent syncNet, verify ruleset preserves @ssh_pause_v4, then rotate-token clears it.
		sock := filepath.Join(m.root(v.ID), vsockUDS)
		if err := os.Rename(sock, sock+".old"); err != nil {
			t.Fatal(err)
		}
		mkReq := func(gen int64) sshPolicyRequest {
			cfg := sshPolicyConfig{
				Version: 1, URL: "https://api.example/v1/vms/" + v.ID + "/ssh/authorize",
				Token: fmt.Sprintf("tok-%d", gen), VMID: v.ID, TeamID: v.Team,
				Generation: gen, BootEpoch: v.SSHBootEpoch, Users: []string{"dev"},
			}
			return sshPolicyRequest{Version: 1, BootEpoch: v.SSHBootEpoch, Config: cfg, HelperB64: b64("#!/usr/bin/env python3\n# rotate-token install-and-verify\n")}
		}
		var cur atomic.Value
		cur.Store(mkReq(0))
		sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
			if q["op"] != "exec" {
				return []map[string]any{{"ok": true}}
			}
			return sshAttest(cur.Load().(sshPolicyRequest))
		})
		if _, err := m.installSSHPolicy(context.Background(), v.ID, mkReq(0)); err != nil {
			t.Fatal(err)
		}
		if _, err := m.createSnapshotOpts(context.Background(), v.ID, "s-ready", true, true); err != nil {
			t.Fatal(err)
		}
		if err := m.syncNet(); err != nil {
			t.Fatal(err)
		}
		h.mu.Lock()
		loaded := h.nft
		h.mu.Unlock()
		if !strings.Contains(loaded, "set ssh_pause_v4 { type ipv4_addr; elements = { "+v.IP+" }; }") {
			t.Fatalf("mid-pause syncNet dropped @ssh_pause_v4 element:\n%s", loaded)
		}
		req1 := mkReq(1)
		cur.Store(req1)
		if _, err := m.installSSHPolicy(context.Background(), v.ID, req1); err != nil {
			t.Fatal(err)
		}
		now, _ = m.get(v.ID)
		if !now.SSHPolicyReady || now.sshPauseFenced || m.isSSHPausedInNft(now) {
			t.Fatalf("pause-fence not cleared after mid-pause syncNet + rotate-token: %+v", now)
		}
	})
}

func TestSyncNetIdempotentSkipsUnchangedRuleset(t *testing.T) {
	m, _ := newTestManager(t, nil)
	v, err := m.Create(context.Background(), CreateReq{Team: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	var nftCalls atomic.Int32
	var failNext atomic.Bool
	origRun := m.run
	m.run = func(stdin, name string, args ...string) error {
		if name == "nft" {
			nftCalls.Add(1)
			if failNext.CompareAndSwap(true, false) {
				return errors.New("transient nft error")
			}
		}
		return origRun(stdin, name, args...)
	}

	// Unchanged VM/config state: duplicate syncNet() must be a zero-nft no-op.
	if err := m.syncNet(); err != nil {
		t.Fatal(err)
	}
	if err := m.syncNet(); err != nil {
		t.Fatal(err)
	}
	if got := nftCalls.Load(); got != 0 {
		t.Fatalf("idempotent syncNet spawned %d nft calls, want 0", got)
	}

	// Real ruleset change spawns exactly 1 nft reload, then subsequent syncNet is 0 again.
	m.cfg.CtPerVM = 32
	if err := m.syncNet(); err != nil {
		t.Fatal(err)
	}
	if got := nftCalls.Load(); got != 1 {
		t.Fatalf("changed ruleset spawned %d nft calls, want 1", got)
	}
	if err := m.syncNet(); err != nil {
		t.Fatal(err)
	}
	if got := nftCalls.Load(); got != 1 {
		t.Fatalf("repeat syncNet after change spawned %d nft calls, want 1", got)
	}

	// Failed syncNet invalidates lastNftRuleset so the next syncNet retries nft -f -.
	// Make v stopped first so stopUnfencedGuests doesn't mutate running VMs on intentional failure.
	if _, err := m.Stop(context.Background(), v.ID, "api"); err != nil {
		t.Fatal(err)
	}
	nftCalls.Store(0)
	m.cfg.CtPerVM = 64
	failNext.Store(true)
	if err := m.syncNet(); err == nil {
		t.Fatal("expected transient nft error")
	}
	if got := nftCalls.Load(); got != 1 {
		t.Fatalf("failed syncNet made %d nft calls, want 1", got)
	}
	if err := m.syncNet(); err != nil {
		t.Fatalf("retry syncNet failed: %v", err)
	}
	if got := nftCalls.Load(); got != 2 {
		t.Fatalf("retry after failed syncNet made %d total nft calls, want 2", got)
	}
}

func TestSSHPolicyFastNoOpAndLiveTokenRotate(t *testing.T) {
	m, _ := newTestManager(t, nil)
	srv := m.routes()
	vms, err := m.CreateBatch(context.Background(), CreateReq{Team: "blue", Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	v1, v2 := vms[0], vms[1]

	helperSrc := "#!/usr/bin/env python3\n# boat-ssh-authorize with rotate-token support\n"
	mkReq := func(v VM, token string, gen int64, helper string) sshPolicyRequest {
		cfg := sshPolicyConfig{
			Version: 1, URL: "https://api.example/v1/vms/" + v.ID + "/ssh/authorize",
			Token: token, VMID: v.ID, TeamID: v.Team,
			Generation: gen, BootEpoch: v.SSHBootEpoch, Users: []string{"dev"},
		}
		return sshPolicyRequest{Version: 1, BootEpoch: v.SSHBootEpoch, Config: cfg, HelperB64: b64(helper)}
	}

	type peerTracker struct {
		mu     sync.Mutex
		ops    []string
		health *atomic.Int32
		expect atomic.Value
	}
	attach := func(v VM, initReq sshPolicyRequest) *peerTracker {
		sock := filepath.Join(m.root(v.ID), vsockUDS)
		if err := os.Rename(sock, sock+".old"); err != nil {
			t.Fatal(err)
		}
		pt := &peerTracker{}
		pt.expect.Store(initReq)
		pt.health = sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
			if q["op"] == "put_file" {
				pt.mu.Lock()
				pt.ops = append(pt.ops, "put")
				pt.mu.Unlock()
				return []map[string]any{{"ok": true}}
			}
			argv := q["argv"].([]any)
			mode := fmt.Sprint(argv[3])
			if mode == "rotate-token" {
				// Live token rotation must NEVER quarantine a healthy VM mid-refresh.
				cur, gerr := m.get(v.ID)
				if gerr != nil || !cur.SSHPolicyReady || !m.isPublishedReadyInNft(cur) {
					t.Errorf("VM %s was quarantined during live rotate-token: %+v", v.ID, cur)
				}
			}
			pt.mu.Lock()
			pt.ops = append(pt.ops, mode)
			pt.mu.Unlock()
			return sshAttest(pt.expect.Load().(sshPolicyRequest))
		})
		return pt
	}
	opsOf := func(pt *peerTracker) string {
		pt.mu.Lock()
		defer pt.mu.Unlock()
		return strings.Join(pt.ops, ",")
	}
	resetPeer := func(pt *peerTracker) {
		pt.mu.Lock()
		pt.ops = nil
		pt.mu.Unlock()
		pt.health.Store(0)
	}

	r1 := mkReq(v1, "tok-0", 0, helperSrc)
	r2 := mkReq(v2, "tok-0", 0, helperSrc)
	pt1 := attach(v1, r1)
	pt2 := attach(v2, r2)

	var nftCalls atomic.Int32
	origRun := m.run
	m.run = func(stdin, name string, args ...string) error {
		if name == "nft" {
			nftCalls.Add(1)
		}
		return origRun(stdin, name, args...)
	}

	// 1. Initial install on !SSHPolicyReady preserves exact "put,install,verify" sequence.
	rep1, err := m.installSSHPolicy(context.Background(), v1.ID, r1)
	if err != nil || !rep1.Readiness {
		t.Fatalf("initial install v1: %+v %v", rep1, err)
	}
	if got := opsOf(pt1); got != "put,install,verify" {
		t.Fatalf("initial v1 ops = %q, want put,install,verify", got)
	}
	if got := pt1.health.Load(); got != 1 {
		t.Fatalf("initial v1 health = %d, want 1", got)
	}
	rep2, err := m.installSSHPolicy(context.Background(), v2.ID, r2)
	if err != nil || !rep2.Readiness {
		t.Fatalf("initial install v2: %+v %v", rep2, err)
	}

	// 2. Exact-match O(1) no-op on both single installSSHPolicy and installSSHPolicyBatch.
	resetPeer(pt1)
	resetPeer(pt2)
	nftCalls.Store(0)
	noopRep, err := m.installSSHPolicy(context.Background(), v1.ID, r1)
	if err != nil || !noopRep.Readiness || noopRep.ConfigDigest != rep1.ConfigDigest {
		t.Fatalf("exact-match no-op single: %+v %v", noopRep, err)
	}
	batchBody, _ := json.Marshal(sshPolicyBatchRequest{Items: []sshPolicyBatchItem{
		{ID: v1.ID, sshPolicyRequest: r1},
		{ID: v2.ID, sshPolicyRequest: r2},
	}})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/vms/ssh-policy-batch", bytes.NewReader(batchBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("batch no-op status=%d body=%s", rec.Code, rec.Body.String())
	}
	if nftCalls.Load() != 0 || pt1.health.Load() != 0 || pt2.health.Load() != 0 || opsOf(pt1) != "" || opsOf(pt2) != "" {
		t.Fatalf("exact-match no-op was not O(1): nft=%d h1=%d h2=%d ops1=%q ops2=%q",
			nftCalls.Load(), pt1.health.Load(), pt2.health.Load(), opsOf(pt1), opsOf(pt2))
	}

	// 3. Live non-disruptive token rotation on single installSSHPolicy.
	r1Rot := mkReq(v1, "tok-1", 1, helperSrc)
	pt1.expect.Store(r1Rot)
	resetPeer(pt1)
	nftCalls.Store(0)
	rotRep, err := m.installSSHPolicy(context.Background(), v1.ID, r1Rot)
	if err != nil || !rotRep.Readiness || rotRep.ConfigDigest == rep1.ConfigDigest {
		t.Fatalf("single rotate-token failed: %+v %v", rotRep, err)
	}
	if got := opsOf(pt1); got != "rotate-token" {
		t.Fatalf("single rotate-token ops = %q, want rotate-token", got)
	}
	if nftCalls.Load() != 0 || pt1.health.Load() != 1 {
		t.Fatalf("single rotate-token nft=%d health=%d, want 0 and 1", nftCalls.Load(), pt1.health.Load())
	}
	rawVM, _ := os.ReadFile(filepath.Join(m.vmDir(v1.ID), "vm.json"))
	if strings.Contains(string(rawVM), "tok-1") {
		t.Fatal("rotated lookup token persisted to vm.json")
	}

	// 4. Live non-disruptive token rotation on installSSHPolicyBatch across both VMs.
	r1Rot2 := mkReq(v1, "tok-2", 2, helperSrc)
	r2Rot2 := mkReq(v2, "tok-2", 2, helperSrc)
	pt1.expect.Store(r1Rot2)
	pt2.expect.Store(r2Rot2)
	resetPeer(pt1)
	resetPeer(pt2)
	nftCalls.Store(0)
	batchBody, _ = json.Marshal(sshPolicyBatchRequest{Items: []sshPolicyBatchItem{
		{ID: v1.ID, sshPolicyRequest: r1Rot2},
		{ID: v2.ID, sshPolicyRequest: r2Rot2},
	}})
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/vms/ssh-policy-batch", bytes.NewReader(batchBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("batch rotate-token status=%d body=%s", rec.Code, rec.Body.String())
	}
	var batchRep sshPolicyBatchReply
	if err := json.Unmarshal(rec.Body.Bytes(), &batchRep); err != nil {
		t.Fatal(err)
	}
	if len(batchRep.Results) != 2 || len(batchRep.Errors) != 0 {
		t.Fatalf("unexpected batch rotate-token reply: %+v", batchRep)
	}
	if nftCalls.Load() != 0 || opsOf(pt1) != "rotate-token" || opsOf(pt2) != "rotate-token" {
		t.Fatalf("batch rotate-token nft=%d ops1=%q ops2=%q, want 0 and rotate-token", nftCalls.Load(), opsOf(pt1), opsOf(pt2))
	}

	// 5. Helper binary change forces full fenced "put,install,verify" reinstall.
	r1NewHelper := mkReq(v1, "tok-3", 3, helperSrc+"# v2\n")
	pt1.expect.Store(r1NewHelper)
	resetPeer(pt1)
	nftCalls.Store(0)
	if _, err := m.installSSHPolicy(context.Background(), v1.ID, r1NewHelper); err != nil {
		t.Fatalf("helper upgrade reinstall failed: %v", err)
	}
	if got := opsOf(pt1); got != "put,install,verify" {
		t.Fatalf("helper upgrade ops = %q, want put,install,verify", got)
	}
	if nftCalls.Load() != 2 {
		t.Fatalf("helper upgrade nft reloads = %d, want 2 (pre-fence + post-publish)", nftCalls.Load())
	}
}

func TestSSHPolicyRotateTokenFallbackAndFailClosed(t *testing.T) {
	helperWithRotate := "#!/usr/bin/env python3\n# boat-ssh-authorize rotate-token\n"
	helperLegacy := "#!/usr/bin/env python3\n# boat-ssh-authorize legacy\n"

	t.Run("unsupported_exit2_falls_back_to_full_install", func(t *testing.T) {
		m, v, r := sshPolicyFixture(t)
		r.HelperB64 = b64(helperWithRotate)
		var expect atomic.Value
		expect.Store(r)
		var ops []string
		sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
			if q["op"] == "put_file" {
				ops = append(ops, "put")
				return []map[string]any{{"ok": true}}
			}
			mode := fmt.Sprint(q["argv"].([]any)[3])
			ops = append(ops, mode)
			if mode == "rotate-token" {
				return []map[string]any{
					{"stream": "stderr", "data_b64": b64("usage: unsupported command rotate-token\n")},
					{"exit": 2},
				}
			}
			return sshAttest(expect.Load().(sshPolicyRequest))
		})
		if _, err := m.installSSHPolicy(context.Background(), v.ID, r); err != nil {
			t.Fatal(err)
		}
		ops = nil
		r2 := r
		r2.Config.Token = "rotated-tok"
		r2.Config.Generation = 1
		expect.Store(r2)
		rep, err := m.installSSHPolicy(context.Background(), v.ID, r2)
		if err != nil || !rep.Readiness {
			t.Fatalf("fallback install failed: %+v %v", rep, err)
		}
		if got := strings.Join(ops, ","); got != "rotate-token,put,install,verify" {
			t.Fatalf("fallback ops = %q, want rotate-token,put,install,verify", got)
		}
		now, _ := m.get(v.ID)
		if !now.SSHPolicyReady || now.SSHPolicyDigest != rep.ConfigDigest {
			t.Fatalf("VM not ready after fallback: %+v", now)
		}
	})

	t.Run("legacy_script_without_rotate_token_skips_to_full_install", func(t *testing.T) {
		m, v, r := sshPolicyFixture(t)
		r.HelperB64 = b64(helperLegacy)
		var expect atomic.Value
		expect.Store(r)
		var ops []string
		sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
			if q["op"] == "put_file" {
				ops = append(ops, "put")
				return []map[string]any{{"ok": true}}
			}
			mode := fmt.Sprint(q["argv"].([]any)[3])
			ops = append(ops, mode)
			return sshAttest(expect.Load().(sshPolicyRequest))
		})
		if _, err := m.installSSHPolicy(context.Background(), v.ID, r); err != nil {
			t.Fatal(err)
		}
		ops = nil
		r2 := r
		r2.Config.Token = "rotated-tok"
		r2.Config.Generation = 1
		expect.Store(r2)
		if _, err := m.installSSHPolicy(context.Background(), v.ID, r2); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(ops, ","); got != "put,install,verify" {
			t.Fatalf("legacy helper ops = %q, want put,install,verify", got)
		}
	})

	t.Run("tamper_error_fails_closed_and_quarantines_single_and_batch", func(t *testing.T) {
		m, _ := newTestManager(t, nil)
		vms, err := m.CreateBatch(context.Background(), CreateReq{Team: "blue", Count: 2})
		if err != nil {
			t.Fatal(err)
		}
		mk := func(v VM, tok string, gen int64) sshPolicyRequest {
			cfg := sshPolicyConfig{
				Version: 1, URL: "https://api.example/v1/vms/" + v.ID + "/ssh/authorize",
				Token: tok, VMID: v.ID, TeamID: v.Team,
				Generation: gen, BootEpoch: v.SSHBootEpoch, Users: []string{"dev"},
			}
			return sshPolicyRequest{Version: 1, BootEpoch: v.SSHBootEpoch, Config: cfg, HelperB64: b64(helperWithRotate)}
		}
		var tamper atomic.Bool
		for _, v := range vms {
			sock := filepath.Join(m.root(v.ID), vsockUDS)
			if err := os.Rename(sock, sock+".old"); err != nil {
				t.Fatal(err)
			}
			v := v
			sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
				if q["op"] == "put_file" {
					return []map[string]any{{"ok": true}}
				}
				mode := fmt.Sprint(q["argv"].([]any)[3])
				if mode == "rotate-token" && tamper.Load() {
					return []map[string]any{
						{"stream": "stderr", "data_b64": b64("sshd_config drop-in drift detected\n")},
						{"exit": 1},
					}
				}
				return sshAttest(mk(v, "tok-0", 0))
			})
			if _, err := m.installSSHPolicy(context.Background(), v.ID, mk(v, "tok-0", 0)); err != nil {
				t.Fatal(err)
			}
		}

		tamper.Store(true)
		// Single VM fail-closed quarantine:
		if _, err := m.installSSHPolicy(context.Background(), vms[0].ID, mk(vms[0], "tok-1", 1)); err == nil {
			t.Fatal("expected tamper error on rotate-token")
		}
		now0, _ := m.get(vms[0].ID)
		if now0.SSHPolicyReady || !m.isQuarantinedInNft(now0) {
			t.Fatalf("tampered single VM not quarantined: %+v", now0)
		}

		// Batch VM fail-closed quarantine:
		batchRep, err := m.installSSHPolicyBatch(context.Background(), sshPolicyBatchRequest{
			Items: []sshPolicyBatchItem{{ID: vms[1].ID, sshPolicyRequest: mk(vms[1], "tok-1", 1)}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if batchRep.Errors[vms[1].ID] == "" || len(batchRep.Results) != 0 {
			t.Fatalf("expected batch item error on tamper: %+v", batchRep)
		}
		now1, _ := m.get(vms[1].ID)
		if now1.SSHPolicyReady || !m.isQuarantinedInNft(now1) {
			t.Fatalf("tampered batch VM not quarantined: %+v", now1)
		}
		var rf routeFile
		if err := readJSON(filepath.Join(m.cfg.RunDir, "routes.json"), &rf); err != nil {
			t.Fatal(err)
		}
		if len(rf.VMs) != 0 || len(rf.Quarantined) != 2 {
			t.Fatalf("expected both tampered VMs quarantined in routes.json: %+v", rf)
		}
	})
}

func TestSetIdentityDefersSSHRestartOnlyWhenPolicyManaged(t *testing.T) {
	mManaged, hManaged := newTestManager(t, nil)
	vManaged, err := mManaged.Create(context.Background(), CreateReq{Team: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	idents := hManaged.fcOf(t, mManaged, vManaged.ID).idents
	if len(idents) != 1 || idents[0]["defer_ssh_restart"] != true {
		t.Fatalf("expected defer_ssh_restart=true in managed mode, got %+v", idents)
	}

	vWarm, err := mManaged.createFrom(context.Background(), CreateReq{Team: warmTeam}, "")
	if err != nil {
		t.Fatal(err)
	}
	warmIdents := hManaged.fcOf(t, mManaged, vWarm.ID).idents
	if len(warmIdents) != 1 || warmIdents[0]["defer_ssh_restart"] != nil {
		t.Fatalf("expected warm template builder to omit defer_ssh_restart, got %+v", warmIdents)
	}

	mStandalone, hStandalone := newTestManager(t, func(c *Config) { c.StandaloneSSH = true })
	vStandalone, err := mStandalone.Create(context.Background(), CreateReq{Team: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	stdIdents := hStandalone.fcOf(t, mStandalone, vStandalone.ID).idents
	if len(stdIdents) != 1 || stdIdents[0]["defer_ssh_restart"] != nil {
		t.Fatalf("expected standaloneSSH mode to omit defer_ssh_restart, got %+v", stdIdents)
	}
}

func TestSSHPolicyInstallAndVerifyFastPathAndAttestationState(t *testing.T) {
	realHelper, err := os.ReadFile(filepath.Join("..", "guest", "ssh-authorize.py"))
	if errors.Is(err, os.ErrNotExist) && os.Getenv("CI") == "" && os.Getenv("GITHUB_ACTIONS") == "" {
		t.Skip("not in the source tree (nix builds only this directory)")
	}
	if err != nil {
		t.Fatalf("read ../guest/ssh-authorize.py: %v", err)
	}
	wantHelperDigest := sha256Hex(realHelper)

	assertCommittedAttestation := func(t *testing.T, rep sshPolicyReply, v VM, wantDigest string) {
		t.Helper()
		if rep.Version != 1 || !rep.Readiness || rep.BootEpoch != v.SSHBootEpoch || rep.ConfigDigest != wantDigest {
			t.Fatalf("unexpected attestation proof fields: %+v", rep)
		}
		if rep.VMID != v.ID || rep.Team != v.Team || rep.State != Running || rep.PendingCopy ||
			!rep.SSHPolicyReady || rep.SSHPolicyDigest != wantDigest {
			t.Fatalf("missing/mismatched live committed VM state in attestation reply: %+v", rep)
		}
	}

	t.Run("baked_matching_helper_completes_in_1_exec_zero_put_file", func(t *testing.T) {
		m, v, r := sshPolicyFixture(t)
		r.HelperB64 = b64(string(realHelper))
		_, wantDigest, _ := canonicalSSHConfig(r.Config)

		var ops []string
		healthCalls := sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
			if q["op"] == "put_file" {
				ops = append(ops, "put")
				return []map[string]any{{"ok": true}}
			}
			argv := q["argv"].([]any)
			var tail []string
			for _, a := range argv[3:] {
				tail = append(tail, fmt.Sprint(a))
			}
			ops = append(ops, strings.Join(tail, " "))
			return sshAttest(r)
		})

		rep, err := m.installSSHPolicy(context.Background(), v.ID, r)
		if err != nil {
			t.Fatalf("installSSHPolicy fast path: %v", err)
		}
		assertCommittedAttestation(t, rep, v, wantDigest)
		if got := strings.Join(ops, ","); got != "install-and-verify --expect-helper-sha256 "+wantHelperDigest {
			t.Fatalf("baked fast path ops = %q, want single install-and-verify with --expect-helper-sha256", got)
		}
		if got := healthCalls.Load(); got != 1 {
			t.Fatalf("baked fast path healthCalls = %d, want 1", got)
		}

		// Exact-match O(1) no-op and live rotate-token also populate committed VM SSH fields.
		ops = nil
		healthCalls.Store(0)
		noopRep, err := m.installSSHPolicy(context.Background(), v.ID, r)
		if err != nil {
			t.Fatalf("fast no-op: %v", err)
		}
		assertCommittedAttestation(t, noopRep, v, wantDigest)
		if len(ops) != 0 || healthCalls.Load() != 0 {
			t.Fatalf("expected O(1) no-op, got ops=%v health=%d", ops, healthCalls.Load())
		}
	})

	t.Run("missing_or_mismatched_baked_helper_falls_back_to_put_plus_install_and_verify", func(t *testing.T) {
		m, v, r := sshPolicyFixture(t)
		r.HelperB64 = b64(string(realHelper))
		_, wantDigest, _ := canonicalSSHConfig(r.Config)

		var ops []string
		var uploaded bool
		healthCalls := sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
			if q["op"] == "put_file" {
				if q["owner"] != "root" || q["mode"] != "0555" || q["path"] != sshPolicyHelper {
					t.Errorf("unexpected put_file args: %+v", q)
				}
				uploaded = true
				ops = append(ops, "put")
				return []map[string]any{{"ok": true}}
			}
			argv := q["argv"].([]any)
			var tail []string
			for _, a := range argv[3:] {
				tail = append(tail, fmt.Sprint(a))
			}
			cmd := strings.Join(tail, " ")
			ops = append(ops, cmd)
			if strings.Contains(cmd, "--expect-helper-sha256") && !uploaded {
				// Simulate missing or mismatched baked helper in guest image.
				return []map[string]any{{"exit": 1}}
			}
			return sshAttest(r)
		})

		rep, err := m.installSSHPolicy(context.Background(), v.ID, r)
		if err != nil {
			t.Fatalf("fallback installSSHPolicy failed: %v", err)
		}
		assertCommittedAttestation(t, rep, v, wantDigest)
		wantOps := "install-and-verify --expect-helper-sha256 " + wantHelperDigest + ",put,install-and-verify"
		if got := strings.Join(ops, ","); got != wantOps {
			t.Fatalf("fallback ops = %q, want %q", got, wantOps)
		}
		if got := healthCalls.Load(); got != 1 {
			t.Fatalf("fallback healthCalls = %d, want 1", got)
		}
	})

	t.Run("fallback_fails_closed_on_bad_proof_exit_or_epoch_drift", func(t *testing.T) {
		for _, failMode := range []string{"bad_proof", "bad_exit", "epoch_drift_on_baked", "epoch_drift_on_fallback"} {
			t.Run(failMode, func(t *testing.T) {
				m, v, r := sshPolicyFixture(t)
				r.HelperB64 = b64(string(realHelper))
				var ops []string
				sshPolicyPeer(t, m, v, func(q map[string]any) []map[string]any {
					if q["op"] == "put_file" {
						ops = append(ops, "put")
						return []map[string]any{{"ok": true}}
					}
					argv := q["argv"].([]any)
					hasExpect := len(argv) > 4
					if hasExpect {
						ops = append(ops, "baked")
						if failMode == "epoch_drift_on_baked" {
							_, _ = m.resetSSHBoot(v.ID)
							_ = m.syncNet()
						}
						return []map[string]any{{"exit": 1}}
					}
					ops = append(ops, "fallback")
					switch failMode {
					case "bad_proof":
						return []map[string]any{{"stream": "stdout", "data_b64": b64(`{"version":1,"config_digest":"wrong","boot_epoch":"` + r.BootEpoch + `","readiness":true}`)}, {"exit": 0}}
					case "bad_exit":
						return []map[string]any{{"exit": 1}}
					case "epoch_drift_on_fallback":
						_, _ = m.resetSSHBoot(v.ID)
						_ = m.syncNet()
						return sshAttest(r)
					}
					return sshAttest(r)
				})

				if _, err := m.installSSHPolicy(context.Background(), v.ID, r); err == nil {
					t.Fatalf("%s: expected error, got nil", failMode)
				}
				now, _ := m.get(v.ID)
				if now.SSHPolicyReady || !m.isQuarantinedInNft(now) {
					t.Fatalf("%s: failed install released VM from quarantine: %+v", failMode, now)
				}
				if failMode == "epoch_drift_on_baked" && strings.Join(ops, ",") != "baked" {
					t.Fatalf("epoch drift during baked probe must abort before put_file, got ops=%v", ops)
				}
			})
		}
	})
}
