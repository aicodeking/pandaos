package main

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// r43: a no_env run's self-token cannot reach earlier snapshots or history, the
// builder gate on compat snapshot downloads and on creates from a snapshot, the
// run record keeping a resumed run's secrets, no_env without secrets meaning
// none, resume and fork with an environment, compat count rules.

// currentSelfToken is the token of the VM's current generation (the one in its
// guest env, whatever the last env push carried).
func (e *env) currentSelfToken(vm string) string {
	e.t.Helper()
	tok := e.srv.store.Snapshot().VMTokens[vm]
	if tok == nil {
		e.t.Fatalf("no VM token for %s (cfg.VMAPIURL unset?)", vm)
	}
	return e.srv.selfToken(vm, tok.Gen)
}

func (e *env) saveEnv(name string, vars map[string]string, secrets []string) string {
	e.t.Helper()
	body := map[string]any{"name": name, "env": vars}
	if secrets != nil {
		body["secrets"] = secrets
	}
	st, out := e.do(e.owner, "POST", "/v1/environments", body)
	if st != 201 {
		e.t.Fatalf("create environment %s = %d %v", name, st, out)
	}
	return out["id"].(string)
}

// 1: snapshot during a secret run, then a no_env start: the new run's self-token
// gets 403 on snapshot list, create and download and on every history route.
func TestR43SelfTokenNoEnvRunRefusedSnapshotsAndHistory(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	e.do(e.owner, "PUT", "/v1/secrets/TEAM_SEC", map[string]any{"value": "top-secret", "inject": true})
	bx := e.newSandbox(nil)
	vm := e.nativeOf(bx)
	sid := e.r39Snapshot(vm, "secret-run") // the secret run's snapshot (disk and memory)

	// Control: while the run is not no_env its own token reads them.
	tok := e.currentSelfToken(vm)
	if st, out := e.do(tok, "GET", "/v1/vms/"+vm+"/snapshots", nil); st != 200 {
		t.Fatalf("control: self-token snapshot list of a normal run = %d %v", st, out)
	}

	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("no_env start = %d %v", st, out)
	}
	tok = e.currentSelfToken(vm) // the new run's token
	if st, out := e.do(tok, "GET", "/v1/vms/"+vm, nil); st != 200 {
		t.Fatalf("control: the no_env run's token on its own VM = %d %v", st, out)
	}
	native := "/v1/vms/" + vm
	compat := "/api/v1/sandboxes/" + bx
	for _, c := range []struct{ method, path string }{
		{"GET", native + "/snapshots"},
		{"POST", native + "/snapshots"},
		{"DELETE", native + "/snapshots/" + sid},
		{"GET", native + "/snapshots/" + sid + "/download"},
		{"GET", native + "/commands"},
		{"GET", native + "/commands/c1/logs"},
		{"GET", native + "/command-history"},
		{"GET", native + "/prompts"},
		{"GET", native + "/prompts/p1/logs"},
		{"GET", native + "/conversations"},
		{"GET", compat + "/snapshots"},
		{"DELETE", compat + "/snapshots"},
		{"GET", compat + "/snapshots/latest"},
		{"GET", compat + "/events"},
		{"GET", compat + "/conversations"},
		{"GET", compat + "/command-history"},
		{"GET", compat + "/command-history/c1"},
		{"GET", compat + "/command-history/c1/logs"},
	} {
		st, out, _ := e.compat(tok, c.method, c.path, map[string]any{}, map[string]string{"X-Ascii-Confirm-Delete": bx})
		if st != http.StatusForbidden || !strings.Contains(fmt.Sprint(out), "VM is no_env") {
			t.Errorf("%s %s with a no_env run's self-token = %d %v, want 403", c.method, c.path, st, out)
		}
	}
	// The owner's key is not limited by the run being no_env.
	if st, out := e.do(e.owner, "GET", native+"/snapshots", nil); st != 200 {
		t.Errorf("owner snapshot list = %d %v", st, out)
	}
	// r46: no_env is sticky: after a stop and a plain start the new token is still
	// limited to the allowlist.
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{}); st != 200 {
		t.Fatalf("plain start = %d %v", st, out)
	}
	if st, out := e.do(e.currentSelfToken(vm), "GET", native+"/snapshots", nil); st != http.StatusForbidden {
		t.Errorf("self-token after a plain start of a no_env VM = %d %v, want 403", st, out)
	}
}

// 2: the compat snapshot download answers the builder 403 to a non-admin key, and
// a chunk URL minted earlier is refused once its snapshot became a build snapshot.
func TestR43CompatSnapshotDownloadBuilderGate(t *testing.T) {
	e := setup(t)
	member := e.newKey("vms:read", "vms:write", "exec")
	bsid, _, _ := e.builderSnapshot("bsnap")
	uuid := snapshotUUID(e.teamID(), bsid)
	st, out, _ := e.compat(member, "GET", "/api/v1/snapshots/"+uuid+"/download", nil, nil)
	if !e.isBuilder403(st, out) {
		t.Fatalf("member compat download of a builder snapshot = %d %v, want the builder 403", st, out)
	}
	if st, out, _ := e.compat(e.owner, "GET", "/api/v1/snapshots/"+uuid+"/download", nil, nil); st != 200 {
		t.Errorf("admin compat download of a builder snapshot = %d %v", st, out)
	}
	// Native download of a builder VM's snapshot (the VM route's own gate).
	if st, out := e.do(member, "GET", vmPath(e.builderOf(bsid), "snapshots", bsid, "download"), nil); !e.isBuilder403(st, out) {
		t.Errorf("member native download = %d %v", st, out)
	}

	// An ordinary snapshot's chunk URL, minted by the member, then it becomes an
	// environment's build snapshot: the URL no longer works.
	vm := e.createVM()
	sid := e.r39Snapshot(vm, "plain")
	st, out, _ = e.compat(member, "GET", "/api/v1/snapshots/"+snapshotUUID(e.teamID(), sid)+"/download", nil, nil)
	if st != 200 {
		t.Fatalf("member download of an ordinary snapshot = %d %v", st, out)
	}
	chunks := out["chunks"].([]any)
	u, err := url.Parse(chunks[0].(map[string]any)["signedUrl"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := e.raw("", "GET", u.Path, nil); st != 200 {
		t.Fatalf("control: the chunk URL = %d, want 200", st)
	}
	if err := e.srv.store.Update(func(s *State) error {
		s.Environments["env_r43"] = &Environment{ID: "env_r43", TeamID: e.teamID(), Name: "r43",
			Build: &EnvBuild{State: "ready", SnapshotID: sid}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if st, body := e.raw("", "GET", u.Path, nil); st != http.StatusForbidden {
		t.Errorf("chunk URL of a snapshot that became a build snapshot = %d %s, want 403", st, body)
	}
}

// builderOf is the VM a snapshot was taken from.
func (e *env) builderOf(sid string) string {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	return str(e.boatd.snaps[sid], "vm_id")
}

// 2: a create from a builder's snapshot, native or compat, is refused for a
// non-admin key; the environment path (its own build snapshot) still works.
func TestR43CreateFromBuilderSnapshotRefused(t *testing.T) {
	e := setup(t)
	member := e.newKey("vms:read", "vms:write", "exec")
	bsid, _, _ := e.builderSnapshot("bsnap")
	n := len(e.boatd.creates)
	st, out := e.do(member, "POST", "/v1/vms", map[string]any{"name": "x", "snapshot": bsid})
	if !e.isBuilder403(st, out) {
		t.Errorf("member create from a builder snapshot = %d %v", st, out)
	}
	st, out, _ = e.compat(member, "POST", "/api/v1/sandboxes", map[string]any{"from": "bsnap"}, nil)
	if !e.isBuilder403(st, out) {
		t.Errorf("member compat create from a builder snapshot = %d %v", st, out)
	}
	if len(e.boatd.creates) != n {
		t.Errorf("a refused create reached boatd")
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "adm", "snapshot": bsid}); st != 201 {
		t.Errorf("admin create from a builder snapshot = %d %v", st, out)
	}

	// An environment's build snapshot: named directly, refused; through the
	// environment, it is used.
	vm := e.createVM()
	sid := e.r39Snapshot(vm, "built")
	if err := e.srv.store.Update(func(s *State) error {
		s.Environments["env_r43b"] = &Environment{ID: "env_r43b", TeamID: e.teamID(), Name: "r43b", Version: 1,
			Build: &EnvBuild{State: "ready", SnapshotID: sid, Version: 1}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if st, out := e.do(member, "POST", "/v1/vms", map[string]any{"name": "y", "snapshot": sid}); !e.isBuilder403(st, out) {
		t.Errorf("member create from a build snapshot by id = %d %v", st, out)
	}
	st, out = e.do(member, "POST", "/v1/vms", map[string]any{"name": "z", "environment": "r43b"})
	if st != 201 {
		t.Fatalf("member create through the environment = %d %v", st, out)
	}
	if got := e.lastCreate()["snapshot"]; got != sid {
		t.Errorf("environment create used snapshot %v, want the build snapshot %s", got, sid)
	}
	// Ordinary snapshots are still creatable by a member.
	osid := e.r39Snapshot(e.createVM(), "ordinary")
	if st, out := e.do(member, "POST", "/v1/vms", map[string]any{"name": "o", "snapshot": osid}); st != 201 {
		t.Errorf("member create from an ordinary snapshot = %d %v", st, out)
	}
}

// 3: a secret un-defaulted since the run began is still held by the resumed
// memory, so a snapshot taken after a cold start keeps it in its record.
func TestR43ColdStartKeepsResumedRunNames(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/X", map[string]any{"value": "x1", "inject": true})
	vm := e.createVM()
	if err := e.srv.store.Update(func(st *State) error {
		st.Secrets[secretKey(e.teamID(), "X")].Inject = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{}); st != 200 {
		t.Fatalf("start = %d %v", st, out)
	}
	if names, _ := runNamesOf(e.srv.store.Snapshot(), vm); !slices.Contains(names, "X") {
		t.Fatalf("run names after the resume = %v, want X kept", names)
	}
	sid := e.r39Snapshot(vm, "resumed")
	if rec := e.snapRecord(sid); !slices.Contains(rec, "X") {
		t.Fatalf("record = %v, want X", rec)
	}

	// r46: after a no_env run (memory without team secrets) the VM stays no_env: a
	// plain start is a no_env start, and a snapshot of it is @no_env.
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{"no_env": true})
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{}); st != 200 {
		t.Fatalf("start after no_env = %d %v", st, out)
	}
	if rec := e.snapRecord(e.r39Snapshot(vm, "after-no-env")); !slices.Contains(rec, snapNoEnv) {
		t.Errorf("record after a plain start of a no_env VM = %v, want @no_env", rec)
	}
}

// 4: no_env with secrets absent selects none, not @default.
func TestR43NoEnvCreateWithoutSecretsSelectsNone(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/X", map[string]any{"value": "x1", "inject": true})
	st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "ne", "no_env": true})
	if st != 201 {
		t.Fatalf("create = %d %v", st, out)
	}
	vm := out["id"].(string)
	if sel := e.srv.vmSecrets(vm); sel == nil || len(sel) != 0 {
		t.Errorf("selection = %v, want [] (none)", sel)
	}
	if names, ok := runNamesOf(e.srv.store.Snapshot(), vm); !ok || len(names) != 0 {
		t.Errorf("run names = %v %v, want none", names, ok)
	}
	// r47: no_env never takes secrets: naming some is 400 no_env_sticky; the default
	// (no no_env) is unchanged.
	st, out = e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "ne2", "no_env": true, "secrets": []string{"X"}})
	if st != 400 || r46Code(out) != errNoEnvSticky {
		t.Errorf("no_env with named secrets = %d %v, want 400 no_env_sticky", st, out)
	}
	st, out = e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "dflt"})
	if st != 201 || !slices.Equal(e.srv.vmSecrets(out["id"].(string)), []string{defaultSelection}) {
		t.Errorf("default create selection = %v", e.srv.vmSecrets(out["id"].(string)))
	}
}

// 5: native resume with an environment applies its vars and secret selection,
// for this run and the VM's later ones; an unknown name is refused before the
// VM is touched.
func TestR43NativeStartWithEnvironment(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/S1", map[string]any{"value": "one", "inject": true})
	e.do(e.owner, "PUT", "/v1/secrets/S2", map[string]any{"value": "two", "inject": false})
	envID := e.saveEnv("prod", map[string]string{"E1": "v1"}, []string{"S2"})
	vm := e.createVM() // the default selection: S1
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	starts := len(e.boatd.starts)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{"environment": "nope"}); st != 404 {
		t.Fatalf("start with an unknown environment = %d %v, want 404", st, out)
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{"environment": "prod", "no_env": true}); st != 400 {
		t.Fatalf("start with environment and no_env = %d %v, want 400", st, out)
	}
	if len(e.boatd.starts) != starts {
		t.Fatalf("a refused start reached boatd")
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{"environment": "prod"}); st != 200 {
		t.Fatalf("start with environment = %d %v", st, out)
	}
	got, _ := e.lastStart()["env"].(map[string]any)
	if got["E1"] != "v1" || got["S2"] != "two" || got["S1"] != nil {
		t.Fatalf("start env = %v, want E1 and S2, not S1", got)
	}
	st := e.srv.store.Snapshot()
	if !slices.Equal(st.VMSecrets[vm], []string{"S2"}) || st.VMEnvs[vm] != envID {
		t.Errorf("recorded selection %v env %q, want [S2] %s", st.VMSecrets[vm], st.VMEnvs[vm], envID)
	}
	// The next plain start keeps the environment's selection and vars.
	e.do(e.owner, "POST", "/v1/vms/"+vm+"/stop", nil)
	if s, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{}); s != 200 {
		t.Fatalf("plain start = %d %v", s, out)
	}
	got, _ = e.lastStart()["env"].(map[string]any)
	if got["E1"] != "v1" || got["S2"] != "two" || got["S1"] != nil {
		t.Errorf("later start env = %v, want the environment's", got)
	}
	// A running VM cannot switch environments in place.
	if s, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/start", map[string]any{"environment": "prod"}); s != 409 {
		t.Errorf("start of a running VM with environment = %d %v, want 409", s, out)
	}
}

// 5: native fork with an environment gives the child its selection and vars, and
// a child whose selection does not cover the parent's run is cold-booted.
func TestR43NativeForkWithEnvironment(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/S1", map[string]any{"value": "one", "inject": true})
	e.do(e.owner, "PUT", "/v1/secrets/S2", map[string]any{"value": "two", "inject": false})
	envID := e.saveEnv("prod", map[string]string{"E1": "v1"}, []string{"S2"})
	vm := e.createVM()
	forks := len(e.boatd.releaseBodies)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/fork", map[string]any{"environment": "nope"}); st != 404 {
		t.Fatalf("fork with an unknown environment = %d %v, want 404", st, out)
	}
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/fork", map[string]any{"environment": "prod", "no_env": true}); st != 400 {
		t.Fatalf("fork with environment and no_env = %d %v, want 400", st, out)
	}
	if len(e.boatd.releaseBodies) != forks || len(e.boatd.vms) != 1 {
		t.Fatalf("a refused fork copied the VM")
	}
	st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/fork", map[string]any{"environment": "prod"})
	if st != 201 {
		t.Fatalf("fork with environment = %d %v", st, out)
	}
	child := out["id"].(string)
	snap := e.srv.store.Snapshot()
	if !slices.Equal(snap.VMSecrets[child], []string{"S2"}) || snap.VMEnvs[child] != envID {
		t.Errorf("child selection %v env %q, want [S2] %s", snap.VMSecrets[child], snap.VMEnvs[child], envID)
	}
	if e.freshReleasesOf(child) != 1 {
		t.Errorf("a child whose selection does not cover the parent's run must be cold-booted")
	}
	if names, _ := runNamesOf(snap, child); slices.Contains(names, "S1") {
		t.Errorf("child run names = %v: a cold-booted child holds no S1", names)
	}
	// The same selection as the parent's resumes its memory.
	e.saveEnv("same", map[string]string{"E2": "v2"}, []string{defaultSelection})
	st, out = e.do(e.owner, "POST", "/v1/vms/"+vm+"/fork", map[string]any{"environment": "same"})
	if st != 201 || e.freshReleasesOf(out["id"].(string)) != 0 {
		t.Errorf("fork with a covering environment = %d %v, want no fresh boot", st, out)
	}
}

// 5: compat resume and fork take an environment.
func TestR43CompatResumeAndForkWithEnvironment(t *testing.T) {
	e := setup(t)
	e.do(e.owner, "PUT", "/v1/secrets/S1", map[string]any{"value": "one", "inject": true})
	e.do(e.owner, "PUT", "/v1/secrets/S2", map[string]any{"value": "two", "inject": false})
	envID := e.saveEnv("prod", map[string]string{"E1": "v1"}, []string{"S2"})
	bx := e.newSandbox(nil)
	vm := e.nativeOf(bx)
	base := "/api/v1/sandboxes/" + bx
	e.ok("POST", base+"/stop", nil, 202)
	starts := len(e.boatd.starts)
	st, out, _ := e.compat(e.owner, "POST", base+"/resume", map[string]any{"environment": "nope"}, nil)
	if st != 404 || errCode(out) != "unknown_environment" {
		t.Fatalf("resume with an unknown environment = %d %v", st, out)
	}
	if st, out, _ := e.compat(e.owner, "POST", base+"/resume", map[string]any{"environment": "prod", "noEnv": true}, nil); st != 400 {
		t.Fatalf("resume with environment and noEnv = %d %v", st, out)
	}
	if len(e.boatd.starts) != starts {
		t.Fatalf("a refused resume reached boatd")
	}
	// A type change with an unknown environment must not stop or resize anything.
	if st, _, _ := e.compat(e.owner, "POST", base+"/resume", map[string]any{"environment": "nope", "type": "large"}, nil); st != 404 {
		t.Fatalf("resume with a type and an unknown environment = %d", st)
	}
	e.ok("POST", base+"/resume", map[string]any{"environment": "prod"}, 202)
	got, _ := e.lastStart()["env"].(map[string]any)
	if got["E1"] != "v1" || got["S2"] != "two" || got["S1"] != nil {
		t.Fatalf("resume env = %v, want E1 and S2, not S1", got)
	}
	if s := e.srv.store.Snapshot(); !slices.Equal(s.VMSecrets[vm], []string{"S2"}) || s.VMEnvs[vm] != envID {
		t.Errorf("recorded selection %v env %q", s.VMSecrets[vm], s.VMEnvs[vm])
	}

	// Fork.
	if st, out, _ := e.compat(e.owner, "POST", base+"/fork", map[string]any{"environment": "nope"}, nil); st != 404 || errCode(out) != "unknown_environment" {
		t.Fatalf("fork with an unknown environment = %d %v", st, out)
	}
	if st, out, _ := e.compat(e.owner, "POST", base+"/fork", map[string]any{"environment": "prod", "noEnv": true}, nil); st != 400 {
		t.Fatalf("fork with environment and noEnv = %d %v", st, out)
	}
	e.saveEnv("other", map[string]string{"E3": "v3"}, []string{"S1"})
	out = e.ok("POST", base+"/fork", map[string]any{"environment": "other"}, 202)
	child := e.nativeOf(out["sandbox"].(map[string]any)["id"].(string))
	if s := e.srv.store.Snapshot(); !slices.Equal(s.VMSecrets[child], []string{"S1"}) {
		t.Errorf("forked child selection = %v, want [S1]", s.VMSecrets[child])
	}
}

// 6: compat create's count rules, refused before createVM.
func TestR43CompatCountRules(t *testing.T) {
	e := setup(t)
	e.r39Snapshot(e.createVM(), "src")
	n := len(e.boatd.creates)
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"count": 2}, nil)
	if st != 400 || errCode(out) != "count_requires_from" {
		t.Errorf("count without from = %d %v", st, out)
	}
	st, out, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"count": 2, "failFast": true}, nil)
	if st != 400 || errCode(out) != "count_requires_from" {
		t.Errorf("count and failFast without from = %d %v", st, out)
	}
	st, out, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"count": 2, "from": "src", "failFast": true}, nil)
	if st != 400 || errCode(out) != "count_with_fail_fast" {
		t.Errorf("count with failFast = %d %v", st, out)
	}
	if len(e.boatd.creates) != n {
		t.Errorf("a refused create reached boatd")
	}
	// A count of 1, and a count with from, still work.
	if st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"count": 1}, nil); st != 202 {
		t.Errorf("count 1 = %d %v", st, out)
	}
	if st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"count": 2, "from": "src"}, nil); st != 202 {
		t.Errorf("count 2 from a snapshot = %d %v", st, out)
	}
}
