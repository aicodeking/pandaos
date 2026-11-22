package main

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"
)

// r42: the builder gate on the team-level snapshot routes, no raw AI keys for a
// no_env run, the snapshot source taken from the run's recorded secrets, the
// lifetime AI usage row cap, compat fork --type keeping fail_fast.

func (e *env) isBuilder403(st int, out map[string]any) bool {
	return st == http.StatusForbidden && isBuilderRefusal(out)
}

// builderSnapshot is a snapshot of a builder VM (taken by the admin key), with the
// builder's compat id.
func (e *env) builderSnapshot(name string) (sid, vm, bx string) {
	vm, bx = e.builderVM()
	sid = e.r39Snapshot(vm, name)
	return sid, vm, bx
}

func (e *env) snapExists(sid string) bool {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	return e.boatd.snaps[sid] != nil
}

// 1: a non-admin key cannot delete a builder VM's snapshot by any team-level route.
func TestR42TeamSnapshotRoutesRefuseBuilderSnapshot(t *testing.T) {
	e := setup(t)
	member := e.newKey("vms:read", "vms:write", "exec")
	sid, _, _ := e.builderSnapshot("bsnap")
	uuid := snapshotUUID(e.teamID(), sid)

	cases := []struct{ name, method, path string }{
		{"native", "DELETE", "/v1/snapshots/" + sid},
		{"compat by id", "DELETE", "/api/v1/snapshots/" + uuid},
		{"saved by name", "DELETE", "/api/v1/saved-snapshots/bsnap"},
		{"named by name", "DELETE", "/api/v1/named-snapshots/bsnap"},
	}
	for _, c := range cases {
		st, out, _ := e.compat(member, c.method, c.path, nil, map[string]string{"X-Ascii-Confirm-Delete": uuid})
		if !e.isBuilder403(st, out) {
			t.Errorf("%s = %d %v, want the builder 403", c.name, st, out)
		}
		if !e.snapExists(sid) {
			t.Fatalf("%s deleted the builder's snapshot", c.name)
		}
	}
	// Overwrite by name refuses before it takes (or deletes) anything.
	other := e.createVM()
	n := len(e.boatd.snaps)
	for _, path := range []string{"/api/v1/saved-snapshots", "/api/v1/named-snapshots"} {
		st, out, _ := e.compat(member, "POST", path, map[string]any{"name": "bsnap", "sandboxId": other}, nil)
		if !e.isBuilder403(st, out) {
			t.Errorf("POST %s over a builder snapshot = %d %v, want the builder 403", path, st, out)
		}
	}
	if len(e.boatd.snaps) != n || !e.snapExists(sid) {
		t.Errorf("a refused overwrite changed the snapshots (%d -> %d)", n, len(e.boatd.snaps))
	}
	// The admin key still deletes it.
	if st, out := e.do(e.owner, "DELETE", "/v1/snapshots/"+sid, nil); st != 200 {
		t.Errorf("admin delete = %d %v", st, out)
	}
	if e.snapExists(sid) {
		t.Errorf("admin delete left the snapshot")
	}
}

// 1: an environment's build snapshot is protected whatever its VM is.
func TestR42EnvBuildSnapshotProtected(t *testing.T) {
	e := setup(t)
	member := e.newKey("vms:read", "vms:write", "exec")
	vm := e.createVM()
	sid := e.r39Snapshot(vm, "built")
	if err := e.srv.store.Update(func(st *State) error {
		st.Environments["env_r42"] = &Environment{ID: "env_r42", TeamID: e.teamID(), Name: "r42",
			Build: &EnvBuild{State: "ready", SnapshotID: sid}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if st, out := e.do(member, "DELETE", "/v1/snapshots/"+sid, nil); !e.isBuilder403(st, out) {
		t.Fatalf("member delete of a build snapshot = %d %v, want the builder 403", st, out)
	}
	if st, out, _ := e.compat(member, "DELETE", "/api/v1/saved-snapshots/built", nil, nil); !e.isBuilder403(st, out) {
		t.Fatalf("member saved-snapshot delete of a build snapshot = %d %v", st, out)
	}
	if !e.snapExists(sid) {
		t.Fatal("the build snapshot was deleted")
	}
	// Another team's environment does not protect it; an ordinary snapshot is
	// deletable by the member.
	other := e.createVM()
	osid := e.r39Snapshot(other, "plain")
	if st, out := e.do(member, "DELETE", "/v1/snapshots/"+osid, nil); st != 200 {
		t.Errorf("member delete of an ordinary snapshot = %d %v", st, out)
	}
	if st, out := e.do(e.owner, "DELETE", "/v1/snapshots/"+sid, nil); st != 200 {
		t.Errorf("admin delete of the build snapshot = %d %v", st, out)
	}
}

// 2: a no_env run never gets raw AI keys when the team proxy is off; with the proxy
// on it gets its per-VM proxy token.
func TestR42NoEnvRunGetsNoRawAIKey(t *testing.T) {
	e := setup(t)
	team := e.teamID()
	sealed, err := e.srv.keys.Seal([]byte("sk-REAL-raw"), "ai/"+aiCredKey(team, "openai"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.store.Update(func(st *State) error {
		st.AICreds[aiCredKey(team, "openai")] = &AICredential{TeamID: team, Provider: "openai", Sealed: sealed, Last4: "-raw", Updated: time.Now().UTC()}
		st.AIProxyOff[team] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	id := e.createVM()
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{}); st != 200 {
		t.Fatalf("start = %d %v", st, out)
	}
	if v, _ := e.lastStart()["env"].(map[string]any); v["OPENAI_API_KEY"] != "sk-REAL-raw" {
		t.Fatalf("control: a normal start env = %v, want the raw key", v)
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("no_env start = %d %v", st, out)
	}
	if v, _ := e.lastStart()["env"].(map[string]any); v["OPENAI_API_KEY"] != nil {
		t.Fatalf("no_env start env = %v, want no raw AI key", v)
	}
	if st, _ := e.raw(e.owner, "POST", "/v1/vms/"+id+"/prompt", map[string]any{"prompt": "hi"}); st != 200 {
		t.Fatalf("no_env prompt = %d", st)
	}
	if v := e.lastExecEnv(); v["OPENAI_API_KEY"] != nil {
		t.Fatalf("no_env run's prompt env = %v, want no raw AI key", v)
	}

	// Proxy on: the no_env run keeps its per-VM proxy token (revocable), never the key.
	e.srv.cfg.AIProxyURL = "https://ai.example"
	if err := e.srv.store.Update(func(st *State) error { delete(st.AIProxyOff, team); return nil }); err != nil {
		t.Fatal(err)
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("no_env start (proxy on) = %d %v", st, out)
	}
	v, _ := e.lastStart()["env"].(map[string]any)
	if k, _ := v["OPENAI_API_KEY"].(string); k == "" || k == "sk-REAL-raw" || v["OPENAI_BASE_URL"] == nil {
		t.Fatalf("no_env start env (proxy on) = %v, want the proxy token and base url", v)
	}
}

// 3: the snapshot's record holds the secrets the run was started with: a default
// un-defaulted since the run began is still in it, so a create with the default
// selection boots fresh.
func TestR42SnapshotRecordUsesRunStartNames(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/X", map[string]any{"value": "x1", "inject": true})
	vm := e.createVM() // its run holds X
	if names, ok := runNamesOf(e.srv.store.Snapshot(), vm); !ok || !slices.Equal(names, []string{"X"}) {
		t.Fatalf("recorded run names = %v %v, want [X]", names, ok)
	}
	if err := e.srv.store.Update(func(st *State) error {
		st.Secrets[secretKey(e.teamID(), "X")].Inject = false // un-default X
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sid := e.r39Snapshot(vm, "after-undefault")
	if rec := e.snapRecord(sid); !slices.Contains(rec, "X") {
		t.Fatalf("record = %v, want X (the run's secret) in it", rec)
	}
	st, out := e.createFrom(sid, nil) // absent secrets: the default selection, now without X
	if st != 201 || e.freshCreatesOf(out["id"].(string)) != 1 {
		t.Fatalf("create from it with the default selection = %d %v, want a fresh boot", st, out)
	}
	// A selection that covers the run's (the defaults it took, and X by name, as an
	// admin) still resumes it.
	st, out = e.createFrom(sid, map[string]any{"secrets": []string{defaultSelection, "X"}})
	if st != 201 || e.freshCreatesOf(out["id"].(string)) != 0 {
		t.Fatalf("create from it with the default selection and X = %d %v, want no fresh boot", st, out)
	}
}

// 3: a run with no names recorded (one from before they were kept) makes a
// snapshot of unknown source.
func TestR42SnapshotOfRunWithoutNamesIsUnknown(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/X", map[string]any{"value": "x1", "inject": true})
	vm := e.createVM()
	if err := e.srv.store.Update(func(st *State) error { delete(st.VMSecrets, runNamesKey(vm)); return nil }); err != nil {
		t.Fatal(err)
	}
	sid := e.r39Snapshot(vm, "legacy")
	if rec := e.snapRecord(sid); len(rec) != 0 {
		t.Fatalf("record = %v, want none", rec)
	}
	// A start records the run again.
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{}); st != 200 {
		t.Fatalf("start = %d %v", st, out)
	}
	if names, ok := runNamesOf(e.srv.store.Snapshot(), vm); !ok || !slices.Equal(names, []string{"X"}) {
		t.Fatalf("names after a start = %v %v, want [X]", names, ok)
	}
	// A deleted VM leaves no names.
	if st, _ := e.do(e.owner, "DELETE", "/v1/vms/"+vm, nil); st != 200 && st != 204 {
		t.Fatalf("delete = %d", st)
	}
	if _, ok := runNamesOf(e.srv.store.Snapshot(), vm); ok {
		t.Errorf("a deleted VM kept its run names")
	}
}

// 4: lifetime AI usage rows are capped per team; the least recently used are
// folded into the team's aggregate and the totals stay.
func TestR42AIUsageLifetimeRowCap(t *testing.T) {
	old := aiUsageLifeMaxRows
	aiUsageLifeMaxRows = 10
	t.Cleanup(func() { aiUsageLifeMaxRows = old })
	st := newUsageState()
	now := time.Now().UTC()
	day := now.Format(time.DateOnly)
	for i := 0; i < 30; i++ {
		useVM(st, "t1", fmt.Sprintf("vm%02d", i), "openai", "m", day, 1, 10, 5)
		// vm00 is the least recently used, vm29 the most
		st.AIUsage[aiUsageKey("t1", fmt.Sprintf("vm%02d", i), "openai", "m")].Updated = now.Add(time.Duration(i) * time.Second)
	}
	for i := 0; i < 5; i++ { // another team under the cap
		useVM(st, "t2", fmt.Sprintf("vm%d", i), "openai", "m", day, 2, 20, 10)
	}
	reqBefore, inBefore, outBefore := sumRows(st.AIUsage, "t1")
	if !capAIUsageLife(st) {
		t.Fatal("nothing folded")
	}
	n1, n2 := 0, 0
	for _, r := range st.AIUsage {
		switch r.TeamID {
		case "t1":
			n1++
		case "t2":
			n2++
		}
	}
	if n1 != 10 || n2 != 5 {
		t.Fatalf("rows t1=%d t2=%d, want 10 and 5", n1, n2)
	}
	if req, in, out := sumRows(st.AIUsage, "t1"); req != reqBefore || in != inBefore || out != outBefore {
		t.Fatalf("t1 totals %d/%d/%d, want %d/%d/%d", req, in, out, reqBefore, inBefore, outBefore)
	}
	if st.AIUsage[aiUsageKey("t1", aiDeletedVM, "openai", "m")] == nil {
		t.Fatal("no aggregate row")
	}
	// The most recently used rows stay, the least recently used were folded.
	if st.AIUsage[aiUsageKey("t1", "vm29", "openai", "m")] == nil || st.AIUsage[aiUsageKey("t1", "vm00", "openai", "m")] != nil {
		t.Fatalf("not the least recently used rows folded")
	}
	if req, _, _ := sumRows(st.AIUsage, "t2"); req != 10 {
		t.Fatalf("t2 totals changed: %d", req)
	}
	if capAIUsageLife(st) {
		t.Fatal("a second pass folded again")
	}
}

// 4: the cap is applied by the meter's flush.
func TestR42AIUsageFlushAppliesLifetimeCap(t *testing.T) {
	old := aiUsageLifeMaxRows
	aiUsageLifeMaxRows = 5
	t.Cleanup(func() { aiUsageLifeMaxRows = old })
	e := setup(t)
	team := e.teamID()
	m := e.srv.aiMeter
	for i := 0; i < 12; i++ {
		m.add(team, fmt.Sprintf("vm%d", i), "openai", "m", 3, 0)
	}
	m.flushFinal(e.srv)
	rows, req := 0, int64(0)
	for _, r := range e.srv.store.Snapshot().AIUsage {
		if r.TeamID == team {
			rows++
			req += r.Requests
		}
	}
	if rows > 5 || req != 12 {
		t.Fatalf("rows=%d requests=%d, want <=5 rows and 12 requests", rows, req)
	}
}

// 5: compat fork with a type forwards fail_fast to the post-resize start.
func TestR42CompatForkTypeKeepsFailFast(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(map[string]any{"name": "parent"})
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/fork", map[string]any{"type": "large", "failFast": true}, nil)
	if st != http.StatusAccepted {
		t.Fatalf("fork --type = %d %v", st, out)
	}
	if got := e.lastStart()["fail_fast"]; got != true {
		t.Fatalf("post-resize start body = %v, want fail_fast", e.lastStart())
	}
}
