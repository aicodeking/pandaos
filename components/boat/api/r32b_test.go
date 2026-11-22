package main

import "testing"

// r32b: native VM bodies expose no_env_run so clients restoring a run (boat
// resume --type after a failed resize) keep a no_env run secret-free.
func TestR32bNoEnvRunExposed(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	noEnvRun := func(where string) bool {
		t.Helper()
		st, out := e.do(e.owner, "GET", "/v1/vms/"+id, nil)
		if st != 200 {
			t.Fatalf("%s: GET = %d %v", where, st, out)
		}
		v, ok := out["no_env_run"].(bool)
		if !ok {
			t.Fatalf("%s: no_env_run missing in %v", where, out)
		}
		return v
	}
	if noEnvRun("normal create") {
		t.Fatal("a normal run reports no_env_run=true")
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", map[string]any{"no_env": true})
	if st != 200 || out["no_env_run"] != true {
		t.Fatalf("no_env start = %d %v, want no_env_run=true in the body", st, out)
	}
	if !noEnvRun("after no_env start") {
		t.Fatal("GET after a no_env start reports no_env_run=false")
	}
	e.do(e.owner, "POST", "/v1/vms/"+id+"/stop", nil)
	if st, _ := e.do(e.owner, "POST", "/v1/vms/"+id+"/start", nil); st != 200 {
		t.Fatalf("normal start = %d", st)
	}
	if !noEnvRun("after plain start") {
		t.Fatal("a plain cold start of a no_env VM reports no_env_run=false (sticky)")
	}
}
