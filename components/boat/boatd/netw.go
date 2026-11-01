package main

import (
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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

// Egress from VMs to these is dropped: RFC 1918, CGNAT, loopback, link-local
// (cloud metadata 169.254.169.254), benchmarking, multicast/reserved, and the
// IPv6 equivalents (ULA covers fd00:ec2::254). Published-port hairpins are
// accepted earlier (ct status dnat).
var (
	privateV4 = []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/3"}
	privateV6 = []string{"::1", "fc00::/7", "fe80::/10", "ff00::/8", "64:ff9b:1::/48"}
)

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
	// A tenant must not be able to configure the host: no RAs or redirects on
	// the bridge. Absent only when the host has IPv6 off, which has no such risk.
	for _, k := range []string{"accept_ra=0", "accept_redirects=0", "autoconf=0"} {
		if err := run("", "sysctl", "-qw", "net.ipv6.conf."+c.Bridge+"."+k); err != nil {
			log.Printf("bridge: %v", err)
		}
	}
	return nil
}

// guestTap is the tap firecracker opens. It lives in the VM's own netns, so
// every VM (and every fork restored from a snapshot) uses the same name.
const guestTap = "tap0"

func netnsFor(i int) string { return fmt.Sprintf("boat%d", i) }

func netnsPath(i int) string { return "/run/netns/" + netnsFor(i) }

// addVMNet builds the VM's network: netns boat<n> holding tap0 (owned by the
// VM's uid) bridged to veth0, whose peer bt<n> is the VM's port on boatbr0.
// The jailer joins the netns, so firecracker sees no host interface at all.
func addVMNet(c Config, run runner, v VM, uid int) error {
	delVMNet(run, v)
	ns, u := netnsFor(v.Index), fmt.Sprint(uid)
	steps := [][]string{
		{"ip", "netns", "add", ns},
		{"ip", "link", "add", v.Tap, "type", "veth", "peer", "name", "veth0", "netns", ns},
		{"ip", "netns", "exec", ns, "sysctl", "-qw", "net.ipv6.conf.all.disable_ipv6=1"},
		{"ip", "-n", ns, "link", "add", "br0", "type", "bridge"},
		{"ip", "-n", ns, "tuntap", "add", "dev", guestTap, "mode", "tap", "user", u, "group", u},
		{"ip", "-n", ns, "link", "set", guestTap, "master", "br0", "up"},
		{"ip", "-n", ns, "link", "set", "veth0", "master", "br0", "up"},
		{"ip", "-n", ns, "link", "set", "br0", "up"},
		{"ip", "link", "set", v.Tap, "master", c.Bridge, "up"},
	}
	for _, st := range steps {
		if err := run("", st[0], st[1:]...); err != nil {
			delVMNet(run, v)
			return err
		}
	}
	return nil
}

// delVMNet removes the netns (with tap0, br0, the veth pair). Errors = absent.
func delVMNet(run runner, v VM) {
	run("", "ip", "netns", "del", netnsFor(v.Index))
	run("", "ip", "link", "del", v.Tap)
}

// conntrackDel deletes matching flows. conntrack exits 1 when nothing matched;
// anything else means stale flows survive, so it is logged and counted
// (/v1/health conntrack_flush_errors).
func (m *Manager) conntrackDel(args ...string) {
	err := m.run("", "conntrack", append([]string{"-D"}, args...)...)
	if err != nil && !strings.Contains(err.Error(), "0 flow entries") {
		m.ctFlushErrs.Add(1)
		log.Printf("conntrack: %v", err)
	}
}

// flushConntrack drops tracked flows of an IP being released or reused, so a
// new VM never inherits a previous owner's NAT/established state.
func (m *Manager) flushConntrack(ips ...string) {
	for _, ip := range ips {
		if ip == "" {
			continue
		}
		family := "ipv4"
		if strings.Contains(ip, ":") {
			family = "ipv6"
		}
		m.conntrackDel("-f", family, "-s", ip)
		m.conntrackDel("-f", family, "-d", ip)
	}
}

// flushPort drops the flows of a port that stopped being public: established
// connections would otherwise outlive the rule that allowed them.
func (m *Manager) flushPort(v VM, p Port) {
	if p.HostPort > 0 {
		m.conntrackDel("-f", "ipv4", "-p", "tcp", "--dport", strconv.Itoa(p.HostPort))
	}
	if v.IPv6 != "" {
		m.conntrackDel("-f", "ipv6", "-d", v.IPv6, "-p", "tcp", "--dport", strconv.Itoa(p.GuestPort))
	}
}

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
	if c.AIProxyPort > 0 {
		w("    iifname %q ip daddr %s tcp dport %d accept", br, gw, c.AIProxyPort)
	}
	w("    iifname %q icmp type { echo-request, destination-unreachable, time-exceeded } accept", br)
	w("    iifname %q icmpv6 type { nd-router-advert, nd-redirect } drop", br)
	w("    iifname %q meta l4proto ipv6-icmp accept", br)
	w("    iifname %q drop\n  }", br)

	w("  chain forward {\n    type filter hook forward priority 0; policy accept;")
	w("    ct state established,related accept")
	if c.CtPerVM > 0 { // ponytail: counts restart on each reload; share a dynamic set if bursts matter
		for _, v := range vms {
			w("    iifname %q ip saddr %s ct count over %d drop", br, v.IP, c.CtPerVM)
			if v.IPv6 != "" {
				w("    iifname %q ip6 saddr %s ct count over %d drop", br, v.IPv6, c.CtPerVM)
			}
		}
	}
	w("    oifname %q ct status dnat accept", br)
	for _, v := range vms { // IPv6 inbound: published ports only
		for _, p := range v.Ports {
			if v.IPv6 != "" && p.HostPort > 0 {
				w("    oifname %q ip6 daddr %s tcp dport %d accept", br, v.IPv6, p.GuestPort)
			}
		}
	}
	w("    oifname %q drop", br)
	w("    iifname %q ip daddr { %s } drop", br, strings.Join(privateV4, ", "))
	w("    iifname %q ip6 daddr { %s } drop", br, strings.Join(privateV6, ", "))
	if !c.AllowSMTP {
		w("    iifname %q tcp dport { 25, 465, 587 } drop", br)
	}
	w("  }")
	if c.Jailer != "" { // a VMM escaping into the host netns still can't reach host services
		w("  chain output {\n    type filter hook output priority 0; policy accept;")
		w("    meta skuid %d-%d drop\n  }", c.JailUIDBase, c.JailUIDBase+maxIndex)
	}

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
	w("  chain output_nat {\n    type nat hook output priority -100; policy accept;")
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
	known := make([]string, 0, len(vms))
	for _, v := range vms {
		known = append(known, fmt.Sprintf("%q", v.Tap))
	}
	w("  chain prerouting {\n    type filter hook prerouting priority 0; policy accept;")
	w("    iifname \"bt*\" icmpv6 type { nd-router-advert, nd-redirect } counter drop")
	if len(known) > 0 { // a port with no rules yet (or left behind) passes nothing
		w("    iifname \"bt*\" iifname != { %s } drop", strings.Join(known, ", "))
	} else {
		w("    iifname \"bt*\" drop")
	}
	for _, v := range vms {
		if c.EgressMbit > 0 { // L2: the L3 hooks only see boatbr0, not the VM's port
			// Burst 100 ms: nft's default of a whole second lets 12% through on a 1 GiB transfer.
			w("    iifname %q limit rate over %d kbytes/second burst %d kbytes drop", v.Tap, c.EgressMbit*125, max(c.EgressMbit*125/10, 64))
		}
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
	Public      []int  `json:"public"` // reachable without a route token (published with a host port)
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
		ports, public := []int{}, []int{}
		for _, p := range v.Ports {
			ports = append(ports, p.GuestPort)
			if p.HostPort > 0 {
				public = append(public, p.GuestPort)
			}
		}
		sort.Ints(ports)
		sort.Ints(public)
		rf.VMs[v.ID] = routeVM{IP: v.IP, Ports: ports, Public: public, DesktopPort: c.DesktopPort, Team: v.Team, State: v.State}
	}
	if err := os.MkdirAll(c.RunDir, 0o755); err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(c.RunDir, "routes.json"), rf, 0o644)
}
