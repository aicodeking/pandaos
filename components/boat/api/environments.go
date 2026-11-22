package main

import (
	"context"
	"errors"
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
	Name         *string            `json:"name"`
	Image        *string            `json:"image"`
	Size         *string            `json:"size"`
	SetupScript  *string            `json:"setup_script"`
	Env          *map[string]string `json:"env"`
	Secrets      *[]string          `json:"secrets"`
	ResetSecrets bool               `json:"reset_secrets"`
	IsDefault    *bool              `json:"is_default"`
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
	case e.Secrets != nil && e.ResetSecrets:
		return "secrets and reset_secrets conflict"
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
	} else if e.ResetSecrets && env.Secrets != nil {
		env.Secrets, changed = nil, true
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
		if _, err := requirePrincipalIn(st, p, "admin", time.Now().UTC()); err != nil {
			return err
		}
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
		if _, err := requirePrincipalIn(st, p, "admin", time.Now().UTC()); err != nil {
			return err
		}
		env := st.Environments[cur.ID]
		if env == nil || env.TeamID != p.Team.ID {
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
	s.deleteEnvWithPolicy(w, r, p, false)
}

// Compatibility keeps the default and final team environment. Check this in
// the same transaction as deletion, without restricting native /v1 deletes.
func (s *server) deleteEnvWithPolicy(w http.ResponseWriter, r *http.Request, p *principal, keepRequired bool) {
	cur, ok := s.teamEnvironment(w, r, p)
	if !ok {
		return
	}
	conflict := ""
	if err := s.store.Update(func(st *State) error {
		if _, err := requirePrincipalIn(st, p, "admin", time.Now().UTC()); err != nil {
			return err
		}
		env := st.Environments[cur.ID]
		if env == nil || env.TeamID != p.Team.ID {
			return errConflict
		}
		if keepRequired {
			if env.IsDefault {
				conflict = "the default environment cannot be deleted"
				return errConflict
			}
			n := 0
			for _, e := range st.Environments {
				if e.TeamID == p.Team.ID {
					n++
				}
			}
			if n <= 1 {
				conflict = "at least one environment must be kept"
				return errConflict
			}
		}
		cur = env // retire the builder belonging to the record actually deleted
		delete(st.Environments, cur.ID)
		return nil
	}); err != nil {
		if conflict != "" {
			writeErr(w, r, http.StatusConflict, "conflict", conflict)
			return
		}
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
		if _, err := requirePrincipalIn(st, p, "admin", time.Now().UTC()); err != nil {
			return err
		}
		env := st.Environments[cur.ID]
		if env == nil || env.TeamID != p.Team.ID {
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
	// The builder holds exactly the environment's secret selection, durably: on
	// its boatd labels and in the store. Without it a later start or fork of the
	// builder would inject every team secret.
	only := envSelection(env)
	labels, _ := withSelection(map[string]string{builderLabel: env.ID}, only)
	body := map[string]any{"name": "env-build-" + randHex(4), "size": env.Size, "image": env.Image, "team": team,
		"idle_stop_s": -1, "labels": labels, "env": run}
	var vm VM
	doneLifecycle := s.vmCreates.begin(team)
	defer doneLifecycle()
	if err := s.boatd.call(ctx, "POST", "/v1/vms", body, &vm); err != nil {
		return "", "", fmt.Errorf("create builder: %w", err)
	}
	vmID = str(vm, "id")
	if s.sshPolicyConfigured() {
		defer func() {
			if rerr := s.retireVM(vmID); rerr != nil && !errors.Is(rerr, errStoreClosed) {
				log.Printf("env %s: retire builder %s: %v", env.ID, vmID, rerr)
			}
		}()
	}
	fail := func(e error) (string, string, error) {
		if derr := s.discardCopyVM(vmID); derr != nil {
			log.Printf("env %s: delete failed builder %s: %v", env.ID, vmID, derr)
		}
		return "", "", e
	}
	// Written before the call returns (the store may lag the label otherwise).
	if err := s.recordVM(vmID, team, only, false, false); err != nil {
		return fail(fmt.Errorf("record the builder's secret selection: %w", err))
	}
	s.recordRunNames(vmID, sortedNames(selectionNames(s.store.Snapshot(), team, only)), false) // the team secrets the build pushed
	if s.sshPolicyConfigured() {
		if err := s.initBuilderVMToken(vmID, team); err != nil {
			return fail(fmt.Errorf("ssh_policy: %w", err))
		}
		if _, err := s.prepareSSHPolicy(ctx, vmID, team); err != nil {
			return fail(fmt.Errorf("ssh_policy: %w", err))
		}
	}
	doneLifecycle()
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

// builderLabel marks an environment's builder VM with the environment's id.
const builderLabel = "boat.env"

// builderOf is the environment a VM is the builder of ("" = an ordinary VM).
func builderOf(vm VM) string {
	labels, _ := vm["labels"].(map[string]any)
	id, _ := labels[builderLabel].(string)
	return id
}

// builderReadRoutes are the only routes a key without scope admin may use on an
// environment builder: metadata reads, and revoking an SSH grant. Every other
// route (start, stop, fork, delete, patch, exec, commands, prompts, files, ports,
// SSH, desktop, snapshots, ...) is refused, so a route added later is closed by
// default.
var builderReadRoutes = map[string]bool{
	"GET /v1/vms/{id}":                            true,
	"GET /api/v1/sandboxes/{id}":                  true,
	"GET /v1/vms/{id}/usage":                      true,
	"GET /api/v1/sandboxes/{id}/usage":            true,
	"GET /v1/vms/{id}/ai-usage":                   true,
	"GET /api/v1/sandboxes/{id}/ai-usage":         true,
	"GET /v1/vms/{id}/snapshots":                  true,
	"GET /api/v1/sandboxes/{id}/snapshots":        true,
	"GET /api/v1/sandboxes/{id}/snapshots/latest": true,
	"GET /v1/vms/{id}/ports":                      true,
	"GET /v1/vms/{id}/ssh":                        true,
	"DELETE /v1/vms/{id}/ssh/{fingerprint}":       true,
}

// refuseBuilder answers 403 for a key without scope admin that does anything but
// read an environment builder's metadata (reports whether it did). A builder
// holds the environment's secrets, which only an admin may put to use: the
// builds themselves are admin-only (buildEnv). ownVM applies it to every route
// that resolves a VM, so no guest or lifecycle operation can skip it. The
// builder's own guest token (it acts on that VM only) is not a tenant.
func (s *server) refuseBuilder(w http.ResponseWriter, r *http.Request, p *principal, vm VM) bool {
	if builderOf(vm) == "" || p.has("admin") || (p.VMID != "" && p.VMID == str(vm, "id")) {
		return false
	}
	if builderReadRoutes[info(r).pattern] {
		return false
	}
	writeErr(w, r, http.StatusForbidden, "forbidden", "an environment builder VM holds the environment's secrets: only an admin key may start, stop, fork, delete, snapshot or run anything on it")
	return true
}

// envSelection is the secret selection an environment's builds run under: its
// own, or the team's inject defaults.
func envSelection(env Environment) []string {
	if env.Secrets != nil {
		return append([]string{}, *env.Secrets...)
	}
	return []string{defaultSelection}
}

// builderSelection is the selection of the builder of environment envID, for a
// builder that has none recorded (an environment that is gone: no secrets).
func (s *server) builderSelection(envID string) []string {
	if env := s.store.Snapshot().Environments[envID]; env != nil {
		return envSelection(*env)
	}
	return []string{}
}

// envRunVars: the environment's secret selection plus its plain vars.
func (s *server) envRunVars(env Environment, team string) (map[string]string, error) {
	only := envSelection(env)
	vars, err := s.teamEnv(team, only)
	if err != nil {
		return nil, err
	}
	for k, v := range env.Env {
		vars[k] = v
	}
	return vars, nil
}
