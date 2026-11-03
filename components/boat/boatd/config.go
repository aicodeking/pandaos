package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Size is a VM shape. DiskGiB is the sparse disk the rootfs is grown to.
type Size struct {
	VCPU    int `json:"vcpu"`
	MemMiB  int `json:"mem_mib"`
	DiskGiB int `json:"disk_gib"`
}

// TeamPolicy is a per-team quota. Zero = unlimited. maxVMs/maxVCPU/maxMemMiB
// count active VMs; maxTotalVMs counts stopped ones too; maxDiskGiB is the
// committed storage (each VM's disk + mem file, each snapshot's disk + mem).
type TeamPolicy struct {
	MaxVMs         int  `json:"maxVMs"`
	MaxVCPU        int  `json:"maxVCPU"`
	MaxMemMiB      int  `json:"maxMemMiB"`
	MaxTotalVMs    int  `json:"maxTotalVMs"`
	MaxSnapshots   int  `json:"maxSnapshots"`
	MaxDiskGiB     int  `json:"maxDiskGiB"`
	MaxPorts       int  `json:"maxPorts"` // ports across the team's VMs
	AllowIntraTeam bool `json:"allowIntraTeam"`
}

// Config is /etc/boat/boatd.json (written by the NixOS module). Missing
// fields take the defaults below.
type Config struct {
	StateDir    string   `json:"stateDir"`    // images/, vms/, snapshots/, usage.jsonl
	RunDir      string   `json:"runDir"`      // routes.json
	Socket      string   `json:"socket"`      // internal API
	SocketGroup string   `json:"socketGroup"` // group allowed on the socket (boat-api)
	Firecracker string   `json:"firecracker"` // binary path
	Jailer      string   `json:"jailer"`      // jailer binary; "" = run firecracker as FCUser
	JailerArgs  []string `json:"jailerArgs"`  // extra jailer flags (cgroups, netns...)
	JailUIDBase int      `json:"jailUidBase"` // VM n runs as uid/gid base+n under the jailer
	FCUID       int      `json:"fcUid"`       // no-jailer mode: uid/gid firecracker runs as (0 = boatd's)
	FCGID       int      `json:"fcGid"`
	Reflink     string   `json:"reflink"` // cp --reflink=<always|auto>
	Bridge      string   `json:"bridge"`
	Subnet      string   `json:"subnet"` // IPv4 /16; .0.1 is the gateway
	DNS         []string `json:"dns"`
	IPv6Prefix  string   `json:"ipv6Prefix"` // routed prefix, e.g. 2001:db8:1::/64; VM n gets prefix::n/128
	PublicIPv4  []string `json:"publicIPv4"` // 1:1 NAT pool, routed to this host
	PortMin     int      `json:"portMin"`
	PortMax     int      `json:"portMax"`
	DesktopPort int      `json:"desktopPort"` // guest port the gateway proxies the desktop to

	Sizes        map[string]Size `json:"sizes"`
	DefaultSize  string          `json:"defaultSize"`
	DefaultImage string          `json:"defaultImage"`
	BootArgs     string          `json:"bootArgs"`
	BootTimeoutS int             `json:"bootTimeoutS"`  // wait for the guest agent
	IdleStopS    int             `json:"idleStopS"`     // 0 = never auto-stop on idle
	ReapS        int             `json:"reapIntervalS"` // TTL/idle check period

	// Host protection.
	MinFreePct        int     `json:"minFreePct"`        // refuse create/fork/snapshot below this free % of stateDir
	MinFreeGiB        int     `json:"minFreeGiB"`        // ... or below this many GiB (the larger floor wins)
	MemOvercommit     float64 `json:"memOvercommit"`     // active VM RAM <= (host RAM - reserve) * this
	CPUOvercommit     float64 `json:"cpuOvercommit"`     // active vCPUs <= host CPUs * this
	HostReserveMiB    int     `json:"hostReserveMiB"`    // RAM kept for the host itself
	HostMemMiB        int     `json:"hostMemMiB"`        // 0 = /proc/meminfo
	HostCPUs          int     `json:"hostCPUs"`          // 0 = runtime.NumCPU
	VMOverheadMiB     int     `json:"vmOverheadMiB"`     // cgroup memory.high = VM RAM + this
	VMPageCacheMiB    int     `json:"vmPageCacheMiB"`    // memory.max = memory.high + this: host page cache of the VM's disk I/O
	CgroupParent      string  `json:"cgroupParent"`      // jailer --parent-cgroup, outside boatd's unit so VMs survive a crash
	CgroupsOptional   bool    `json:"cgroupsOptional"`   // start even if VMs would run without cgroup limits
	BtrfsQgroups      bool    `json:"btrfsQgroups"`      // each team's data is a subvolume with an exclusive-bytes limit
	StorageOvercommit float64 `json:"storageOvercommit"` // committed storage of all teams <= stateDir's size * this; 0 = no limit
	UntrustedTenants  bool    `json:"untrustedTenants"`  // refuse to run without the limits and host settings hostile tenants call for
	DiffSnapshots     bool    `json:"diffSnapshots"`     // dirty-page snapshots merged onto a reflinked base

	// Performance and density. See PROTOCOL.md "Performance and density".
	DiskEngine     string `json:"diskEngine"`     // auto (Async, else Sync), Async or Sync
	DiskNoCow      bool   `json:"diskNoCow"`      // btrfs: images and the VM disks reflinked from them are NOCOW
	Balloon        bool   `json:"balloon"`        // virtio-balloon: free page reporting, deflate on OOM
	HostDirtyRatio int    `json:"hostDirtyRatio"` // vm.dirty_ratio set at startup, 0 = leave the host's: a dump must not be throttled to the disk
	WarmPool       bool   `json:"warmPool"`       // create restores a booted template of the image + size
	KSMTeam        string `json:"ksmTeam"`        // "" off; one team's VMs may merge; "*" all (one trust domain only)

	// Egress policy.
	AllowSMTP   bool `json:"allowSMTP"`   // else tcp 25/465/587 out is dropped
	AIProxyPort int  `json:"aiProxyPort"` // gateway port VMs may reach (boat-api's AI proxy); 0 = closed
	EgressMbit  int  `json:"egressMbit"`  // per-VM egress cap (bridge policer); 0 = none

	// Per-VM limits. 0 = none. Firecracker limiters apply from the next cold boot
	// (a restored VM keeps those saved in its snapshot).
	IngressMbit   int  `json:"ingressMbit"`   // Firecracker rx rate limiter
	DiskMBps      int  `json:"diskMBps"`      // Firecracker drive rate limiter
	DiskIOPS      int  `json:"diskIOPS"`      // ... operations
	IOMaxMBps     int  `json:"ioMaxMBps"`     // cgroup io.max on stateDir's disk (rbps+wbps)
	IOMaxIOPS     int  `json:"ioMaxIOPS"`     // ... riops+wiops
	CtPerVM       int  `json:"ctPerVM"`       // new forwarded connections refused past this many tracked
	MaxPortsPerVM int  `json:"maxPortsPerVM"` // routes + public ports of one VM
	ResumeCrashed bool `json:"resumeCrashed"` // cold-boot VMs that were running when boatd/the host died

	cgroupLimits bool                  // set at startup when the cgroup controllers could be delegated
	stateDisk    string                // "major:minor" of stateDir's disk, for io.max
	Teams        map[string]TeamPolicy `json:"teams"`
	DefaultTeam  TeamPolicy            `json:"defaultTeam"`
}

var (
	reName  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reTeam  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	reEnv   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	reLabel = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,63}$`)
	reImage = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

func defaultConfig() Config {
	return Config{
		StateDir:          "/var/lib/boat",
		RunDir:            "/run/boat",
		Socket:            "/run/boat/boatd.sock",
		Firecracker:       "firecracker",
		JailUIDBase:       900000,
		Reflink:           "always",
		Bridge:            "boatbr0",
		Subnet:            "10.200.0.0/16",
		DNS:               []string{"1.1.1.1", "9.9.9.9"},
		PortMin:           20000,
		PortMax:           40000,
		DesktopPort:       6080,
		DefaultSize:       "default",
		DefaultImage:      "ubuntu-24.04",
		BootArgs:          "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw",
		BootTimeoutS:      60,
		BtrfsQgroups:      true,
		StorageOvercommit: 10, // disks are sparse: real space is what the floor guards
		ReapS:             30,
		MinFreePct:        10,
		MinFreeGiB:        5,
		MemOvercommit:     1.5,
		CPUOvercommit:     4.0,
		HostReserveMiB:    1024,
		VMOverheadMiB:     256,
		VMPageCacheMiB:    512,
		DiffSnapshots:     true,
		CgroupParent:      "boat-vms",
		DiskEngine:        "auto",
		DiskNoCow:         true,
		Balloon:           true,
		HostDirtyRatio:    50,
		CtPerVM:           4096, // nf_conntrack_max is about 260k on 16 GiB, or the module's 1M
		MaxPortsPerVM:     32,
		ResumeCrashed:     true,
		DefaultTeam:       TeamPolicy{MaxTotalVMs: 100, MaxSnapshots: 200, MaxDiskGiB: 4096, MaxPorts: 512},
		Sizes: map[string]Size{
			"small":   {2, 4096, 51},
			"default": {4, 8192, 51},
			"large":   {8, 16384, 111},
			"xlarge":  {16, 32768, 251},
		},
	}
}

func loadConfig(path string) (Config, error) {
	c := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return c, err
	}
	if err == nil {
		sizes := c.Sizes
		c.Sizes = nil
		if err := json.Unmarshal(data, &c); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
		if c.Sizes == nil {
			c.Sizes = sizes
		}
	}
	return c, c.validate()
}

func (c Config) validate() error {
	p, err := netip.ParsePrefix(c.Subnet)
	if err != nil || !p.Addr().Is4() || p.Bits() != 16 || p.Masked() != p {
		return fmt.Errorf("subnet %q: want an IPv4 /16 network", c.Subnet)
	}
	if c.IPv6Prefix != "" {
		p6, err := netip.ParsePrefix(c.IPv6Prefix)
		if err != nil || !p6.Addr().Is6() || p6.Bits() > 112 {
			return fmt.Errorf("ipv6Prefix %q: want an IPv6 prefix of /112 or shorter", c.IPv6Prefix)
		}
	}
	for _, a := range append(append([]string{}, c.DNS...), c.PublicIPv4...) {
		if _, err := netip.ParseAddr(a); err != nil {
			return fmt.Errorf("bad address %q", a)
		}
	}
	if c.PortMin < 1024 || c.PortMax > 65535 || c.PortMin > c.PortMax {
		return fmt.Errorf("port range %d-%d invalid", c.PortMin, c.PortMax)
	}
	if c.Reflink != "always" && c.Reflink != "auto" {
		return fmt.Errorf("reflink must be always or auto")
	}
	if c.DiskEngine != "auto" && c.DiskEngine != "Async" && c.DiskEngine != "Sync" {
		return fmt.Errorf("diskEngine must be auto, Async or Sync")
	}
	if c.KSMTeam != "" && c.KSMTeam != "*" && !reTeam.MatchString(c.KSMTeam) {
		return fmt.Errorf("ksmTeam must be a team name or *")
	}
	if _, ok := c.Sizes[c.DefaultSize]; !ok {
		return fmt.Errorf("defaultSize %q not in sizes", c.DefaultSize)
	}
	for n, s := range c.Sizes {
		if s.VCPU < 1 || s.MemMiB < 128 || s.DiskGiB < 1 {
			return fmt.Errorf("size %s invalid", n)
		}
	}
	if c.MemOvercommit <= 0 || c.CPUOvercommit <= 0 || c.MinFreePct < 0 || c.MinFreePct > 90 || c.MinFreeGiB < 0 || c.ReapS < 1 {
		return fmt.Errorf("memOvercommit/cpuOvercommit must be > 0, minFreePct 0-90, minFreeGiB >= 0, reapIntervalS >= 1")
	}
	if c.StorageOvercommit < 0 {
		return fmt.Errorf("storageOvercommit must be >= 0")
	}
	if err := c.validateUntrusted(); err != nil {
		return err
	}
	if c.HostDirtyRatio < 0 || c.HostDirtyRatio > 90 {
		return fmt.Errorf("hostDirtyRatio must be 0-90")
	}
	for _, n := range []int{c.AIProxyPort, c.EgressMbit, c.IngressMbit, c.DiskMBps, c.DiskIOPS, c.IOMaxMBps, c.IOMaxIOPS, c.CtPerVM, c.MaxPortsPerVM, c.VMOverheadMiB, c.VMPageCacheMiB} {
		if n < 0 {
			return fmt.Errorf("limits must be >= 0")
		}
	}
	if strings.HasPrefix(c.CgroupParent, "/") || strings.Contains(c.CgroupParent, "..") {
		return fmt.Errorf("cgroupParent must be relative to /sys/fs/cgroup")
	}
	if !filepath.IsAbs(c.StateDir) || !filepath.IsAbs(c.RunDir) {
		return fmt.Errorf("stateDir and runDir must be absolute")
	}
	return nil
}

// validateUntrusted: untrustedTenants is a promise that these hold. The NixOS option
// sets them all; a config that overrides one is refused rather than quietly weaker.
func (c Config) validateUntrusted() error {
	if !c.UntrustedTenants {
		return nil
	}
	switch {
	case c.KSMTeam != "":
		return fmt.Errorf("untrustedTenants: ksmTeam is a side channel between guests")
	case c.WarmPool:
		return fmt.Errorf("untrustedTenants: warmPool gives tenants one template's ASLR")
	case c.Jailer == "":
		return fmt.Errorf("untrustedTenants: needs the jailer")
	case c.CgroupParent == "" || c.CgroupsOptional:
		return fmt.Errorf("untrustedTenants: needs cgroup limits")
	case !c.BtrfsQgroups:
		return fmt.Errorf("untrustedTenants: needs btrfsQgroups")
	case c.StorageOvercommit <= 0 || c.StorageOvercommit > 1:
		return fmt.Errorf("untrustedTenants: storageOvercommit must be in (0, 1]")
	case c.EgressMbit == 0 || c.IngressMbit == 0 || c.IOMaxMBps == 0 || c.IOMaxIOPS == 0 || c.CtPerVM == 0:
		return fmt.Errorf("untrustedTenants: egressMbit, ingressMbit, ioMaxMBps, ioMaxIOPS and ctPerVM must be set")
	}
	return nil
}

func (c Config) team(name string) TeamPolicy {
	if p, ok := c.Teams[name]; ok {
		return p
	}
	return c.DefaultTeam
}
