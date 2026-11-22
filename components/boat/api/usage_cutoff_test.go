package main

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeUsageExclusionCoverage(t *testing.T) {
	for _, prune := range []string{"cap", "age", "legacy"} {
		t.Run(prune, func(t *testing.T) {
			e := setup(t)
			id := e.nativeOf(e.newSandbox(nil))
			team := e.teamID()
			base := time.Now().UTC().Add(-time.Hour)
			if prune == "age" {
				base = base.AddDate(0, 0, -envSpanRetentionDays-1)
			}
			lostEnd := base.Add(time.Second)
			if err := e.srv.store.Update(func(st *State) error {
				if prune == "cap" {
					for i := 0; i <= maxEnvSpansPerVM; i++ {
						at := base.Add(time.Duration(i) * 2 * time.Second)
						setEnvErrorSpan(st, id, true, at)
						setEnvErrorSpan(st, id, false, at.Add(time.Second))
					}
				} else if prune == "age" {
					setEnvErrorSpan(st, id, true, base)
					setEnvErrorSpan(st, id, false, lostEnd)
					pruneEnvErrorSpans(st, time.Now())
				} else {
					st.EnvErrorLegacyBefore = lostEnd
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			e.boatd.mu.Lock()
			e.boatd.usageWindowFn = func(q url.Values) map[string]any {
				start, _ := parseUsageBound(q.Get("since"))
				end, _ := parseUsageBound(q.Get("until"))
				return map[string]any{"exact": true, "since": start, "until": end, "intervals": []any{
					map[string]any{"id": id, "team": team, "size": "small", "start": start, "end": end},
				}}
			}
			e.boatd.mu.Unlock()
			for _, tc := range []struct {
				name string
				from time.Time
				to   time.Time
				want int
			}{
				{"lost interval", base, lostEnd, 501},
				{"straddles cutoff", lostEnd.Add(-time.Nanosecond), lostEnd.Add(time.Nanosecond), 501},
				{"exact cutoff", lostEnd, lostEnd.Add(time.Second), 200},
				{"recent window", time.Now().UTC().Add(-time.Minute), time.Now().UTC().Add(-time.Second), 200},
			} {
				t.Run(tc.name, func(t *testing.T) {
					q := url.Values{"since": {tc.from.Format(time.RFC3339Nano)}, "until": {tc.to.Format(time.RFC3339Nano)}}
					status, out := e.do(e.owner, "GET", "/v1/vms/"+id+"/usage?"+q.Encode(), nil)
					if status != tc.want {
						t.Fatalf("status = %d, want %d: %v", status, tc.want, out)
					}
					if status == 501 && out["error"].(map[string]any)["code"] != "not_supported" {
						t.Fatalf("unsupported error: %v", out)
					}
				})
			}
		})
	}
}

func TestNativeUsageDeleteClearsExclusionCoverage(t *testing.T) {
	e := setup(t)
	id := e.nativeOf(e.newSandbox(nil))
	end := time.Now().UTC().Add(-time.Minute)
	if err := e.srv.store.Update(func(st *State) error {
		setEnvErrorSpan(st, id, true, end.Add(-time.Second))
		setEnvErrorSpan(st, id, false, end)
		st.EnvErrorCutoffs[id] = end.Add(-time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.srv.forgetVM(id)
	state := e.srv.store.Snapshot()
	if _, exists := state.EnvErrorCutoffs[id]; exists {
		t.Fatal("deleted VM retains exact-usage coverage metadata")
	}
	if len(state.EnvErrorSpans[id]) != 1 {
		t.Fatal("deletion lost aggregate usage exclusions")
	}
}

func TestNativeUsageCoverageChecksOnlyBillableIntervals(t *testing.T) {
	e := setup(t)
	id := e.nativeOf(e.newSandbox(nil))
	team := e.teamID()
	cutoff := time.Now().UTC().Add(-time.Hour)
	if err := e.srv.store.Update(func(st *State) error {
		st.EnvErrorCutoffs[id] = cutoff
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.boatd.mu.Lock()
	e.boatd.usageWindowFn = func(url.Values) map[string]any {
		return map[string]any{"exact": true, "until": cutoff.Add(time.Minute), "intervals": []any{
			map[string]any{"id": id, "team": team, "size": "small", "start": cutoff, "end": cutoff.Add(time.Second)},
			map[string]any{"id": "other", "team": team, "size": "small", "start": cutoff.Add(-time.Minute), "end": cutoff},
			map[string]any{"id": id, "team": "other", "size": "small", "start": cutoff.Add(-time.Minute), "end": cutoff},
			map[string]any{"id": id, "team": team, "size": "small", "start": cutoff.Add(-time.Minute), "end": cutoff.Add(-time.Minute)},
		}}
	}
	e.boatd.mu.Unlock()
	// No since bound does not imply missing coverage before the first actual run.
	if status, out := e.do(e.owner, "GET", "/v1/vms/"+id+"/usage", nil); status != 200 || out["vm_seconds"] != 1.0 {
		t.Fatalf("unaffected intervals: %d %v", status, out)
	}
}

func TestEnvErrorCoverageMigrationAndPersistence(t *testing.T) {
	for _, legacy := range []string{"{}", `{"env_error_spans":{"vm":[{"start":"2025-01-01T00:00:00Z","end":"2025-01-01T00:00:01Z"}]}}`} {
		t.Run(legacy, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0600); err != nil {
				t.Fatal(err)
			}
			before := time.Now().UTC()
			store, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			cutoff := store.Snapshot().EnvErrorLegacyBefore
			if cutoff.Before(before) || cutoff.After(time.Now()) || store.Snapshot().EnvErrorCutoffs == nil {
				t.Fatalf("legacy migration: %+v", store.Snapshot())
			}
			// Opening alone must durably record the boundary, before any mutation/Flush.
			reopened, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			if !reopened.Snapshot().EnvErrorLegacyBefore.Equal(cutoff) {
				t.Fatalf("migration boundary moved on reopen: %v -> %v", cutoff, reopened.Snapshot().EnvErrorLegacyBefore)
			}
		})
	}

	t.Run("tracked store", func(t *testing.T) {
		dir := t.TempDir()
		store, err := OpenStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		if err := store.Update(func(*State) error { return nil }); err != nil {
			t.Fatal(err)
		}
		reopened, err := OpenStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		if !reopened.Snapshot().EnvErrorLegacyBefore.IsZero() {
			t.Fatal("empty tracked history was mistaken for legacy loss")
		}
		end := time.Now().UTC().Add(-time.Hour)
		if err := reopened.Update(func(st *State) error {
			st.EnvErrorCutoffs["vm"] = end
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		last, err := OpenStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = last.Close() })
		if !last.Snapshot().EnvErrorCutoffs["vm"].Equal(end) {
			t.Fatal("per-VM cutoff did not persist")
		}
	})
}
