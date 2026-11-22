package main

import (
	"errors"
	"maps"
	"net/http"
	"time"
)

const (
	foreignLabel    = "boat.foreign"
	restrictedLabel = "boat.restricted"
	noSelfLabel     = "boat.no_self_api"
	protectedLabel  = "boat.protected_lineage"
)

// CredentialTransition is durable intent, not evidence that old memory or the
// guest environment has been discarded. Only a successful fresh boot and guest
// update may remove it. Retrying after a crash repeats both steps.
type CredentialTransition struct {
	NoEnv         bool              `json:"no_env,omitempty"`
	EnvID         string            `json:"environment,omitempty"`
	Version       int               `json:"version,omitempty"`
	Vars          map[string]string `json:"vars,omitempty"`
	Extra         map[string]string `json:"extra,omitempty"`
	Selection     []string          `json:"selection"`
	KeepSelection bool              `json:"keep_selection,omitempty"`
}

func cloneTransition(t *CredentialTransition) *CredentialTransition {
	if t == nil {
		return nil
	}
	cp := *t
	cp.Vars = maps.Clone(t.Vars)
	cp.Extra = maps.Clone(t.Extra)
	if t.Selection != nil {
		cp.Selection = append([]string{}, t.Selection...)
	}
	return &cp
}

func transitionFor(noEnv bool, ne *namedEnv, selection []string) *CredentialTransition {
	t := &CredentialTransition{NoEnv: noEnv, Selection: selection}
	if ne != nil {
		t.KeepSelection = ne.keepSel
		if ne.env != nil {
			t.EnvID, t.Version, t.Vars = ne.env.ID, ne.env.Version, maps.Clone(ne.env.Env)
		}
	}
	return cloneTransition(t)
}

func (t *CredentialTransition) named(team string) *namedEnv {
	if t == nil || t.NoEnv {
		return nil
	}
	n := &namedEnv{sel: t.Selection, keepSel: t.KeepSelection}
	if t.EnvID != "" {
		n.env = &Environment{ID: t.EnvID, TeamID: team, Version: t.Version, Env: maps.Clone(t.Vars)}
	}
	return n
}

func (s *server) beginCredentialTransition(id string, t *CredentialTransition) error {
	return s.store.Update(func(st *State) error { st.VMTransitions[id] = cloneTransition(t); return nil })
}

func (s *server) finishCredentialTransition(id string) error {
	return s.store.Update(func(st *State) error { delete(st.VMTransitions, id); return nil })
}

func (s *server) refusePendingCredentialCommand(w http.ResponseWriter, r *http.Request, id string) bool {
	if s.store.Snapshot().VMTransitions[id] == nil {
		return false
	}
	switch info(r).pattern {
	case "POST /v1/vms/{id}/commands", "POST /v1/vms/{id}/exec",
		"POST /v1/vms/{id}/prompt", "POST /v1/vms/{id}/prompts",
		"POST /api/v1/sandboxes/{id}/commands", "POST /api/v1/sandboxes/{id}/prompt":
		writeErr(w, r, http.StatusConflict, "sandbox_not_ready", "a credential transition is pending; resume the VM to retry")
		return true
	}
	return false
}

// Rehydrate reserved creation labels before minting/adopting any credentials.
// Labels only add restrictions; the absence of labels retains legacy policy.
func rehydrateCopyPolicy(st *State, vm VM) {
	id, team := str(vm, "id"), str(vm, "team")
	if id == "" || st.Teams[team] == nil {
		return
	}
	l, _ := vm["labels"].(map[string]any)
	newForeign := l[foreignLabel] == "1" && !st.ForeignVMs[id]
	if l[foreignLabel] == "1" {
		st.ForeignVMs[id] = true
	}
	if st.ForeignVMs[id] && st.AIVMs[id] == nil {
		st.AIVMs[id] = &AIVM{TeamID: team, Blocked: true}
	}
	if labeledNoEnv(vm) {
		if _, ok := st.NoEnvRuns[id]; !ok {
			st.NoEnvRuns[id] = time.Now().UTC()
		}
		st.VMSecrets[id] = []string{}
	}
	if st.VMTokens[id] == nil && l[noSelfLabel] == "1" {
		// A disabled self API must remain disabled even if the entire token row
		// was lost. Gen 1 is a lookup identity only; NoSelfAPI forbids its use.
		st.VMTokens[id] = &VMToken{TeamID: team, Gen: 1, NoSelfAPI: true}
	}
	if t := st.VMTokens[id]; t != nil && !t.Retired && t.TeamID == team {
		if st.ForeignVMs[id] || l[restrictedLabel] == "1" || l[noSelfLabel] == "1" || labeledNoEnv(vm) {
			t = st.cowVMToken(id)
			if st.ForeignVMs[id] || l[restrictedLabel] == "1" {
				t.RestrictSelf = true
			}
			if l[noSelfLabel] == "1" {
				t.NoSelfAPI = true
			}
			if l[restrictedLabel] == "1" && t.NarrowGen == 0 {
				t.NarrowGen, t.NarrowAt = t.Gen, time.Now().UTC()
			}
			if labeledNoEnv(vm) {
				t.NoEnvGen = t.Gen
			}
		}
	}
	if a := st.AIVMs[id]; a != nil && newForeign {
		st.cowAIVM(id).Blocked = true
	}
}

// needsCopyPolicyRehydrate reports whether vm's creation labels require any
// state mutation in st. Pure read-only check so callers skip Store.Update when
// policy is already reflected.
func needsCopyPolicyRehydrate(st *State, vm VM) bool {
	l, _ := vm["labels"].(map[string]any)
	id, team := str(vm, "id"), str(vm, "team")
	if id == "" || (l[foreignLabel] != "1" && l[restrictedLabel] != "1" && !labeledNoEnv(vm) && l[noSelfLabel] != "1" && !st.ForeignVMs[id]) {
		return false
	}
	t := st.VMTokens[id]
	_, marked := st.NoEnvRuns[id]
	needs := (l[foreignLabel] == "1" && !st.ForeignVMs[id]) || (labeledNoEnv(vm) && !marked)
	needs = needs || (t == nil && l[noSelfLabel] == "1")
	needs = needs || (st.ForeignVMs[id] && st.AIVMs[id] == nil)
	if t != nil && !t.Retired && (team == "" || t.TeamID == team) {
		needs = needs || (st.ForeignVMs[id] && !t.RestrictSelf)
		needs = needs || (l[restrictedLabel] == "1" && (!t.RestrictSelf || t.NarrowGen == 0))
		needs = needs || (l[noSelfLabel] == "1" && !t.NoSelfAPI)
		needs = needs || (labeledNoEnv(vm) && t.NoEnvGen != t.Gen)
	}
	return needs
}

func (s *server) adoptCopyPolicy(vm VM) error {
	if !needsCopyPolicyRehydrate(s.store.Snapshot(), vm) {
		return nil
	}
	return s.store.Update(func(st *State) error {
		if st.Teams[str(vm, "team")] == nil {
			return errors.New("VM team no longer exists")
		}
		rehydrateCopyPolicy(st, vm)
		return nil
	})
}

// protectedRunPolicy uses only the VM already read under the lifecycle lock.
// Failure to read policy can withhold secrets, but cannot certify copied memory.
func protectedRunPolicy(st *State, id string, vm VM) bool {
	if vm == nil || str(vm, "id") != id {
		return false
	}
	_, marked := st.NoEnvRuns[id]
	return marked || labeledNoEnv(vm)
}

// completeCredentialStart commits the effective policy and clears the fence in
// one durable write. The successful guest reply proves env delivery, not tenant
// disk cleanup. A failed commit leaves the pending intent for a fresh retry.
func (s *server) completeCredentialStart(p *principal, id string, ne *namedEnv, extra map[string]string, names []string, noEnv bool) error {
	return s.store.Update(func(st *State) error {
		if _, err := requirePrincipalIn(st, p, "vms:write", time.Now()); err != nil {
			return err
		}
		if tok := st.VMTokens[id]; tok != nil && (tok.Retired || tok.TeamID != p.Team.ID) {
			return errors.New("VM credentials are retired")
		}
		if !noEnv {
			if sel, ok := ne.selection(); ok {
				st.VMSecrets[id] = append([]string{}, sel...)
			}
			if ne != nil {
				if ne.env != nil {
					st.VMEnvs[id], st.VMEnvPins[id] = ne.env.ID, pinOf(ne.env)
				} else {
					delete(st.VMEnvs, id)
					delete(st.VMEnvPins, id)
				}
			}
			if len(extra) > 0 {
				st.VMExtraEnv[id] = maps.Clone(extra)
			}
		}
		setRunNames(st, id, names, false)
		delete(st.VMTransitions, id)
		return nil
	})
}
