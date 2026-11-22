package main

import (
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func historyPageFixture(t *testing.T, count int) (*env, string, string) {
	t.Helper()
	e := setup(t)
	bx := e.newSandbox(nil)
	vm := e.nativeOf(bx)
	e.ok("POST", "/api/v1/sandboxes/"+bx+"/stop", nil, 202)
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := e.srv.store.Update(func(st *State) error {
		for i := 0; i < count; i++ {
			st.Commands[vm] = append(st.Commands[vm], CommandRecord{
				ID: fmt.Sprintf("c_%03d", i), Cmd: []string{"bash", "-lc", fmt.Sprintf("echo task %03d", i)},
				Kind: "command", StartedAt: started.Add(time.Duration(i) * time.Second), State: "exited",
			})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return e, bx, vm
}

func historyPageIDs(t *testing.T, out map[string]any, limit int, more bool) []string {
	t.Helper()
	if _, exists := out["pageInfo"]; exists {
		t.Fatal("unexpected pageInfo extension")
	}
	cursor, exists := out["nextCursor"]
	if !exists {
		t.Fatal("missing nextCursor")
	}
	if more {
		if value, ok := cursor.(string); !ok || value == "" {
			t.Fatalf("continuation cursor = %v, want nonempty string", cursor)
		}
	} else if cursor != nil {
		t.Fatalf("terminal nextCursor = %v, want null", cursor)
	}
	rows := out["commands"].([]any)
	if len(rows) > limit {
		t.Fatalf("page returned %d > %d rows", len(rows), limit)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.(map[string]any)["commandId"].(string))
	}
	return ids
}

func TestCompatHistoryDefaultAndBoundedPages(t *testing.T) {
	e, bx, _ := historyPageFixture(t, 125)
	base := "/api/v1/sandboxes/" + bx + "/command-history"
	out := e.ok("GET", base, nil, 200)
	ids := historyPageIDs(t, out, 20, true)
	if len(ids) != 20 || ids[0] != "c_124" || ids[19] != "c_105" {
		t.Fatalf("default page = %v", ids)
	}
	out = e.ok("GET", base+"?limit=100", nil, 200)
	ids = historyPageIDs(t, out, 100, true)
	out = e.ok("GET", base+"?limit=100&cursor="+url.QueryEscape(out["nextCursor"].(string)), nil, 200)
	ids = append(ids, historyPageIDs(t, out, 100, false)...)
	want := make([]string, 125)
	for i := range want {
		want[i] = fmt.Sprintf("c_%03d", 124-i)
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("traversal = %v", ids)
	}
}

func TestCompatHistoryFilters(t *testing.T) {
	e, bx, vm := historyPageFixture(t, 6)
	if err := e.srv.store.Update(func(st *State) error {
		st.Commands[vm][1].Kind = "prompt"
		st.Commands[vm][3].Kind = "setup"
		st.Commands[vm][5].Cmd = []string{"printf", "Task", "005"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/sandboxes/" + bx + "/command-history"
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"mode=process", []string{"c_003", "c_001"}},
		{"mode=detached", []string{"c_005", "c_004", "c_002", "c_000"}},
		{"q=TASK+005", []string{"c_005"}},
		{"mode=process&q=task+003", []string{"c_003"}},
		{"q=not-present", []string{}},
		{"mode=sync", []string{}},
		{"mode=stream", []string{}},
		{"mode=commands", []string{"c_005", "c_004", "c_002", "c_000"}},
		{"mode=sync,detached", []string{"c_005", "c_004", "c_002", "c_000"}},
		{"mode=process,detached", []string{"c_005", "c_004", "c_003", "c_002", "c_001", "c_000"}},
		{"mode=detached,detached", []string{"c_005", "c_004", "c_002", "c_000"}},
		{"q=" + url.QueryEscape(strings.Repeat("界", 200)), []string{}},
	} {
		t.Run(tc.query[:min(len(tc.query), 45)], func(t *testing.T) {
			out := e.ok("GET", base+"?"+tc.query, nil, 200)
			if ids := historyPageIDs(t, out, 20, false); !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("filter %s = %v, want %v", tc.query, ids, tc.want)
			}
		})
	}
	out := e.ok("GET", base+"?mode=process&limit=1", nil, 200)
	if ids := historyPageIDs(t, out, 1, true); !reflect.DeepEqual(ids, []string{"c_003"}) {
		t.Fatal(ids)
	}
	out = e.ok("GET", base+"?mode=process&limit=1&cursor="+url.QueryEscape(out["nextCursor"].(string)), nil, 200)
	if ids := historyPageIDs(t, out, 1, false); !reflect.DeepEqual(ids, []string{"c_001"}) {
		t.Fatal(ids)
	}
}

func TestCompatHistoryCanonicalModeCursor(t *testing.T) {
	e, bx, vm := historyPageFixture(t, 5)
	base := "/api/v1/sandboxes/" + bx + "/command-history"
	out := e.ok("GET", base+"?mode=commands&limit=2", nil, 200)
	if ids := historyPageIDs(t, out, 2, true); !reflect.DeepEqual(ids, []string{"c_004", "c_003"}) {
		t.Fatal(ids)
	}
	cursor := url.QueryEscape(out["nextCursor"].(string))
	out = e.ok("GET", "/api/v1/sandboxes/"+vm+"/command-history?mode=detached,stream,sync,detached&limit=2&cursor="+cursor, nil, 200)
	if ids := historyPageIDs(t, out, 2, true); !reflect.DeepEqual(ids, []string{"c_002", "c_001"}) {
		t.Fatal(ids)
	}
	if status, _, _ := e.compat(e.owner, "GET", base+"?mode=process&cursor="+cursor, nil, nil); status != 400 {
		t.Fatalf("different mode cursor status = %d", status)
	}
	// The default includes every supported mode, even those not retained locally.
	out = e.ok("GET", base+"?limit=2", nil, 200)
	cursor = url.QueryEscape(out["nextCursor"].(string))
	e.ok("GET", base+"?mode=process,sync,stream,detached&cursor="+cursor, nil, 200)
}

func TestCompatHistoryCursorAcrossAppendAndTrimming(t *testing.T) {
	e, bx, vm := historyPageFixture(t, 8)
	base := "/api/v1/sandboxes/" + bx + "/command-history"
	// Equal timestamps still have a stable, unique order.
	if err := e.srv.store.Update(func(st *State) error {
		st.Commands[vm][6].StartedAt = st.Commands[vm][7].StartedAt
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out := e.ok("GET", base+"?limit=2", nil, 200)
	ids := historyPageIDs(t, out, 2, true)
	if !reflect.DeepEqual(ids, []string{"c_007", "c_006"}) {
		t.Fatal(ids)
	}
	if err := e.srv.store.Update(func(st *State) error {
		rows := st.Commands[vm]
		// Retention trims the oldest row; even removal of the cursor record
		// leaves the key boundary intact. A new append must not repeat rows.
		st.Commands[vm] = append([]CommandRecord{}, rows[1:6]...)
		st.Commands[vm] = append(st.Commands[vm], rows[7], CommandRecord{
			ID: "c_008", Kind: "command", Cmd: []string{"true"}, StartedAt: rows[7].StartedAt.Add(time.Second),
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for out["nextCursor"] != nil {
		// The native VM ID alias shares the same continuation scope.
		out = e.ok("GET", "/api/v1/sandboxes/"+vm+"/command-history?limit=2&cursor="+url.QueryEscape(out["nextCursor"].(string)), nil, 200)
		ids = append(ids, historyPageIDs(t, out, 2, len(ids) < 6)...)
	}
	if want := []string{"c_007", "c_006", "c_005", "c_004", "c_003", "c_002", "c_001"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("after append/trim = %v, want %v", ids, want)
	}
}

func TestCompatHistoryQueryValidationAndOwnership(t *testing.T) {
	e, bx, _ := historyPageFixture(t, 2)
	base := "/api/v1/sandboxes/" + bx + "/command-history"
	for _, query := range []string{"limit=0", "limit=101", "limit=-1", "limit=abc", "mode=unknown", "mode=process,", "q=" + strings.Repeat("a", 201), "cursor=not-a-cursor"} {
		if st, out, _ := e.compat(e.owner, "GET", base+"?"+query, nil, nil); st != 400 || errCode(out) != "invalid_query" {
			t.Errorf("query %s = %d %v", query, st, out)
		}
	}
	out := e.ok("GET", base+"?limit=1", nil, 200)
	cursor := url.QueryEscape(out["nextCursor"].(string))
	if st, out, _ := e.compat(e.owner, "GET", base+"?mode=process&cursor="+cursor, nil, nil); st != 400 || errCode(out) != "invalid_query" {
		t.Errorf("changed filter = %d %v", st, out)
	}
	if st, out, _ := e.compat(otherTeamKey(e), "GET", base, nil, nil); st != 404 || errCode(out) != "not_found" {
		t.Errorf("other team = %d %v", st, out)
	}
	otherBX := e.newSandbox(nil)
	out = e.ok("GET", "/api/v1/sandboxes/"+otherBX+"/command-history", nil, 200)
	if ids := historyPageIDs(t, out, 20, false); len(ids) != 0 {
		t.Fatalf("unrelated sandbox = %v", ids)
	}
	if st, out, _ := e.compat(e.owner, "GET", "/api/v1/sandboxes/"+otherBX+"/command-history?cursor="+cursor, nil, nil); st != 400 || errCode(out) != "invalid_query" {
		t.Errorf("other sandbox cursor = %d %v", st, out)
	}
}
