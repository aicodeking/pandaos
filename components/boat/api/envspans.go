package main

import (
	"context"
	"log"
	"math"
	"net/url"
	"slices"
	"sync"
	"time"
)

// Env-error billing. boat.dev does not bill a sandbox in state "error", and
// here a running VM whose environment never reached the guest reports exactly
// that (VMEnvErrors). State.EnvErrorSpans records those intervals per VM so the
// usage reports can take them out of vm_seconds:
//
//	opened  when an env push fails (recordEnvResult) and no span is open
//	closed  when a push succeeds, or the VM is stopped, deleted or cold-started
//	        (a VM that expired without an API call is closed at its stopped_at
//	        the next time a report or a start looks at it)
//
// Reports clip every span to the requested window.

// EnvErrorSpan is one interval of a VM running in the env-error state.
type EnvErrorSpan struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"` // zero: still open
}

const (
	envSpanRetentionDays = 400
	maxEnvSpansPerVM     = 200
)

func hasOpenSpan(sp []EnvErrorSpan) bool {
	return len(sp) > 0 && sp[len(sp)-1].End.IsZero()
}

// setEnvErrorSpan opens (failed) or closes (!failed) vmID's current span at
// `at` inside a store mutation. Opening an open span or closing a closed one
// does nothing.
func setEnvErrorSpan(st *State, vmID string, failed bool, at time.Time) {
	sp := st.EnvErrorSpans[vmID]
	open := hasOpenSpan(sp)
	switch {
	case failed && !open:
		if _, tracked := st.EnvErrorCutoffs[vmID]; !tracked {
			st.EnvErrorCutoffs[vmID] = time.Time{}
		}
		sp = append(sp, EnvErrorSpan{Start: at.UTC()})
		if len(sp) > maxEnvSpansPerVM {
			for _, lost := range sp[:len(sp)-maxEnvSpansPerVM] {
				recordEnvErrorCutoff(st, vmID, lost.End)
			}
			sp = sp[len(sp)-maxEnvSpansPerVM:]
		}
	case !failed && open:
		if _, tracked := st.EnvErrorCutoffs[vmID]; !tracked {
			st.EnvErrorCutoffs[vmID] = time.Time{}
		}
		last := &sp[len(sp)-1]
		last.End = at.UTC()
		if last.End.Before(last.Start) {
			last.End = last.Start
		}
	default:
		return
	}
	st.EnvErrorSpans[vmID] = sp
	pruneEnvErrorSpans(st, at)
}

// A discarded exclusion makes earlier billable intervals unprovable.
func recordEnvErrorCutoff(st *State, vmID string, end time.Time) {
	if end.After(st.EnvErrorCutoffs[vmID]) {
		st.EnvErrorCutoffs[vmID] = end.UTC()
	}
}

// pruneEnvErrorSpans drops closed spans past the retention window (and VMs left
// with none).
func pruneEnvErrorSpans(st *State, now time.Time) {
	cutoff := now.UTC().AddDate(0, 0, -envSpanRetentionDays)
	for id, sp := range st.EnvErrorSpans {
		kept := slices.DeleteFunc(slices.Clone(sp), func(x EnvErrorSpan) bool {
			if !x.End.IsZero() && x.End.Before(cutoff) {
				// Deleted VMs retain aggregate exclusions, but no exact-usage marker.
				// Untracked legacy closed spans are covered by EnvErrorLegacyBefore.
				if _, tracked := st.EnvErrorCutoffs[id]; tracked {
					recordEnvErrorCutoff(st, id, x.End)
				}
				return true
			}
			return false
		})
		if len(kept) == 0 {
			delete(st.EnvErrorSpans, id)
		} else {
			st.EnvErrorSpans[id] = kept
		}
	}
}

// closeEnvErrorSpan ends vmID's open span at `at` (a stop, a delete, a start
// from a stopped VM). A no-op when none is open.
func (s *server) closeEnvErrorSpan(vmID string, at time.Time) {
	if !hasOpenSpan(s.store.Snapshot().EnvErrorSpans[vmID]) {
		return
	}
	if err := s.store.Apply(func(st *State) error {
		setEnvErrorSpan(st, vmID, false, at)
		return nil
	}); err != nil {
		log.Printf("close env error span of %s: %v", vmID, err)
	}
}

// vmStoppedAt is when a VM stopped, capped at now. Legacy records without a
// valid stopped_at fall back to updated_at.
func vmStoppedAt(vm VM, now time.Time) time.Time {
	for _, field := range []string{"stopped_at", "updated_at"} {
		if t, err := time.Parse(time.RFC3339Nano, str(vm, field)); err == nil {
			if t.Before(now) {
				return t
			}
			return now
		}
	}
	return now
}

// vmLookup is a VM's boatd record; false = unknown (the lookup failed or the VM is gone).
type vmLookup func(ctx context.Context, vmID string) (VM, bool)

// fetchVM is vmLookup with one boatd GET.
func (s *server) fetchVM(ctx context.Context, vmID string) (VM, bool) {
	var vm VM
	if err := s.boatd.call(ctx, "GET", vmPath(vmID), nil, &vm); err != nil {
		return nil, false
	}
	return vm, true
}

// openSpanEnd is where vmID's still-open span ends for a report running to
// `to`: `to` while the VM runs (or is unknown: fail toward the VM's own
// bill, not a refund), else when it stopped.
func (s *server) openSpanEnd(ctx context.Context, look vmLookup, vmID string, start, to time.Time) time.Time {
	vm, ok := look(ctx, vmID)
	if !ok || str(vm, "state") == "running" {
		return to
	}
	end := vmStoppedAt(vm, to)
	if end.Before(start) {
		return start
	}
	return end
}

// envErrorSeconds is the time vmID spent in the env-error state inside [from, to]
// (from zero = since ever).
func (s *server) envErrorSeconds(ctx context.Context, vmID string, from, to time.Time) float64 {
	return s.envErrorSecondsWith(ctx, s.fetchVM, vmID, from, to)
}

func (s *server) envErrorSecondsWith(ctx context.Context, look vmLookup, vmID string, from, to time.Time) float64 {
	spans := s.store.Snapshot().EnvErrorSpans[vmID]
	if now := time.Now(); to.After(now) {
		to = now
	}
	var total float64
	for _, sp := range spans {
		end := sp.End
		if end.IsZero() {
			end = s.openSpanEnd(ctx, look, vmID, sp.Start, to)
		}
		lo, hi := sp.Start, end
		if lo.Before(from) {
			lo = from
		}
		if hi.After(to) {
			hi = to
		}
		if hi.After(lo) {
			total += hi.Sub(lo).Seconds()
		}
	}
	return total
}

// reportVMLookup answers a report's open-span lookups from one boatd list call
// (made at the first one, none when no span is open) instead of a GET per VM.
// teams are the report's teams: a single one narrows the list.
func (s *server) reportVMLookup(teams map[string]bool) vmLookup {
	var once sync.Once
	var byID map[string]VM
	return func(ctx context.Context, vmID string) (VM, bool) {
		once.Do(func() {
			path := "/v1/vms"
			if len(teams) == 1 {
				for t := range teams {
					path += "?team=" + url.QueryEscape(t)
				}
			}
			var list struct {
				VMs []VM `json:"vms"`
			}
			if err := s.boatd.call(ctx, "GET", path, nil, &list); err != nil {
				log.Printf("env-error spans: list VMs: %v", err)
				return // every VM unknown: spans run to the report's end
			}
			byID = make(map[string]VM, len(list.VMs))
			for _, vm := range list.VMs {
				byID[str(vm, "id")] = vm
			}
		})
		vm, ok := byID[vmID]
		return vm, ok
	}
}

// withoutEnvErrorSeconds returns a copy of a boatd usage report (vms rows and
// teams rows with vm_seconds) with every VM's env-error time inside
// [since, until] taken out of vm_seconds, the team's vm_seconds and its
// vm_seconds_by_size. The input is not modified.
func (s *server) withoutEnvErrorSeconds(ctx context.Context, rep map[string]any, since, until time.Time) map[string]any {
	if len(s.store.Snapshot().EnvErrorSpans) == 0 {
		return rep
	}
	if now := time.Now(); until.IsZero() || until.After(now) {
		until = now
	}
	rows, _ := rep["vms"].([]any)
	openTeams := map[string]bool{}
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok {
			if id, _ := m["id"].(string); hasOpenSpan(s.store.Snapshot().EnvErrorSpans[id]) {
				team, _ := m["team"].(string)
				openTeams[team] = true
			}
		}
	}
	look := s.reportVMLookup(openTeams)
	teamSub := map[string]float64{}
	teamSizeSub := map[string]map[string]float64{}
	out := make(map[string]any, len(rep))
	for k, v := range rep {
		out[k] = v
	}
	vms := make([]any, 0, len(rows))
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			vms = append(vms, row)
			continue
		}
		id, _ := m["id"].(string)
		sub := s.envErrorSecondsWith(ctx, look, id, since, until)
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		if sub > 0 {
			f, _ := m["vm_seconds"].(float64)
			sub = math.Min(sub, f)
			cp["vm_seconds"] = f - sub
			team, _ := m["team"].(string)
			size, _ := m["size"].(string)
			teamSub[team] += sub
			if teamSizeSub[team] == nil {
				teamSizeSub[team] = map[string]float64{}
			}
			teamSizeSub[team][size] += sub
		}
		vms = append(vms, cp)
	}
	if rows != nil {
		out["vms"] = vms
	}
	if len(teamSub) == 0 {
		return out
	}
	trows, _ := rep["teams"].([]any)
	teams := make([]any, 0, len(trows))
	for _, row := range trows {
		m, ok := row.(map[string]any)
		if !ok {
			teams = append(teams, row)
			continue
		}
		team, _ := m["team"].(string)
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		if sub := teamSub[team]; sub > 0 {
			f, _ := m["vm_seconds"].(float64)
			cp["vm_seconds"] = math.Max(0, f-sub)
			if sizes, ok := m["vm_seconds_by_size"].(map[string]any); ok {
				by := make(map[string]any, len(sizes))
				for size, v := range sizes {
					g, _ := v.(float64)
					by[size] = math.Max(0, g-teamSizeSub[team][size])
				}
				cp["vm_seconds_by_size"] = by
			}
		}
		teams = append(teams, cp)
	}
	out["teams"] = teams
	return out
}
