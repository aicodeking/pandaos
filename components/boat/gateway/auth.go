package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net/http"
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

func mac(key []byte, payload string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// signDesktop is boat-api's SignDesktop: hex(HMAC(key, "<vmid>.<exp>")).
func signDesktop(key []byte, vmid string, exp int64) string {
	return mac(key, fmt.Sprintf("%s.%d", vmid, exp))
}

// signRoute: hex(HMAC(key, "route.<port>-<vmid>.<exp>")) (boat-api mints it).
func signRoute(key []byte, port int, vmid string, exp int64) string {
	return mac(key, fmt.Sprintf("route.%d-%s.%d", port, vmid, exp))
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
// But a SameSite=Strict cookie set during a navigation that started on another
// site is withheld on its redirects, so in that one case (the browser says
// Sec-Fetch-Site: cross-site) a same-origin page starts a fresh navigation.
func setSession(w http.ResponseWriter, r *http.Request, name, value string, exp time.Time, same http.SameSite, target string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", Expires: exp,
		HttpOnly: true, Secure: true, SameSite: same,
	})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Header.Get("Sec-Fetch-Site") != "cross-site" {
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t := html.EscapeString(target)
	fmt.Fprintf(w, `<!doctype html><meta http-equiv="refresh" content="0;url=%s"><a href="%s">Continue</a>`+"\n", t, t)
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
