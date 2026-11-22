package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestR20FileGrantIssuanceAuthority(t *testing.T) {
	for _, kind := range []string{"download", "upload"} {
		for _, change := range []string{"zero-overlap", "current", "becomes-previous", "previous", "expired-previous", "replaced-previous", "expired-key", "reduced-scope", "deleted-key", "removed-member", "self-current", "self-retired"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				e := setup(t)
				vm := e.createVM()
				path := "/api/v1/sandboxes/" + vm + "/files"
				e.ok("PUT", path, map[string]any{"path": "secret.txt", "content": "synthetic-tenant-secret"}, 200)
				token := e.newKey("exec", "vms:read")
				id, _, _ := ParseToken(token)
				rotate := func(overlap int) string {
					code, out := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": overlap})
					if code != 200 {
						t.Fatalf("rotate %d %v", code, out)
					}
					return out["token"].(string)
				}
				switch change {
				case "current":
					token = rotate(0)
				case "previous", "expired-previous", "replaced-previous":
					rotate(60)
				case "self-current", "self-retired":
					gen, err := e.srv.bumpSelf(vm, e.teamID())
					if err != nil {
						t.Fatal(err)
					}
					token = e.srv.selfToken(vm, gen)
				}
				allowed := change == "current" || change == "becomes-previous" || change == "previous" || change == "self-current"
				status, out, _ := r19PausedIssuance(t, e, token, path+"/"+kind+"-url", map[string]any{"path": "secret.txt", "ttlSeconds": 3600}, func() {
					switch change {
					case "zero-overlap":
						rotate(0)
					case "becomes-previous", "replaced-previous":
						rotate(60)
					}
					if err := e.srv.store.Update(func(st *State) error {
						past := time.Now().Add(-time.Minute)
						switch change {
						case "expired-previous":
							st.Keys[id].PrevUntil = &past
						case "expired-key":
							st.Keys[id].Expires = &past
						case "reduced-scope":
							st.Keys[id].Scopes = []string{"vms:read"}
						case "deleted-key":
							delete(st.Keys, id)
						case "removed-member":
							st.Teams[e.teamID()].Members = slices.DeleteFunc(st.Teams[e.teamID()].Members, func(m Member) bool { return m.UserID == st.Keys[id].UserID })
						case "self-retired":
							st.cowVMToken(vm).Retired = true
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				})
				// Always exercise any URL returned by a vulnerable minter, including denied cases.
				raw, _ := out["url"].(string)
				if raw != "" {
					method := "GET"
					var body io.Reader
					if kind == "upload" {
						method = "PUT"
						body = strings.NewReader("post-revocation-overwrite")
					}
					req, err := http.NewRequest(method, strings.Replace(raw, "http://test", e.api.URL, 1), body)
					if err != nil {
						t.Fatal(err)
					}
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					data, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil {
						t.Fatal(err)
					}
					if !allowed && resp.StatusCode/100 == 2 {
						t.Errorf("revoked credential minted usable %s URL: %d %q", kind, resp.StatusCode, data)
					}
					if allowed && (resp.StatusCode != 200 || (kind == "download" && string(data) != "synthetic-tenant-secret")) {
						t.Errorf("allowed URL use: %d %q", resp.StatusCode, data)
					}
					if allowed {
						if err := e.srv.store.Update(func(st *State) error {
							if change == "self-current" {
								st.cowVMToken(vm).Retired = true
							} else {
								delete(st.Keys, id)
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
						back, err := OpenStore(e.dir)
						if err != nil {
							t.Fatal(err)
						}
						restored := &server{store: back, keys: e.srv.keys}
						prefix := fileDownloadPath
						if kind == "upload" {
							prefix = fileUploadPath
						}
						_, valid := restored.openFileGrant(strings.Split(raw, prefix)[1], time.Now())
						back.Close()
						if valid {
							t.Error("grant survived durable issuer revocation")
						}
						req, err = http.NewRequest(method, strings.Replace(raw, "http://test", e.api.URL, 1), strings.NewReader("forbidden"))
						if err != nil {
							t.Fatal(err)
						}
						resp, err = http.DefaultClient.Do(req)
						if err != nil {
							t.Fatal(err)
						}
						resp.Body.Close()
						if resp.StatusCode != 404 {
							t.Errorf("revoked URL use=%d", resp.StatusCode)
						}
					}
				}
				if allowed {
					if status != 200 || raw == "" {
						t.Fatalf("valid issuer rejected: %d %v", status, out)
					}
				} else if status != 403 || raw != "" {
					t.Errorf("stale issuer mint: %d %v", status, out)
				}
			})
		}
	}
}

func TestR20FileGrantOriginalAuthorityBounds(t *testing.T) {
	for _, kind := range []string{"download", "upload"} {
		for _, change := range []string{"original-expiry", "current-expiry", "original-scope", "original-expired", "foreign-vm"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				e := setup(t)
				vm := e.createVM()
				scope := "exec"
				if change == "original-scope" {
					scope = "vms:read"
				}
				token := e.newKey(scope)
				id, _, _ := ParseToken(token)
				expires := time.Now().Add(30 * time.Second)
				if err := e.srv.store.Update(func(st *State) error { st.Keys[id].Expires = &expires; return nil }); err != nil {
					t.Fatal(err)
				}
				p := e.srv.authenticate("Bearer " + token)
				if p == nil {
					t.Fatal("authentication failed")
				}
				want := expires.Unix()
				if change == "original-expired" {
					past := time.Now().Add(-time.Minute)
					copyKey := *p.Key
					copyKey.Expires = &past
					p.Key = &copyKey
				}
				if err := e.srv.store.Update(func(st *State) error {
					switch change {
					case "original-expiry", "original-expired":
						st.Keys[id].Expires = nil
					case "current-expiry":
						shorter := expires.Add(-10 * time.Second)
						st.Keys[id].Expires = &shorter
						want = shorter.Unix()
					case "original-scope":
						st.Keys[id].Scopes = []string{"exec"}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if change == "foreign-vm" {
					e.boatd.mu.Lock()
					e.boatd.vms[vm]["team"] = "foreign"
					e.boatd.mu.Unlock()
				}
				pattern := "POST /api/v1/sandboxes/{id}/files/" + kind + "-url"
				r := httptest.NewRequest("POST", "/", nil)
				r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, &reqInfo{pattern: pattern}))
				grant := fileGrant{VM: vm, Path: "/home/dev/secret.txt", Exp: time.Now().Add(time.Hour).Unix()}
				if kind == "upload" {
					grant.Op = "put"
				}
				before := e.srv.store.Snapshot()
				w := httptest.NewRecorder()
				tok := e.srv.mintFileURL(w, r, p, &grant)
				if e.srv.store.Snapshot() != before {
					t.Error("file signing published persistent state")
				}
				if change == "original-scope" || change == "original-expired" || change == "foreign-vm" {
					wantCode := 403
					if change == "foreign-vm" {
						wantCode = 404
					}
					if tok != "" || w.Code != wantCode {
						t.Fatalf("stale authority signed token: code=%d", w.Code)
					}
					return
				}
				opened, ok := e.srv.openFileGrant(tok, time.Now())
				if !ok || opened.Exp != want {
					t.Fatalf("expiry bound: valid=%v exp=%d want=%d", ok, opened.Exp, want)
				}
				if _, ok := e.srv.openFileGrant(tok, time.Unix(want, 0)); ok {
					t.Error("grant valid at expiry")
				}
			})
		}
	}
}
