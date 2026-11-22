package main

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
)

// Create from a memory snapshot with fresh_boot cold boots and carries no memory.
func TestR39CreateFromSnapshotFreshBootColdBoots(t *testing.T) {
	m, h := newTestManager(t, nil)
	ctx := context.Background()
	src, err := m.Create(ctx, CreateReq{Team: "t1", Name: "src"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.CreateSnapshot(ctx, src.ID, "snap")
	if err != nil || !s.HasMem {
		t.Fatalf("snapshot: %+v %v", s, err)
	}

	warm, err := m.Create(ctx, CreateReq{Team: "t1", Name: "warm", Snapshot: s.ID})
	if err != nil || !h.fcOf(t, m, warm.ID).called("PUT /snapshot/load") {
		t.Fatalf("control: a plain create must restore memory: %v", err)
	}

	v, err := m.Create(ctx, CreateReq{Team: "t1", Name: "fresh", Snapshot: s.ID, FreshBoot: true})
	if err != nil || v.State != Running || v.FromSnapshot != s.ID {
		t.Fatalf("fresh create: %+v %v", v, err)
	}
	fc := h.fcOf(t, m, v.ID)
	if fc.called("PUT /snapshot/load") || !fc.called("PUT /actions") {
		t.Fatalf("fresh_boot must cold boot: %v", fc.calls)
	}
	root := m.root(v.ID)
	for _, f := range []string{vmstate, memFile} {
		if exists(filepath.Join(root, f)) {
			t.Fatalf("%s was copied into the fresh VM", f)
		}
	}
	if !exists(filepath.Join(root, diskFile)) {
		t.Fatal("disk missing")
	}
	// the snapshot itself is untouched.
	if !exists(filepath.Join(m.snapDir(s.ID), memFile)) {
		t.Fatal("snapshot lost its memory")
	}
}

func TestR39FreshBootStartOnRunningVMIs409(t *testing.T) {
	m, _ := newTestManager(t, nil)
	v, err := m.Create(context.Background(), CreateReq{Name: "r"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.startVM(context.Background(), v.ID, nil, nil, true)
	ae, ok := err.(*apiErr)
	if !ok || ae.code != http.StatusConflict {
		t.Fatalf("want 409, got %v", err)
	}
	if got, _ := m.get(v.ID); got.State != Running || got.PID != v.PID {
		t.Fatalf("vm changed: %+v", got)
	}
}
