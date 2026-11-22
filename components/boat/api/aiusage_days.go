package main

import (
	"maps"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Per-day AI usage. State.AIUsage keeps one lifetime row per team/VM/provider/
// model (native GET /v1/ai/usage sums those). State.AIUsageDays keeps the same
// counters per UTC day so boat.dev's ?days=N windows count only the window.

// aiUsageRetentionDays is how many UTC days of per-day rows are kept.
const aiUsageRetentionDays = 400

// aiDeletedVM is the VM id of the per-team aggregate rows that hold the usage of
// deleted VMs (and of days folded out of the per-day table), so the team totals
// stay right while the tables stay bounded.
const aiDeletedVM = "_deleted"

// aiUsageDaysMaxRows caps State.AIUsageDays (20k rows keep state.json near 6 MiB
// and a flush under the writer lock in tens of milliseconds). Past it the oldest
// days are folded into the per-team aggregate (VM dropped), then dropped (their
// counts live on in State.AIUsage). A variable for tests.
var aiUsageDaysMaxRows = 20000

func aiUsageKey(team, vm, provider, model string) string {
	return team + "/" + vm + "/" + provider + "/" + model
}

// addAIUsage adds d's counters to m[key] (created when missing). It returns the
// row and whether it is new (a new key invalidates the per-team index; a
// counter change only needs the row swapped in it, patchAIUsageIndex).
func addAIUsage(m map[string]*AIUsageRow, key string, d *AIUsageRow, day string, now time.Time) (*AIUsageRow, bool) {
	prev := m[key]
	created := prev == nil
	var cp AIUsageRow
	if created {
		cp = AIUsageRow{TeamID: d.TeamID, VMID: d.VMID, Provider: d.Provider, Model: d.Model, Day: day}
	} else {
		cp = *prev
	}
	cp.Requests += d.Requests
	cp.InputTokens += d.InputTokens
	cp.OutputTokens += d.OutputTokens
	cp.Unmetered += d.Unmetered
	cp.Updated = now
	m[key] = &cp
	return &cp, created
}

// foldAIRow moves row r (owned by m, not yet an aggregate row) into the
// aggregate row of its team for the same provider, model and day (VM
// aiDeletedVM) and removes it from m.
func foldAIRow(m map[string]*AIUsageRow, k string, r *AIUsageRow) {
	delete(m, k)
	key := aiUsageKey(r.TeamID, aiDeletedVM, r.Provider, r.Model)
	if r.Day != "" {
		key += "/" + r.Day
	}
	var cp AIUsageRow
	if prev := m[key]; prev == nil {
		cp = AIUsageRow{TeamID: r.TeamID, VMID: aiDeletedVM, Provider: r.Provider, Model: r.Model, Day: r.Day}
	} else {
		cp = *prev
	}
	cp.Requests += r.Requests
	cp.InputTokens += r.InputTokens
	cp.OutputTokens += r.OutputTokens
	cp.Unmetered += r.Unmetered
	if r.Updated.After(cp.Updated) {
		cp.Updated = r.Updated
	}
	m[key] = &cp
}

// foldDeletedVMAIUsage folds a deleted VM's lifetime and per-day usage rows into
// its team's aggregate rows (VM id aiDeletedVM), so the team totals do not change
// while the VM's rows stop taking space. Call it inside a store Apply/Update.
func foldDeletedVMAIUsage(st *State, vmID string) {
	if st == nil || vmID == "" || vmID == aiDeletedVM {
		return
	}
	for _, m := range []map[string]*AIUsageRow{st.AIUsage, st.AIUsageDays} {
		for k, r := range m {
			if r != nil && r.VMID == vmID {
				foldAIRow(m, k, r)
			}
		}
	}
	st.dropAIUsageIndex()
}

// deleteTeamAIUsage drops every AI usage row of a deleted team (lifetime and per
// day), so the tables do not keep rows nobody can read.
func deleteTeamAIUsage(st *State, team string) {
	for _, m := range []map[string]*AIUsageRow{st.AIUsage, st.AIUsageDays} {
		for k, r := range m {
			if r != nil && r.TeamID == team {
				delete(m, k)
			}
		}
	}
	st.dropAIUsageIndex()
}

// pruneAIUsageDays keeps the per-day rows bounded: it drops rows older than the
// retention window, and past aiUsageDaysMaxRows folds the oldest rows into the
// team aggregate (then drops them). The lifetime rows are never touched. It
// reports whether a key was removed or added (the per-team index is then
// dropped); one pass over the rows, no sort unless the cap is passed.
func pruneAIUsageDays(st *State, now time.Time) bool {
	cutoff := now.UTC().AddDate(0, 0, -aiUsageRetentionDays).Format(time.DateOnly)
	changed := false
	perDay := map[string]int{} // rows per day, for the cap
	for k, r := range st.AIUsageDays {
		if r == nil || r.Day < cutoff {
			delete(st.AIUsageDays, k)
			changed = true
			continue
		}
		perDay[r.Day]++
	}
	if len(st.AIUsageDays) > aiUsageDaysMaxRows {
		capAIUsageDays(st, perDay)
		changed = true
	}
	if changed {
		st.dropAIUsageIndex()
	}
	return changed
}

// capAIUsageDays brings the table to the cap. Only the oldest rows are ever
// sorted: a window of the oldest days holding about twice the excess, widened
// (x4) only when folding it did not free enough. perDay is the row count per day.
func capAIUsageDays(st *State, perDay map[string]int) {
	days := make([]string, 0, len(perDay))
	for d := range perDay {
		days = append(days, d)
	}
	sort.Strings(days)
	window := 2*(len(st.AIUsageDays)-aiUsageDaysMaxRows) + 64
	for {
		limit, all := windowLastDay(days, perDay, window)
		keys := aiDayKeysUpTo(st.AIUsageDays, limit)
		// Pass 1: the oldest days lose their VM (folded into the team aggregate).
		for _, k := range keys {
			if len(st.AIUsageDays) <= aiUsageDaysMaxRows {
				return
			}
			if r := st.AIUsageDays[k]; r != nil && r.VMID != aiDeletedVM {
				foldAIRow(st.AIUsageDays, k, r)
			}
		}
		if all {
			break // every row was visited
		}
		window *= 4
	}
	// Pass 2: still too many (days x providers x models): drop the oldest days.
	for _, k := range aiDayKeysUpTo(st.AIUsageDays, "\xff") {
		if len(st.AIUsageDays) <= aiUsageDaysMaxRows {
			return
		}
		delete(st.AIUsageDays, k)
	}
}

// windowLastDay is the newest day among the oldest rows that number at least
// want; all is true when that is every day.
func windowLastDay(days []string, perDay map[string]int, want int) (last string, all bool) {
	n := 0
	for _, d := range days {
		if n += perDay[d]; n >= want {
			return d, d == days[len(days)-1]
		}
	}
	return "\xff", true
}

// aiDayKeysUpTo returns the keys of rows whose day is at most last, oldest day
// first (then by key).
func aiDayKeysUpTo(m map[string]*AIUsageRow, last string) []string {
	keys := make([]string, 0, 1024)
	for k, r := range m {
		if r != nil && r.Day <= last {
			keys = append(keys, k)
		}
	}
	aiCapSortedRows.Add(int64(len(keys)))
	sort.Slice(keys, func(i, j int) bool {
		a, b := m[keys[i]], m[keys[j]]
		if a.Day != b.Day {
			return a.Day < b.Day
		}
		return keys[i] < keys[j]
	})
	return keys
}

// aiCapSortedRows counts the keys the cap has sorted (tests check that an
// over-cap flush sorts a window, not the table).
var aiCapSortedRows atomic.Int64

// migrateAIUsageDays seeds the per-day rows once from the lifetime rows of a
// state written before they existed: each lifetime row is attributed, whole, to
// the UTC day of its last update (the best date the old format kept).
func migrateAIUsageDays(st *State) {
	if st.AIUsageDaysV1 {
		return
	}
	for k, r := range st.AIUsage {
		if r == nil || r.Updated.IsZero() {
			continue
		}
		cp := *r
		cp.Day = r.Updated.UTC().Format(time.DateOnly)
		st.AIUsageDays[k+"/"+cp.Day] = &cp
	}
	pruneAIUsageDays(st, time.Now())
	st.dropAIUsageIndex()
	st.AIUsageDaysV1 = true
}

// aiUsageIndex lists a state's AI usage rows by team and by team+VM, so a read
// touches only its own team's rows. It belongs to one published State (rows are
// never changed after publication) and is built on first use. Mutators of
// State.AIUsage / AIUsageDays call dropAIUsageIndex.
type aiUsageIndex struct {
	life, days map[string][]*AIUsageRow // team -> rows
	lifeVM     map[string][]*AIUsageRow // team + "\x00" + vm -> rows
	daysVM     map[string][]*AIUsageRow
}

// aiIdxMu guards State.aiIdx (set lazily on published snapshots).
var aiIdxMu sync.Mutex

func (st *State) dropAIUsageIndex() {
	aiIdxMu.Lock()
	st.aiIdx = nil
	aiIdxMu.Unlock()
}

// patchAIUsageIndex keeps the index of st across a flush that only changed the
// counters of existing rows (life and days are those rows, already in st): the
// index it carried over from the previous state points at the previous copies, so
// the touched rows are swapped for the new ones in copies of their team's and
// VM's lists (the old lists belong to the published state and stay as they are).
// With no index, or one that does not hold a row, nothing is kept.
func (st *State) patchAIUsageIndex(life, days []*AIUsageRow) {
	aiIdxMu.Lock()
	defer aiIdxMu.Unlock()
	old := st.aiIdx
	if old == nil {
		return
	}
	ix := &aiUsageIndex{life: maps.Clone(old.life), days: maps.Clone(old.days),
		lifeVM: maps.Clone(old.lifeVM), daysVM: maps.Clone(old.daysVM)}
	if !patchAIRows(ix.life, ix.lifeVM, life) || !patchAIRows(ix.days, ix.daysVM, days) {
		st.aiIdx = nil
		return
	}
	st.aiIdx = ix
}

func aiRowID(r *AIUsageRow) string {
	return r.VMID + "\x00" + r.Provider + "\x00" + r.Model + "\x00" + r.Day
}

func patchAIRows(byTeam, byVM map[string][]*AIUsageRow, rows []*AIUsageRow) bool {
	teams, vms := map[string]map[string]*AIUsageRow{}, map[string]map[string]*AIUsageRow{}
	put := func(m map[string]map[string]*AIUsageRow, key string, r *AIUsageRow) {
		if m[key] == nil {
			m[key] = map[string]*AIUsageRow{}
		}
		m[key][aiRowID(r)] = r
	}
	for _, r := range rows {
		put(teams, r.TeamID, r)
		put(vms, r.TeamID+"\x00"+r.VMID, r)
	}
	return patchAIList(byTeam, teams) && patchAIList(byVM, vms)
}

// patchAIList swaps rows (by identity) in copies of the lists of m named in news.
func patchAIList(m map[string][]*AIUsageRow, news map[string]map[string]*AIUsageRow) bool {
	for key, want := range news {
		list := slices.Clone(m[key])
		hit := 0
		for i, r := range list {
			if nr := want[aiRowID(r)]; nr != nil {
				list[i] = nr
				hit++
			}
		}
		if hit != len(want) {
			return false
		}
		m[key] = list
	}
	return true
}

// sharedAIUsageIndex is what clone carries over: the rows it indexes are never
// mutated once their state is published.
func (st *State) sharedAIUsageIndex() *aiUsageIndex {
	aiIdxMu.Lock()
	defer aiIdxMu.Unlock()
	return st.aiIdx
}

func (st *State) aiUsageIdx() *aiUsageIndex {
	aiIdxMu.Lock()
	defer aiIdxMu.Unlock()
	if st.aiIdx == nil {
		ix := &aiUsageIndex{life: map[string][]*AIUsageRow{}, days: map[string][]*AIUsageRow{},
			lifeVM: map[string][]*AIUsageRow{}, daysVM: map[string][]*AIUsageRow{}}
		for _, r := range st.AIUsage {
			if r != nil {
				ix.life[r.TeamID] = append(ix.life[r.TeamID], r)
				ix.lifeVM[r.TeamID+"\x00"+r.VMID] = append(ix.lifeVM[r.TeamID+"\x00"+r.VMID], r)
			}
		}
		for _, r := range st.AIUsageDays {
			if r != nil {
				ix.days[r.TeamID] = append(ix.days[r.TeamID], r)
				ix.daysVM[r.TeamID+"\x00"+r.VMID] = append(ix.daysVM[r.TeamID+"\x00"+r.VMID], r)
			}
		}
		st.aiIdx = ix
	}
	return st.aiIdx
}

// teamAIUsageRows returns the team's usage rows (of one VM when vmID is set) from
// st with the meter's unflushed batch (pending) added: the lifetime rows, or
// with perDay the per-day rows. Only the team's rows are visited.
func teamAIUsageRows(st *State, pending map[string]*AIUsageRow, team, vmID string, perDay bool) []*AIUsageRow {
	ix := st.aiUsageIdx()
	var base []*AIUsageRow
	switch {
	case perDay && vmID == "":
		base = ix.days[team]
	case perDay:
		base = ix.daysVM[team+"\x00"+vmID]
	case vmID == "":
		base = ix.life[team]
	default:
		base = ix.lifeVM[team+"\x00"+vmID]
	}
	mine := map[string]*AIUsageRow{}
	for k, d := range pending {
		if d.TeamID == team && (vmID == "" || d.VMID == vmID) {
			mine[k] = d
		}
	}
	if len(mine) == 0 {
		return slices.Clone(base)
	}
	// The overlay needs only the keys the batch touches; the other rows pass as is.
	touched := map[string]bool{}
	for _, d := range mine {
		k := aiUsageKey(d.TeamID, d.VMID, d.Provider, d.Model)
		if perDay {
			k += "/" + d.Day
		}
		touched[k] = true
	}
	sub := map[string]*AIUsageRow{}
	out := make([]*AIUsageRow, 0, len(base)+len(mine))
	for _, r := range base {
		k := aiUsageKey(r.TeamID, r.VMID, r.Provider, r.Model)
		if perDay {
			k += "/" + r.Day
		}
		if touched[k] {
			sub[k] = r
		} else {
			out = append(out, r)
		}
	}
	return append(out, overlayAIUsage(sub, mine, perDay)...)
}

// aiUsageDays returns the team's per-day rows (of one VM when vmID is set) from
// the UTC day of since on, with the meter's unflushed counts folded in (a read
// never writes the store). One row per day, VM,
// provider and model; sorted by day, VM, provider, model.
func (s *server) aiUsageDays(team, vmID string, since time.Time) []*AIUsageRow {
	st, pending := s.aiMeter.view(s.store, team, vmID)
	from := since.UTC().Format(time.DateOnly)
	rows := []*AIUsageRow{}
	for _, u := range teamAIUsageRows(st, pending, team, vmID, true) {
		if u.Day >= from {
			rows = append(rows, u)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Day != b.Day {
			return a.Day < b.Day
		}
		return a.VMID+"\x00"+a.Provider+"\x00"+a.Model < b.VMID+"\x00"+b.Provider+"\x00"+b.Model
	})
	return rows
}

// mergeAIUsageDays folds rows to one per day, provider and model (the VM is
// dropped), in the order of the first occurrence. It builds new rows.
func mergeAIUsageDays(rows []*AIUsageRow) []*AIUsageRow {
	idx := map[string]*AIUsageRow{}
	out := []*AIUsageRow{}
	for _, u := range rows {
		k := u.Day + "\x00" + u.Provider + "\x00" + u.Model
		m := idx[k]
		if m == nil {
			m = &AIUsageRow{TeamID: u.TeamID, Provider: u.Provider, Model: u.Model, Day: u.Day}
			idx[k] = m
			out = append(out, m)
		}
		m.Requests += u.Requests
		m.InputTokens += u.InputTokens
		m.OutputTokens += u.OutputTokens
		if u.Updated.After(m.Updated) {
			m.Updated = u.Updated
		}
	}
	return out
}

// overlayAIUsage returns base's rows with the meter's unflushed batch (pending,
// keyed usage key + "/" + day) added. perDay selects State.AIUsageDays (one row
// per day) or State.AIUsage (lifetime: the days of a key fold into one row). Rows
// base owns are never changed: a row that gains counts is a copy.
func overlayAIUsage(base, pending map[string]*AIUsageRow, perDay bool) []*AIUsageRow {
	rows := make(map[string]*AIUsageRow, len(base)+len(pending))
	for k, r := range base {
		rows[k] = r
	}
	owned := map[string]bool{}
	for _, d := range pending {
		key := aiUsageKey(d.TeamID, d.VMID, d.Provider, d.Model)
		day := ""
		if perDay {
			key, day = key+"/"+d.Day, d.Day
		}
		cur := rows[key]
		if cur == nil {
			cur = &AIUsageRow{TeamID: d.TeamID, VMID: d.VMID, Provider: d.Provider, Model: d.Model, Day: day}
			owned[key] = true
		} else if !owned[key] {
			cp := *cur
			cur = &cp
			owned[key] = true
		}
		cur.Requests += d.Requests
		cur.InputTokens += d.InputTokens
		cur.OutputTokens += d.OutputTokens
		cur.Unmetered += d.Unmetered
		if d.Updated.After(cur.Updated) {
			cur.Updated = d.Updated
		}
		rows[key] = cur
	}
	out := make([]*AIUsageRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	return out
}
