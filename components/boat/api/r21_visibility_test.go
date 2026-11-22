package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestR21VisibilityRejectedBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		visibility any
		code       string
		extra      map[string]any
	}{
		{name: "private", visibility: "private", code: "invalid_request"},
		{name: "private_batch", visibility: "private", code: "invalid_request", extra: map[string]any{"count": 2}},
		{name: "private_snapshot", visibility: "private", code: "invalid_request", extra: map[string]any{"from": "missing-snapshot"}},
		{name: "private_files", visibility: "private", code: "invalid_request", extra: map[string]any{"files": []any{map[string]any{"path": "secret.txt", "content": "secret"}}}},
		{name: "private_setup_script", visibility: "private", code: "invalid_request", extra: map[string]any{"setupScript": "echo hi"}},
		{name: "private_hosted_ports", visibility: "private", code: "invalid_request", extra: map[string]any{"hostedPorts": []any{map[string]any{"port": 8080, "public": true}}}},
		{name: "public", visibility: "public", code: "invalid_request"},
		{name: "team", visibility: "team", code: "invalid_request"},
		{name: "unknown", visibility: "unsupported", code: "invalid_request"},
		{name: "case_private", visibility: "PRIVATE", code: "invalid_request"},
		{name: "case_shared", visibility: "SHARED", code: "invalid_request"},
		{name: "whitespace", visibility: " shared ", code: "invalid_request"},
		{name: "number", visibility: 123, code: "invalid_json"},
		{name: "boolean", visibility: true, code: "invalid_json"},
		{name: "object", visibility: map[string]any{}, code: "invalid_json"},
		{name: "array", visibility: []any{"shared"}, code: "invalid_json"},
	} {
		for _, key := range []string{"", "visibility-key"} {
			t.Run(tc.name+"/key="+key, func(t *testing.T) {
				e := setup(t)
				// Rejection must precede even idempotency's expired-record purge.
				if err := e.srv.store.Update(func(st *State) error {
					st.Idempotency["expired"] = &IdemRecord{At: time.Now().Add(-2 * idemTTL)}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				before := e.srv.store.Snapshot()
				beforeBytes, err := json.Marshal(before)
				if err != nil {
					t.Fatal(err)
				}
				var calls atomic.Int64
				e.srv.boatd.hc.Transport = copyRoundTrip(func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					return http.DefaultTransport.RoundTrip(r)
				})
				body := map[string]any{"visibility": tc.visibility}
				for k, v := range tc.extra {
					body[k] = v
				}
				hdr := map[string]string{}
				if key != "" {
					hdr["Idempotency-Key"] = key
				}
				st, out, respHdr := e.compat(e.owner, "POST", "/api/v1/sandboxes", body, hdr)
				errObj, _ := out["error"].(map[string]any)
				msg, _ := out["message"].(string)
				if st != http.StatusBadRequest || out["ok"] != false || out["type"] != "sandbox.error" || errCode(out) != tc.code ||
					errObj["code"] != tc.code || errObj["status"] != float64(http.StatusBadRequest) || errObj["message"] != msg ||
					!strings.HasPrefix(str(out, "requestId"), "req_") || respHdr.Get("Idempotent-Replayed") != "" {
					t.Errorf("unsupported visibility: %d %v", st, out)
				}
				if tc.code == "invalid_request" && (!strings.Contains(msg, "visibility") || !strings.Contains(msg, "not supported") || !strings.Contains(msg, "whole team")) {
					t.Errorf("unexpected error message %q", msg)
				}
				if n := calls.Load(); n != 0 {
					t.Errorf("rejected visibility made %d boatd requests", n)
				}
				e.boatd.mu.Lock()
				creates, vms := len(e.boatd.creates), len(e.boatd.vms)
				e.boatd.mu.Unlock()
				if creates != 0 || vms != 0 {
					t.Errorf("rejected visibility mutated boatd: creates=%d vms=%d", creates, vms)
				}
				after := e.srv.store.Snapshot()
				afterBytes, err := json.Marshal(after)
				if err != nil {
					t.Fatal(err)
				}
				if after != before || !bytes.Equal(beforeBytes, afterBytes) {
					t.Error("rejected visibility changed the in-memory API store")
				}
				if len(after.CompatIDs) != 0 || len(after.VMTokens) != 0 || len(after.CompatSetups) != 0 || after.Idempotency["expired"] == nil {
					t.Errorf("unexpected store state after rejected visibility: compatIDs=%d vmTokens=%d setups=%d expired=%v",
						len(after.CompatIDs), len(after.VMTokens), len(after.CompatSetups), after.Idempotency["expired"])
				}
				disk, err := OpenStore(e.dir)
				if err != nil {
					t.Fatal(err)
				}
				defer disk.Close()
				diskBytes, err := json.Marshal(disk.Snapshot())
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(beforeBytes, diskBytes) {
					t.Error("rejected visibility changed durable store on disk")
				}
				e.srv.idem.mu.Lock()
				pending := len(e.srv.idem.pending)
				e.srv.idem.mu.Unlock()
				if pending != 0 {
					t.Error("rejected visibility left an idempotency claim")
				}
			})
		}
	}
}

func compatDirect(t *testing.T, baseURL, token, method, path string, body any, hdr map[string]string) (int, map[string]any, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return resp.StatusCode, out, resp.Header
}

func TestR21VisibilitySupportedKeepsTeamAccess(t *testing.T) {
	for _, tc := range []struct {
		name       string
		visibility any
		rawSrv     bool // "" and null are accepted by the handler though outside OpenAPI's request enum
	}{
		{name: "omitted"},
		{name: "shared", visibility: "shared"},
		{name: "empty", visibility: "", rawSrv: true},
		{name: "null", visibility: nil, rawSrv: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			targetURL := e.api.URL
			if tc.rawSrv {
				raw := httptest.NewServer(e.srv)
				defer raw.Close()
				targetURL = raw.URL
			}
			member, _ := e.addMember("member")
			other := otherTeamKey(e)
			hdr := map[string]string{"Idempotency-Key": "visibility-key"}
			if st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"visibility": "private"}, hdr); st != http.StatusBadRequest {
				t.Fatalf("private request: %d %v", st, out)
			}
			body := map[string]any{"name": "team-sandbox", "type": "small"}
			if tc.name != "omitted" {
				body["visibility"] = tc.visibility
			}
			st, out, _ := compatDirect(t, targetURL, e.owner, "POST", "/api/v1/sandboxes", body, hdr)
			if st != http.StatusAccepted || out["ok"] != true || out["type"] != "sandbox.created" {
				t.Fatalf("supported visibility: %d %v", st, out)
			}
			id := out["sandbox"].(map[string]any)["id"].(string)
			if st, out, _ := e.compat(member, "GET", "/api/v1/sandboxes/"+id, nil, nil); st != http.StatusOK || out["sandbox"].(map[string]any)["id"] != id {
				t.Errorf("same-team access: %d %v", st, out)
			}
			if st, listOut, _ := e.compat(member, "GET", "/api/v1/sandboxes", nil, nil); st != http.StatusOK || len(listOut["sandboxes"].([]any)) != 1 {
				t.Errorf("same-team list access: %d %v", st, listOut)
			}
			if st, out, _ := e.compat(other, "GET", "/api/v1/sandboxes/"+id, nil, nil); st != http.StatusNotFound {
				t.Errorf("other-team access: %d %v", st, out)
			}
			if st, out, h := compatDirect(t, targetURL, e.owner, "POST", "/api/v1/sandboxes", body, hdr); st != http.StatusAccepted || h.Get("Idempotent-Replayed") != "true" || out["sandbox"].(map[string]any)["id"] != id {
				t.Errorf("supported visibility replay: %d %v", st, out)
			}
			e.boatd.mu.Lock()
			defer e.boatd.mu.Unlock()
			if len(e.boatd.creates) != 1 || len(e.boatd.vms) != 1 {
				t.Errorf("supported visibility created %d times, %d VMs", len(e.boatd.creates), len(e.boatd.vms))
			}
		})
	}
}

func TestR21VisibilityIdempotencyRetriesAndNonPoisoning(t *testing.T) {
	e := setup(t)

	// Repeated retries with the same key and unsupported visibility must consistently
	// return 400 invalid_request without poisoning the key or mutating state.
	sameKey := map[string]string{"Idempotency-Key": "retry-private-key"}
	for i := 0; i < 3; i++ {
		st, out, hdr := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "priv", "visibility": "private"}, sameKey)
		if st != http.StatusBadRequest || errCode(out) != "invalid_request" || hdr.Get("Idempotent-Replayed") != "" {
			t.Fatalf("retry %d: %d %v", i, st, out)
		}
	}
	if st, out, hdr := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "priv", "visibility": "public"}, sameKey); st != http.StatusBadRequest || errCode(out) != "invalid_request" || hdr.Get("Idempotent-Replayed") != "" {
		t.Fatalf("different invalid visibility on same key: %d %v", st, out)
	}
	e.boatd.mu.Lock()
	if len(e.boatd.creates) != 0 || len(e.boatd.vms) != 0 {
		e.boatd.mu.Unlock()
		t.Fatalf("rejected retries created VMs: creates=%d vms=%d", len(e.boatd.creates), len(e.boatd.vms))
	}
	e.boatd.mu.Unlock()

	// Valid request with a fresh key succeeds cleanly.
	freshKey := map[string]string{"Idempotency-Key": "fresh-valid-key"}
	validBody := map[string]any{"name": "shared-one", "type": "small", "visibility": "shared"}
	st, out, hdr := e.compat(e.owner, "POST", "/api/v1/sandboxes", validBody, freshKey)
	if st != http.StatusAccepted || out["ok"] != true || hdr.Get("Idempotent-Replayed") != "" {
		t.Fatalf("valid create with new key: %d %v", st, out)
	}
	firstID := out["sandbox"].(map[string]any)["id"].(string)

	// Sending a private request against an already cached key is rejected with 400
	// invalid_request and preserves the cached valid response for replay.
	if st, out, hdr := e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "shared-one", "type": "small", "visibility": "private"}, freshKey); st != http.StatusBadRequest || errCode(out) != "invalid_request" || hdr.Get("Idempotent-Replayed") != "" {
		t.Fatalf("private request on cached key: %d %v", st, out)
	}
	if st, out, hdr := e.compat(e.owner, "POST", "/api/v1/sandboxes", validBody, freshKey); st != http.StatusAccepted || hdr.Get("Idempotent-Replayed") != "true" || out["sandbox"].(map[string]any)["id"] != firstID {
		t.Fatalf("replay after rejected private request: %d %v", st, out)
	}

	// Previously rejected retry-private-key was never poisoned, so a valid request can still use it.
	st, out, hdr = e.compat(e.owner, "POST", "/api/v1/sandboxes", map[string]any{"name": "shared-two", "type": "small"}, sameKey)
	if st != http.StatusAccepted || out["ok"] != true || hdr.Get("Idempotent-Replayed") != "" {
		t.Fatalf("valid create reusing previously rejected key: %d %v", st, out)
	}
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	if len(e.boatd.creates) != 2 || len(e.boatd.vms) != 2 {
		t.Errorf("expected 2 creates and 2 VMs, got creates=%d vms=%d", len(e.boatd.creates), len(e.boatd.vms))
	}
}
