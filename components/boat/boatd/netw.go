package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
// (cloud metadata 169.254.169.254), benchmarking, multicast/reserved, the
// IPv6 equivalents (ULA covers fd00:ec2::254), and the IPv6 prefixes that carry IPv4
// (NAT64 64:ff9b::/96 and local-use 64:ff9b:1::/48, 6to4 2002::/16), which would
// otherwise reach the ranges above. Published-port hairpins are
// accepted earlier (ct status dnat).
var (
	privateV4 = []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/3"}
	privateV6 = []string{
		"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48",
		"100::/64", "2001::/32", "2002::/16", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
	}
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

func linkLocalForMAC(mac string) string {
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 {
		return ""
	}
	return netip.AddrFrom16([16]byte{
		0xfe, 0x80, 0, 0, 0, 0, 0, 0,
		hw[0] ^ 0x02, hw[1], hw[2], 0xff, 0xfe, hw[3], hw[4], hw[5],
	}).String()
}

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
// VM's uid and gid: the kernel checks both, so a shared jailGid must be the tap's group) bridged to veth0, whose peer bt<n> is the VM's port on boatbr0.
// The jailer joins the netns, so firecracker sees no host interface at all.
// Sequential ip/bridge commands are batched via `-force -batch -` to avoid ~15
// per-VM subprocess spawns on cold boot while preserving atomic error cleanup.
func addVMNet(c Config, run runner, v VM, uid, gid int) error {
	delVMNet(run, v)
	ns, u, g := netnsFor(v.Index), fmt.Sprint(uid), fmt.Sprint(gid)
	hostLines := []string{
		fmt.Sprintf("netns add %s", ns),
		fmt.Sprintf("link add %s type veth peer name veth0 netns %s", v.Tap, ns),
		fmt.Sprintf("link set %s master %s up", v.Tap, c.Bridge),
	}
	if v.IP != "" && v.MAC != "" {
		hostLines = append(hostLines, fmt.Sprintf("neigh replace %s lladdr %s dev %s nud permanent", v.IP, v.MAC, c.Bridge))
	}
	if v.IPv6 != "" && v.MAC != "" {
		hostLines = append(hostLines, fmt.Sprintf("neigh replace %s lladdr %s dev %s nud permanent", v.IPv6, v.MAC, c.Bridge))
	}
	nsLines := []string{
		"link add br0 type bridge",
		fmt.Sprintf("tuntap add dev %s mode tap user %s group %s", guestTap, u, g),
		fmt.Sprintf("link set %s master br0 up", guestTap),
		"link set veth0 master br0 up",
		"link set br0 up",
	}
	var bridgeLines []string
	if v.MAC != "" {
		bridgeLines = append(bridgeLines, fmt.Sprintf("fdb replace %s dev %s master static", v.MAC, v.Tap))
	}
	bridgeLines = append(bridgeLines, fmt.Sprintf("link set dev %s learning off flood off", v.Tap))

	type netStep struct {
		stdin string
		name  string
		args  []string
	}
	steps := []netStep{
		{strings.Join(hostLines, "\n") + "\n", "ip", []string{"-force", "-batch", "-"}},
		{"", "ip", []string{"netns", "exec", ns, "sysctl", "-qw", "net.ipv6.conf.all.disable_ipv6=1"}},
		{strings.Join(nsLines, "\n") + "\n", "ip", []string{"-n", ns, "-force", "-batch", "-"}},
		{strings.Join(bridgeLines, "\n") + "\n", "bridge", []string{"-force", "-batch", "-"}},
	}
	for _, st := range steps {
		if err := run(st.stdin, st.name, st.args...); err != nil {
			delVMNet(run, v)
			return err
		}
	}
	return nil
}

// l2Pins fixes the host's view of a VM. Neighbour entries are permanent, so no ARP or
// NA from any guest can repoint them. With them permanent the host never resolves, and
// nothing keeps a learned MAC from ageing out of the bridge, which would flood the
// VM's inbound traffic to every port: so its MAC is static and the port neither learns
// nor takes unknown unicast.
func l2Pins(c Config, v VM) [][]string {
	pins := [][]string{
		{"bridge", "fdb", "replace", v.MAC, "dev", v.Tap, "master", "static"},
		{"bridge", "link", "set", "dev", v.Tap, "learning", "off", "flood", "off"},
		{"ip", "neigh", "replace", v.IP, "lladdr", v.MAC, "dev", c.Bridge, "nud", "permanent"},
	}
	if v.IPv6 != "" {
		pins = append(pins, []string{"ip", "-6", "neigh", "replace", v.IPv6, "lladdr", v.MAC, "dev", c.Bridge, "nud", "permanent"})
	}
	return pins
}

// delNeighbours: the VM's address may be free for a long time. Errors = absent.
func delNeighbours(c Config, run runner, v VM) {
	var lines []string
	if v.IP != "" {
		lines = append(lines, fmt.Sprintf("neigh del %s dev %s", v.IP, c.Bridge))
	}
	if v.IPv6 != "" {
		lines = append(lines, fmt.Sprintf("neigh del %s dev %s", v.IPv6, c.Bridge))
	}
	if len(lines) > 0 {
		run(strings.Join(lines, "\n")+"\n", "ip", "-force", "-batch", "-")
	}
}

// delVMNet removes the netns (with tap0, br0, the veth pair). Errors = absent.
func delVMNet(run runner, v VM) {
	batch := fmt.Sprintf("netns del %s\nlink del %s\n", netnsFor(v.Index), v.Tap)
	run(batch, "ip", "-force", "-batch", "-")
}

const (
	ctUnknown uint8 = iota
	ctClean
	ctDirty

	conntrackParallel = 8
)

// markConntrackDirty records that ips may hold live conntrack flows (VM booted
// or adopted) and must be flushed before reuse or after deletion.
func (m *Manager) markConntrackDirty(ips ...string) {
	m.mu.Lock()
	if m.ctState == nil {
		m.ctState = map[string]uint8{}
	}
	if m.ctDirtyGen == nil {
		m.ctDirtyGen = map[string]uint64{}
	}
	for _, ip := range ips {
		if ip == "" {
			continue
		}
		m.ctState[ip] = ctDirty
		m.ctDirtyGen[ip]++
	}
	m.mu.Unlock()
}

// conntrackDel deletes matching flows. conntrack exits 1 when nothing matched;
// anything else means stale flows survive, so it is logged and counted
// (/v1/health conntrack_flush_errors). Returns true when no real error occurred.
func (m *Manager) conntrackDel(args ...string) bool {
	err := m.run("", "conntrack", append([]string{"-D"}, args...)...)
	if err != nil && !strings.Contains(err.Error(), "0 flow entries") {
		m.ctFlushErrs.Add(1)
		log.Printf("conntrack: %v", err)
		return false
	}
	return true
}

func (m *Manager) flushOneIP(ip string) {
	m.mu.Lock()
	if m.ctState != nil && m.ctState[ip] == ctClean {
		m.mu.Unlock()
		return
	}
	var gen uint64
	if m.ctDirtyGen != nil {
		gen = m.ctDirtyGen[ip]
	}
	m.mu.Unlock()

	family := "ipv4"
	if strings.Contains(ip, ":") {
		family = "ipv6"
	}
	ok1 := m.conntrackDel("-f", family, "-s", ip)
	ok2 := m.conntrackDel("-f", family, "-d", ip)

	m.mu.Lock()
	if m.ctState == nil {
		m.ctState = map[string]uint8{}
	}
	if ok1 && ok2 {
		if m.ctDirtyGen == nil || m.ctDirtyGen[ip] == gen {
			m.ctState[ip] = ctClean
		}
	} else {
		m.ctState[ip] = ctDirty
	}
	m.mu.Unlock()
}

// flushConntrack drops tracked flows of an IP being released or reused, so a
// new VM never inherits a previous owner's NAT/established state. Known-clean
// IPs (never booted since last flush, or already flushed) are skipped.
func (m *Manager) flushConntrack(ips ...string) {
	for _, ip := range ips {
		if ip == "" {
			continue
		}
		m.flushOneIP(ip)
	}
}

// flushConntrackBatch flushes dirty/unknown IPs for a batch of VMs concurrently
// with a bounded worker pool instead of running conntrack subprocesses sequentially.
func (m *Manager) flushConntrackBatch(vms []VM) {
	seen := map[string]bool{}
	var work []string
	m.mu.Lock()
	for _, v := range vms {
		for _, ip := range []string{v.IP, v.IPv6, v.PublicIP} {
			if ip == "" || seen[ip] {
				continue
			}
			seen[ip] = true
			if m.ctState == nil || m.ctState[ip] != ctClean {
				work = append(work, ip)
			}
		}
	}
	m.mu.Unlock()
	if len(work) == 0 {
		return
	}
	limit := min(conntrackParallel, len(work))
	ch := make(chan string, len(work))
	for _, ip := range work {
		ch <- ip
	}
	close(ch)
	var wg sync.WaitGroup
	for range limit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range ch {
				m.flushOneIP(ip)
			}
		}()
	}
	wg.Wait()
}

// flushPort drops the flows of a port that stopped being public: established
// connections would otherwise outlive the rule that allowed them.
func (m *Manager) flushPort(v VM, p Port) {
	if p.HostPort > 0 {
		m.conntrackDel("-f", "ipv4", "-p", "tcp", "--dport", strconv.Itoa(p.HostPort))
		if v.PublicIP != "" {
			m.conntrackDel("-f", "ipv4", "-d", v.PublicIP, "-p", "tcp", "--dport", strconv.Itoa(p.GuestPort))
		}
	}
	if v.IPv6 != "" {
		m.conntrackDel("-f", "ipv6", "-d", v.IPv6, "-p", "tcp", "--dport", strconv.Itoa(p.GuestPort))
	}
}

// ruleset renders the whole boat nftables config from the VM list. It is
// loaded atomically with `nft -f -` on every change (create/delete/port).
//
//	inet boat:   host input from VMs (DNS, ICMP, AI proxy and hostHttpsPort
//	             only), forward policy,
//	             DNAT for published ports (host and public IPv4), masquerade
//	bridge boat: per-tap anti-spoofing (MAC/IPv4/IPv6/ARP) and VM<->VM
//	             isolation, except same-team VMs when allowIntraTeam
func ruleset(c Config, vms []VM) string {
	// Skip VMs with no live tap/netns (Stopped, Error) so the ruleset scales with
	// active VMs only. Unknown bt* ports are still dropped by bridge prerouting.
	nActive := 0
	for _, v := range vms {
		if v.State != Stopped && v.State != Error {
			nActive++
		}
	}
	active := make([]VM, 0, nActive)
	for _, v := range vms {
		if v.State == Stopped || v.State == Error {
			continue
		}
		active = append(active, v)
	}
	vms = active
	sort.Slice(vms, func(i, j int) bool { return vms[i].Index < vms[j].Index })
	br, gw := c.Bridge, c.gateway()
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }

	var pauseV4, pauseV6, pauseTap []string
	for _, v := range vms {
		if v.sshNetworkReady() && v.sshPauseFenced {
			if v.IP != "" {
				pauseV4 = append(pauseV4, v.IP)
			}
			if v.IPv6 != "" {
				pauseV6 = append(pauseV6, v.IPv6)
			}
			if v.Tap != "" {
				pauseTap = append(pauseTap, fmt.Sprintf("%q", v.Tap))
			}
		}
	}
	writeSet := func(name, typ string, elems []string) {
		if len(elems) > 0 {
			w("  set %s { type %s; elements = { %s }; }", name, typ, strings.Join(elems, ", "))
		} else {
			w("  set %s { type %s; }", name, typ)
		}
	}

	w("table inet boat\ndelete table inet boat\ntable bridge boat\ndelete table bridge boat")
	ctLimit(w, c)
	w("table inet boat {")
	w("  set quarantine_v4 { type ipv4_addr; }")
	w("  set quarantine_v6 { type ipv6_addr; }")
	writeSet("ssh_pause_v4", "ipv4_addr", pauseV4)
	writeSet("ssh_pause_v6", "ipv6_addr", pauseV6)
	w("  chain input {\n    type filter hook input priority 0; policy accept;")
	w("    ip saddr @quarantine_v4 drop")
	w("    ip6 saddr @quarantine_v6 drop")
	quarantineRules(w, vms, "input")
	w("    iifname %q ct state established,related accept", br)
	w("    iifname %q ip daddr %s meta l4proto { udp, tcp } th dport 53 accept", br, gw)
	if c.AIProxyPort > 0 {
		w("    iifname %q ip daddr %s tcp dport %d accept", br, gw, c.AIProxyPort)
	}
	if c.HostHTTPSPort > 0 { // the public URL resolves to the host itself: input, not forward
		if v4, v6 := c.hostHTTPSAllowlists(); len(v4) > 0 || len(v6) > 0 {
			if len(v4) > 0 {
				w("    iifname %q ip daddr { %s } tcp dport %d accept", br, strings.Join(v4, ", "), c.HostHTTPSPort)
			}
			if len(v6) > 0 {
				w("    iifname %q ip6 daddr { %s } tcp dport %d accept", br, strings.Join(v6, ", "), c.HostHTTPSPort)
			}
		} else {
			w("    iifname %q ip daddr { %s } tcp dport %d drop", br, strings.Join(privateV4, ", "), c.HostHTTPSPort)
			w("    iifname %q ip6 daddr { %s } tcp dport %d drop", br, strings.Join(privateV6, ", "), c.HostHTTPSPort)
			w("    iifname %q fib daddr type local tcp dport %d accept", br, c.HostHTTPSPort)
		}
	}
	w("    iifname %q icmp type { echo-request, destination-unreachable, time-exceeded } accept", br)
	w("    iifname %q icmpv6 type { nd-router-advert, nd-redirect } drop", br)
	w("    iifname %q icmpv6 type { echo-request, destination-unreachable, packet-too-big, time-exceeded, parameter-problem, nd-router-solicit, nd-neighbor-solicit, nd-neighbor-advert } accept", br)
	w("    iifname %q drop\n  }", br)

	w("  chain forward {\n    type filter hook forward priority 0; policy accept;")
	w("    ip saddr @quarantine_v4 drop")
	w("    ip daddr @quarantine_v4 drop")
	w("    ip6 saddr @quarantine_v6 drop")
	w("    ip6 daddr @quarantine_v6 drop")
	quarantineRules(w, vms, "forward")
	w("    ip daddr @ssh_pause_v4 tcp dport 22 tcp flags & (fin|syn|rst|ack) == syn drop")
	w("    ip6 daddr @ssh_pause_v6 tcp dport 22 tcp flags & (fin|syn|rst|ack) == syn drop")
	w("    ct state established,related accept")
	w("    oifname %q ct status dnat accept", br)
	for _, v := range vms { // IPv6 inbound: published ports only
		if !v.sshNetworkReady() {
			continue
		}
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
	w("  chain output {\n    type filter hook output priority 0; policy accept;")
	w("    ip daddr @quarantine_v4 drop")
	w("    ip6 daddr @quarantine_v6 drop")
	quarantineRules(w, vms, "output")
	w("    ip daddr @ssh_pause_v4 tcp dport 22 tcp flags & (fin|syn|rst|ack) == syn drop")
	w("    ip6 daddr @ssh_pause_v6 tcp dport 22 tcp flags & (fin|syn|rst|ack) == syn drop")
	w("    meta skuid %d drop", nobodyID) // the import decompressor (parity.go), should it leave its netns
	if c.Jailer != "" {                   // a VMM escaping into the host netns still can't reach host services
		w("    meta skuid %d-%d drop", c.JailUIDBase, c.JailUIDBase+maxIndex)
	}
	w("  }")

	w("  chain prerouting {\n    type nat hook prerouting priority dstnat; policy accept;")
	for _, v := range vms {
		if !v.sshNetworkReady() {
			continue
		}
		for _, p := range v.Ports {
			if p.HostPort > 0 {
				if v.PublicIP != "" {
					w("    ip daddr %s tcp dport %d dnat ip to %s:%d", v.PublicIP, p.GuestPort, v.IP, p.GuestPort)
				}
				w("    ip daddr != 127.0.0.0/8 fib daddr type local tcp dport %d dnat ip to %s:%d", p.HostPort, v.IP, p.GuestPort)
			}
		}
	}
	w("  }")
	w("  chain output_nat {\n    type nat hook output priority -100; policy accept;")
	for _, v := range vms {
		if !v.sshNetworkReady() {
			continue
		}
		for _, p := range v.Ports {
			if p.HostPort > 0 {
				w("    ip daddr != 127.0.0.0/8 fib daddr type local tcp dport %d dnat ip to %s:%d", p.HostPort, v.IP, p.GuestPort)
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
	w("  set quarantine_tap { type ifname; }")
	writeSet("ssh_pause_tap", "ifname", pauseTap)
	teams := map[string][]string{}
	for _, v := range vms {
		if v.sshNetworkReady() && c.team(v.Team).AllowIntraTeam {
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
	w("    iifname @quarantine_tap ether type ip6 drop")
	w("    iifname \"bt*\" icmpv6 type { nd-router-advert, nd-redirect } counter drop")
	if len(known) > 0 { // a port with no rules yet (or left behind) passes nothing
		w("    iifname \"bt*\" iifname != { %s } drop", strings.Join(known, ", "))
	} else {
		w("    iifname \"bt*\" drop")
	}
	for _, v := range vms {
		if c.EgressMbit > 0 { // L2: the L3 hooks only see boatbr0, not the VM's port
			// nft kbytes are KiB. Its bucket holds at least one second of tokens, so
			// the first second of a burst can reach twice the cap; the steady rate is the cap.
			w("    iifname %q limit rate over %d kbytes/second drop", v.Tap, c.EgressMbit*1000000/8/1024)
		}
		w("    iifname %q ether saddr != %s drop", v.Tap, v.MAC)
		w("    iifname %q ether type arp arp saddr ip != %s drop", v.Tap, v.IP)
		w("    iifname %q ether type arp arp saddr ether != %s counter drop", v.Tap, v.MAC)
		w("    iifname %q ether type ip ip saddr != %s drop", v.Tap, v.IP)
		ll := linkLocalForMAC(v.MAC)
		if ll == "" && v.Index > 0 {
			ll = linkLocalForMAC(macFor(v.Index))
		}
		var v6s []string
		if v.IPv6 != "" {
			v6s = append(v6s, v.IPv6)
		}
		if ll != "" {
			v6s = append(v6s, ll)
		}
		v6s = append(v6s, "::")
		w("    iifname %q ether type ip6 ip6 saddr != { %s } drop", v.Tap, strings.Join(v6s, ", "))
		if !v.sshNetworkReady() {
			w("    iifname %q ether type ip6 drop", v.Tap)
		}
		// A neighbour advertisement names any address as its target: only its own
		// global IPv6 and pinned EUI-64 link-local address are permitted, so a guest
		// cannot poison neighbours or exhaust inet boat_ct @v6_<n> with forged fe80::/10 addresses.
		var targets []string
		if v.IPv6 != "" {
			targets = append(targets, v.IPv6)
		}
		if ll != "" {
			targets = append(targets, ll)
		}
		if len(targets) > 0 {
			w("    iifname %q icmpv6 type nd-neighbor-advert icmpv6 taddr != { %s } counter drop", v.Tap, strings.Join(targets, ", "))
		} else {
			w("    iifname %q icmpv6 type nd-neighbor-advert counter drop", v.Tap)
		}
	}
	w("  }")
	w("  chain forward {\n    type filter hook forward priority 0; policy accept;")
	w("    oifname @quarantine_tap drop")
	w("    iifname @quarantine_tap drop")
	for _, v := range vms {
		if !v.sshNetworkReady() {
			w("    oifname %q drop", v.Tap)
			w("    iifname %q drop", v.Tap)
		}
	}
	w("    oifname @ssh_pause_tap ether type ip tcp dport 22 tcp flags & (fin|syn|rst|ack) == syn drop")
	w("    oifname @ssh_pause_tap ether type ip6 tcp dport 22 tcp flags & (fin|syn|rst|ack) == syn drop")
	for i := range names {
		w("    iifname @team%d oifname @team%d accept", i, i)
	}
	w("    iifname \"bt*\" oifname \"bt*\" drop\n  }")
	w("  chain output { type filter hook output priority 0; policy accept;")
	w("    oifname @quarantine_tap drop")
	for _, v := range vms {
		if !v.sshNetworkReady() {
			w("    oifname %q drop", v.Tap)
		}
	}
	w("  }\n}")
	return b.String()
}

// ctLimit: new connections per VM, tracked in a table of its own that no reload
// deletes, so a count survives every VM change on the host (the rule used to live in
// inet boat and restart at each one). The sets carry the limit in their name: an
// element keeps the limit it was created with, so a new limit needs a new set.
func ctLimit(w func(string, ...any), c Config) {
	n, br := c.CtPerVM, c.Bridge
	hooks := []string{"forward", "input"} // input: each DNS query to the gateway is tracked too
	w("table inet boat_ct {")
	if n > 0 {
		w("  set v4_%d { type ipv4_addr; size 65535; flags dynamic; }", n)
		w("  set v6_%d { type ipv6_addr; size 65535; flags dynamic; }", n)
	}
	for _, hook := range hooks {
		w("  chain %s { type filter hook %s priority -1; policy accept; }", hook, hook)
	}
	w("}")
	for _, hook := range hooks {
		w("flush chain inet boat_ct %s", hook)
		if n > 0 {
			w("add rule inet boat_ct %s iifname %q ct state new add @v4_%d { ip saddr ct count over %d } drop", hook, br, n, n)
			w("add rule inet boat_ct %s iifname %q ct state new add @v6_%d { ip6 saddr ct count over %d } drop", hook, br, n, n)
		}
	}
}

// routeFile is /run/boat/routes.json, read by boat-gateway:
// <port>-<vmid>.vm.<domain> -> ip:port for each listed guest port, plus
// quarantined running VM IPs rejected with 403 if they ever reach the gateway.
type routeFile struct {
	VMs         map[string]routeVM `json:"vms"`
	Quarantined []string           `json:"quarantined"`
}

type routeVM struct {
	IP          string `json:"ip"`
	Ports       []int  `json:"ports"`
	Public      []int  `json:"public"` // reachable without a route token (published with a host port)
	DesktopPort int    `json:"desktopPort"`
	Team        string `json:"team"`
	State       State  `json:"state"`
}

func buildRouteFile(c Config, vms []VM) routeFile {
	rf := routeFile{VMs: map[string]routeVM{}, Quarantined: []string{}}
	seenQ := map[string]bool{}
	for _, v := range vms {
		if !v.sshNetworkReady() {
			if v.State == Running && v.IP != "" && !seenQ[v.IP] {
				seenQ[v.IP] = true
				rf.Quarantined = append(rf.Quarantined, v.IP)
			}
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
	sort.Strings(rf.Quarantined)
	return rf
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameRouteVM(a, b routeVM) bool {
	return a.IP == b.IP && a.DesktopPort == b.DesktopPort && a.Team == b.Team && a.State == b.State &&
		sameInts(a.Ports, b.Ports) && sameInts(a.Public, b.Public)
}

func sameRouteFile(a, b routeFile) bool {
	if len(a.VMs) != len(b.VMs) || len(a.Quarantined) != len(b.Quarantined) {
		return false
	}
	for i := range a.Quarantined {
		if a.Quarantined[i] != b.Quarantined[i] {
			return false
		}
	}
	for id, va := range a.VMs {
		vb, ok := b.VMs[id]
		if !ok || !sameRouteVM(va, vb) {
			return false
		}
	}
	return true
}

func routePreSyncNeeded(prev *routeFile, pre routeFile) bool {
	if prev == nil {
		return true
	}
	for id, oldVM := range prev.VMs {
		curVM, ok := pre.VMs[id]
		if !ok || !sameRouteVM(oldVM, curVM) {
			return true
		}
	}
	prevQ := make(map[string]bool, len(prev.Quarantined))
	for _, ip := range prev.Quarantined {
		prevQ[ip] = true
	}
	for _, ip := range pre.Quarantined {
		if !prevQ[ip] {
			return true
		}
	}
	return false
}

func writeRoutes(c Config, vms []VM) error {
	return writeRouteFile(c, buildRouteFile(c, vms))
}

func writeRouteFile(c Config, rf routeFile) error {
	if err := os.MkdirAll(c.RunDir, 0o755); err != nil {
		return err
	}
	// No fsync: it is rewritten from the VM list at every start, and an fsync here took
	// up to 5.3 s behind a memory dump, under netMu, on a create (boatbench 41628497611).
	// 0640 root:routesGroup: every running VM's IP and team, for boat-gateway and the ssh
	// jump host's key command only. The group is set before the rename: never unreadable.
	path := filepath.Join(c.RunDir, "routes.json")
	b, err := json.MarshalIndent(rf, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(c.RunDir, ".routes.json.*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := f.Chmod(0o640); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if c.routesGID > 0 {
		if err := os.Chown(tmp, -1, c.routesGID); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// lookupRoutesGroup sets routesGID from routesGroup, else socketGroup (who may drive
// boatd may read its routes); neither: root only.
func lookupRoutesGroup(c *Config) error {
	name := c.RoutesGroup
	if name == "" {
		name = c.SocketGroup
	}
	if name == "" {
		return nil
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		return fmt.Errorf("routesGroup: %w", err)
	}
	c.routesGID, err = strconv.Atoi(g.Gid)
	return err
}

// quarantineNFTArgs builds a single atomic nft invocation to add or delete v's
// addresses and tap in the dedicated quarantine sets (inet boat quarantine_v4/v6
// and bridge boat quarantine_tap) without reloading the whole ruleset.
func quarantineNFTArgs(op string, v VM) []string {
	args := []string{op, "element", "inet", "boat", "quarantine_v4", "{ " + v.IP + " }"}
	if v.IPv6 != "" {
		args = append(args, ";", op, "element", "inet", "boat", "quarantine_v6", "{ "+v.IPv6+" }")
	}
	args = append(args, ";", op, "element", "bridge", "boat", "quarantine_tap", fmt.Sprintf("{ %q }", v.Tap))
	return args
}

// sshPauseNFTArgs builds a single atomic nft invocation to add or delete v's
// addresses and tap in the transient SSH-only pause-fence sets (blocking new
// inbound TCP/22 SYNs while keeping existing HTTP/WebSocket/SSE sessions live).
func sshPauseNFTArgs(op string, v VM) []string {
	args := []string{op, "element", "inet", "boat", "ssh_pause_v4", "{ " + v.IP + " }"}
	if v.IPv6 != "" {
		args = append(args, ";", op, "element", "inet", "boat", "ssh_pause_v6", "{ "+v.IPv6+" }")
	}
	args = append(args, ";", op, "element", "bridge", "boat", "ssh_pause_tap", fmt.Sprintf("{ %q }", v.Tap))
	return args
}

// Quarantine is evaluated before generic established/DNAT acceptance. Guest SSH
// policy installation runs offline over Firecracker vsock, so unverified VMs
// receive zero IPv4 or IPv6 network exceptions in input, forward, or output.
func quarantineRules(w func(string, ...any), vms []VM, hook string) {
	for _, v := range vms {
		if v.sshNetworkReady() {
			continue
		}
		switch hook {
		case "input":
			w("    ip saddr %s drop", v.IP)
			if v.IPv6 != "" {
				w("    ip6 saddr %s drop", v.IPv6)
			}
		case "forward":
			w("    ip saddr %s drop", v.IP)
			w("    ip daddr %s drop", v.IP)
			if v.IPv6 != "" {
				w("    ip6 saddr %s drop", v.IPv6)
				w("    ip6 daddr %s drop", v.IPv6)
			}
		case "output":
			w("    ip daddr %s drop", v.IP)
			if v.IPv6 != "" {
				w("    ip6 daddr %s drop", v.IPv6)
			}
		}
	}
}
