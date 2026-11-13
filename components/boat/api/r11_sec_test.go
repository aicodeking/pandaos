package main

import (
	"net/http"
	"testing"
)

// A public snapshot link must die with the key that minted it.
func TestShareLinkDiesWithKey(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	sid := e.snapshotOf(id)
	st, k := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "sharer"})
	if st != 201 {
		t.Fatalf("key: %d %v", st, k)
	}
	st, sh := e.do(k["token"].(string), "POST", "/v1/vms/"+id+"/snapshots/"+sid+"/shares", map[string]any{})
	if st != 201 {
		t.Fatalf("share: %d %v", st, sh)
	}
	link := "/v1/snapshot-links/" + sh["code"].(string) + "/download"
	if st, _ := e.raw("", "GET", link, nil); st != 200 {
		t.Fatalf("live link: %d", st)
	}
	e.do(e.owner, "DELETE", "/v1/keys/"+k["id"].(string), nil)
	if st, _ := e.raw("", "GET", link, nil); st == 200 {
		t.Errorf("link outlived its key: %d", st)
	}
}

func (e *env) selfTokenOf(vm string) string {
	e.t.Helper()
	tok, _ := lastEnvOp(e, vm)["BOAT_API_KEY"].(string)
	if tok == "" {
		e.t.Fatal("no self token")
	}
	return tok
}

// A VM's own token cannot open public ports or add ssh keys unless the team allows it.
func TestSelfTokenPublicPortAndSSH(t *testing.T) {
	e := setup(t)
	e.srv.cfg.VMAPIURL = "https://boat.example/"
	vm := e.createVM()
	self := e.selfTokenOf(vm)
	if st, out := e.do(self, "POST", "/v1/vms/"+vm+"/ports", map[string]any{"port": 8080, "auth": "public"}); st != 403 {
		t.Errorf("self token opened a public port: %d %v", st, out)
	}
	if st, out := e.do(self, "POST", "/v1/vms/"+vm+"/ssh", map[string]any{"public_key": r11Pub}); st != 403 {
		t.Errorf("self token added an ssh key: %d %v", st, out)
	}
	if st, _ := e.do(self, "POST", "/v1/vms/"+vm+"/ports", map[string]any{"port": 8081}); st != 201 {
		t.Errorf("self token private port: %d", st)
	}
	if st, out := e.do(e.owner, "PUT", "/v1/teams/"+e.teamID()+"/self-token", map[string]any{"public_ports": true, "ssh": true}); st != http.StatusOK {
		t.Fatalf("policy: %d %v", st, out)
	}
	if _, tm := e.do(e.owner, "GET", "/v1/teams/"+e.teamID(), nil); tm["self_token"].(map[string]any)["ssh"] != true {
		t.Errorf("team view: %v", tm)
	}
	if st, out := e.do(self, "POST", "/v1/vms/"+vm+"/ports", map[string]any{"port": 8080, "auth": "public"}); st != 201 {
		t.Errorf("allowed public port: %d %v", st, out)
	}
	if st, out := e.do(self, "POST", "/v1/vms/"+vm+"/ssh", map[string]any{"public_key": r11Pub}); st != 200 {
		t.Errorf("allowed ssh: %d %v", st, out)
	}
}
