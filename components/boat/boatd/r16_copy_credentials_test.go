package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestR16PreparedCopiesStayClosedUntilRelease(t *testing.T) {
	m, h := newTestManager(t, nil)
	p, err := m.Create(context.Background(), CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	var child, snap map[string]any
	var raw []byte
	if code := call(t, srv, "POST", "/v1/vms/"+p.ID+"/fork/prepare", map[string]any{"team": "t1"}, &raw); code != 201 {
		t.Fatalf("prepare fork: %d %s", code, raw)
	}
	if err := json.Unmarshal(raw, &child); err != nil {
		t.Fatal(err)
	}
	cid := child["id"].(string)
	if child["pending_copy"] != true || child["pid"] != nil || child["state"] == "running" {
		t.Fatalf("prepared child executed: %v", child)
	}
	for _, op := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/vms/" + cid, nil},
		{"GET", "/v1/vms/" + cid + "/env", nil},
		{"POST", "/v1/vms/" + cid + "/start", nil},
		{"POST", "/v1/vms/" + cid + "/exec", map[string]any{"cmd": []string{"true"}}},
		{"POST", "/v1/vms/" + cid + "/fork", nil},
		{"POST", "/v1/vms/" + cid + "/snapshots", nil},
	} {
		if code := call(t, srv, op.method, op.path, op.body, nil); code/100 == 2 {
			t.Errorf("pending child accessible: %s %s", op.method, op.path)
		}
	}
	var visible struct{ VMs []VM }
	call(t, srv, "GET", "/v1/vms?team=t1", nil, &visible)
	if len(visible.VMs) != 1 {
		t.Fatalf("pending child listed: %v", visible.VMs)
	}
	call(t, srv, "GET", "/v1/vms?team=t1&include_pending=true", nil, &visible)
	if len(visible.VMs) != 2 {
		t.Fatal("cleanup/quota listing lost pending child")
	}
	if code := call(t, srv, "POST", "/v1/vms/"+p.ID+"/snapshots/prepare", nil, &snap); code != 201 {
		t.Fatalf("prepare snapshot: %d %v", code, snap)
	}
	sid := snap["id"].(string)
	var snapshots struct{ Snapshots []Snapshot }
	call(t, srv, "GET", "/v1/vms/"+p.ID+"/snapshots", nil, &snapshots)
	if len(snapshots.Snapshots) != 0 {
		t.Fatal("pending snapshot listed on VM")
	}
	call(t, srv, "GET", "/v1/snapshots?team=t1", nil, &snapshots)
	if len(snapshots.Snapshots) != 0 {
		t.Fatal("pending team snapshot listed")
	}
	call(t, srv, "GET", "/v1/snapshots?team=t1&include_pending=true", nil, &snapshots)
	if len(snapshots.Snapshots) != 1 {
		t.Fatal("cleanup listing lost pending snapshot")
	}
	if code := call(t, srv, "GET", "/v1/vms/"+p.ID+"/snapshots/"+sid+"/download", nil, nil); code != 404 {
		t.Fatalf("pending export: %d", code)
	}
	for _, count := range []int{1, 2} {
		if code := call(t, srv, "POST", "/v1/vms", map[string]any{"team": "t1", "snapshot": sid, "count": count}, nil); code != 404 {
			t.Errorf("pending restore count%d: %d", count, code)
		}
	}
	// Reload preserves the fence, including a lost prepare response.
	m2, err := newManager(m.cfg, h.run, h)
	if err != nil {
		t.Fatal(err)
	}
	srv2 := httptest.NewServer(m2.routes())
	defer srv2.Close()
	if code := call(t, srv2, "POST", "/v1/vms/"+cid+"/start", nil, nil); code/100 == 2 {
		t.Fatal("restart released pending child")
	}
	var released VM
	if code := call(t, srv2, "POST", "/v1/vms/"+cid+"/release", nil, &released); code != 200 || released.State != Running {
		t.Fatalf("release: %d %+v", code, released)
	}
	pid := released.PID
	if code := call(t, srv2, "POST", "/v1/vms/"+cid+"/release", nil, &released); code != 200 || released.PID != pid {
		t.Fatal("lost release response booted twice")
	}
	if code := call(t, srv2, "POST", "/v1/vms/"+p.ID+"/snapshots/"+sid+"/release", nil, nil); code != 200 {
		t.Fatalf("release snapshot: %d", code)
	}
	if _, err := m2.Create(context.Background(), CreateReq{Team: "t1", Snapshot: sid}); err != nil {
		t.Fatalf("released restore: %v", err)
	}
}

func TestR16GuestEnvironmentIsSeparateFromMetadata(t *testing.T) {
	m, _ := newTestManager(t, nil)
	vars := map[string]string{"BOAT_ID": "own", "BOAT_API_KEY": "user-key", "CUSTOM": "it's quoted\nnext line"}
	v, err := m.Create(context.Background(), CreateReq{Team: "t1", Env: vars})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	var raw []byte
	if code := call(t, srv, "GET", "/v1/vms/"+v.ID+"/env", nil, &raw); code != 200 {
		t.Fatalf("guest env getter: %d %s", code, raw)
	}
	var got struct {
		Vars map[string]string `json:"vars"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Vars) != len(vars) {
		t.Fatalf("guest environment: %v", got.Vars)
	}
	for k, want := range vars {
		if got.Vars[k] != want {
			t.Errorf("environment %s changed", k)
		}
	}
	var metadata map[string]any
	call(t, srv, "GET", "/v1/vms/"+v.ID, nil, &metadata)
	if _, leaked := metadata["env"]; leaked {
		t.Fatal("guest secrets leaked into VM metadata")
	}
}

func TestR16IncompleteCopyAndFailedReleaseRemainPending(t *testing.T) {
	m, h := newTestManager(t, nil)
	p, err := m.Create(context.Background(), CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	realSave := m.saveState
	m.mu.Lock()
	m.saveState = func(dir string, v VM) error {
		if v.CopyReady {
			return syscall.ENOSPC
		}
		return realSave(dir, v)
	}
	m.mu.Unlock()
	if _, err := m.fork(context.Background(), p.ID, "", "t1", nil, true); err == nil {
		t.Fatal("ready marker write failure ignored")
	}
	var pending VM
	for _, v := range m.list("") {
		if v.ID != p.ID {
			pending = v
		}
	}
	if !pending.PendingCopy || pending.CopyReady || pending.PID != 0 {
		t.Fatalf("incomplete copy escaped: %+v", pending)
	}
	if _, err := m.releaseCopy(context.Background(), pending.ID, nil); err == nil {
		t.Fatal("incomplete capture released")
	}
	m.mu.Lock()
	m.saveState = realSave
	m.mu.Unlock()
	if _, err := m.Delete(context.Background(), pending.ID); err != nil {
		t.Fatal(err)
	}
	c, err := m.fork(context.Background(), p.ID, "", "t1", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	// A real publication write failure must not change the in-memory pending fence.
	dir := m.vmDir(c.ID)
	if err := os.Rename(dir, dir+"-saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	_, releaseErr := m.releaseCopy(context.Background(), c.ID, nil)
	os.Remove(dir)
	if err := os.Rename(dir+"-saved", dir); err != nil {
		t.Fatal(err)
	}
	if releaseErr == nil {
		t.Fatal("release persistence failure ignored")
	}
	v, _ := m.get(c.ID)
	if !v.PendingCopy || v.PID != 0 {
		t.Fatal("failed release ran child")
	}
	h.mu.Lock()
	starts := h.starts
	h.mu.Unlock()
	m2, err := newManager(m.cfg, h.run, h)
	if err != nil {
		t.Fatal(err)
	}
	m2.resumeAll(context.Background())
	v, _ = m2.get(c.ID)
	if !v.PendingCopy || v.PID != 0 {
		t.Fatal("restart opened failed release")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.starts != starts {
		t.Fatal("startup automatically ran pending VM")
	}
}

func TestR16SnapshotSyncKeepsCurrentAuthorization(t *testing.T) {
	m, _ := newTestManager(t, nil)
	p, err := m.Create(context.Background(), CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.createSnapshot(context.Background(), p.ID, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.releaseSnapshot(p.ID, s.ID); err != nil {
		t.Fatal(err)
	}
	m.syncSaved(s) // the background worker captured the old pending metadata
	m.mu.Lock()
	cur := m.snaps[s.ID]
	m.mu.Unlock()
	var disk Snapshot
	if err := readJSON(filepath.Join(m.snapDir(s.ID), "snap.json"), &disk); err != nil {
		t.Fatal(err)
	}
	if cur.PendingCopy || disk.PendingCopy || cur.Unsynced || disk.Unsynced {
		t.Fatalf("stale sync overwrote release: %+v %+v", cur, disk)
	}
	if _, err := m.releaseSnapshot(p.ID, s.ID); err != nil {
		t.Fatal("snapshot release was not idempotent")
	}
}

func TestR16PendingCopiesReserveQuotaAndCleanUp(t *testing.T) {
	m, _ := newTestManager(t, func(c *Config) { c.Teams = map[string]TeamPolicy{"t1": {MaxVMs: 2}} })
	p, err := m.Create(context.Background(), CreateReq{Team: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, m, p.ID, "/home/dev/credential", "captured-old-token")
	c, err := m.fork(context.Background(), p.ID, "", "t1", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, m, p.ID, "/home/dev/credential", "fresh-source-token")
	var copied map[string]string
	if err := readJSON(filepath.Join(m.root(c.ID), diskFile), &copied); err != nil {
		t.Fatal(err)
	}
	if copied["/home/dev/credential"] != b64("captured-old-token") {
		t.Fatal("source refresh changed the immutable copy")
	}
	var ae *apiErr
	if _, err := m.Create(context.Background(), CreateReq{Team: "t1"}); !errors.As(err, &ae) || ae.code != 429 {
		t.Fatalf("pending copy evaded quota: %v", err)
	}
	s, err := m.createSnapshot(context.Background(), p.ID, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Delete(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	_, exists := m.snaps[s.ID]
	m.mu.Unlock()
	if exists || len(m.Snapshots(p.ID)) != 0 {
		t.Fatal("parent deletion leaked hidden pending snapshot")
	}
	if _, err := m.Delete(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
}
