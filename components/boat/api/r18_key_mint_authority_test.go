package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Expect: 100-continue pauses the real handler after authentication and before
// decoding. No timing sleeps or production test hooks are needed.
func TestR18KeyMintCurrentAuthority(t *testing.T) {
	for _, change := range []string{"delete-key", "delete-user", "delete-team", "membership", "role", "scope", "expired", "rotate", "overlap-expired", "overlap", "unchanged", "expiry-cap", "quota", "persist", "previous-valid", "previous-expired", "current-with-expired-previous", "key-user-changed", "key-team-changed", "operator-allowed", "operator-revoked", "target-membership", "target-role", "default-scope", "default-narrow", "expiry-no-ttl", "nonoperator-cross-team", "unknown-scope", "escalation"} {
		t.Run(change, func(t *testing.T) {
			e := setup(t)
			token := e.newKey("exec")
			if change == "default-narrow" {
				token = e.newKey("exec", "vms:read")
			}
			if change == "role" || change == "target-role" {
				token = e.newKey("admin")
			}
			api := httptest.NewServer(e.srv)
			defer api.Close()
			id, _, _ := ParseToken(token)
			parent := e.srv.store.Snapshot().Keys[id]
			if strings.HasPrefix(change, "previous-") || change == "current-with-expired-previous" {
				status, out := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": 3600})
				if status != 200 {
					t.Fatalf("pre-rotate %d %v", status, out)
				}
				if change == "current-with-expired-previous" {
					token = out["token"].(string)
				}
			}
			target := ""
			if strings.HasPrefix(change, "operator-") || strings.HasPrefix(change, "target-") || change == "nonoperator-cross-team" {
				status, out := e.do(e.owner, "POST", "/v1/teams", map[string]any{"name": "second"})
				if status != 201 {
					t.Fatalf("second team %d %v", status, out)
				}
				target = out["id"].(string)
				if change == "nonoperator-cross-team" {
					if err := e.srv.store.Update(func(st *State) error { st.Users[parent.UserID].Operator = false; return nil }); err != nil {
						t.Fatal(err)
					}
				}
			}
			conn, err := net.Dial("tcp", strings.TrimPrefix(api.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			requestBody := map[string]any{"name": "child", "scopes": []string{"exec"}, "expires_in_s": 3600}
			if change == "role" || change == "target-role" {
				requestBody["scopes"] = []string{"admin"}
			}
			if target != "" {
				requestBody["team_id"] = target
			}
			if change == "default-scope" || change == "default-narrow" {
				delete(requestBody, "scopes")
			}
			if change == "expiry-no-ttl" {
				delete(requestBody, "expires_in_s")
			}
			if change == "unknown-scope" {
				requestBody["scopes"] = []string{"unknown"}
			}
			if change == "escalation" {
				requestBody["scopes"] = []string{"admin"}
			}
			encoded, err := json.Marshal(requestBody)
			if err != nil {
				t.Fatal(err)
			}
			body := string(encoded)
			fmt.Fprintf(conn, "POST /v1/keys HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nExpect: 100-continue\r\nContent-Length: %d\r\n\r\n", token, len(body))
			reader := bufio.NewReader(conn)
			interim, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatal(err)
			}
			interim.Body.Close()
			if interim.StatusCode != 100 {
				t.Fatalf("pause status %d", interim.StatusCode)
			}
			want := 401
			if change == "rotate" || change == "overlap" || change == "overlap-expired" {
				overlap := 0
				if change != "rotate" {
					overlap = 3600
				}
				status, out := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": overlap})
				if status != 200 {
					t.Fatalf("rotate %d %v", status, out)
				}
			}
			var cap time.Time
			if err := e.srv.store.Update(func(st *State) error {
				switch change {
				case "default-narrow":
					st.Keys[id].Scopes = []string{"exec"}
					want = 201
				case "expiry-no-ttl":
					cap = time.Now().Add(time.Minute)
					st.Keys[id].Expires = &cap
					want = 400
				case "nonoperator-cross-team", "escalation":
					want = 403
				case "unknown-scope":
					want = 400
				case "previous-valid", "current-with-expired-previous", "operator-allowed":
					want = 201
					if change == "current-with-expired-previous" {
						past := time.Now().Add(-time.Hour)
						st.Keys[id].PrevUntil = &past
					}
				case "previous-expired":
					past := time.Now().Add(-time.Hour)
					st.Keys[id].PrevUntil = &past
				case "key-user-changed":
					st.Keys[id].UserID = "other"
				case "key-team-changed":
					st.Keys[id].TeamID = "other"
				case "operator-revoked":
					st.Users[parent.UserID].Operator = false
					want = 403
				case "target-membership":
					st.Teams[target].Members = nil
					want = 404
				case "target-role":
					st.Teams[target].Members[0].Role = "member"
					want = 403
				case "default-scope":
					st.Keys[id].Scopes = []string{"vms:read"}
					want = 403
				case "delete-key":
					delete(st.Keys, id)
				case "delete-user":
					delete(st.Users, parent.UserID)
				case "delete-team":
					delete(st.Teams, parent.TeamID)
				case "membership":
					st.Teams[parent.TeamID].Members = nil
				case "role":
					st.Teams[parent.TeamID].Members[0].Role = "member"
					want = 403
				case "scope":
					st.Keys[id].Scopes = []string{"vms:read"}
					want = 403
				case "expired":
					past := time.Now().Add(-time.Hour)
					st.Keys[id].Expires = &past
				case "overlap-expired":
					past := time.Now().Add(-time.Hour)
					st.Keys[id].PrevUntil = &past
				case "expiry-cap":
					cap = time.Now().Add(time.Minute)
					st.Keys[id].Expires = &cap
					want = 201
				case "overlap", "unchanged":
					want = 201
				case "quota":
					want = 403
				case "persist":
					want = 500
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if change == "quota" {
				e.srv.cfg.MaxKeysPerTeam = len(e.srv.store.Snapshot().Keys)
			}
			if change == "persist" {
				if err := e.srv.store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before := e.srv.store.Snapshot()
			fmt.Fprint(conn, body)
			response, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			out, _ := io.ReadAll(response.Body)
			if response.StatusCode != want {
				t.Fatalf("mint status %d want %d: %s", response.StatusCode, want, out)
			}
			after := e.srv.store.Snapshot()
			if want != 201 && after != before {
				t.Fatal("rejected mint published state")
			}
			if want == 201 {
				if len(after.Keys) != len(before.Keys)+1 {
					t.Fatal("missing child")
				}
				for keyID, k := range after.Keys {
					if before.Keys[keyID] == nil && change == "default-narrow" && (len(k.Scopes) != 1 || k.Scopes[0] != "exec") {
						t.Fatalf("default child scopes %v", k.Scopes)
					}
					if before.Keys[keyID] == nil && change == "expiry-cap" && (k.Expires == nil || !k.Expires.Equal(cap)) {
						t.Fatalf("child expiry %v want %v", k.Expires, cap)
					}
				}
			}
		})
	}
}

// callNative intentionally forwards the principal without bearer headers.
func TestR18KeyMintHeaderlessCompatibility(t *testing.T) {
	for _, change := range []string{"allowed", "previous-valid", "previous-expired", "rotated", "self-token"} {
		t.Run(change, func(t *testing.T) {
			e := setup(t)
			token := e.newKey("exec")
			id, _, _ := ParseToken(token)
			if strings.HasPrefix(change, "previous-") {
				status, out := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": 3600})
				if status != 200 {
					t.Fatalf("rotate %d %v", status, out)
				}
			}
			p := e.srv.authenticate("Bearer " + token)
			if change == "self-token" {
				e.srv.cfg.VMAPIURL = "https://boat.example"
				vm := e.createVM()
				p = e.srv.authenticate("Bearer " + e.selfTokenOf(vm))
			}
			if p == nil {
				t.Fatal("authentication failed")
			}
			want := 201
			if change == "rotated" {
				status, out := e.do(e.owner, "POST", "/v1/keys/"+id+"/rotate", map[string]any{"overlap_s": 0})
				if status != 200 {
					t.Fatalf("rotate %d %v", status, out)
				}
				want = 401
			}
			if change == "previous-expired" {
				if err := e.srv.store.Update(func(st *State) error { past := time.Now().Add(-time.Hour); st.Keys[id].PrevUntil = &past; return nil }); err != nil {
					t.Fatal(err)
				}
				want = 401
			}
			if change == "self-token" {
				want = 404
			} // existing callNative selfAllowed gate
			r := httptest.NewRequest("POST", "/", nil)
			r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, &reqInfo{id: "r18-compat"}))
			before := len(e.srv.store.Snapshot().Keys)
			out := e.srv.callNative(r, p, "POST /v1/keys", e.srv.createKey, "", map[string]any{"name": "compat-child", "scopes": []string{"exec"}})
			if out.status != want {
				t.Fatalf("mint %d want %d: %s", out.status, want, out.body.String())
			}
			if want != 201 && len(e.srv.store.Snapshot().Keys) != before {
				t.Fatal("rejected compat mint published key")
			}
		})
	}
}
