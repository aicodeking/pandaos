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

// One upstream on 127.0.0.1 stands in for both VMs; routes.json points at it.
// VM abc-1: private route on the upstream port; VM xyz-2: the same port public.
func setup(t *testing.T) (*gateway, string, int) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("path=" + r.URL.RequestURI() + " cookie=" + r.Header.Get("Cookie") +
			" host=" + r.Host + " hdr=" + r.Header.Get(routeHeader)))
	}))
	t.Cleanup(up.Close)
	_, ps, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	rp := filepath.Join(t.TempDir(), "routes.json")
	writeRoutes(t, rp, `{"vms":{"abc-1":{"ip":"127.0.0.1","ports":[`+ps+`],"desktopPort":`+ps+`},
		"xyz-2":{"ip":"127.0.0.1","ports":[`+ps+`],"public":[`+ps+`],"desktopPort":`+ps+`}}}`)
	g := &gateway{domain: "boat.test", vmDomain: "vm.boatvms.test", desktopDomain: "desktop.boat.test",
		key: []byte(strings.Repeat("k", 32)), routes: &routes{path: rp}, now: time.Now}
	port, _ := strconv.Atoi(ps)
	return g, rp, port
}

func writeRoutes(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func req(g http.Handler, host, path string, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://"+host+path, nil)
	for _, m := range mod {
		m(r)
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}

func cookie(name, value string) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: name, Value: value}) }
}

func header(k, v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

func tok(sig func(int64) string, d time.Duration) (string, int64) {
	exp := time.Now().Add(d).Unix()
	return strconv.FormatInt(exp, 10) + "." + sig(exp), exp
}

func TestPortRoutesPrivateByDefault(t *testing.T) {
	g, rp, port := setup(t)
	p := strconv.Itoa(port)
	priv, pub := p+"-abc-1.vm.boatvms.test", p+"-xyz-2.vm.boatvms.test"

	if w := req(g, pub, "/x"); w.Code != 200 || !strings.Contains(w.Body.String(), "host="+pub) {
		t.Fatalf("public route: %d %s", w.Code, w.Body)
	}
	if w := req(g, priv, "/x"); w.Code != 401 {
		t.Fatalf("private route without token: %d", w.Code)
	}
	good, exp := tok(func(e int64) string { return signRoute(g.key, port, "abc-1", e) }, time.Hour)
	w := req(g, priv, "/x", header(routeHeader, good), cookie("app", "1"))
	if b := w.Body.String(); w.Code != 200 || strings.Contains(b, good) || !strings.Contains(b, "cookie=app=1") {
		t.Fatalf("header token: %d %s", w.Code, b)
	}
	bad := map[string]string{
		"expired":    first(tok(func(e int64) string { return signRoute(g.key, port, "abc-1", e) }, -time.Second)),
		"other vm":   first(tok(func(e int64) string { return signRoute(g.key, port, "xyz-2", e) }, time.Hour)),
		"other port": first(tok(func(e int64) string { return signRoute(g.key, port+1, "abc-1", e) }, time.Hour)),
		"desktop":    first(tok(func(e int64) string { return signDesktop(g.key, "abc-1", e) }, time.Hour)),
		"garbage":    "1.00",
	}
	for name, b := range bad {
		if w := req(g, priv, "/", header(routeHeader, b)); w.Code != 401 {
			t.Errorf("%s token: want 401, got %d", name, w.Code)
		}
		if w := req(g, priv, "/", cookie(routeCookie, b)); w.Code != 401 {
			t.Errorf("%s cookie: want 401, got %d", name, w.Code)
		}
	}
	// Link -> host-only cookie, signature dropped from the URL.
	sig := strings.SplitN(good, ".", 2)[1]
	w = req(g, priv, "/a?q=1&boat_exp="+strconv.FormatInt(exp, 10)+"&boat_sig="+sig)
	c := w.Result().Cookies()
	if w.Code != 200 || len(c) != 1 || c[0].Name != routeCookie || c[0].Domain != "" || c[0].Path != "/" ||
		!c[0].Secure || !c[0].HttpOnly || !strings.Contains(w.Body.String(), `url=/a?q=1"`) {
		t.Fatalf("route link: %d %v %s", w.Code, c, w.Body)
	}
	if w := req(g, priv, "/a", cookie(routeCookie, "junk"), cookie(routeCookie, c[0].Value)); w.Code != 200 ||
		strings.Contains(w.Body.String(), routeCookie) {
		t.Fatalf("route cookie: %d %s", w.Code, w.Body)
	}
	for _, h := range []string{"1-abc-1.vm.boatvms.test", p + "-nope.vm.boatvms.test", "x-abc-1.vm.boatvms.test",
		p + "-abc-1.vm.boat.test", "a." + priv} {
		if w := req(g, h, "/", header(routeHeader, good)); w.Code != 404 {
			t.Errorf("%s: want 404, got %d", h, w.Code)
		}
	}
	// Route removed on disk -> gone without restart (size changes).
	writeRoutes(t, rp, `{"vms":{}}`)
	if w := req(g, pub, "/"); w.Code != 404 {
		t.Fatalf("stale route served: %d", w.Code)
	}
}

func first(s string, _ int64) string { return s }

func TestDesktopPerVMOrigin(t *testing.T) {
	g, _, _ := setup(t)
	link := func(vmid string, d time.Duration) string {
		exp := time.Now().Add(d).Unix()
		return "exp=" + strconv.FormatInt(exp, 10) + "&sig=" + signDesktop(g.key, vmid, exp)
	}
	a, b := "abc-1.desktop.boat.test", "xyz-2.desktop.boat.test"

	// boat-api's shared-origin link -> the VM's own origin, nothing set here.
	w := req(g, "desktop.boat.test", "/abc-1?"+link("abc-1", time.Hour), header("X-Forwarded-Proto", "https"))
	loc := w.Header().Get("Location")
	if w.Code != 302 || !strings.HasPrefix(loc, "https://"+a+"/?exp=") || len(w.Result().Cookies()) != 0 {
		t.Fatalf("legacy link: %d %q", w.Code, loc)
	}
	for _, p := range []string{"/abc-1/", "/abc-1", "/nope?" + link("nope", time.Hour), "/"} {
		if w := req(g, "desktop.boat.test", p); w.Code != 404 {
			t.Errorf("desktop.boat.test%s: want 404, got %d", p, w.Code)
		}
	}

	cases := map[string]int{
		"/?" + link("abc-1", -time.Second): 403, // expired
		"/?" + link("xyz-2", time.Hour):    403, // B's link on A's origin
		"/?exp=1&sig=00":                   403,
		"/":                                401,
	}
	for p, want := range cases {
		if w := req(g, a, p); w.Code != want {
			t.Errorf("%s%s: want %d, got %d", a, p, want, w.Code)
		}
	}
	w = req(g, a, "/vnc.html?a=1&"+link("abc-1", time.Hour))
	c := w.Result().Cookies()
	if w.Code != 200 || len(c) != 1 || c[0].Name != desktopCookie || c[0].Domain != "" || c[0].Path != "/" ||
		!c[0].Secure || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode ||
		!strings.Contains(w.Body.String(), `url=/vnc.html?a=1"`) {
		t.Fatalf("desktop session: %d %+v %s", w.Code, c, w.Body)
	}
	w = req(g, a, "/vnc.html", cookie(desktopCookie, c[0].Value), cookie("app", "1"), cookie(guestCookie, "client-chosen"))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "path=/vnc.html") || strings.Contains(body, desktopCookie) ||
		!strings.Contains(body, "app=1") || strings.Contains(body, "client-chosen") ||
		!strings.Contains(body, guestCookie+"="+guestToken(g.key, "abc-1")) {
		t.Fatalf("proxied desktop: %d %s", w.Code, body)
	}
	// A's session cookie is worthless on B's origin (and vice versa).
	if w := req(g, b, "/", cookie(desktopCookie, c[0].Value)); w.Code != 401 {
		t.Fatalf("A's cookie opened B's desktop: %d", w.Code)
	}
	if w := req(g, "x.abc-1.desktop.boat.test", "/", cookie(desktopCookie, c[0].Value)); w.Code != 404 {
		t.Fatalf("nested desktop host: %d", w.Code)
	}
}

func TestAsk(t *testing.T) {
	g, _, port := setup(t)
	p := strconv.Itoa(port)
	for host, want := range map[string]int{
		p + "-abc-1.vm.boatvms.test": 200, "desktop.boat.test": 200, "api.boat.test": 200,
		"abc-1.desktop.boat.test": 200, "nope.desktop.boat.test": 404,
		"9-abc-1.vm.boatvms.test": 404, "evil.test": 404, "": 404,
	} {
		w := httptest.NewRecorder()
		g.ask(w, httptest.NewRequest("GET", "/ask?domain="+host, nil))
		if w.Code != want {
			t.Errorf("ask %q: want %d, got %d", host, want, w.Code)
		}
	}
}

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
