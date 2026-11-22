package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	if c, err := loadConfig(filepath.Join(dir, "missing.json")); err != nil || c.Sizes["xlarge"].VCPU != 16 || c.Bridge != "boatbr0" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	p := filepath.Join(dir, "c.json")
	os.WriteFile(p, []byte(`{"stateDir":"/srv/boat","ipv6Prefix":"","defaultSize":"s",
		"sizes":{"s":{"vcpu":1,"mem_mib":256,"disk_gib":2}},"defaultTeam":{"maxVMs":3,"allowIntraTeam":true}}`), 0o600)
	c, err := loadConfig(p)
	if err != nil || c.StateDir != "/srv/boat" || len(c.Sizes) != 1 || c.team("x").MaxVMs != 3 || !c.team("x").AllowIntraTeam {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{`{"subnet":"10.200.0.0/24"}`, `{"ipv6Prefix":"10.0.0.0/8"}`, `{"dns":["x"]}`,
		`{"portMin":100}`, `{"hostHttpsPort":30000}`, `{"hostHttpsPort":-1}`, `{"reflink":"never"}`, `{"defaultSize":"nope"}`, `{"stateDir":"rel"}`, `{`} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := loadConfig(p); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestJailerMemoryLimits(t *testing.T) {
	c := defaultConfig()
	c.cgroupLimits = true
	args := strings.Join(jailerArgs(c, VM{ID: "vm-x", MemMiB: 2048, VCPU: 2}, 900001, 900001, "/d"), " ")
	for _, want := range []string{"memory.high=2415919104", "memory.max=2952790016", "cpu.max=300000 100000"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %s in %s", want, args)
		}
	}
	c.cgroupLimits = false
	if args := strings.Join(jailerArgs(c, VM{ID: "vm-x", MemMiB: 2048}, 1, 1, "/d"), " "); strings.Contains(args, "memory.") {
		t.Fatalf("limits without delegated controllers: %s", args)
	}
}

func TestSetupBridge(t *testing.T) {
	h := newFakeHost()
	c := defaultConfig()
	c.IPv6Prefix = "2001:db8:1::/64"
	if err := setupBridge(c, h.run); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(h.cmds, "\n")
	for _, want := range []string{"ip addr replace 10.200.0.1/16 dev boatbr0", "ip -6 addr replace 2001:db8:1::1/64 dev boatbr0",
		"sysctl -qw net.ipv6.conf.all.forwarding=1"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
}

func TestListenSocket(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "bs")
	defer os.RemoveAll(dir)
	c := defaultConfig()
	c.Socket = filepath.Join(dir, "run", "boatd.sock")
	ln, err := listenSocket(c)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if st, _ := os.Stat(c.Socket); st.Mode().Perm() != 0o660 {
		t.Fatalf("mode %v", st.Mode())
	}
	if conn, err := net.Dial("unix", c.Socket); err != nil {
		t.Fatal(err)
	} else {
		conn.Close()
	}
}

func TestEnableIOCost(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "io.cost.qos"), nil, 0o600) // cgroupfs has them already
	os.WriteFile(filepath.Join(dir, "io.cost.model"), nil, 0o600)
	if err := enableIOCost(dir, "", "", ""); err == nil {
		t.Fatal("unknown disk accepted")
	}
	if err := enableIOCost(dir, "259:0", "rbps=3000000000 rrandiops=400000", ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "io.cost.model")); string(b) != "259:0 ctrl=user model=linear rbps=3000000000 rrandiops=400000" {
		t.Fatalf("io.cost.model = %q", b)
	}
	c := defaultConfig()
	c.IOCostModel = "rbps=1; echo"
	if c.validate() == nil {
		t.Fatal("ioCostModel with junk accepted")
	}
	c = defaultConfig()
	c.IOCostQoS = "rpct=95 enable=0"
	if c.validate() == nil {
		t.Fatal("ioCostQoS with another key accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "io.cost.qos")); string(b) != "259:0 enable=1" {
		t.Fatalf("io.cost.qos = %q", b)
	}
	if err := enableIOCost(dir, "259:0", "", "rpct=95 rlat=5000 wpct=95 wlat=10000 min=25 max=400"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "io.cost.qos")); string(b) != "259:0 enable=1 ctrl=user rpct=95 rlat=5000 wpct=95 wlat=10000 min=25 max=400" {
		t.Fatalf("io.cost.qos with targets = %q", b)
	}
}

func TestHostHTTPSAddrsAndStandaloneSSH(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")

	// Default standaloneSSH is false and hostHttpsAddrs is empty.
	def, err := loadConfig(filepath.Join(dir, "nonexistent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if def.StandaloneSSH || len(def.HostHTTPSAddrs) != 0 {
		t.Fatalf("unexpected defaults: standaloneSSH=%v hostHttpsAddrs=%v", def.StandaloneSSH, def.HostHTTPSAddrs)
	}

	// Valid hostHttpsAddrs (IPv4 + IPv6) and standaloneSSH=true (when untrustedTenants=false):
	// hostHTTPSAllowlists honors IPv4 entries literally without injecting 10.200.0.1 unless configured.
	os.WriteFile(p, []byte(`{"standaloneSSH":true,"hostHttpsAddrs":["203.0.113.10","198.51.100.0/24","2001:db8::1","2001:db8:2::/64","fd7a:115c:a1e0::1"]}`), 0o600)
	c, err := loadConfig(p)
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if !c.StandaloneSSH {
		t.Fatal("standaloneSSH not loaded")
	}
	v4, v6 := c.hostHTTPSAllowlists()
	if got, want := strings.Join(v4, ","), "203.0.113.10,198.51.100.0/24"; got != want {
		t.Fatalf("hostHTTPSAllowlists v4 = %q, want %q", got, want)
	}
	if got, want := strings.Join(v6, ","), "2001:db8::1,2001:db8:2::/64,fd7a:115c:a1e0::1"; got != want {
		t.Fatalf("hostHTTPSAllowlists v6 = %q, want %q", got, want)
	}

	// Explicit 10.200.0.1 is honored in literal order and deduplicated.
	os.WriteFile(p, []byte(`{"hostHttpsAddrs":["203.0.113.10","10.200.0.1","198.51.100.0/24","10.200.0.1"]}`), 0o600)
	cGw, err := loadConfig(p)
	if err != nil {
		t.Fatalf("valid explicit gateway config rejected: %v", err)
	}
	if v4Gw, v6Gw := cGw.hostHTTPSAllowlists(); strings.Join(v4Gw, ",") != "203.0.113.10,10.200.0.1,198.51.100.0/24" || len(v6Gw) != 0 {
		t.Fatalf("explicit gateway hostHTTPSAllowlists = (%v, %v)", v4Gw, v6Gw)
	}

	// IPv6-only hostHttpsAddrs emits only IPv6 allowlist entries (no automatic IPv4 gateway).
	os.WriteFile(p, []byte(`{"hostHttpsAddrs":["2001:db8::1","2001:db8:2::/64"]}`), 0o600)
	c6, err := loadConfig(p)
	if err != nil {
		t.Fatalf("valid IPv6-only hostHttpsAddrs rejected: %v", err)
	}
	if v4Only, v6Only := c6.hostHTTPSAllowlists(); len(v4Only) != 0 || strings.Join(v6Only, ",") != "2001:db8::1,2001:db8:2::/64" {
		t.Fatalf("IPv6-only hostHTTPSAllowlists = (%v, %v)", v4Only, v6Only)
	}

	// Invalid hostHttpsAddrs entries must be rejected.
	for _, bad := range []string{
		`""`,
		`" 203.0.113.10"`,
		`"203.0.113.10 "`,
		`"203.0.113.10:443"`,
		`"[2001:db8::1]:443"`,
		`"2001:0db8::1"`,
		`"0.0.0.0"`,
		`"::"`,
		`"127.0.0.1"`,
		`"::1"`,
		`"127.0.0.0/8"`,
		`"0.0.0.0/0"`,
		`"::/0"`,
		`"224.0.0.1"`,
		`"ff02::1"`,
		`"255.255.255.255"`,
		`"::ffff:203.0.113.10"`,
		`"198.51.100.5/24"`,
		`"2001:db8::1/64"`,
		`"203.0.113.10/33"`,
		`"2001:db8::/129"`,
		`"203.0.113.10; drop"`,
		`"api.example.com"`,
	} {
		os.WriteFile(p, []byte(`{"hostHttpsAddrs":[`+bad+`]}`), 0o600)
		if _, err := loadConfig(p); err == nil {
			t.Errorf("invalid hostHttpsAddrs %s accepted", bad)
		}
	}

	// standaloneSSH cannot be enabled when untrustedTenants is true.
	u := defaultConfig()
	u.UntrustedTenants = true
	u.StandaloneSSH = true
	err = u.validate()
	if err == nil || !strings.Contains(err.Error(), "standaloneSSH cannot be enabled when untrustedTenants is true") {
		t.Fatalf("standaloneSSH + untrustedTenants error = %v", err)
	}
}
