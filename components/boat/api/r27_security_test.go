package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// openStream starts a GET (or POST) with token and returns the response once
// its headers arrived, plus a channel closed when the body ends.
func (e *env) openStream(token, method, path, body string) (*http.Response, <-chan struct{}) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	e.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, method, e.api.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	ended := make(chan struct{})
	go func() {
		io.Copy(io.Discard, resp.Body)
		close(ended)
	}()
	return resp, ended
}

func expectEnds(t *testing.T, what string, ended <-chan struct{}, within time.Duration) {
	t.Helper()
	select {
	case <-ended:
	case <-time.After(within):
		t.Fatalf("%s: stream still open %v after the credential was revoked", what, within)
	}
}

func expectOpen(t *testing.T, what string, ended <-chan struct{}, wait time.Duration) {
	t.Helper()
	select {
	case <-ended:
		t.Fatalf("%s: stream ended although its credential is alive", what)
	case <-time.After(wait):
	}
}

// Events stream: a key revoked while the stream is open ends it within the
// recheck interval; the owner's own stream (control) keeps running.
func TestR27EventsStreamEndsOnKeyRevoke(t *testing.T) {
	e := setup(t)
	e.srv.streamRecheck = 100 * time.Millisecond
	st, k := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "watcher"})
	if st != 201 {
		t.Fatalf("key: %d %v", st, k)
	}
	resp, ended := e.openStream(k["token"].(string), "GET", "/v1/events", "")
	if resp.StatusCode != 200 {
		t.Fatalf("events: %d", resp.StatusCode)
	}
	_, ownerEnded := e.openStream(e.owner, "GET", "/v1/events", "")
	expectOpen(t, "events before revocation", ended, 400*time.Millisecond)
	if st, out := e.do(e.owner, "DELETE", "/v1/keys/"+k["id"].(string), nil); st != 200 {
		t.Fatalf("revoke: %d %v", st, out)
	}
	expectEnds(t, "events", ended, 3*time.Second)
	expectOpen(t, "owner events (control)", ownerEnded, 300*time.Millisecond)
}

// A member removed from the team loses an open stream too.
func TestR27EventsStreamEndsOnMemberRemoval(t *testing.T) {
	e := setup(t)
	e.srv.streamRecheck = 100 * time.Millisecond
	tok, uid := e.addMember("member")
	_, ended := e.openStream(tok, "GET", "/v1/events", "")
	expectOpen(t, "member events", ended, 300*time.Millisecond)
	if st, out := e.do(e.owner, "DELETE", "/v1/teams/"+e.teamID()+"/members/"+uid, nil); st != 200 {
		t.Fatalf("remove: %d %v", st, out)
	}
	expectEnds(t, "member events", ended, 3*time.Second)
}

// Follow-mode command logs (query-selected stream) are covered as well.
func TestR27FollowLogsEndOnKeyRevoke(t *testing.T) {
	e := setup(t)
	e.srv.streamRecheck = 100 * time.Millisecond
	id := e.createVM()
	st, cmd := e.do(e.owner, "POST", "/v1/vms/"+id+"/commands", map[string]any{"cmd": []string{"sleep", "600"}})
	if st != 200 && st != 201 && st != 202 {
		t.Fatalf("start command: %d %v", st, cmd)
	}
	cid, _ := cmd["id"].(string)
	if cid == "" {
		cid, _ = cmd["command_id"].(string)
	}
	st, k := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "follower"})
	if st != 201 {
		t.Fatalf("key: %d %v", st, k)
	}
	resp, ended := e.openStream(k["token"].(string), "GET", "/v1/vms/"+id+"/commands/"+cid+"/logs?follow=1", "")
	if resp.StatusCode != 200 {
		t.Fatalf("follow: %d", resp.StatusCode)
	}
	expectOpen(t, "follow logs", ended, 400*time.Millisecond)
	e.do(e.owner, "DELETE", "/v1/keys/"+k["id"].(string), nil)
	expectEnds(t, "follow logs", ended, 3*time.Second)
}

func TestR27IsStreamRequest(t *testing.T) {
	mk := func(method, target string, hdr map[string]string) *http.Request {
		r, _ := http.NewRequest(method, "http://x"+target, nil)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	for _, c := range []struct {
		pattern string
		r       *http.Request
		want    bool
	}{
		{"GET /v1/events", mk("GET", "/v1/events", nil), true},
		{"POST /v1/vms/{id}/exec", mk("POST", "/v1/vms/a/exec", nil), true},
		{"POST /v1/vms/{id}/prompt", mk("POST", "/v1/vms/a/prompt", nil), true},
		{"GET /api/v1/sandboxes/{id}/events", mk("GET", "/api/v1/sandboxes/a/events", nil), true},
		{"GET /api/v1/sandboxes/{id}/commands/{pid}/stream", mk("GET", "/api/v1/sandboxes/a/commands/p/stream", nil), true},
		{"POST /api/v1/sandboxes/{id}/commands", mk("POST", "/api/v1/sandboxes/a/commands", nil), true},
		{"GET /v1/vms/{id}/commands/{cid}/logs", mk("GET", "/v1/vms/a/commands/c/logs", nil), true},
		{"PUT /v1/vms/{id}/files", mk("PUT", "/v1/vms/a/files", nil), true},
		{"GET /v1/vms/{id}/ports", mk("GET", "/v1/vms/a/ports?follow=true", nil), true},
		{"GET /v1/vms/{id}/ports", mk("GET", "/v1/vms/a/ports?stream=1", nil), true},
		{"GET /v1/vms/{id}/ports", mk("GET", "/v1/vms/a/ports", map[string]string{"Accept": "text/event-stream"}), true},
		{"GET /v1/vms/{id}/ports", mk("GET", "/v1/vms/a/ports", map[string]string{"Upgrade": "websocket"}), true},
		{"GET /v1/vms/{id}/ports", mk("GET", "/v1/vms/a/ports", nil), false},
		{"GET /v1/vms/{id}/ports", mk("GET", "/v1/vms/a/ports?follow=0", nil), false},
		{"GET /v1/me", mk("GET", "/v1/me", nil), false},
	} {
		if got := isStreamRequest(c.pattern, c.r); got != c.want {
			t.Errorf("isStreamRequest(%q, %s) = %v, want %v", c.pattern, c.r.URL, got, c.want)
		}
	}
}

// ---- POST /v1/vms/{id}/ssh/authorize ----

func TestR27GuestAuthorizeUniform401AndNoBoatdCall(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	vm := e.createVM()
	var calls atomic.Int64
	r26Proxy(e, func(r *http.Request) bool { calls.Add(1); return false })
	good := e.lookupSSHBearer(vm)
	otherVM := e.createVM()
	otherTok := e.lookupSSHBearer(otherVM)
	body := map[string]string{"user": "dev", "key_type": "ssh-ed25519", "key_blob": strings.Fields(r11Pub)[1]}
	calls.Store(0)

	bad := sshLookupPrefix + strings.Repeat("0", 64)
	status := func(tok, id string) int {
		t.Helper()
		st, _ := e.do(tok, "POST", "/v1/vms/"+id+"/ssh/authorize", body)
		return st
	}
	unknown := status(good, "no-such-vm") // unknown VM, a well-formed bearer
	wrong := status(bad, vm)              // known VM, wrong bearer
	badID := status(good, "bad%20id")     // not a VM id at all
	foreign := status(otherTok, vm)       // another VM's bearer
	if unknown != 401 || wrong != 401 || badID != 401 || foreign != 401 {
		t.Fatalf("unknown=%d wrong=%d badID=%d foreign=%d, want all 401", unknown, wrong, badID, foreign)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("boatd was called %d times for unauthenticated lookups", n)
	}
	// Control: the right bearer for the right VM is answered.
	if st := status(good, vm); st != 204 {
		t.Errorf("valid bearer: %d, want 204 (no grant)", st)
	}
	if calls.Load() == 0 {
		t.Errorf("a verified bearer must reach boatd")
	}
}

// lookupSSHBearer returns the guest's lookup bearer from its installed policy.
func (e *env) lookupSSHBearer(vm string) string {
	e.t.Helper()
	var config map[string]any
	e.boatd.mu.Lock()
	b := slices.Clone(e.boatd.files[vm+":/etc/boat/ssh-auth.json"])
	e.boatd.mu.Unlock()
	if err := json.Unmarshal(b, &config); err != nil {
		e.t.Fatal(err)
	}
	return config["token"].(string)
}

func TestR27GuestAuthorizeRateLimited(t *testing.T) {
	e := setup(t)
	e.enableSSH()
	vm := e.createVM()
	tok := e.lookupSSHBearer(vm)
	e.srv.limiter = newLimiter(0.001, 3)
	body := map[string]string{"user": "dev", "key_type": "ssh-ed25519", "key_blob": strings.Fields(r11Pub)[1]}
	var codes []int
	for i := 0; i < 5; i++ {
		st, _ := e.do(tok, "POST", "/v1/vms/"+vm+"/ssh/authorize", body)
		codes = append(codes, st)
	}
	if codes[0] == 429 || codes[4] != 429 {
		t.Fatalf("codes = %v, want the first answered and the last throttled (429 + Retry-After)", codes)
	}
}

// ---- revokeSSHGrant ----

func TestR27RevokeOthersGrantNeedsAdminScope(t *testing.T) {
	e := setup(t)
	_, vm := e.jumpSetup()
	tok, _ := e.addMember("member")
	e.grantSSH(tok, vm)
	fp := sshFingerprint(r11Pub[:strings.LastIndex(r11Pub, " ")])
	st, narrow := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "exec-only", "scopes": []string{"exec"}})
	if st != 201 {
		t.Fatalf("key: %d %v", st, narrow)
	}
	if st, out := e.do(narrow["token"].(string), "DELETE", "/v1/vms/"+vm+"/ssh/"+url.PathEscape(fp), nil); st != 403 {
		t.Fatalf("owner-role key without admin scope revoked a member's grant: %d %v", st, out)
	}
	if n := len(liveSSHGrants(e.srv.store.Snapshot())); n != 1 {
		t.Fatalf("grant should survive, live=%d", n)
	}
	if st, out := e.do(e.owner, "DELETE", "/v1/vms/"+vm+"/ssh/"+url.PathEscape(fp), nil); st != 200 {
		t.Fatalf("admin revoke: %d %v", st, out)
	}
}

func TestR27RevokeGrantRevalidatesCaller(t *testing.T) {
	e := setup(t)
	_, vm := e.jumpSetup()
	st, k := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "gone"})
	if st != 201 {
		t.Fatalf("key: %d %v", st, k)
	}
	tok := k["token"].(string)
	e.grantSSH(tok, vm)
	fp := sshFingerprint(r11Pub[:strings.LastIndex(r11Pub, " ")])
	p := e.srv.authenticate("Bearer " + tok)
	if p == nil {
		t.Fatal("authenticate")
	}
	e.do(e.owner, "DELETE", "/v1/keys/"+k["id"].(string), nil)
	// The stale principal (authenticated before the revocation) must not act.
	err := e.srv.store.Update(func(st *State) error {
		_, aerr := requirePrincipalIn(st, p, "exec", time.Now())
		return aerr
	})
	if err == nil {
		t.Fatal("revoked key still passes requirePrincipalIn")
	}
	if st, _ := e.do(tok, "DELETE", "/v1/vms/"+vm+"/ssh/"+url.PathEscape(fp), nil); st != 401 {
		t.Errorf("revoked key revoking a grant: %d, want 401", st)
	}
}

// ---- forwarding headers on internal routes ----

func TestR27InternalRoutesRejectAnyForwardingHeader(t *testing.T) {
	e := setup(t)
	for _, h := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip"} {
		req, _ := http.NewRequest("GET", e.api.URL+"/internal/route-gens", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("route-gens without %s: %d", h, resp.StatusCode)
		}
		req.Header.Set(h, "203.0.113.9")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("route-gens with %s: %d, want 404", h, resp.StatusCode)
		}
		req, _ = http.NewRequest("POST", e.api.URL+"/internal/ssh/authorize", strings.NewReader(`{"user":"jump","key_type":"ssh-ed25519","key_blob":"AAAA"}`))
		req.Header.Set(h, "203.0.113.9")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("jump authorize with %s: %d, want 403", h, resp.StatusCode)
		}
	}
}
