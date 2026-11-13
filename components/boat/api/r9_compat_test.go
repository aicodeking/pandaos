package main

import (
	"strings"
	"testing"
)

func init() {
	for p, what := range compatUnsupported {
		noCoverage[p] = "always 501 not_supported: " + what + " (TestCompatUnsupportedAre501)"
	}
	for _, p := range []string{"POST /api/v1/credential-profiles", "PUT /api/v1/credential-profiles/{profile}/custom-apis/{name}",
		"DELETE /api/v1/credential-profiles/{profile}/custom-apis/{name}"} {
		noCoverage[p] = "always 501 not_supported: one profile per team, no custom APIs (TestCompatCredentialProfiles)"
	}
	noCoverage["DELETE /api/v1/credential-profiles/{profile}"] = "always 409 is_default: the team's one profile (TestCompatCredentialProfiles)"
}

// boat.dev's credential profiles map onto /v1/ai/credentials: one "default"
// profile per team, one account per credential.
func TestCompatCredentialProfiles(t *testing.T) {
	e := setup(t)
	const base = "/api/v1/credential-profiles"
	st, out, _ := e.compat(e.owner, "GET", base, nil, nil)
	if st != 200 || out["type"] != "credential_profile.list" {
		t.Fatalf("list: %d %v", st, out)
	}
	prof := out["profiles"].([]any)[0].(map[string]any)
	if prof["name"] != "default" || prof["isDefault"] != true || len(prof["connected"].([]any)) != 0 {
		t.Errorf("profile: %v", prof)
	}
	st, out, _ = e.compat(e.owner, "PUT", base+"/default/keys/anthropic-api-key", map[string]any{"value": "sk-ant-REAL-KEY-123456"}, nil)
	if st != 200 || strings.Join(toStrings(out["profile"].(map[string]any)["connected"]), ",") != "anthropic-api-key" {
		t.Fatalf("set key: %d %v", st, out)
	}
	if st, out := e.do(e.owner, "GET", "/v1/ai/credentials", nil); st != 200 || len(out["credentials"].([]any)) != 1 {
		t.Errorf("native store: %d %v", st, out)
	}
	// the profile id (the team id) works as well as its name
	st, out, _ = e.compat(e.owner, "GET", base+"/"+e.teamID()+"/accounts/anthropic-api-key", nil, nil)
	if st != 200 || len(out["accounts"].([]any)) != 1 || out["accounts"].([]any)[0].(map[string]any)["label"] != "…3456" {
		t.Fatalf("accounts: %d %v", st, out)
	}
	if st, out, _ := e.compat(e.owner, "PUT", base+"/default/accounts/anthropic-api-key/anthropic-api-key/active", nil, nil); st != 200 {
		t.Errorf("switch to the account in use: %d %v", st, out)
	}
	if st, _, _ := e.compat(e.owner, "PUT", base+"/default/accounts/anthropic-api-key/other/active", nil, nil); st != 404 {
		t.Errorf("switch to an unknown account: %d", st)
	}
	if st, out, _ := e.compat(e.owner, "POST", base+"/default/accounts/anthropic-api-key", map[string]any{"value": "sk-ant-OTHER-KEY-9999"}, nil); st != 501 || errCode(out) != "not_supported" {
		t.Errorf("second account: %d %v", st, out)
	}
	if st, out, _ := e.compat(e.owner, "PUT", base+"/default", map[string]any{"isDefault": true}, nil); st != 200 || out["type"] != "credential_profile.updated" {
		t.Errorf("update: %d %v", st, out)
	}
	for _, c := range []struct {
		method, path string
		body         any
		status       int
		code         string
	}{
		{"POST", base, map[string]any{"name": "work"}, 501, "not_supported"},
		{"PUT", base + "/default", map[string]any{"name": "work"}, 501, "not_supported"},
		{"DELETE", base + "/default", nil, 409, "is_default"},
		{"PUT", base + "/work/keys/openai-api-key", map[string]any{"value": "sk-12345678"}, 404, "profile_not_found"},
		{"PUT", base + "/default/keys/claude-subscription", map[string]any{"value": "sk-12345678"}, 501, "not_supported"},
		{"PUT", base + "/default/keys/nope", map[string]any{"value": "sk-12345678"}, 400, "invalid_request"},
		{"PUT", base + "/default/custom-apis/STRIPE_KEY", nil, 501, "not_supported"},
		{"DELETE", base + "/default/custom-apis/STRIPE_KEY", nil, 501, "not_supported"},
	} {
		if st, out, _ := e.compat(e.owner, c.method, c.path, c.body, nil); st != c.status || errCode(out) != c.code {
			t.Errorf("%s %s: %d %v, want %d %s", c.method, c.path, st, out, c.status, c.code)
		}
	}
	st, out, _ = e.compat(e.owner, "DELETE", base+"/default/accounts/anthropic-api-key/anthropic-api-key", nil, nil)
	if st != 200 || len(out["accounts"].([]any)) != 0 {
		t.Fatalf("delete account: %d %v", st, out)
	}
	e.compat(e.owner, "PUT", base+"/default/keys/openai-api-key", map[string]any{"value": "sk-openai-12345678"}, nil)
	if st, out, _ := e.compat(e.owner, "POST", base+"/default/accounts/xai-api-key", map[string]any{"value": "xai-12345678"}, nil); st != 201 || len(out["accounts"].([]any)) != 1 {
		t.Errorf("first account: %d %v", st, out)
	}
	st, out, _ = e.compat(e.owner, "DELETE", base+"/default/keys/openai-api-key", nil, nil)
	if st != 200 || strings.Join(toStrings(out["profile"].(map[string]any)["connected"]), ",") != "xai-api-key" {
		t.Errorf("delete key: %d %v", st, out)
	}
	// needs admin, like /v1/ai/credentials
	if st, _, _ := e.compat(e.newKey("vms:read"), "GET", base, nil, nil); st != 403 {
		t.Errorf("non-admin list: %d", st)
	}
}

func toStrings(v any) []string {
	out := []string{}
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// GET /api/v1/snapshots/{id}/download answers boat.dev's JSON: one chunk
// whose signed URL streams the native archive with no API key.
func TestCompatSnapshotDownload(t *testing.T) {
	e := setup(t)
	id := e.createVM()
	sid := e.snapshotOf(id)
	for _, ref := range []string{snapshotUUID(e.teamID(), sid), sid} {
		st, out, _ := e.compat(e.owner, "GET", "/api/v1/snapshots/"+ref+"/download", nil, nil)
		chunks, _ := out["chunks"].([]any)
		if st != 200 || out["snapshotId"] != snapshotUUID(e.teamID(), sid) || len(chunks) != 1 {
			t.Fatalf("download %s: %d %v", ref, st, out)
		}
		u := chunks[0].(map[string]any)["signedUrl"].(string)
		path := u[strings.Index(u, fileDownloadPath):]
		if st, b := e.raw("", "GET", path, nil); st != 200 || string(b) != "ARCHIVE:"+sid {
			t.Errorf("chunk %s: %d %q", ref, st, b)
		}
		if st, _ := e.raw("", "GET", path+"x", nil); st != 404 {
			t.Errorf("tampered chunk URL: %d", st)
		}
	}
	if st, _, _ := e.compat(otherTeamKey(e), "GET", "/api/v1/snapshots/"+snapshotUUID(e.teamID(), sid)+"/download", nil, nil); st != 404 {
		t.Errorf("other team: %d", st)
	}
}

// Every boat.dev operation without an equivalent answers 501 not_supported,
// with a message, instead of a bare 404.
func TestCompatUnsupportedAre501(t *testing.T) {
	e := setup(t)
	for pattern := range compatUnsupported {
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.NewReplacer("{id}", "env1", "{jobId}", "job1", "{repositoryId}", "repo1", "{org}", "org1", "{sid}", "snap1").Replace(path)
		st, out, _ := e.compat(e.owner, method, path, nil, nil)
		if st != 501 || errCode(out) != "not_supported" || !strings.Contains(out["message"].(string), "not supported") {
			t.Errorf("%s %s: %d %v", method, path, st, out)
		}
	}
}
