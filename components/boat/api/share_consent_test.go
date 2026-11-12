package main

import (
	"reflect"
	"sort"
	"testing"
)

// Council round 3: a team grant to an unknown team answered 404, a grant to a
// real one 201 (an existence oracle), and the grant appeared in the recipient's
// list unasked. Now both answer alike and the recipient must opt in.
func TestShareNeedsRecipientConsent(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	base := "/v1/vms/" + id + "/snapshots/" + e.snapshotOf(id) + "/shares"
	tid, other := e.newTeam("other")
	keys := func(m map[string]any) []string {
		var k []string
		for x := range m {
			if x != "id" && x != "created" && x != "grantee" {
				k = append(k, x)
			}
		}
		sort.Strings(k)
		return k
	}
	stNo, no := e.do(e.owner, "POST", base, map[string]any{"team_id": "team_000000000000"})
	stYes, yes := e.do(e.owner, "POST", base, map[string]any{"team_id": tid})
	if stNo != stYes || !reflect.DeepEqual(keys(no), keys(yes)) {
		t.Errorf("existence oracle: unknown %d %v, real %d %v", stNo, no, stYes, yes)
	}
	gid, _ := yes["id"].(string)
	if _, l := e.do(other, "GET", "/v1/shared-snapshots", nil); len(list(l, "shares")) != 0 {
		t.Errorf("unsolicited share listed: %v", l)
	}
	if st, _ := e.do(other, "POST", "/v1/snapshots/import-share", map[string]any{"share_id": gid, "name": "c"}); st != 404 {
		t.Errorf("unaccepted share imported: %d", st)
	}
	// The recipient opts in to the sender's team.
	if st, out := e.do(other, "PUT", "/v1/teams/"+tid+"/share-allowlist", map[string]any{"teams": []string{e.teamID()}}); st != 200 {
		t.Fatalf("allowlist: %d %v", st, out)
	}
	if _, l := e.do(other, "GET", "/v1/shared-snapshots", nil); len(list(l, "shares")) != 1 {
		t.Errorf("accepted share not listed: %v", l)
	}
	if st, out := e.do(other, "POST", "/v1/snapshots/import-share", map[string]any{"share_id": gid, "name": "c"}); st != 201 {
		t.Errorf("accepted share: %d %v", st, out)
	}
	// Only the recipient's own admins set its allowlist.
	if st, _ := e.do(e.owner, "PUT", "/v1/teams/"+tid+"/share-allowlist", map[string]any{"teams": []string{}}); st != 404 {
		t.Errorf("foreign team set the allowlist: %d", st)
	}
	if st, _ := e.do(other, "PUT", "/v1/teams/"+tid+"/share-allowlist", map[string]any{"teams": []string{"bad id"}}); st != 400 {
		t.Errorf("bad team id: %d", st)
	}
}
