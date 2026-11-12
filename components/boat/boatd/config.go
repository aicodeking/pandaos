package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
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
	// JailGID > 0: every jailed VMM runs with this gid instead of base+n (uids stay per VM;
	// each chroot is 0700, so the shared gid opens no file of another VM). The NixOS module
	// sets it with kernel.io_uring_group under untrustedTenants: only VMMs may use io_uring.
	JailGID     int    `json:"jailGid"`
	RoutesGroup string `json:"routesGroup"` // group that may read routes.json (mode 0640); "" = socketGroup
	// UnpackSandbox: run the snapshot-import decompressor as a transient systemd unit with no
	// network and a syscall filter (systemd-run). Off: a fresh network namespace as nobody.
	UnpackSandbox bool     `json:"unpackSandbox"`
	FCUID         int      `json:"fcUid"` // no-jailer mode: uid/gid firecracker runs as (0 = boatd's)
	FCGID         int      `json:"fcGid"`
	Reflink       string   `json:"reflink"` // cp --reflink=<always|auto>
	Bridge        string   `json:"bridge"`
	Subnet        string   `json:"subnet"` // IPv4 /16; .0.1 is the gateway
	DNS           []string `json:"dns"`
	IPv6Prefix    string   `json:"ipv6Prefix"` // routed prefix, e.g. 2001:db8:1::/64; VM n gets prefix::n/128
	PublicIPv4    []string `json:"publicIPv4"` // 1:1 NAT pool, routed to this host
	PortMin       int      `json:"portMin"`
	PortMax       int      `json:"portMax"`
	DesktopPort   int      `json:"desktopPort"` // guest port the gateway proxies the desktop to

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
	DiskCache      string `json:"diskCache"`      // Firecracker cache_type: Unsafe (guest flushes ignored) or Writeback (honoured)
	DiskNoCow      bool   `json:"diskNoCow"`      // btrfs: images and the VM disks reflinked from them are NOCOW
	Balloon        bool   `json:"balloon"`        // virtio-balloon: free page reporting, deflate on OOM
	HostDirtyRatio int    `json:"hostDirtyRatio"` // vm.dirty_ratio set at startup, 0 = leave the host's: a dump must not be throttled to the disk
	WarmPool       bool   `json:"warmPool"`       // create restores a booted template of the image + size
	KSMTeam        string `json:"ksmTeam"`        // "" off; one team's VMs may merge; "*" all (one trust domain only)
	VCPUCPUWeight  int    `json:"vcpuCpuWeight"`  // cpu.weight of <vm>/vcpu, the VM's vCPU threads apart from the VMM thread (vcpucg.go); 0 = one cgroup
	CPUTraceS      int    `json:"cpuTraceS"`      // log each running VM's per-thread CPU and PSI every N s; 0 = off
	// HugePages: guest memory on 2 MiB hugetlbfs pages (Firecracker huge_pages). Needs host
	// hugepages (vm.nr_hugepages / nr_overcommit_hugepages), balloon, diffSnapshots and warmPool
	// off, and Firecracker cannot restore such a VM from a memory file: stop/start and forks
	// cold-boot from the disk instead (lifecycle.go falls back).
	HugePages bool `json:"hugePages"`

	// Egress policy.
	AllowSMTP   bool `json:"allowSMTP"`   // else tcp 25/465/587 out is dropped
	AIProxyPort int  `json:"aiProxyPort"` // gateway port VMs may reach (boat-api's AI proxy); 0 = closed
	EgressMbit  int  `json:"egressMbit"`  // per-VM egress cap (bridge policer); 0 = none
	// UplinkSharePct > 0: an unset egressMbit/ingressMbit becomes this share of the uplink's
	// speed (the default route's NIC, /sys/class/net/<dev>/speed; 1000 Mbit when unknown).
	UplinkSharePct int `json:"uplinkSharePct"`
	// UplinkFairQueue: cake on the uplink's egress with `nat dual-srchost`, so VMs share the
	// port by source host (each VM, before masquerade) and then by flow.
	UplinkFairQueue bool `json:"uplinkFairQueue"`
	// HostHTTPSPort: tcp port VMs may reach on any host-local address (Caddy's
	// 443, so boat-api at its public URL, BOAT_URL, works from a guest on a
	// single-host install); 0 = closed. Nothing else on the host is opened.
	HostHTTPSPort int `json:"hostHttpsPort"`

	// Per-VM limits. 0 = none. Firecracker limiters apply from the next cold boot
	// (a restored VM keeps those saved in its snapshot).
	IngressMbit int  `json:"ingressMbit"` // Firecracker rx rate limiter
	DiskMBps    int  `json:"diskMBps"`    // Firecracker drive rate limiter
	DiskIOPS    int  `json:"diskIOPS"`    // ... operations
	IOMaxMBps   int  `json:"ioMaxMBps"`   // cgroup io.max on stateDir's disk (rbps+wbps)
	IOMaxIOPS   int  `json:"ioMaxIOPS"`   // ... riops+wiops
	IOCost      bool `json:"ioCost"`      // blk-iocost on stateDir's disk: busy VMs share it by io.weight (equal), idle capacity is free to take
	// io.cost.model for that disk, e.g. "rbps=3e9 rseqiops=400000 rrandiops=400000 wbps=2e9 wseqiops=300000 wrandiops=300000"
	// (iocost_coef_gen.py or fio on the disk). Empty: the kernel's default, which caps an SSD near 8.5k rand read IOPS.
	IOCostModel string `json:"ioCostModel"`
	// io.cost.qos targets, e.g. "rpct=95 rlat=5000 wpct=95 wlat=10000 min=25 max=400": iocost lowers the
	// disk's virtual rate when p95 latency misses them, so a flood cannot starve a VM that flushes. Empty: none.
	IOCostQoS     string `json:"ioCostQoS"`
	CtPerVM       int    `json:"ctPerVM"`       // new forwarded connections refused past this many tracked
	MaxPortsPerVM int    `json:"maxPortsPerVM"` // routes + public ports of one VM
	ResumeCrashed bool   `json:"resumeCrashed"` // cold-boot VMs that were running when boatd/the host died

	IndexQuarantineS int `json:"indexQuarantineS"` // a deleted VM's IP/MAC/uid is not reused for this long (if another is free); 0 = off

	cgroupLimits bool                  // set at startup when the cgroup controllers could be delegated
	stateDisk    string                // "major:minor" of stateDir's disk, for io.max
	routesGID    int                   // routesGroup's gid, set at startup
	Teams        map[string]TeamPolicy `json:"teams"`
	DefaultTeam  TeamPolicy            `json:"defaultTeam"`
}

// defaultBootArgs: quiet, because every kernel line goes out through the emulated
// 8250 at boot: the guest kernel spent 96 ms flushing its log when the console came
// up (AX42, 4 vCPU; 420 ms kernel). Warnings, errors and panics still reach
// firecracker.log, and the guest's own writes to /dev/ttyS0 are untouched.
const defaultBootArgs = "console=ttyS0 quiet reboot=k panic=1 pci=off root=/dev/vda rw"

var (
	reIOCostQoS   = regexp.MustCompile(`^((rpct|rlat|wpct|wlat|min|max)=[0-9]+(\.[0-9]+)? ?)+$`)
	reIOCostModel = regexp.MustCompile(`^((rbps|rseqiops|rrandiops|wbps|wseqiops|wrandiops)=[0-9]+ ?)+$`)
	reName        = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reTeam        = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	reEnv         = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	reLabel       = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,63}$`)
	reImage       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
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
		VCPUCPUWeight:     100,
		PortMin:           20000,
		PortMax:           40000,
		DesktopPort:       6080,
		DefaultSize:       "default",
		DefaultImage:      "ubuntu-24.04",
		BootArgs:          defaultBootArgs,
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
		DiskCache:         "Unsafe",
		DiskNoCow:         true,
		Balloon:           true,
		HostDirtyRatio:    50,
		CtPerVM:           4096, // nf_conntrack_max is about 260k on 16 GiB, or the module's 1M
		MaxPortsPerVM:     32,
		ResumeCrashed:     true,
		IndexQuarantineS:  600, // boat-api's jump grants follow deletes through events: minutes when it reconnects
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
	if c.IOCost && c.IOCostModel == "" {
		c.IOCostModel = storedIOCostModel(c.StateDir) // boatd -measure-disk
	}
	if c.IOCost && c.IOCostModel == "" {
		// A fresh host before (or after a failed) boat-iocost-model: start anyway. iocost
		// stays off and each VM is held to the io.max ceilings (ioMaxMBps/ioMaxIOPS, which
		// untrustedTenants requires); busy VMs then share the disk first come, first served.
		log.Printf("ioCost: no ioCostModel, disk shared under io.max ceilings only; run `%s -config %s` once (about 25 s of direct I/O; it writes %s), or set ioCostModel (NixOS: services.boat.ioCostModel; the boat-iocost-model unit measures before boatd)",
			measureCmd, path, filepath.Join(c.StateDir, ioCostModelFile))
	}
	if err := c.validate(); err != nil {
		return c, err
	}
	return c, nil
}

// readConfig: the file over the defaults, not validated (boatd -measure-disk needs
// only stateDir, and runs exactly when the config lacks a model).
func readConfig(path string) (Config, error) {
	c := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return c, err
	}
	if err == nil {
		if err := json.Unmarshal(data, &c); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
	}
	if c.StateDir == "" {
		return c, errors.New("no stateDir")
	}
	return c, nil
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
	if c.IOCostQoS != "" && !reIOCostQoS.MatchString(c.IOCostQoS) {
		return fmt.Errorf("ioCostQoS: rpct= rlat= wpct= wlat= min= max= with numbers")
	}
	if c.IOCostModel != "" && !reIOCostModel.MatchString(c.IOCostModel) {
		return fmt.Errorf("ioCostModel: rbps= rseqiops= rrandiops= wbps= wseqiops= wrandiops= with integers")
	}
	if c.DiskCache != "Unsafe" && c.DiskCache != "Writeback" {
		return fmt.Errorf("diskCache must be Unsafe or Writeback")
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
	if c.HugePages && (c.Balloon || c.DiffSnapshots || c.WarmPool) {
		return fmt.Errorf("hugePages needs balloon, diffSnapshots and warmPool off (Firecracker supports none of them with huge pages)")
	}
	if c.UplinkSharePct < 0 || c.UplinkSharePct > 100 || c.JailGID < 0 {
		return fmt.Errorf("uplinkSharePct must be 0-100 and jailGid >= 0")
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
	if c.VCPUCPUWeight < 0 || c.VCPUCPUWeight > 10000 || c.CPUTraceS < 0 {
		return fmt.Errorf("vcpuCpuWeight must be 0-10000 and cpuTraceS >= 0")
	}
	if c.HostHTTPSPort < 0 || c.HostHTTPSPort > 65535 || (c.HostHTTPSPort >= c.PortMin && c.HostHTTPSPort <= c.PortMax) {
		return fmt.Errorf("hostHttpsPort must be 0-65535 and outside portMin-portMax")
	}
	for _, n := range []int{c.AIProxyPort, c.EgressMbit, c.IngressMbit, c.DiskMBps, c.DiskIOPS, c.IOMaxMBps, c.IOMaxIOPS, c.CtPerVM, c.MaxPortsPerVM, c.VMOverheadMiB, c.VMPageCacheMiB, c.IndexQuarantineS} {
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

// untrustedRule: one key of untrusted.json, the settings services.boat.untrustedTenants
// sets (module.nix imports that file). Each key can be raised, never weakened: weak is a
// value that must be refused (config_test.go applies it to every key in the file).
type untrustedRule struct {
	key  string
	weak any
	ok   func(Config) bool
	why  string
}

var untrustedRules = []untrustedRule{
	{"btrfsQgroups", false, func(c Config) bool { return c.BtrfsQgroups }, "each team's data needs a quota"},
	{"storageOvercommit", 2, func(c Config) bool { return c.StorageOvercommit > 0 && c.StorageOvercommit <= 1 },
		"must be in (0, 1]: committed storage fits the disk, so no tenant can fill it"},
	{"memOvercommit", 1.5, func(c Config) bool { return c.MemOvercommit <= 1 }, "must be <= 1: guests' RAM fits the host's"},
	{"uplinkSharePct", 0, func(c Config) bool {
		return (c.UplinkSharePct > 0 && c.UplinkSharePct <= 100) || (c.EgressMbit > 0 && c.IngressMbit > 0)
	}, "needs a per-VM bandwidth cap: uplinkSharePct 1-100, or egressMbit and ingressMbit"},
	{"uplinkFairQueue", false, func(c Config) bool { return c.UplinkFairQueue }, "the uplink must be shared fairly between VMs"},
	{"ioCost", false, func(c Config) bool { return c.IOCost }, "busy VMs share the disk by weight, see PROTOCOL.md \"Disk: measured\""},
	{"ioCostQoS", "", func(c Config) bool { return c.IOCostQoS != "" }, "latency targets keep a flood from starving a VM that flushes"},
	{"ioMaxMBps", 0, func(c Config) bool { return c.IOMaxMBps > 0 }, "a per-VM disk bandwidth ceiling is needed"},
	{"ioMaxIOPS", 0, func(c Config) bool { return c.IOMaxIOPS > 0 }, "a per-VM disk IOPS ceiling is needed"},
	{"ctPerVM", 0, func(c Config) bool { return c.CtPerVM > 0 }, "one VM must not fill the conntrack table"},
	{"hostDirtyRatio", 50, func(c Config) bool { return c.HostDirtyRatio > 0 && c.HostDirtyRatio <= 20 },
		"must be 1-20: a tenant must not dirty half of the host's RAM"},
	{"allowSMTP", true, func(c Config) bool { return !c.AllowSMTP }, "tenants would send spam from this host's address"},
	{"warmPool", true, func(c Config) bool { return !c.WarmPool }, "gives tenants one template's ASLR"},
	{"ksmTeam", "*", func(c Config) bool { return c.KSMTeam == "" }, "is a side channel between guests"},
}

// validateUntrusted: untrustedTenants is a promise that these hold. The NixOS option
// sets them all; a config that overrides one is refused rather than quietly weaker.
func (c Config) validateUntrusted() error {
	if !c.UntrustedTenants {
		return nil
	}
	for _, r := range untrustedRules {
		if !r.ok(c) {
			return fmt.Errorf("untrustedTenants: %s %s", r.key, r.why)
		}
	}
	switch {
	case c.Jailer == "":
		return fmt.Errorf("untrustedTenants: needs the jailer")
	case c.CgroupParent == "" || c.CgroupsOptional:
		return fmt.Errorf("untrustedTenants: needs cgroup limits")
	case c.HugePages:
		return fmt.Errorf("untrustedTenants: hugePages memory is outside the VMs' cgroup memory limits")
	}
	return nil
}

func (c Config) team(name string) TeamPolicy {
	if p, ok := c.Teams[name]; ok {
		return p
	}
	return c.DefaultTeam
}
