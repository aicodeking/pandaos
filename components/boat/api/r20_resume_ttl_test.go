package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestR20CompatLifecycleTTLRejectedBeforeSideEffects(t *testing.T) {
	for _, action := range []string{"resume", "fork"} {
		for _, state := range []string{"running", "stopped"} {
			t.Run(action+"/"+state, func(t *testing.T) {
				e := setup(t)
				e.srv.cfg.VMAPIURL = "https://boat.example"
				bx := e.newSandbox(map[string]any{"ttlSeconds": 600, "type": "small"})
				id, base := e.nativeOf(bx), "/api/v1/sandboxes/"+bx
				if state == "stopped" {
					e.ok("POST", base+"/stop", nil, 202)
				}
				// A rejected resume must not supersede an outstanding stop.
				op := e.srv.stops.begin(e.teamID(), id)
				e.srv.stops.fail(id, op, "test stop failed")
				token := e.selfTokenOf(id)
				if err := e.srv.store.Flush(); err != nil {
					t.Fatal(err)
				}
				beforeStore, err := json.Marshal(e.srv.store.Snapshot())
				if err != nil {
					t.Fatal(err)
				}
				snapshot := func() []byte {
					t.Helper()
					e.boatd.mu.Lock()
					defer e.boatd.mu.Unlock()
					b, err := json.Marshal(map[string]any{
						"vms": e.boatd.vms, "starts": e.boatd.starts, "stops": e.boatd.stops,
						"forks": e.boatd.forks, "resizes": e.boatd.resizes, "patches": e.boatd.patches,
						"envOps": e.boatd.envOps, "state": e.srv.store.Snapshot(),
					})
					if err != nil {
						t.Fatal(err)
					}
					return b
				}
				before := snapshot()
				for _, tc := range []struct {
					name, body, code, message string
				}{
					{"zero", `{"ttlSeconds":0}`, "invalid_request", "must be 1-2592000"},
					{"negative", `{"ttlSeconds":-1}`, "invalid_request", "must be 1-2592000"},
					{"too_large", `{"ttlSeconds":2592001}`, "invalid_request", "must be 1-2592000"},
					{"fraction", `{"ttlSeconds":1.5}`, "invalid_request", "must be 1-2592000"},
					{"exponent", `{"ttlSeconds":1e3}`, "invalid_request", "must be 1-2592000"},
					{"string", `{"ttlSeconds":"7200"}`, "invalid_request", "must be 1-2592000"},
					{"boolean", `{"ttlSeconds":true}`, "invalid_request", "must be 1-2592000"},
					{"object", `{"ttlSeconds":{}}`, "invalid_request", "must be 1-2592000"},
					{"array", `{"ttlSeconds":[]}`, "invalid_request", "must be 1-2592000"},
					{"malformed", `{"ttlSeconds":}`, "invalid_json", ""},
					{"truncated", `{"ttlSeconds":`, "invalid_json", ""},
				} {
					t.Run(tc.name, func(t *testing.T) {
						body := tc.body
						if tc.code != "invalid_json" {
							body = strings.TrimSuffix(body, "}") + `,"type":"large","failFast":true}`
						}
						r, err := http.NewRequest("POST", e.api.URL+base+"/"+action, strings.NewReader(body))
						if err != nil {
							t.Fatal(err)
						}
						r.Header.Set("Authorization", "Bearer "+e.owner)
						r.Header.Set("Idempotency-Key", "r20-ttl-"+tc.name)
						resp, err := http.DefaultClient.Do(r)
						if err != nil {
							t.Fatal(err)
						}
						defer resp.Body.Close()
						var out map[string]any
						if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
							t.Fatal(err)
						}
						if resp.StatusCode != 400 || out["ok"] != false || out["type"] != "sandbox.error" ||
							errCode(out) != tc.code || !strings.Contains(str(out, "message"), tc.message) {
							t.Errorf("TTL rejection: %d %v", resp.StatusCode, out)
						}
						if !bytes.Equal(before, snapshot()) {
							t.Error("rejected TTL changed VM, lifecycle calls, or stored operations/credentials")
						}
					})
				}
				sb := e.ok("GET", base, nil, 200)["sandbox"].(map[string]any)
				if sb["ttlSeconds"] != float64(600) {
					t.Errorf("rejected TTL changed persisted VM TTL: %v", sb["ttlSeconds"])
				}
				if st, _ := e.do(token, "GET", vmPath(id), nil); st != 200 {
					t.Errorf("rejected TTL revoked the existing guest token: %d", st)
				}
				back, err := OpenStore(e.dir)
				if err != nil {
					t.Fatal(err)
				}
				defer back.Close()
				persisted, err := json.Marshal(back.Snapshot())
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(beforeStore, persisted) {
					t.Error("rejected TTL changed durable operations or credential generations")
				}
			})
		}
	}
}

func TestR20CompatLifecycleWithoutTTL(t *testing.T) {
	for _, action := range []string{"resume", "fork"} {
		for _, failFast := range []bool{false, true} {
			name := action
			if failFast {
				name += "/fail_fast"
			}
			t.Run(name, func(t *testing.T) {
				e := setup(t)
				e.srv.cfg.VMAPIURL = "https://boat.example"
				bx := e.newSandbox(map[string]any{"ttlSeconds": 600})
				id, base := e.nativeOf(bx), "/api/v1/sandboxes/"+bx
				if action == "resume" {
					e.ok("POST", base+"/stop", nil, 202)
				}
				oldToken := e.selfTokenOf(id)
				out := e.ok("POST", base+"/"+action, map[string]any{"failFast": failFast}, 202)
				returned := out["sandbox"].(map[string]any)
				// r46: a fork without a ttl gets the default hour, not its parent's lifetime.
				wantTTL := float64(600)
				if action == "fork" {
					wantTTL = 3600
				}
				if returned["ttlSeconds"] != wantTTL {
					t.Errorf("absent TTL changed the returned target TTL: %v", returned["ttlSeconds"])
				}
				target := e.ok("GET", "/api/v1/sandboxes/"+out["id"].(string), nil, 200)["sandbox"].(map[string]any)
				if target["ttlSeconds"] != wantTTL {
					t.Errorf("absent TTL changed the resolved target TTL: %v", target["ttlSeconds"])
				}
				sb := e.ok("GET", base, nil, 200)["sandbox"].(map[string]any)
				if sb["ttlSeconds"] != float64(600) {
					t.Errorf("absent TTL changed the source TTL: %v", sb["ttlSeconds"])
				}
				if (out["id"] == bx) != (action == "resume") {
					t.Errorf("unexpected lifecycle target: %v", out["id"])
				}
				if st, _ := e.do(oldToken, "GET", vmPath(id), nil); st != 401 {
					t.Errorf("successful lifecycle did not revoke the previous guest token: %d", st)
				}
				e.boatd.mu.Lock()
				defer e.boatd.mu.Unlock()
				calls := e.boatd.starts
				if action == "fork" {
					calls = e.boatd.forks
				}
				if len(calls) != 1 || (calls[0]["fail_fast"] == true) != failFast {
					t.Errorf("failFast was not forwarded: %v", calls)
				}
				if len(e.boatd.patches) != 0 {
					t.Error("absent TTL unexpectedly patched a VM")
				}
			})
		}
	}
}

func TestR20CompatLifecycleAtomicTTL(t *testing.T) {
	for _, action := range []string{"resume", "fork"} {
		for _, tc := range []struct {
			name   string
			req    any
			want   any // float64 or nil ("infinite" / null)
			ttlS   float64
			rawSrv bool // "infinite" string alias is accepted though OpenAPI uses integer | null
		}{
			{"requested", 7200, float64(7200), 7200, false},
			{"unchanged", 600, float64(600), 600, false},
			{"minimum", 1, float64(1), 1, false},
			{"maximum", 2592000, float64(2592000), 2592000, false},
			{"null", nil, nil, 0, false},
			{"infinite_alias", "infinite", nil, 0, true},
		} {
			t.Run(action+"/"+tc.name, func(t *testing.T) {
				e := setup(t)
				e.srv.cfg.VMAPIURL = "https://boat.example"
				bx := e.newSandbox(map[string]any{"ttlSeconds": 600, "type": "small"})
				base := "/api/v1/sandboxes/" + bx
				if action == "resume" {
					e.ok("POST", base+"/stop", nil, 202)
				}
				var out map[string]any
				if tc.rawSrv {
					b, _ := json.Marshal(map[string]any{"ttlSeconds": tc.req, "failFast": true})
					req := httptest.NewRequest("POST", base+"/"+action, bytes.NewReader(b))
					req.Header.Set("Authorization", "Bearer "+e.owner)
					req.Header.Set("Content-Type", "application/json")
					rec := httptest.NewRecorder()
					e.srv.ServeHTTP(rec, req)
					if rec.Code != 202 {
						t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
						t.Fatal(err)
					}
				} else {
					out = e.ok("POST", base+"/"+action, map[string]any{"ttlSeconds": tc.req, "failFast": true}, 202)
				}
				returned := out["sandbox"].(map[string]any)
				if returned["ttlSeconds"] != tc.want {
					t.Errorf("returned ttlSeconds = %v, want %v", returned["ttlSeconds"], tc.want)
				}
				if (returned["archiveAfter"] == nil) != (tc.want == nil) {
					t.Errorf("returned archiveAfter = %v for want=%v", returned["archiveAfter"], tc.want)
				}
				target := e.ok("GET", "/api/v1/sandboxes/"+out["id"].(string), nil, 200)["sandbox"].(map[string]any)
				if target["ttlSeconds"] != tc.want {
					t.Errorf("target ttlSeconds = %v, want %v", target["ttlSeconds"], tc.want)
				}
				if action == "fork" {
					parent := e.ok("GET", base, nil, 200)["sandbox"].(map[string]any)
					if parent["ttlSeconds"] != float64(600) {
						t.Errorf("fork changed parent ttlSeconds = %v, want 600", parent["ttlSeconds"])
					}
				}
				e.boatd.mu.Lock()
				defer e.boatd.mu.Unlock()
				calls := e.boatd.starts
				if action == "fork" {
					calls = e.boatd.forks
				}
				if len(calls) != 1 || calls[0]["ttl_s"] != tc.ttlS {
					t.Errorf("atomic ttl_s not forwarded on %s: %v", action, calls)
				}
				if len(e.boatd.patches) != 0 {
					t.Errorf("expected atomic TTL without separate PATCH, got patches=%v", e.boatd.patches)
				}
			})
		}
	}
}
