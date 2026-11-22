package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func r20Reload(t *testing.T, e *env) *server {
	t.Helper()
	store, err := OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return &server{store: store, keys: e.srv.keys}
}

func TestR20DuplicateScopesCannotEscalate(t *testing.T) {
	for _, source := range []string{"http", "stored"} {
		for _, operation := range []string{"member-owner", "member-admin", "member-member", "team"} {
			for _, scopes := range [][]string{{"admin", "admin", "admin", "admin"}, {"admin", "exec", "admin", "exec"}, {"admin", "exec", "exec", "exec"}, {"admin", "exec", "vms:read", "vms:write", "admin"}} {
				t.Run(source+"/"+operation+"/"+strings.Join(scopes, ","), func(t *testing.T) {
					e := setup(t)
					allowed := slices.Clone(scopes)
					slices.Sort(allowed)
					allowed = slices.Compact(allowed)
					parent := e.newKey(allowed...)
					status, out := e.do(parent, "POST", "/v1/keys", map[string]any{"name": "duplicates", "scopes": scopes})
					if status != 201 {
						t.Fatalf("duplicate key: %d %v", status, out)
					}
					token := out["token"].(string)
					if source == "stored" {
						if err := e.srv.store.Update(func(st *State) error { st.Keys[keyID(e, token)].Scopes = slices.Clone(scopes); return nil }); err != nil {
							t.Fatal(err)
						}
					}
					if p := e.srv.authenticate("Bearer " + token); p == nil || p.has("vms:write") != slices.Contains(allowed, "vms:write") {
						t.Fatal("parent authority differs from requested scopes")
					}
					path := "/v1/teams/" + e.teamID() + "/members"
					body := map[string]any{"name": "child", "role": strings.TrimPrefix(operation, "member-")}
					if operation == "team" {
						path, body = "/v1/teams", map[string]any{"name": "child-team", "owner": "child-owner"}
					}
					granted := slices.Clone(allowed)
					if operation == "member-member" {
						granted = slices.DeleteFunc(granted, func(sc string) bool { return sc == "admin" })
					}
					before := e.srv.store.Snapshot()
					status, out = e.do(token, "POST", path, body)
					if len(granted) == 0 {
						if status != 403 || e.srv.store.Snapshot() != before {
							t.Fatalf("empty intersection granted role scopes: %d %v", status, out)
						}
						return
					}
					if status != 201 {
						t.Fatalf("issuance: %d %v", status, out)
					}
					if operation == "team" {
						out = out["owner"].(map[string]any)
					}
					child := out["key"].(map[string]any)["token"].(string)
					if status, out := e.do(child, "GET", "/v1/me", nil); status != 200 {
						t.Fatalf("child unusable: %d %v", status, out)
					}
					for _, srv := range []*server{e.srv, r20Reload(t, e)} {
						p := srv.authenticate("Bearer " + child)
						if p == nil {
							t.Fatal("child missing after durable reload")
						}
						for _, sc := range allScopes {
							if p.has(sc) != slices.Contains(granted, sc) {
								t.Fatalf("expected %v, minted child scopes %v", granted, p.Scopes)
							}
						}
					}
					if !slices.Contains(granted, "vms:write") {
						if status, out := e.do(child, "POST", "/v1/keys", map[string]any{"name": "grandchild", "scopes": []string{"vms:write"}}); status != 403 {
							t.Fatalf("child escalated through key mint: %d %v", status, out)
						}
					}
				})
			}
		}
	}
}

func TestR20CreateTeamCurrentAuthority(t *testing.T) {
	for _, owner := range []bool{false, true} {
		for _, change := range []string{"delete-key", "delete-user", "delete-team", "membership", "key-user-changed", "key-team-changed", "expired", "rotate", "overlap", "overlap-expired", "previous-valid", "previous-expired", "current-expired-previous", "scope-revoked", "role-revoked", "operator-revoked", "original-operator", "scope-reduced", "scope-expanded", "expiry-shortened", "original-expiry", "restricted", "unchanged", "persist"} {
			t.Run(map[bool]string{false: "self", true: "new-owner"}[owner]+"/"+change, func(t *testing.T) {
				e := setup(t)
				token := e.newKey()
				if change == "scope-expanded" || change == "restricted" {
					token = e.newKey("admin")
				}
				id := keyID(e, token)
				parent := e.srv.store.Snapshot().Keys[id]
				rotate := func(overlap int) string {
					status, out := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": overlap})
					if status != 200 {
						t.Fatalf("rotate parent: %d %v", status, out)
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
				if err := e.srv.store.Update(func(st *State) error {
					if change == "original-expiry" {
						st.Keys[id].Expires = &cap
					}
					if change == "original-operator" {
						st.Users[parent.UserID].Operator = false
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				body := map[string]any{"name": "issued-team"}
				if owner {
					body["owner"] = "issued-owner"
				}
				want := 201
				status, out, before := r19PausedIssuance(t, e, token, "/v1/teams", body, func() {
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
							st.Keys[id].Scopes = []string{"exec"}
							want = 403
						case "role-revoked":
							st.Teams[parent.TeamID].Members[0].Role = "member"
							want = 403
						case "operator-revoked":
							st.Users[parent.UserID].Operator = false
							want = 403
						case "original-operator":
							st.Users[parent.UserID].Operator = true
							want = 403
						case "scope-reduced":
							st.Keys[id].Scopes = []string{"admin"}
						case "scope-expanded":
							st.Keys[id].Scopes = nil
						case "expiry-shortened":
							st.Keys[id].Expires = &cap
						case "original-expiry":
							st.Keys[id].Expires = nil
						case "persist":
							want = 500
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
					if change == "persist" {
						if err := e.srv.store.Close(); err != nil {
							t.Fatal(err)
						}
					}
				})
				if status != want {
					t.Fatalf("create team status %d want %d: %v", status, want, out)
				}
				reloaded := r20Reload(t, e)
				if status >= 400 {
					after := reloaded.store.Snapshot()
					if len(after.Teams) != len(before.Teams) || len(after.Users) != len(before.Users) || len(after.Keys) != len(before.Keys) {
						t.Fatal("denied team issuance persisted objects")
					}
					return
				}
				for _, srv := range []*server{e.srv, reloaded} {
					st := srv.store.Snapshot()
					team := st.Teams[out["id"].(string)]
					if team == nil || len(st.Teams) != len(before.Teams)+1 {
						t.Fatal("team missing after durable reload")
					}
					if !owner {
						if team.role(parent.UserID) != "owner" || len(st.Keys) != len(before.Keys) || len(st.Users) != len(before.Users) {
							t.Fatal("self-owned team changed users or keys")
						}
						continue
					}
					child := out["owner"].(map[string]any)["key"].(map[string]any)["token"].(string)
					p := srv.authenticate("Bearer " + child)
					if p == nil || p.Operator || p.Role != "owner" || len(st.Keys) != len(before.Keys)+1 || len(st.Users) != len(before.Users)+1 {
						t.Fatal("owner credential missing or wrong authority after reload")
					}
					if (change == "expiry-shortened" || change == "original-expiry") && (p.Key.Expires == nil || !p.Key.Expires.Equal(cap)) {
						t.Fatalf("child expiry %v want %v", p.Key.Expires, cap)
					}
					if change == "scope-reduced" || change == "scope-expanded" || change == "restricted" {
						if !slices.Equal(p.Scopes, []string{"admin"}) {
							t.Fatalf("child exceeds original/current scopes: %v", p.Scopes)
						}
					} else if !slices.Equal(p.Scopes, allScopes) || len(p.Key.Scopes) != 0 {
						t.Fatalf("full owner scope semantics changed: %v", p.Key.Scopes)
					}
				}
			})
		}
	}
}
