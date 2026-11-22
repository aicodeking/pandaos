package main

import (
	"net/url"
	"testing"
	"time"
)

func TestR50NativeUsageExactBoundsAndSizes(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	id := e.nativeOf(bx)
	team := e.teamID()
	since := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute).Add(17*time.Second + 125*time.Millisecond)
	split := since.Add(11*time.Second + 250*time.Millisecond)
	until := since.Add(29*time.Second + 750*time.Millisecond)
	e.boatd.mu.Lock()
	e.boatd.usageWindowFn = func(q url.Values) map[string]any {
		if q.Get("team") != team || q.Get("vm_id") != id || q.Get("since") != since.Format(time.RFC3339Nano) || q.Get("until") != until.Format(time.RFC3339Nano) {
			t.Errorf("query: %v", q)
		}
		return map[string]any{"exact": true, "since": since, "until": until, "intervals": []any{
			map[string]any{"id": id, "team": team, "size": "small", "start": since, "end": split},
			map[string]any{"id": id, "team": team, "size": "large", "start": split, "end": until},
			map[string]any{"id": id, "team": "other", "size": "large", "start": since, "end": until},
			map[string]any{"id": "other-vm", "team": team, "size": "large", "start": since, "end": until},
		}}
	}
	before := len(e.boatd.usageQueries)
	e.boatd.mu.Unlock()
	// One exclusion straddles the resize, so each historical size loses its own part.
	seedSpans(e, id, EnvErrorSpan{Start: split.Add(-2 * time.Second), End: split.Add(3 * time.Second)})
	q := url.Values{"since": {since.Format(time.RFC3339Nano)}, "until": {until.Format(time.RFC3339Nano)}}
	st, out := e.do(e.owner, "GET", "/v1/vms/"+id+"/usage?"+q.Encode(), nil)
	if st != 200 || out["vm_seconds"] != 24.75 {
		t.Fatalf("native: %d %v", st, out)
	}
	sizes, _ := out["vm_seconds_by_size"].(map[string]any)
	if sizes["small"] != 9.25 || sizes["large"] != 15.5 {
		t.Fatalf("historical sizes: %v", sizes)
	}
	e.boatd.mu.Lock()
	n := len(e.boatd.usageQueries) - before
	e.boatd.mu.Unlock()
	if n != 1 {
		t.Fatalf("usage reads: %d, want 1", n)
	}
	if st, out := e.do(e.owner, "GET", "/v1/vms/"+id+"/usage?since=bad", nil); st != 400 {
		t.Fatalf("invalid bounds: %d %v", st, out)
	}
	if st, out := e.do(e.owner, "GET", "/v1/vms/"+id+"/usage?since=5&until=5", nil); st != 400 {
		t.Fatalf("equal bounds: %d %v", st, out)
	}
}

func TestR50NativeUsageRequiresExactBoatd(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	id := e.nativeOf(bx)
	e.boatd.mu.Lock()
	e.boatd.usageWindowFn = func(url.Values) map[string]any {
		return map[string]any{"vms": []any{map[string]any{"id": id, "vm_seconds": 900}}}
	}
	e.boatd.mu.Unlock()
	if st, out := e.do(e.owner, "GET", "/v1/vms/"+id+"/usage", nil); st != 501 || out["error"].(map[string]any)["code"] != "not_supported" {
		t.Fatalf("legacy boatd: %d %v", st, out)
	}
}

func TestR50CompatUsageUnsupportedAfterOwnership(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	if st, out, _ := e.compat(e.owner, "GET", "/api/v1/sandboxes/"+bx+"/usage", nil, nil); st != 501 || errCode(out) != "not_supported" {
		t.Fatalf("compat usage: %d %v", st, out)
	}
	if st, _, _ := e.compat(e.owner, "GET", "/api/v1/sandboxes/bx_22222222/usage", nil, nil); st != 404 {
		t.Fatalf("unknown sandbox status %d", st)
	}
	e.boatd.mu.Lock()
	n := len(e.boatd.usageQueries)
	e.boatd.mu.Unlock()
	if n != 0 {
		t.Fatalf("unsupported usage reached ledger %d times", n)
	}
}

func TestR50NativeUsageOwnership(t *testing.T) {
	e := setup(t)
	bx := e.newSandbox(nil)
	id := e.nativeOf(bx)
	other := otherTeamKey(e)
	if st, out := e.do(other, "GET", "/v1/vms/"+id+"/usage", nil); st != 404 {
		t.Fatalf("foreign usage: %d %v", st, out)
	}
	if st, out, _ := e.compat(other, "GET", "/api/v1/sandboxes/"+bx+"/usage", nil, nil); st != 404 {
		t.Fatalf("foreign compat usage: %d %v", st, out)
	}
	e.boatd.mu.Lock()
	n := len(e.boatd.usageQueries)
	e.boatd.mu.Unlock()
	if n != 0 {
		t.Fatalf("foreign usage reached ledger %d times", n)
	}
}
