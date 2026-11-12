package main

import (
	"slices"
	"testing"
)

// Council round 3 PoC: an admin-only key added a member and got back a key
// with the role's full scopes (exec included).
func TestAddMemberCannotWidenScopes(t *testing.T) {
	e := setup(t)
	adm := e.newKey("admin")
	team := e.teamID()
	st, m := e.do(adm, "POST", "/v1/teams/"+team+"/members", map[string]any{"name": "sock", "role": "admin"})
	if st == 201 {
		tok := m["key"].(map[string]any)["token"].(string)
		_, me := e.do(tok, "GET", "/v1/me", nil)
		for _, sc := range me["scopes"].([]any) {
			if sc != "admin" {
				t.Errorf("admin-only key minted a member key with %v", sc)
			}
		}
		if st, _ := e.do(tok, "POST", "/v1/vms", map[string]any{"name": "x", "size": "small"}); st != 403 {
			t.Errorf("minted key created a VM: %d", st)
		}
	} else if st != 403 {
		t.Errorf("add member with admin-only key: %d %v", st, m)
	}
	// A member role ∩ ["admin"] is empty: refuse rather than store "full".
	if st, m := e.do(adm, "POST", "/v1/teams/"+team+"/members", map[string]any{"name": "mem", "role": "member"}); st != 403 {
		t.Errorf("empty-scope member key: %d %v", st, m)
	}
	// A full key still gives the member the member role's scopes.
	st, m = e.do(e.owner, "POST", "/v1/teams/"+team+"/members", map[string]any{"name": "ok", "role": "member"})
	if st != 201 {
		t.Fatalf("owner add member: %d %v", st, m)
	}
	_, me := e.do(m["key"].(map[string]any)["token"].(string), "GET", "/v1/me", nil)
	if !slices.Contains(me["scopes"].([]any), any("exec")) {
		t.Errorf("full owner key: member scopes %v", me["scopes"])
	}
}

// A non-operator admin of two teams must use a key of the team in the path.
func TestTeamAdminUsesKeyTeam(t *testing.T) {
	e := setup(t)
	teamA := e.teamID()
	_, tb := e.do(e.owner, "POST", "/v1/teams", map[string]any{"name": "other"})
	teamB := tb["id"].(string)
	st, m := e.do(e.owner, "POST", "/v1/teams/"+teamA+"/members", map[string]any{"name": "bob", "role": "admin"})
	if st != 201 {
		t.Fatalf("add bob: %d %v", st, m)
	}
	bobA := m["key"].(map[string]any)["token"].(string)
	bobID := m["user"].(map[string]any)["id"].(string)
	if err := e.srv.store.Update(func(st *State) error {
		tm := st.Teams[teamB]
		tm.Members = append(slices.Clone(tm.Members), Member{UserID: bobID, Role: "admin"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.do(bobA, "POST", "/v1/teams/"+teamB+"/members", map[string]any{"name": "eve", "role": "admin"}); st != 404 {
		t.Errorf("team-A key managed team B: %d", st)
	}
	// The operator may still manage any team they belong to.
	if st, m := e.do(e.owner, "POST", "/v1/teams/"+teamB+"/members", map[string]any{"name": "eve", "role": "member"}); st != 201 {
		t.Errorf("operator managing team B: %d %v", st, m)
	}
}
