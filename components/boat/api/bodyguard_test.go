package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// partialBody (text/plain, so the drift recorder does not read it first) sends a request that announces a large body but sends only a
// few bytes, then returns the status line the server answers within wait
// ("" if none: the server is still waiting for the body).
func (e *env) partialBody(token, method, path string, wait time.Duration) string {
	e.t.Helper()
	c, err := net.Dial("tcp", e.api.Listener.Addr().String())
	if err != nil {
		e.t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "%s %s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n{\"path\":\"",
		method, path, token, 7<<20)
	c.SetReadDeadline(time.Now().Add(wait))
	line, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line)
}

// Another team's VM: the answer is 404 before boat-api reads one byte of the
// body, so a tenant cannot make it buffer megabytes per request against VMs
// it does not own (compat file write, and native JSON routes alike).
func TestForeignVMBodyNotRead(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	_, other := e.newTeam("other")
	for _, r := range []string{"PUT /api/v1/sandboxes/%s/files", "POST /v1/vms/%s/fork", "POST /v1/vms/%s/commands", "PUT /v1/vms/%s/files"} {
		method, path, _ := strings.Cut(fmt.Sprintf(r, id), " ")
		if got := e.partialBody(other, method, path, 3*time.Second); !strings.Contains(got, " 404 ") {
			t.Errorf("%s: %q, want 404 without reading the body", r, got)
		}
	}
}

// A body that trickles in is cut off at BodyTimeout on a JSON route.
func TestBodyReadDeadline(t *testing.T) {
	e := setup(t)
	e.srv.cfg.BodyTimeout = 200 * time.Millisecond
	id := e.createVM()
	start := time.Now()
	got := e.partialBody(e.owner, "POST", "/v1/vms/"+id+"/fork", 5*time.Second)
	if !strings.Contains(got, " 400 ") || time.Since(start) > 3*time.Second {
		t.Errorf("slow body: %q after %v, want 400 at the deadline", got, time.Since(start))
	}
}

// One cap per team over every route: a held stream counts against it.
func TestTeamInflightCap(t *testing.T) {
	e := setup(t)
	_, other := e.newTeam("other")
	e.srv.teamAll = newInflight(1)
	req, _ := http.NewRequest("GET", e.api.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+e.owner)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("events: %v %v", resp, err)
	}
	defer resp.Body.Close()
	if st, out := e.do(e.owner, "GET", "/v1/me", nil); st != 429 {
		t.Errorf("second request with the team at its cap: %d %v", st, out)
	}
	if st, _ := e.do(other, "GET", "/v1/me", nil); st != 200 {
		t.Errorf("another team is not limited: %d", st)
	}
}

// The compat file routes hold the same per-team upload and stream slots as
// their native twins.
func TestCompatFileRoutesCapped(t *testing.T) {
	if !uploadRoutes["PUT /api/v1/sandboxes/{id}/files"] || !streamRoutes["GET /api/v1/sandboxes/{id}/files"] {
		t.Fatal("compat file routes are outside the upload/stream caps")
	}
	e := setup(t)
	e.srv.uploads = newInflight(0)
	e.srv.uploads.max = 1
	release, _ := e.srv.uploads.acquire(e.teamID())
	defer release()
	id := e.createVM()
	st, out, _ := e.compat(e.owner, "PUT", "/api/v1/sandboxes/"+id+"/files", map[string]any{"path": "a", "content": "x"}, nil)
	if st != 429 {
		t.Errorf("compat write with the upload slots full: %d %v", st, out)
	}
}
