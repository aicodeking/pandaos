package main

import "sort"

// aiUsageLifeMaxRows caps the lifetime rows of State.AIUsage per team (one row
// per VM/provider/model, so a team that creates VMs without end would grow it
// without bound). Past it the least recently used rows are folded into the
// team's aggregate row (VM aiDeletedVM) as foldAIRow does for a deleted VM: the
// team's totals stay, the usage of the folded VMs is no longer listed by VM. A
// variable for tests.
var aiUsageLifeMaxRows = 2000

// capAIUsageLife brings every team's lifetime rows to aiUsageLifeMaxRows. It
// reports whether it folded a row (the per-team index is then to be dropped).
// One pass over the table when no team is over the cap; only an over-cap team's
// rows are sorted.
func capAIUsageLife(st *State) bool {
	if len(st.AIUsage) <= aiUsageLifeMaxRows {
		return false
	}
	perTeam := map[string]int{}
	for _, r := range st.AIUsage {
		if r != nil {
			perTeam[r.TeamID]++
		}
	}
	changed := false
	for team, n := range perTeam {
		if n <= aiUsageLifeMaxRows {
			continue
		}
		var keys []string // the team's rows that are not the aggregate
		for k, r := range st.AIUsage {
			if r != nil && r.TeamID == team && r.VMID != aiDeletedVM {
				keys = append(keys, k)
			}
		}
		sort.Slice(keys, func(i, j int) bool {
			a, b := st.AIUsage[keys[i]], st.AIUsage[keys[j]]
			if !a.Updated.Equal(b.Updated) {
				return a.Updated.Before(b.Updated)
			}
			return keys[i] < keys[j]
		})
		for _, k := range keys {
			if n <= aiUsageLifeMaxRows {
				break
			}
			before := len(st.AIUsage)
			foldAIRow(st.AIUsage, k, st.AIUsage[k])
			n += len(st.AIUsage) - before // -1, or 0 when the fold made the aggregate row
			changed = true
		}
	}
	if changed {
		st.dropAIUsageIndex()
	}
	return changed
}
