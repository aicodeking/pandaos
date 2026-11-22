package main

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"sort"
	"strings"
	"time"
)

// r48: a sandbox is pinned to the environment version it started on (boat.dev).
// State.VMEnvPins holds that version and its vars per VM. Editing an environment
// does not change a plain start of a VM already on it; naming the environment at
// a start or fork, or upgrading it (POST /v1/environments/{id}/upgrade), moves the pin.

// pinnedVars are the vars a plain start of VM id replays: its pin's, else (a VM
// from before r48, never pinned) the environment's current ones. None when the
// VM has no environment. A deleted environment does not erase its existing pin.
func (s *server) pinnedVars(st *State, team, id string) map[string]string {
	envID := st.VMEnvs[id]
	if envID == "" {
		return nil
	}
	if pin := st.VMEnvPins[id]; pin != nil {
		return pin.Vars
	}
	if e := st.Environments[envID]; e != nil && e.TeamID == team {
		return e.Env
	}
	return nil
}

// parentPin is the pin a fork's child inherits from its parent: a copy of the
// parent's, or (a parent never pinned) the environment as it is now. nil when
// the parent has no environment.
func (s *server) parentPin(st *State, team, parent string) *EnvPin {
	envID := st.VMEnvs[parent]
	if envID == "" {
		return nil
	}
	if pin := st.VMEnvPins[parent]; pin != nil {
		return &EnvPin{Version: pin.Version, Vars: maps.Clone(pin.Vars)}
	}
	if e := st.Environments[envID]; e != nil && e.TeamID == team {
		return pinOf(e)
	}
	return nil
}

// upgradeResult counts only acknowledged applications (or durable stopped intent).
type upgradeResult struct {
	Upgraded, Live, Stopped, Failed int
}

type upgradeRequest struct {
	AgentIDs upgradeAgentIDs `json:"agentIds,omitempty"`
}

type upgradeAgentIDs []string

func (ids *upgradeAgentIDs) UnmarshalJSON(data []byte) error {
	if strings.TrimSpace(string(data)) == "null" {
		return errors.New("agentIds must be an array, not null")
	}
	return json.Unmarshal(data, (*[]string)(ids))
}

// upgradeEnv validates the entire target set before changing any sandbox. A
// running guest can accept additions and value updates; removing policy requires
// a cold boot because inherited process environments cannot be revoked live.
func (s *server) upgradeEnv(w http.ResponseWriter, r *http.Request, p *principal) {
	e, ok := s.teamEnvironment(w, r, p)
	if !ok {
		return
	}
	var req upgradeRequest
	if r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0 {
		if !decode(w, r, &req) {
			return
		}
	}
	if s.revalidatePrincipal(w, r, p, "admin") == nil {
		return
	}
	st := s.store.Snapshot()
	ids := []string{}
	if req.AgentIDs != nil {
		seen := map[string]bool{}
		for _, ref := range req.AgentIDs {
			id := ref
			if strings.HasPrefix(ref, compatIDPrefix) {
				c := st.CompatIDs[ref]
				if c == nil || c.Team != p.Team.ID {
					writeErr(w, r, 404, "not_found", "sandbox not found")
					return
				}
				id = c.VM
			}
			if !reID.MatchString(id) || st.VMEnvs[id] != e.ID {
				writeErr(w, r, 404, "not_found", "sandbox not found on this environment")
				return
			}
			if !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
	} else {
		for id, envID := range st.VMEnvs {
			if envID == e.ID {
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	// Validate team ownership with boatd too: a stale API row is no authority.
	// On an environment-wide upgrade (req.AgentIDs == nil), a stale VMEnvs row
	// whose VM is gone from boatd (404) is skipped and pruned instead of
	// poisoning the whole rollout.
	var (
		liveIDs []string
		stale   []string
	)
	for _, id := range ids {
		var vm VM
		if err := s.boatd.call(r.Context(), "GET", vmPath(id), nil, &vm); err != nil {
			var be *boatdError
			if req.AgentIDs == nil && errors.As(err, &be) && be.Status == http.StatusNotFound {
				stale = append(stale, id)
				continue
			}
			writeUpstream(w, r, err)
			return
		}
		if str(vm, "team") != p.Team.ID {
			if req.AgentIDs == nil {
				stale = append(stale, id)
				continue
			}
			writeErr(w, r, 404, "not_found", "sandbox not found")
			return
		}
		liveIDs = append(liveIDs, id)
	}
	if len(stale) > 0 {
		_ = s.store.Apply(func(st *State) error {
			for _, id := range stale {
				if st.VMEnvs[id] == e.ID {
					delete(st.VMEnvs, id)
					delete(st.VMEnvPins, id)
				}
			}
			return nil
		})
	}
	ids = liveIDs
	res := upgradeResult{}
	for _, id := range ids {
		outcome := s.upgradeOne(r, p, e, id)
		switch outcome {
		case "live":
			res.Upgraded++
			res.Live++
		case "stopped":
			res.Upgraded++
			res.Stopped++
		case "failed":
			res.Failed++
		}
	}
	writeJSON(w, 200, map[string]any{"environment": e.ID, "version": e.Version,
		"upgraded": res.Upgraded, "upgraded_live": res.Live, "upgraded_stopped": res.Stopped, "failed": res.Failed})
}

func (s *server) upgradeOne(r *http.Request, p *principal, latest *Environment, id string) string {
	unlock := startLocks.lock(id)
	defer unlock()
	release := s.markEnvPush(id)
	defer release()
	var vm VM
	if err := s.boatd.call(r.Context(), "GET", vmPath(id), nil, &vm); err != nil || str(vm, "team") != p.Team.ID {
		return "failed"
	}
	if err := s.adoptCopyPolicy(vm); err != nil {
		return "failed"
	}
	st := s.store.Snapshot()
	if _, err := requirePrincipalIn(st, p, "admin", time.Now().UTC()); err != nil {
		return "failed"
	}
	current := st.Environments[latest.ID]
	if current == nil || current.TeamID != p.Team.ID || current.Version != latest.Version || st.VMEnvs[id] != latest.ID ||
		s.noEnvRun(id) || vm["pending_copy"] == true || st.VMTransitions[id] != nil {
		return "failed"
	}
	if pin := st.VMEnvPins[id]; pin != nil && pin.Version == latest.Version && maps.Equal(pin.Vars, latest.Env) && s.envErrorOf(vm) == "" {
		return "unchanged"
	}
	ne := &namedEnv{env: latest, sel: []string{defaultSelection}, keepSel: st.ForeignVMs[id]}
	if latest.Secrets != nil {
		ne.sel = append([]string{}, *latest.Secrets...)
	}
	selection := ne.sel
	if ne.keepSel {
		selection = s.vmSecretsOf(id, vm)
	}
	if s.checkSecretNames(p.Team.ID, selection) != "" {
		return "failed"
	}
	running := str(vm, "state") == "running"
	if !running && str(vm, "state") != "stopped" {
		return "failed"
	}
	// A legacy sandbox without a pin has no authoritative previous variable
	// set. It must not be called a successful live cleanup by guessing.
	removal := st.VMEnvPins[id] == nil || s.forkEnvNeedsFreshBoot(p.Team.ID, id, false, &namedEnv{env: latest, sel: selection, keepSel: ne.keepSel}, st.VMExtraEnv[id])
	if running && removal {
		return "failed"
	}
	if removal && st.ForeignVMs[id] {
		return "failed"
	} // no trusted preboot cleanup evidence for foreign disks
	if !running {
		// Persist the effective policy and the pin together. A removal also carries
		// durable cold-boot intent, so an empty resume cannot restore old memory.
		if err := s.store.Update(func(st *State) error {
			if _, err := requirePrincipalIn(st, p, "admin", time.Now().UTC()); err != nil {
				return err
			}
			if st.VMEnvs[id] != latest.ID || st.Environments[latest.ID] == nil || st.Environments[latest.ID].Version != latest.Version {
				return errConflict
			}
			st.VMEnvPins[id] = pinOf(latest)
			st.VMSecrets[id] = append([]string{}, selection...)
			if removal {
				st.VMTransitions[id] = transitionFor(false, ne, selection)
			}
			return nil
		}); err != nil {
			return "failed"
		}
		return "stopped"
	}
	vars, err := s.teamEnv(p.Team.ID, selection)
	if err != nil {
		return "failed"
	}
	runNames := envNames(vars)
	maps.Copy(vars, latest.Env)
	maps.Copy(vars, st.VMExtraEnv[id])
	ai, err := s.aiEnvRun(id, p.Team.ID, false, false)
	if err != nil {
		return "failed"
	}
	maps.Copy(vars, ai)
	maps.Copy(vars, s.guestEnvExisting(id, p.Team.ID, vars, p.has("exec") && p.has("vms:read")))
	if _, err := requirePrincipalIn(s.store.Snapshot(), p, "admin", time.Now().UTC()); err != nil {
		return "failed"
	}
	// An unanswered push may have landed. Commit every possibly delivered name
	// before the side effect, while keeping an unknown previous run unknown.
	if err := s.store.Update(func(st *State) error {
		if _, err := requirePrincipalIn(st, p, "admin", time.Now().UTC()); err != nil {
			return err
		}
		if st.VMEnvs[id] != latest.ID || st.Environments[latest.ID] == nil || st.Environments[latest.ID].Version != latest.Version {
			return errConflict
		}
		setRunNames(st, id, runNames, true)
		return nil
	}); err != nil {
		return "failed"
	}
	if err := s.boatd.call(r.Context(), "POST", vmPath(id, "env"), map[string]any{"vars": vars}, nil); err != nil {
		return "failed"
	}
	// Revalidate at commit as well as immediately before the guest side effect.
	if err := s.store.Update(func(st *State) error {
		if _, err := requirePrincipalIn(st, p, "admin", time.Now().UTC()); err != nil {
			return err
		}
		if st.VMEnvs[id] != latest.ID || st.Environments[latest.ID] == nil || st.Environments[latest.ID].Version != latest.Version {
			return errConflict
		}
		st.VMEnvPins[id] = pinOf(latest)
		st.VMSecrets[id] = append([]string{}, selection...)
		return nil
	}); err != nil {
		s.stopFailClosed(r, id, s.readRunMark(id))
		return "failed"
	}
	s.recordEnvResult(id, nil)
	return "live"
}

// compatUpgradeEnv is boat.dev's upgradeSandboxEnvironment.
func (s *server) compatUpgradeEnv(w http.ResponseWriter, r *http.Request, p *principal) {
	e, ok := s.pathEnv(w, r, p)
	if !ok {
		return
	}
	var req upgradeRequest
	if r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0 {
		if !decode(w, r, &req) {
			return
		}
	}
	var body any
	if req.AgentIDs != nil {
		body = map[string]any{"agentIds": req.AgentIDs}
	}
	res := s.callNative(r, p, "POST /v1/environments/{id}/upgrade", s.upgradeEnv, e.ID, body)
	if !res.ok() {
		res.relay(w)
		return
	}
	var out struct {
		Version  int `json:"version"`
		Upgraded int `json:"upgraded"`
		Live     int `json:"upgraded_live"`
		Stopped  int `json:"upgraded_stopped"`
		Failed   int `json:"failed"`
	}
	if err := json.Unmarshal(res.body.Bytes(), &out); err != nil {
		writeErr(w, r, 500, "internal", "unreadable upgrade answer")
		return
	}
	writeJSON(w, 200, map[string]any{"success": out.Failed == 0, "upgraded": out.Upgraded, "upgradedLive": out.Live,
		"upgradedStopped": out.Stopped, "failed": out.Failed, "latestVersionId": envUUID(e.ID, out.Version)})
}

// envVersions are the compat "versions" of environment e: the latest one first,
// then every older version some sandbox is still pinned to, each with the number
// of sandboxes on it. A sandbox never pinned (from before r48) counts on the latest.
func envVersions(st *State, e *Environment) []any {
	counts := map[int]int{e.Version: 0}
	for id, envID := range st.VMEnvs {
		if envID != e.ID {
			continue
		}
		if pin := st.VMEnvPins[id]; pin != nil {
			counts[pin.Version]++
		} else {
			counts[e.Version]++
		}
	}
	nums := make([]int, 0, len(counts))
	for v := range counts {
		nums = append(nums, v)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(nums)))
	out := make([]any, 0, len(nums))
	for _, v := range nums {
		created := e.Created
		if v == e.Version {
			created = e.Updated
		}
		out = append(out, map[string]any{"id": envUUID(e.ID, v), "versionNumber": v, "sandboxCount": counts[v], "createdAt": created})
	}
	return out
}
