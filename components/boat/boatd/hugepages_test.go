package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestHugePages(t *testing.T) {
	if mc := machineConfig(VM{VCPU: 2, MemMiB: 1024}, false, Config{}); mc["huge_pages"] != nil {
		t.Errorf("huge pages without the option: %v", mc)
	}
	if mc := machineConfig(VM{VCPU: 2, MemMiB: 1024}, false, Config{HugePages: true}); mc["huge_pages"] != "2M" {
		t.Errorf("hugePages: %v", mc)
	}
	c := defaultConfig()
	c.HugePages = true
	if c.validate() == nil {
		t.Error("hugePages with the balloon accepted")
	}
	m, _ := newTestManager(t, func(c *Config) { c.HugePages, c.Balloon, c.DiffSnapshots = true, false, false })
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{Team: "t"})
	if err != nil {
		t.Fatal(err)
	}
	var api *apiErr
	if _, err := m.Fork(ctx, v.ID, "", "", nil); !errors.As(err, &api) || api.code != 409 {
		t.Errorf("fork of a huge-page VM: %v", err)
	}
	if _, err := m.CreateSnapshot(ctx, v.ID, "s"); !errors.As(err, &api) || api.code != 409 {
		t.Errorf("snapshot of a huge-page VM: %v", err)
	}
	if v, err = m.Stop(ctx, v.ID, "test"); err != nil || v.HasSnapshot {
		t.Fatalf("stop: %v, snapshot %v (Firecracker could not restore it)", err, v.HasSnapshot)
	}
	if v, err = m.Start(ctx, v.ID, nil); err != nil || v.State != Running {
		t.Fatalf("cold start: %v %s", err, v.State)
	}
}

// The kernel lets a process open a tap only if its euid is the tap's user AND its gid
// the tap's group: with a shared jailGid, that gid must be the tap's group.
func TestTapOwnedByVMMGid(t *testing.T) {
	var cmds []string
	run := func(_, name string, args ...string) error {
		cmds = append(cmds, name+" "+strings.Join(args, " "))
		return nil
	}
	if err := addVMNet(Config{Bridge: "boatbr0"}, run, VM{Index: 7, Tap: "bt7"}, 900007, 899999); err != nil {
		t.Fatal(err)
	}
	want := "ip -n boat7 tuntap add dev tap0 mode tap user 900007 group 899999"
	for _, c := range cmds {
		if c == want {
			return
		}
	}
	t.Errorf("no %q in %q", want, cmds)
}
