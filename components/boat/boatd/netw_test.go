package main

import (
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
		`oifname "boatbr0" ip6 daddr 2001:db8:1::/64 accept`,
		"fib daddr type local tcp dport 20000 dnat ip to 10.200.0.2:8080",
		"ip daddr 203.0.113.7 dnat ip to 10.200.0.2",
		`ip saddr 10.200.0.2 oifname != "boatbr0" snat ip to 203.0.113.7`,
		`ip saddr 10.200.0.0/16 oifname != "boatbr0" masquerade`,
		`set team0 { type ifname; elements = { "bt2", "bt3" }; }`,
		"iifname @team0 oifname @team0 accept",
		`iifname "bt*" oifname "bt*" drop`,
		`iifname "bt300" ether type ip ip saddr != 10.200.1.44 drop`,
		`iifname "bt300" ether saddr != 06:00:0a:c8:01:2c drop`,
		`iifname "bt2" ether type ip6 ip6 saddr != { 2001:db8:1::2, fe80::/10, :: } drop`,
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("ruleset missing %q\n%s", want, rs)
		}
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
