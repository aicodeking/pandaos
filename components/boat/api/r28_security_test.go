package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// r28SlowProxy sits between boat-api and the fake boatd. Requests selected by
// endless get a 200 that never ends (16 KiB every 20ms until the client side
// goes away); everything else is forwarded. observe sees every request first.
func r28SlowProxy(e *env, endless func(*http.Request) bool, observe func(*http.Request)) {
	e.t.Helper()
	target, err := url.Parse(e.srv.boatd.base)
	if err != nil {
		e.t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if observe != nil {
			observe(r)
		}
		if endless != nil && endless(r) {
			w.WriteHeader(200)
			chunk := make([]byte, 16<<10) // enough to push boat-api's response buffer out
			for {
				if _, err := w.Write(chunk); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		}
		rp.ServeHTTP(w, r)
	}))
	e.t.Cleanup(ps.Close)
	e.srv.boatd = newBoatd(ps.URL)
}

// openPublic starts an unauthenticated GET and returns once headers arrived.
func (e *env) openPublic(rawURL string) (*http.Response, <-chan struct{}) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	e.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	ended := make(chan struct{})
	go func() {
		buf := make([]byte, 512)
		for {
			if _, err := resp.Body.Read(buf); err != nil {
				close(ended)
				return
			}
		}
	}()
	return resp, ended
}

// 3a: a signed snapshot link stops streaming once its share dies mid-download.
func TestR28LinkDownloadEndsWhenShareRevoked(t *testing.T) {
	e := setup(t)
	e.srv.streamRecheck = 100 * time.Millisecond
	id := e.createVM()
	sid := e.snapshotOf(id)
	st, k := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "sharer"})
	if st != 201 {
		t.Fatalf("key: %d %v", st, k)
	}
	st, sh := e.do(k["token"].(string), "POST", "/v1/vms/"+id+"/snapshots/"+sid+"/shares", map[string]any{"ttl_s": 600})
	if st != 201 {
		t.Fatalf("share: %d %v", st, sh)
	}
	r28SlowProxy(e, func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/download") }, nil)
	resp, ended := e.openPublic(e.api.URL + "/v1/snapshot-links/" + sh["code"].(string) + "/download")
	if resp.StatusCode != 200 {
		t.Fatalf("download: %d", resp.StatusCode)
	}
	expectOpen(t, "link download before revocation", ended, 400*time.Millisecond)
	if st, out := e.do(e.owner, "DELETE", "/v1/keys/"+k["id"].(string), nil); st != 200 {
		t.Fatalf("revoke: %d %v", st, out)
	}
	expectEnds(t, "link download", ended, 3*time.Second)
}

// 3a control: a live share keeps streaming across several recheck ticks.
func TestR28LinkDownloadKeepsStreamingWhileShareAlive(t *testing.T) {
	e := setup(t)
	e.srv.streamRecheck = 50 * time.Millisecond
	id := e.createVM()
	sid := e.snapshotOf(id)
	st, sh := e.do(e.owner, "POST", "/v1/vms/"+id+"/snapshots/"+sid+"/shares", map[string]any{"ttl_s": 600})
	if st != 201 {
		t.Fatalf("share: %d %v", st, sh)
	}
	r28SlowProxy(e, func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/download") }, nil)
	_, ended := e.openPublic(e.api.URL + "/v1/snapshot-links/" + sh["code"].(string) + "/download")
	expectOpen(t, "live share download", ended, 600*time.Millisecond)
}

// 3b: a compat file download URL stops once the key that minted it is revoked.
func TestR28CompatFileDownloadEndsWhenKeyRevoked(t *testing.T) {
	e := setup(t)
	e.srv.streamRecheck = 100 * time.Millisecond
	bx := e.newSandbox(nil)
	e.ok("PUT", "/api/v1/sandboxes/"+bx+"/files", map[string]any{"path": "big.bin", "content": "x"}, 200)
	st, k := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "minter"})
	if st != 201 {
		t.Fatalf("key: %d %v", st, k)
	}
	code, d, _ := e.compat(k["token"].(string), "POST", "/api/v1/sandboxes/"+bx+"/files/download-url", map[string]any{"path": "big.bin", "ttlSeconds": 600}, nil)
	if code != 200 {
		t.Fatalf("download-url: %d %v", code, d)
	}
	r28SlowProxy(e, func(r *http.Request) bool {
		return r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/files") && r.URL.Query().Get("path") != ""
	}, nil)
	resp, ended := e.openPublic(d["url"].(string))
	if resp.StatusCode != 200 {
		t.Fatalf("download: %d", resp.StatusCode)
	}
	expectOpen(t, "file download before revocation", ended, 400*time.Millisecond)
	if st, out := e.do(e.owner, "DELETE", "/v1/keys/"+k["id"].(string), nil); st != 200 {
		t.Fatalf("revoke: %d %v", st, out)
	}
	expectEnds(t, "file download", ended, 3*time.Second)
}

// 2a: a non-owner cannot raise another VM's "policy in flight" flag. The proxy
// samples the flag at every boatd call the request makes.
func TestR28NonOwnerCannotSetPolicyInFlight(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	vm := e.createVM()
	var mu sync.Mutex
	sawInFlight := false
	r28SlowProxy(e, nil, func(r *http.Request) {
		if strings.Contains(r.URL.Path, vm) && e.srv.isPolicyInFlight(vm) {
			mu.Lock()
			sawInFlight = true
			mu.Unlock()
		}
	})
	_, other := e.newTeam("intruder")
	st, out := e.do(other, "POST", "/v1/vms/"+vm+"/ssh", map[string]any{"public_key": r11Pub})
	if st != 404 {
		t.Fatalf("non-owner ssh: %d %v, want 404", st, out)
	}
	mu.Lock()
	got := sawInFlight
	mu.Unlock()
	if got {
		t.Fatal("a non-owner request raised the policy-in-flight flag before the ownership check")
	}
	if e.srv.isPolicyInFlight(vm) {
		t.Fatal("flag leaked")
	}
	// Control: the owner's request does enter the in-flight window.
	if st, out := e.do(e.owner, "POST", "/v1/vms/"+vm+"/ssh", map[string]any{"public_key": ""}); st != 200 {
		t.Fatalf("owner ssh: %d %v", st, out)
	}
}

// 2b: while a policy push is in flight the lookup still proves the bearer
// against locally known token material before boatd is called.
func TestR28GuestAuthorizeInFlightStillVerifiesBearer(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	vm := e.createVM()
	good := e.lookupSSHBearer(vm)
	other := e.lookupSSHBearer(e.createVM())
	var calls atomic.Int64
	r28SlowProxy(e, nil, func(*http.Request) { calls.Add(1) })
	body := map[string]string{"user": "dev", "key_type": "ssh-ed25519", "key_blob": strings.Fields(r11Pub)[1]}
	post := func(tok string) int {
		t.Helper()
		st, _ := e.do(tok, "POST", "/v1/vms/"+vm+"/ssh/authorize", body)
		return st
	}
	bearerFor := func(epoch string) string {
		cfg, err := e.srv.sshPolicyConfig(e.srv.store.Snapshot(), VM{"id": vm, "team": e.teamID(), "ssh_boot_epoch": epoch})
		if err != nil {
			t.Fatal(err)
		}
		return cfg["token"].(string)
	}

	e.srv.enterPolicy(vm)
	defer e.srv.leavePolicy(vm)
	calls.Store(0)
	bad := sshLookupPrefix + strings.Repeat("0", 64)
	for name, tok := range map[string]string{"wrong": bad, "foreign VM": other, "unknown epoch": bearerFor("not-a-known-epoch")} {
		if st := post(tok); st != 401 {
			t.Errorf("in flight, %s bearer: %d, want 401", name, st)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("boatd was called %d times for unverified bearers while a push was in flight", n)
	}
	// The attested epoch still verifies (the VM keeps working during a re-push)...
	if st := post(good); st == 401 {
		t.Errorf("in flight, attested bearer rejected")
	}
	if calls.Load() == 0 {
		t.Errorf("verified bearer never reached boatd")
	}
	// ...and so does the epoch being installed, once the push noted it.
	e.srv.notePendingEpoch(vm, "pending-epoch-1")
	calls.Store(0)
	post(bearerFor("pending-epoch-1"))
	if calls.Load() == 0 {
		t.Errorf("bearer for the epoch being installed never reached boatd")
	}
}

// After the push ends, pending epochs stop verifying.
func TestR28PendingEpochDroppedAfterPush(t *testing.T) {
	e := setup(t)
	e.srv.enterPolicy("vm_x")
	e.srv.notePendingEpoch("vm_x", "e1")
	if !slices.Contains(e.srv.knownBootEpochs("vm_x"), "e1") {
		t.Fatal("pending epoch not known while in flight")
	}
	e.srv.leavePolicy("vm_x")
	if len(e.srv.knownBootEpochs("vm_x")) != 0 {
		t.Fatal("pending epoch survived the push")
	}
}

// Contract the gateway's jump-reaper relies on: the very lookup sshd makes
// answers 200 + lines while the grant stands and 204 (no lines) once the key
// is deleted or the VM is gone, so a differing answer ends the session.
func TestR28JumpLookupChangesOnRevocation(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	vm := e.createVM()
	st, k := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "jumper"})
	if st != 201 {
		t.Fatalf("key: %d %v", st, k)
	}
	if st, out := e.do(k["token"].(string), "POST", "/v1/vms/"+vm+"/ssh", map[string]any{"public_key": r11Pub}); st != 200 {
		t.Fatalf("grant: %d %v", st, out)
	}
	code, lines := e.jumpSSH(r11Pub)
	if code == 200 && len(lines) == 0 {
		t.Fatalf("200 without lines")
	}
	if code == 204 {
		t.Skip("fake boatd has no routable VM address; lookup is 204 even for a live grant")
	}
	e.do(e.owner, "DELETE", "/v1/keys/"+k["id"].(string), nil)
	if code, lines := e.jumpSSH(r11Pub); code != 204 || len(lines) != 0 {
		t.Fatalf("after key delete: %d %v, want 204", code, lines)
	}
}
