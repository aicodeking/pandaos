package main

import (
	"context"
	"testing"
	"time"
)

func TestVMStoppedAt(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 123456789, time.UTC)
	stopped := now.Add(-time.Hour)
	updated := now.Add(-time.Minute)
	for _, tc := range []struct {
		name string
		vm   VM
		want time.Time
	}{
		{"stop survives metadata edit", VM{"stopped_at": stopped.Format(time.RFC3339Nano), "updated_at": updated.Format(time.RFC3339Nano)}, stopped},
		{"future stop capped without legacy fallback", VM{"stopped_at": now.Add(time.Hour).Format(time.RFC3339Nano), "updated_at": updated.Format(time.RFC3339Nano)}, now},
		{"stop at report end", VM{"stopped_at": now.Format(time.RFC3339Nano)}, now},
		{"legacy missing stop", VM{"updated_at": updated.Format(time.RFC3339Nano)}, updated},
		{"invalid stop", VM{"stopped_at": "invalid", "updated_at": updated.Format(time.RFC3339Nano)}, updated},
		{"legacy future update capped", VM{"updated_at": now.Add(time.Hour).Format(time.RFC3339Nano)}, now},
		{"no valid timestamp", VM{"stopped_at": "invalid", "updated_at": "invalid"}, now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := vmStoppedAt(tc.vm, now); !got.Equal(tc.want) {
				t.Fatalf("vmStoppedAt = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEnvErrorSecondsUsesStopBeforeMetadataEdit(t *testing.T) {
	e := setup(t)
	now := time.Now().UTC().Add(-time.Hour)
	start := now.Add(-30 * time.Minute)
	stop := start.Add(10*time.Minute + 125*time.Millisecond)
	seedSpans(e, "vm1", EnvErrorSpan{Start: start})
	look := func(context.Context, string) (VM, bool) {
		return VM{"state": "stopped", "stopped_at": stop.Format(time.RFC3339Nano), "updated_at": now.Format(time.RFC3339Nano)}, true
	}
	if got := e.srv.envErrorSecondsWith(context.Background(), look, "vm1", start, now); got != 600.125 {
		t.Fatalf("env-error seconds = %v, want 600.125", got)
	}
}

func TestEnvErrorCutoffsRetainBoundaryAndOpenSpan(t *testing.T) {
	st := &State{}
	st.init()
	now := time.Now().UTC()
	boundary := now.AddDate(0, 0, -envSpanRetentionDays)
	st.EnvErrorSpans["vm"] = []EnvErrorSpan{
		{Start: boundary.Add(-2 * time.Second), End: boundary.Add(-time.Nanosecond)},
		{Start: boundary.Add(-time.Second), End: boundary},
		{Start: boundary.Add(-time.Hour)},
	}
	st.EnvErrorCutoffs["vm"] = time.Time{}
	prior := st.clone()
	pruneEnvErrorSpans(st, now)
	if got := st.EnvErrorCutoffs["vm"]; !got.Equal(boundary.Add(-time.Nanosecond)) {
		t.Fatalf("cutoff = %v", got)
	}
	if len(st.EnvErrorSpans["vm"]) != 2 || !st.EnvErrorSpans["vm"][0].End.Equal(boundary) || !hasOpenSpan(st.EnvErrorSpans["vm"]) {
		t.Fatalf("retention boundary or open span lost: %v", st.EnvErrorSpans["vm"])
	}
	// A prune triggered by an older timestamp cannot restore previously lost coverage.
	pruneEnvErrorSpans(st, now.Add(-time.Hour))
	if !st.EnvErrorCutoffs["vm"].Equal(boundary.Add(-time.Nanosecond)) || len(prior.EnvErrorSpans["vm"]) != 3 || !prior.EnvErrorCutoffs["vm"].IsZero() {
		t.Fatal("pruning moved the cutoff backward or mutated an old snapshot")
	}
}

func TestEnvErrorPruneDoesNotRecreateDeletedCoverage(t *testing.T) {
	st := &State{}
	st.init()
	now := time.Now().UTC()
	end := now.AddDate(0, 0, -envSpanRetentionDays-1)
	setEnvErrorSpan(st, "deleted", true, end.Add(-time.Second))
	setEnvErrorSpan(st, "deleted", false, end)
	delete(st.EnvErrorCutoffs, "deleted")
	pruneEnvErrorSpans(st, now)
	if len(st.EnvErrorCutoffs) != 0 || len(st.EnvErrorSpans) != 0 {
		t.Fatal("pruning recreated deleted-VM metadata")
	}
}
