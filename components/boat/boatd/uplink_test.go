package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSetupUplink(t *testing.T) {
	dir := t.TempDir()
	route := filepath.Join(dir, "route")
	os.WriteFile(route, []byte("Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\n"+
		"boatbr0\t0000C80A\t00000000\t0001\t0\t0\t0\t0000FFFF\n"+
		"wlan0\t00000000\t0101A8C0\t0003\t0\t0\t600\t00000000\n"+
		"enp1s0\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\n"), 0o644)
	sysNet := filepath.Join(dir, "net")
	os.MkdirAll(filepath.Join(sysNet, "enp1s0"), 0o755)
	os.WriteFile(filepath.Join(sysNet, "enp1s0", "speed"), []byte("1000\n"), 0o644)

	var cmds []string
	run := func(_, name string, args ...string) error {
		cmds = append(cmds, name+" "+strings.Join(args, " "))
		return nil
	}
	c := Config{UplinkSharePct: 25, UplinkFairQueue: true, IngressMbit: 900}
	if err := setupUplink(&c, run, route, sysNet); err != nil {
		t.Fatal(err)
	}
	if c.EgressMbit != 250 || c.IngressMbit != 900 {
		t.Errorf("caps %d/%d, want 250 (25%% of 1 Gbit) and the explicit 900", c.EgressMbit, c.IngressMbit)
	}
	if len(cmds) != 1 || cmds[0] != "tc qdisc replace dev enp1s0 root cake unlimited besteffort dual-srchost nat" {
		t.Errorf("commands %q", cmds)
	}

	// No cake: fq_codel. A NIC without a speed: 1 Gbit.
	os.Remove(filepath.Join(sysNet, "enp1s0", "speed"))
	cmds = nil
	run = func(_, name string, args ...string) error {
		cmds = append(cmds, name+" "+strings.Join(args, " "))
		if strings.Contains(strings.Join(args, " "), "cake") {
			return errors.New("unknown qdisc")
		}
		return nil
	}
	c = Config{UplinkSharePct: 10, UplinkFairQueue: true}
	if err := setupUplink(&c, run, route, sysNet); err != nil || c.EgressMbit != 100 || len(cmds) != 2 || !strings.HasSuffix(cmds[1], "root fq_codel") {
		t.Errorf("fallback: %v, egress %d, %q", err, c.EgressMbit, cmds)
	}
	// Nothing asked: nothing read or run.
	c = Config{}
	if err := setupUplink(&c, run, filepath.Join(dir, "missing"), sysNet); err != nil {
		t.Error(err)
	}
}

// routes.json lists every VM's IP and team: 0640, group routesGroup, from the first write.
func TestRoutesFileMode(t *testing.T) {
	c := Config{RunDir: t.TempDir(), routesGID: os.Getgid()}
	for i := 0; i < 2; i++ { // the second write replaces the first through the tmp file
		if err := writeRoutes(c, []VM{{ID: "vm-a", IP: "10.200.0.2", State: Running, Team: "t"}}); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Stat(filepath.Join(c.RunDir, "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want 0640", fi.Mode().Perm())
	}
	if gid := statGID(fi); gid != os.Getgid() {
		t.Errorf("group %d, want %d", gid, os.Getgid())
	}
}

func statGID(fi os.FileInfo) int { return int(fi.Sys().(*syscall.Stat_t).Gid) }

// The import decompressor parses tenant bytes: under unpackSandbox it runs in a transient
// unit with no network and a syscall filter; otherwise as nobody (in its own netns on Linux).
func TestUnpackCommand(t *testing.T) {
	argv, attr, err := unpackCommand(Config{UnpackSandbox: true}, []string{"sh", "-c", "x"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"systemd-run ", "--pipe", "--wait", "-p User=nobody", "-p PrivateNetwork=yes",
		"-p SystemCallFilter=~@network-io", "-p SystemCallFilter=@system-service", "-p CapabilityBoundingSet="} {
		if !strings.Contains(joined, want) {
			t.Errorf("sandboxed argv lacks %q: %s", want, joined)
		}
	}
	if attr != nil || !strings.HasSuffix(joined, "/sh -c x") {
		t.Errorf("sandboxed: attr %v, argv %s", attr, joined)
	}
	if argv, attr, _ := unpackCommand(Config{UnpackSandbox: true}, []string{"sh"}, 1000); argv[0] != "sh" || attr != nil {
		t.Errorf("not root: %v %v", argv, attr)
	}
	if _, _, err := unpackCommand(Config{UnpackSandbox: true}, []string{"no-such-decompressor"}, 0); err == nil {
		t.Error("missing binary accepted")
	}
	argv, attr, _ = unpackCommand(Config{}, []string{"zstd", "-dcq"}, 0)
	if argv[0] != "zstd" || attr == nil || attr.Credential.Uid != nobodyID {
		t.Errorf("plain root: %v %+v", argv, attr)
	}
}
