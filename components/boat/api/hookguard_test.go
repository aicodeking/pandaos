package main

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// Webhooks never reach this host (its public addresses included: a service
// bound there is the host itself) nor any VM of the pool.
func TestHookGuardDeniesHostAndVMPool(t *testing.T) {
	host := netip.MustParseAddr("93.184.216.10")  // the host's own public address
	vm := netip.MustParseAddr("2a01:4f8:1:2::77") // a VM's public IPv6
	g := &hookGuard{now: time.Now,
		deny:     []netip.Prefix{netip.MustParsePrefix("185.10.20.0/28")}, // -webhook-deny: the VM pool
		hostAddr: func() []netip.Addr { return []netip.Addr{host} },
		vmAddrs:  func(context.Context) ([]netip.Addr, error) { return []netip.Addr{vm}, nil }}
	for _, a := range []string{"93.184.216.10:443", "[2a01:4f8:1:2::77]:443", "185.10.20.5:443", "127.0.0.1:80", "[::ffff:93.184.216.10]:443"} {
		if err := g.control("tcp", a, nil); err == nil {
			t.Errorf("%s allowed", a)
		}
	}
	if err := g.control("tcp", "93.184.216.34:443", nil); err != nil {
		t.Errorf("a public address elsewhere: %v", err)
	}
	// The VM list cannot be read: fail closed until it can.
	g2 := &hookGuard{now: time.Now, hostAddr: func() []netip.Addr { return nil },
		vmAddrs: func(context.Context) ([]netip.Addr, error) { return nil, errors.New("down") }}
	if err := g2.control("tcp", "93.184.216.34:443", nil); err == nil || !strings.Contains(err.Error(), "VM pool") {
		t.Errorf("boatd down: %v", err)
	}
}

// The real wiring: the server's guard reads VM addresses from boatd.
func TestHookGuardReadsVMsFromBoatd(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	e.boatd.mu.Lock()
	e.boatd.vms[id]["public_ip"] = "198.51.100.200"
	e.boatd.vms[id]["ipv6"] = "2a01:4f8:9::5"
	e.boatd.mu.Unlock()
	g := e.srv.newHookGuard()
	if err := g.control("tcp", "[2a01:4f8:9::5]:443", nil); err == nil {
		t.Error("a VM's IPv6 is a webhook target")
	}
	for _, a := range interfaceAddrs() {
		if a.Is4() || a.Is6() {
			if err := g.control("tcp", netip.AddrPortFrom(a, 443).String(), nil); err == nil {
				t.Errorf("host address %s allowed", a)
			}
		}
	}
}
