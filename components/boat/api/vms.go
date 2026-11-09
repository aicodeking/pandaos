package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

var (
	reVMName  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reID      = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	reEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	reLabel   = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,63}$`)
)

type VM = map[string]any

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// publicVM drops fields that may carry secrets (boatd echoes env back).
func publicVM(vm VM) VM {
	out := make(VM, len(vm))
	for k, v := range vm {
		if k != "env" {
			out[k] = v
		}
	}
	return out
}

func vmPath(id string, rest ...string) string {
	p := "/v1/vms/" + url.PathEscape(id)
	for _, r := range rest {
		p += "/" + r
	}
	return p
}

// ownVM fetches the VM and checks it belongs to the caller's team; foreign
// VMs are reported as 404 so ids cannot be probed across teams.
func (s *server) ownVM(w http.ResponseWriter, r *http.Request, p *principal) (VM, bool) {
	id := r.PathValue("id")
	if !reID.MatchString(id) {
		writeErr(w, r, 404, "not_found", "vm not found")
		return nil, false
	}
	var vm VM
	if err := s.boatd.call(r.Context(), "GET", vmPath(id), nil, &vm); err != nil {
		writeUpstream(w, r, err)
		return nil, false
	}
	if str(vm, "team") != p.Team.ID {
		writeErr(w, r, 404, "not_found", "vm not found")
		return nil, false
	}
	info(r).vmOK = true
	return vm, true
}

// teamEnv decrypts the team secrets injected as env vars: all of them, or
// only the names in only when it is non-nil (per-VM selection).
func (s *server) teamEnv(teamID string, only []string) (map[string]string, error) {
	env := map[string]string{}
	for _, sec := range s.store.Snapshot().Secrets {
		if sec.TeamID != teamID || (only != nil && !slices.Contains(only, sec.Name)) {
			continue
		}
		v, err := s.keys.Open(sec.Sealed, secretKey(sec.TeamID, sec.Name))
		if err != nil {
			return nil, fmt.Errorf("decrypt secret %s: %w", sec.Name, err)
		}
		env[sec.Name] = string(v)
	}
	return env, nil
}

// snapshotForeign reports whether a VM from this snapshot boots a disk the team
// did not build: an import, or a snapshot of a VM that itself booted one. Such a
// disk's init runs before anyone can look at it. Unknown means foreign.
func (s *server) snapshotForeign(r *http.Request, team, sid string) (bool, error) {
	var list struct {
		Snapshots []map[string]any `json:"snapshots"`
	}
	if err := s.boatd.call(r.Context(), "GET", "/v1/snapshots?team="+url.QueryEscape(team), nil, &list); err != nil {
		return true, err
	}
	for _, sn := range list.Snapshots {
		if str(sn, "id") == sid && str(sn, "team") == team {
			from := str(sn, "vm_id")
			return from == "" || s.store.Snapshot().ForeignVMs[from], nil
		}
	}
	return true, nil
}

// secretsLabel carries a VM's secret selection on its boatd record: boatd
// writes it durably as part of the create and copies it to forks, so the API
// does not have to wait for its own fsync before answering. Comma-separated
// names; "" = none. Absent = see the store (VMs from before the label).
const (
	secretsLabel   = "boat.secrets"
	reservedLabels = "boat." // set by boat-api only
	maxLabelValue  = 256     // boatd's limit
)

// selectionLabel is the label value for a selection, false if it does not fit.
func selectionLabel(only []string) (string, bool) {
	v := strings.Join(only, ",")
	return v, len(v) <= maxLabelValue
}

// labeledSelection reads the selection from a VM's labels.
func labeledSelection(vm VM) ([]string, bool) {
	labels, _ := vm["labels"].(map[string]any)
	v, ok := labels[secretsLabel].(string)
	if !ok {
		return nil, false
	}
	if v == "" {
		return []string{}, true
	}
	return strings.Split(v, ","), true
}

// recordVM stores a new VM's secret selection (nil = all) and whether its disk
// is foreign. A foreign VM starts blocked at the AI proxy; PUT .../ai-access
// opens it. labeled: the selection is on the VM's boatd labels already, so
// the store is only a cache and need not wait for the disk.
func (s *server) recordVM(vmID, team string, only []string, foreign, labeled bool) error {
	if only == nil && !foreign {
		return nil
	}
	write := s.store.Update
	if labeled && !foreign {
		write = s.store.Apply
	}
	return write(func(st *State) error {
		if only != nil {
			st.VMSecrets[vmID] = append([]string{}, only...)
		}
		if foreign {
			st.ForeignVMs[vmID] = true
			st.AIVMs[vmID] = &AIVM{TeamID: team, Blocked: true}
		}
		return nil
	})
}

// foreignSecrets applies the rule for a foreign disk to c: no team secrets unless
// asked for by name, and only by a key that could read them from any VM anyway.
// explicit is what the request itself said, not its environment.
func foreignSecrets(c *createReq, explicit *[]string, p *principal) string {
	if explicit == nil {
		c.Secrets = &[]string{}
		return ""
	}
	c.Secrets = explicit
	if len(*explicit) > 0 && !p.has("exec") {
		return "naming secrets for a shared or imported disk needs the exec scope"
	}
	return ""
}

// vmSecrets is the VM's secret selection (nil = all team secrets).
func (s *server) vmSecrets(vmID string) []string {
	return s.vmSecretsOf(vmID, nil)
}

// selectionFor is vmSecrets for a request that has not fetched the VM: when
// the store has no record it asks boatd for the VM's selection label.
func (s *server) selectionFor(r *http.Request, vmID string) []string {
	if _, ok := s.store.Snapshot().VMSecrets[vmID]; ok {
		return s.vmSecrets(vmID)
	}
	var vm VM
	if err := s.boatd.call(r.Context(), "GET", vmPath(vmID), nil, &vm); err != nil {
		log.Printf("%s: selection label of %s: %v; no secrets", info(r).id, vmID, err)
		return []string{} // fail closed
	}
	return s.vmSecretsOf(vmID, vm)
}

// vmSecretsOf is vmSecrets falling back to vm's label, which outlives a store
// write lost in a crash (vm nil = store only).
func (s *server) vmSecretsOf(vmID string, vm VM) []string {
	sel, ok := s.store.Snapshot().VMSecrets[vmID]
	if !ok {
		if l, ok := labeledSelection(vm); ok {
			return l
		}
		return nil
	}
	if sel == nil {
		return []string{}
	}
	return sel
}

func (s *server) envOrFail(w http.ResponseWriter, r *http.Request, p *principal, only []string) (map[string]string, bool) {
	env, err := s.teamEnv(p.Team.ID, only)
	if err != nil {
		log.Printf("%s: %v", info(r).id, err)
		writeErr(w, r, 500, "secrets_unavailable", "cannot decrypt team secrets")
		return nil, false
	}
	return env, true
}

func (s *server) listVMs(w http.ResponseWriter, r *http.Request, p *principal) {
	var list struct {
		VMs []VM `json:"vms"`
	}
	if err := s.boatd.call(r.Context(), "GET", "/v1/vms?team="+url.QueryEscape(p.Team.ID), nil, &list); err != nil {
		writeUpstream(w, r, err)
		return
	}
	out := []VM{}
	for _, vm := range list.VMs {
		if str(vm, "team") == p.Team.ID { // defence in depth if boatd ignores ?team
			out = append(out, publicVM(vm))
		}
	}
	writeJSON(w, 200, map[string]any{"vms": out})
}

func (s *server) getVM(w http.ResponseWriter, r *http.Request, p *principal) {
	if vm, ok := s.ownVM(w, r, p); ok {
		writeJSON(w, 200, publicVM(vm))
	}
}

type createReq struct {
	Name  string `json:"name"`
	Size  string `json:"size"`
	Image string `json:"image"`
	TTLS  int64  `json:"ttl_s"`
	// IdleStopS overrides boatd's idleStopS; -1 = never.
	IdleStopS int64 `json:"idle_stop_s"`
	// Snapshot creates the VM from a snapshot of the caller's team.
	Snapshot string            `json:"snapshot"`
	Labels   map[string]string `json:"labels"`
	Env      map[string]string `json:"env"`
	// Secrets selects which team secrets the VM gets (absent = all, [] = none).
	Secrets *[]string `json:"secrets"`
	// Environment names one (default: the team's default environment); NoEnv opts out.
	Environment string `json:"environment"`
	NoEnv       bool   `json:"no_env"`
	// From: a share code bsh_..., imported into this team first.
	From string `json:"from"`
}

func (s *server) validateCreate(c *createReq) string {
	if c.Name != "" && !reVMName.MatchString(c.Name) {
		return "name must be lowercase letters, digits and dashes (max 63)"
	}
	// Sizes are boatd's config; boatd rejects unknown ones, "" = its defaultSize.
	if c.Size != "" && !reVMName.MatchString(c.Size) {
		return "invalid size"
	}
	if c.Image == "" && len(s.cfg.Images) > 0 {
		c.Image = s.cfg.Images[0]
	}
	if len(s.cfg.Images) > 0 && !slices.Contains(s.cfg.Images, c.Image) {
		return "unknown image " + c.Image
	}
	if c.TTLS < 0 {
		return "ttl_s must be >= 0"
	}
	if c.IdleStopS < -1 {
		return "idle_stop_s must be >= -1"
	}
	if c.Snapshot != "" && !reID.MatchString(c.Snapshot) {
		return "invalid snapshot id"
	}
	if len(c.Labels) > 32 {
		return "at most 32 labels"
	}
	for k, v := range c.Labels {
		if !reLabel.MatchString(k) || len(v) > maxLabelValue {
			return "invalid label " + k
		}
		if strings.HasPrefix(k, reservedLabels) {
			return "labels starting with " + reservedLabels + " are reserved"
		}
	}
	for k := range c.Env {
		if !reEnvName.MatchString(k) {
			return "invalid env name " + k
		}
	}
	return ""
}

// checkSecretNames reports the first selected name the team does not have.
func (s *server) checkSecretNames(team string, names []string) string {
	st := s.store.Snapshot()
	for _, n := range names {
		if st.Secrets[secretKey(team, n)] == nil {
			return "unknown secret " + n
		}
	}
	return ""
}

// resolveEnv applies the environment to the create request: its image, size,
// vars and secret selection fill what the caller did not set.
func (s *server) resolveEnv(c *createReq, team string) (*Environment, string) {
	st := s.store.Snapshot()
	var env *Environment
	switch {
	case c.NoEnv && c.Environment != "":
		return nil, "environment and no_env conflict"
	case c.NoEnv:
		return nil, ""
	case c.Environment != "":
		if env = envByName(st, team, c.Environment); env == nil {
			return nil, "unknown environment " + c.Environment // before anything is created
		}
	default:
		if env = defaultEnv(st, team); env == nil {
			return nil, ""
		}
	}
	if c.Size == "" {
		c.Size = env.Size
	}
	if c.Image == "" {
		c.Image = env.Image
	}
	if c.Secrets == nil && env.Secrets != nil {
		c.Secrets = env.Secrets
	}
	if len(env.Env) > 0 {
		merged := map[string]string{}
		for k, v := range env.Env {
			merged[k] = v
		}
		for k, v := range c.Env {
			merged[k] = v
		}
		c.Env = merged
	}
	return env, ""
}

func (s *server) createVM(w http.ResponseWriter, r *http.Request, p *principal) {
	var c createReq
	if !decode(w, r, &c) {
		return
	}
	t := newPhases()
	explicit := c.Secrets
	env, msg := s.resolveEnv(&c, p.Team.ID)
	if msg == "" {
		msg = s.validateCreate(&c)
	}
	if msg == "" && c.From != "" && c.Snapshot != "" {
		msg = "from and snapshot conflict"
	}
	if msg != "" {
		writeErr(w, r, 400, "invalid_request", msg)
		return
	}
	foreign := c.From != ""
	if c.Snapshot != "" {
		var err error
		if foreign, err = s.snapshotForeign(r, p.Team.ID, c.Snapshot); err != nil {
			writeUpstream(w, r, err)
			return
		}
	}
	if foreign {
		if msg := foreignSecrets(&c, explicit, p); msg != "" {
			writeErr(w, r, 403, "forbidden", msg)
			return
		}
	}
	var only []string
	if c.Secrets != nil {
		only = append([]string{}, *c.Secrets...)
		if msg := s.checkSecretNames(p.Team.ID, only); msg != "" {
			writeErr(w, r, 400, "invalid_request", msg)
			return
		}
	}
	vars, ok := s.envOrFail(w, r, p, only)
	if !ok {
		return
	}
	for k, v := range c.Env { // explicit env overrides team secrets
		vars[k] = v
	}
	if c.From != "" {
		sh := s.shareByCode(c.From)
		if sh == nil {
			writeErr(w, r, 404, "not_found", "share not found or expired")
			return
		}
		release, ok := s.acquireOr429(w, r, s.uploads, p.Team.ID)
		if !ok {
			return
		}
		defer release()
		snap, err := s.copyShare(r, sh, p.Team.ID, "")
		if err != nil {
			writeUpstream(w, r, err)
			return
		}
		c.Snapshot = str(snap, "id")
	}
	// A current build means the setup is already on the snapshot.
	built := env != nil && c.Snapshot == "" && env.Build != nil && env.Build.SnapshotID != "" && env.Build.Version == env.Version
	if built {
		c.Snapshot = env.Build.SnapshotID
	}
	labels, labeled := withSelection(c.Labels, only)
	body := map[string]any{"name": c.Name, "size": c.Size, "image": c.Image, "ttl_s": c.TTLS,
		"idle_stop_s": c.IdleStopS, "snapshot": c.Snapshot, "team": p.Team.ID, "labels": labels, "env": vars}
	t.mark("prepare")
	var vm VM
	if err := s.boatd.call(r.Context(), "POST", "/v1/vms", body, &vm); err != nil {
		writeUpstream(w, r, err)
		return
	}
	t.mark("boatd")
	id := str(vm, "id")
	defer t.done("create", id)
	if err := s.recordVM(id, p.Team.ID, only, foreign, labeled); err != nil {
		// Fail loudly: without the record a later start would inject all secrets.
		log.Printf("%s: record secret selection for %s: %v; deleting the VM", info(r).id, id, err)
		s.boatd.call(r.Context(), "POST", vmPath(id, "delete"), nil, nil)
		writeErr(w, r, 500, "store_error", "could not record the secret selection")
		return
	}
	t.mark("record")
	// The AI proxy token depends on the VM id, so it goes in after create.
	if ai, err := s.aiEnvNewVM(id, p.Team.ID); err != nil {
		log.Printf("%s: AI env for %s: %v", info(r).id, id, err)
	} else if len(ai) > 0 {
		s.pushEnv(r, id, vars, ai)
	}
	t.mark("ai")
	out := publicVM(vm)
	if env != nil {
		s.applyEnvToVM(r, p, id, env, built, vars, out)
		t.mark("environment")
	}
	writeJSON(w, 201, out)
}

// withSelection adds the secret selection label to the caller's labels; false
// when there is no selection to carry or it does not fit in a label.
func withSelection(labels map[string]string, only []string) (map[string]string, bool) {
	if only == nil {
		return labels, false
	}
	v, ok := selectionLabel(only)
	if !ok {
		return labels, false
	}
	out := make(map[string]string, len(labels)+1)
	for k, val := range labels {
		out[k] = val
	}
	out[secretsLabel] = v
	return out, true
}

// pushEnv rewrites the VM's /etc/boat/env. boatd's env op replaces the file.
func (s *server) pushEnv(r *http.Request, vmID string, base, extra map[string]string) {
	all := map[string]string{}
	for k, v := range base {
		all[k] = v
	}
	for k, v := range extra {
		all[k] = v
	}
	if err := s.boatd.call(r.Context(), "POST", vmPath(vmID, "env"), map[string]any{"vars": all}, nil); err != nil {
		log.Printf("%s: set env on %s: %v", info(r).id, vmID, err)
	}
}

// applyEnvToVM records the environment and starts its setup unless the VM came
// from a current build. The setup runs detached; follow it by setup_cmd_id.
func (s *server) applyEnvToVM(r *http.Request, p *principal, vmID string, env *Environment, built bool, vars map[string]string, out VM) {
	out["environment"] = env.Name
	out["from_build"] = built
	// Only for display and fork bookkeeping: harmless to lose in a crash.
	if err := s.store.Apply(func(st *State) error { st.VMEnvs[vmID] = env.ID; return nil }); err != nil {
		log.Printf("%s: record env of %s: %v", info(r).id, vmID, err)
	}
	if built || strings.TrimSpace(env.SetupScript) == "" {
		return
	}
	cid, err := s.startDetached(r, p, vmID, "setup", []string{"bash", "-lc", "set -e\n" + env.SetupScript}, vars, "")
	if err != nil {
		log.Printf("%s: setup on %s: %v", info(r).id, vmID, err)
		out["setup_error"] = "could not start the setup script"
		return
	}
	out["setup_cmd_id"] = cid
}

func (s *server) startVM(w http.ResponseWriter, r *http.Request, p *principal) {
	t := newPhases()
	vm, ok := s.ownVM(w, r, p)
	if !ok {
		return
	}
	id := r.PathValue("id")
	t.mark("get")
	defer t.done("start", id)
	env, ok := s.envOrFail(w, r, p, s.vmSecretsOf(id, vm))
	if !ok {
		return
	}
	// A new token generation per run: tokens copied from a past run are dead.
	ai, err := s.aiEnv(id, p.Team.ID, true)
	if err != nil {
		log.Printf("%s: AI env for %s: %v", info(r).id, id, err)
		writeErr(w, r, 500, "secrets_unavailable", "cannot prepare AI credentials")
		return
	}
	for k, v := range ai {
		env[k] = v
	}
	t.mark("env")
	s.vmAction(w, r, "start", map[string]any{"env": env})
	t.mark("boatd")
}

func (s *server) stopVM(w http.ResponseWriter, r *http.Request, p *principal) {
	t := newPhases()
	if _, ok := s.ownVM(w, r, p); ok {
		t.mark("get")
		s.vmAction(w, r, "stop", nil)
		t.mark("boatd")
		t.done("stop", r.PathValue("id"))
	}
}

func (s *server) deleteVM(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); ok {
		if s.vmAction(w, r, "delete", nil) {
			s.forgetVM(r.PathValue("id"))
			s.syncJumpKeys(r.Context())
		}
	}
}

func (s *server) forkVM(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Name != "" && !reVMName.MatchString(body.Name) {
		writeErr(w, r, 400, "invalid_request", "invalid name")
		return
	}
	t := newPhases()
	pvm, ok := s.ownVM(w, r, p)
	if !ok {
		return
	}
	t.mark("get")
	var child VM
	parent := r.PathValue("id")
	if err := s.boatd.call(r.Context(), "POST", vmPath(parent, "fork"), map[string]any{"name": body.Name, "team": p.Team.ID}, &child); err != nil {
		writeUpstream(w, r, err)
		return
	}
	t.mark("boatd")
	childID := str(child, "id")
	defer t.done("fork", childID)
	// The child inherits the parent's disk, hence its secrets, so it keeps the
	// parent's selection for later starts too, and its foreign mark. boatd
	// copies the parent's labels, the selection label included.
	st := s.store.Snapshot()
	sel := s.vmSecretsOf(parent, pvm)
	if sel != nil {
		sel = append([]string{}, sel...)
	}
	_, labeled := labeledSelection(child)
	if err := s.recordVM(childID, p.Team.ID, sel, st.ForeignVMs[parent], labeled); err != nil {
		log.Printf("%s: record secret selection for fork %s: %v; deleting it", info(r).id, childID, err)
		s.boatd.call(r.Context(), "POST", vmPath(childID, "delete"), nil, nil)
		writeErr(w, r, 500, "store_error", "could not record the secret selection")
		return
	}
	t.mark("record")
	// The child's disk holds the parent's token; give it its own. Its native
	// agent sessions came along too, so its conversations do as well.
	if ai, err := s.aiEnvNewVM(childID, p.Team.ID); err != nil {
		log.Printf("%s: AI env for fork %s: %v", info(r).id, childID, err)
	} else if len(ai) > 0 {
		if vars, err := s.teamEnv(p.Team.ID, s.vmSecrets(childID)); err == nil {
			s.pushEnv(r, childID, vars, ai)
		}
	}
	t.mark("ai")
	s.copyConversations(parent, childID)
	t.mark("conversations")
	writeJSON(w, 201, publicVM(child))
}

// copyConversations is history, so it does not wait for the disk.
func (s *server) copyConversations(parent, child string) {
	if len(s.store.Snapshot().Conversations[parent]) == 0 {
		return
	}
	err := s.store.Apply(func(st *State) error {
		for _, c := range st.Conversations[parent] {
			cp := *c
			cp.VMID = child
			st.Conversations[child] = append(st.Conversations[child], &cp)
		}
		return nil
	})
	if err != nil {
		log.Printf("copy conversations to %s: %v", child, err)
	}
}

// forgetVM drops boat-api state tied to a deleted VM.
func (s *server) forgetVM(id string) {
	err := s.store.Update(func(st *State) error {
		delete(st.VMSecrets, id)
		delete(st.AIVMs, id)
		delete(st.Commands, id)
		delete(st.Prompts, id)
		delete(st.Conversations, id)
		delete(st.VMEnvs, id)
		delete(st.ForeignVMs, id)
		delete(st.RouteGens, id)
		for k, sh := range st.SnapShares {
			if sh.VMID == id {
				delete(st.SnapShares, k)
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("forget vm %s: %v", id, err)
	}
}

// vmAction proxies a lifecycle POST and reports whether it succeeded.
func (s *server) vmAction(w http.ResponseWriter, r *http.Request, action string, body any) bool {
	var vm VM
	if err := s.boatd.call(r.Context(), "POST", vmPath(r.PathValue("id"), action), body, &vm); err != nil {
		writeUpstream(w, r, err)
		return false
	}
	if vm == nil {
		writeJSON(w, 200, map[string]any{"ok": true})
		return true
	}
	writeJSON(w, 200, publicVM(vm))
	return true
}

// ---- exec & prompt ----

type execReq struct {
	Cmd      []string          `json:"cmd"`
	Env      map[string]string `json:"env,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
	StdinB64 string            `json:"stdin_b64,omitempty"`
	TimeoutS int               `json:"timeout_s,omitempty"`
}

func (s *server) execVM(w http.ResponseWriter, r *http.Request, p *principal) {
	var e execReq
	if !decode(w, r, &e) {
		return
	}
	if len(e.Cmd) == 0 || e.Cmd[0] == "" {
		writeErr(w, r, 400, "invalid_request", "cmd is required")
		return
	}
	for k := range e.Env {
		if !reEnvName.MatchString(k) {
			writeErr(w, r, 400, "invalid_request", "invalid env name "+k)
			return
		}
	}
	if _, ok := s.ownVM(w, r, p); ok {
		s.streamExec(w, r, e)
	}
}

var agents = map[string]func(prompt, model string) []string{
	"claude": func(prompt, model string) []string {
		c := []string{"claude", "-p", "--dangerously-skip-permissions"}
		if model != "" {
			c = append(c, "--model", model)
		}
		return append(c, "--", prompt)
	},
	"codex": func(prompt, model string) []string {
		c := []string{"codex", "exec", "--full-auto", "--skip-git-repo-check"}
		if model != "" {
			c = append(c, "-m", model)
		}
		return append(c, "--", prompt)
	},
	"opencode": func(prompt, model string) []string {
		c := []string{"opencode", "run"}
		if model != "" {
			c = append(c, "-m", model)
		}
		return append(c, "--", prompt)
	},
}

var reModel = regexp.MustCompile(`^[A-Za-z0-9_./:@-]{1,128}$`)

func (s *server) promptVM(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Prompt   string `json:"prompt"`
		Agent    string `json:"agent"`
		Model    string `json:"model"`
		Cwd      string `json:"cwd"`
		TimeoutS int    `json:"timeout_s"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Agent == "" {
		body.Agent = "claude"
	}
	build, ok := agents[body.Agent]
	switch {
	case !ok:
		writeErr(w, r, 400, "invalid_request", "agent must be claude, codex or opencode")
		return
	case strings.TrimSpace(body.Prompt) == "":
		writeErr(w, r, 400, "invalid_request", "prompt is required")
		return
	case body.Model != "" && !reModel.MatchString(body.Model):
		writeErr(w, r, 400, "invalid_request", "invalid model")
		return
	}
	vm, ok := s.ownVM(w, r, p)
	if !ok {
		return
	}
	env, ok := s.envOrFail(w, r, p, s.vmSecretsOf(r.PathValue("id"), vm))
	if !ok {
		return
	}
	ai, err := s.aiEnv(r.PathValue("id"), p.Team.ID, false)
	if err != nil {
		log.Printf("%s: AI env: %v", info(r).id, err)
		writeErr(w, r, 500, "secrets_unavailable", "cannot prepare AI credentials")
		return
	}
	for k, v := range ai { // the proxy token replaces a raw key of the same name
		env[k] = v
	}
	s.streamExec(w, r, execReq{Cmd: build(body.Prompt, body.Model), Env: env, Cwd: body.Cwd, TimeoutS: body.TimeoutS})
}

// streamExec relays boatd's JSON-lines exec stream, flushing per chunk.
func (s *server) streamExec(w http.ResponseWriter, r *http.Request, e execReq) {
	b, _ := json.Marshal(e)
	resp, err := s.boatd.do(r.Context(), "POST", vmPath(r.PathValue("id"), "exec"), bytes.NewReader(b), "application/json")
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		writeUpstream(w, r, &boatdError{Status: resp.StatusCode, Body: body})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// ---- files ----

func validFilePath(p string) bool {
	return strings.HasPrefix(p, "/") && len(p) < 4096 && !strings.ContainsRune(p, 0)
}

func (s *server) getFile(w http.ResponseWriter, r *http.Request, p *principal) {
	path := r.URL.Query().Get("path")
	if !validFilePath(path) {
		writeErr(w, r, 400, "invalid_request", "path must be absolute")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	resp, err := s.boatd.do(r.Context(), "GET", vmPath(r.PathValue("id"), "files")+"?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode == 404 {
			writeErr(w, r, 404, "not_found", "file not found")
			return
		}
		writeUpstream(w, r, &boatdError{Status: resp.StatusCode, Body: body})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(200)
	io.Copy(w, resp.Body)
}

func (s *server) putFile(w http.ResponseWriter, r *http.Request, p *principal) {
	path := r.URL.Query().Get("path")
	if !validFilePath(path) {
		writeErr(w, r, 400, "invalid_request", "path must be absolute")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	spool, n, ok := s.spoolUpload(w, r)
	if !ok {
		return
	}
	defer spool.Close()
	if err := s.putVMFile(r, r.PathValue("id"), path, spool); err != nil {
		writeUpstream(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": path, "bytes": n})
}

// spoolUpload copies the body to an unlinked temp file instead of memory. It
// does not relay the body straight to boatd because boatd cannot tell a client
// that gave up from the end of the file (boatd/api.go putFileHandler treats
// io.ErrUnexpectedEOF as the last chunk): a dropped upload would replace the
// guest's file with its first part.
func (s *server) spoolUpload(w http.ResponseWriter, r *http.Request) (*os.File, int64, bool) {
	if r.ContentLength > s.cfg.MaxFileBytes {
		writeErr(w, r, http.StatusRequestEntityTooLarge, "too_large", "file exceeds limit")
		return nil, 0, false
	}
	f, err := os.CreateTemp(filepath.Dir(s.store.path), ".upload-*")
	if err != nil {
		log.Printf("%s: spool upload: %v", info(r).id, err)
		writeErr(w, r, 500, "store_error", "could not store the upload")
		return nil, 0, false
	}
	os.Remove(f.Name()) // gone as soon as it is closed, even after a crash
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, s.cfg.MaxFileBytes))
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		f.Close()
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, r, http.StatusRequestEntityTooLarge, "too_large", "file exceeds limit")
		} else {
			writeErr(w, r, 400, "invalid_request", "could not read the upload")
		}
		return nil, 0, false
	}
	return f, n, true
}

func (s *server) putVMFile(r *http.Request, id, path string, data io.Reader) error {
	resp, err := s.boatd.do(r.Context(), "PUT", vmPath(id, "files")+"?path="+url.QueryEscape(path), data, "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return &boatdError{Status: resp.StatusCode, Body: body}
	}
	return nil
}

// ---- snapshots ----

// proxySnapshots forwards GET/POST /snapshots and DELETE /snapshots/{sid}.
func (s *server) proxySnapshots(w http.ResponseWriter, r *http.Request, p *principal) {
	path := vmPath(r.PathValue("id"), "snapshots")
	if sid := r.PathValue("sid"); sid != "" {
		if !reID.MatchString(sid) {
			writeErr(w, r, 404, "not_found", "snapshot not found")
			return
		}
		path += "/" + url.PathEscape(sid)
	}
	var in any
	if r.Method == "POST" {
		var body struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &body) {
			return
		}
		in = body
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	var out any
	if err := s.boatd.call(r.Context(), r.Method, path, in, &out); err != nil {
		writeUpstream(w, r, err)
		return
	}
	status := 200
	if r.Method == "POST" {
		status = 201
	}
	if out == nil {
		out = map[string]any{"ok": true}
	}
	writeJSON(w, status, out)
}

// ---- ssh & desktop ----

var reSSHKey = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) [A-Za-z0-9+/=]{16,}( [^\r\n]{0,256})?$`)

func (s *server) sshVM(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		PublicKey string `json:"public_key"`
	}
	if !decode(w, r, &body) {
		return
	}
	pub := strings.TrimSpace(body.PublicKey)
	if len(pub) > 16<<10 {
		pub = "x" // fails the regexp below
	}
	if pub != "" && !reSSHKey.MatchString(pub) {
		writeErr(w, r, 400, "invalid_request", "public_key must be a single OpenSSH public key line")
		return
	}
	vm, ok := s.ownVM(w, r, p)
	if !ok {
		return
	}
	if pub != "" {
		if err := s.authorizeKey(r, r.PathValue("id"), pub); err != nil {
			writeUpstream(w, r, err)
			return
		}
		if err := s.grantJump(r, p.Team.ID, r.PathValue("id"), pub); err != nil {
			if errors.Is(err, errLimit) {
				s.storeErr(w, r, err)
				return
			}
			log.Printf("%s: jump keys: %v", info(r).id, err)
			writeErr(w, r, 500, "jump_sync_failed", "could not update ssh jump host")
			return
		}
	}
	host := str(vm, "ssh_host")
	if host == "" {
		host = str(vm, "ip")
	}
	port := 22
	if f, ok := vm["ssh_port"].(float64); ok && f > 0 {
		port = int(f)
	}
	out := map[string]any{"host": host, "port": port, "user": s.cfg.SSHUser}
	if s.cfg.SSHJump != "" {
		out["jump"] = s.cfg.SSHJump
	}
	writeJSON(w, 200, out)
}

// authorizeKey appends pub to the VM user's authorized_keys if absent.
func (s *server) authorizeKey(r *http.Request, id, pub string) error {
	path := "/home/" + s.cfg.SSHUser + "/.ssh/authorized_keys"
	if s.cfg.SSHUser == "root" {
		path = "/root/.ssh/authorized_keys"
	}
	resp, err := s.boatd.do(r.Context(), "GET", vmPath(id, "files")+"?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return err
	}
	cur, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	switch {
	case resp.StatusCode == 404:
		cur = nil
	case resp.StatusCode/100 != 2:
		return &boatdError{Status: resp.StatusCode, Body: cur}
	}
	fields := strings.Fields(pub)
	for _, line := range strings.Split(string(cur), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == fields[0] && f[1] == fields[1] {
			return nil
		}
	}
	next := string(cur)
	if next != "" && !strings.HasSuffix(next, "\n") {
		next += "\n"
	}
	return s.putVMFile(r, id, path, strings.NewReader(next+pub+"\n"))
}

func (s *server) desktopVM(w http.ResponseWriter, r *http.Request, p *principal) {
	if s.cfg.DesktopURL == "" || len(s.cfg.DesktopKey) == 0 {
		writeErr(w, r, 501, "not_configured", "desktop gateway is not configured")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id := r.PathValue("id")
	// Start the guest desktop proxy with the token the gateway presents as
	// Cookie boat_desktop=<token> (see SignGuestDesktop).
	if err := s.boatd.call(r.Context(), "POST", vmPath(id, "desktop"), map[string]string{"action": "start", "token": SignGuestDesktop(s.cfg.DesktopKey, id)}, nil); err != nil {
		writeUpstream(w, r, err)
		return
	}
	exp := time.Now().Add(s.cfg.DesktopTTL).Unix()
	u := fmt.Sprintf("%s/%s?exp=%d&sig=%s", strings.TrimRight(s.cfg.DesktopURL, "/"), url.PathEscape(id), exp, SignDesktop(s.cfg.DesktopKey, id, exp))
	writeJSON(w, 200, map[string]any{"url": u, "expires_at": time.Unix(exp, 0).UTC()})
}
