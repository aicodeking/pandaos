package main

import (
	"errors"
	"maps"
	"net/http"
	"strings"
	"time"
)

// namedEnv is an environment a start or fork names (boat.dev's `resume
// --environment` and `fork --environment`): what create would take from it. env
// is nil for "base" on a team with no default environment: no vars, and the
// default selection.
type namedEnv struct {
	env *Environment
	sel []string // the secret selection the environment gives (never nil unless keepSel)
	// keepSel: the VM (or fork parent) boots a foreign disk (an import, a share, or
	// a copy of one). Its secret selection is not the environment's to change: the
	// environment gives its vars only, as at create (foreignSecrets), and the VM
	// keeps the selection it has.
	keepSel bool
}

// selection is the secret selection the start or fork takes from the
// environment; false when the VM keeps its own (a foreign disk).
func (n *namedEnv) selection() ([]string, bool) {
	if n == nil || n.keepSel {
		return nil, false
	}
	return n.sel, true
}

// vars: the environment's variables (a copy, never nil).
func (n *namedEnv) vars() map[string]string {
	out := map[string]string{}
	if n != nil && n.env != nil {
		for k, v := range n.env.Env {
			out[k] = v
		}
	}
	return out
}

const (
	unknownEnvPrefix      = "unknown environment "
	errUnknownEnvironment = "unknown_environment"
)

// envRefusalCode is the error code of a namedStartEnv refusal by its status.
func envRefusalCode(status int) string {
	if status == http.StatusNotFound {
		return errUnknownEnvironment
	}
	return "invalid_request"
}

// namedStartEnv resolves the environment name of a start or fork the way
// create's resolveEnv does (resolveEnvRef): "" = none named; any other value an
// environment id, else an exact name, else "base" = the team default when no
// environment is named "base"; an unknown one 404 unknown_environment, and a name
// together with no_env 400, both before anything is stopped, started or copied. The selection is the environment's own, else
// @default, as at create.
func (s *server) namedStartEnv(team, vmID, name string, noEnv bool) (*namedEnv, int, string) {
	if name == "" {
		return nil, 0, ""
	}
	if noEnv {
		return nil, http.StatusBadRequest, "environment and no_env conflict"
	}
	st := s.store.Snapshot()
	env, found := resolveEnvRef(st, team, name)
	if !found {
		return nil, http.StatusNotFound, unknownEnvPrefix + name
	}
	if st.ForeignVMs[vmID] {
		return &namedEnv{env: env, keepSel: true}, 0, ""
	}
	sel := []string{defaultSelection}
	if env != nil && env.Secrets != nil {
		sel = append([]string{}, *env.Secrets...)
	}
	if msg := s.checkSecretNames(team, sel); msg != "" { // as create: a deleted secret refuses
		return nil, http.StatusBadRequest, msg
	}
	return &namedEnv{env: env, sel: sel}, 0, ""
}

// recordStartEnv makes ne the VM's environment and secret selection once a start
// with it has taken effect (later prompts, execs and starts use it). A foreign
// VM keeps its selection (ne.keepSel): only the environment's vars stay.
func (s *server) recordStartEnv(p *principal, vmID string, ne *namedEnv) error {
	team := p.Team.ID
	return s.store.Update(func(st *State) error {
		if _, err := requirePrincipalIn(st, p, "vms:write", time.Now()); err != nil {
			return err
		}
		if st.Teams[team] == nil {
			return errors.New("VM team no longer exists")
		}
		if sel, ok := ne.selection(); ok {
			st.VMSecrets[vmID] = append([]string{}, sel...)
		}
		if ne.env != nil {
			st.VMEnvs[vmID] = ne.env.ID
			st.VMEnvPins[vmID] = pinOf(ne.env)
		} else {
			delete(st.VMEnvs, vmID)
			delete(st.VMEnvPins, vmID)
		}
		return nil
	})
}

// recordExtraEnv makes env the VM's per-sandbox env (replayed on every later
// start), replacing any stored one.
func (s *server) recordExtraEnv(p *principal, vmID string, env map[string]string) error {
	team := p.Team.ID
	return s.store.Update(func(st *State) error {
		if _, err := requirePrincipalIn(st, p, "vms:write", time.Now()); err != nil {
			return err
		}
		if st.Teams[team] == nil {
			return errors.New("VM team no longer exists")
		}
		st.VMExtraEnv[vmID] = maps.Clone(env)
		return nil
	})
}

// snapshotSourceExtraEnv: the per-sandbox env of the VM snapshot sid was taken
// from (nil when unknown, or the VM is gone).
func (s *server) snapshotSourceExtraEnv(sid string) map[string]string {
	st := s.store.Snapshot()
	rec := st.VMSecrets[snapSourceKey(sid)]
	if len(rec) == 0 || !strings.HasPrefix(rec[0], snapSourceVM) {
		return nil
	}
	return maps.Clone(st.VMExtraEnv[strings.TrimPrefix(rec[0], snapSourceVM)])
}

// forkEnvNeedsFreshBoot: a fork with an environment whose selection does not
// cover everything the parent's run holds (its recorded names; an unknown record
// holds every team secret) is released by a cold boot, never by resuming the
// parent's memory, as a create from a snapshot with a narrower selection is. A
// no_env parent's memory holds no team secrets.
func (s *server) forkEnvNeedsFreshBoot(team, parent string, parentNoEnv bool, ne *namedEnv, extra map[string]string) bool {
	sel, ok := ne.selection()
	if ne == nil || parentNoEnv {
		return false // no named environment, or no team environment in the parent run
	}
	st := s.store.Snapshot()
	// A per-sandbox override can keep a pinned variable after the environment
	// drops it. Compare effective values, including secret names and extra vars.
	haveVars := ne.vars()
	if !ok {
		sel = s.vmSecretsOf(parent, nil)
	}
	have := selectionNames(st, team, sel)
	for key := range s.pinnedVars(st, team, parent) {
		if _, kept := haveVars[key]; !kept && !have[key] {
			if _, custom := extra[key]; !custom {
				return true
			}
		}
	}
	held, known := runNamesOf(st, parent)
	if !known {
		held = envNamesOfTeam(st, team)
	}
	for _, n := range held {
		if !have[n] {
			return true
		}
	}
	return false
}

// envNamesOfTeam: every team secret's name.
func envNamesOfTeam(st *State, team string) []string {
	var out []string
	for _, sec := range st.Secrets {
		if sec.TeamID == team {
			out = append(out, sec.Name)
		}
	}
	return out
}
