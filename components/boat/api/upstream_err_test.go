package main

import (
	"strings"
	"testing"
)

// Council round 3: desktop on a slim image answered 502 "vm manager error" (and
// code http_500 under /api/v1 next to status 502). A guest agent refusal is the
// caller's problem: a 4xx that keeps the guest's message, alike on both APIs.
func TestGuestRefusalIs4xx(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	_, me := e.do(e.owner, "GET", "/v1/vms", nil)
	id := list(me, "vms")[0].(map[string]any)["id"].(string)
	e.boatd.mu.Lock()
	e.boatd.deskErr = "desktop is not installed in this image (slim variant)"
	e.boatd.mu.Unlock()
	st, out := e.do(e.owner, "GET", "/v1/vms/"+id+"/desktop", nil)
	er, _ := out["error"].(map[string]any)
	if st != 422 || er["code"] != "not_supported" || !strings.Contains(er["message"].(string), "slim") {
		t.Errorf("native: %d %v", st, out)
	}
	st, c, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/desktop", nil, nil)
	if st != 422 || c["status"] != float64(422) || c["code"] != "not_supported" || !strings.Contains(c["message"].(string), "slim") {
		t.Errorf("compat: %d %v", st, c)
	}
	// An unknown guest failure stays a 502, but status and code agree.
	e.boatd.mu.Lock()
	e.boatd.deskErr = "desktop did not come up on 127.0.0.1:6080: timeout"
	e.boatd.mu.Unlock()
	st, c, _ = e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/desktop", nil, nil)
	if st != 502 || c["code"] != "http_502" || !strings.Contains(c["message"].(string), "did not come up") {
		t.Errorf("compat 502: %d %v", st, c)
	}
	// Bad input the guest rejects is a 400.
	e.boatd.mu.Lock()
	e.boatd.deskErr = "token must be 16-128 url-safe chars"
	e.boatd.mu.Unlock()
	if st, out := e.do(e.owner, "GET", "/v1/vms/"+id+"/desktop", nil); st != 400 {
		t.Errorf("guest bad input: %d %v", st, out)
	}
}

// boatd's 504 resize_timeout (the guest's resize2fs hung) reaches the caller as such,
// not as a 502 "vm manager error", on both APIs.
func TestResizeTimeoutIs504(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	e.boatd.mu.Lock()
	e.boatd.resizeTimeout = true
	e.boatd.mu.Unlock()
	st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/resize", map[string]any{"disk_gib": 9})
	er, _ := out["error"].(map[string]any)
	if st != 504 || er["code"] != "resize_timeout" || !strings.Contains(er["message"].(string), "resize2fs") {
		t.Errorf("native: %d %v", st, out)
	}
}
