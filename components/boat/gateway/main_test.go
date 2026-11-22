package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	good, exp := tok(func(e int64) string { return signRoute(g.k(purposePortRoute), port, "abc-1", 0, e) }, time.Hour)
	w := req(g, priv, "/x", header(routeHeader, good), cookie("app", "1"))
	if b := w.Body.String(); w.Code != 200 || strings.Contains(b, good) || !strings.Contains(b, "cookie=app=1") {
		t.Fatalf("header token: %d %s", w.Code, b)
	}
	bad := map[string]string{
		"expired":    first(tok(func(e int64) string { return signRoute(g.k(purposePortRoute), port, "abc-1", 0, e) }, -time.Second)),
		"other vm":   first(tok(func(e int64) string { return signRoute(g.k(purposePortRoute), port, "xyz-2", 0, e) }, time.Hour)),
		"other port": first(tok(func(e int64) string { return signRoute(g.k(purposePortRoute), port+1, "abc-1", 0, e) }, time.Hour)),
		"desktop":    first(tok(func(e int64) string { return signDesktop(g.k(purposeDesktopLink), "abc-1", 0, e) }, time.Hour)),
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
	if w.Code != 302 || len(c) != 1 || c[0].Name != routeCookie || c[0].Domain != "" || c[0].Path != "/" ||
		!c[0].Secure || !c[0].HttpOnly || w.Header().Get("Location") != "/a?q=1" {
		t.Fatalf("route link: %d %v %q", w.Code, c, w.Header().Get("Location"))
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
		return "exp=" + strconv.FormatInt(exp, 10) + "&sig=" + signDesktop(g.k(purposeDesktopLink), vmid, 0, exp)
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
	if w.Code != 302 || len(c) != 1 || c[0].Name != desktopCookie || c[0].Domain != "" || c[0].Path != "/" ||
		!c[0].Secure || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode ||
		w.Header().Get("Location") != "/vnc.html?a=1" {
		t.Fatalf("desktop session: %d %+v %q", w.Code, c, w.Header().Get("Location"))
	}
	// Started on another site: a Strict cookie needs a fresh same-origin navigation.
	w = req(g, a, "/vnc.html?a=1&"+link("abc-1", time.Hour), header("Sec-Fetch-Site", "cross-site"), header("Sec-Fetch-Mode", "navigate"), header("Sec-Fetch-Dest", "document"))
	if w.Code != 200 || len(w.Result().Cookies()) != 1 || !strings.Contains(w.Body.String(), `url=/vnc.html?a=1"`) {
		t.Fatalf("cross-site desktop session: %d %s", w.Code, w.Body)
	}
	w = req(g, a, "/vnc.html", cookie(desktopCookie, c[0].Value), cookie("app", "1"), cookie(guestCookie, "client-chosen"))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "path=/vnc.html") || strings.Contains(body, desktopCookie) ||
		!strings.Contains(body, "app=1") || strings.Contains(body, "client-chosen") ||
		!strings.Contains(body, guestCookie+"="+guestToken(g.k(purposeGuestDesktop), "abc-1")) {
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

// A page on VM A must not use the browser's session for VM B, although both
// origins are same-site. Covers the websocket upgrade the council found open.
func TestCrossOriginRefused(t *testing.T) {
	g, _, port := setup(t)
	p := strconv.Itoa(port)
	dA, dB := "abc-1.desktop.boat.test", "xyz-2.desktop.boat.test"
	cB := cookie(desktopCookie, first(tok(func(e int64) string { return signDesktop(g.k(purposeDesktopLink), "xyz-2", 0, e) }, time.Hour)))
	https := header("X-Forwarded-Proto", "https")
	ws := func(r *http.Request) {
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
	}
	cases := []struct {
		name string
		mods []func(*http.Request)
		want int
	}{
		{"own origin", []func(*http.Request){cB, https, header("Origin", "https://"+dB)}, 200},
		{"own origin with default port", []func(*http.Request){cB, https, header("Origin", "https://"+dB+":443")}, 200},
		{"no origin, typed url", []func(*http.Request){cB, https, header("Sec-Fetch-Site", "none")}, 200},
		{"websocket from VM A", []func(*http.Request){cB, https, ws, header("Origin", "https://"+dA)}, 403},
		{"xhr from VM A", []func(*http.Request){cB, https, header("Origin", "https://"+dA)}, 403},
		{"port route page of VM A", []func(*http.Request){cB, https, header("Origin", "https://8080-abc-1.vm.boatvms.test")}, 403},
		{"downgraded scheme", []func(*http.Request){cB, https, header("Origin", "http://"+dB)}, 403},
		{"null origin", []func(*http.Request){cB, https, header("Origin", "null")}, 403},
		{"same-site subresource", []func(*http.Request){cB, https, header("Sec-Fetch-Site", "same-site"), header("Sec-Fetch-Mode", "no-cors")}, 403},
		{"same-site iframe", []func(*http.Request){cB, https, header("Sec-Fetch-Site", "same-site"), header("Sec-Fetch-Mode", "navigate"), header("Sec-Fetch-Dest", "iframe")}, 403},
		{"same-site navigation with the cookie", []func(*http.Request){cB, https, header("Sec-Fetch-Site", "same-site"), header("Sec-Fetch-Mode", "navigate"), header("Sec-Fetch-Dest", "document")}, 403},
	}
	for _, c := range cases {
		if w := req(g, dB, "/", c.mods...); w.Code != c.want {
			t.Errorf("desktop, %s: want %d, got %d", c.name, c.want, w.Code)
		}
	}
	// A signed link may still arrive from anywhere, and ends on a same-origin page.
	exp := time.Now().Add(time.Hour).Unix()
	q := "/?exp=" + strconv.FormatInt(exp, 10) + "&sig=" + signDesktop(g.k(purposeDesktopLink), "xyz-2", 0, exp)
	for _, site := range []string{"cross-site", "same-site"} {
		w := req(g, dB, q, https, header("Sec-Fetch-Site", site), header("Sec-Fetch-Mode", "navigate"), header("Sec-Fetch-Dest", "document"))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "http-equiv") || len(w.Result().Cookies()) != 1 {
			t.Errorf("link from %s: %d %s", site, w.Code, w.Body)
		}
	}
	if w := req(g, dB, "/", cB, https); !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'self'") {
		t.Errorf("desktop is embeddable: %v", w.Header())
	}

	// Private port route: another VM is refused, another port of the same VM is
	// allowed, and the explicit token header needs no Origin rule.
	priv := p + "-abc-1.vm.boatvms.test"
	rt := first(tok(func(e int64) string { return signRoute(g.k(purposePortRoute), port, "abc-1", 0, e) }, time.Hour))
	cR := cookie(routeCookie, rt)
	for name, c := range map[string]struct {
		mods []func(*http.Request)
		want int
	}{
		"own":                       {[]func(*http.Request){cR, https, header("Origin", "https://"+priv)}, 200},
		"same VM":                   {[]func(*http.Request){cR, https, header("Origin", "https://3000-abc-1.vm.boatvms.test")}, 200},
		"same VM downgraded scheme": {[]func(*http.Request){cR, https, header("Origin", "http://3000-abc-1.vm.boatvms.test")}, 403},
		"other VM":                  {[]func(*http.Request){cR, https, ws, header("Origin", "https://3000-xyz-2.vm.boatvms.test")}, 403},
		"no-cors":                   {[]func(*http.Request){cR, https, header("Sec-Fetch-Site", "same-site"), header("Sec-Fetch-Mode", "no-cors")}, 403},
		"navigation":                {[]func(*http.Request){cR, https, header("Sec-Fetch-Site", "same-site"), header("Sec-Fetch-Mode", "navigate"), header("Sec-Fetch-Dest", "document")}, 200},
		"header only":               {[]func(*http.Request){header(routeHeader, rt), https, header("Origin", "https://app.example")}, 200},
	} {
		if w := req(g, priv, "/", c.mods...); w.Code != c.want {
			t.Errorf("route, %s: want %d, got %d", name, c.want, w.Code)
		}
	}
	// Public routes are public: no Origin rule.
	if w := req(g, p+"-xyz-2.vm.boatvms.test", "/", header("Origin", "https://evil.example")); w.Code != 200 {
		t.Errorf("public route with foreign Origin: %d", w.Code)
	}
}

func TestCheckOriginSameTenantScheme(t *testing.T) {
	same := func(host string) bool { return host == "3000-abc-1.vm.boatvms.test" }
	for _, tc := range []struct {
		name   string
		proto  string
		origin string
		wantOK bool
	}{
		{"https to https sibling", "https", "https://3000-abc-1.vm.boatvms.test", true},
		{"http to https sibling rejected", "https", "http://3000-abc-1.vm.boatvms.test", false},
		{"https to http sibling rejected", "http", "https://3000-abc-1.vm.boatvms.test", false},
		{"http to http sibling", "http", "http://3000-abc-1.vm.boatvms.test", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://8080-abc-1.vm.boatvms.test/", nil)
			r.Host = "8080-abc-1.vm.boatvms.test"
			r.Header.Set("X-Forwarded-Proto", tc.proto)
			r.Header.Set("Origin", tc.origin)
			err := checkOrigin(r, same)
			if tc.wantOK && err != nil {
				t.Errorf("want nil, got %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Errorf("want error, got nil")
			}
		})
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

// A tenant's Domain cookie must not reach sibling tenants.
func TestCookieDomainStripped(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "evil=1; Domain=vm.boatvms.test; Path=/; Secure")
		w.Header().Add("Set-Cookie", "evil2=1;domain=.vm.boatvms.test")
		w.Header().Add("Set-Cookie", "keep=domain=x; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", "domain=kept; Path=/")
	}))
	defer up.Close()
	_, ps, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	rp := filepath.Join(t.TempDir(), "routes.json")
	writeRoutes(t, rp, `{"vms":{"xyz-2":{"ip":"127.0.0.1","ports":[`+ps+`],"public":[`+ps+`]}}}`)
	g := &gateway{domain: "boat.test", vmDomain: "vm.boatvms.test", desktopDomain: "desktop.boat.test",
		key: []byte(strings.Repeat("k", 32)), routes: &routes{path: rp}, now: time.Now}
	got := req(g, ps+"-xyz-2.vm.boatvms.test", "/").Header().Values("Set-Cookie")
	want := []string{"evil=1; Path=/; Secure", "evil2=1", "keep=domain=x; Path=/; HttpOnly", "domain=kept; Path=/"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A revoked generation kills earlier tokens, and only for that VM's routes.
func TestRouteTokenRevocation(t *testing.T) {
	g, _, port := setup(t)
	gens := `{"gens":{}}`
	status := 200
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/route-gens" {
			t.Errorf("polled %s", r.URL.Path)
		}
		w.WriteHeader(status)
		w.Write([]byte(gens))
	}))
	defer api.Close()
	g.gens = newRouteGens(api.URL)
	host := strconv.Itoa(port) + "-abc-1.vm.boatvms.test"
	old := first(tok(func(e int64) string { return signRoute(g.k(purposePortRoute), port, "abc-1", 0, e) }, time.Hour))
	withTok := func(tk string) int {
		return req(g, host, "/", func(r *http.Request) { r.Header.Set(routeHeader, tk) }).Code
	}
	if c := withTok(old); c != 200 {
		t.Fatalf("generation 0 token before any revocation: %d", c)
	}
	gens = `{"gens":{"abc-1":1}}`
	if err := g.gens.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c := withTok(old); c != 401 {
		t.Errorf("revoked token: %d", c)
	}
	fresh := first(tok(func(e int64) string { return signRoute(g.k(purposePortRoute), port, "abc-1", 1, e) }, time.Hour))
	if c := withTok(fresh); c != 200 {
		t.Errorf("token of the new generation: %d", c)
	}
	// A failing API keeps the table: no way back to accepting the old token.
	status = 500
	if err := g.gens.refresh(t.Context()); err == nil {
		t.Error("HTTP 500 accepted")
	}
	if c := withTok(old); c != 401 {
		t.Errorf("revoked token while the API fails: %d", c)
	}
}

// Before the first answer a revoked token cannot be recognised, so none passes;
// public routes do not depend on it. An API without the route (404) has no
// revocations.
func TestRouteGensFailClosedAtStart(t *testing.T) {
	g, _, port := setup(t)
	status := 500
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer api.Close()
	g.gens = newRouteGens(api.URL)
	good := first(tok(func(e int64) string { return signRoute(g.k(purposePortRoute), port, "abc-1", 0, e) }, time.Hour))
	withTok := func(r *http.Request) { r.Header.Set(routeHeader, good) }
	host := strconv.Itoa(port) + "-abc-1.vm.boatvms.test"
	if c := req(g, host, "/", withTok).Code; c != 503 {
		t.Errorf("no table yet: %d", c)
	}
	if c := req(g, strconv.Itoa(port)+"-xyz-2.vm.boatvms.test", "/").Code; c != 200 {
		t.Errorf("public route without a table: %d", c)
	}
	// A 404 (an API without the route, or a misrouted -api) fails closed too.
	status = 404
	if c := req(g, host, "/", withTok).Code; c != 503 {
		t.Errorf("route-gens 404: %d, want 503", c)
	}
}

// Desktop links follow the same generations: a revoked VM's old link and
// cookie stop working, the new generation's work.
func TestDesktopLinkRevocation(t *testing.T) {
	g, _, _ := setup(t)
	gens := `{"gens":{},"desktops":["abc-1"]}`
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(gens)) }))
	defer api.Close()
	g.gens = newRouteGens(api.URL)
	link := func(gen int) string {
		exp := time.Now().Add(time.Hour).Unix()
		return "/?exp=" + strconv.FormatInt(exp, 10) + "&sig=" + signDesktop(g.k(purposeDesktopLink), "abc-1", gen, exp)
	}
	host := "abc-1.desktop.boat.test"
	old := link(0)
	if c := req(g, host, old).Code; c != 302 {
		t.Fatalf("link before revocation: %d", c)
	}
	gens = `{"gens":{"abc-1":1},"desktops":["abc-1"]}`
	if err := g.gens.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c := req(g, host, old).Code; c != 403 {
		t.Errorf("revoked desktop link: %d", c)
	}
	if c := req(g, host, link(1)).Code; c != 302 {
		t.Errorf("new generation's link: %d", c)
	}
}

// ask approves a desktop host only once a link was minted for it, and only so
// many new hostnames per team per window; known hostnames always pass.
func TestAskDesktopNeedsLinkAndTeamQuota(t *testing.T) {
	g, rp, port := setup(t)
	p := strconv.Itoa(port)
	writeRoutes(t, rp, `{"vms":{"abc-1":{"ip":"127.0.0.1","ports":[`+p+`,81,82],"team":"t1"},
		"xyz-2":{"ip":"127.0.0.1","ports":[`+p+`],"team":"t1"},"other":{"ip":"127.0.0.1","ports":[`+p+`],"team":"t2"}}}`)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"gens":{},"desktops":["abc-1"]}`))
	}))
	defer api.Close()
	g.gens = newRouteGens(api.URL)
	now := time.Now()
	g.now = func() time.Time { return now }
	g.certs, _ = newCertQuota(2, 24*time.Hour, 0, 0, "")
	ask := func(host string) int {
		w := httptest.NewRecorder()
		g.ask(w, httptest.NewRequest("GET", "/ask?domain="+host, nil))
		return w.Code
	}
	steps := []struct {
		host string
		want int
	}{
		{"xyz-2.desktop.boat.test", 404}, // no link minted
		{"abc-1.desktop.boat.test", 200},
		{p + "-abc-1.vm.boatvms.test", 200},
		{"81-abc-1.vm.boatvms.test", 429}, // t1's quota (2) is used up
		{p + "-xyz-2.vm.boatvms.test", 429},
		{"abc-1.desktop.boat.test", 200},    // already approved: renewals pass
		{p + "-other.vm.boatvms.test", 200}, // another team has its own quota
		{"api.boat.test", 200},
	}
	for _, s := range steps {
		if c := ask(s.host); c != s.want {
			t.Errorf("ask %s: %d, want %d", s.host, c, s.want)
		}
	}
	now = now.Add(25 * time.Hour)
	if c := ask("81-abc-1.vm.boatvms.test"); c != 200 {
		t.Errorf("after the window: %d", c)
	}
}

// Each kind of token has its own key derived from the master key, and a token
// signed with the master key itself (before the split) verifies only during
// the legacy window and only if it expires within it.
func TestPurposeKeysAndLegacyWindow(t *testing.T) {
	g, _, port := setup(t)
	priv := strconv.Itoa(port) + "-abc-1.vm.boatvms.test"
	route := func(key []byte, d time.Duration) string {
		return first(tok(func(e int64) string { return signRoute(key, port, "abc-1", 0, e) }, d))
	}
	code := func(token string) int { return req(g, priv, "/", header(routeHeader, token)).Code }
	keys := map[string]bool{string(g.key): true}
	for _, p := range []string{purposeDesktopLink, purposePortRoute, purposeGuestDesktop} {
		keys[string(g.k(p))] = true
	}
	if len(keys) != 4 {
		t.Fatal("purpose keys must differ from each other and from the master key")
	}
	// boat-api's PurposeKey for the same master (api/crypto.go): HMAC(master, "boat-hmac-v2/<purpose>").
	if got := hex.EncodeToString(purposeKey([]byte(strings.Repeat("k", 32)), purposePortRoute)); got != mac([]byte(strings.Repeat("k", 32)), "boat-hmac-v2/port-route") {
		t.Fatalf("derivation: %s", got)
	}
	for name, k := range map[string][]byte{"desktop-link key": g.k(purposeDesktopLink), "guest key": g.k(purposeGuestDesktop), "master key": g.key} {
		if c := code(route(k, time.Hour)); c != 401 {
			t.Errorf("route token signed with the %s, no legacy window: %d", name, c)
		}
	}
	if c := code(route(g.k(purposePortRoute), time.Hour)); c != 200 {
		t.Fatalf("route token: %d", c)
	}
	if strings.Count(g.guestCookies("abc-1"), guestCookie+"=") != 1 {
		t.Errorf("guest cookies outside the window: %s", g.guestCookies("abc-1"))
	}

	g.legacyUntil = time.Now().Add(2 * time.Hour)
	if c := code(route(g.key, time.Hour)); c != 200 {
		t.Errorf("legacy token within the window: %d", c)
	}
	if c := code(route(g.key, 3*time.Hour)); c != 401 {
		t.Errorf("legacy token expiring after the window: %d", c)
	}
	if c := code(route(g.k(purposeDesktopLink), time.Hour)); c != 401 {
		t.Errorf("another purpose's key during the window: %d", c)
	}
	gc := g.guestCookies("abc-1")
	if !strings.Contains(gc, guestCookie+"="+guestToken(g.k(purposeGuestDesktop), "abc-1")) ||
		!strings.Contains(gc, guestCookie+"="+guestToken(g.key, "abc-1")) {
		t.Errorf("guest cookies during the window: %s", gc)
	}

	g.legacyUntil = time.Now().Add(-time.Second)
	if c := code(route(g.key, time.Minute)); c != 401 {
		t.Errorf("legacy token after the window: %d", c)
	}
}

// Caddy sends api.<domain> paths to boat-api and the rest to the dashboard.
// The official boat.dev SDKs call https://api.<domain>/api/v1/...: without
// /api/v1/* in the API's paths they get the dashboard's index.html.
func TestCaddyProxiesCompatAPI(t *testing.T) {
	mod, err := os.ReadFile("module.nix")
	if os.IsNotExist(err) {
		t.Skip("not in the source tree (the nix package builds only the Go files); ci.yml's go test runs it")
	}
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`apiPaths = \[([^\]]*)\]`).FindSubmatch(mod)
	if m == nil || !strings.Contains(string(m[1]), `"/api/v1/*"`) || !strings.Contains(string(m[1]), `"/v1/*"`) {
		t.Errorf("module.nix apiPaths must hold /v1/* and /api/v1/*: %q", m)
	}
	edge, err := os.ReadFile("e2e-edge.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`handle /api/v1/\* \{\s*reverse_proxy 127\.0\.0\.1:8740`).Match(edge) {
		t.Error("e2e-edge.sh's Caddyfile must proxy /api/v1/* like module.nix")
	}
}

func TestQuarantinedSourceIPBlocked(t *testing.T) {
	g, rp, port := setup(t)
	p := strconv.Itoa(port)
	pub := p + "-xyz-2.vm.boatvms.test"
	writeRoutes(t, rp, `{"vms":{"abc-1":{"ip":"127.0.0.1","ports":[`+p+`],"desktopPort":`+p+`},
		"xyz-2":{"ip":"127.0.0.1","ports":[`+p+`],"public":[`+p+`],"desktopPort":`+p+`}},
		"quarantined":["10.200.0.3","::ffff:10.200.0.4"," fd42:b0a7::3 "]}`)

	remote := func(addr string) func(*http.Request) {
		return func(r *http.Request) { r.RemoteAddr = addr }
	}
	ws := func(r *http.Request) {
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
	}

	// Direct requests from quarantined IPs (IPv4, no-port, IPv4-mapped IPv6, IPv6) -> 403 across all routes.
	for _, peer := range []string{"10.200.0.3:54321", "10.200.0.3", "[::ffff:10.200.0.3]:54321", "10.200.0.4:1111", "[fd42:b0a7::3]:2222"} {
		for _, target := range []struct {
			host, path string
			mods       []func(*http.Request)
		}{
			{pub, "/probe", nil},
			{pub, "/ws", []func(*http.Request){ws}},
			{"desktop.boat.test", "/xyz-2?exp=9999999999&sig=00", nil},
			{"xyz-2.desktop.boat.test", "/", nil},
		} {
			mods := append([]func(*http.Request){remote(peer)}, target.mods...)
			w := req(g, target.host, target.path, mods...)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "quarantined vm cannot access gateway") {
				t.Fatalf("direct peer %q -> %s%s: got %d %q, want 403", peer, target.host, target.path, w.Code, w.Body.String())
			}
		}
	}

	// Loopback Caddy-proxied requests carrying a quarantined client IP in X-Forwarded-For or X-Real-Ip -> 403.
	for _, loop := range []string{"127.0.0.1:43100", "127.0.0.2:43100", "[::1]:43100", "[::ffff:127.0.0.1]:43100"} {
		for _, mod := range []func(*http.Request){
			header("X-Forwarded-For", "10.200.0.3"),
			header("X-Forwarded-For", "203.0.113.9, ::ffff:10.200.0.3"),
			header("X-Real-Ip", "10.200.0.4"),
			header("X-Real-Ip", "fd42:b0a7::3"),
		} {
			w := req(g, pub, "/via-caddy", remote(loop), mod)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "quarantined vm cannot access gateway") {
				t.Fatalf("loopback %q forwarded quarantined IP: got %d %q, want 403", loop, w.Code, w.Body.String())
			}
		}
		// Loopback Caddy forwarding a non-quarantined client IP -> 200 OK.
		if w := req(g, pub, "/ok", remote(loop), header("X-Forwarded-For", "10.200.0.99")); w.Code != http.StatusOK {
			t.Fatalf("loopback %q non-quarantined XFF: got %d, want 200", loop, w.Code)
		}
	}

	// Non-loopback peers cannot spoof X-Forwarded-For / X-Real-Ip; non-quarantined external client succeeds.
	if w := req(g, pub, "/external", remote("203.0.113.50:55555"),
		header("X-Forwarded-For", "10.200.0.3"), header("X-Real-Ip", "10.200.0.3")); w.Code != http.StatusOK {
		t.Fatalf("external peer with spoofed XFF: got %d %q, want 200", w.Code, w.Body.String())
	}
}

func TestProxyConnectionPoolingAndIdlePrune(t *testing.T) {
	var newConns, closedConns atomic.Int64
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok:" + r.URL.Path))
	}))
	up.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		switch st {
		case http.StateNew:
			newConns.Add(1)
		case http.StateClosed:
			closedConns.Add(1)
		}
	}
	up.Start()
	t.Cleanup(up.Close)

	_, ps, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	rp := filepath.Join(t.TempDir(), "routes.json")
	writeRoutes(t, rp, `{"vms":{"xyz-2":{"ip":"127.0.0.1","ports":[`+ps+`],"public":[`+ps+`]}}}`)
	g := &gateway{
		domain: "boat.test", vmDomain: "vm.boatvms.test", desktopDomain: "desktop.boat.test",
		key: []byte(strings.Repeat("k", 32)), routes: &routes{path: rp}, now: time.Now,
	}
	gwSrv := httptest.NewServer(g)
	t.Cleanup(gwSrv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	pubHost := ps + "-xyz-2.vm.boatvms.test"
	for i := range 5 {
		httpReq, err := http.NewRequest("GET", gwSrv.URL+"/seq-"+strconv.Itoa(i), nil)
		if err != nil {
			t.Fatal(err)
		}
		httpReq.Host = pubHost
		resp, err := client.Do(httpReq)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "ok:/seq-"+strconv.Itoa(i) {
			t.Fatalf("req %d: status=%d body=%q", i, resp.StatusCode, body)
		}
	}
	if got := newConns.Load(); got != 1 {
		t.Fatalf("backend TCP connection count across 5 sequential requests = %d, want 1", got)
	}

	// Removing the VM from routes.json prunes pooled idle connections so recycled IPs never hit stale sockets.
	writeRoutes(t, rp, `{"vms":{}}`)
	if w := req(g, pubHost, "/after-remove"); w.Code != http.StatusNotFound {
		t.Fatalf("removed route status = %d, want 404", w.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for closedConns.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := closedConns.Load(); got != 1 {
		t.Fatalf("closed pooled backend connections after route removal = %d, want 1", got)
	}
}

func TestSingleStatPerRequestAndAtomicRouteUpdates(t *testing.T) {
	g, rp, port := setup(t)
	var statCalls atomic.Int64
	g.routes.stat = func(p string) (os.FileInfo, error) {
		statCalls.Add(1)
		return os.Stat(p)
	}

	p := strconv.Itoa(port)
	pub := p + "-xyz-2.vm.boatvms.test"
	priv := p + "-abc-1.vm.boatvms.test"
	goodTok := first(tok(func(e int64) string { return signRoute(g.k(purposePortRoute), port, "abc-1", 0, e) }, time.Hour))
	deskExp := time.Now().Add(time.Hour).Unix()
	deskSig := signDesktop(g.k(purposeDesktopLink), "abc-1", 0, deskExp)
	deskTok := strconv.FormatInt(deskExp, 10) + "." + deskSig

	assertSingleStat := func(name string, fn func() *httptest.ResponseRecorder, wantCode int) {
		t.Helper()
		statCalls.Store(0)
		w := fn()
		if w.Code != wantCode {
			t.Fatalf("%s: status = %d, want %d (body=%q)", name, w.Code, wantCode, w.Body.String())
		}
		if got := statCalls.Load(); got != 1 {
			t.Fatalf("%s: os.Stat count = %d, want 1", name, got)
		}
	}

	assertSingleStat("public port", func() *httptest.ResponseRecorder {
		return req(g, pub, "/ping")
	}, http.StatusOK)
	assertSingleStat("private port", func() *httptest.ResponseRecorder {
		return req(g, priv, "/ping", header(routeHeader, goodTok))
	}, http.StatusOK)
	assertSingleStat("desktop shared redirect", func() *httptest.ResponseRecorder {
		return req(g, "desktop.boat.test", fmt.Sprintf("/abc-1?exp=%d&sig=%s", deskExp, deskSig))
	}, http.StatusFound)
	assertSingleStat("desktop proxy", func() *httptest.ResponseRecorder {
		return req(g, "abc-1.desktop.boat.test", "/vnc.html", cookie(desktopCookie, deskTok))
	}, http.StatusOK)

	// Atomic update to routes.json must be observed immediately on the very next single-stat request.
	tmp := rp + ".tmp"
	nextJSON := `{"vms":{"new-vm":{"ip":"127.0.0.1","ports":[` + p + `],"public":[` + p + `]}},"quarantined":["10.200.9.9"]}`
	if err := os.WriteFile(tmp, []byte(nextJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(tmp, future, future)
	if err := os.Rename(tmp, rp); err != nil {
		t.Fatal(err)
	}

	assertSingleStat("removed vm immediately 404", func() *httptest.ResponseRecorder {
		return req(g, pub, "/after")
	}, http.StatusNotFound)
	assertSingleStat("added vm immediately 200", func() *httptest.ResponseRecorder {
		return req(g, p+"-new-vm.vm.boatvms.test", "/after")
	}, http.StatusOK)
	assertSingleStat("quarantined peer 403", func() *httptest.ResponseRecorder {
		return req(g, p+"-new-vm.vm.boatvms.test", "/after", func(r *http.Request) {
			r.RemoteAddr = "10.200.9.9:44444"
		})
	}, http.StatusForbidden)
}

func TestPerVMTransportPoolingNoCrossTenantBlastRadius(t *testing.T) {
	startUpstream := func(tag string) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
		var newConns, closedConns atomic.Int64
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(tag + ":" + r.URL.Path))
		}))
		srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
			switch st {
			case http.StateNew:
				newConns.Add(1)
			case http.StateClosed:
				closedConns.Add(1)
			}
		}
		srv.Start()
		t.Cleanup(srv.Close)
		return srv, &newConns, &closedConns
	}

	upA, newA, closedA := startUpstream("A")
	upB, newB, closedB := startUpstream("B")
	addrA := upA.Listener.Addr().String()
	addrB := upB.Listener.Addr().String()

	const vmPort = 8080
	ps := strconv.Itoa(vmPort)
	rp := filepath.Join(t.TempDir(), "routes.json")
	writeAtomicRoutes := func(body string) {
		t.Helper()
		tmp := rp + ".tmp"
		if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		bump := time.Now().Add(time.Duration(newA.Load()+newB.Load()+closedA.Load()+closedB.Load()+1) * time.Second)
		_ = os.Chtimes(tmp, bump, bump)
		if err := os.Rename(tmp, rp); err != nil {
			t.Fatal(err)
		}
	}

	writeAtomicRoutes(`{"vms":{
		"vm-a":{"ip":"10.200.0.2","ports":[` + ps + `],"public":[` + ps + `]},
		"vm-b":{"ip":"10.200.0.3","ports":[` + ps + `],"public":[` + ps + `]}
	}}`)

	g := &gateway{
		domain: "boat.test", vmDomain: "vm.boatvms.test", desktopDomain: "desktop.boat.test",
		key: []byte(strings.Repeat("k", 32)), routes: &routes{path: rp}, now: time.Now,
		dialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			var d net.Dialer
			switch host {
			case "10.200.0.2":
				return d.DialContext(ctx, "tcp", addrA)
			case "10.200.0.3":
				return d.DialContext(ctx, "tcp", addrB)
			default:
				return nil, fmt.Errorf("unexpected backend dial %s", addr)
			}
		},
	}
	gwSrv := httptest.NewServer(g)
	t.Cleanup(gwSrv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	call := func(vmid, path string) (int, string) {
		t.Helper()
		httpReq, err := http.NewRequest("GET", gwSrv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		httpReq.Host = ps + "-" + vmid + ".vm.boatvms.test"
		resp, err := client.Do(httpReq)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	waitClosed := func(counter *atomic.Int64, want int64) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for counter.Load() < want && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := counter.Load(); got != want {
			t.Fatalf("closed connections = %d, want %d", got, want)
		}
	}

	// Warm pooled keep-alive connections to both VM A and VM B.
	for i := range 3 {
		if code, body := call("vm-a", fmt.Sprintf("/a-%d", i)); code != 200 || body != fmt.Sprintf("A:/a-%d", i) {
			t.Fatalf("vm-a warm %d: %d %q", i, code, body)
		}
		if code, body := call("vm-b", fmt.Sprintf("/b-%d", i)); code != 200 || body != fmt.Sprintf("B:/b-%d", i) {
			t.Fatalf("vm-b warm %d: %d %q", i, code, body)
		}
	}
	if newA.Load() != 1 || newB.Load() != 1 || closedA.Load() != 0 || closedB.Load() != 0 {
		t.Fatalf("initial pool state: newA=%d closedA=%d newB=%d closedB=%d", newA.Load(), closedA.Load(), newB.Load(), closedB.Load())
	}

	// 1. Quarantining VM B must close VM B's idle connection while keeping VM A's connection alive and reused.
	writeAtomicRoutes(`{"vms":{
		"vm-a":{"ip":"10.200.0.2","ports":[` + ps + `],"public":[` + ps + `]},
		"vm-b":{"ip":"10.200.0.3","ports":[` + ps + `],"public":[` + ps + `]}
	},"quarantined":["::ffff:10.200.0.3"]}`)
	if code, body := call("vm-a", "/after-b-quarantine"); code != 200 || body != "A:/after-b-quarantine" {
		t.Fatalf("vm-a after vm-b quarantine: %d %q", code, body)
	}
	waitClosed(closedB, 1)
	if newA.Load() != 1 || closedA.Load() != 0 {
		t.Fatalf("VM A keep-alive torn down by VM B quarantine: newA=%d closedA=%d", newA.Load(), closedA.Load())
	}

	// 2. Unquarantine VM B, warm a second pooled connection to VM B, then stop/remove VM B:
	// VM B's connection closes again, while VM A still reuses its original TCP connection (#1).
	writeAtomicRoutes(`{"vms":{
		"vm-a":{"ip":"10.200.0.2","ports":[` + ps + `],"public":[` + ps + `]},
		"vm-b":{"ip":"10.200.0.3","ports":[` + ps + `],"public":[` + ps + `]}
	}}`)
	if code, body := call("vm-b", "/b-rewarm"); code != 200 || body != "B:/b-rewarm" {
		t.Fatalf("vm-b rewarm: %d %q", code, body)
	}
	if newB.Load() != 2 {
		t.Fatalf("expected second TCP connection to vm-b after quarantine, got %d", newB.Load())
	}
	writeAtomicRoutes(`{"vms":{
		"vm-a":{"ip":"10.200.0.2","ports":[` + ps + `],"public":[` + ps + `]}
	}}`)
	if code, body := call("vm-a", "/after-b-stop"); code != 200 || body != "A:/after-b-stop" {
		t.Fatalf("vm-a after vm-b stop: %d %q", code, body)
	}
	waitClosed(closedB, 2)
	if newA.Load() != 1 || closedA.Load() != 0 {
		t.Fatalf("VM A keep-alive torn down by VM B stop: newA=%d closedA=%d", newA.Load(), closedA.Load())
	}

	// Direct unit check on routesPruned returned IPs.
	gotPruned := routesPruned(
		map[string]vm{"a": {IP: "10.200.0.2"}, "b": {IP: "10.200.0.3"}, "c": {IP: "10.200.0.4"}},
		map[string]vm{"a": {IP: "10.200.0.2"}, "c": {IP: "10.200.0.5"}},
		map[string]bool{"10.200.0.9": true},
	)
	wantPruned := []string{"10.200.0.3", "10.200.0.4", "10.200.0.9"}
	if !slices.Equal(gotPruned, wantPruned) {
		t.Fatalf("routesPruned = %v, want %v", gotPruned, wantPruned)
	}

	// 3. Removing routes.json altogether closes all remaining pooled transports.
	if err := os.Remove(rp); err != nil {
		t.Fatal(err)
	}
	if code, _ := call("vm-a", "/after-file-remove"); code != 404 {
		t.Fatalf("vm-a after routes.json removal = %d, want 404", code)
	}
	waitClosed(closedA, 1)
}

func makeSSHWireBlob(keyType string, payload []byte) string {
	buf := make([]byte, 4+len(keyType)+len(payload))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(keyType)))
	copy(buf[4:], keyType)
	copy(buf[4+len(keyType):], payload)
	return base64.StdEncoding.EncodeToString(buf)
}

func startRawLoopbackServer(t *testing.T, handler func(net.Conn, []byte)) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(time.Second))
				var reqBuf [65536]byte
				n, _ := conn.Read(reqBuf[:])
				handler(conn, reqBuf[:n])
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestJumpKeysSubcommand(t *testing.T) {
	edType := "ssh-ed25519"
	edBlob := makeSSHWireBlob(edType, append([]byte{0, 0, 0, 32}, bytes.Repeat([]byte("x"), 32)...))
	validLine := `restrict,port-forwarding,permitopen="10.200.0.2:22" ` + edType + " " + edBlob

	// 16384-bit RSA public key wire blob: [type="ssh-rsa"][e=65537][n=2049 bytes (leading 0x00 + 2048 bytes)].
	rsaType := "ssh-rsa"
	var rsaPayload bytes.Buffer
	_ = binary.Write(&rsaPayload, binary.BigEndian, uint32(3))
	rsaPayload.Write([]byte{0x01, 0x00, 0x01})
	_ = binary.Write(&rsaPayload, binary.BigEndian, uint32(2049))
	rsaPayload.WriteByte(0x00)
	rsaPayload.Write(bytes.Repeat([]byte{0xab}, 2048))
	rsa16kBlob := makeSSHWireBlob(rsaType, rsaPayload.Bytes())
	rsa16kLine := `restrict,port-forwarding,permitopen="10.200.0.2:22",permitopen="10.200.0.3:22" ` + rsaType + " " + rsa16kBlob

	// Exact 16384-byte wire key succeeds; 16385-byte wire key fails.
	maxWireBlob := makeSSHWireBlob(rsaType, bytes.Repeat([]byte{0x42}, jumpMaxKeyBytes-4-len(rsaType)))
	overMaxWireBlob := makeSSHWireBlob(rsaType, bytes.Repeat([]byte{0x42}, jumpMaxKeyBytes+1-4-len(rsaType)))

	// Standard HTTP server for request verification and key variants.
	var gotReq map[string]string
	var respBody []byte
	var respStatus int
	var forceChunked bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/ssh/authorize" ||
			r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Cache-Control") != "no-store" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotReq)
		w.WriteHeader(respStatus)
		if forceChunked {
			fl, _ := w.(http.Flusher)
			mid := len(respBody) / 2
			_, _ = w.Write(respBody[:mid])
			if fl != nil {
				fl.Flush()
			}
			_, _ = w.Write(respBody[mid:])
			if fl != nil {
				fl.Flush()
			}
			return
		}
		_, _ = w.Write(respBody)
	}))
	t.Cleanup(srv.Close)
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	authURL := "http://127.0.0.1:" + portStr + "/internal/ssh/authorize"

	// Proxy environment variables must be ignored.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")

	// Valid ed25519 (Content-Length framing).
	respStatus = http.StatusOK
	forceChunked = false
	respBody = []byte(`{"authorized_keys":[` + strconv.Quote(validLine) + `]}`)
	lines, err := runJumpKeys([]string{authURL, edType, edBlob, "jump"}, 2*time.Second)
	if err != nil || !slices.Equal(lines, []string{validLine}) {
		t.Fatalf("valid ed25519: lines=%v err=%v", lines, err)
	}
	if gotReq["user"] != "jump" || gotReq["key_type"] != edType || gotReq["key_blob"] != edBlob {
		t.Fatalf("unexpected POST body: %+v", gotReq)
	}

	// Valid 16384-bit RSA key over Transfer-Encoding: chunked framing.
	forceChunked = true
	respBody = []byte(`{"authorized_keys":[` + strconv.Quote(rsa16kLine) + `]}`)
	lines, err = runJumpKeys([]string{authURL, rsaType, rsa16kBlob, "jump"}, 2*time.Second)
	if err != nil || !slices.Equal(lines, []string{rsa16kLine}) {
		t.Fatalf("16384-bit RSA chunked: lines=%v err=%v", lines, err)
	}

	// Valid exact-16384-byte wire key, ecdsa, and sk-* key types.
	forceChunked = false
	for _, tc := range []struct {
		kt, kb string
	}{
		{rsaType, maxWireBlob},
		{"ecdsa-sha2-nistp256", makeSSHWireBlob("ecdsa-sha2-nistp256", bytes.Repeat([]byte{1}, 65))},
		{"sk-ssh-ed25519@openssh.com", makeSSHWireBlob("sk-ssh-ed25519@openssh.com", bytes.Repeat([]byte{2}, 40))},
	} {
		wantLine := `restrict,port-forwarding,permitopen="10.200.0.2:22" ` + tc.kt + " " + tc.kb
		respBody = []byte(`{"authorized_keys":[` + strconv.Quote(wantLine) + `]}`)
		lines, err = runJumpKeys([]string{authURL, tc.kt, tc.kb, "jump"}, 2*time.Second)
		if err != nil || !slices.Equal(lines, []string{wantLine}) {
			t.Fatalf("valid %s: lines=%v err=%v", tc.kt, lines, err)
		}
	}

	// Empty authorized_keys list is valid and returns zero lines.
	respBody = []byte(`{"authorized_keys":[]}`)
	if lines, err = runJumpKeys([]string{authURL, edType, edBlob, "jump"}, 2*time.Second); err != nil || len(lines) != 0 {
		t.Fatalf("empty authorized_keys: lines=%v err=%v", lines, err)
	}

	// Input validation failures.
	badInputs := [][]string{
		{},
		{authURL, edType, edBlob},
		{authURL, edType, edBlob, "jump", "extra"},
		{authURL, edType, edBlob, "root"},
		{"https://127.0.0.1:" + portStr + "/internal/ssh/authorize", edType, edBlob, "jump"},
		{"http://localhost:" + portStr + "/internal/ssh/authorize", edType, edBlob, "jump"},
		{"http://127.0.0.1:0/internal/ssh/authorize", edType, edBlob, "jump"},
		{"http://127.0.0.1:65536/internal/ssh/authorize", edType, edBlob, "jump"},
		{"http://127.0.0.1:" + portStr + "/v1/vms", edType, edBlob, "jump"},
		{authURL, "rsa-bad", edBlob, "jump"},
		{authURL, edType, edBlob + "\n", "jump"},
		{authURL, rsaType, strings.TrimRight(makeSSHWireBlob(rsaType, []byte{1, 2}), "="), "jump"},
		{authURL, edType, rsa16kBlob, "jump"},                   // wire key_type != arg keyType
		{authURL, edType, makeSSHWireBlob(edType, nil), "jump"}, // no key payload after prefix
		{authURL, rsaType, overMaxWireBlob, "jump"},             // > 16384 decoded bytes
	}
	for i, args := range badInputs {
		if _, err := runJumpKeys(args, 2*time.Second); err == nil {
			t.Errorf("bad input #%d (%v): expected error", i, args)
		}
	}

	// JSON shape, duplicate keys, line limit, and permitopen IP validation failures.
	var over200 []string
	for range 201 {
		over200 = append(over200, strconv.Quote(validLine))
	}
	badBodies := [][]byte{
		[]byte(`{}`),
		[]byte(`[]`),
		[]byte(`{"authorized_keys":null}`),
		[]byte(`{"authorized_keys":"` + validLine + `"}`),
		[]byte(`{"authorized_keys":[123]}`),
		[]byte(`{"authorized_keys":[],"authorized_keys":[]}`),
		[]byte(`{"authorized_keys":[` + strconv.Quote(validLine) + `],"authorized_keys":[` + strconv.Quote(validLine) + `]}`),
		[]byte(`{"authorized_keys":[` + strconv.Quote(validLine) + `],"extra":true}`),
		[]byte(`{"authorized_keys":[]} {}`),
		[]byte(`{"authorized_keys":[` + strings.Join(over200, ",") + `]}`),
		bytes.Repeat([]byte("x"), jumpMaxBodyBytes+1),
	}
	for _, badIP := range []string{
		"127.0.0.1", "127.0.0.2", "0.0.0.0", "0.1.2.3", "169.254.0.1", "169.254.255.254",
		"224.0.0.1", "239.255.255.250", "240.0.0.1", "255.255.255.255", "256.1.1.1",
		"010.200.0.2", "10.200.0.2:80", "::1", "localhost",
	} {
		line := strings.Replace(validLine, "10.200.0.2", badIP, 1)
		badBodies = append(badBodies, []byte(`{"authorized_keys":[`+strconv.Quote(line)+`]}`))
	}
	for _, badLine := range []string{
		`command="id",` + validLine,
		strings.Replace(validLine, ":22", ":2222", 1),
		`restrict,port-forwarding ` + edType + " " + edBlob,
		validLine + "\ncommand=\"id\"",
		`restrict,port-forwarding,permitopen="10.200.0.2:22" ` + edType + " " + rsa16kBlob,
	} {
		badBodies = append(badBodies, []byte(`{"authorized_keys":[`+strconv.Quote(badLine)+`]}`))
	}
	for i, b := range badBodies {
		respStatus = http.StatusOK
		respBody = b
		if _, err := runJumpKeys([]string{authURL, edType, edBlob, "jump"}, 2*time.Second); err == nil {
			t.Errorf("bad body #%d (%q): expected error", i, string(b[:min(len(b), 120)]))
		}
	}

	// Non-200 status codes and redirects must fail.
	respBody = []byte(`{"authorized_keys":[` + strconv.Quote(validLine) + `]}`)
	for _, code := range []int{http.StatusNoContent, http.StatusMovedPermanently, http.StatusFound, http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError} {
		respStatus = code
		if _, err := runJumpKeys([]string{authURL, edType, edBlob, "jump"}, 2*time.Second); err == nil {
			t.Errorf("status %d: expected error", code)
		}
	}

	// Raw HTTP framing smuggling & ambiguous header tests.
	goodJSON := `{"authorized_keys":[` + strconv.Quote(validLine) + `]}`
	rawCases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{
			name: "http10 eof valid",
			raw:  "HTTP/1.0 200 OK\r\nContent-Type: application/json\r\n\r\n" + goodJSON,
			ok:   true,
		},
		{
			name: "duplicate identical Content-Length",
			raw:  fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\nContent-Length: %d\r\n\r\n%s", len(goodJSON), len(goodJSON), goodJSON),
			ok:   false,
		},
		{
			name: "conflicting Content-Length and Transfer-Encoding",
			raw:  fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", len(goodJSON), len(goodJSON), goodJSON),
			ok:   false,
		},
		{
			name: "duplicate Transfer-Encoding",
			raw:  fmt.Sprintf("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", len(goodJSON), goodJSON),
			ok:   false,
		},
		{
			name: "non-digit Content-Length",
			raw:  fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: +%d\r\n\r\n%s", len(goodJSON), goodJSON),
			ok:   false,
		},
		{
			name: "truncated Content-Length",
			raw:  fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(goodJSON)+20, goodJSON),
			ok:   false,
		},
		{
			name: "redirect 302",
			raw:  "HTTP/1.1 302 Found\r\nLocation: " + authURL + "\r\nContent-Length: 0\r\n\r\n",
			ok:   false,
		},
	}
	for _, tc := range rawCases {
		rawPort := startRawLoopbackServer(t, func(c net.Conn, _ []byte) {
			_, _ = c.Write([]byte(tc.raw))
		})
		rawURL := fmt.Sprintf("http://127.0.0.1:%d/internal/ssh/authorize", rawPort)
		got, err := runJumpKeys([]string{rawURL, edType, edBlob, "jump"}, time.Second)
		if tc.ok && (err != nil || !slices.Equal(got, []string{validLine})) {
			t.Errorf("raw %s: got=%v err=%v", tc.name, got, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("raw %s: expected error, got %v", tc.name, got)
		}
	}

	// Hard deadline / slow-drip timeout enforcement.
	dripPort := startRawLoopbackServer(t, func(c net.Conn, _ []byte) {
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\n\r\n{\"authorized_keys\":"))
		time.Sleep(600 * time.Millisecond)
		_, _ = c.Write([]byte("[]}"))
	})
	t0 := time.Now()
	if _, err := runJumpKeys([]string{fmt.Sprintf("http://127.0.0.1:%d/internal/ssh/authorize", dripPort), edType, edBlob, "jump"}, 200*time.Millisecond); err == nil {
		t.Fatal("slow drip response should have timed out")
	}
	if elapsed := time.Since(t0); elapsed > time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

func TestJumpKeysCLIExitAndSilenceOnError(t *testing.T) {
	if os.Getenv("BOAT_TEST_JUMP_KEYS_CLI") == "1" {
		idx := slices.Index(os.Args, "--")
		os.Args = append([]string{"boat-gateway", "jump-keys"}, os.Args[idx+1:]...)
		main()
		os.Exit(0)
	}

	edType := "ssh-ed25519"
	edBlob := makeSSHWireBlob(edType, append([]byte{0, 0, 0, 32}, bytes.Repeat([]byte("z"), 32)...))
	validLine := `restrict,port-forwarding,permitopen="10.200.0.5:22" ` + edType + " " + edBlob

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"authorized_keys":[` + strconv.Quote(validLine) + `]}`))
	}))
	t.Cleanup(srv.Close)
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	authURL := "http://127.0.0.1:" + portStr + "/internal/ssh/authorize"

	runCLIEnv := func(extraEnv string, args ...string) (int, string, string) {
		t.Helper()
		cmdArgs := append([]string{"-test.run=^TestJumpKeysCLIExitAndSilenceOnError$", "--"}, args...)
		cmd := exec.Command(os.Args[0], cmdArgs...)
		cmd.Env = append(os.Environ(), "BOAT_TEST_JUMP_KEYS_CLI=1", extraEnv)
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		err := cmd.Run()
		code := 0
		if err != nil {
			var exitErr *exec.ExitError
			if !strings.Contains(err.Error(), "exit status") {
				t.Fatalf("exec error: %v", err)
			}
			if ok := execAsExitError(err, &exitErr); ok {
				code = exitErr.ExitCode()
			} else {
				code = 1
			}
		}
		return code, outBuf.String(), errBuf.String()
	}

	runCLI := func(args ...string) (int, string, string) {
		return runCLIEnv("BOAT_JUMP_ALLOW_UNTRACKED=1", args...)
	}

	// Fail closed: the parent is not an identifiable sshd, so no keys are printed.
	if code, stdout, _ := runCLIEnv("BOAT_JUMP_ALLOW_UNTRACKED=0", authURL, edType, edBlob, "jump"); code != 1 || stdout != "" {
		t.Fatalf("untracked parent must deny: code=%d stdout=%q", code, stdout)
	}

	code, stdout, stderr := runCLI(authURL, edType, edBlob, "jump")
	if code != 0 || stdout != validLine+"\n" || stderr != "" {
		t.Fatalf("valid CLI: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	code, stdout, stderr = runCLI("http://127.0.0.1:1/internal/ssh/authorize", edType, edBlob, "jump")
	if code != 1 || stdout != "" || stderr != "" {
		t.Fatalf("denied CLI must exit 1 with empty stdout/stderr: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func execAsExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// -legacy-hmac-until: the default (0) and -1 keep the window closed so a restart
// never re-arms master-key tokens; only an absolute time opens it.
func TestLegacyCutoffDefaultsClosed(t *testing.T) {
	for _, v := range []int64{0, -1, -100} {
		if got := legacyCutoff(v); !got.IsZero() {
			t.Errorf("legacyCutoff(%d) = %v, want zero (closed)", v, got)
		}
	}
	at := time.Now().Add(time.Hour).Unix()
	if got := legacyCutoff(at); got.Unix() != at {
		t.Errorf("legacyCutoff(%d) = %v", at, got)
	}
}

func TestRoutesThrottledStatAndConcurrentReads(t *testing.T) {
	g, rp, port := setup(t)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	var statCalls atomic.Int64
	g.routes = newRoutes(rp)
	if g.routes.minStatInterval != defaultRouteStatInterval {
		t.Fatalf("newRoutes minStatInterval = %v, want %v", g.routes.minStatInterval, defaultRouteStatInterval)
	}
	g.routes.now = func() time.Time { return time.Unix(0, clock.Load()) }
	g.routes.stat = func(p string) (os.FileInfo, error) {
		statCalls.Add(1)
		return os.Stat(p)
	}

	p := strconv.Itoa(port)
	pub := p + "-xyz-2.vm.boatvms.test"

	// 64 concurrent hot-path requests within the throttle window perform exactly 1 os.Stat total.
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := req(g, pub, "/hot"); w.Code != http.StatusOK {
				errs <- fmt.Sprintf("got %d, want 200", w.Code)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Fatal(msg)
	}
	if got := statCalls.Load(); got != 1 {
		t.Fatalf("concurrent hot-path os.Stat count = %d, want 1", got)
	}

	// Still inside the 50ms window (49ms elapsed): zero additional os.Stat calls.
	clock.Add(int64(defaultRouteStatInterval - time.Millisecond))
	if w := req(g, pub, "/still-hot"); w.Code != http.StatusOK {
		t.Fatalf("within window: got %d, want 200", w.Code)
	}
	if got := statCalls.Load(); got != 1 {
		t.Fatalf("within window os.Stat count = %d, want 1", got)
	}

	// Atomically update routes.json: once the interval elapses, exactly 1 os.Stat reloads it.
	tmp := rp + ".tmp"
	if err := os.WriteFile(tmp, []byte(`{"vms":{"fresh-1":{"ip":"127.0.0.1","ports":[`+p+`],"public":[`+p+`]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(tmp, future, future)
	if err := os.Rename(tmp, rp); err != nil {
		t.Fatal(err)
	}

	clock.Add(int64(2 * time.Millisecond))
	if w := req(g, pub, "/removed"); w.Code != http.StatusNotFound {
		t.Fatalf("removed route after interval: got %d, want 404", w.Code)
	}
	if got := statCalls.Load(); got != 2 {
		t.Fatalf("after interval os.Stat count = %d, want 2", got)
	}
	if w := req(g, p+"-fresh-1.vm.boatvms.test", "/added"); w.Code != http.StatusOK {
		t.Fatalf("added route after interval: got %d, want 200", w.Code)
	}
	if got := statCalls.Load(); got != 2 {
		t.Fatalf("second request in new window os.Stat count = %d, want 2", got)
	}

	// Forced refresh (used by revalidateLive) observes removals immediately even inside the 50ms window.
	if err := os.WriteFile(tmp, []byte(`{"vms":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	future2 := future.Add(2 * time.Second)
	_ = os.Chtimes(tmp, future2, future2)
	if err := os.Rename(tmp, rp); err != nil {
		t.Fatal(err)
	}
	snap := g.snapshotRoutesNow()
	if _, ok := snap.lookup("fresh-1"); ok {
		t.Fatal("snapshotRoutesNow served stale route inside throttle window")
	}
	if got := statCalls.Load(); got != 3 {
		t.Fatalf("after snapshotRoutesNow os.Stat count = %d, want 3", got)
	}
}

func assertSafeRelativeTarget(t *testing.T, label, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %q, want %q", label, got, want)
	}
	if !strings.HasPrefix(got, "/") {
		t.Errorf("%s: target %q must start with /", label, got)
	}
	if len(got) > 1 && (got[1] == '/' || got[1] == '\\') {
		t.Errorf("%s: target %q allows protocol-relative open redirect", label, got)
	}
}

func TestOpenRedirectNormalized(t *testing.T) {
	g, _, port := setup(t)
	p := strconv.Itoa(port)
	deskHost := "abc-1.desktop.boat.test"
	portHost := p + "-abc-1.vm.boatvms.test"
	exp := time.Now().Add(time.Hour).Unix()
	deskAuth := "exp=" + strconv.FormatInt(exp, 10) + "&sig=" + signDesktop(g.k(purposeDesktopLink), "abc-1", 0, exp)
	portAuth := "boat_exp=" + strconv.FormatInt(exp, 10) + "&boat_sig=" + signRoute(g.k(purposePortRoute), port, "abc-1", 0, exp)

	cases := []struct {
		name    string
		rawPath string // set directly on r.URL.Path when non-empty
		reqURI  string // used when rawPath == ""
		extraQ  string
		want    string
	}{
		{name: "double slash", reqURI: "//evil.example/landing", want: "/evil.example/landing"},
		{name: "triple slash", reqURI: "///evil.example/landing", want: "/evil.example/landing"},
		{name: "double slash with query", reqURI: "//evil.example/landing", extraQ: "a=1&", want: "/evil.example/landing?a=1"},
		{name: "slash backslash raw", rawPath: `/\evil.example/landing`, want: "/evil.example/landing"},
		{name: "slash backslash raw with query", rawPath: `/\evil.example/landing`, extraQ: "a=1&", want: "/evil.example/landing?a=1"},
		{name: "percent encoded backslash", reqURI: "/%5cevil.example/landing", want: "/evil.example/landing"},
		{name: "mixed slash backslash", rawPath: `/\/evil.example/landing`, want: "/evil.example/landing"},
		{name: "only double slash", reqURI: "//", want: "/"},
	}

	crossSiteMods := []func(*http.Request){
		header("Sec-Fetch-Site", "cross-site"),
		header("Sec-Fetch-Mode", "navigate"),
		header("Sec-Fetch-Dest", "document"),
	}

	for _, endpoint := range []struct {
		name       string
		host       string
		authQuery  string
		cookieName string
	}{
		{"desktop", deskHost, deskAuth, desktopCookie},
		{"private-port", portHost, portAuth, routeCookie},
	} {
		for _, tc := range cases {
			for _, crossSite := range []bool{false, true} {
				mode := "302"
				if crossSite {
					mode = "meta-refresh"
				}
				t.Run(endpoint.name+"/"+mode+"/"+tc.name, func(t *testing.T) {
					uri := tc.reqURI
					if uri == "" {
						uri = "/"
					}
					r := httptest.NewRequest("GET", "http://"+endpoint.host+uri+"?"+tc.extraQ+endpoint.authQuery, nil)
					if tc.rawPath != "" {
						r.URL.Path = tc.rawPath
					}
					if crossSite {
						for _, m := range crossSiteMods {
							m(r)
						}
					}
					w := httptest.NewRecorder()
					g.ServeHTTP(w, r)

					cookies := w.Result().Cookies()
					if len(cookies) != 1 || cookies[0].Name != endpoint.cookieName {
						t.Fatalf("expected session cookie %s, got %+v (status=%d body=%q)", endpoint.cookieName, cookies, w.Code, w.Body.String())
					}
					if !crossSite {
						if w.Code != http.StatusFound {
							t.Fatalf("302 status = %d, want 302 (body=%q)", w.Code, w.Body.String())
						}
						assertSafeRelativeTarget(t, "Location", w.Header().Get("Location"), tc.want)
					} else {
						if w.Code != http.StatusOK {
							t.Fatalf("meta-refresh status = %d, want 200 (body=%q)", w.Code, w.Body.String())
						}
						body := w.Body.String()
						wantMeta := `content="0;url=` + tc.want + `"`
						wantHref := `href="` + tc.want + `"`
						if !strings.Contains(body, wantMeta) || !strings.Contains(body, wantHref) {
							t.Fatalf("meta-refresh body = %q, want %q and %q", body, wantMeta, wantHref)
						}
						if strings.Contains(body, "url=//") || strings.Contains(body, `url=/\`) ||
							strings.Contains(body, `href="//`) || strings.Contains(body, `href="/\`) {
							t.Fatalf("meta-refresh body still contains open redirect: %q", body)
						}
					}
				})
			}
		}
	}

	// Defense-in-depth: direct setSession calls also normalize unsafe targets on both paths.
	for _, tc := range []struct{ in, want string }{
		{"//evil.example/landing", "/evil.example/landing"},
		{`/\evil.example/landing`, "/evil.example/landing"},
		{"/\t/evil.example/landing?x=1", "/evil.example/landing?x=1"},
		{"", "/"},
		{"//", "/"},
	} {
		for _, crossSite := range []bool{false, true} {
			r := httptest.NewRequest("GET", "http://"+deskHost+"/", nil)
			if crossSite {
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			}
			w := httptest.NewRecorder()
			setSession(w, r, desktopCookie, "v", time.Now().Add(time.Hour), http.SameSiteStrictMode, tc.in)
			if !crossSite {
				assertSafeRelativeTarget(t, "direct 302 "+tc.in, w.Header().Get("Location"), tc.want)
			} else {
				body := w.Body.String()
				if !strings.Contains(body, `content="0;url=`+tc.want+`"`) || !strings.Contains(body, `href="`+tc.want+`"`) {
					t.Errorf("direct meta-refresh %q: body=%q want target %q", tc.in, body, tc.want)
				}
			}
		}
	}
}

func TestHasDesktopSingleflightAndCooldown(t *testing.T) {
	t.Run("concurrent unknown desktop asks coalesce to one upstream call and cool down", func(t *testing.T) {
		g, _, _ := setup(t)
		var calls atomic.Int32
		var desktopsDoc atomic.Value
		desktopsDoc.Store(`{"gens":{},"desktops":["abc-1"]}`)
		firstEntered := make(chan struct{})
		releaseFirst := make(chan struct{})

		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			if n == 1 {
				close(firstEntered)
				<-releaseFirst
			}
			_, _ = w.Write([]byte(desktopsDoc.Load().(string)))
		}))
		t.Cleanup(api.Close)

		var clock atomic.Int64
		clock.Store(time.Now().UnixNano())
		g.gens = newRouteGens(api.URL)
		g.gens.now = func() time.Time { return time.Unix(0, clock.Load()) }
		g.now = g.gens.now

		const n = 32
		var wg sync.WaitGroup
		codes := make([]int, n)
		for i := range n {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				w := httptest.NewRecorder()
				g.ask(w, httptest.NewRequest("GET", "/ask?domain=xyz-2.desktop.boat.test", nil))
				codes[idx] = w.Code
			}(i)
		}
		<-firstEntered
		// Give sibling goroutines a moment to pile onto the singleflight call.
		time.Sleep(20 * time.Millisecond)
		close(releaseFirst)
		wg.Wait()

		for i, c := range codes {
			if c != http.StatusNotFound {
				t.Fatalf("goroutine %d got status %d, want 404", i, c)
			}
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("upstream /internal/route-gens calls for %d concurrent unknown asks = %d, want 1", n, got)
		}

		// Subsequent probes inside the 2s cooldown do not hit boat-api and stay rejected.
		clock.Add(int64(time.Second))
		for range 5 {
			w := httptest.NewRecorder()
			g.ask(w, httptest.NewRequest("GET", "/ask?domain=xyz-2.desktop.boat.test", nil))
			if w.Code != http.StatusNotFound {
				t.Fatalf("during cooldown got %d, want 404", w.Code)
			}
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("upstream calls during cooldown = %d, want 1", got)
		}

		// Once the 2s cooldown elapses, a newly minted desktop is discovered in one refresh.
		desktopsDoc.Store(`{"gens":{},"desktops":["abc-1","xyz-2"]}`)
		clock.Add(int64(desktopRefreshMinInterval))
		w := httptest.NewRecorder()
		g.ask(w, httptest.NewRequest("GET", "/ask?domain=xyz-2.desktop.boat.test", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("after cooldown got %d, want 200", w.Code)
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("upstream calls after cooldown = %d, want 2", got)
		}

		// Once known, further asks hit the in-memory table with zero upstream calls.
		w = httptest.NewRecorder()
		g.ask(w, httptest.NewRequest("GET", "/ask?domain=xyz-2.desktop.boat.test", nil))
		if w.Code != http.StatusOK || calls.Load() != 2 {
			t.Fatalf("cached known desktop: code=%d calls=%d, want 200 and 2", w.Code, calls.Load())
		}
	})

	t.Run("first-time ask still refreshes even after recent poll", func(t *testing.T) {
		g, _, _ := setup(t)
		var calls atomic.Int32
		var desktopsDoc atomic.Value
		desktopsDoc.Store(`{"gens":{},"desktops":["abc-1"]}`)

		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = w.Write([]byte(desktopsDoc.Load().(string)))
		}))
		t.Cleanup(api.Close)

		var clock atomic.Int64
		clock.Store(time.Now().UnixNano())
		g.gens = newRouteGens(api.URL)
		g.gens.now = func() time.Time { return time.Unix(0, clock.Load()) }
		g.now = g.gens.now

		// Background poll ran 100ms ago before xyz-2's desktop link was minted.
		if err := g.gens.refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		desktopsDoc.Store(`{"gens":{},"desktops":["abc-1","xyz-2"]}`)
		clock.Add(int64(100 * time.Millisecond))

		w := httptest.NewRecorder()
		g.ask(w, httptest.NewRequest("GET", "/ask?domain=xyz-2.desktop.boat.test", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("first-time ask after fresh poll got %d, want 200", w.Code)
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("calls = %d, want 2 (1 poll + 1 first-time ask)", got)
		}
	})
}
