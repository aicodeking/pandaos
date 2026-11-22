package main

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
)

func TestR48NarrowedResumeSelfToken(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example"
	for _, name := range []string{"PROD", "DEV"} {
		e.do(e.owner, "PUT", "/v1/secrets/"+name, map[string]any{"value": name, "inject": true})
	}
	e.saveEnv("dev", map[string]string{}, []string{"DEV"})
	bx := e.newSandbox(nil)
	vm := e.nativeOf(bx)
	native, compat := vmPath(vm), "/api/v1/sandboxes/"+bx
	e.ok("POST", compat+"/commands", map[string]any{"command": "echo PROD", "detached": true}, 200)
	sid := e.r39Snapshot(vm, "prod")
	e.do(e.owner, "POST", native+"/stop", nil)
	if st, out := e.do(e.owner, "POST", native+"/start", map[string]any{"environment": "dev"}); st != 200 {
		t.Fatalf("narrow start = %d %v", st, out)
	}
	e.boatd.mu.Lock()
	fresh := slices.Contains(e.boatd.freshBoots, vm)
	e.boatd.mu.Unlock()
	if !fresh {
		t.Fatal("narrow start did not cold boot")
	}
	if e.srv.noEnvRun(vm) {
		t.Fatal("narrowing must not make the VM no_env")
	}
	tok := e.currentSelfToken(vm)
	for _, path := range []string{
		native + "/snapshots", native + "/snapshots/" + sid + "/download",
		native + "/commands", native + "/command-history", native + "/prompts",
		compat + "/snapshots", compat + "/command-history", compat + "/commands/1",
	} {
		if st, body := e.raw(tok, "GET", path, nil); st != http.StatusForbidden {
			t.Errorf("narrow token GET %s = %d %s", path, st, body)
		}
	}
	for _, rec := range e.srv.store.Snapshot().Commands[vm] {
		if st, body := e.raw(tok, "GET", native+"/commands/"+rec.ID, nil); st != http.StatusForbidden {
			t.Errorf("earlier command = %d %s", st, body)
		}
	}
	st, own, _ := e.compat(tok, "POST", compat+"/commands", map[string]any{"command": "echo mine", "detached": true}, nil)
	if st != 200 {
		t.Fatalf("own command = %d %v", st, own)
	}
	path := fmt.Sprintf("%s/commands/%.0f", compat, own["processId"])
	if st, body := e.raw(tok, "GET", path, nil); st != 200 {
		t.Errorf("own command status = %d %s", st, body)
	}
	// Snapshotting rotates a running token; that must not release the restriction.
	e.r39Snapshot(vm, "dev")
	tok = e.currentSelfToken(vm)
	if st, body := e.raw(tok, "GET", native+"/snapshots", nil); st != http.StatusForbidden {
		t.Errorf("rotated narrow token = %d %s", st, body)
	}
	e.do(e.owner, "POST", native+"/stop", nil)
	if st, body := e.raw(tok, "GET", native+"/command-history", nil); st != http.StatusForbidden {
		t.Errorf("stopped narrow token = %d %s", st, body)
	}
}

func TestR48DurableSecretSelection(t *testing.T) {
	for _, noEnv := range []bool{false, true} {
		for _, fork := range []bool{false, true} {
			t.Run(fmt.Sprintf("no_env=%v/fork=%v", noEnv, fork), func(t *testing.T) {
				e := setup(t)
				e.do(e.owner, "PUT", "/v1/secrets/PROD", map[string]any{"value": "secret", "inject": true})
				id := e.createVMWith(map[string]any{"no_env": noEnv})
				if fork {
					st, out := e.do(e.owner, "POST", vmPath(id, "fork"), map[string]any{})
					if st != 201 {
						t.Fatalf("fork = %d %v", st, out)
					}
					id = out["id"].(string)
				}
				e.boatd.mu.Lock()
				labels := e.boatd.vms[id]["labels"].(map[string]any)
				if labels["boat.sel"] != "1" {
					t.Errorf("missing durable selection marker: %v", labels)
				}
				if noEnv && (labels["boat.no_env"] != "1" || labels[secretsLabel] != "") {
					t.Errorf("missing durable no_env labels: %v", labels)
				}
				if !noEnv {
					delete(labels, secretsLabel) // a selection too large for the label
				}
				e.boatd.mu.Unlock()
				if err := e.srv.store.Update(func(st *State) error {
					delete(st.VMSecrets, id)
					delete(st.NoEnvRuns, id)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if got := e.srv.noEnvRun(id); got != noEnv {
					t.Errorf("durable no_env = %v, want %v", got, noEnv)
				}
				e.stopStart(id, nil)
				if got := e.guestVar(id, "PROD"); got != nil && got != "" {
					t.Errorf("lost record released PROD: %v", got)
				}
				if noEnv {
					if st, out := e.do(e.owner, "POST", vmPath(id, "start"), map[string]any{"no_env": false}); st != 400 || r46Code(out) != errNoEnvSticky {
						t.Errorf("lost-record sticky start = %d %v", st, out)
					}
				}
			})
		}
	}
}

func TestR48SelectionLegacyAndReservedLabels(t *testing.T) {
	e := setup(t)
	if got := e.srv.vmSecretsOf("legacy", VM{}); got != nil {
		t.Fatalf("legacy fallback = %v, want all", got)
	}
	for _, key := range []string{"boat.sel", "boat.no_env"} {
		if st, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"labels": map[string]string{key: "1"}}); st != 400 {
			t.Errorf("reserved label %s = %d %v", key, st, out)
		}
	}
}
