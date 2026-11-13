package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

// Environments: a named base image + setup script + env vars + secret
// selection. `POST /v1/vms {environment}` runs the setup in the new VM as a
// detached command. `POST /v1/environments/{id}/build` runs it once in a
// builder VM and snapshots the result, so later VMs start from the snapshot
// with the setup already done. The builder stays stopped: deleting a VM deletes
// its snapshots. Secrets given to the build end up on the snapshot's disk.

const (
	maxEnvsPerTeam = 100
	maxSetupScript = 256 << 10
	buildExecS     = 3600
)

type envReq struct {
	Name        *string            `json:"name"`
	Image       *string            `json:"image"`
	Size        *string            `json:"size"`
	SetupScript *string            `json:"setup_script"`
	Env         *map[string]string `json:"env"`
	Secrets     *[]string          `json:"secrets"`
	IsDefault   *bool              `json:"is_default"`
}

func (s *server) validateEnvReq(team string, e *envReq) string {
	switch {
	case e.Name != nil && !reVMName.MatchString(*e.Name):
		return "name must be lowercase letters, digits and dashes (max 63)"
	case e.Image != nil && *e.Image != "" && len(s.cfg.Images) > 0 && !slices.Contains(s.cfg.Images, *e.Image):
		return "unknown image " + *e.Image
	case e.Size != nil && *e.Size != "" && !reVMName.MatchString(*e.Size):
		return "invalid size"
	case e.SetupScript != nil && (len(*e.SetupScript) > maxSetupScript || strings.ContainsRune(*e.SetupScript, 0)):
		return "setup_script must be text of at most 256 KiB"
	}
	if e.Env != nil {
		if msg := validEnv(*e.Env); msg != "" {
			return msg
		}
	}
	if e.Secrets != nil {
		return s.checkSecretNames(team, *e.Secrets)
	}
	return ""
}

// applyEnvReq updates env in place and reports whether what it builds changed.
func applyEnvReq(env *Environment, e *envReq) bool {
	changed := false
	set := func(dst *string, v *string) {
		if v != nil && *dst != *v {
			*dst, changed = *v, true
		}
	}
	if e.Name != nil {
		env.Name = *e.Name
	}
	set(&env.Image, e.Image)
	set(&env.Size, e.Size)
	set(&env.SetupScript, e.SetupScript)
	if e.Env != nil {
		env.Env, changed = *e.Env, true
	}
	if e.Secrets != nil {
		cp := append([]string{}, *e.Secrets...)
		env.Secrets, changed = &cp, true
	}
	if e.IsDefault != nil {
		env.IsDefault = *e.IsDefault
	}
	return changed
}

// teamEnvBytes is what the team's environments add to state.json.
func teamEnvBytes(st *State, team string) int {
	n := 0
	for _, e := range st.Environments {
		if e.TeamID != team {
			continue
		}
		n += len(e.SetupScript)
		for k, v := range e.Env {
			n += len(k) + len(v)
		}
	}
	return n
}

func envByName(st *State, team, name string) *Environment {
	for _, e := range st.Environments {
		if e.TeamID == team && e.Name == name {
			return e
		}
	}
	return nil
}

func defaultEnv(st *State, team string) *Environment {
	for _, e := range st.Environments {
		if e.TeamID == team && e.IsDefault {
			return e
		}
	}
	return nil
}

// clearOtherDefaults keeps one default per team.
func clearOtherDefaults(st *State, keep *Environment) {
	if !keep.IsDefault {
		return
	}
	for _, e := range st.Environments {
		if e.TeamID == keep.TeamID && e.ID != keep.ID {
			e.IsDefault = false
		}
	}
}

func (s *server) listEnvs(w http.ResponseWriter, r *http.Request, p *principal) {
	out := []*Environment{}
	for _, e := range s.store.Snapshot().Environments {
		if e.TeamID == p.Team.ID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, 200, map[string]any{"environments": out})
}

func (s *server) createEnv(w http.ResponseWriter, r *http.Request, p *principal) {
	var e envReq
	if !decode(w, r, &e) {
		return
	}
	if e.Name == nil {
		writeErr(w, r, 400, "invalid_request", "name is required")
		return
	}
	if msg := s.validateEnvReq(p.Team.ID, &e); msg != "" {
		writeErr(w, r, 400, "invalid_request", msg)
		return
	}
	now := time.Now().UTC()
	env := &Environment{ID: "env_" + randHex(8), TeamID: p.Team.ID, Version: 1, Created: now, Updated: now}
	applyEnvReq(env, &e)
	conflict := ""
	err := s.store.Update(func(st *State) error {
		n := 0
		for _, x := range st.Environments {
			if x.TeamID == p.Team.ID {
				n++
			}
		}
		if n >= maxEnvsPerTeam {
			return errLimit
		}
		if envByName(st, p.Team.ID, env.Name) != nil {
			conflict = "an environment named " + env.Name + " exists"
			return errConflict
		}
		st.Environments[env.ID] = env
		if teamEnvBytes(st, p.Team.ID) > maxEnvBytesPerTeam {
			return errLimit
		}
		clearOtherDefaults(st, env)
		return nil
	})
	if conflict != "" {
		writeErr(w, r, 409, "conflict", conflict)
		return
	}
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 201, env)
}

// teamEnvironment finds {id} (or a name) in the caller's team.
func (s *server) teamEnvironment(w http.ResponseWriter, r *http.Request, p *principal) (*Environment, bool) {
	st := s.store.Snapshot()
	ref := r.PathValue("id")
	e := st.Environments[ref]
	if e == nil || e.TeamID != p.Team.ID {
		e = envByName(st, p.Team.ID, ref)
	}
	if e == nil {
		writeErr(w, r, 404, "not_found", "environment not found")
		return nil, false
	}
	return e, true
}

func (s *server) getEnv(w http.ResponseWriter, r *http.Request, p *principal) {
	if e, ok := s.teamEnvironment(w, r, p); ok {
		writeJSON(w, 200, e)
	}
}

func (s *server) updateEnv(w http.ResponseWriter, r *http.Request, p *principal) {
	var e envReq
	if !decode(w, r, &e) {
		return
	}
	if msg := s.validateEnvReq(p.Team.ID, &e); msg != "" {
		writeErr(w, r, 400, "invalid_request", msg)
		return
	}
	cur, ok := s.teamEnvironment(w, r, p)
	if !ok {
		return
	}
	var out *Environment
	conflict := ""
	err := s.store.Update(func(st *State) error {
		env := st.Environments[cur.ID]
		if env == nil {
			return errConflict
		}
		if e.Name != nil {
			if o := envByName(st, p.Team.ID, *e.Name); o != nil && o.ID != env.ID {
				conflict = "an environment named " + *e.Name + " exists"
				return errConflict
			}
		}
		if applyEnvReq(env, &e) {
			env.Version++ // an existing build no longer matches
		}
		if teamEnvBytes(st, p.Team.ID) > maxEnvBytesPerTeam {
			return errLimit
		}
		env.Updated = time.Now().UTC()
		clearOtherDefaults(st, env)
		out = env
		return nil
	})
	if conflict != "" {
		writeErr(w, r, 409, "conflict", conflict)
		return
	}
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, out)
}

func (s *server) deleteEnv(w http.ResponseWriter, r *http.Request, p *principal) {
	cur, ok := s.teamEnvironment(w, r, p)
	if !ok {
		return
	}
	if err := s.store.Update(func(st *State) error { delete(st.Environments, cur.ID); return nil }); err != nil {
		s.storeErr(w, r, err)
		return
	}
	if cur.Build != nil && cur.Build.VMID != "" {
		if err := s.discardCopyVM(cur.Build.VMID); err != nil {
			log.Printf("%s: delete env builder %s: %v", info(r).id, cur.Build.VMID, err)
			writeErr(w, r, 500, "cleanup_failed", "environment removed but its builder could not be retired and deleted")
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// buildEnv starts a build; poll GET /v1/environments/{id} for build.state.
func (s *server) buildEnv(w http.ResponseWriter, r *http.Request, p *principal) {
	cur, ok := s.teamEnvironment(w, r, p)
	if !ok {
		return
	}
	var out *Environment
	busy := false
	err := s.store.Update(func(st *State) error {
		env := st.Environments[cur.ID]
		if env == nil {
			return errConflict
		}
		if env.Build != nil && env.Build.State == "building" {
			busy = true
			return errConflict
		}
		old := env.Build
		env.Build = &EnvBuild{State: "building", Version: env.Version, At: time.Now().UTC()}
		if old != nil && old.State == "ready" { // keep serving the old one until replaced
			env.Build.VMID, env.Build.SnapshotID = old.VMID, old.SnapshotID
		}
		out = env
		return nil
	})
	if busy {
		writeErr(w, r, 409, "conflict", "a build is already running")
		return
	}
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	go s.runEnvBuild(*cur, p.Team.ID)
	writeJSON(w, 202, out)
}

// runEnvBuild has no request context: it outlives the POST.
func (s *server) runEnvBuild(env Environment, team string) {
	ctx, cancel := context.WithTimeout(s.baseCtx(), time.Duration(buildExecS+600)*time.Second)
	defer cancel()
	vmID, snapID, err := s.envBuildSteps(ctx, env, team)
	uerr := s.store.Update(func(st *State) error {
		cur := st.Environments[env.ID]
		if cur == nil {
			return nil
		}
		prev := ""
		if cur.Build != nil {
			prev = cur.Build.VMID
		}
		b := &EnvBuild{State: "ready", Version: env.Version, VMID: vmID, SnapshotID: snapID, At: time.Now().UTC()}
		if err != nil {
			b = &EnvBuild{State: "failed", Version: env.Version, Error: err.Error(), At: time.Now().UTC()}
			if cur.Build != nil { // the last good snapshot stays usable while versions match
				b.VMID, b.SnapshotID = prev, cur.Build.SnapshotID
			}
		} else if prev != "" && prev != vmID {
			go func() {
				if derr := s.discardCopyVM(prev); derr != nil {
					log.Printf("env %s: delete old builder %s: %v", env.ID, prev, derr)
				}
			}()
		}
		cur.Build = b
		return nil
	})
	if uerr != nil {
		log.Printf("env %s: record build: %v", env.ID, uerr)
	}
}

func (s *server) envBuildSteps(ctx context.Context, env Environment, team string) (vmID, snapID string, err error) {
	run, err := s.envRunVars(env, team)
	if err != nil {
		return "", "", err
	}
	body := map[string]any{"name": "env-build-" + randHex(4), "size": env.Size, "image": env.Image, "team": team,
		"idle_stop_s": -1, "labels": map[string]string{"boat.env": env.ID}, "env": run}
	var vm VM
	if err := s.boatd.call(ctx, "POST", "/v1/vms", body, &vm); err != nil {
		return "", "", fmt.Errorf("create builder: %w", err)
	}
	vmID = str(vm, "id")
	fail := func(e error) (string, string, error) {
		if derr := s.discardCopyVM(vmID); derr != nil {
			log.Printf("env %s: delete failed builder %s: %v", env.ID, vmID, derr)
		}
		return "", "", e
	}
	if strings.TrimSpace(env.SetupScript) != "" {
		req, _ := http.NewRequestWithContext(ctx, "POST", "http://boat-api/internal/env-build", nil)
		_, stderr, exit, xerr := s.execCapture(req, vmID, []string{"bash", "-lc", "set -e\n" + env.SetupScript}, buildExecS)
		if xerr != nil {
			return fail(fmt.Errorf("run setup: %w", xerr))
		}
		if exit != 0 {
			if len(stderr) > 2000 {
				stderr = stderr[len(stderr)-2000:]
			}
			return fail(fmt.Errorf("setup exited %d: %s", exit, strings.TrimSpace(stderr)))
		}
	}
	snap, err := s.captureSnapshot(ctx, vmID, team, fmt.Sprintf("env-%s-v%d", env.Name, env.Version))
	if err != nil {
		return fail(fmt.Errorf("snapshot: %w", err))
	}
	if err := s.boatd.call(ctx, "POST", vmPath(vmID, "stop"), nil, nil); err != nil {
		log.Printf("env %s: stop builder %s: %v", env.ID, vmID, err)
	}
	return vmID, str(snap, "id"), nil
}

// envRunVars: the environment's secret selection plus its plain vars.
func (s *server) envRunVars(env Environment, team string) (map[string]string, error) {
	only := []string{defaultSelection}
	if env.Secrets != nil {
		only = append([]string{}, *env.Secrets...)
	}
	vars, err := s.teamEnv(team, only)
	if err != nil {
		return nil, err
	}
	for k, v := range env.Env {
		vars[k] = v
	}
	return vars, nil
}
