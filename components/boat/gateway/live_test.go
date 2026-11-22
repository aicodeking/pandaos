package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// liveEnv is a gateway in front of one streaming upstream (SSE on /sse, a
// WebSocket-style upgrade on /ws), with a fake boat-api whose generations the
// test controls.
type liveEnv struct {
	g       *gateway
	srv     *httptest.Server
	rp      string
	port    int
	host    string // private port route of abc-1
	pubHost string // public port route of xyz-2
	gensDoc atomic.Value
	cancel  context.CancelFunc
	skew    atomic.Int64 // added to the gateway clock (ns)
}

func newLiveEnv(t *testing.T) *liveEnv {
	t.Helper()
	g, rp, port := setup(t)
	e := &liveEnv{g: g, rp: rp, port: port}
	g.now = func() time.Time { return time.Now().Add(time.Duration(e.skew.Load())) }
	e.gensDoc.Store(`{"gens":{},"desktops":["abc-1"]}`)
	// Replace setup's upstream: one that streams.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			c, rw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer c.Close()
			rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			rw.Flush()
			for {
				if _, err := rw.WriteString("tick\n"); err != nil {
					return
				}
				if rw.Flush() != nil {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for {
			if _, err := fmt.Fprint(w, "data: tick\n\n"); err != nil {
				return
			}
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(up.Close)
	_, ps, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	e.port, _ = strconv.Atoi(ps)
	writeRoutes(t, rp, `{"vms":{"abc-1":{"ip":"127.0.0.1","ports":[`+ps+`],"desktopPort":`+ps+`},
		"xyz-2":{"ip":"127.0.0.1","ports":[`+ps+`],"public":[`+ps+`],"desktopPort":`+ps+`}}}`)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(e.gensDoc.Load().(string)))
	}))
	t.Cleanup(api.Close)
	g.gens = newRouteGens(api.URL)
	g.gens.every = 25 * time.Millisecond
	g.gens.onRefresh = func() { g.revalidateLive() }
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	t.Cleanup(cancel)
	if err := g.gens.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	go g.gens.poll(ctx)
	go g.watchLive(ctx, 25*time.Millisecond)
	e.srv = httptest.NewServer(g)
	t.Cleanup(e.srv.Close)
	e.host = ps + "-abc-1.vm.boatvms.test"
	e.pubHost = ps + "-xyz-2.vm.boatvms.test"
	return e
}

func (e *liveEnv) routeToken(gen int) string {
	return first(tok(func(x int64) string { return signRoute(e.g.k(purposePortRoute), e.port, "abc-1", gen, x) }, time.Hour))
}

// openSSE starts a stream through the gateway and returns a reader of its lines.
func (e *liveEnv) openSSE(t *testing.T, host string, mod ...func(*http.Request)) *bufio.Reader {
	t.Helper()
	r, err := http.NewRequest("GET", e.srv.URL+"/sse", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Host = host
	for _, m := range mod {
		m(r)
	}
	resp, err := (&http.Client{}).Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != 200 {
		t.Fatalf("sse: %d", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("no first event: %v", err)
	}
	return br
}

// openWS performs a raw WebSocket-style upgrade through the gateway.
func (e *liveEnv) openWS(t *testing.T, host string, hdr map[string]string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(e.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	var b strings.Builder
	b.WriteString("GET /ws HTTP/1.1\r\nHost: " + host + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n")
	for k, v := range hdr {
		b.WriteString(k + ": " + v + "\r\n")
	}
	b.WriteString("\r\n")
	if _, err := c.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		t.Fatalf("upgrade: %q %v", status, err)
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if l == "\r\n" {
			break
		}
	}
	if l, err := br.ReadString('\n'); err != nil || l != "tick\n" {
		t.Fatalf("first frame %q %v", l, err)
	}
	return c, br
}

// closesWithin reports whether reads from br fail before d elapses.
func closesWithin(c net.Conn, br *bufio.Reader, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(deadline)
		if _, err := br.ReadString('\n'); err != nil {
			return !isTimeout(err)
		}
	}
	return false
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

func sseClosesWithin(br *bufio.Reader, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := br.ReadString('\n'); err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func TestLiveSSEClosesWhenGenerationBumps(t *testing.T) {
	e := newLiveEnv(t)
	br := e.openSSE(t, e.host, header(routeHeader, e.routeToken(0)))
	// Healthy: still flowing across several polls.
	time.Sleep(150 * time.Millisecond)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("stream died without a revocation: %v", err)
	}
	e.gensDoc.Store(`{"gens":{"abc-1":1},"desktops":["abc-1"]}`)
	if !sseClosesWithin(br, 2*time.Second) {
		t.Fatal("SSE stream survived a generation bump")
	}
}

func TestLiveWebSocketClosesWhenGenerationBumps(t *testing.T) {
	e := newLiveEnv(t)
	c, br := e.openWS(t, e.host, map[string]string{routeHeader: e.routeToken(0)})
	time.Sleep(100 * time.Millisecond)
	e.gensDoc.Store(`{"gens":{"abc-1":1},"desktops":["abc-1"]}`)
	if !closesWithin(c, br, 2*time.Second) {
		t.Fatal("WebSocket survived a generation bump")
	}
}

func TestLiveUnrelatedVMBumpKeepsStream(t *testing.T) {
	e := newLiveEnv(t)
	br := e.openSSE(t, e.host, header(routeHeader, e.routeToken(0)))
	e.gensDoc.Store(`{"gens":{"other-9":4},"desktops":["abc-1"]}`)
	time.Sleep(300 * time.Millisecond)
	if sseClosesWithin(br, 10*time.Millisecond) {
		t.Fatal("another VM's revocation ended this stream")
	}
}

func TestLiveDesktopStreamClosesWhenGenerationBumps(t *testing.T) {
	e := newLiveEnv(t)
	exp := time.Now().Add(time.Hour).Unix()
	cookieTok := strconv.FormatInt(exp, 10) + "." + signDesktop(e.g.k(purposeDesktopLink), "abc-1", 0, exp)
	br := e.openSSE(t, "abc-1.desktop.boat.test", cookie(desktopCookie, cookieTok))
	e.gensDoc.Store(`{"gens":{"abc-1":1},"desktops":["abc-1"]}`)
	if !sseClosesWithin(br, 2*time.Second) {
		t.Fatal("desktop stream survived a link revocation")
	}
}

func TestLiveStreamClosesWhenRouteDisappears(t *testing.T) {
	e := newLiveEnv(t)
	br := e.openSSE(t, e.host, header(routeHeader, e.routeToken(0)))
	writeRoutes(t, e.rp, `{"vms":{}}`)
	if !sseClosesWithin(br, 2*time.Second) {
		t.Fatal("stream survived its VM leaving routes.json")
	}
}

func TestLiveStreamClosesWhenVMIPChanges(t *testing.T) {
	e := newLiveEnv(t)
	br := e.openSSE(t, e.pubHost)
	ps := strconv.Itoa(e.port)
	writeRoutes(t, e.rp, `{"vms":{"xyz-2":{"ip":"127.0.0.2","ports":[`+ps+`],"public":[`+ps+`]}}}`)
	if !sseClosesWithin(br, 2*time.Second) {
		t.Fatal("stream survived the VM's IP being reassigned")
	}
}

func TestLivePublicStreamClosesWhenPortTurnsPrivate(t *testing.T) {
	e := newLiveEnv(t)
	br := e.openSSE(t, e.pubHost)
	time.Sleep(100 * time.Millisecond)
	ps := strconv.Itoa(e.port)
	writeRoutes(t, e.rp, `{"vms":{"xyz-2":{"ip":"127.0.0.1","ports":[`+ps+`],"public":[],"desktopPort":`+ps+`}}}`)
	if !sseClosesWithin(br, 2*time.Second) {
		t.Fatal("public-admitted stream survived its port becoming private")
	}
}

func TestLiveStreamClosesWhenGenerationTableGoesStale(t *testing.T) {
	e := newLiveEnv(t)
	br := e.openSSE(t, e.host, header(routeHeader, e.routeToken(0)))
	// The API goes away; the table ages out (routeGenMaxAge) and the stream fails closed.
	clock := new(atomic.Int64)
	clock.Store(time.Now().UnixNano())
	e.g.gens.mu.Lock()
	e.g.gens.now = func() time.Time { return time.Unix(0, clock.Load()) }
	e.g.gens.fetched = time.Unix(0, clock.Load())
	e.g.gens.mu.Unlock()
	e.cancel() // stop polling so nothing renews the table
	time.Sleep(60 * time.Millisecond)
	clock.Add(int64(routeGenMaxAge + time.Second))
	e.g.revalidateLive()
	if !sseClosesWithin(br, 2*time.Second) {
		t.Fatal("stream survived a stale revocation table")
	}
}

func TestLiveRegistryEmptiesWhenRequestsEnd(t *testing.T) {
	e := newLiveEnv(t)
	resp, err := http.Get(e.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, ep := range []string{"/", "/"} {
		r, _ := http.NewRequest("GET", e.srv.URL+ep, nil)
		r.Host = e.pubHost
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		r = r.WithContext(ctx)
		if resp, err := http.DefaultClient.Do(r); err == nil {
			resp.Body.Close()
		}
		cancel()
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(e.g.live.snapshot()) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := len(e.g.live.snapshot()); n != 0 {
		t.Fatalf("%d requests still registered after they ended", n)
	}
}

// An open stream ends when the token or cookie that admitted it expires, even
// though nothing was revoked. The public route has no credential and stays.
func TestLiveStreamClosesWhenAdmittingTokenExpires(t *testing.T) {
	later := int64(2 * time.Hour)
	t.Run("port token header", func(t *testing.T) {
		e := newLiveEnv(t)
		br := e.openSSE(t, e.host, header(routeHeader, e.routeToken(0)))
		e.skew.Store(later)
		if !sseClosesWithin(br, 2*time.Second) {
			t.Fatal("SSE survived its port token expiring")
		}
	})
	t.Run("port cookie websocket", func(t *testing.T) {
		e := newLiveEnv(t)
		c, br := e.openWS(t, e.host, map[string]string{"Cookie": routeCookie + "=" + e.routeToken(0)})
		e.skew.Store(later)
		if !closesWithin(c, br, 2*time.Second) {
			t.Fatal("WebSocket survived its route cookie expiring")
		}
	})
	t.Run("desktop link cookie", func(t *testing.T) {
		e := newLiveEnv(t)
		exp := time.Now().Add(time.Hour).Unix()
		ct := strconv.FormatInt(exp, 10) + "." + signDesktop(e.g.k(purposeDesktopLink), "abc-1", 0, exp)
		br := e.openSSE(t, "abc-1.desktop.boat.test", cookie(desktopCookie, ct))
		e.skew.Store(later)
		if !sseClosesWithin(br, 2*time.Second) {
			t.Fatal("desktop stream survived its link expiring")
		}
	})
	t.Run("public unaffected", func(t *testing.T) {
		e := newLiveEnv(t)
		br := e.openSSE(t, e.pubHost)
		e.skew.Store(later)
		time.Sleep(300 * time.Millisecond)
		if sseClosesWithin(br, 10*time.Millisecond) {
			t.Fatal("public stream ended by clock advance")
		}
	})
}
