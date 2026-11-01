package main

import (
	"errors"
	"strings"
	"testing"
)

func TestRuleset(t *testing.T) {
	c := defaultConfig()
	c.IPv6Prefix = "2001:db8:1::/64"
	c.PublicIPv4 = []string{"203.0.113.7"}
	c.Teams = map[string]TeamPolicy{"blue": {AllowIntraTeam: true}}
	vms := []VM{
		{Index: 2, Team: "blue", Tap: "bt2", IP: c.ipFor(2), MAC: macFor(2), IPv6: c.ipv6For(2), PublicIP: "203.0.113.7",
			Ports: []Port{{GuestPort: 8080, HostPort: 20000}, {GuestPort: 3000}}},
		{Index: 3, Team: "blue", Tap: "bt3", IP: c.ipFor(3), MAC: macFor(3)},
		{Index: 300, Team: "red", Tap: "bt300", IP: c.ipFor(300), MAC: macFor(300)},
	}
	rs := ruleset(c, vms)
	for _, want := range []string{
		"table inet boat\ndelete table inet boat", // atomic replace
		`iifname "boatbr0" ip daddr 10.200.0.1 meta l4proto { udp, tcp } th dport 53 accept`,
		`iifname "boatbr0" drop`,
		"fib daddr type local tcp dport 20000 dnat ip to 10.200.0.2:8080",
		"ip daddr 203.0.113.7 dnat ip to 10.200.0.2",
		`ip saddr 10.200.0.2 oifname != "boatbr0" snat ip to 203.0.113.7`,
		`ip saddr 10.200.0.0/16 oifname != "boatbr0" masquerade`,
		`set team0 { type ifname; elements = { "bt2", "bt3" }; }`,
		"iifname @team0 oifname @team0 accept",
		`iifname "bt*" oifname "bt*" drop`,
		`iifname "bt*" icmpv6 type { nd-router-advert, nd-redirect } counter drop`,
		`iifname "boatbr0" icmpv6 type { nd-router-advert, nd-redirect } drop`,
		`iifname "bt*" iifname != { "bt2", "bt3", "bt300" } drop`,
		`iifname "boatbr0" ip saddr 10.200.0.2 ct count over 32768 drop`,
		`iifname "boatbr0" ip6 saddr 2001:db8:1::2 ct count over 32768 drop`,
		`iifname "boatbr0" ip daddr { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12,`,
		`iifname "boatbr0" ip6 daddr { ::1, fc00::/7, fe80::/10, ff00::/8, 64:ff9b:1::/48 } drop`,
		`iifname "boatbr0" tcp dport { 25, 465, 587 } drop`,
		`oifname "boatbr0" ip6 daddr 2001:db8:1::2 tcp dport 8080 accept`,
		"ct state established,related accept",
		`iifname "bt300" ether type ip ip saddr != 10.200.1.44 drop`,
		`iifname "bt300" ether saddr != 06:00:0a:c8:01:2c drop`,
		`iifname "bt2" ether type ip6 ip6 saddr != { 2001:db8:1::2, fe80::/10, :: } drop`,
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("ruleset missing %q\n%s", want, rs)
		}
	}
	if strings.Contains(rs, "dport 3000 accept") || strings.Contains(rs, "ip6 daddr 2001:db8:1::/64 accept") || strings.Contains(rs, "skuid") {
		t.Errorf("unpublished v6 port open, whole prefix open, or jail-uid rule without a jailer\n%s", rs)
	}
	if strings.Contains(rs, "dport 8744") {
		t.Error("AI proxy port open by default")
	}
	c.AIProxyPort = 8744
	if !strings.Contains(ruleset(c, vms), `iifname "boatbr0" ip daddr 10.200.0.1 tcp dport 8744 accept`) {
		t.Error("AI proxy port not opened")
	}
	c.Jailer, c.AllowSMTP, c.EgressMbit = "/bin/jailer", true, 100
	rs = ruleset(c, vms)
	if !strings.Contains(rs, "meta skuid 900000-965534 drop") || strings.Contains(rs, "dport { 25") {
		t.Errorf("jailer/smtp options\n%s", rs)
	}
	// The policer must sit in the bridge table: on the L3 hooks iifname is boatbr0.
	_, bridge, _ := strings.Cut(rs, "table bridge boat {")
	if !strings.Contains(bridge, `iifname "bt2" limit rate over 12500 kbytes/second drop`) {
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
