package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Reading 100 Continue proves authentication finished before the state change.
func r19PausedIssuance(t *testing.T, e *env, token, path string, body any, change func()) (int, map[string]any, *State) {
	t.Helper()
	api := httptest.NewServer(e.srv)
	defer api.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(api.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nExpect: 100-continue\r\nContent-Length: %d\r\n\r\n", path, token, len(encoded)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	interim, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	interim.Body.Close()
	if interim.StatusCode != 100 {
		t.Fatalf("pause status %d", interim.StatusCode)
	}
	change()
	before := e.srv.store.Snapshot()
	disk, err := os.ReadFile(filepath.Join(e.dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(encoded); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(response.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode >= 400 {
		if e.srv.store.Snapshot() != before {
			t.Fatal("denied issuance published state")
		}
		after, err := os.ReadFile(filepath.Join(e.dir, "state.json"))
		if err != nil || !bytes.Equal(disk, after) {
			t.Fatalf("denied issuance changed durable state: %v", err)
		}
		encoded, _ := json.Marshal(out)
		if bytes.Contains(encoded, []byte(`"token"`)) {
			t.Fatalf("denied issuance exposed credential: %s", encoded)
		}
	}
	return response.StatusCode, out, before
}

func TestR19IssuanceCurrentAuthority(t *testing.T) {
	for _, operation := range []string{"member", "rotate"} {
		t.Run(operation, func(t *testing.T) {
			for _, change := range []string{"delete-key", "delete-user", "delete-team", "membership", "key-user-changed", "key-team-changed", "expired", "rotate", "overlap", "overlap-expired", "previous-valid", "previous-expired", "current-expired-previous", "scope-revoked", "scope-reduced", "scope-expanded", "expiry-shortened", "original-expiry", "unchanged", "persist", "quota"} {
				t.Run(change, func(t *testing.T) {
					e := setup(t)
					token := e.newKey()
					if operation == "rotate" {
						token = e.newKey("exec")
					} else if change == "scope-expanded" {
						token = e.newKey("admin")
					}
					id, _, _ := ParseToken(token)
					parent := e.srv.store.Snapshot().Keys[id]
					rotate := func(overlap int) string {
						status, out := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": overlap})
						if status != 200 {
							t.Fatalf("revoke/overlap rotation: %d %v", status, out)
						}
						return out["token"].(string)
					}
					if strings.HasPrefix(change, "previous-") || change == "current-expired-previous" {
						fresh := rotate(3600)
						if change == "current-expired-previous" {
							token = fresh
						}
					}
					cap := time.Now().UTC().Add(time.Hour)
					if change == "original-expiry" {
						if err := e.srv.store.Update(func(st *State) error { st.Keys[id].Expires = &cap; return nil }); err != nil {
							t.Fatal(err)
						}
					}
					path := "/v1/teams/" + parent.TeamID + "/members"
					body := map[string]any{"name": "new-owner", "role": "owner"}
					want := 201
					if operation == "rotate" {
						path, body, want = "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": 0}, 200
					}
					status, out, before := r19PausedIssuance(t, e, token, path, body, func() {
						if change == "delete-key" {
							if status, out := e.do(e.owner, "DELETE", "/v1/keys/"+id, nil); status != 200 {
								t.Fatalf("delete parent: %d %v", status, out)
							}
						} else if change == "rotate" {
							rotate(0)
						} else if change == "overlap" || change == "overlap-expired" {
							rotate(3600)
						}
						if err := e.srv.store.Update(func(st *State) error {
							past := time.Now().Add(-time.Hour)
							switch change {
							case "delete-key", "rotate":
								want = 401
							case "delete-user":
								delete(st.Users, parent.UserID)
								want = 401
							case "delete-team":
								delete(st.Teams, parent.TeamID)
								want = 401
							case "membership":
								st.Teams[parent.TeamID].Members = nil
								want = 401
							case "key-user-changed":
								st.Keys[id].UserID = "other"
								want = 401
							case "key-team-changed":
								st.Keys[id].TeamID = "other"
								want = 401
							case "expired":
								st.Keys[id].Expires = &past
								want = 401
							case "previous-expired", "overlap-expired":
								st.Keys[id].PrevUntil = &past
								want = 401
							case "current-expired-previous":
								st.Keys[id].PrevUntil = &past
							case "scope-revoked":
								st.Keys[id].Scopes = []string{"vms:read"}
								want = 403
								if operation == "rotate" {
									want = 404
								}
							case "scope-reduced":
								if operation == "member" {
									st.Keys[id].Scopes = []string{"admin"}
								}
							case "scope-expanded":
								st.Keys[id].Scopes = nil
								if operation == "rotate" {
									want = 404
								}
							case "expiry-shortened":
								st.Keys[id].Expires = &cap
							case "original-expiry":
								st.Keys[id].Expires = nil
							case "persist":
								want = 500
							case "quota":
								if operation == "member" {
									want = 403
								}
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
						if want == 401 {
							if status, _ := e.do(token, "GET", "/v1/me", nil); status != 401 {
								t.Fatalf("revoked parent still authenticates: %d", status)
							}
						}
						if change == "quota" {
							e.srv.cfg.MaxKeysPerTeam = teamKeyCount(e.srv.store.Snapshot(), parent.TeamID)
						}
						if change == "persist" {
							if err := e.srv.store.Close(); err != nil {
								t.Fatal(err)
							}
						}
					})
					if status != want {
						t.Fatalf("issuance status %d want %d: %v", status, want, out)
					}
					if status >= 400 {
						return
					}
					if operation == "member" {
						if len(e.srv.store.Snapshot().Keys) != len(before.Keys)+1 || len(e.srv.store.Snapshot().Users) != len(before.Users)+1 {
							t.Fatal("missing new member/key")
						}
						out = out["key"].(map[string]any)
					}
					issued := out["token"].(string)
					if status, _ := e.do(issued, "GET", "/v1/me", nil); status != 200 {
						t.Fatalf("issued credential unusable: %d", status)
					}
					keyID, _, _ := ParseToken(issued)
					key := e.srv.store.Snapshot().Keys[keyID]
					if operation == "member" && (change == "scope-reduced" || change == "scope-expanded") && !slices.Equal(key.Scopes, []string{"admin"}) {
						t.Fatalf("child scopes exceed current/original authority: %v", key.Scopes)
					}
					if (change == "expiry-shortened" || (operation == "member" && change == "original-expiry")) && (key.Expires == nil || !key.Expires.Equal(cap)) {
						t.Fatalf("issued expiry %v want %v", key.Expires, cap)
					}
				})
			}
		})
	}
}

func TestR19MemberTargetAuthority(t *testing.T) {
	for _, change := range []string{"role-revoked", "owner-demoted", "original-role", "operator-allowed", "operator-revoked", "original-operator", "target-deleted", "target-membership", "target-role", "target-admin-member"} {
		t.Run(change, func(t *testing.T) {
			e := setup(t)
			token := e.newKey()
			id, _, _ := ParseToken(token)
			parent := e.srv.store.Snapshot().Keys[id]
			target := parent.TeamID
			if strings.HasPrefix(change, "operator-") || strings.HasPrefix(change, "target-") || change == "original-operator" {
				status, out := e.do(e.owner, "POST", "/v1/teams", map[string]any{"name": "target"})
				if status != 201 {
					t.Fatalf("target team: %d %v", status, out)
				}
				target = out["id"].(string)
			}
			if change == "original-role" || change == "original-operator" {
				if err := e.srv.store.Update(func(st *State) error {
					if change == "original-role" {
						st.Teams[parent.TeamID].Members[0].Role = "admin"
					} else {
						st.Users[parent.UserID].Operator = false
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			role, want := "owner", 403
			if change == "operator-allowed" || change == "target-admin-member" {
				want = 201
			}
			if change == "target-admin-member" {
				role = "member"
			}
			status, out, _ := r19PausedIssuance(t, e, token, "/v1/teams/"+target+"/members", map[string]any{"name": "child", "role": role}, func() {
				if err := e.srv.store.Update(func(st *State) error {
					switch change {
					case "role-revoked":
						st.Teams[parent.TeamID].Members[0].Role = "member"
					case "owner-demoted":
						st.Teams[parent.TeamID].Members[0].Role = "admin"
					case "original-role":
						st.Teams[parent.TeamID].Members[0].Role = "owner"
					case "operator-revoked":
						st.Users[parent.UserID].Operator = false
						want = 404
					case "original-operator":
						st.Users[parent.UserID].Operator = true
						want = 404
					case "target-deleted":
						delete(st.Teams, target)
						want = 404
					case "target-membership":
						st.Teams[target].Members = nil
						want = 404
					case "target-role":
						st.Teams[target].Members[0].Role = "member"
					case "target-admin-member":
						st.Teams[target].Members[0].Role = "admin"
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
			if status != want {
				t.Fatalf("member status %d want %d: %v", status, want, out)
			}
		})
	}
}

func TestR19RotationTargetAuthority(t *testing.T) {
	for _, change := range []string{"allowed", "caller-role", "original-role", "target-role", "target-operator", "caller-operator", "target-scope", "target-team", "target-deleted"} {
		t.Run(change, func(t *testing.T) {
			e := setup(t)
			token, caller := e.addMember("admin")
			targetToken, targetUser := e.addMember("member")
			targetID, _, _ := ParseToken(targetToken)
			if change == "caller-operator" {
				token = e.newKey()
				id, _, _ := ParseToken(token)
				caller = e.srv.store.Snapshot().Keys[id].UserID
			}
			if change == "original-role" {
				if err := e.srv.store.Update(func(st *State) error {
					for i := range st.Teams[e.teamID()].Members {
						if st.Teams[e.teamID()].Members[i].UserID == targetUser {
							st.Teams[e.teamID()].Members[i].Role = "owner"
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			want := 404
			if change == "allowed" {
				want = 200
			}
			status, out, _ := r19PausedIssuance(t, e, token, "/v1/keys/"+targetID+"/rotate", map[string]any{"overlap_s": 0}, func() {
				if err := e.srv.store.Update(func(st *State) error {
					team := st.Teams[st.Keys[targetID].TeamID]
					for i := range team.Members {
						m := &team.Members[i]
						if change == "caller-role" && m.UserID == caller {
							m.Role = "member"
						}
						if change == "original-role" && m.UserID == caller {
							m.Role = "owner"
						}
						if (change == "target-role" || change == "caller-operator") && m.UserID == targetUser {
							m.Role = "owner"
						}
					}
					switch change {
					case "target-operator":
						st.Users[targetUser].Operator = true
					case "caller-operator":
						st.Users[caller].Operator = false
						st.Users[targetUser].Operator = true
					case "target-scope":
						st.Keys[targetID].Scopes = nil
						st.Keys[keyID(e, token)].Scopes = []string{"admin"}
					case "target-team":
						st.Keys[targetID].TeamID = "other"
					case "target-deleted":
						delete(st.Keys, targetID)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
			if status != want {
				t.Fatalf("rotation status %d want %d: %v", status, want, out)
			}
		})
	}
}

func TestR19RotationHeaderlessCompatibility(t *testing.T) {
	for _, change := range []string{"allowed", "previous-valid", "previous-expired", "rotated"} {
		t.Run(change, func(t *testing.T) {
			e := setup(t)
			token := e.newKey("exec")
			id, _, _ := ParseToken(token)
			if strings.HasPrefix(change, "previous-") {
				if status, out := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": 3600}); status != 200 {
					t.Fatalf("pre-rotate: %d %v", status, out)
				}
			}
			p := e.srv.authenticate("Bearer " + token)
			if p == nil {
				t.Fatal("authentication failed")
			}
			want := 200
			if change == "rotated" {
				if status, out := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": 0}); status != 200 {
					t.Fatalf("revoke: %d %v", status, out)
				}
				want = 401
			}
			if change == "previous-expired" {
				if err := e.srv.store.Update(func(st *State) error { past := time.Now().Add(-time.Hour); st.Keys[id].PrevUntil = &past; return nil }); err != nil {
					t.Fatal(err)
				}
				want = 401
			}
			before := e.srv.store.Snapshot()
			r := httptest.NewRequest("POST", "/", nil)
			r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, &reqInfo{id: "r19-compat"}))
			out := e.srv.callNative(r, p, "POST /v1/keys/{id}/rotate", e.srv.rotateKey, id, map[string]any{"overlap_s": 0})
			if out.status != want {
				t.Fatalf("compat rotation status %d want %d: %s", out.status, want, out.body.String())
			}
			if want != 200 && (e.srv.store.Snapshot() != before || strings.Contains(out.body.String(), `"token"`)) {
				t.Fatal("denied compatibility rotation published credential/state")
			}
		})
	}
}
