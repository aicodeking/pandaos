package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Cookies are host-only (no Domain) with the __Host- prefix, which browsers
// accept only with Secure, Path=/ and no Domain: a page on a sibling origin
// (another tenant's VM) can neither read nor overwrite them.
const (
	desktopCookie = "__Host-boat_gw"    // <vmid>.<desktopDomain>, SameSite=Strict
	routeCookie   = "__Host-boat_route" // <port>-<vmid>.<vmDomain>, SameSite=Lax
	routeHeader   = "X-Boat-Route-Token"
	guestCookie   = "boat_desktop" // the guest agent's desktop proxy (:6080)
)

// Per-purpose keys: boat-api's PurposeKey. The shared key file is a master
// key; each kind of token is signed with HMAC-SHA256(master,
// "boat-hmac-v2/<purpose>"), so a desktop link, a port route token and the
// guest desktop token can never stand in for each other, whatever their
// payloads. Before this split all three were signed with the master key and
// told apart by payload prefixes only; such legacy tokens still verify until
// legacyUntil (they all carry an expiry), never after.
const (
	purposeDesktopLink  = "desktop-link"
	purposePortRoute    = "port-route"
	purposeGuestDesktop = "guest-desktop"
)

func purposeKey(master []byte, purpose string) []byte {
	m := hmac.New(sha256.New, master)
	m.Write([]byte("boat-hmac-v2/" + purpose))
	return m.Sum(nil)
}

// legacyWindow: how long after start a gateway accepts tokens signed with
// the master key: the longest token boat-api mints (a 7-day port URL) plus a day.
const legacyWindow = 8 * 24 * time.Hour

func mac(key []byte, payload string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// signDesktop is boat-api's SignDesktop: hex(HMAC(key, "<vmid>.<exp>")).
func signDesktop(key []byte, vmid string, exp int64) string {
	return mac(key, fmt.Sprintf("%s.%d", vmid, exp))
}

// signRoute: hex(HMAC(key, "route.<port>-<vmid>.<exp>")) (boat-api mints it),
// with ".g<gen>" before the expiry once the VM's tokens have been revoked.
func signRoute(key []byte, port int, vmid string, gen int, exp int64) string {
	if gen == 0 {
		return mac(key, fmt.Sprintf("route.%d-%s.%d", port, vmid, exp))
	}
	return mac(key, fmt.Sprintf("route.%d-%s.g%d.%d", port, vmid, gen, exp))
}

// guestToken is the token boat-api starts the guest desktop with:
// hex(HMAC(key, "guest-desktop.<vmid>")); no per-VM state here.
func guestToken(key []byte, vmid string) string {
	return mac(key, "guest-desktop."+vmid)
}

// checkToken verifies "<exp>.<sig>" against sign(exp); returns the expiry.
func checkToken(token string, now time.Time, sign func(exp int64) string) (time.Time, error) {
	expStr, sig, ok := strings.Cut(token, ".")
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if !ok || err != nil {
		return time.Time{}, errors.New("malformed token")
	}
	if !hmac.Equal([]byte(sign(exp)), []byte(strings.ToLower(sig))) {
		return time.Time{}, errors.New("bad signature")
	}
	t := time.Unix(exp, 0)
	if !now.Before(t) {
		return time.Time{}, errors.New("token expired")
	}
	return t, nil
}

// verify checks a "<exp>.<sig>" token of one purpose: signed with that
// purpose's key, or, until legacyUntil, a legacy token signed with the master
// key whose expiry is no later than legacyUntil. sign is the payload signer.
func (g *gateway) verify(token, purpose string, sign func(key []byte, exp int64) string) (time.Time, error) {
	exp, err := checkToken(token, g.now(), func(e int64) string { return sign(g.k(purpose), e) })
	if err == nil || g.legacyUntil.IsZero() || !g.now().Before(g.legacyUntil) || err.Error() != "bad signature" {
		return exp, err
	}
	exp, lerr := checkToken(token, g.now(), func(e int64) string { return sign(g.key, e) })
	if lerr != nil || exp.After(g.legacyUntil) {
		return time.Time{}, err
	}
	return exp, nil
}

func (g *gateway) k(purpose string) []byte { return purposeKey(g.key, purpose) }

// guestCookies is the Cookie pair the guest desktop proxy checks: the token
// boat-api starts it with now, and during the legacy window also the master-key
// token a guest started before the split still holds (it takes any match).
func (g *gateway) guestCookies(vmid string) string {
	c := guestCookie + "=" + guestToken(g.k(purposeGuestDesktop), vmid)
	if !g.legacyUntil.IsZero() && g.now().Before(g.legacyUntil) {
		c += "; " + guestCookie + "=" + guestToken(g.key, vmid)
	}
	return c
}

// anyCookie reports whether one of the request's cookies called name passes
// check. A browser can send several with one name; try each.
func anyCookie(r *http.Request, name string, check func(string) error) error {
	err := errors.New("no session cookie")
	for _, c := range r.Cookies() {
		if c.Name == name {
			if err = check(c.Value); err == nil {
				return nil
			}
		}
	}
	return err
}

// setSession sets a host-only cookie and moves on to target. Normally a 302.
// But a navigation that began on another origin keeps that context across its
// redirects: a Strict cookie would be withheld (cross-site), and checkOrigin
// would refuse it (same-site). So there a same-origin page starts a fresh
// navigation.
func setSession(w http.ResponseWriter, r *http.Request, name, value string, exp time.Time, same http.SameSite, target string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", Expires: exp,
		HttpOnly: true, Secure: true, SameSite: same,
	})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if site := r.Header.Get("Sec-Fetch-Site"); site == "" || site == "none" || site == "same-origin" {
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t := html.EscapeString(target)
	fmt.Fprintf(w, `<!doctype html><meta http-equiv="refresh" content="0;url=%s"><a href="%s">Continue</a>`+"\n", t, t)
}

// originHost parses an Origin header into scheme and host:port with the
// scheme's default port removed. "null" and garbage give ok=false.
func originHost(origin string) (scheme, host string, ok bool) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", false
	}
	return u.Scheme, canonHost(u.Scheme, u.Host), true
}

func canonHost(scheme, host string) string {
	host = strings.ToLower(host)
	if h, p, err := net.SplitHostPort(host); err == nil &&
		((scheme == "https" && p == "443") || (scheme == "http" && p == "80")) {
		return h
	}
	return host
}

// checkOrigin stops another origin from using this browser's session cookie.
// Every VM origin shares a registrable domain with its siblings, so SameSite
// does not help: a page on VM A can open wss://<B>... with B's cookie attached.
// So: an Origin header must be this request's own origin, or one allowed by
// sameTenant. Without Origin (no-cors GETs and navigations) Sec-Fetch-Site says
// where it came from; only top-level navigations may come from elsewhere, and
// the session's own checks still apply to them.
func checkOrigin(r *http.Request, sameTenant func(host string) bool) error {
	if o := r.Header.Get("Origin"); o != "" {
		s, h, ok := originHost(o)
		switch {
		case !ok:
			return errors.New("bad Origin")
		case s == scheme(r) && h == canonHost(s, r.Host):
			return nil
		case sameTenant != nil && sameTenant(hostOnly(h)):
			return nil
		}
		return errors.New("request from another origin")
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-site", "cross-site":
		if r.Header.Get("Sec-Fetch-Mode") != "navigate" || r.Header.Get("Sec-Fetch-Dest") != "document" {
			return errors.New("embedded request from another origin")
		}
	}
	return nil
}

// loadKey mirrors boat-api's LoadMasterKey: 32 raw bytes or 64 hex chars.
func loadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) == 32 {
		return b, nil
	}
	k, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(k) != 32 {
		return nil, fmt.Errorf("%s: want 32 raw bytes or 64 hex chars", path)
	}
	return k, nil
}
