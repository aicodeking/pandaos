package main

import (
	"testing"
)

// Native /v1 routes (and so the native CLI) accept a boat.dev bx_ id; another
// team's bx_ id is still 404.
func TestNativeRoutesResolveBxIDs(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	st, vm := e.do(e.owner, "GET", "/v1/vms/"+bx, nil)
	if st != 200 || vm["id"] == bx || vm["id"] == nil {
		t.Fatalf("GET /v1/vms/%s: %d %v", bx, st, vm)
	}
	if st, _ := e.do(otherTeamKey(e), "GET", "/v1/vms/"+bx, nil); st != 404 {
		t.Errorf("other team: %d, want 404", st)
	}
	if st, _ := e.do(e.owner, "GET", "/v1/vms/bx_22222222", nil); st != 404 {
		t.Errorf("unknown bx_: %d, want 404", st)
	}
}
