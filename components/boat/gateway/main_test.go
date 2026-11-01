package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// One upstream on 127.0.0.1 stands in for a VM; routes.json points at it.
func setup(t *testing.T) (*gateway, *httptest.Server, string, int) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("path=" + r.URL.Path + " cookie=" + r.Header.Get("Cookie") + " host=" + r.Host))
	}))
	t.Cleanup(up.Close)
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	dir := t.TempDir()
	rp := filepath.Join(dir, "routes.json")
	os.WriteFile(rp, []byte(`{"vms":{"abc-1":{"ip":"127.0.0.1","ports":[`+portStr+`],"desktopPort":`+portStr+`}}}`), 0o644)
	g := &gateway{domain: "boat.test", key: []byte(strings.Repeat("k", 32)), routes: &routes{path: rp}, now: time.Now}
	port, _ := strconv.Atoi(portStr)
	return g, up, rp, port
}

func do(g http.Handler, host, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://"+host+path, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}

func TestPortRoutes(t *testing.T) {
	g, _, rp, port := setup(t)
	p := itoa(port)
	if w := do(g, p+"-abc-1.vm.boat.test", "/x"); w.Code != 200 || !strings.Contains(w.Body.String(), "path=/x") ||
		!strings.Contains(w.Body.String(), "host="+p+"-abc-1.vm.boat.test") {
		t.Fatalf("route: %d %s", w.Code, w.Body)
	}
	for _, h := range []string{"1-abc-1.vm.boat.test", p + "-nope.vm.boat.test", "x-abc-1.vm.boat.test", p + "-abc-1.vm.evil.test", "a." + p + "-abc-1.vm.boat.test"} {
		if w := do(g, h, "/"); w.Code != 404 {
			t.Fatalf("%s: want 404, got %d", h, w.Code)
		}
	}
	// Route removed on disk -> gone without restart (mtime/size changes).
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(rp, []byte(`{"vms":{}}`), 0o644)
	if w := do(g, p+"-abc-1.vm.boat.test", "/"); w.Code != 404 {
		t.Fatalf("stale route served: %d", w.Code)
	}
}

func TestDesktopAuth(t *testing.T) {
	g, _, _, _ := setup(t)
	link := func(key []byte, vmid string, d time.Duration) string {
		exp := time.Now().Add(d).Unix()
		return "exp=" + strconv.FormatInt(exp, 10) + "&sig=" + sign(key, vmid, exp)
	}
	good := link(g.key, "abc-1", time.Hour)
	cases := map[string]int{
		"/abc-1/?" + good: 302,
		"/abc-1/?" + link(g.key, "abc-1", -time.Second):        403, // expired
		"/abc-1/?" + link(g.key, "other", time.Hour):           403, // other VM
		"/abc-1/?" + link([]byte("wrong"), "abc-1", time.Hour): 403,
		"/abc-1/?exp=1&sig=00":                                 403,
		"/abc-1/?sig=":                                         403,
		"/abc-1/":                                              401,
		"/abc-1?" + good:                                       302, // boat-api's link has no trailing slash
		"/nope/":                                               404,
	}
	for path, want := range cases {
		if w := do(g, "desktop.boat.test", path); w.Code != want {
			t.Errorf("%s: want %d, got %d", path, want, w.Code)
		}
	}
	if w := do(g, "desktop.boat.test", "/abc-1?"+good); w.Header().Get("Location") != "/abc-1/?"+good {
		t.Fatalf("slash redirect drops the link: %q", w.Header().Get("Location"))
	}
	w := do(g, "desktop.boat.test", "/abc-1/vnc.html?"+good+"&a=1")
	if loc := w.Header().Get("Location"); loc != "/abc-1/vnc.html?a=1" {
		t.Fatalf("redirect keeps the signature: %q", loc)
	}
	c := w.Result().Cookies()[0]
	if c.Path != "/abc-1/" || !c.HttpOnly || !c.Secure {
		t.Fatalf("cookie: %+v", c)
	}
	w = do(g, "desktop.boat.test", "/abc-1/vnc.html", c, &http.Cookie{Name: "app", Value: "1"},
		&http.Cookie{Name: guestCookie, Value: "client-chosen"})
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "path=/vnc.html") || strings.Contains(body, cookieName) ||
		!strings.Contains(body, "app=1") || strings.Contains(body, "client-chosen") ||
		!strings.Contains(body, guestCookie+"="+guestToken(g.key, "abc-1")) {
		t.Fatalf("proxied desktop: %d %s", w.Code, body)
	}
}

func TestAsk(t *testing.T) {
	g, _, _, port := setup(t)
	for host, want := range map[string]int{
		itoa(port) + "-abc-1.vm.boat.test": 200, "desktop.boat.test": 200, "api.boat.test": 200,
		"9-abc-1.vm.boat.test": 404, "evil.test": 404, "": 404,
	} {
		w := httptest.NewRecorder()
		g.ask(w, httptest.NewRequest("GET", "/ask?domain="+host, nil))
		if w.Code != want {
			t.Errorf("ask %q: want %d, got %d", host, want, w.Code)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	raw, hx, bad := filepath.Join(dir, "raw"), filepath.Join(dir, "hex"), filepath.Join(dir, "bad")
	os.WriteFile(raw, []byte(strings.Repeat("k", 32)), 0o600)
	os.WriteFile(hx, []byte(strings.Repeat("6b", 32)+"\n"), 0o600)
	os.WriteFile(bad, []byte("short"), 0o600)
	a, err1 := loadKey(raw)
	b, err2 := loadKey(hx)
	if err1 != nil || err2 != nil || string(a) != string(b) {
		t.Fatalf("raw/hex keys differ: %v %v", err1, err2)
	}
	if _, err := loadKey(bad); err == nil {
		t.Fatal("short key accepted")
	}
}
