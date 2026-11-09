package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"time"
)

// hookGuard is the webhook dialer's Control. Besides the non-public nets
// (denyPrivate) it refuses this host's own addresses, public ones included,
// since a service bound to them (or to loopback behind a NAT rule) is the
// host itself, and every address of the VM pool: the configured -webhook-deny
// prefixes plus each VM's addresses as boatd reports them.

const hookGuardRefresh = 10 * time.Second

type hookGuard struct {
	deny     []netip.Prefix
	hostAddr func() []netip.Addr                         // this host's addresses
	vmAddrs  func(context.Context) ([]netip.Addr, error) // every VM's addresses
	now      func() time.Time

	mu      sync.Mutex
	at      time.Time
	blocked map[netip.Addr]bool
}

func (s *server) newHookGuard() *hookGuard {
	g := &hookGuard{hostAddr: interfaceAddrs, now: time.Now,
		vmAddrs: func(context.Context) ([]netip.Addr, error) { return nil, nil }}
	if s != nil {
		g.deny, g.vmAddrs = s.cfg.HookDeny, s.allVMAddrs
	}
	return g
}

func interfaceAddrs() []netip.Addr {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, p.Addr().Unmap().WithZone(""))
		}
	}
	return out
}

// allVMAddrs lists ip, ipv6 and public_ip of every VM on this host.
func (s *server) allVMAddrs(ctx context.Context) ([]netip.Addr, error) {
	var list struct {
		VMs []VM `json:"vms"`
	}
	if err := s.boatd.call(ctx, "GET", "/v1/vms", nil, &list); err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, vm := range list.VMs {
		for _, k := range []string{"ip", "ipv6", "public_ip"} {
			if a, err := netip.ParseAddr(strings.Split(str(vm, k), "/")[0]); err == nil {
				out = append(out, a.Unmap())
			}
		}
	}
	return out, nil
}

// refresh rebuilds the blocked set when it is older than hookGuardRefresh. A
// failed VM listing keeps the previous set and fails closed while it has none.
func (g *hookGuard) refresh() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blocked != nil && g.now().Sub(g.at) < hookGuardRefresh {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	vms, err := g.vmAddrs(ctx)
	if err != nil {
		if g.blocked == nil {
			return fmt.Errorf("cannot list the VM pool: %w", err)
		}
		return nil
	}
	b := map[netip.Addr]bool{}
	for _, a := range append(g.hostAddr(), vms...) {
		b[a] = true
	}
	g.blocked, g.at = b, g.now()
	return nil
}

func (g *hookGuard) control(network, address string, c syscall.RawConn) error {
	if err := denyPrivate(network, address, c); err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(address)
	ip, _ := netip.ParseAddr(host)
	ip = ip.Unmap().WithZone("")
	for _, p := range g.deny {
		if p.Contains(ip) {
			return fmt.Errorf("webhook target %s is in the VM pool", host)
		}
	}
	if err := g.refresh(); err != nil {
		return fmt.Errorf("webhook target %s: %w", host, err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blocked[ip] {
		return fmt.Errorf("webhook target %s is this host or one of its VMs", host)
	}
	return nil
}

// prefixFlag is a repeatable CIDR flag.
type prefixFlag []netip.Prefix

func (f *prefixFlag) String() string { return fmt.Sprint(*f) }
func (f *prefixFlag) Set(v string) error {
	p, err := netip.ParsePrefix(strings.TrimSpace(v))
	if err != nil {
		return err
	}
	*f = append(*f, p.Masked())
	return nil
}
