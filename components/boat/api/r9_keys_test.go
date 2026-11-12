package main

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func keyExpiry(t *testing.T, e *env, tok string) *time.Time {
	t.Helper()
	id, _, _ := ParseToken(tok)
	k := e.srv.store.Snapshot().Keys[id]
	if k == nil {
		t.Fatalf("key %s not stored", id)
	}
	return k.Expires
}

// A short-lived key cannot mint a longer-lived or immortal one, natively, via
// the boat.dev route, or as a new member's or team owner's first key.
func TestChildKeyNeverOutlivesParent(t *testing.T) {
	e := setup(t)
	st, out := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "short", "expires_in_s": 60})
	if st != 201 {
		t.Fatalf("short key: %d %v", st, out)
	}
	short := out["token"].(string)
	parentExp := keyExpiry(t, e, short)

	if st, out := e.do(short, "POST", "/v1/keys", map[string]any{"name": "forever"}); st != 400 {
		t.Errorf("immortal child of an expiring key: %d %v", st, out)
	}
	if st, out, _ := e.compat(short, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "forever", "preset": "read-only"}, nil); st != 400 {
		t.Errorf("compat immortal child: %d %v", st, out)
	}
	st, out = e.do(short, "POST", "/v1/keys", map[string]any{"name": "long", "expires_in_s": 86400})
	if st != 201 {
		t.Fatalf("long child: %d %v", st, out)
	}
	if exp := keyExpiry(t, e, out["token"].(string)); exp == nil || exp.After(*parentExp) {
		t.Errorf("child expiry %v not clamped to parent %v", exp, parentExp)
	}
	st, out = e.do(short, "POST", "/v1/teams/"+e.teamID()+"/members", map[string]any{"name": "bob", "role": "member"})
	if st != 201 {
		t.Fatalf("add member: %d %v", st, out)
	}
	if exp := keyExpiry(t, e, out["key"].(map[string]any)["token"].(string)); exp == nil || exp.After(*parentExp) {
		t.Errorf("member key expiry %v, parent %v", exp, parentExp)
	}
	st, out = e.do(short, "POST", "/v1/teams", map[string]any{"name": "t2", "owner": "o2"})
	if st != 201 {
		t.Fatalf("create team: %d %v", st, out)
	}
	if exp := keyExpiry(t, e, out["owner"].(map[string]any)["key"].(map[string]any)["token"].(string)); exp == nil || exp.After(*parentExp) {
		t.Errorf("owner key expiry %v, parent %v", exp, parentExp)
	}
	// A key that never expires still mints keys that never expire.
	st, out = e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "forever"})
	if st != 201 || keyExpiry(t, e, out["token"].(string)) != nil {
		t.Errorf("immortal parent: %d %v", st, out)
	}
}

// A key bound to team A cannot mint a key for team B, even when its user has a
// role there; the operator can.
func TestKeyMintStaysInKeyTeam(t *testing.T) {
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
	if st, out := e.do(bobA, "POST", "/v1/keys", map[string]any{"name": "x", "team_id": teamB}); st != 403 {
		t.Errorf("team-A key minted for team B: %d %v", st, out)
	}
	if st, out := e.do(e.owner, "POST", "/v1/keys", map[string]any{"name": "x", "team_id": teamB}); st != 201 {
		t.Errorf("operator mint for team B: %d %v", st, out)
	}
}

// A desktop link never outlives its key, and deleting the key (or removing its
// member) revokes it, like a private port link. The gateway learns which VMs
// have desktop links from /internal/route-gens.
func TestDesktopLinkCappedAndRevoked(t *testing.T) {
	for _, how := range []string{"delete-key", "remove-member"} {
		t.Run(how, func(t *testing.T) {
			e := setup(t)
			e.srv.cfg.DesktopTTL = 24 * time.Hour
			st, m := e.do(e.owner, "POST", "/v1/teams/"+e.teamID()+"/members", map[string]any{"name": "bob", "role": "member"})
			if st != 201 {
				t.Fatalf("add member: %d %v", st, m)
			}
			uid := m["user"].(map[string]any)["id"].(string)
			bob := m["key"].(map[string]any)["token"].(string)
			st, out := e.do(bob, "POST", "/v1/keys", map[string]any{"name": "short", "expires_in_s": 600})
			if st != 201 {
				t.Fatalf("key: %d %v", st, out)
			}
			short := out["token"].(string)
			shortID, _, _ := ParseToken(short)
			id := e.createVM()
			st, d := e.do(short, "GET", "/v1/vms/"+id+"/desktop", nil)
			if st != 200 {
				t.Fatalf("desktop: %d %v", st, d)
			}
			exp, _ := time.Parse(time.RFC3339, d["expires_at"].(string))
			if keyExp := keyExpiry(t, e, short); exp.After(*keyExp) {
				t.Errorf("desktop link expires %v, after its key %v", exp, keyExp)
			}
			if _, b := e.raw("", "GET", "/internal/route-gens", nil); !strings.Contains(string(b), `"desktops":["`+id+`"]`) {
				t.Errorf("route-gens lacks the desktop VM: %s", b)
			}
			gen := e.srv.store.Snapshot().RouteGens[id]
			if how == "delete-key" {
				if st, out := e.do(e.owner, "DELETE", "/v1/keys/"+shortID, nil); st != 200 {
					t.Fatalf("delete key: %d %v", st, out)
				}
			} else if st, out := e.do(e.owner, "DELETE", "/v1/teams/"+e.teamID()+"/members/"+uid, nil); st != 200 {
				t.Fatalf("remove member: %d %v", st, out)
			}
			if e.srv.store.Snapshot().RouteGens[id] <= gen {
				t.Errorf("desktop link survives %s", how)
			}
			// The next link is signed with the new generation.
			_, d = e.do(e.owner, "GET", "/v1/vms/"+id+"/desktop", nil)
			u, _ := url.Parse(d["url"].(string))
			x, _ := strconv.ParseInt(u.Query().Get("exp"), 10, 64)
			if u.Query().Get("sig") != SignDesktop(PurposeKey([]byte("desktop-key"), purposeDesktopLink), id, e.srv.store.Snapshot().RouteGens[id], x) {
				t.Errorf("new link not signed with generation %d", e.srv.store.Snapshot().RouteGens[id])
			}
		})
	}
}

// Forking or snapshotting a VM ends the AI proxy token on its disk, like
// creating a share does: the copy must not carry a live token.
func TestForkAndSnapshotRotateAIToken(t *testing.T) {
	for _, op := range []string{"fork", "snapshots"} {
		t.Run(op, func(t *testing.T) {
			e, _, _ := aiSetup(t)
			if st, c := e.do(e.owner, "PUT", "/v1/ai/credentials/anthropic", map[string]any{"value": "sk-ant-REAL-KEY-123456"}); st != 200 {
				t.Fatalf("set: %d %v", st, c)
			}
			id := e.createVM()
			gen := e.srv.store.Snapshot().AIVMs[id].Gen
			if st, out := e.do(e.owner, "POST", "/v1/vms/"+id+"/"+op, map[string]any{}); st != 201 {
				t.Fatalf("%s: %d %v", op, st, out)
			}
			if g := e.srv.store.Snapshot().AIVMs[id].Gen; g <= gen {
				t.Errorf("%s left the parent's AI token generation at %d", op, g)
			}
		})
	}
}
