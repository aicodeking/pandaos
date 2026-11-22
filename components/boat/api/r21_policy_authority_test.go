package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type r21StateSnap struct {
	mem  *State
	disk *State
	raw  []byte
}

func r21CaptureState(t *testing.T, e *env) r21StateSnap {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return r21StateSnap{
		mem:  e.srv.store.Snapshot(),
		disk: r20Reload(t, e).store.Snapshot(),
		raw:  raw,
	}
}

// The interim response proves the real handler authenticated before the change.
func r21PausedWrite(t *testing.T, e *env, token, method, path string, body any, change func()) (int, map[string]any, r21StateSnap) {
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
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nExpect: 100-continue\r\nContent-Length: %d\r\n\r\n", method, path, token, len(b)); err != nil {
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
	before := r21CaptureState(t, e)
	if _, err := conn.Write(b); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode >= 400 {
		r21AssertUnchanged(t, e, before)
	}
	return resp.StatusCode, out, before
}

func r21ChangeAuthority(t *testing.T, e *env, token, change string) int {
	t.Helper()
	id, _, _ := ParseToken(token)
	if change == "delete-key" {
		if status, out := e.do(e.owner, "DELETE", "/v1/keys/"+id, nil); status != 200 {
			t.Fatalf("revoke key: %d %v", status, out)
		}
		if status, _ := e.do(token, "GET", "/v1/me", nil); status != 401 {
			t.Fatalf("revoked key authenticates: %d", status)
		}
		return 401
	}
	if change == "rotate" || change == "overlap" {
		overlap := 0
		if change == "overlap" {
			overlap = 3600
		}
		if status, out := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": overlap}); status != 200 {
			t.Fatalf("rotate key: %d %v", status, out)
		}
		if overlap == 0 {
			return 401
		}
		return 0
	}
	want := 0
	if err := e.srv.store.Update(func(st *State) error {
		key := st.Keys[id]
		past := time.Now().Add(-time.Hour)
		switch change {
		case "expired":
			key.Expires = &past
			want = 401
		case "expired-previous":
			key.PrevUntil = &past
			want = 401
		case "scope":
			key.Scopes = []string{"exec"}
			want = 403
		case "role":
			st.Teams[key.TeamID].Members[0].Role = "member"
			want = 403
		case "admin-role":
			st.Teams[key.TeamID].Members[0].Role = "admin"
		case "membership":
			st.Teams[key.TeamID].Members = nil
			want = 401
		case "key-team":
			key.TeamID = "team_0123456789abcdef"
			want = 401
		case "key-user":
			key.UserID = "usr_other"
			want = 401
		case "delete-user":
			delete(st.Users, key.UserID)
			want = 401
		case "delete-team":
			delete(st.Teams, key.TeamID)
			want = 401
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return want
}

func r21AssertUnchanged(t *testing.T, e *env, before r21StateSnap) {
	t.Helper()
	if cur := e.srv.store.Snapshot(); cur != before.mem || !reflect.DeepEqual(cur, before.mem) {
		t.Error("denied request changed state in memory")
	}
	afterRaw, err := os.ReadFile(filepath.Join(e.dir, "state.json"))
	if err != nil || !bytes.Equal(afterRaw, before.raw) {
		t.Errorf("denied request changed durable state file: %v", err)
	}
	if !reflect.DeepEqual(r20Reload(t, e).store.Snapshot(), before.disk) {
		t.Error("denied request changed state after durable reopen")
	}
}

var r21AuthorityChanges = []string{"delete-key", "rotate", "overlap", "expired", "previous-valid", "expired-previous", "scope", "role", "membership", "key-team", "key-user", "delete-user", "delete-team", "admin-role", "unchanged"}

func TestR21SecurityPolicyCurrentAuthority(t *testing.T) {
	for _, policy := range []string{"self-token", "share-allowlist"} {
		for _, change := range append(append([]string{}, r21AuthorityChanges...), "persist") {
			t.Run(policy+"/"+change, func(t *testing.T) {
				e := setup(t)
				token := e.newKey("admin")
				if strings.Contains(change, "previous") {
					id, _, _ := ParseToken(token)
					if status, out := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": 3600}); status != 200 {
						t.Fatalf("pre-rotate: %d %v", status, out)
					}
				}
				team := e.teamID()
				body := map[string]any{"ssh": true, "public_ports": true}
				if policy == "share-allowlist" {
					body = map[string]any{"teams": []string{"team_02", "team_01", "team_02"}}
				}
				want := 200
				status, out, before := r21PausedWrite(t, e, token, "PUT", "/v1/teams/"+team+"/"+policy, body, func() {
					if denied := r21ChangeAuthority(t, e, token, change); denied != 0 {
						want = denied
					}
					if change == "persist" {
						want = 500
						if err := e.srv.store.Close(); err != nil {
							t.Fatal(err)
						}
					}
				})
				if status != want {
					t.Errorf("policy status %d want %d: %v", status, want, out)
				}
				if want != 200 {
					r21AssertUnchanged(t, e, before)
					return
				}
				if status != 200 {
					return
				}
				for _, st := range []*State{e.srv.store.Snapshot(), r20Reload(t, e).store.Snapshot()} {
					if policy == "self-token" && st.Teams[team].SelfToken != (SelfTokenPolicy{SSH: true, PublicPorts: true}) {
						t.Error("valid self-token policy not persisted")
					}
					if policy == "share-allowlist" && !reflect.DeepEqual(st.Teams[team].AcceptSharesFrom, []string{"team_01", "team_02"}) {
						t.Error("valid allowlist not sorted, deduplicated and persisted")
					}
				}
			})
		}

		for _, change := range []string{"operator-allowed", "operator-revoked", "original-operator", "target-deleted", "target-membership", "target-role", "target-admin"} {
			t.Run(policy+"/cross-team/"+change, func(t *testing.T) {
				e := setup(t)
				token := e.newKey("admin")
				id, _, _ := ParseToken(token)
				parent := e.srv.store.Snapshot().Keys[id]
				st, created := e.do(e.owner, "POST", "/v1/teams", map[string]any{"name": "target"})
				if st != 201 {
					t.Fatalf("target team: %d %v", st, created)
				}
				target := created["id"].(string)
				if change == "original-operator" {
					if err := e.srv.store.Update(func(s *State) error {
						s.Users[parent.UserID].Operator = false
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				body := map[string]any{"ssh": true, "public_ports": true}
				if policy == "share-allowlist" {
					body = map[string]any{"teams": []string{"team_01"}}
				}
				want := 200
				status, out, before := r21PausedWrite(t, e, token, "PUT", "/v1/teams/"+target+"/"+policy, body, func() {
					if err := e.srv.store.Update(func(s *State) error {
						switch change {
						case "operator-revoked":
							s.Users[parent.UserID].Operator = false
							want = 404
						case "original-operator":
							s.Users[parent.UserID].Operator = true
							want = 404
						case "target-deleted":
							delete(s.Teams, target)
							want = 404
						case "target-membership":
							s.Teams[target].Members = nil
							want = 404
						case "target-role":
							s.Teams[target].Members[0].Role = "member"
							want = 403
						case "target-admin":
							s.Teams[target].Members[0].Role = "admin"
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				})
				if status != want {
					t.Errorf("cross-team policy status %d want %d: %v", status, want, out)
				}
				if want != 200 {
					r21AssertUnchanged(t, e, before)
				}
			})
		}
	}
}

func TestR21SnapshotShareCurrentAuthority(t *testing.T) {
	for _, kind := range []string{"team", "code"} {
		for _, change := range append(append([]string{}, r21AuthorityChanges...), "retired-self", "foreign-self", "retired-ai", "foreign-ai", "quota", "persist", "consent-removed", "before-consent") {
			t.Run(kind+"/"+change, func(t *testing.T) {
				e := setup(t)
				e.srv.cfg.VMAPIURL = "https://boat.example"
				vm := e.createVM()
				// Keep credential refresh local and synchronous in the fake host.
				e.boatd.mu.Lock()
				e.boatd.vms[vm]["state"] = "stopped"
				e.boatd.mu.Unlock()
				sid := e.snapshotOf(vm)
				team := e.teamID()
				recipient, other := e.newTeam("recipient")
				if change != "before-consent" {
					if status, out := e.do(other, "PUT", "/v1/teams/"+recipient+"/share-allowlist", map[string]any{"teams": []string{team}}); status != 200 {
						t.Fatalf("consent: %d %v", status, out)
					}
				}
				if err := e.srv.store.Update(func(st *State) error {
					if st.VMTokens[vm] == nil {
						st.VMTokens[vm] = &VMToken{TeamID: team, Gen: 1}
					}
					st.AIVMs[vm] = &AIVM{TeamID: team, Gen: 7}
					st.PortMinters[vm] = []string{selfKeyID(vm, st.VMTokens[vm].Gen)}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				token := e.newKey("admin")
				if strings.Contains(change, "previous") {
					id, _, _ := ParseToken(token)
					if status, out := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": 3600}); status != 200 {
						t.Fatalf("pre-rotate: %d %v", status, out)
					}
				}
				body := map[string]any{}
				if kind == "team" {
					body["team_id"] = recipient
				}
				want := 201
				status, out, before := r21PausedWrite(t, e, token, "POST", "/v1/vms/"+vm+"/snapshots/"+sid+"/shares", body, func() {
					if denied := r21ChangeAuthority(t, e, token, change); denied != 0 {
						want = denied
					}
					if err := e.srv.store.Update(func(st *State) error {
						switch change {
						case "retired-self":
							st.cowVMToken(vm).Retired = true
							want = 500
						case "foreign-self":
							st.cowVMToken(vm).TeamID = recipient
							want = 500
						case "retired-ai":
							st.cowAIVM(vm).Retired = true
							want = 500
						case "foreign-ai":
							st.cowAIVM(vm).TeamID = recipient
							want = 500
						case "quota":
							for i := 0; i < maxSharesPerTeam; i++ {
								id := fmt.Sprintf("quota-%d", i)
								st.SnapShares[id] = &SnapShare{ID: id, TeamID: team, Grantee: recipient}
							}
							want = 403
						case "consent-removed":
							st.Teams[recipient].AcceptSharesFrom = nil
						case "persist":
							want = 500
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if change == "persist" {
						if err := e.srv.store.Close(); err != nil {
							t.Fatal(err)
						}
					}
				})
				if status != want {
					t.Errorf("share status %d want %d: %v", status, want, out)
				}
				if want != 201 {
					r21AssertUnchanged(t, e, before)
				}
				if share, ok := out["id"].(string); ok {
					if kind == "team" {
						code, imported := e.do(other, "POST", "/v1/snapshots/import-share", map[string]any{"share_id": share, "name": "recipient-copy"})
						if want != 201 && code == 201 {
							t.Errorf("stale authority minted usable grant: %v", imported)
						}
						if want == 201 {
							unconsented := change == "consent-removed" || change == "before-consent"
							if (unconsented && code != 404) || (!unconsented && code != 201) {
								t.Errorf("recipient consent/control: %d %v", code, imported)
							}
							if change == "before-consent" {
								if cSt, cOut := e.do(other, "PUT", "/v1/teams/"+recipient+"/share-allowlist", map[string]any{"teams": []string{team}}); cSt != 200 {
									t.Fatalf("late consent: %d %v", cSt, cOut)
								}
								if iSt, iOut := e.do(other, "POST", "/v1/snapshots/import-share", map[string]any{"share_id": share, "name": "recipient-copy"}); iSt != 201 {
									t.Errorf("share created before consent unusable after consent: %d %v", iSt, iOut)
								}
							}
						}
					} else if shareCode, ok := out["code"].(string); ok {
						code, data := e.raw("", "GET", "/v1/snapshot-links/"+shareCode+"/download", nil)
						if want != 201 && code == 200 {
							t.Error("stale authority minted usable code")
						}
						if want == 201 && (code != 200 || string(data) != "ARCHIVE:"+sid) {
							t.Errorf("valid code download: %d %q", code, data)
						}
					}
				}
				if want != 201 || status != 201 {
					return
				}
				for _, st := range []*State{e.srv.store.Snapshot(), r20Reload(t, e).store.Snapshot()} {
					if st.SnapShares[out["id"].(string)] == nil || st.VMTokens[vm].Gen != before.mem.VMTokens[vm].Gen+1 || st.AIVMs[vm].Gen != before.mem.AIVMs[vm].Gen+1 || st.RouteGens[vm] != before.mem.RouteGens[vm]+1 || len(st.PortMinters[vm]) != 0 {
						t.Error("share and credential/link revocation not durably committed together")
					}
				}
			})
		}
	}
}

func TestR21ShareRevocationDuringSnapshotLookup(t *testing.T) {
	for _, change := range []string{"delete-key", "scope", "role"} {
		t.Run(change, func(t *testing.T) {
			e := setup(t)
			e.srv.cfg.VMAPIURL = "https://boat.example"
			vm := e.createVM()
			e.boatd.mu.Lock()
			e.boatd.vms[vm]["state"] = "stopped"
			e.boatd.mu.Unlock()
			sid := e.snapshotOf(vm)
			team := e.teamID()
			if err := e.srv.store.Update(func(st *State) error {
				st.AIVMs[vm] = &AIVM{TeamID: team, Gen: 7}
				st.PortMinters[vm] = []string{selfKeyID(vm, st.VMTokens[vm].Gen)}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			token := e.newKey("admin")
			beforeCh := make(chan r21StateSnap, 1)
			want := 401
			var once sync.Once
			e.boatd.mu.Lock()
			e.boatd.snapListHook = func(id string) {
				if id != vm {
					return
				}
				once.Do(func() {
					want = r21ChangeAuthority(t, e, token, change)
					beforeCh <- r21CaptureState(t, e)
				})
			}
			e.boatd.mu.Unlock()
			status, out := e.do(token, "POST", "/v1/vms/"+vm+"/snapshots/"+sid+"/shares", map[string]any{})
			before := <-beforeCh
			if status != want {
				t.Errorf("revoked during upstream lookup: status %d want %d: %v", status, want, out)
			}
			r21AssertUnchanged(t, e, before)
		})
	}
}
