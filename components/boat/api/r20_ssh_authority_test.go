package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type r20SSHTransport func(*http.Request) (*http.Response, error)

func (f r20SSHTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func r20NoSSH(t *testing.T, e *env, vm, jump string) {
	t.Helper()
	e.boatd.mu.Lock()
	guest := string(e.boatd.files[vm+":"+e.srv.authorizedKeysPath()])
	e.boatd.mu.Unlock()
	if strings.Contains(guest, strings.Fields(r11Pub)[1]) {
		t.Errorf("guest retains SSH key: %q", guest)
	}
	if b, err := os.ReadFile(jump); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	} else if strings.Contains(string(b), strings.Fields(r11Pub)[1]) {
		t.Errorf("jump retains SSH key: %q", b)
	}
	back, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	for _, st := range []*State{e.srv.store.Snapshot(), back.Snapshot()} {
		if len(st.SSHGrants) != 0 {
			t.Errorf("SSH grants remain in memory or durable reload: %+v", st.SSHGrants)
		}
	}
}

func TestR20SSHBodyAuthority(t *testing.T) {
	for _, change := range []string{"rotate", "delete", "scope", "expire", "overlap"} {
		t.Run(change, func(t *testing.T) {
			e := setup(t)
			jump, vm := e.jumpSetup()
			token := e.newKey("exec")
			id, _, _ := ParseToken(token)
			status, _, _ := r19PausedIssuance(t, e, token, vmPath(vm, "ssh"), map[string]any{"public_key": r11Pub}, func() {
				switch change {
				case "rotate", "overlap":
					overlap := 0
					if change == "overlap" {
						overlap = 60
					}
					if code, out := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": overlap}); code != 200 {
						t.Fatalf("rotate: %d %v", code, out)
					}
					want := 401
					if overlap > 0 {
						want = 200
					}
					if code, _ := e.do(token, "GET", "/v1/me", nil); code != want {
						t.Fatalf("GET me=%d want %d", code, want)
					}
				case "delete":
					if code, _ := e.do(e.owner, "DELETE", "/v1/keys/"+id, nil); code != 200 {
						t.Fatalf("delete=%d", code)
					}
				default:
					if err := e.srv.store.Update(func(st *State) error {
						k := st.Keys[id]
						if change == "scope" {
							k.Scopes = []string{"vms:read"}
						} else {
							past := time.Now().Add(-time.Second)
							k.Expires = &past
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
			})
			if change == "overlap" {
				if status != 200 {
					t.Fatalf("valid overlap=%d", status)
				}
				e.grantSSH(token, vm)
				return
			}
			if status != 401 && status != 403 {
				t.Errorf("revoked authority HTTP=%d", status)
			}
			r20NoSSH(t, e, vm, jump)
		})
	}
}

func TestR20SSHSelfAuthority(t *testing.T) {
	for _, change := range []string{"allowed", "generation", "policy", "retired"} {
		t.Run(change, func(t *testing.T) {
			e := setup(t)
			e.srv.cfg.VMAPIURL = "https://boat.example/"
			jump, vm := e.jumpSetup()
			token := e.selfTokenOf(vm)
			if code, out := e.do(e.owner, "PUT", "/v1/teams/"+e.teamID()+"/self-token", map[string]any{"ssh": true}); code != 200 {
				t.Fatalf("policy: %d %v", code, out)
			}
			code, _, _ := r19PausedIssuance(t, e, token, vmPath(vm, "ssh"), map[string]any{"public_key": r11Pub}, func() {
				if change == "allowed" {
					return
				}
				if err := e.srv.store.Update(func(st *State) error {
					switch change {
					case "generation":
						st.cowVMToken(vm).Gen++
					case "retired":
						st.cowVMToken(vm).Retired = true
					case "policy":
						st.Teams[e.teamID()].SelfToken.SSH = false
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
			if change == "allowed" {
				if code != 200 {
					t.Fatalf("allowed self SSH=%d", code)
				}
				if _, err := e.srv.bumpSelf(vm, e.teamID()); err != nil {
					t.Fatal(err)
				}
				if err := e.srv.syncJumpKeysErr(context.Background()); err != nil {
					t.Fatal(err)
				}
				if len(e.srv.store.Snapshot().SSHGrants) != 1 {
					t.Error("generation change revoked already committed durable self SSH grant")
				}
			} else {
				if code != 403 {
					t.Errorf("revoked self SSH=%d", code)
				}
				r20NoSSH(t, e, vm, jump)
			}
		})
	}
}

func TestR20SSHOriginalAuthority(t *testing.T) {
	for _, change := range []string{"expiry", "scopes"} {
		t.Run(change, func(t *testing.T) {
			e := setup(t)
			jump, vm := e.jumpSetup()
			token := e.newKey("exec")
			p := e.srv.authenticate("Bearer " + token)
			if change == "expiry" {
				key := *p.Key
				past := time.Now().Add(-time.Second)
				key.Expires = &past
				p.Key = &key
			} else {
				p.Scopes = []string{"vms:read"}
			}
			req, _ := http.NewRequest("POST", "http://test", nil)
			if err := e.srv.grantJump(req, p, vm, r11Pub); !errors.Is(err, errSSHAuthority) {
				t.Fatalf("original authority accepted: %v", err)
			}
			r20NoSSH(t, e, vm, jump)
		})
	}
}

func TestR20SSHCleanupIntentCap(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			e := setup(t)
			_, vm := e.jumpSetup()
			token := e.newKey("exec")
			e.grantSSH(token, vm)
			if err := e.srv.store.Update(func(st *State) error {
				for i := 1; i < maxGrantsPerTeam+extra; i++ {
					id := pendingSSHKeyPrefix + fmt.Sprint(i)
					g := &SSHGrant{TeamID: e.teamID(), PublicKey: "ssh-ed25519 AAAAAAAAAAAAAAAAAAAA", KeyID: id, VMs: []string{vm}}
					st.SSHGrants[grantKey(g.TeamID, g.PublicKey, id)] = g
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// Denied refresh must not run external publication or grow durable intent.
			before := e.srv.store.Snapshot()
			code, _ := e.do(token, "POST", vmPath(vm, "ssh"), map[string]any{"public_key": r11Pub})
			if extra == 0 {
				if code != 200 {
					t.Errorf("existing grant refresh at cap=%d", code)
				}
			} else {
				if code/100 == 2 {
					t.Error("existing grant bypassed bounded pending-intent capacity")
				}
				if e.srv.store.Snapshot() != before {
					t.Error("capacity rejection changed durable state")
				}
			}
		})
	}
}

func (e *env) requireSSHInstall(vm string) {
	e.boatd.mu.Lock()
	e.boatd.vms[vm]["ssh_policy_ready"] = false
	e.boatd.mu.Unlock()
}

func TestR20SSHRotationDuringGuestWrite(t *testing.T) {
	for _, failure := range []string{"rotate", "commit", "prepare", "install", "attestation", "boot"} {
		t.Run(failure, func(t *testing.T) {
			e := setup(t)
			jump, vm := e.jumpSetup()
			token := e.newKey("exec")
			id, _, _ := ParseToken(token)
			e.requireSSHInstall(vm)
			statePath := filepath.Join(e.dir, "state.json")
			blocked := false
			block := func() {
				t.Helper()
				if err := os.Rename(statePath, statePath+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(statePath, 0700); err != nil {
					t.Fatal(err)
				}
				blocked = true
			}
			restore := func() {
				if blocked {
					if err := os.Remove(statePath); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(statePath+".saved", statePath); err != nil {
						t.Fatal(err)
					}
					blocked = false
				}
			}
			defer restore()
			if failure == "prepare" {
				block()
			}
			installs := 0
			e.srv.boatd.hc.Transport = r20SSHTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" && r.URL.Path == vmPath(vm, "ssh-policy") {
					installs++
					if failure == "install" {
						return nil, errors.New("installation unavailable")
					}
					resp, err := http.DefaultTransport.RoundTrip(r)
					if err != nil {
						return resp, err
					}
					switch failure {
					case "rotate":
						if c, _ := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": 0}); c != 200 {
							t.Errorf("rotate=%d", c)
						}
					case "commit":
						block()
					case "boot":
						e.boatd.mu.Lock()
						e.boatd.vms[vm]["ssh_boot_epoch"] = randHex(16)
						e.boatd.mu.Unlock()
					case "attestation":
						resp.Body.Close()
						resp.Body = io.NopCloser(strings.NewReader(`{"version":1,"readiness":true,"boot_epoch":"wrong","config_digest":"wrong"}`))
					}
					// A successfully installed helper still cannot authorize an uncommitted key.
					if failure != "boot" && failure != "attestation" {
						e.assertSSH(vm, r11Pub, 204)
					}
					return resp, err
				}
				return http.DefaultTransport.RoundTrip(r)
			})
			code, _ := e.do(token, "POST", vmPath(vm, "ssh"), map[string]any{"public_key": r11Pub})
			if code/100 == 2 {
				t.Fatalf("failed/revoked registration succeeded: %d", code)
			}
			restore()
			e.srv.boatd.hc.Transport = http.DefaultTransport
			// A crash/reload can only recover committed authority, never the helper installation.
			back, err := OpenStore(e.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer back.Close()
			for _, st := range []*State{e.srv.store.Snapshot(), back.Snapshot()} {
				if len(liveSSHGrants(st)) != 0 {
					t.Fatal("failed commit persisted SSH authority")
				}
			}
			if failure != "boot" {
				r20NoSSH(t, e, vm, jump)
			}
			if installs == 0 {
				t.Fatal("policy preparation not exercised")
			}
		})
	}
}

func TestR20SSHReadBeforeWriteAuthority(t *testing.T) {
	e := setup(t)
	jump, vm := e.jumpSetup()
	token := e.newKey("exec")
	id, _, _ := ParseToken(token)
	e.requireSSHInstall(vm)
	var once sync.Once
	e.srv.boatd.hc.Transport = r20SSHTransport(func(r *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(r)
		if r.Method == "GET" && r.URL.Path == vmPath(vm) {
			once.Do(func() {
				if c, _ := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": 0}); c != 200 {
					t.Errorf("rotate=%d", c)
				}
			})
		}
		return resp, err
	})
	if code, _ := e.do(token, "POST", vmPath(vm, "ssh"), map[string]any{"public_key": r11Pub}); code != 403 {
		t.Fatalf("revoked during inventory read=%d", code)
	}
	r20NoSSH(t, e, vm, jump)
}

func TestR20SSHRevokeCleanupReload(t *testing.T) {
	e := setup(t)
	jump, vm := e.jumpSetup()
	token := e.newKey("exec")
	e.grantSSH(token, vm)
	e.assertSSH(vm, r11Pub, 200)
	e.requireSSHInstall(vm)
	e.srv.boatd.hc.Transport = r20SSHTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" && r.URL.Path == vmPath(vm, "ssh-policy") {
			return nil, errors.New("guest offline")
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	if code, _ := e.do(token, "DELETE", vmPath(vm, "ssh", url.PathEscape(sshFingerprint(r11Pub))), nil); code != 200 {
		t.Fatalf("revoke=%d", code)
	}
	back, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if len(back.Snapshot().SSHGrants) != 1 || len(liveSSHGrants(back.Snapshot())) != 0 {
		t.Fatal("revocation lost original cleanup debt or retained authority")
	}
	e.srv.store = back
	// Readiness denial is independent of failed cleanup; restored readiness still denies the revoked key.
	e.srv.boatd.hc.Transport = http.DefaultTransport
	if _, err := e.srv.prepareSSHPolicy(context.Background(), vm, e.teamID()); err != nil {
		t.Fatal(err)
	}
	e.assertSSH(vm, r11Pub, 204)
	if c, _ := e.jumpSSH(r11Pub); c != 204 {
		t.Fatalf("revoked jump=%d", c)
	}
	if err := e.srv.syncJumpKeysErr(context.Background()); err != nil {
		t.Fatal(err)
	}
	r20NoSSH(t, e, vm, jump)
}

func TestR20SSHBlockedGuestOrdering(t *testing.T) {
	for _, operation := range []string{"rotate", "delete-key", "sync"} {
		t.Run(operation, func(t *testing.T) {
			e := setup(t)
			jump, vm := e.jumpSetup()
			token := e.newKey("exec")
			id, _, _ := ParseToken(token)
			e.requireSSHInstall(vm)
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			e.srv.boatd.hc.Transport = r20SSHTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" && r.URL.Path == vmPath(vm, "ssh-policy") {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}
				return http.DefaultTransport.RoundTrip(r)
			})
			result := make(chan int, 1)
			go func() {
				c, _ := e.do(token, "POST", vmPath(vm, "ssh"), map[string]any{"public_key": r11Pub})
				result <- c
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("policy install not reached")
			}
			done := make(chan int, 1)
			go func() {
				switch operation {
				case "rotate":
					c, _ := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": 0})
					done <- c
				case "delete-key":
					c, _ := e.do(e.owner, "DELETE", "/v1/keys/"+id, nil)
					done <- c
				case "sync":
					if err := e.srv.syncJumpKeysErr(context.Background()); err != nil {
						done <- 500
					} else {
						done <- 200
					}
				}
			}()
			select {
			case c := <-done:
				if c != 200 {
					t.Fatalf("%s=%d", operation, c)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("invalidation blocked behind guest I/O")
			}
			unblock()
			select {
			case c := <-result:
				want := 403
				if operation == "sync" {
					want = 200
				}
				if c != want {
					t.Fatalf("registration=%d want %d", c, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("registration deadlocked")
			}
			if operation == "sync" {
				e.assertSSH(vm, r11Pub, 200)
			} else {
				r20NoSSH(t, e, vm, jump)
			}
		})
	}
}

func TestR20SSHFailedRequestPreservesOtherGrant(t *testing.T) {
	e := setup(t)
	_, vm := e.jumpSetup()
	other := e.newKey("exec")
	e.grantSSH(other, vm)
	token := e.newKey("exec")
	id, _, _ := ParseToken(token)
	e.requireSSHInstall(vm)
	var once sync.Once
	e.srv.boatd.hc.Transport = r20SSHTransport(func(r *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(r)
		if r.Method == "POST" && r.URL.Path == vmPath(vm, "ssh-policy") {
			once.Do(func() { e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": 0}) })
		}
		return resp, err
	})
	if c, _ := e.do(token, "POST", vmPath(vm, "ssh"), map[string]any{"public_key": r11Pub}); c != 403 {
		t.Fatalf("revoked request=%d", c)
	}
	if len(e.srv.store.Snapshot().SSHGrants) != 1 {
		t.Fatal("failed request modified independent grant")
	}
	e.assertSSH(vm, r11Pub, 200)
	if c, lines := e.jumpSSH(r11Pub); c != 200 || len(lines) != 1 {
		t.Fatalf("independent jump=%d %v", c, lines)
	}
}

func TestR20SSHFinalCommitAuthority(t *testing.T) {
	e := setup(t)
	jump, vm := e.jumpSetup()
	token := e.newKey("exec")
	id, _, _ := ParseToken(token)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var reads atomic.Int32
	e.srv.boatd.hc.Transport = r20SSHTransport(func(r *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(r)
		if r.Method == "GET" && r.URL.Path == vmPath(vm) && reads.Add(1) == 3 {
			once.Do(func() { close(entered); <-release })
		}
		return resp, err
	})
	result := make(chan int, 1)
	go func() {
		c, _ := e.do(token, "POST", vmPath(vm, "ssh"), map[string]any{"public_key": r11Pub})
		result <- c
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("final inventory read not reached")
	}
	if b, _ := os.ReadFile(jump); len(b) != 0 {
		t.Fatal("candidate key published before commit")
	}
	if len(liveSSHGrants(e.srv.store.Snapshot())) != 0 {
		t.Fatal("grant visible before final commit")
	}
	if err := e.srv.store.Update(func(st *State) error {
		k := st.Keys[id]
		k.Salt = randHex(16)
		k.Hash = e.srv.keys.HashSecret(k.Salt, randHex(32))
		k.PrevSalt = ""
		k.PrevHash = ""
		k.PrevUntil = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case c := <-result:
		if c != 403 {
			t.Fatalf("final commit accepted revoked credential=%d", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("commit deadlocked")
	}
	r20NoSSH(t, e, vm, jump)
}
