package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPatchVM(t *testing.T) {
	clock := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	m, _ := newTestManager(t, nil)
	m.now = func() time.Time { return clock }
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	ctx := context.Background()
	a, err := m.Create(ctx, CreateReq{Name: "a", Team: "t", TTLS: 600})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, CreateReq{Name: "b", Team: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, CreateReq{Name: "c", Team: "other"}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/vms/" + a.ID
	var v VM
	var e map[string]any
	patch := func(body string, out any) int { // fresh v and e: omitempty fields must not linger
		v, e = VM{}, nil
		return call(t, srv, "PATCH", path, body, out)
	}

	// Validation, as create; nothing changes on a refused request.
	tooManyLabels := make([]string, 0, 65)
	for i := 0; i < 65; i++ {
		tooManyLabels = append(tooManyLabels, fmt.Sprintf(`"k%d":"v"`, i))
	}
	for _, body := range []string{
		`{}`, `{"name":"Bad!"}`, `{"ttl_s":-1}`, `{"extend_s":0}`, `{"name":"x","extend_s":-5}`, `{`,
		`{"idle_stop_s":-2}`, `{"labels":{"bad key!":"v"}}`, `{"labels":{"k":"` + strings.Repeat("x", 257) + `"}}`,
		`{"labels":{` + strings.Join(tooManyLabels, ",") + `}}`,
	} {
		if c := patch(body, &e); c != 400 {
			t.Fatalf("%s: %d %v", body, c, e)
		}
	}
	if c := call(t, srv, "PATCH", "/v1/vms/vm-nope", `{"name":"x"}`, &e); c != 404 {
		t.Fatalf("unknown vm: %d", c)
	}

	// Rename: unique within the team, free across teams, to itself is a no-op.
	if c := patch(`{"name":"b"}`, &e); c != 409 {
		t.Fatalf("name of another VM of the team: %d %v", c, e)
	}
	if c := patch(`{"name":"c"}`, &v); c != 200 || v.Name != "c" || v.ID != a.ID {
		t.Fatalf("rename to another team's name: %d %+v", c, v)
	}
	if c := patch(`{"name":"c"}`, &v); c != 200 || v.Name != "c" {
		t.Fatalf("rename to itself: %d %+v", c, v)
	}
	if g, _ := m.get(a.ID); g.Name != "c" {
		t.Fatalf("rename not kept: %+v", g)
	}

	// ttl_s while running: TTLS and a fresh deadline from now.
	clock = clock.Add(100 * time.Second)
	if c := patch(`{"ttl_s":3600}`, &v); c != 200 || v.TTLS != 3600 || v.ExpiresAt == nil || !v.ExpiresAt.Equal(clock.Add(time.Hour)) {
		t.Fatalf("ttl_s: %d %+v", c, v)
	}
	// extend_s pushes the deadline out from itself...
	if c := patch(`{"extend_s":60}`, &v); c != 200 || !v.ExpiresAt.Equal(clock.Add(time.Hour+time.Minute)) || v.TTLS != 3600 {
		t.Fatalf("extend_s: %d %+v", c, v)
	}
	// ...or from now once the deadline is past.
	clock = clock.Add(2 * time.Hour)
	if c := patch(`{"extend_s":60}`, &v); c != 200 || !v.ExpiresAt.Equal(clock.Add(time.Minute)) {
		t.Fatalf("extend_s past the deadline: %d %+v", c, v)
	}
	// ttl_s 0 clears the auto-stop; extend_s then has nothing to extend.
	if c := patch(`{"ttl_s":0}`, &v); c != 200 || v.TTLS != 0 || v.ExpiresAt != nil {
		t.Fatalf("ttl_s 0: %d %+v", c, v)
	}
	if c := patch(`{"extend_s":60}`, &e); c != 400 {
		t.Fatalf("extend_s without a TTL: %d %v", c, e)
	}
	// Both in one request: ttl_s first, then extend_s on top.
	if c := patch(`{"ttl_s":100,"extend_s":50}`, &v); c != 200 || !v.ExpiresAt.Equal(clock.Add(150*time.Second)) {
		t.Fatalf("ttl_s + extend_s: %d %+v", c, v)
	}

	// Stopped: ttl_s is kept for the next start, extend_s is a conflict.
	if _, err := m.Stop(ctx, a.ID, "api"); err != nil {
		t.Fatal(err)
	}
	if c := patch(`{"extend_s":60}`, &e); c != 409 {
		t.Fatalf("extend_s on a stopped VM: %d %v", c, e)
	}
	if c := patch(`{"ttl_s":900}`, &v); c != 200 || v.TTLS != 900 || v.ExpiresAt != nil {
		t.Fatalf("ttl_s on a stopped VM: %d %+v", c, v)
	}
	if c := patch(`{"name":"d","extend_s":60}`, &e); c != 409 {
		t.Fatalf("rename + extend on a stopped VM: %d %v", c, e)
	}
	if g, _ := m.get(a.ID); g.Name != "c" {
		t.Fatalf("a refused request renamed the VM: %+v", g)
	}
	s, err := m.Start(ctx, a.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.ExpiresAt == nil || !s.ExpiresAt.Equal(clock.Add(900*time.Second)) {
		t.Fatalf("start did not use the patched ttl_s: %+v", s)
	}

	// Rich PATCH: labels (including 32 user + 7 reserved boat.* = 39 keys, up to 64 max, and {} clear), idle_stop_s, resume_on_boot.
	richLabels := make([]string, 0, 39)
	for i := 0; i < 32; i++ {
		richLabels = append(richLabels, fmt.Sprintf(`"u%d":"%d"`, i, i))
	}
	for _, rk := range []string{"boat.secrets", "boat.env", "boat.sel", "boat.no_env", "boat.protected", "boat.self_api", "boat.owner"} {
		richLabels = append(richLabels, fmt.Sprintf(`%q:"1"`, rk))
	}
	if c := patch(`{"labels":{`+strings.Join(richLabels, ",")+`},"idle_stop_s":-1,"resume_on_boot":true}`, &v); c != 200 ||
		len(v.Labels) != 39 || v.Labels["boat.secrets"] != "1" || v.Labels["boat.no_env"] != "1" || v.IdleStopS != -1 || !v.ResumeOnBoot {
		t.Fatalf("rich patch (39 labels + idle_stop_s + resume_on_boot): %d %+v", c, v)
	}
	maxLabels := make([]string, 0, 64)
	for i := 0; i < 64; i++ {
		maxLabels = append(maxLabels, fmt.Sprintf(`"m%d":"%d"`, i, i))
	}
	if c := patch(`{"labels":{`+strings.Join(maxLabels, ",")+`}}`, &v); c != 200 || len(v.Labels) != 64 {
		t.Fatalf("max patch (64 labels): %d %+v", c, v)
	}
	// Atomic rollback on combined invalid/conflicting request: labels/idle_stop_s must not partially apply.
	if c := patch(`{"labels":{"only":"one"},"idle_stop_s":120,"name":"b"}`, &e); c != 409 {
		t.Fatalf("conflicting rich patch: %d %v", c, e)
	}
	if g, _ := m.get(a.ID); len(g.Labels) != 64 || g.IdleStopS != -1 || !g.ResumeOnBoot {
		t.Fatalf("failed rich patch mutated VM: %+v", g)
	}
	// Empty labels map {} clears labels; idle_stop_s 300 and resume_on_boot false apply.
	if c := patch(`{"labels":{},"idle_stop_s":300,"resume_on_boot":false}`, &v); c != 200 || len(v.Labels) != 0 || v.IdleStopS != 300 || v.ResumeOnBoot {
		t.Fatalf("clear labels + idle_stop_s + resume_on_boot: %d %+v", c, v)
	}
	if c := patch(`{"labels":{"env":"prod"}}`, &v); c != 200 || v.Labels["env"] != "prod" {
		t.Fatalf("set single label: %d %+v", c, v)
	}

	// Atomic ttl_s on start (stopped -> running, running -> updated deadline) and fork / fork/prepare.
	if _, err := m.Stop(ctx, a.ID, "api"); err != nil {
		t.Fatal(err)
	}
	if c := call(t, srv, "POST", path+"/start", `{"ttl_s":-1}`, &e); c != 400 {
		t.Fatalf("negative start ttl_s: %d %v", c, e)
	}
	v = VM{}
	if c := call(t, srv, "POST", path+"/start", `{"ttl_s":120}`, &v); c != 200 || v.TTLS != 120 || v.ExpiresAt == nil || !v.ExpiresAt.Equal(clock.Add(120*time.Second)) {
		t.Fatalf("start with ttl_s=120: %d %+v", c, v)
	}
	// Starting an already-running VM with ttl_s updates TTLS and ExpiresAt atomically (including 0 to clear).
	v = VM{}
	if c := call(t, srv, "POST", path+"/start", `{"ttl_s":0}`, &v); c != 200 || v.TTLS != 0 || v.ExpiresAt != nil {
		t.Fatalf("running start with ttl_s=0: %d %+v", c, v)
	}
	v = VM{}
	if c := call(t, srv, "POST", path+"/start", `{"ttl_s":900}`, &v); c != 200 || v.TTLS != 900 || v.ExpiresAt == nil || !v.ExpiresAt.Equal(clock.Add(900*time.Second)) {
		t.Fatalf("running start with ttl_s=900: %d %+v", c, v)
	}

	var child VM
	if c := call(t, srv, "POST", path+"/fork", `{"name":"child-ttl","ttl_s":45}`, &child); c != 201 || child.TTLS != 45 || child.ExpiresAt == nil || !child.ExpiresAt.Equal(clock.Add(45*time.Second)) {
		t.Fatalf("fork with ttl_s=45: %d %+v", c, child)
	}
	if parentNow, _ := m.get(a.ID); parentNow.TTLS != 900 {
		t.Fatalf("fork mutated parent TTLS: %+v", parentNow)
	}
	var prepChild VM
	if c := call(t, srv, "POST", path+"/fork/prepare", `{"name":"prep-ttl","ttl_s":75}`, &prepChild); c != 201 || prepChild.TTLS != 75 || !prepChild.PendingCopy {
		t.Fatalf("fork/prepare with ttl_s=75: %d %+v", c, prepChild)
	}
	if c := call(t, srv, "PATCH", "/v1/vms/"+prepChild.ID, `{"name":"nope"}`, &e); c != 404 {
		t.Fatalf("patch pending copy should 404: %d %v", c, e)
	}
	var relChild VM
	if c := call(t, srv, "POST", "/v1/vms/"+prepChild.ID+"/release", `{}`, &relChild); c != 200 || relChild.TTLS != 75 || relChild.ExpiresAt == nil || !relChild.ExpiresAt.Equal(clock.Add(75*time.Second)) {
		t.Fatalf("release fork/prepare child ttl_s=75: %d %+v", c, relChild)
	}
	if parentNow, _ := m.get(a.ID); parentNow.TTLS != 900 {
		t.Fatalf("fork/prepare mutated parent TTLS: %+v", parentNow)
	}

	// Persisted: a restarted boatd sees the new name, TTL, labels, and idle_stop_s.
	m.closeWatches()
	m2, err := newManager(m.cfg, m.run, m.procs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m2.closeWatches)
	if g, err := m2.get(a.ID); err != nil || g.Name != "c" || g.TTLS != 900 || g.Labels["env"] != "prod" || g.IdleStopS != 300 {
		t.Fatalf("patch not persisted: %+v %v", g, err)
	}
}
