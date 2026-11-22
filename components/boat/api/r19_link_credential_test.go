package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func r19Mint(t *testing.T, e *env, token string, p *principal, vm, kind, entry string) (int, map[string]any) {
	t.Helper()
	if entry == "headerless" {
		out := r17Mint(e, p, vm, kind)
		var body map[string]any
		if err := json.Unmarshal(out.body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return out.status, body
	}
	path := vmPath(vm, "ports/8080/url")
	if kind == "desktop" {
		path = vmPath(vm, "desktop")
	}
	return e.do(token, "GET", path, nil)
}

func r19CheckLink(t *testing.T, e *env, vm, kind string, out map[string]any, wantExp int64) {
	t.Helper()
	raw, ok := out["url"].(string)
	if !ok {
		t.Fatalf("missing link: %v", out)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	expStr, sig := u.Query().Get("boat_exp"), u.Query().Get("boat_sig")
	if kind == "desktop" {
		expStr, sig = u.Query().Get("exp"), u.Query().Get("sig")
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	gen := e.srv.store.Snapshot().RouteGens[vm]
	want := SignRoute(PurposeKey(e.srv.cfg.DesktopKey, purposePortRoute), "8080-"+vm, gen, exp)
	if kind == "desktop" {
		want = SignDesktop(PurposeKey(e.srv.cfg.DesktopKey, purposeDesktopLink), vm, gen, exp)
	}
	if sig != want || exp <= time.Now().Unix() || (wantExp != 0 && exp != wantExp) {
		t.Errorf("wrong signature, generation or expiry: %s (want expiry %d)", raw, wantExp)
	}
}

func TestR19LinkMintRechecksCredentialAfterBoatd(t *testing.T) {
	for _, kind := range []string{"port", "desktop"} {
		for _, entry := range []string{"http", "headerless"} {
			for _, credential := range []string{"zero-overlap", "current", "becomes-previous", "previous", "expired-previous", "replaced-previous"} {
				t.Run(kind+"/"+entry+"/"+credential, func(t *testing.T) {
					e, vm := r17MintSetup(t, kind)
					token := e.newKey("exec")
					id, _, _ := ParseToken(token)
					rotate := func(overlap int) string {
						status, out := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": overlap})
						if status != 200 {
							t.Fatalf("rotate: %d %v", status, out)
						}
						return out["token"].(string)
					}
					switch credential {
					case "current":
						token = rotate(0)
					case "previous", "expired-previous", "replaced-previous":
						rotate(60)
					}
					p := e.srv.authenticate("Bearer " + token)
					if p == nil {
						t.Fatal("credential did not authenticate before boatd")
					}
					rejected := credential == "zero-overlap" || credential == "expired-previous" || credential == "replaced-previous"
					var paused atomic.Bool
					var before atomic.Pointer[State]
					base := e.srv.boatd.hc.Transport
					if base == nil {
						base = http.DefaultTransport
					}
					e.srv.boatd.hc = &http.Client{Transport: r17MintTransport(func(r *http.Request) (*http.Response, error) {
						resp, err := base.RoundTrip(r)
						match := r.Method == "GET" && r.URL.Path == vmPath(vm)
						if kind == "desktop" {
							match = r.Method == "POST" && r.URL.Path == vmPath(vm, "desktop")
						}
						if err == nil && match && paused.CompareAndSwap(false, true) {
							switch credential {
							case "zero-overlap":
								rotate(0)
							case "becomes-previous", "replaced-previous":
								rotate(60)
							}
							if err := e.srv.store.Update(func(st *State) error {
								if credential == "expired-previous" {
									past := time.Now().Add(-time.Minute)
									st.Keys[id].PrevUntil = &past
								}
								st.RouteGens[vm] = 7
								return nil
							}); err != nil {
								t.Fatal(err)
							}
							if rejected {
								if status, out := e.do(token, "GET", "/v1/me", nil); status != 401 {
									t.Fatalf("old secret still authenticates: %d %v", status, out)
								}
							}
							before.Store(e.srv.store.Snapshot())
						}
						return resp, err
					})}
					status, out := r19Mint(t, e, token, p, vm, kind, entry)
					if !paused.Load() {
						t.Fatal("request did not pause before minter registration")
					}
					st := e.srv.store.Snapshot()
					if rejected {
						if status != 403 || out["url"] != nil || out["token"] != nil {
							t.Errorf("revoked secret minted a link: %d %v", status, out)
						}
						if st != before.Load() {
							t.Error("rejected credential published minter/desktop state")
						}
						return
					}
					if status != 200 {
						t.Fatalf("valid credential rejected: %d %v", status, out)
					}
					r19CheckLink(t, e, vm, kind, out, 0)
					if !slices.Equal(st.PortMinters[vm], []string{id}) || st.DesktopVMs[vm] != (kind == "desktop") {
						t.Error("valid credential missing minter/desktop registration")
					}
				})
			}
		}
	}
}

func TestR19LinkMintPreservesAuthorityBounds(t *testing.T) {
	for _, kind := range []string{"port", "desktop"} {
		for _, change := range []string{"original-expiry", "current-expiry", "expired-key", "reduced-scope", "original-scope"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				e, vm := r17MintSetup(t, kind)
				scope := "exec"
				if change == "original-scope" {
					scope = "vms:read"
				}
				token := e.newKey(scope)
				id, _, _ := ParseToken(token)
				expires := time.Now().Add(30 * time.Second)
				if err := e.srv.store.Update(func(st *State) error {
					st.Keys[id].Expires = &expires
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				p := e.srv.authenticate("Bearer " + token)
				if p == nil {
					t.Fatal("credential did not authenticate")
				}
				wantExp := expires.Unix()
				if err := e.srv.store.Update(func(st *State) error {
					switch change {
					case "original-expiry":
						st.Keys[id].Expires = nil
					case "current-expiry":
						shorter := expires.Add(-10 * time.Second)
						st.Keys[id].Expires = &shorter
						wantExp = shorter.Unix()
					case "expired-key":
						past := time.Now().Add(-time.Minute)
						st.Keys[id].Expires = &past
					case "reduced-scope":
						st.Keys[id].Scopes = []string{"vms:read"}
					case "original-scope":
						st.Keys[id].Scopes = []string{"exec"}
					}
					st.RouteGens[vm] = 7
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				before := e.srv.store.Snapshot()
				status, out := r19Mint(t, e, token, p, vm, kind, "headerless")
				if change == "expired-key" || change == "reduced-scope" || change == "original-scope" {
					if status != 403 || e.srv.store.Snapshot() != before {
						t.Errorf("stale authority minted/published a link: %d %v", status, out)
					}
					return
				}
				if status != 200 {
					t.Fatalf("bounded credential rejected: %d %v", status, out)
				}
				r19CheckLink(t, e, vm, kind, out, wantExp)
			})
		}
	}
}
