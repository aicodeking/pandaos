package main

import (
	"net/url"
	"reflect"
	"sort"
	"testing"
)

func TestCompatListContractFiltersBeforePagination(t *testing.T) {
	e := setup(t)
	// No API stop operations are recorded: this also covers boatd auto-stops.
	fixtures := []struct{ name, state, updated string }{
		{"old", "stopped", "2026-10-01T00:00:00Z"},
		{"boundary", "stopped", "2026-10-02T00:00:00Z"},
		{"recent", "stopped", "2026-10-03T00:00:00Z"},
		{"running", "running", "2026-10-01T00:00:00Z"},
		{"unknown-time", "stopped", ""},
	}
	for _, f := range fixtures {
		bx := e.newSandbox(map[string]any{"name": f.name})
		id := e.nativeOf(bx)
		e.boatd.mu.Lock()
		e.boatd.vms[id]["state"] = f.state
		e.boatd.vms[id]["updated_at"] = f.updated
		e.boatd.vms[id]["created_at"] = "2026-09-01T00:00:00Z"
		e.boatd.vms[id]["expires_at"] = "2026-09-01T01:00:00Z" // auto-stop deadline is not data expiry
		e.boatd.mu.Unlock()
	}
	// A foreign stopped sandbox must never enter the filtered pages.
	e.boatd.mu.Lock()
	e.boatd.vms["foreign-filter"] = VM{"id": "foreign-filter", "name": "foreign", "team": "other", "state": "stopped", "updated_at": "2026-10-01T00:00:00Z"}
	e.boatd.mu.Unlock()
	for _, tc := range []struct {
		query string
		names []string
	}{
		{"stoppedBefore=2026-10-02T00:00:00Z", []string{"old"}},
		{"stoppedBefore=0001-01-01T00:00:00Z", []string{}},
		{"stoppedBefore=2026-10-02T02:00:00%2B02:00", []string{"old"}},
		{"stoppedBefore=2026-10-02T00:00:00.000000001Z", []string{"boundary", "old"}},
		{"stoppedBefore=2026-10-04T00:00:00Z&dataExpired=false", []string{"boundary", "old", "recent"}},
		{"dataExpired=true", []string{}},
		{"dataExpired=false&name=running", []string{"running"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got := []string{}
			cursor := ""
			for pages := 0; ; pages++ {
				if pages > len(fixtures) {
					t.Fatal("pagination did not finish")
				}
				st, out, _ := e.compat(e.owner, "GET", "/api/v1/sandboxes?limit=1&sort=asc&"+tc.query+"&cursor="+url.QueryEscape(cursor), nil, nil)
				if st != 200 {
					t.Fatalf("list: %d %v", st, out)
				}
				for _, raw := range out["sandboxes"].([]any) {
					got = append(got, raw.(map[string]any)["name"].(string))
				}
				page := out["pageInfo"].(map[string]any)
				if page["limit"] != float64(1) {
					t.Fatalf("limit: %v", page)
				}
				if page["hasMore"] == false {
					if page["nextCursor"] != nil {
						t.Fatalf("terminal cursor: %v", page)
					}
					break
				}
				cursor = page["nextCursor"].(string)
			}
			// IDs are random; compare the full filtered set, not its ID order.
			sort.Strings(got)
			if !reflect.DeepEqual(got, tc.names) {
				t.Fatalf("names: %v, want %v", got, tc.names)
			}
		})
	}
}

// A later edit must not move the stop boundary. Legacy records have no stopped_at.
func TestCompatStoppedAtUsesTransitionAndLegacyFallback(t *testing.T) {
	e := setup(t)
	for _, tc := range []struct {
		name    string
		stopped any
		present bool
		want    any
		match   bool
	}{
		{"transition", "2026-10-01T00:00:00Z", true, "2026-10-01T00:00:00Z", true},
		{"legacy", nil, false, "2026-10-03T00:00:00Z", false},
		{"invalid", "invalid", true, "invalid", false},
		{"empty", "", true, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bx := e.newSandbox(map[string]any{"name": tc.name})
			id := e.nativeOf(bx)
			e.boatd.mu.Lock()
			e.boatd.vms[id]["state"] = "stopped"
			e.boatd.vms[id]["updated_at"] = "2026-10-03T00:00:00Z"
			if tc.present {
				e.boatd.vms[id]["stopped_at"] = tc.stopped
			}
			e.boatd.mu.Unlock()
			if tc.name == "transition" {
				if st, vm := e.do(e.owner, "GET", "/v1/vms/"+id, nil); st != 200 || vm["stopped_at"] != tc.stopped {
					t.Fatalf("native stopped_at: %d %v", st, vm)
				}
			}
			st, out, _ := e.compat(e.owner, "GET", "/api/v1/sandboxes/"+bx, nil, nil)
			if st != 200 || out["sandbox"].(map[string]any)["stoppedAt"] != tc.want {
				t.Fatalf("stoppedAt: %d %v, want %v", st, out, tc.want)
			}
			st, out, _ = e.compat(e.owner, "GET", "/api/v1/sandboxes?name="+tc.name+"&stoppedBefore=2026-10-02T00:00:00Z", nil, nil)
			if st != 200 || (len(out["sandboxes"].([]any)) == 1) != tc.match {
				t.Fatalf("stoppedBefore: %d %v, want match %v", st, out, tc.match)
			}
		})
	}
}

func TestCompatListContractRejectsUnsupportedIncludesAndInvalidFilters(t *testing.T) {
	e := setup(t)
	for _, q := range []string{"include=cost", "include=deletedCost", "include=cost,deletedCost", "include=cost&include=deletedCost", "include=unknown", "include=unknown&include="} {
		st, out, _ := e.compat(e.owner, "GET", "/api/v1/sandboxes?"+q, nil, nil)
		if st != 501 || errCode(out) != "not_supported" || out["ok"] != false {
			t.Errorf("%s: %d %v", q, st, out)
		}
	}
	for _, q := range []string{"stoppedBefore=12345", "stoppedBefore=2026-10-02", "stoppedBefore=bad", "stoppedBefore=", "dataExpired=1", "dataExpired=TRUE", "dataExpired=bad", "dataExpired=", "dataExpired=true&dataExpired=false", "stoppedBefore=2026-10-02T00:00:00Z&stoppedBefore=2026-10-03T00:00:00Z"} {
		st, out, _ := e.compat(e.owner, "GET", "/api/v1/sandboxes?"+q, nil, nil)
		if st != 400 || errCode(out) != "invalid_query" {
			t.Errorf("%s: %d %v", q, st, out)
		}
	}
	if st, out, _ := e.compat(e.owner, "GET", "/api/v1/sandboxes?include=", nil, nil); st != 200 {
		t.Fatalf("empty include: %d %v", st, out)
	}
}

func TestCompatUnsupportedOperationSchemas(t *testing.T) {
	loadDriftSpec()
	for _, pattern := range []string{"POST /api/v1/sandboxes/{id}/share", "POST /api/v1/sandboxes/{id}/unshare", "GET /api/v1/sandboxes/{id}/usage"} {
		op, _ := operation(pattern)
		responses := asMap(op["responses"])
		if len(responses) != 2 || responses["501"] == nil || responses["default"] == nil {
			t.Errorf("%s must document only 501 and default errors: %v", pattern, responses)
		}
	}
}

func TestCompatUpgradeRequestSchema(t *testing.T) {
	loadDriftSpec()
	op, _ := operation("POST /api/v1/environments/{id}/upgrade")
	body := asMap(op["requestBody"])
	if body["required"] != false {
		t.Fatalf("upgrade request body must be optional: %v", body)
	}
	schema := asMap(asMap(asMap(body["content"])["application/json"])["schema"])
	if asMap(resolveRef(schema)["properties"])["agentIds"] == nil {
		t.Fatal("upgrade request schema must include agentIds")
	}
	for _, v := range []any{map[string]any{}, map[string]any{"agentIds": []any{}}, map[string]any{"agentIds": []any{"bx_23456789"}}} {
		if problems := validate(v, schema, "upgrade"); len(problems) != 0 {
			t.Errorf("valid upgrade request %v: %v", v, problems)
		}
	}
	if problems := validate(map[string]any{"agentIds": "bx_23456789"}, schema, "upgrade"); len(problems) == 0 {
		t.Fatal("upgrade agentIds must be an array")
	}
}

func TestCompatUpgradeOptionalRequestBody(t *testing.T) {
	e := setup(t)
	st, env := e.do(e.owner, "POST", "/v1/environments", map[string]any{"name": "upgrade-body"})
	if st != 201 {
		t.Fatalf("create environment: %d %v", st, env)
	}
	for _, base := range []string{"/v1", "/api/v1"} {
		for _, body := range []any{nil, map[string]any{"agentIds": []string{}}} {
			st, out := e.do(e.owner, "POST", base+"/environments/"+str(env, "id")+"/upgrade", body)
			if st != 200 || out["upgraded"] != float64(0) {
				t.Fatalf("%s upgrade %v: %d %v", base, body, st, out)
			}
		}
	}
}
