package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestR16RejectedResumePreservesRunningVM(t *testing.T) {
	for _, typ := range []string{"../../invalid", "missing-size"} {
		t.Run(typ, func(t *testing.T) {
			e := setup(t)
			bx := e.newSandbox(nil)
			id := e.nativeOf(bx)
			st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/resume", map[string]any{"type": typ}, nil)
			if st != 400 {
				t.Fatalf("resume: %d %v", st, out)
			}
			e.boatd.mu.Lock()
			defer e.boatd.mu.Unlock()
			if e.boatd.vms[id]["state"] != "running" || e.boatd.vms[id]["size"] != "default" {
				t.Errorf("rejected resume changed VM: %v", e.boatd.vms[id])
			}
			if typ == "../../invalid" && len(e.boatd.stops) != 0 {
				t.Error("invalid type stopped VM")
			}
		})
	}
}

func TestR16SnapshotPagination(t *testing.T) {
	e := setup(t)
	list := []map[string]any{{"id": "c", "created_at": "2026-01-02"}, {"id": "b", "created_at": "2026-01-01"}, {"id": "a", "created_at": "2026-01-01"}}
	page := func(team, path, query string) (int, map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path+"?"+query, nil)
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, &reqInfo{id: "r16"}))
		e.srv.snapshotPage(w, r, team, append([]map[string]any(nil), list...))
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return w.Code, out
	}
	if st, out := page("team", "/snapshots", ""); st != 200 || out["pageInfo"].(map[string]any)["limit"] != float64(100) {
		t.Fatalf("schema default limit: %d %v", st, out)
	}
	for _, order := range []string{"asc", "desc"} {
		cursor := ""
		want := []string{"a", "b", "c"}
		if order == "desc" {
			want = []string{"c", "b", "a"}
		}
		for i, id := range want {
			st, out := page("team", "/snapshots", "limit=1&sort="+order+"&cursor="+url.QueryEscape(cursor))
			if st != 200 {
				t.Fatalf("page: %d %v", st, out)
			}
			rows := out["snapshots"].([]any)
			if len(rows) != 1 || rows[0].(map[string]any)["id"] != snapshotUUID("team", id) {
				t.Fatalf("page %d: %v", i, out)
			}
			pi := out["pageInfo"].(map[string]any)
			if pi["hasMore"] != (i < 2) {
				t.Fatal(pi)
			}
			if i == 2 {
				if pi["nextCursor"] != nil {
					t.Fatal(pi)
				}
				break
			}
			var ok bool
			cursor, ok = pi["nextCursor"].(string)
			if !ok || cursor == "" {
				t.Fatalf("missing continuation: %v", pi)
			}
			opposite := "asc"
			if order == "asc" {
				opposite = "desc"
			}
			for _, m := range []struct{ team, path, sort string }{{"other", "/snapshots", order}, {"team", "/other", order}, {"team", "/snapshots", opposite}} {
				if st, _ := page(m.team, m.path, "sort="+m.sort+"&cursor="+url.QueryEscape(cursor)); st != 400 {
					t.Errorf("mismatched cursor accepted: %v", m)
				}
			}
		}
	}
	for _, q := range []string{"cursor=broken", "limit=0", "limit=201", "sort=bad"} {
		if st, _ := page("team", "/snapshots", q); st != 400 {
			t.Errorf("accepted %s", q)
		}
	}
}

func TestR16ResumeDeniedResizeHasNoStop(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	id := e.nativeOf(bx)
	e.srv.routeScopes["POST /v1/vms/{id}/resize"] = "ungranted:resize"
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/resume", map[string]any{"type": "large"}, nil)
	if st != 403 {
		t.Fatalf("denial: %d %v", st, out)
	}
	e.boatd.mu.Lock()
	defer e.boatd.mu.Unlock()
	if e.boatd.vms[id]["state"] != "running" || len(e.boatd.stops) != 0 {
		t.Fatal("denied resize stopped VM")
	}
}

func TestR16ResumeReportsRestorationFailure(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	e.boatd.mu.Lock()
	e.boatd.refuse = "host_full"
	e.boatd.mu.Unlock()
	st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/resume", map[string]any{"type": "missing-size"}, nil)
	if st != 500 || errCode(out) != "restore_failed" {
		t.Fatalf("restoration failure hidden: %d %v", st, out)
	}
}

func TestR16ResumeValidRunningResize(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	id := e.nativeOf(bx)
	for _, typ := range []string{"large", "default"} {
		st, out, _ := e.compat(e.owner, "POST", "/api/v1/sandboxes/"+bx+"/resume", map[string]any{"type": typ}, nil)
		if st != 202 {
			t.Fatalf("resume: %d %v", st, out)
		}
		e.boatd.mu.Lock()
		state, size := e.boatd.vms[id]["state"], e.boatd.vms[id]["size"]
		e.boatd.mu.Unlock()
		if state != "running" || size != typ {
			t.Fatalf("resize: %v %v", state, size)
		}
	}
}
