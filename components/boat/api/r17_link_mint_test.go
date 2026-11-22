package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

type r17MintTransport func(*http.Request) (*http.Response, error)

func (f r17MintTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func r17MintSetup(t *testing.T, kind string) (*env, string) {
	t.Helper()
	e := setup(t)
	e.srv.cfg.VMDomain, e.srv.cfg.VMAPIURL = "vm.example", "https://boat.example"
	e.srv.cfg.PortURLTTL = time.Hour
	vm := e.createVM()
	if kind == "port" {
		if status, out := e.do(e.owner, "POST", vmPath(vm, "ports"), map[string]any{"port": 8080}); status != 201 {
			t.Fatalf("expose port: %d %v", status, out)
		}
	}
	return e, vm
}

// Compat calls carry the authenticated principal, without an Authorization header.
func r17Mint(e *env, p *principal, vm, kind string) *nativeAnswer {
	r := httptest.NewRequest("GET", "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, &reqInfo{id: "r17-mint"}))
	if kind == "desktop" {
		return e.srv.callNative(r, p, "GET /v1/vms/{id}/desktop", e.srv.desktopVM, vm, nil)
	}
	return e.srv.callNative(r, p, "GET /v1/vms/{id}/ports/{port}/url", e.srv.portURL, vm, nil, "port", "8080")
}

func r17MintRevoker(t *testing.T, e *env, vm, how string) (*principal, func()) {
	t.Helper()
	token, uid := e.selfTokenOf(vm), ""
	if how == "delete-key" || how == "delete-member" {
		status, out := e.do(e.owner, "POST", "/v1/teams/"+e.teamID()+"/members", map[string]any{"name": "bob", "role": "member"})
		if status != 201 {
			t.Fatalf("member: %d %v", status, out)
		}
		token, uid = out["key"].(map[string]any)["token"].(string), out["user"].(map[string]any)["id"].(string)
	}
	p := e.srv.authenticate("Bearer " + token)
	if p == nil {
		t.Fatal("principal was not authenticated")
	}
	return p, func() {
		t.Helper()
		switch how {
		case "self-rotation":
			if _, err := e.srv.bumpSelf(vm, p.Team.ID); err != nil {
				t.Fatal(err)
			}
		case "self-copy":
			if status, out := e.do(e.owner, "POST", vmPath(vm, "snapshots"), nil); status != 201 {
				t.Fatalf("copy: %d %v", status, out)
			}
		case "delete-key":
			if status, out := e.do(e.owner, "DELETE", "/v1/keys/"+p.Key.ID, nil); status != 200 {
				t.Fatalf("delete key: %d %v", status, out)
			}
		case "delete-member":
			if status, out := e.do(e.owner, "DELETE", "/v1/teams/"+p.Team.ID+"/members/"+uid, nil); status != 200 {
				t.Fatalf("delete member: %d %v", status, out)
			}
		}
	}
}

func TestR17LinkMintRejectsRevocationBeforeRegistration(t *testing.T) {
	for _, kind := range []string{"port", "desktop"} {
		for _, existing := range []bool{false, true} {
			for _, how := range []string{"self-rotation", "self-copy", "delete-key", "delete-member"} {
				name := kind + "/" + how + "/new-minter"
				if existing {
					name = kind + "/" + how + "/existing-minter"
				}
				t.Run(name, func(t *testing.T) {
					e, vm := r17MintSetup(t, kind)
					p, revoke := r17MintRevoker(t, e, vm, how)
					if existing {
						if out := r17Mint(e, p, vm, kind); out.status != 200 {
							t.Fatalf("initial mint: %d %s", out.status, out.body.String())
						}
					}
					// Pause after ownership/guest work, immediately before registration.
					var paused atomic.Bool
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
							revoke()
						}
						return resp, err
					})}
					out := r17Mint(e, p, vm, kind)
					if !paused.Load() {
						t.Fatal("mint did not reach pause")
					}
					if out.status != http.StatusForbidden {
						t.Errorf("revoked principal minted a link: %d %s", out.status, out.body.String())
					}
					if slices.Contains(e.srv.store.Snapshot().PortMinters[vm], p.Key.ID) {
						t.Error("revoked principal was registered again")
					}
					if !existing && e.srv.store.Snapshot().DesktopVMs[vm] {
						t.Error("rejected mint approved desktop routing")
					}
				})
			}
		}
	}
}

func TestR17LinkMintCapturesGenerationBeforeRevocation(t *testing.T) {
	for _, kind := range []string{"port", "desktop"} {
		for _, how := range []string{"self-rotation", "self-copy", "delete-key", "delete-member"} {
			t.Run(kind+"/"+how, func(t *testing.T) {
				e, vm := r17MintSetup(t, kind)
				p, revoke := r17MintRevoker(t, e, vm, how)
				gen, exp, err := e.srv.recordPortMinter(vm, p, kind == "desktop", time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				// Pause after durable registration, before signing/returning the link.
				back, err := OpenStore(e.dir)
				if err != nil {
					t.Fatal(err)
				}
				persisted := back.Snapshot()
				back.Close()
				if !slices.Contains(persisted.PortMinters[vm], p.Key.ID) || persisted.RouteGens[vm] != gen ||
					persisted.DesktopVMs[vm] != (kind == "desktop") {
					t.Fatal("mint was not atomically durable before signing")
				}
				revoke()
				current := e.srv.store.Snapshot().RouteGens[vm]
				if current <= gen {
					t.Fatal("revocation missed the registered minter")
				}
				if kind == "desktop" {
					key := PurposeKey(e.srv.cfg.DesktopKey, purposeDesktopLink)
					if SignDesktop(key, vm, gen, exp) == SignDesktop(key, vm, current, exp) {
						t.Error("captured desktop generation survived revocation")
					}
				} else {
					key := PurposeKey(e.srv.cfg.DesktopKey, purposePortRoute)
					if SignRoute(key, "8080-"+vm, gen, exp) == SignRoute(key, "8080-"+vm, current, exp) {
						t.Error("captured port generation survived revocation")
					}
				}
				if _, _, err := e.srv.recordPortMinter(vm, p, kind == "desktop", time.Hour); err != errLinkUnauthorized {
					t.Errorf("revoked principal recaptured current generation: %v", err)
				}
			})
		}
	}
}

func TestR17LinkMintRechecksAuthorization(t *testing.T) {
	for _, kind := range []string{"port", "desktop"} {
		for _, how := range []string{"user", "team", "member", "key-user", "key-team", "scope", "expiry", "target-retired", "self-epoch"} {
			t.Run(kind+"/"+how, func(t *testing.T) {
				e, vm := r17MintSetup(t, kind)
				token := e.newKey("exec")
				if how == "self-epoch" {
					token = e.selfTokenOf(vm)
				}
				p := e.srv.authenticate("Bearer " + token)
				if err := e.srv.store.Update(func(st *State) error {
					switch how {
					case "user":
						delete(st.Users, p.User.ID)
					case "team":
						delete(st.Teams, p.Team.ID)
					case "member":
						st.Teams[p.Team.ID].Members = nil
					case "key-user":
						st.Keys[p.Key.ID].UserID = "another-user"
					case "key-team":
						st.Keys[p.Key.ID].TeamID = "another-team"
					case "scope":
						st.Keys[p.Key.ID].Scopes = []string{"vms:read"}
					case "expiry":
						past := time.Now().Add(-time.Minute)
						st.Keys[p.Key.ID].Expires = &past
					case "target-retired":
						retireCredentials(st, vm)
					case "self-epoch":
						st.CopyCredentialsV1 = !st.CopyCredentialsV1
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				before := e.srv.store.Snapshot()
				if out := r17Mint(e, p, vm, kind); out.status != 403 {
					t.Errorf("stale %s authorization: %d %s", how, out.status, out.body.String())
				}
				if e.srv.store.Snapshot() != before {
					t.Error("rejected authorization changed durable registration")
				}
			})
		}
	}
}

func TestR17LinkMintPersistenceFailure(t *testing.T) {
	for _, kind := range []string{"port", "desktop"} {
		for _, existing := range []bool{false, true} {
			name := kind + "/new-minter"
			if existing {
				name = kind + "/existing-minter"
			}
			t.Run(name, func(t *testing.T) {
				e, vm := r17MintSetup(t, kind)
				p := e.srv.authenticate("Bearer " + e.owner)
				if existing {
					if out := r17Mint(e, p, vm, kind); out.status != 200 {
						t.Fatalf("initial mint: %d %s", out.status, out.body.String())
					}
				}
				if err := e.srv.store.Close(); err != nil {
					t.Fatal(err)
				}
				before := e.srv.store.Snapshot()
				out := r17Mint(e, p, vm, kind)
				if out.status != 500 {
					t.Errorf("mint without durable registration: %d %s", out.status, out.body.String())
				}
				if e.srv.store.Snapshot() != before {
					t.Error("failed persistence published registration")
				}
			})
		}
	}
}

func TestR17LinkMintAllowedWithCurrentExpiry(t *testing.T) {
	for _, kind := range []string{"port", "desktop"} {
		t.Run(kind, func(t *testing.T) {
			e, vm := r17MintSetup(t, kind)
			p := e.srv.authenticate("Bearer " + e.newKey("exec"))
			expires := time.Now().Add(30 * time.Second)
			if err := e.srv.store.Update(func(st *State) error {
				st.Keys[p.Key.ID].Expires = &expires
				st.RouteGens[vm] = 7
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				out := r17Mint(e, p, vm, kind)
				if out.status != 200 {
					t.Fatalf("allowed mint: %d %s", out.status, out.body.String())
				}
				var link struct{ URL string }
				if err := json.Unmarshal(out.body.Bytes(), &link); err != nil {
					t.Fatal(err)
				}
				u, err := url.Parse(link.URL)
				if err != nil {
					t.Fatal(err)
				}
				exp, sig := u.Query().Get("boat_exp"), u.Query().Get("boat_sig")
				want := SignRoute(PurposeKey(e.srv.cfg.DesktopKey, purposePortRoute), "8080-"+vm, 7, expires.Unix())
				if kind == "desktop" {
					exp, sig = u.Query().Get("exp"), u.Query().Get("sig")
					want = SignDesktop(PurposeKey(e.srv.cfg.DesktopKey, purposeDesktopLink), vm, 7, expires.Unix())
				}
				if exp != strconv.FormatInt(expires.Unix(), 10) || sig != want {
					t.Errorf("mint ignored current expiry/generation: %s", link.URL)
				}
			}
			if keys := e.srv.store.Snapshot().PortMinters[vm]; !slices.Equal(keys, []string{p.Key.ID}) {
				t.Errorf("duplicate/missing minter registration: %v", keys)
			}
		})
	}
}
