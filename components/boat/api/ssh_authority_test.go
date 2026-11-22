package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSSHAuthorityQuarantinesUnowned(t *testing.T) {
	st := &State{}
	if grantAlive(st, &SSHGrant{TeamID: "team", PublicKey: r11Pub}, time.Now()) {
		t.Fatal("historical unowned grant authorizes managed SSH")
	}
}

func TestSSHAuthorityLookupAudience(t *testing.T) {
	e := setup(t)
	_, vm := e.jumpSetup()
	code, _ := e.do(e.owner, "POST", vmPath(vm, "ssh", "authorize"), map[string]string{"user": "dev", "key_type": "ssh-ed25519", "key_blob": strings.Fields(r11Pub)[1]})
	if code != 401 {
		t.Fatalf("regular bearer lookup status=%d want 401", code)
	}
}

func TestSSHAuthorityInternalRejectsForwarded(t *testing.T) {
	e := setup(t)
	for _, remote := range []string{"192.0.2.1:1234", "127.0.0.1:1234"} {
		r := httptest.NewRequest(http.MethodPost, "/internal/ssh/authorize", strings.NewReader(`{"user":"jump","key_type":"ssh-ed25519","key_blob":"AAAAAAAAAAAAAAAAAAAA"}`))
		r.RemoteAddr = remote
		if strings.HasPrefix(remote, "127.") {
			r.Header.Set("X-Forwarded-For", "192.0.2.1")
		}
		w := httptest.NewRecorder()
		e.srv.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Errorf("remote %s forwarded=%s status=%d", remote, r.Header.Get("X-Forwarded-For"), w.Code)
		}
	}
}

// The fake installs the same wire policy without executing guest software.
func (e *env) enableSSH() {
	e.t.Helper()
	enableSSHConfig(e.t, &e.srv.cfg)
}

func enableSSHConfig(t *testing.T, cfg *Config) {
	t.Helper()
	if cfg.SSHPolicyHelperFile != "" {
		return
	}
	p := filepath.Join(t.TempDir(), "ssh-helper")
	if err := os.WriteFile(p, []byte("mock-policy-helper"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.SSHPolicyHelperFile = p
	cfg.VMAPIURL = "https://boat.example"
}

func (e *env) lookupSSH(vm, pub string) (int, map[string]any) {
	e.t.Helper()
	var config map[string]any
	e.boatd.mu.Lock()
	b := slices.Clone(e.boatd.files[vm+":/etc/boat/ssh-auth.json"])
	e.boatd.mu.Unlock()
	if err := json.Unmarshal(b, &config); err != nil {
		e.t.Fatal(err)
	}
	f := strings.Fields(pub)
	return e.do(config["token"].(string), "POST", vmPath(vm, "ssh", "authorize"), map[string]string{"user": "dev", "key_type": f[0], "key_blob": f[1]})
}

func (e *env) jumpSSH(pub string) (int, []string) {
	e.t.Helper()
	f := strings.Fields(pub)
	code, out := e.do("", "POST", "/internal/ssh/authorize", map[string]string{"user": "jump", "key_type": f[0], "key_blob": f[1]})
	var lines []string
	if a, ok := out["authorized_keys"].([]any); ok {
		for _, v := range a {
			lines = append(lines, v.(string))
		}
	}
	return code, lines
}

func (e *env) assertSSH(vm, pub string, want int) {
	e.t.Helper()
	code, out := e.lookupSSH(vm, pub)
	if code != want {
		e.t.Fatalf("lookup %s=%d want %d: %v", vm, code, want, out)
	}
	if want == 200 && out["authorized_key"] != strings.Join(strings.Fields(pub)[:2], " ") {
		e.t.Fatalf("lookup changed key: %v", out)
	}
}

func TestSSHAuthorityLifecycleIdentity(t *testing.T) {
	e := setup(t)
	_, vm := e.jumpSetup()
	e.grantSSH(e.owner, vm)
	e.assertSSH(vm, r11Pub, 200)
	e.boatd.mu.Lock()
	old := slices.Clone(e.boatd.files[vm+":/etc/boat/ssh-auth.json"])
	e.boatd.mu.Unlock()
	var config map[string]any
	json.Unmarshal(old, &config)
	token := config["token"].(string)
	if c, _ := e.do(token, "GET", vmPath(vm), nil); c != 401 {
		t.Fatalf("lookup bearer accepted regular route=%d", c)
	}
	other := e.createVM()
	f := strings.Fields(r11Pub)
	body := map[string]string{"user": "dev", "key_type": f[0], "key_blob": f[1]}
	if c, _ := e.do(token, "POST", vmPath(other, "ssh", "authorize"), body); c != 401 {
		t.Fatalf("copied token accepted other VM=%d", c)
	}
	if _, err := e.srv.bumpSelf(vm, e.teamID()); err != nil {
		t.Fatal(err)
	}
	if c, _ := e.do(token, "POST", vmPath(vm, "ssh", "authorize"), body); c != 401 {
		t.Fatalf("old generation lookup=%d", c)
	}
	if err := e.srv.refreshCredentials(context.Background(), vm); err != nil {
		t.Fatal(err)
	}
	e.assertSSH(vm, r11Pub, 200)
	if len(e.srv.store.Snapshot().SSHGrants) != 1 {
		t.Fatal("rotation discarded durable grant")
	}
	e.boatd.mu.Lock()
	e.boatd.vms[vm]["ssh_boot_epoch"] = randHex(16)
	e.boatd.vms[vm]["ssh_policy_ready"] = false
	e.boatd.mu.Unlock()
	if c, _ := e.lookupSSH(vm, r11Pub); c != 401 {
		t.Fatalf("copied old boot lookup=%d", c)
	}
	if c, _ := e.jumpSSH(r11Pub); c != 204 {
		t.Fatalf("unready boot jump=%d", c)
	}
}

func TestSSHAuthorityOverrideStillProvisions(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	code, out := e.do(e.owner, "POST", "/v1/vms", map[string]any{"name": "override", "env": map[string]string{"BOAT_API_KEY": "tenant-owned-key"}})
	if code != 201 {
		t.Fatalf("create=%d %v", code, out)
	}
	id := out["id"].(string)
	if e.srv.store.Snapshot().VMTokens[id] == nil {
		t.Fatal("tenant API override skipped lookup identity")
	}
	e.boatd.mu.Lock()
	before := slices.Clone(e.boatd.files[id+":/etc/boat/ssh-auth.json"])
	vars := fakeCopyEnv(e.boatd.vms[id])
	e.boatd.mu.Unlock()
	if len(before) == 0 || vars["BOAT_API_KEY"] != "tenant-owned-key" {
		t.Fatalf("lookup provisioning or tenant override lost: %s %v", before, vars)
	}
	if _, err := e.srv.bumpSelf(id, e.teamID()); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.refreshCredentials(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	e.boatd.mu.Lock()
	after := slices.Clone(e.boatd.files[id+":/etc/boat/ssh-auth.json"])
	vars = fakeCopyEnv(e.boatd.vms[id])
	e.boatd.mu.Unlock()
	if string(before) == string(after) || vars["BOAT_API_KEY"] != "tenant-owned-key" {
		t.Fatal("generation refresh skipped lookup or overwrote tenant key")
	}
}

func TestSSHAuthorityFreshFailures(t *testing.T) {
	for _, change := range []string{"inventory", "ready", "team", "ip", "user", "scope", "expiry", "unowned"} {
		t.Run(change, func(t *testing.T) {
			e := setup(t)
			_, vm := e.jumpSetup()
			token := e.newKey("exec")
			id, _, _ := ParseToken(token)
			e.grantSSH(token, vm)
			switch change {
			case "inventory":
				e.srv.boatd.hc.Transport = r20SSHTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })
			case "ready":
				e.requireSSHInstall(vm)
			case "team":
				e.boatd.mu.Lock()
				e.boatd.vms[vm]["team"] = "foreign"
				e.boatd.mu.Unlock()
			case "ip":
				e.boatd.mu.Lock()
				e.boatd.vms[vm]["ip"] = "127.0.0.1"
				e.boatd.mu.Unlock()
			case "user", "scope", "expiry", "unowned":
				if err := e.srv.store.Update(func(st *State) error {
					switch change {
					case "user":
						delete(st.Users, st.Keys[id].UserID)
					case "scope":
						st.Keys[id].Scopes = []string{"vms:read"}
					case "expiry":
						past := time.Now().Add(-time.Second)
						st.Keys[id].Expires = &past
					case "unowned":
						for _, g := range st.SSHGrants {
							g.KeyID = ""
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if change != "ip" {
				if c, _ := e.lookupSSH(vm, r11Pub); c == 200 {
					t.Fatal("fresh invalidity authorized guest")
				}
			}
			if c, _ := e.jumpSSH(r11Pub); c == 200 {
				t.Fatal("fresh invalidity authorized jump")
			}
		})
	}
}

func TestSSHAuthorityLookupBodyBounds(t *testing.T) {
	e := setup(t)
	_, vm := e.jumpSetup()
	e.boatd.mu.Lock()
	b := slices.Clone(e.boatd.files[vm+":/etc/boat/ssh-auth.json"])
	e.boatd.mu.Unlock()
	var config map[string]any
	json.Unmarshal(b, &config)
	for _, body := range []string{`{}`, `{"user":"dev","key_type":"ssh-ed25519","key_blob":"AAAAAAAAAAAAAAAAAAAA","extra":1}`, `{"user":"dev","key_type":"ssh-ed25519","key_blob":"AAAAAAAAAAAAAAAAAAAA"} {}`, strings.Repeat(" ", 33<<10), `{"user":"dev","key_type":"ssh-ed25519","key_blob":"AAAAAAAAAAAAAAAAAAAA\ncommand=evil"}`} {
		r := httptest.NewRequest("POST", vmPath(vm, "ssh", "authorize"), strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+config["token"].(string))
		w := httptest.NewRecorder()
		e.srv.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Errorf("invalid body status=%d", w.Code)
		}
	}
}

func TestSSHAuthorityCleanupCoalescesOriginalRecords(t *testing.T) {
	e := setup(t)
	_, vm := e.jumpSetup()
	ids := []string{vm, e.createVM(), e.createVM()}
	if err := e.srv.store.Update(func(st *State) error {
		for i := 0; i < maxGrantsPerTeam; i++ {
			k := fmt.Sprintf("owner-%d", i)
			st.SSHGrants[k] = &SSHGrant{TeamID: e.teamID(), PublicKey: strings.Join(strings.Fields(r11Pub)[:2], " "), KeyID: e.srv.authenticate("Bearer " + e.owner).Key.ID, UserID: e.srv.authenticate("Bearer " + e.owner).User.ID, VMs: slices.Clone(ids)}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		e.requireSSHInstall(id)
	}
	var installs atomic.Int32
	e.srv.boatd.hc.Transport = r20SSHTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/ssh-policy") {
			installs.Add(1)
			return nil, errors.New("offline")
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	for _, id := range ids {
		if c, _ := e.do(e.owner, "DELETE", vmPath(id, "ssh", url.PathEscape(sshFingerprint(r11Pub))), nil); c != 200 {
			t.Fatalf("revoke=%d", c)
		}
	}
	st := e.srv.store.Snapshot()
	if len(st.SSHGrants) != maxGrantsPerTeam || len(liveSSHGrants(st)) != 0 {
		t.Fatalf("debt inflated or still authorizing: %d", len(st.SSHGrants))
	}
	for _, g := range st.SSHGrants {
		if len(g.CleanupVMs) != 3 {
			t.Fatalf("cleanup missing/dedup failure: %v", g.CleanupVMs)
		}
	}
	installs.Store(0)
	if err := e.srv.syncJumpKeysErr(context.Background()); err == nil {
		t.Fatal("offline cleanup succeeded")
	}
	if installs.Load() != 3 {
		t.Fatalf("cleanup must coalesce per VM: %d", installs.Load())
	}
}

func TestSSHAuthorityFencedLookupInvalidation(t *testing.T) {
	for _, change := range []string{"revoke", "scope", "generation"} {
		t.Run(change, func(t *testing.T) {
			e := setup(t)
			_, vm := e.jumpSetup()
			token := e.newKey("exec")
			id, _, _ := ParseToken(token)
			e.grantSSH(token, vm)
			e.boatd.mu.Lock()
			b := slices.Clone(e.boatd.files[vm+":/etc/boat/ssh-auth.json"])
			e.boatd.mu.Unlock()
			var config map[string]any
			json.Unmarshal(b, &config)
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			// Hold a fresh inventory response while the invalidation commits. The
			// subsequent decision must re-read state under its shared publication fence.
			e.srv.boatd.hc.Transport = r20SSHTransport(func(r *http.Request) (*http.Response, error) {
				resp, err := http.DefaultTransport.RoundTrip(r)
				if r.Method == "GET" && r.URL.Path == vmPath(vm) {
					once.Do(func() { close(entered); <-release })
				}
				return resp, err
			})
			result := make(chan int, 1)
			go func() {
				f := strings.Fields(r11Pub)
				c, _ := e.do(config["token"].(string), "POST", vmPath(vm, "ssh", "authorize"), map[string]string{"user": "dev", "key_type": f[0], "key_blob": f[1]})
				result <- c
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("lookup inventory not reached")
			}
			if err := e.srv.store.Update(func(st *State) error {
				switch change {
				case "revoke":
					for _, g := range st.SSHGrants {
						g.CleanupVMs = slices.Clone(g.VMs)
						g.VMs = nil
					}
				case "scope":
					st.Keys[id].Scopes = []string{"vms:read"}
				case "generation":
					st.VMTokens[vm].Gen++
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			close(release)
			select {
			case c := <-result:
				if c == 200 {
					t.Fatal("stale inventory resurrected invalidated authority")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("lookup deadlocked")
			}
		})
	}
}

func TestSSHAuthorityRenewedKeySurvivesCleanup(t *testing.T) {
	e := setup(t)
	_, vm := e.jumpSetup()
	e.grantSSH(e.owner, vm)
	e.requireSSHInstall(vm)
	e.srv.boatd.hc.Transport = r20SSHTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" && r.URL.Path == vmPath(vm, "ssh-policy") {
			return nil, errors.New("offline")
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	if c, _ := e.do(e.owner, "DELETE", vmPath(vm, "ssh", url.PathEscape(sshFingerprint(r11Pub))), nil); c != 200 {
		t.Fatal(c)
	}
	e.srv.boatd.hc.Transport = http.DefaultTransport
	e.grantSSH(e.owner, vm)
	if err := e.srv.syncJumpKeysErr(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.assertSSH(vm, r11Pub, 200)
	if c, _ := e.jumpSSH(r11Pub); c != 200 {
		t.Fatalf("cleanup destroyed renewed same key=%d", c)
	}
	if len(e.srv.store.Snapshot().SSHGrants) != 1 {
		t.Fatal("renewal grew debt records")
	}
}

func TestSSHAuthorityAliasesAndNoHTTPS(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	e.srv.cfg.SSHPolicyUsers = []string{"user", "dev", "user"}
	vm := e.createVM()
	e.grantSSH(e.owner, vm)
	e.boatd.mu.Lock()
	b := slices.Clone(e.boatd.files[vm+":/etc/boat/ssh-auth.json"])
	e.boatd.mu.Unlock()
	var config map[string]any
	json.Unmarshal(b, &config)
	f := strings.Fields(r11Pub)
	for _, user := range []string{"dev", "user", "root"} {
		want := 200
		if user == "root" {
			want = 204
		}
		if c, _ := e.do(config["token"].(string), "POST", vmPath(vm, "ssh", "authorize"), map[string]string{"user": user, "key_type": f[0], "key_blob": f[1]}); c != want {
			t.Fatalf("user %s=%d want%d", user, c, want)
		}
	}
	e.srv.cfg.VMAPIURL = "http://boat.example"
	if c, _ := e.lookupSSH(vm, r11Pub); c == 200 {
		t.Fatal("non-HTTPS config authorized")
	}
	if c, _ := e.jumpSSH(r11Pub); c == 200 {
		t.Fatal("non-HTTPS jump authorized")
	}
}

func TestSSHAuthoritySnapshotRefreshWithoutAI(t *testing.T) {
	e := setup(t)
	_, vm := e.jumpSetup()
	e.grantSSH(e.owner, vm)
	if err := e.srv.store.Update(func(st *State) error { delete(st.AIVMs, vm); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.revokeCopiedCredentials(context.Background(), vm, e.teamID()); err != nil {
		t.Fatal(err)
	}
	e.assertSSH(vm, r11Pub, 200)
}

func TestSSHAuthorityPublicRejectsTrustedRootSelector(t *testing.T) {
	e := setup(t)
	vm := e.createVM()
	sandbox := e.newSandbox(nil)
	for _, route := range []struct {
		path string
		body map[string]any
	}{
		{vmPath(vm, "exec"), map[string]any{"cmd": []string{"true"}}},
		{vmPath(vm, "commands"), map[string]any{"cmd": []string{"true"}}},
		{"/api/v1/sandboxes/" + sandbox + "/commands", map[string]any{"command": "true"}},
		{"/api/v1/sandboxes/" + sandbox + "/commands", map[string]any{"command": "true", "detached": true}},
	} {
		for _, field := range []string{"trusted_root_exec", "user"} {
			t.Run(route.path+"/"+field, func(t *testing.T) {
				body := map[string]any{}
				for k, v := range route.body {
					body[k] = v
				}
				if field == "user" {
					body[field] = "root"
				} else {
					body[field] = true
				}
				b, _ := json.Marshal(body)
				r := httptest.NewRequest("POST", route.path, strings.NewReader(string(b)))
				r.Header.Set("Authorization", "Bearer "+e.owner)
				w := httptest.NewRecorder()
				var forwarded atomic.Int32
				e.srv.boatd.hc.Transport = r20SSHTransport(func(r *http.Request) (*http.Response, error) {
					if r.Method == "POST" && (strings.HasSuffix(r.URL.Path, "/exec") || strings.HasSuffix(r.URL.Path, "/agent")) {
						forwarded.Add(1)
					}
					return http.DefaultTransport.RoundTrip(r)
				})
				e.srv.ServeHTTP(w, r)
				if w.Code != 400 {
					t.Errorf("hostile public selector status=%d body=%s", w.Code, w.Body.String())
				}
				if forwarded.Load() != 0 {
					t.Fatal("hostile execution forwarded to boatd")
				}
			})
		}
	}
}

func TestSSHAuthorityInternalBuildSourceGetsIdentity(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	// Environment builds create temporary VMs directly through boatd, before
	// finishCreate has issued any self identity. Their snapshot source still
	// needs a current lookup config before access can be released.
	var vm VM
	if err := e.srv.boatd.call(context.Background(), "POST", "/v1/vms", map[string]any{"team": e.teamID(), "name": "build-source"}, &vm); err != nil {
		t.Fatal(err)
	}
	id := str(vm, "id")
	if e.srv.store.Snapshot().VMTokens[id] != nil {
		t.Fatal("source unexpectedly already has an identity")
	}
	if err := e.srv.revokeCopiedCredentials(context.Background(), id, e.teamID()); err != nil {
		t.Fatalf("source credential provisioning: %v", err)
	}
	if e.srv.store.Snapshot().VMTokens[id] == nil {
		t.Fatal("source lookup identity was skipped")
	}
	if c, _ := e.lookupSSH(id, r11Pub); c != 204 {
		t.Fatalf("ungranted source lookup=%d", c)
	}
}

func TestSSHAuthorityDecisionsSharePublicationFence(t *testing.T) {
	for _, kind := range []string{"guest", "jump"} {
		t.Run(kind, func(t *testing.T) {
			e := setup(t)
			_, id := e.jumpSetup()
			e.grantSSH(e.owner, id)
			var vm VM
			if err := e.srv.boatd.call(context.Background(), "GET", vmPath(id), nil, &vm); err != nil {
				t.Fatal(err)
			}
			config, err := e.srv.sshPolicyConfig(e.srv.store.Snapshot(), vm)
			if err != nil {
				t.Fatal(err)
			}
			f := strings.Fields(r11Pub)
			in := sshLookupRequest{User: "dev", KeyType: f[0], KeyBlob: f[1]}
			decide := func() int {
				if kind == "guest" {
					c, _ := e.srv.guestSSHDecision(vm, id, config["token"].(string), in)
					return c
				}
				c, _ := e.srv.jumpSSHDecision([]VM{vm}, in)
				return c
			}
			e.srv.store.mu.Lock()
			var once sync.Once
			unlock := func() { once.Do(func() { e.srv.store.mu.Unlock() }) }
			defer unlock()
			next, err := e.srv.store.next(func(st *State) error {
				for _, g := range st.SSHGrants {
					g.CleanupVMs = slices.Clone(g.VMs)
					g.VMs = nil
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			// Lock-free Snapshot() never blocks on store.mu during disk persistence
			// and never observes an unpublished/unpersisted next state.
			if c := decide(); c != 200 {
				t.Fatalf("decision before atomic publication = %d, want 200", c)
			}
			if err := e.srv.store.persist(next); err != nil {
				t.Fatal(err)
			}
			e.srv.store.cur.Store(next)
			unlock()
			if c := decide(); c != 204 {
				t.Fatalf("decision ignored atomic publication invalidation: %d", c)
			}
		})
	}
}
