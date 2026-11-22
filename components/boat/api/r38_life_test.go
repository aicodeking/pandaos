package main

import (
	"net/http"
	"testing"
)

// r38: env_start_failed (a no_env cold start that failed closed), a no_env fork of
// a parent whose run is not no_env cold-boots its child, no in-place conversion to
// no_env, compat provisioning while a retry is in flight, a VM's own token never
// starts its VM.

func (e *env) setReleaseFailures(failBefore, failAfterBoot int) {
	e.boatd.mu.Lock()
	e.boatd.failRelease, e.boatd.failReleaseAfterBoot = failBefore, failAfterBoot
	e.boatd.mu.Unlock()
}

func (e *env) freshReleasesOf(id string) int {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	n := 0
	for _, v := range e.boatd.freshReleases {
		if v == id {
			n++
		}
	}
	return n
}

func (e *env) boatdVMCount() int {
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	return len(e.boatd.vms)
}

// runningSecretVM is a running VM whose run has team secrets (a normal start).
func (e *env) runningSecretVM() string {
	id := e.secretVM()
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{}); st != 200 {
		e.t.Fatalf("start = %d %v", st, out)
	}
	return id
}

// 1: a fail-closed no_env start answers 502 env_start_failed with the stopped VM,
// natively and through compat resume.
func TestR38EnvStartFailedNativeAndCompat(t *testing.T) {
	e := setup(t)
	id := e.secretVM()
	e.setEnvFailures(false, true)
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true})
	if st != 502 || nativeErrCode(out) != "env_start_failed" {
		t.Fatalf("native = %d %v, want 502 env_start_failed", st, out)
	}
	if vm, _ := out["vm"].(map[string]any); vm["state"] != "stopped" || vm["id"] != id {
		t.Fatalf("vm = %v, want the stopped VM", out["vm"])
	}
	// The same through compat resume.
	st, cout, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"noEnv": true}, nil)
	if st != 502 || cout["code"] != "env_start_failed" {
		t.Fatalf("compat resume = %d %v, want 502 env_start_failed", st, cout)
	}
	if s := e.boatdState(id); s != "stopped" {
		t.Fatalf("VM = %s, want stopped", s)
	}
	// A running VM's failed set_env stays env_update_failed.
	e.setEnvFailures(false, false)
	running := e.createVM()
	e.setEnvFailures(true, false)
	st, out = e.do(e.owner, "POST", "/v1/vms/"+running+"/start", map[string]any{"env": map[string]any{"A": "b"}})
	if st != 502 || nativeErrCode(out) != "env_update_failed" {
		t.Fatalf("running set_env failure = %d %v, want env_update_failed", st, out)
	}
}

// 2: a no_env fork of a parent whose run is not no_env releases the child with
// fresh_boot; a no_env parent's child does not need it; a later no_env start of a
// cleanly booted child needs none either.
func TestR38NoEnvForkColdBoots(t *testing.T) {
	e := setup(t)
	parent := e.runningSecretVM()
	st, kid := e.do(e.owner, "POST", "/v1/vms/"+parent+"/fork", map[string]any{"name": "kid", "no_env": true})
	if st != 201 {
		t.Fatalf("fork = %d %v", st, kid)
	}
	child := kid["id"].(string)
	if e.freshReleasesOf(child) != 1 {
		t.Fatalf("fresh releases of %s = %d, want 1", child, e.freshReleasesOf(child))
	}
	if !e.noEnvRunOf(child) {
		t.Fatal("a cleanly booted no_env child is not marked")
	}
	e.do(e.owner, "POST", "/v1/vms/"+child+"/stop", nil)
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+child+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("child no_env start = %d %v", st, out)
	}
	if n := e.freshBootsOf(child); n != 0 {
		t.Fatalf("fresh boots of the cleanly booted child = %d, want 0", n)
	}
	// A converted no_env parent lacks protected lineage, so forking it cold-boots;
	// a parent born no_env has protected lineage and forks without a cold boot.
	np := e.noEnvVM()
	st, kid = e.do(e.owner, "POST", "/v1/vms/"+np+"/fork", map[string]any{"name": "kid2"})
	if st != 201 || e.freshReleasesOf(kid["id"].(string)) != 1 {
		t.Fatalf("fork of a converted no_env run = %d %v, fresh releases %d, want 1", st, kid, e.freshReleasesOf(kid["id"].(string)))
	}
	born := e.createVMWith(map[string]any{"no_env": true})
	st, kid = e.do(e.owner, "POST", "/v1/vms/"+born+"/fork", map[string]any{"name": "kid2b"})
	if st != 201 || e.freshReleasesOf(kid["id"].(string)) != 0 {
		t.Fatalf("fork of a born no_env run = %d %v, fresh releases %d, want none", st, kid, e.freshReleasesOf(kid["id"].(string)))
	}
	// A normal fork never asks for one.
	st, kid = e.do(e.owner, "POST", "/v1/vms/"+parent+"/fork", map[string]any{"name": "kid3"})
	if st != 201 || e.freshReleasesOf(kid["id"].(string)) != 0 {
		t.Fatalf("normal fork = %d %v", st, kid)
	}
	e.boatd.mu.Lock()
	last := e.boatd.releaseBodies[len(e.boatd.releaseBodies)-1]
	e.boatd.mu.Unlock()
	if last["fresh_boot"] != nil {
		t.Fatalf("normal release body = %v, want no fresh_boot", last)
	}
}

// 2: a fresh-boot release whose env push fails leaves the child stopped (forced,
// no memory), with no clean-run mark, and answers 502 env_start_failed. Its
// durable no_env policy stays sticky even though the run did not come up clean.
func TestR38NoEnvForkEnvFailureFailsClosed(t *testing.T) {
	e := setup(t)
	parent := e.runningSecretVM()
	e.setReleaseEnvFail(true)
	st, out := e.do(e.owner, "POST", "/v1/vms/"+parent+"/fork", map[string]any{"name": "kid", "no_env": true})
	if st != 502 || nativeErrCode(out) != "env_start_failed" {
		t.Fatalf("fork = %d %v, want 502 env_start_failed", st, out)
	}
	vm, _ := out["vm"].(map[string]any)
	child, _ := vm["id"].(string)
	if child == "" || vm["state"] != "stopped" {
		t.Fatalf("vm = %v, want the stopped child", out["vm"])
	}
	if s := e.boatdState(child); s != "stopped" {
		t.Fatalf("child = %s, want stopped", s)
	}
	e.boatd.mu.Lock()
	childVM := fakeVMMetadata(e.boatd.vms[child])
	e.boatd.mu.Unlock()
	if !e.srv.envNeverLanded(child) || e.srv.snapshotRunNoEnv(child, childVM) {
		t.Fatal("a failed fresh boot left the child marked clean for RAM reuse")
	}
	if !e.noEnvRunOf(child) {
		t.Fatal("a failed fresh boot lost the child's sticky no_env policy")
	}
	e.setReleaseEnvFail(false)
	// A later no_env start of the child cold-boots again (it was never marked clean).
	if st, o := e.do(e.owner, "POST", "/v1/vms/"+child+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("child start = %d %v", st, o)
	}
	if e.freshBootsOf(child) != 1 {
		t.Fatalf("fresh boots of the child = %d, want 1", e.freshBootsOf(child))
	}
}

// 2: a release that errors (before or after booting the child) leaves no running
// child and answers 502 env_start_failed.
func TestR38NoEnvForkReleaseErrorFailsClosed(t *testing.T) {
	for name, c := range map[string][2]int{"before-boot": {1, 0}, "after-boot": {0, 1}} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			parent := e.runningSecretVM()
			e.setReleaseFailures(c[0], c[1])
			st, out := e.do(e.owner, "POST", "/v1/vms/"+parent+"/fork", map[string]any{"name": "kid", "no_env": true})
			if st != 502 || nativeErrCode(out) != "env_start_failed" {
				t.Fatalf("fork = %d %v, want 502 env_start_failed", st, out)
			}
			e.boatd.mu.Lock()
			for id, vm := range e.boatd.vms {
				if id != parent && vm["state"] == "running" {
					t.Errorf("child %s is running after the failed release", id)
				}
			}
			e.boatd.mu.Unlock()
			if vm, ok := out["vm"].(map[string]any); ok {
				cid := vm["id"].(string)
				e.boatd.mu.Lock()
				childVM := fakeVMMetadata(e.boatd.vms[cid])
				e.boatd.mu.Unlock()
				if !e.srv.envNeverLanded(cid) || e.srv.snapshotRunNoEnv(cid, childVM) {
					t.Error("failed child is marked clean for RAM reuse")
				}
				if !e.noEnvRunOf(cid) {
					t.Error("failed child lost its sticky no_env policy")
				}
			}
		})
	}
}

// 3: no_env on a running VM that is not a no_env run is refused (stop first); a
// running no_env run may still start with no_env.
func TestR38StartNoEnvOnRunningRefused(t *testing.T) {
	e := setup(t)
	id := e.runningSecretVM()
	e.boatd.mu.Lock()
	ops := len(e.boatd.envOps)
	e.boatd.mu.Unlock()
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true})
	if st != 409 || nativeErrCode(out) != "stop_required" {
		t.Fatalf("native = %d %v, want 409 stop_required", st, out)
	}
	st, cout, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"noEnv": true}, nil)
	if st != 409 || cout["code"] != "sandbox_running_stop_first" { // r46
		t.Fatalf("compat = %d %v, want 409 sandbox_running_stop_first", st, cout)
	}
	e.boatd.mu.Lock()
	after := len(e.boatd.envOps)
	e.boatd.mu.Unlock()
	if after != ops {
		t.Fatalf("env ops %d -> %d: the refused start pushed an env", ops, after)
	}
	if e.noEnvRunOf(id) {
		t.Fatal("the refused start marked the run no_env")
	}
	// A no_env run keeps accepting no_env.
	nv := e.noEnvVM()
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+nv+"/start", map[string]any{"no_env": true}); st != 200 {
		t.Fatalf("no_env start of a no_env run = %d %v", st, out)
	}
}

// 4: while an env push is in flight the compat state is provisioning, even when an
// earlier push's error is remembered; the error text stays.
func TestR38CompatProvisioningOverRememberedError(t *testing.T) {
	e := setup(t)
	e.setEnvFailures(true, false)
	bx := e.newSandbox(map[string]any{"env": map[string]string{"A": "1"}})
	id := e.nativeOf(bx)
	sb := e.ok("GET", "/api/v1/sandboxes/"+bx, nil, 200)["sandbox"].(map[string]any)
	if sb["state"] != "error" {
		t.Fatalf("before = %v, want error", sb)
	}
	release := e.srv.markEnvPush(id)
	sb = e.ok("GET", "/api/v1/sandboxes/"+bx, nil, 200)["sandbox"].(map[string]any)
	if sb["state"] != "provisioning" || sb["error"] == nil {
		t.Fatalf("during the push = %v, want provisioning with the error text", sb)
	}
	release()
	sb = e.ok("GET", "/api/v1/sandboxes/"+bx, nil, 200)["sandbox"].(map[string]any)
	if sb["state"] != "error" {
		t.Fatalf("after the push = %v, want error", sb)
	}
}

// 5: a VM's own token never starts its VM, and the refusal rotates nothing.
func TestR38SelfTokenCannotStart(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example/"
	id := e.createVM()
	tok, _ := lastEnvOp(e, id)["BOAT_API_KEY"].(string)
	if tok == "" {
		t.Fatal("no self token injected")
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	gens := func() (int, int) {
		st := e.srv.store.Snapshot()
		g, a := 0, 0
		if t := st.VMTokens[id]; t != nil {
			g = t.Gen
		}
		if v := st.AIVMs[id]; v != nil {
			a = v.Gen
		}
		return g, a
	}
	g0, a0 := gens()
	for _, body := range []map[string]any{{}, {"no_env": true}} {
		st, out := e.raw(tok, "POST", "/v1/vms/"+id+"/start", body)
		if st != http.StatusForbidden {
			t.Fatalf("self-token start %v = %d %v, want 403", body, st, out)
		}
	}
	if g, a := gens(); g != g0 || a != a0 {
		t.Fatalf("token generations %d/%d -> %d/%d: a refused start rotated them", g0, a0, g, a)
	}
	if s := e.boatdState(id); s != "stopped" {
		t.Fatalf("VM = %s, want stopped", s)
	}
}
