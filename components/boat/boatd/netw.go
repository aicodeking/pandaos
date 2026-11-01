package main

import (
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// runner runs a host command with optional stdin; tests record instead.
type runner func(stdin, name string, args ...string) error

func execRunner(stdin, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

const maxIndex = 65534 // slots 2..65534 of the /16; .0.1 is the gateway

func (c Config) gateway() netip.Addr {
	return netip.MustParsePrefix(c.Subnet).Addr().Next()
}

func (c Config) ipFor(i int) string {
	b := netip.MustParsePrefix(c.Subnet).Addr().As4()
	b[2], b[3] = byte(i>>8), byte(i)
	return netip.AddrFrom4(b).String()
}

func macFor(i int) string { return fmt.Sprintf("06:00:0a:c8:%02x:%02x", byte(i>>8), byte(i)) }

func tapFor(i int) string { return fmt.Sprintf("bt%d", i) }

func (c Config) ipv6For(i int) string {
	if c.IPv6Prefix == "" {
		return ""
	}
	b := netip.MustParsePrefix(c.IPv6Prefix).Masked().Addr().As16()
	b[14], b[15] = byte(i>>8), byte(i)
	return netip.AddrFrom16(b).String()
}

func (c Config) ipv6Gateway() string { return c.ipv6For(1) }

// setupBridge creates boatbr0 with the gateway addresses. Idempotent.
func setupBridge(c Config, run runner) error {
	if run("", "ip", "link", "show", c.Bridge) != nil {
		if err := run("", "ip", "link", "add", c.Bridge, "type", "bridge"); err != nil {
			return err
		}
	}
	steps := [][]string{
		{"ip", "addr", "replace", c.gateway().String() + "/16", "dev", c.Bridge},
		{"ip", "link", "set", c.Bridge, "up"},
		{"sysctl", "-qw", "net.ipv4.ip_forward=1"},
	}
	if c.IPv6Prefix != "" {
		bits := netip.MustParsePrefix(c.IPv6Prefix).Bits()
		steps = append(steps,
			[]string{"ip", "-6", "addr", "replace", fmt.Sprintf("%s/%d", c.ipv6Gateway(), bits), "dev", c.Bridge},
			[]string{"sysctl", "-qw", "net.ipv6.conf.all.forwarding=1"})
	}
	for _, s := range steps {
		if err := run("", s[0], s[1:]...); err != nil {
			return err
		}
	}
	return nil
}

func addTap(c Config, run runner, tap string, uid int) error {
	run("", "ip", "link", "del", tap) // stale from a crash; error = absent
	if err := run("", "ip", "tuntap", "add", "dev", tap, "mode", "tap", "user", fmt.Sprint(uid), "group", fmt.Sprint(uid)); err != nil {
		return err
	}
	return run("", "ip", "link", "set", tap, "master", c.Bridge, "up")
}

func delTap(run runner, tap string) { run("", "ip", "link", "del", tap) }

// ruleset renders the whole boat nftables config from the VM list. It is
// loaded atomically with `nft -f -` on every change (create/delete/port).
//
//	inet boat:   host input from VMs (DNS + ICMP only), forward policy,
//	             DNAT for public ports and public IPv4, masquerade
//	bridge boat: per-tap anti-spoofing (MAC/IPv4/IPv6/ARP) and VM<->VM
//	             isolation, except same-team VMs when allowIntraTeam
func ruleset(c Config, vms []VM) string {
	sort.Slice(vms, func(i, j int) bool { return vms[i].Index < vms[j].Index })
	br, gw := c.Bridge, c.gateway()
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }

	w("table inet boat\ndelete table inet boat\ntable bridge boat\ndelete table bridge boat")
	w("table inet boat {")
	w("  chain input {\n    type filter hook input priority 0; policy accept;")
	w("    iifname %q ct state established,related accept", br)
	w("    iifname %q ip daddr %s meta l4proto { udp, tcp } th dport 53 accept", br, gw)
	w("    iifname %q icmp type { echo-request, destination-unreachable, time-exceeded } accept", br)
	w("    iifname %q meta l4proto ipv6-icmp accept", br)
	w("    iifname %q drop\n  }", br)

	w("  chain forward {\n    type filter hook forward priority 0; policy accept;")
	w("    oifname %q ct state established,related accept", br)
	w("    oifname %q ct status dnat accept", br)
	if c.IPv6Prefix != "" {
		w("    oifname %q ip6 daddr %s accept", br, c.IPv6Prefix)
	}
	w("    oifname %q drop", br)
	w("    iifname %q ip daddr { 169.254.0.0/16, %s } drop", br, c.Subnet)
	w("  }")

	w("  chain prerouting {\n    type nat hook prerouting priority dstnat; policy accept;")
	for _, v := range vms {
		if v.PublicIP != "" {
			w("    ip daddr %s dnat ip to %s", v.PublicIP, v.IP)
		}
		for _, p := range v.Ports {
			if p.HostPort > 0 {
				w("    fib daddr type local tcp dport %d dnat ip to %s:%d", p.HostPort, v.IP, p.GuestPort)
			}
		}
	}
	w("  }")
	w("  chain output {\n    type nat hook output priority -100; policy accept;")
	for _, v := range vms {
		for _, p := range v.Ports {
			if p.HostPort > 0 {
				w("    fib daddr type local tcp dport %d dnat ip to %s:%d", p.HostPort, v.IP, p.GuestPort)
			}
		}
	}
	w("  }")
	w("  chain postrouting {\n    type nat hook postrouting priority srcnat; policy accept;")
	for _, v := range vms {
		if v.PublicIP != "" {
			w("    ip saddr %s oifname != %q snat ip to %s", v.IP, br, v.PublicIP)
		}
	}
	w("    ip saddr %s ip daddr %s ct status dnat masquerade", c.Subnet, c.Subnet) // hairpin to a sibling's public port
	w("    ip saddr %s oifname != %q masquerade\n  }\n}", c.Subnet, br)

	w("table bridge boat {")
	teams := map[string][]string{}
	for _, v := range vms {
		if c.team(v.Team).AllowIntraTeam {
			teams[v.Team] = append(teams[v.Team], fmt.Sprintf("%q", v.Tap))
		}
	}
	names := make([]string, 0, len(teams))
	for t := range teams {
		names = append(names, t)
	}
	sort.Strings(names)
	for i, t := range names {
		w("  set team%d { type ifname; elements = { %s }; } # %s", i, strings.Join(teams[t], ", "), strings.ReplaceAll(t, "\n", ""))
	}
	w("  chain prerouting {\n    type filter hook prerouting priority 0; policy accept;")
	for _, v := range vms {
		w("    iifname %q ether saddr != %s drop", v.Tap, v.MAC)
		w("    iifname %q ether type arp arp saddr ip != %s drop", v.Tap, v.IP)
		w("    iifname %q ether type ip ip saddr != %s drop", v.Tap, v.IP)
		v6 := "fe80::/10, ::"
		if v.IPv6 != "" {
			v6 = v.IPv6 + ", " + v6
		}
		w("    iifname %q ether type ip6 ip6 saddr != { %s } drop", v.Tap, v6)
	}
	w("  }")
	w("  chain forward {\n    type filter hook forward priority 0; policy accept;")
	w("    ether type arp accept")
	for i := range names {
		w("    iifname @team%d oifname @team%d accept", i, i)
	}
	w("    iifname \"bt*\" oifname \"bt*\" drop\n  }\n}")
	return b.String()
}

// routeFile is /run/boat/routes.json, read by boat-gateway:
// <port>-<vmid>.vm.<domain> -> ip:port for each listed guest port.
type routeFile struct {
	VMs map[string]routeVM `json:"vms"`
}

type routeVM struct {
	IP          string `json:"ip"`
	Ports       []int  `json:"ports"`
	DesktopPort int    `json:"desktopPort"`
	Team        string `json:"team"`
	State       State  `json:"state"`
}

func writeRoutes(c Config, vms []VM) error {
	rf := routeFile{VMs: map[string]routeVM{}}
	for _, v := range vms {
		if v.State != Running {
			continue
		}
		ports := []int{}
		for _, p := range v.Ports {
			ports = append(ports, p.GuestPort)
		}
		rf.VMs[v.ID] = routeVM{IP: v.IP, Ports: ports, DesktopPort: c.DesktopPort, Team: v.Team, State: v.State}
	}
	if err := os.MkdirAll(c.RunDir, 0o755); err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(c.RunDir, "routes.json"), rf, 0o644)
}
