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
		`{"portMin":100}`, `{"reflink":"never"}`, `{"defaultSize":"nope"}`, `{"stateDir":"rel"}`, `{`} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := loadConfig(p); err == nil {
			t.Errorf("%s: accepted", bad)
		}
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
