package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// uplinkFallbackMbit: a NIC that reports no speed (virtio, some bonds) counts as 1 Gbit.
const uplinkFallbackMbit = 1000

// uplinkDev: the interface of the IPv4 default route with the lowest metric (/proc/net/route).
func uplinkDev(procNetRoute string) (string, error) {
	f, err := os.Open(procNetRoute)
	if err != nil {
		return "", err
	}
	defer f.Close()
	dev, best := "", -1
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// Iface Destination Gateway Flags RefCnt Use Metric Mask ...
		fs := strings.Fields(sc.Text())
		if len(fs) < 8 || fs[1] != "00000000" || fs[7] != "00000000" {
			continue
		}
		if m, err := strconv.Atoi(fs[6]); err == nil && (best < 0 || m < best) {
			dev, best = fs[0], m
		}
	}
	if dev == "" {
		return "", fmt.Errorf("no IPv4 default route in %s", procNetRoute)
	}
	return dev, sc.Err()
}

// uplinkMbit: the NIC's link speed, or uplinkFallbackMbit when it reports none.
func uplinkMbit(sysClassNet, dev string) int {
	b, err := os.ReadFile(filepath.Join(sysClassNet, dev, "speed"))
	if n, perr := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && perr == nil && n > 0 {
		return n
	}
	return uplinkFallbackMbit
}

// setupUplink resolves uplinkSharePct into the per-VM caps left unset, and puts a fair
// queue on the uplink's egress: cake `nat dual-srchost` looks through the masquerade, so
// each VM (and the host) gets an equal share of the port, then each flow within it.
// cake missing from the kernel: fq_codel, fair per flow only.
func setupUplink(c *Config, run runner, procNetRoute, sysClassNet string) error {
	if c.UplinkSharePct == 0 && !c.UplinkFairQueue {
		return nil
	}
	dev, err := uplinkDev(procNetRoute)
	if err != nil {
		return fmt.Errorf("uplink: %w", err)
	}
	if c.UplinkSharePct > 0 {
		speed := uplinkMbit(sysClassNet, dev)
		share := max(speed*c.UplinkSharePct/100, 1)
		if c.EgressMbit == 0 {
			c.EgressMbit = share
		}
		if c.IngressMbit == 0 {
			c.IngressMbit = share
		}
		log.Printf("uplink %s: %d Mbit; per-VM egress %d, ingress %d Mbit", dev, speed, c.EgressMbit, c.IngressMbit)
	}
	if !c.UplinkFairQueue {
		return nil
	}
	cake := run("", "tc", "qdisc", "replace", "dev", dev, "root", "cake", "unlimited", "besteffort", "dual-srchost", "nat")
	if cake == nil {
		log.Printf("uplink %s: cake dual-srchost nat", dev)
		return nil
	}
	if err := run("", "tc", "qdisc", "replace", "dev", dev, "root", "fq_codel"); err != nil {
		return fmt.Errorf("uplink %s: no fair queue: %v; %w", dev, cake, err)
	}
	log.Printf("uplink %s: fq_codel (per flow; cake failed: %v)", dev, cake)
	return nil
}
