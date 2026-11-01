package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSignWebhook(t *testing.T) {
	body := []byte(`{"a":1}`)
	got := SignWebhook([]byte("whsec_x"), 1700000000, body)
	m := hmac.New(sha256.New, []byte("whsec_x"))
	m.Write([]byte("1700000000." + string(body)))
	if want := "t=1700000000,v1=" + hex.EncodeToString(m.Sum(nil)); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestMatchEvent(t *testing.T) {
	cases := []struct {
		f    []string
		typ  string
		want bool
	}{
		{nil, "vm.created", true},
		{[]string{"*"}, "vm.stopped", true},
		{[]string{"vm.*"}, "vm.stopped", true},
		{[]string{"vm.*"}, "vmx.stopped", false},
		{[]string{"vm.created"}, "vm.stopped", false},
	}
	for _, c := range cases {
		if got := matchEvent(c.f, c.typ); got != c.want {
			t.Errorf("matchEvent(%v,%s)=%v", c.f, c.typ, got)
		}
	}
}

type hookTarget struct {
	mu    sync.Mutex
	fails int
	got   []*http.Request
	body  []string
}

func (h *hookTarget) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, r)
	h.body = append(h.body, string(b))
	if h.fails > 0 {
		h.fails--
		w.WriteHeader(500)
		return
	}
	w.WriteHeader(204)
}

func (h *hookTarget) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.got)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWebhookRetryAndSignature(t *testing.T) {
	e := setup(t)
	target := &hookTarget{fails: 2}
	ts := httptest.NewServer(target)
	defer ts.Close()
	st, h := e.do(e.owner, "POST", "/v1/webhooks", map[string]any{"url": ts.URL, "events": []string{"webhook.*"}})
	if st != 201 || !strings.HasPrefix(h["secret"].(string), "whsec_") {
		t.Fatalf("create: %d %v", st, h)
	}
	id, secret := h["id"].(string), h["secret"].(string)
	if _, list := e.raw(e.owner, "GET", "/v1/webhooks", nil); strings.Contains(string(list), secret) {
		t.Error("secret returned after creation")
	}
	e.do(e.owner, "POST", "/v1/webhooks/"+id+"/test", nil)
	waitFor(t, "3 attempts", func() bool { return len(e.srv.store.Snapshot().Deliveries[id]) == 3 })
	d := e.srv.store.Snapshot().Deliveries[id]
	if d[0].OK || d[1].OK || !d[2].OK || d[2].Attempt != 3 {
		t.Errorf("deliveries: %+v", d)
	}
	r := target.got[2]
	var ts0 int64
	fmt.Sscanf(r.Header.Get("X-Boat-Signature"), "t=%d,", &ts0)
	if r.Header.Get("X-Boat-Signature") != SignWebhook([]byte(secret), ts0, []byte(target.body[2])) {
		t.Error("signature does not verify with the returned secret")
	}
	if r.Header.Get("X-Boat-Event") != "webhook.test" {
		t.Errorf("event header %q", r.Header.Get("X-Boat-Event"))
	}
	if st, out := e.do(e.owner, "GET", "/v1/webhooks/"+id+"/deliveries", nil); st != 200 || len(out["deliveries"].([]any)) != 3 {
		t.Errorf("delivery log: %d", st)
	}
}

func TestWebhookGivesUpOn4xx(t *testing.T) {
	e := setup(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(410) }))
	defer ts.Close()
	_, h := e.do(e.owner, "POST", "/v1/webhooks", map[string]any{"url": ts.URL})
	id := h["id"].(string)
	e.do(e.owner, "POST", "/v1/webhooks/"+id+"/test", nil)
	waitFor(t, "1 attempt", func() bool { return len(e.srv.store.Snapshot().Deliveries[id]) == 1 })
	time.Sleep(20 * time.Millisecond)
	if n := len(e.srv.store.Snapshot().Deliveries[id]); n != 1 {
		t.Errorf("4xx should not retry, got %d attempts", n)
	}
}

func TestWebhookFromBoatdEvents(t *testing.T) {
	e := setup(t)
	target := &hookTarget{}
	ts := httptest.NewServer(target)
	defer ts.Close()
	e.do(e.owner, "POST", "/v1/webhooks", map[string]any{"url": ts.URL, "events": []string{"vm.stopped"}})
	vm := e.createVM()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.srv.hooks.watch(ctx)
	e.boatd.events <- `{"type":"vm.created","vm_id":"` + vm + `"}`           // filtered out
	e.boatd.events <- `{"type":"vm.stopped","vm_id":"` + vm + `"}`           // delivered
	e.boatd.events <- `{"type":"vm.stopped","vm":{"id":"x","team":"other"}}` // other team
	waitFor(t, "delivery", func() bool { return target.count() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if target.count() != 1 || !strings.Contains(target.body[0], `"vm.stopped"`) || strings.Contains(target.body[0], "env") {
		t.Errorf("got %d deliveries: %v", target.count(), target.body)
	}
}

func TestDenyPrivate(t *testing.T) {
	for _, a := range []string{"127.0.0.1:80", "10.1.2.3:443", "[::1]:80", "169.254.169.254:80", "100.64.0.1:80"} {
		if denyPrivate("tcp", a, nil) == nil {
			t.Errorf("%s should be denied", a)
		}
	}
	if err := denyPrivate("tcp", "93.184.216.34:443", nil); err != nil {
		t.Errorf("public address denied: %v", err)
	}
}
