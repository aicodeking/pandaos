package main

import (
	"context"
	"errors"
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
	if _, err := m.Fork(ctx, v.ID, "", ""); !errors.As(err, &api) || api.code != 409 {
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
