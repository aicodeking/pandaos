package main

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const r11Pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl me@laptop"

func (e *env) addMember(role string) (tok, userID string) {
	st, m := e.do(e.owner, "POST", "/v1/teams/"+e.teamID()+"/members", map[string]any{"name": "m" + randHex(3), "role": role})
	if st != 201 {
		e.t.Fatalf("add member: %d %v", st, m)
	}
	return m["key"].(map[string]any)["token"].(string), m["user"].(map[string]any)["id"].(string)
}

func (e *env) jumpSetup() (string, string) {
	jf := filepath.Join(e.t.TempDir(), "jump")
	e.srv.cfg.JumpKeysFile = jf
	return jf, e.createVM()
}

// sshRevoked: the key is gone from the jump file, the VM and the grant list.
func (e *env) sshRevoked(jf, vm string) {
	e.t.Helper()
	if b, _ := os.ReadFile(jf); strings.Contains(string(b), "AAAAC3NzaC1lZDI1NTE5AAAAIOMqq") {
		e.t.Errorf("jump file still permits the key: %s", b)
	}
	e.boatd.mu.Lock()
	ak := string(e.boatd.files[vm+":/home/dev/.ssh/authorized_keys"])
	e.boatd.mu.Unlock()
	if strings.Contains(ak, "AAAAC3NzaC1lZDI1NTE5AAAAIOMqq") {
		e.t.Errorf("vm authorized_keys still has the key: %q", ak)
	}
	if n := len(e.srv.store.Snapshot().SSHGrants); n != 0 {
		e.t.Errorf("grants left: %d", n)
	}
}

func (e *env) grantSSH(tok, vm string) {
	e.t.Helper()
	if st, out := e.do(tok, "POST", "/v1/vms/"+vm+"/ssh", map[string]any{"public_key": r11Pub}); st != 200 {
		e.t.Fatalf("ssh: %d %v", st, out)
	}
}

func TestSSHGrantDiesWithMember(t *testing.T) {
	e := setup(t)
	jf, vm := e.jumpSetup()
	tok, uid := e.addMember("member")
	e.grantSSH(tok, vm)
	if b, _ := os.ReadFile(jf); !strings.Contains(string(b), "AAAAC3NzaC1lZDI1NTE5AAAAIOMqq") {
		t.Fatalf("grant not in jump file: %s", b)
	}
	if st, out := e.do(e.owner, "DELETE", "/v1/teams/"+e.teamID()+"/members/"+uid, nil); st != 200 {
		t.Fatalf("remove member: %d %v", st, out)
	}
	e.sshRevoked(jf, vm)
}

func TestSSHGrantDiesWithKey(t *testing.T) {
	e := setup(t)
	jf, vm := e.jumpSetup()
	st, k := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "laptop"})
	if st != 201 {
		t.Fatalf("key: %d %v", st, k)
	}
	e.grantSSH(k["token"].(string), vm)
	if st, out := e.do(e.owner, "DELETE", "/v1/keys/"+k["id"].(string), nil); st != 200 {
		t.Fatalf("delete key: %d %v", st, out)
	}
	e.sshRevoked(jf, vm)
}

func TestSSHGrantDiesWithKeyExpiry(t *testing.T) {
	e := setup(t)
	jf, vm := e.jumpSetup()
	st, k := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "short", "expires_in_s": 60})
	if st != 201 {
		t.Fatalf("key: %d %v", st, k)
	}
	e.grantSSH(k["token"].(string), vm)
	e.srv.store.Update(func(st *State) error {
		past := time.Now().Add(-time.Second)
		nk := *st.Keys[k["id"].(string)]
		nk.Expires = &past
		st.Keys[nk.ID] = &nk
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	go e.srv.jumpExpiryLoop(ctx, 10*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(jf); len(e.srv.store.Snapshot().SSHGrants) == 0 && len(b) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	e.sshRevoked(jf, vm)
}

func TestSSHGrantListRevoke(t *testing.T) {
	e := setup(t)
	jf, vm := e.jumpSetup()
	alice, _ := e.addMember("member")
	bob, _ := e.addMember("member")
	e.grantSSH(alice, vm)
	e.grantSSH(bob, vm) // same key, second owner: one jump line
	if b, _ := os.ReadFile(jf); strings.Count(string(b), "\n") != 1 {
		t.Errorf("one key must render one line: %q", b)
	}
	// a pre-ownership grant: listed unowned, only an admin revokes it
	e.srv.store.Update(func(st *State) error {
		pub := strings.Join(strings.Fields(r11Pub)[:2], " ")
		st.SSHGrants[grantKey(e.teamID(), pub, "")] = &SSHGrant{TeamID: e.teamID(), PublicKey: pub, VMs: []string{vm}}
		return nil
	})
	_, out := e.do(alice, "GET", "/v1/vms/"+vm+"/ssh", nil)
	keys, _ := out["keys"].([]any)
	unowned := 0
	for _, k := range keys {
		if k.(map[string]any)["unowned"] == true {
			unowned++
		}
	}
	if len(keys) != 3 || unowned != 1 {
		t.Fatalf("list: %v", out)
	}
	fp := keys[0].(map[string]any)["fingerprint"].(string)
	if fp != sshFingerprint(r11Pub) || !strings.HasPrefix(fp, "SHA256:") {
		t.Errorf("fingerprint %q", fp)
	}
	if st, _ := e.do(alice, "DELETE", "/v1/vms/"+vm+"/ssh/"+url.PathEscape(fp), nil); st != 403 {
		t.Errorf("member revoked other members' grants: %d", st)
	}
	if st, _ := e.do(e.owner, "DELETE", "/v1/vms/"+vm+"/ssh/nope", nil); st != 404 {
		t.Errorf("unknown fingerprint: %d", st)
	}
	if st, out := e.do(e.owner, "DELETE", "/v1/vms/"+vm+"/ssh/"+url.PathEscape(fp), nil); st != 200 {
		t.Fatalf("admin revoke: %d %v", st, out)
	}
	e.sshRevoked(jf, vm)
}
