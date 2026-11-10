package main

import (
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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
	good, exp := tok(func(e int64) string { return signRoute(g.k(purposePortRoute), port, "abc-1", 0, e) }, time.Hour)
	w := req(g, priv, "/x", header(routeHeader, good), cookie("app", "1"))
	if b := w.Body.String(); w.Code != 200 || strings.Contains(b, good) || !strings.Contains(b, "cookie=app=1") {
		t.Fatalf("header token: %d %s", w.Code, b)
	}
	bad := map[string]string{
		"expired":    first(tok(func(e int64) string { return signRoute(g.k(purposePortRoute), port, "abc-1", 0, e) }, -time.Second)),
		"other vm":   first(tok(func(e int64) string { return signRoute(g.k(purposePortRoute), port, "xyz-2", 0, e) }, time.Hour)),
		"other port": first(tok(func(e int64) string { return signRoute(g.k(purposePortRoute), port+1, "abc-1", 0, e) }, time.Hour)),
		"desktop":    first(tok(func(e int64) string { return signDesktop(g.k(purposeDesktopLink), "abc-1", e) }, time.Hour)),
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
		return "exp=" + strconv.FormatInt(exp, 10) + "&sig=" + signDesktop(g.k(purposeDesktopLink), vmid, exp)
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
	cB := cookie(desktopCookie, first(tok(func(e int64) string { return signDesktop(g.k(purposeDesktopLink), "xyz-2", e) }, time.Hour)))
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
	q := "/?exp=" + strconv.FormatInt(exp, 10) + "&sig=" + signDesktop(g.k(purposeDesktopLink), "xyz-2", exp)
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
		"own":         {[]func(*http.Request){cR, https, header("Origin", "https://"+priv)}, 200},
		"same VM":     {[]func(*http.Request){cR, https, header("Origin", "https://3000-abc-1.vm.boatvms.test")}, 200},
		"other VM":    {[]func(*http.Request){cR, https, ws, header("Origin", "https://3000-xyz-2.vm.boatvms.test")}, 403},
		"no-cors":     {[]func(*http.Request){cR, https, header("Sec-Fetch-Site", "same-site"), header("Sec-Fetch-Mode", "no-cors")}, 403},
		"navigation":  {[]func(*http.Request){cR, https, header("Sec-Fetch-Site", "same-site"), header("Sec-Fetch-Mode", "navigate"), header("Sec-Fetch-Dest", "document")}, 200},
		"header only": {[]func(*http.Request){header(routeHeader, rt), https, header("Origin", "https://app.example")}, 200},
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
	status = 404
	if c := req(g, host, "/", withTok).Code; c != 200 {
		t.Errorf("API without revocation: %d", c)
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
