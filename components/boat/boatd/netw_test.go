package main

import (
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRuleset(t *testing.T) {
	c := defaultConfig()
	c.IPv6Prefix = "2001:db8:1::/64"
	c.PublicIPv4 = []string{"203.0.113.7"}
	c.Teams = map[string]TeamPolicy{"blue": {AllowIntraTeam: true}}
	vms := []VM{
		{State: Running, SSHBootEpoch: "fixture", SSHPolicyReady: true, Index: 2, Team: "blue", Tap: "bt2", IP: c.ipFor(2), MAC: macFor(2), IPv6: c.ipv6For(2), PublicIP: "203.0.113.7",
			Ports: []Port{{GuestPort: 8080, HostPort: 20000}, {GuestPort: 3000}}},
		{State: Running, SSHBootEpoch: "fixture", SSHPolicyReady: true, Index: 3, Team: "blue", Tap: "bt3", IP: c.ipFor(3), MAC: macFor(3)},
		{State: Running, SSHBootEpoch: "fixture", SSHPolicyReady: true, Index: 300, Team: "red", Tap: "bt300", IP: c.ipFor(300), MAC: macFor(300)},
	}
	rs := ruleset(c, vms)
	for _, want := range []string{
		"table inet boat\ndelete table inet boat", // atomic replace
		`iifname "boatbr0" ip daddr 10.200.0.1 meta l4proto { udp, tcp } th dport 53 accept`,
		`iifname "boatbr0" drop`,
		"fib daddr type local tcp dport 20000 dnat ip to 10.200.0.2:8080",
		"ip daddr 203.0.113.7 tcp dport 8080 dnat ip to 10.200.0.2:8080",
		`ip saddr 10.200.0.2 oifname != "boatbr0" snat ip to 203.0.113.7`,
		`ip saddr 10.200.0.0/16 oifname != "boatbr0" masquerade`,
		`set team0 { type ifname; elements = { "bt2", "bt3" }; }`,
		"iifname @team0 oifname @team0 accept",
		`iifname "bt*" oifname "bt*" drop`,
		`iifname "bt*" icmpv6 type { nd-router-advert, nd-redirect } counter drop`,
		`iifname "boatbr0" icmpv6 type { nd-router-advert, nd-redirect } drop`,
		`iifname "boatbr0" icmpv6 type { echo-request, destination-unreachable, packet-too-big, time-exceeded, parameter-problem, nd-router-solicit, nd-neighbor-solicit, nd-neighbor-advert } accept`,
		`iifname "bt*" iifname != { "bt2", "bt3", "bt300" } drop`,
		`add rule inet boat_ct forward iifname "boatbr0" ct state new add @v4_4096 { ip saddr ct count over 4096 } drop`,
		`add rule inet boat_ct forward iifname "boatbr0" ct state new add @v6_4096 { ip6 saddr ct count over 4096 } drop`,
		`add rule inet boat_ct input iifname "boatbr0" ct state new add @v4_4096 { ip saddr ct count over 4096 } drop`,
		`iifname "bt300" ether type arp arp saddr ether != 06:00:0a:c8:01:2c counter drop`,
		`iifname "bt2" icmpv6 type nd-neighbor-advert icmpv6 taddr != { 2001:db8:1::2, fe80::400:aff:fec8:2 } counter drop`,
		`iifname "bt3" icmpv6 type nd-neighbor-advert icmpv6 taddr != { fe80::400:aff:fec8:3 } counter drop`,
		`iifname "boatbr0" ip daddr { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12,`,
		`iifname "boatbr0" ip6 daddr { ::/128, ::1/128, ::ffff:0:0/96, 64:ff9b::/96, 64:ff9b:1::/48, 100::/64, 2001::/32, 2002::/16, fc00::/7, fe80::/10, fec0::/10, ff00::/8 } drop`,
		`iifname "boatbr0" tcp dport { 25, 465, 587 } drop`,
		`oifname "boatbr0" ip6 daddr 2001:db8:1::2 tcp dport 8080 accept`,
		"ct state established,related accept",
		`iifname "bt300" ether type ip ip saddr != 10.200.1.44 drop`,
		`iifname "bt300" ether saddr != 06:00:0a:c8:01:2c drop`,
		`iifname "bt2" ether type ip6 ip6 saddr != { 2001:db8:1::2, fe80::400:aff:fec8:2, :: } drop`,
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("ruleset missing %q\n%s", want, rs)
		}
	}
	if strings.Contains(rs, "dport 3000 accept") || strings.Contains(rs, "dport 3000 dnat") || strings.Contains(rs, "dnat ip to 10.200.0.2\n") || strings.Contains(rs, "ip6 daddr 2001:db8:1::/64 accept") || strings.Contains(rs, "skuid 900000") {
		t.Errorf("unpublished port open/DNATed, whole PublicIP/v6 prefix open, or jail-uid rule without a jailer\n%s", rs)
	}
	if !strings.Contains(rs, "meta skuid 65534 drop") {
		t.Errorf("nobody (the import decompressor) may send from the host\n%s", rs)
	}
	if strings.Contains(rs, "dport 8744") {
		t.Error("AI proxy port open by default")
	}
	c.AIProxyPort = 8744
	if !strings.Contains(ruleset(c, vms), `iifname "boatbr0" ip daddr 10.200.0.1 tcp dport 8744 accept`) {
		t.Error("AI proxy port not opened")
	}
	if strings.Contains(rs, "dport 443 accept") {
		t.Error("host https port open by default")
	}
	c.HostHTTPSPort = 443
	rs = ruleset(c, vms)
	dropV4 := `iifname "boatbr0" ip daddr { ` + strings.Join(privateV4, ", ") + ` } tcp dport 443 drop`
	dropV6 := `iifname "boatbr0" ip6 daddr { ` + strings.Join(privateV6, ", ") + ` } tcp dport 443 drop`
	open := `iifname "boatbr0" fib daddr type local tcp dport 443 accept`
	if d4, d6, i, d := strings.Index(rs, dropV4), strings.Index(rs, dropV6), strings.Index(rs, open), strings.Index(rs, `    iifname "boatbr0" drop`); d4 < 0 || d6 <= d4 || i <= d6 || d <= i {
		t.Errorf("host https port privateV4/privateV6 drops or fib-local accept out of order\n%s", rs)
	}
	c.HostHTTPSAddrs = []string{"203.0.113.10", "198.51.100.0/24"}
	inNoGw := inetChain(t, ruleset(c, vms), "input")
	if !strings.Contains(inNoGw, `iifname "boatbr0" ip daddr { 203.0.113.10, 198.51.100.0/24 } tcp dport 443 accept`) || strings.Contains(inNoGw, "{ 10.200.0.1") || strings.Contains(inNoGw, "fib daddr type local") {
		t.Errorf("hostHttpsAddrs without gateway injected 10.200.0.1 or kept fib-local:\n%s", inNoGw)
	}
	c.HostHTTPSAddrs = []string{"203.0.113.10", "198.51.100.0/24", "10.200.0.1"}
	rsAllow := ruleset(c, vms)
	inAllow := inetChain(t, rsAllow, "input")
	wantAllow := `iifname "boatbr0" ip daddr { 203.0.113.10, 198.51.100.0/24, 10.200.0.1 } tcp dport 443 accept`
	if !strings.Contains(inAllow, wantAllow) || strings.Contains(inAllow, "fib daddr type local") {
		t.Errorf("hostHttpsAddrs allowlist not enforced in input:\n%s", inAllow)
	}
	c.HostHTTPSAddrs = nil
	c.HostHTTPSPort = 0
	c.Jailer, c.AllowSMTP, c.EgressMbit = "/bin/jailer", true, 100
	rs = ruleset(c, vms)
	if !strings.Contains(rs, "meta skuid 900000-965534 drop") || strings.Contains(rs, "dport { 25") {
		t.Errorf("jailer/smtp options\n%s", rs)
	}
	// The policer must sit in the bridge table: on the L3 hooks iifname is boatbr0.
	_, bridge, _ := strings.Cut(rs, "table bridge boat {")
	if !strings.Contains(bridge, `iifname "bt2" limit rate over 12207 kbytes/second drop`) {
		t.Errorf("egress cap not in the bridge table\n%s", bridge)
	}
	if strings.Contains(ruleset(c, nil), `iifname != {`) || !strings.Contains(ruleset(c, nil), "    iifname \"bt*\" drop\n") {
		t.Errorf("no VMs: every bt* port must drop\n%s", ruleset(c, nil))
	}
	if strings.Contains(rs, "dport 0") || strings.Contains(rs, "bt300\", ") {
		t.Errorf("port without host_port forwarded, or red team in blue set\n%s", rs)
	}
	if strings.Count(rs, "{") != strings.Count(rs, "}") {
		t.Error("unbalanced braces")
	}
	if strings.Contains(rs, "delete table inet boat_ct") || strings.Contains(rs, "ether type arp accept") {
		t.Errorf("connection counts must survive a reload, and ARP must not cross teams\n%s", rs)
	}
	c.CtPerVM = 0
	if strings.Contains(ruleset(c, vms), "ct count") {
		t.Error("ctPerVM 0 still limits")
	}
}

func TestAddressing(t *testing.T) {
	c := defaultConfig()
	c.IPv6Prefix = "2001:db8:1::/64"
	if c.ipFor(258) != "10.200.1.2" || c.gateway().String() != "10.200.0.1" || macFor(258) != "06:00:0a:c8:01:02" ||
		c.ipv6For(258) != "2001:db8:1::102" || c.ipv6Gateway() != "2001:db8:1::1" || tapFor(65534) != "bt65534" {
		t.Fatal(c.ipFor(258), macFor(258), c.ipv6For(258))
	}
}

func TestFirewallFailureFailsClosed(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := t.Context()
	a, err := m.Create(ctx, CreateReq{})
	if err != nil {
		t.Fatal(err)
	}
	m.Stop(ctx, a.ID, "")
	h.mu.Lock()
	h.nftErr = errors.New("nft: syntax error")
	h.mu.Unlock()
	if _, err := m.Create(ctx, CreateReq{Name: "b"}); err == nil {
		t.Fatal("VM created without its firewall rules")
	}
	if _, err := m.Start(ctx, a.ID, nil); err == nil {
		t.Fatal("VM started without its firewall rules")
	}
	for _, v := range m.list("") {
		if v.State == Running {
			t.Fatalf("%s running unfiltered", v.Name)
		}
		if h.Alive(v.PID, v.ID) {
			t.Fatalf("%s process left running", v.Name)
		}
	}
}

func TestL2Pinned(t *testing.T) {
	m, h := newTestManager(t, func(c *Config) { c.IPv6Prefix = "fd42::/64" })
	v, err := m.Create(t.Context(), CreateReq{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip neigh replace " + v.IP + " lladdr " + v.MAC + " dev boatbr0 nud permanent",
		"ip -6 neigh replace " + v.IPv6 + " lladdr " + v.MAC + " dev boatbr0 nud permanent",
		"bridge fdb replace " + v.MAC + " dev " + v.Tap + " master static",
		"bridge link set dev " + v.Tap + " learning off flood off",
	} {
		if !h.ran(want) {
			t.Errorf("not run: %s", want)
		}
	}
	m.Delete(t.Context(), v.ID)
	if !h.ran("ip neigh del "+v.IP) || !h.ran("ip -6 neigh del "+v.IPv6) {
		t.Error("neighbour entries left behind")
	}
}

func TestCreateSyncsNet(t *testing.T) {
	m, h := newTestManager(t, nil)
	v, err := m.Create(t.Context(), CreateReq{})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	nft := h.nft
	h.mu.Unlock()
	if !strings.Contains(nft, `iifname "`+v.Tap+`" ether saddr != `+v.MAC+` drop`) {
		t.Fatalf("new vm not in ruleset:\n%s", nft)
	}
}

// IPv6 forms that reach private/reserved addresses or tunnels (loopback, unspecified,
// IPv4-mapped, NAT64, discard, Teredo, 6to4, ULA, link-local, site-local, multicast)
// must all be blocked, while documentation/test guest prefix 2001:db8::/32 stays allowed.
func TestEgressBlocksV4InV6(t *testing.T) {
	for _, a := range []string{
		"::", "::1", "::ffff:169.254.169.254", "::ffff:10.0.0.1",
		"64:ff9b::a9fe:a9fe", "64:ff9b::a00:1", "64:ff9b:1::a00:1",
		"100::1", "2001:0:4136:e378::1", "2002:a9fe:a9fe::1", "2002:a00:1::",
		"fc00::1", "fd00:ec2::254", "fe80::1", "fec0::1", "ff02::1",
	} {
		addr := netip.MustParseAddr(a)
		blocked := false
		for _, p := range privateV6 {
			pf, err := netip.ParsePrefix(p)
			if err != nil {
				t.Fatalf("invalid prefix %q in privateV6: %v", p, err)
			}
			if pf.Contains(addr) {
				blocked = true
			}
		}
		if !blocked {
			t.Errorf("%s not in privateV6", a)
		}
	}
	guestDoc := netip.MustParseAddr("2001:db8:1::2")
	for _, p := range privateV6 {
		if netip.MustParsePrefix(p).Contains(guestDoc) {
			t.Fatalf("privateV6 must not block guest test prefix 2001:db8::/32 (matched %s)", p)
		}
	}
}

func TestSSHControlEndpointsValidation(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "boatd.json")
	rawValid, _ := json.Marshal(map[string]any{
		"hostHttpsPort":       443,
		"sshControlEndpoints": []string{"198.51.100.10:8443", "203.0.113.10"},
	})
	if err := os.WriteFile(cfgFile, rawValid, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfig(cfgFile)
	if err != nil || len(loaded.SSHControlEndpoints) != 2 {
		t.Fatalf("valid sshControlEndpoints rejected: %+v %v", loaded.SSHControlEndpoints, err)
	}

	invalid := []string{
		"",
		" 203.0.113.10",
		"203.0.113.10 ",
		"203.0.113.10:",
		":443",
		"203.0.113.10:abc",
		"203.0.113.10:0",
		"203.0.113.10:-1",
		"203.0.113.10:+443",
		"203.0.113.10:0443",
		"203.0.113.10:65536",
		"203.0.113.10/32",
		"https://203.0.113.10:443",
		"::1",
		"2001:db8::1",
		"[2001:db8::1]:443",
		"::ffff:203.0.113.10",
		"[::ffff:203.0.113.10]:443",
		"api.example.com",
		"api.example.com:443",
		"localhost",
		"localhost:443",
		"0.0.0.0",
		"0.0.0.1:443",
		"127.0.0.1",
		"127.0.0.53:443",
		"10.0.0.1",
		"10.0.0.1:443",
		"10.200.0.1:443",
		"100.64.0.1",
		"100.100.100.200:443",
		"169.254.169.254",
		"169.254.169.254:443",
		"169.254.0.1",
		"172.16.0.1",
		"172.31.255.254:443",
		"192.0.0.1:443",
		"192.168.0.1",
		"192.168.1.1:8443",
		"198.18.0.1:443",
		"224.0.0.1",
		"239.255.255.250:443",
		"240.0.0.1:443",
		"255.255.255.255",
		"255.255.255.255:443",
	}
	for _, bad := range invalid {
		b, _ := json.Marshal(map[string]any{"sshControlEndpoints": []string{bad}})
		if err := os.WriteFile(cfgFile, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(cfgFile); err == nil {
			t.Errorf("invalid sshControlEndpoints %q accepted", bad)
		}
	}
}

func TestQuarantinePrivateEgressAndControlHTTPS(t *testing.T) {
	c := defaultConfig()
	c.IPv6Prefix = "2001:db8:1::/64"
	vms := []VM{
		{State: Running, SSHBootEpoch: "unverified", SSHPolicyReady: false, Index: 2, Team: "blue", Tap: "bt2", IP: c.ipFor(2), MAC: macFor(2), IPv6: c.ipv6For(2)},
		{State: Running, SSHBootEpoch: "verified", SSHPolicyReady: true, Index: 3, Team: "blue", Tap: "bt3", IP: c.ipFor(3), MAC: macFor(3), IPv6: c.ipv6For(3)},
	}

	// HostHTTPSPort=0 opens no local TCP 443 in input.
	c.HostHTTPSPort = 0
	rs0 := ruleset(c, vms)
	if in0 := inetChain(t, rs0, "input"); strings.Contains(in0, "443") || strings.Contains(in0, "tcp dport") {
		t.Fatalf("HostHTTPSPort=0 opened local TCP in input:\n%s", in0)
	}

	// Default HostHTTPSPort=443 with empty HostHTTPSAddrs: quarantined VM is dropped
	// unconditionally in input/forward/output/bridge, while ready VMs have all privateV4
	// and privateV6 ranges dropped on :443 before fib daddr type local.
	c.HostHTTPSPort = 443
	rs443 := ruleset(c, vms)
	in443 := inetChain(t, rs443, "input")
	fwd443 := inetChain(t, rs443, "forward")
	out443 := inetChain(t, rs443, "output")
	br443 := bridgeOutputChain(t, rs443)

	qDropIdx := strings.Index(in443, "ip saddr 10.200.0.2 drop")
	qDropV6Idx := strings.Index(in443, "ip6 saddr 2001:db8:1::2 drop")
	privV4DropIdx := strings.Index(in443, `iifname "boatbr0" ip daddr { `+strings.Join(privateV4, ", ")+` } tcp dport 443 drop`)
	privV6DropIdx := strings.Index(in443, `iifname "boatbr0" ip6 daddr { `+strings.Join(privateV6, ", ")+` } tcp dport 443 drop`)
	fibIdx := strings.Index(in443, `iifname "boatbr0" fib daddr type local tcp dport 443 accept`)
	if !(qDropIdx >= 0 && qDropV6Idx > qDropIdx && privV4DropIdx > qDropV6Idx && privV6DropIdx > privV4DropIdx && fibIdx > privV6DropIdx) {
		t.Fatalf("input quarantine/private-range drops misordered:\n%s", in443)
	}
	if strings.Contains(in443[:qDropV6Idx], " accept\n") {
		t.Fatalf("quarantined VM received input HTTPS or DNS exception:\n%s", in443)
	}
	if strings.Count(in443, "10.200.0.2") != 1 || strings.Count(in443, "2001:db8:1::2") != 1 {
		t.Fatalf("quarantined VM has extra rules in input:\n%s", in443)
	}
	if strings.Contains(fwd443, "443") {
		t.Fatalf("HostHTTPSPort=443 leaked TCP 443 into forward:\n%s", fwd443)
	}
	estIdx := strings.Index(fwd443, "ct state established,related accept")
	for _, qRule := range []string{
		"ip saddr 10.200.0.2 drop",
		"ip daddr 10.200.0.2 drop",
		"ip6 saddr 2001:db8:1::2 drop",
		"ip6 daddr 2001:db8:1::2 drop",
	} {
		if idx := strings.Index(fwd443, qRule); idx < 0 || idx > estIdx {
			t.Fatalf("forward missing %q before established:\n%s", qRule, fwd443)
		}
	}
	if strings.Contains(fwd443[:estIdx], " accept\n") {
		t.Fatalf("quarantined VM has forward accept before established:\n%s", fwd443)
	}
	for _, outRule := range []string{"ip daddr 10.200.0.2 drop", "ip6 daddr 2001:db8:1::2 drop"} {
		if !strings.Contains(out443, outRule) {
			t.Fatalf("output missing %q:\n%s", outRule, out443)
		}
	}
	if strings.Contains(out443, "10.200.0.2 ct") || strings.Contains(br443, `oifname "bt2"`) && strings.Contains(br443, "10.200.0.2") {
		t.Fatalf("output or bridge output leaked exception for quarantined VM:\noutput:\n%s\nbridge:\n%s", out443, br443)
	}
	if !strings.Contains(br443, `oifname "bt2" drop`) || strings.Contains(br443, `oifname "bt2" ether type arp accept`) {
		t.Fatalf("bridge output did not unconditionally drop bt2:\n%s", br443)
	}

	// Configured HostHTTPSAddrs replaces fib daddr type local with an exact allowlist,
	// and SSHControlEndpoints still grants zero quarantine exceptions.
	c.HostHTTPSAddrs = []string{"198.51.100.10", "203.0.113.0/24"}
	c.SSHControlEndpoints = []string{"198.51.100.10:8443", "203.0.113.10"}
	rsExt := ruleset(c, vms)
	if strings.Count(rsExt, "{") != strings.Count(rsExt, "}") {
		t.Fatalf("unbalanced braces in ruleset:\n%s", rsExt)
	}
	inExt := inetChain(t, rsExt, "input")
	fwdExt := inetChain(t, rsExt, "forward")
	brOutExt := bridgeOutputChain(t, rsExt)
	if !strings.Contains(inExt, `iifname "boatbr0" ip daddr { 198.51.100.10, 203.0.113.0/24 } tcp dport 443 accept`) {
		t.Fatalf("input missing exact HostHTTPSAddrs allowlist:\n%s", inExt)
	}
	if strings.Contains(inExt, "{ 10.200.0.1") || strings.Contains(inExt, "10.200.0.1 }") {
		t.Fatalf("input injected unconfigured 10.200.0.1 into HostHTTPSAddrs:\n%s", inExt)
	}
	if strings.Contains(inExt, "fib daddr type local") {
		t.Fatalf("input kept fib daddr type local when HostHTTPSAddrs set:\n%s", inExt)
	}
	if strings.Contains(fwdExt, "8443") || strings.Contains(fwdExt, "198.51.100.10") || strings.Contains(brOutExt, "8443") {
		t.Fatalf("SSHControlEndpoints leaked quarantine exception:\nfwd:\n%s\nbridge:\n%s", fwdExt, brOutExt)
	}
}

func TestHostHTTPSLockdownAndEUI64Pinning(t *testing.T) {
	// Verify documentation prefix 2001:db8::/32 stays excluded from privateV6 per TestEgressBlocksV4InV6.
	docAddr := netip.MustParseAddr("2001:db8::1")
	for _, raw := range privateV6 {
		if netip.MustParsePrefix(raw).Contains(docAddr) {
			t.Fatalf("privateV6 %s unexpectedly matched 2001:db8::/32", raw)
		}
	}

	c := defaultConfig()
	c.IPv6Prefix = "2001:db8:1::/64"
	c.HostHTTPSPort = 443
	vms := []VM{
		{State: Running, SSHBootEpoch: "ok", SSHPolicyReady: true, Index: 2, Team: "blue", Tap: "bt2", IP: c.ipFor(2), MAC: macFor(2), IPv6: c.ipv6For(2)},
		{State: Running, SSHBootEpoch: "ok", SSHPolicyReady: true, Index: 3, Team: "blue", Tap: "bt3", IP: c.ipFor(3), MAC: macFor(3)},
	}

	// 1. Default HostHTTPSAddrs == []: drops all privateV4 and privateV6 (RFC1918, Tailscale CGNAT/ULA,
	// fe80::/10 link-local, loopback, NAT64) on :443 before fib daddr type local.
	rsEmpty := ruleset(c, vms)
	inEmpty := inetChain(t, rsEmpty, "input")
	dropV4 := `iifname "boatbr0" ip daddr { ` + strings.Join(privateV4, ", ") + ` } tcp dport 443 drop`
	dropV6 := `iifname "boatbr0" ip6 daddr { ` + strings.Join(privateV6, ", ") + ` } tcp dport 443 drop`
	fibAcc := `iifname "boatbr0" fib daddr type local tcp dport 443 accept`
	i4, i6, ifib := strings.Index(inEmpty, dropV4), strings.Index(inEmpty, dropV6), strings.Index(inEmpty, fibAcc)
	if !(i4 >= 0 && i6 > i4 && ifib > i6) {
		t.Fatalf("empty HostHTTPSAddrs failed to drop privateV4 + privateV6 before fib-local:\n%s", inEmpty)
	}
	for _, blocked := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "127.0.0.0/8", "fc00::/7", "fe80::/10", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48"} {
		if !strings.Contains(inEmpty[:ifib], blocked) {
			t.Errorf("empty HostHTTPSAddrs did not drop %s before fib-local", blocked)
		}
	}
	if strings.Contains(inEmpty, "meta l4proto ipv6-icmp accept") {
		t.Fatalf("input still has blanket ipv6-icmp accept:\n%s", inEmpty)
	}

	// 2. IPv6-only HostHTTPSAddrs: emits ip6 daddr accept only, no ip daddr and no fib daddr type local.
	c.HostHTTPSAddrs = []string{"2001:db8::10", "2001:db8:2::/64"}
	inV6Only := inetChain(t, ruleset(c, vms), "input")
	if !strings.Contains(inV6Only, `iifname "boatbr0" ip6 daddr { 2001:db8::10, 2001:db8:2::/64 } tcp dport 443 accept`) {
		t.Fatalf("IPv6-only HostHTTPSAddrs missing ip6 daddr rule:\n%s", inV6Only)
	}
	if strings.Contains(inV6Only, "tcp dport 443") && strings.Contains(inV6Only, "ip daddr {") {
		t.Fatalf("IPv6-only HostHTTPSAddrs unexpectedly emitted IPv4 :443 rule:\n%s", inV6Only)
	}
	if strings.Contains(inV6Only, "fib daddr type local") {
		t.Fatalf("IPv6-only HostHTTPSAddrs emitted fib daddr type local:\n%s", inV6Only)
	}

	// 3. Mixed IPv4 + IPv6 HostHTTPSAddrs: emits both ip daddr (literal, without injecting gateway) and ip6 daddr, no fib-local.
	c.HostHTTPSAddrs = []string{"203.0.113.10", "2001:db8::10"}
	inMixed := inetChain(t, ruleset(c, vms), "input")
	if !strings.Contains(inMixed, `iifname "boatbr0" ip daddr { 203.0.113.10 } tcp dport 443 accept`) ||
		strings.Contains(inMixed, "{ 10.200.0.1") ||
		!strings.Contains(inMixed, `iifname "boatbr0" ip6 daddr { 2001:db8::10 } tcp dport 443 accept`) ||
		strings.Contains(inMixed, "fib daddr type local") {
		t.Fatalf("mixed HostHTTPSAddrs input chain mismatch:\n%s", inMixed)
	}

	// 4. Per-tap EUI-64 link-local IPv6 pinning: bridge prerouting must never allow fe80::/10.
	if got := linkLocalForMAC("06:00:0a:c8:00:02"); got != "fe80::400:aff:fec8:2" {
		t.Fatalf("linkLocalForMAC = %q, want fe80::400:aff:fec8:2", got)
	}
	_, bridgePart, ok := strings.Cut(rsEmpty, "table bridge boat {")
	if !ok {
		t.Fatal("missing bridge table")
	}
	if strings.Contains(bridgePart, "fe80::/10") {
		t.Fatalf("bridge table still allows whole fe80::/10 prefix:\n%s", bridgePart)
	}
	for _, want := range []string{
		`iifname "bt2" ether type ip6 ip6 saddr != { 2001:db8:1::2, fe80::400:aff:fec8:2, :: } drop`,
		`iifname "bt2" icmpv6 type nd-neighbor-advert icmpv6 taddr != { 2001:db8:1::2, fe80::400:aff:fec8:2 } counter drop`,
		`iifname "bt3" ether type ip6 ip6 saddr != { fe80::400:aff:fec8:3, :: } drop`,
		`iifname "bt3" icmpv6 type nd-neighbor-advert icmpv6 taddr != { fe80::400:aff:fec8:3 } counter drop`,
	} {
		if !strings.Contains(bridgePart, want) {
			t.Errorf("bridge table missing %q:\n%s", want, bridgePart)
		}
	}

	// 5. Concurrent writeRouteFile writers use unique temp files and never race on routes.json.tmp.
	c.RunDir = t.TempDir()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				rf := routeFile{
					VMs:         map[string]routeVM{"vm-1": {IP: "10.200.0.2", Ports: []int{80 + j}, Public: []int{}, DesktopPort: 6080, Team: "blue", State: Running}},
					Quarantined: []string{},
				}
				if err := writeRouteFile(c, rf); err != nil {
					t.Errorf("concurrent writeRouteFile(%d,%d): %v", i, j, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	entries, err := os.ReadDir(c.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "routes.json" {
		t.Fatalf("unexpected files in RunDir after concurrent writeRouteFile: %v", entries)
	}
}

func TestPublicIPInboundRestrictedToPublishedPorts(t *testing.T) {
	c := defaultConfig()
	c.PublicIPv4 = []string{"198.51.100.25", "198.51.100.26", "198.51.100.27"}
	vms := []VM{
		{
			State: Running, SSHBootEpoch: "ready-1", SSHPolicyReady: true,
			Index: 2, Team: "blue", Tap: "bt2", IP: c.ipFor(2), MAC: macFor(2), PublicIP: "198.51.100.25",
			Ports: []Port{
				{GuestPort: 8080, HostPort: 20000},
				{GuestPort: 8443, HostPort: 20001},
				{GuestPort: 3000, HostPort: 0},
			},
		},
		{
			State: Running, SSHBootEpoch: "ready-2", SSHPolicyReady: true,
			Index: 3, Team: "blue", Tap: "bt3", IP: c.ipFor(3), MAC: macFor(3), PublicIP: "198.51.100.26",
			Ports: []Port{{GuestPort: 9000, HostPort: 0}},
		},
		{
			State: Running, SSHBootEpoch: "quarantined", SSHPolicyReady: false,
			Index: 4, Team: "red", Tap: "bt4", IP: c.ipFor(4), MAC: macFor(4), PublicIP: "198.51.100.27",
			Ports: []Port{{GuestPort: 8080, HostPort: 20002}},
		},
	}
	rs := ruleset(c, vms)
	pre := inetChain(t, rs, "prerouting")
	post := inetChain(t, rs, "postrouting")

	for _, want := range []string{
		"ip daddr 198.51.100.25 tcp dport 8080 dnat ip to 10.200.0.2:8080",
		"ip daddr 198.51.100.25 tcp dport 8443 dnat ip to 10.200.0.2:8443",
	} {
		if !strings.Contains(pre, want) {
			t.Fatalf("prerouting missing published PublicIP DNAT %q:\n%s", want, pre)
		}
	}
	for _, forbidden := range []string{
		"ip daddr 198.51.100.25 dnat ip to 10.200.0.2",
		"dport 3000",
		"dport 22",
		"dport 6080",
		"198.51.100.26",
		"198.51.100.27",
	} {
		if strings.Contains(pre, forbidden) {
			t.Fatalf("prerouting exposed non-published/quarantined PublicIP rule %q:\n%s", forbidden, pre)
		}
	}
	for _, wantSNAT := range []string{
		`ip saddr 10.200.0.2 oifname != "boatbr0" snat ip to 198.51.100.25`,
		`ip saddr 10.200.0.3 oifname != "boatbr0" snat ip to 198.51.100.26`,
	} {
		if !strings.Contains(post, wantSNAT) {
			t.Fatalf("postrouting missing outbound SNAT %q:\n%s", wantSNAT, post)
		}
	}

	// Removing a public port also flushes conntrack flows targeting PublicIP:GuestPort.
	m, h := newTestManager(t, func(cfg *Config) { cfg.PublicIPv4 = []string{"198.51.100.25"} })
	v, err := m.Create(t.Context(), CreateReq{Team: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddPort(v.ID, 8080, true); err != nil {
		t.Fatal(err)
	}
	if err := m.RemovePort(v.ID, 8080); err != nil {
		t.Fatal(err)
	}
	if !h.ran("conntrack -D -f ipv4 -d 198.51.100.25 -p tcp --dport 8080") {
		t.Fatalf("RemovePort did not flush PublicIP conntrack entry")
	}
}

func BenchmarkRulesetStoppedVMs(b *testing.B) {
	c := defaultConfig()
	c.IPv6Prefix = "2001:db8:1::/64"
	c.Teams = map[string]TeamPolicy{"blue": {AllowIntraTeam: true}}
	makeVMs := func(nRunning, nStopped int) []VM {
		vms := make([]VM, 0, nRunning+nStopped)
		idx := 2
		for i := 0; i < nRunning; i++ {
			vms = append(vms, VM{
				ID: "vm-run", State: Running, SSHBootEpoch: "ep", SSHPolicyReady: true,
				Index: idx, Team: "blue", Tap: tapFor(idx), IP: c.ipFor(idx), MAC: macFor(idx), IPv6: c.ipv6For(idx),
				Ports: []Port{{GuestPort: 8080, HostPort: 20000 + i}},
			})
			idx++
		}
		for i := 0; i < nStopped; i++ {
			vms = append(vms, VM{
				ID: "vm-stop", State: Stopped, SSHBootEpoch: "ep",
				Index: idx, Team: "blue", Tap: tapFor(idx), IP: c.ipFor(idx), MAC: macFor(idx), IPv6: c.ipv6For(idx),
			})
			idx++
		}
		return vms
	}
	b.Run("10_running", func(b *testing.B) {
		vms := makeVMs(10, 0)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = ruleset(c, vms)
		}
	})
	b.Run("10_running_500_stopped", func(b *testing.B) {
		vms := makeVMs(10, 500)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = ruleset(c, vms)
		}
	})
}
