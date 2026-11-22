package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
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
	if ri := info(r); ri.vm != nil && str(ri.vm, "id") == id && str(ri.vm, "team") == p.Team.ID {
		// checked by guardBody before the body was read
		if err := s.adoptCopyPolicy(ri.vm); err != nil {
			s.storeErr(w, r, err)
			return nil, false
		}
		if s.refusePendingCredentialCommand(w, r, id) {
			return nil, false
		}
		return ri.vm, !s.refuseBuilder(w, r, p, ri.vm)
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
	info(r).vmOK, info(r).vm = true, vm
	if err := s.adoptCopyPolicy(vm); err != nil {
		s.storeErr(w, r, err)
		return nil, false
	}
	if s.refusePendingCredentialCommand(w, r, id) {
		return nil, false
	}
	return vm, !s.refuseBuilder(w, r, p, vm)
}

// defaultSelection in a VM's selection stands for the team secrets an admin
// marked default-injected (Secret.Inject), read at each start. A VM created
// without a selection gets just this. VMs from before it have no selection at
// all (nil), which still means every team secret; docs/boat.md, "Secrets".
const defaultSelection = "@default"

// teamEnv decrypts the team secrets injected as env vars: all of them, or
// only the names in only when it is non-nil (per-VM selection).
func (s *server) teamEnv(teamID string, only []string) (map[string]string, error) {
	env := map[string]string{}
	for _, sec := range s.store.Snapshot().Secrets {
		picked := only == nil || slices.Contains(only, sec.Name) || (sec.Inject && slices.Contains(only, defaultSelection))
		if sec.TeamID != teamID || !picked {
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
			if from == "" {
				return true, nil
			}
			var vm VM
			if err := s.boatd.call(r.Context(), "GET", vmPath(from), nil, &vm); err != nil {
				return true, nil
			}
			if err := s.adoptCopyPolicy(vm); err != nil {
				return true, err
			}
			return s.store.Snapshot().ForeignVMs[from], nil
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
	selectionMark  = "boat.sel"
	noEnvLabel     = "boat.no_env"
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

func labeledNoEnv(vm VM) bool {
	labels, _ := vm["labels"].(map[string]any)
	return labels[noEnvLabel] == "1"
}

// recordVM stores a new VM's secret selection (nil = all) and whether its disk
// is foreign. A foreign VM starts blocked at the AI proxy; PUT .../ai-access
// opens it. labeled: the selection is on the VM's boatd labels already, so
// the store is only a cache and need not wait for the disk.
func (s *server) recordVM(vmID, team string, only []string, foreign, labeled bool) error {
	return s.recordVMs([]string{vmID}, team, only, foreign, labeled)
}

func (s *server) recordVMs(ids []string, team string, only []string, foreign, labeled bool) error {
	if len(ids) == 0 {
		return nil
	}
	cur := s.store.Snapshot()
	if cur.Teams[team] == nil {
		return errors.New("VM team no longer exists")
	}
	for _, vmID := range ids {
		if tok := cur.VMTokens[vmID]; tok != nil && (tok.Retired || tok.TeamID != team) {
			return errors.New("VM credentials are retired")
		}
		if ai := cur.AIVMs[vmID]; ai != nil && (ai.Retired || ai.TeamID != team) {
			return errors.New("VM credentials are retired")
		}
	}
	if only == nil && !foreign {
		return nil
	}
	write := s.store.Update
	if labeled && !foreign {
		write = s.store.Apply
	}
	return write(func(st *State) error {
		if st.Teams[team] == nil {
			return errors.New("VM team no longer exists")
		}
		for _, vmID := range ids {
			if tok := st.VMTokens[vmID]; tok != nil && (tok.Retired || tok.TeamID != team) {
				return errors.New("VM credentials are retired")
			}
			if ai := st.AIVMs[vmID]; ai != nil && (ai.Retired || ai.TeamID != team) {
				return errors.New("VM credentials are retired")
			}
		}
		for _, vmID := range ids {
			if only != nil {
				st.VMSecrets[vmID] = append([]string{}, only...)
			}
			if foreign {
				st.ForeignVMs[vmID] = true
				if t := st.cowVMToken(vmID); t != nil {
					t.RestrictSelf = true
				}
				av := st.cowAIVM(vmID)
				if av == nil {
					av = &AIVM{TeamID: team}
					st.AIVMs[vmID] = av
				}
				av.Blocked = true
			}
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
	if err := s.adoptCopyPolicy(vm); err != nil {
		return []string{}
	}
	return s.vmSecretsOf(vmID, vm)
}

// runSecretsOf is the team-secret selection a command run now may carry: the
// VM's selection, or none when its current run was started with no_env (the same
// rule startPrompt applies).
func (s *server) runSecretsOf(vmID string, vm VM) []string {
	if s.noEnvRun(vmID) {
		return []string{}
	}
	return s.vmSecretsOf(vmID, vm)
}

// ---- env pushes in flight (per VM) ----

// envPushKey names one VM of one server (tests run several servers in a
// process; ids are only unique per boatd).
type envPushKey struct {
	srv *server
	vm  string
}

var (
	envPushMu sync.Mutex
	envPushN  = map[envPushKey]int{}
)

// markEnvPush records that a create or cold start of vmID is still pushing its
// environment: until release the compat sandbox is provisioning and refuses
// commands. release is idempotent.
func (s *server) markEnvPush(vmID string) (release func()) {
	k := envPushKey{s, vmID}
	envPushMu.Lock()
	envPushN[k]++
	envPushMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			envPushMu.Lock()
			if envPushN[k]--; envPushN[k] <= 0 {
				delete(envPushN, k)
			}
			envPushMu.Unlock()
		})
	}
}

// envPushPending reports whether an env push to vmID is in flight.
func (s *server) envPushPending(vmID string) bool {
	envPushMu.Lock()
	defer envPushMu.Unlock()
	return envPushN[envPushKey{s, vmID}] > 0
}

// vmSecretsOf is vmSecrets falling back to vm's label, which outlives a store
// write lost in a crash (vm nil = store only).
func (s *server) vmSecretsOf(vmID string, vm VM) []string {
	st := s.store.Snapshot()
	if _, noEnv := st.NoEnvRuns[vmID]; noEnv || labeledNoEnv(vm) {
		return []string{}
	}
	sel, ok := st.VMSecrets[vmID]
	if !ok {
		if l, ok := labeledSelection(vm); ok {
			return l
		}
		if labels, _ := vm["labels"].(map[string]any); labels[selectionMark] == "1" {
			return []string{} // only legacy VMs may fall back to all secrets
		}
		if envID := builderOf(vm); envID != "" {
			// An environment builder from before its selection was recorded: the
			// environment's own, never every team secret.
			return s.builderSelection(envID)
		}
		if st.ForeignVMs[vmID] {
			return []string{}
		} // an unknown foreign policy never authorizes all secrets
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
			out = append(out, s.publicVMState(vm))
		}
	}
	writeJSON(w, 200, map[string]any{"vms": out})
}

func (s *server) getVM(w http.ResponseWriter, r *http.Request, p *principal) {
	if vm, ok := s.ownVM(w, r, p); ok {
		writeJSON(w, 200, s.publicVMState(vm))
	}
}

// envErrorOf is the persisted env failure of a running VM ("" = none). A VM that
// is not running has no live environment to be missing.
func (s *server) envErrorOf(vm VM) string {
	if str(vm, "state") != "running" {
		return ""
	}
	return s.store.Snapshot().VMEnvErrors[str(vm, "id")]
}

// publicVMRun is publicVM plus no_env_run: whether the VM's current run was
// started with no_env (clients restoring a run must keep it secret-free).
func (s *server) publicVMRun(vm VM) VM {
	out := s.withDisplay(vm)
	out["no_env_run"] = s.noEnvRun(str(vm, "id"))
	return out
}

// publicVMState is publicVM plus the persisted env failure: a running VM whose
// environment never reached the guest is agent_ready=false with env_error.
func (s *server) publicVMState(vm VM) VM {
	out := s.publicVMRun(vm)
	// env_pending: a create or cold start is still applying the environment; the
	// sandbox refuses commands and prompts (409 sandbox_not_ready) until it settles.
	out["env_pending"] = s.envPushPending(str(vm, "id"))
	if msg := s.envErrorOf(vm); msg != "" {
		out["agent_ready"] = false
		out["env_error"] = msg
	}
	return out
}

// recordEnvResult persists the outcome of an environment push to vmID: a failure
// is remembered (see State.VMEnvErrors), a success forgets an earlier one.
func (s *server) recordEnvResult(vmID string, failure error) {
	s.recordEnvResultVM(vmID, failure, nil)
}

// recordEnvResultVM is recordEnvResult for a caller that fails the VM closed
// (stops it) right after a failure: failVM is the VM in hand, from which the
// sandbox.error webhook is built before the stop (it is ignored on success).
func (s *server) recordEnvResultVM(vmID string, failure error, failVM VM) {
	msg := ""
	if failure != nil {
		msg = envErrorMessage
	}
	snap := s.store.Snapshot()
	cur, had := snap.VMEnvErrors[vmID]
	open := hasOpenSpan(snap.EnvErrorSpans[vmID])
	changed := !((msg == "" && !had && !open) || (msg != "" && cur == msg && open))
	if changed {
		err := s.store.Apply(func(st *State) error {
			if msg == "" {
				delete(st.VMEnvErrors, vmID)
			} else {
				st.VMEnvErrors[vmID] = msg
			}
			setEnvErrorSpan(st, vmID, msg != "", time.Now())
			return nil
		})
		if err != nil {
			log.Printf("record env result of %s: %v", vmID, err)
		}
	}
	// Webhooks: sandbox.ready waits for this; a failure is sandbox.error.
	s.hooks.envSettled(vmID, failure != nil, changed, failVM)
}

// boolErr is a placeholder error for recordEnvResult when only the outcome is known.
func boolErr(failed bool) error {
	if failed {
		return errEnvNotApplied
	}
	return nil
}

var errEnvNotApplied = errors.New("environment not applied")

// envErrorMessage is what clients see while a VM's environment push has failed.
const envErrorMessage = "could not set the VM environment"

type createReq struct {
	Name  string `json:"name"`
	Size  string `json:"size"`
	Image string `json:"image"`
	// TTLS is the auto-stop length in seconds: omitted = defaultCreateTTLS (boat.dev's
	// one hour), an explicit 0 = no auto-stop.
	TTLS *int64 `json:"ttl_s"`
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
	// FailFast: a full host answers 503 no_ready_machine at once (boatd).
	FailFast bool `json:"fail_fast"`
	// Count creates that many VMs, all or none (boatd's batch); > 1 answers {"vms":[…]}.
	Count int `json:"count"`
	// SelfAPI controls BOAT_URL/BOAT_API_KEY injection (default true, or false for foreign disks).
	SelfAPI *bool `json:"self_api,omitempty"`
}

const maxBatch = 16 // boatd's limit on count

// defaultCreateTTLS is the lifetime of a VM created without ttl_s (boat.dev: 1 hour),
// the same as a fork's. An explicit 0 still means no auto-stop.
const defaultCreateTTLS = defaultForkTTLS

// ttlOrDefault is the create's auto-stop length: ttl_s when sent (0 = none), else one hour.
func (c *createReq) ttlOrDefault() int64 {
	if c.TTLS == nil {
		return defaultCreateTTLS
	}
	return *c.TTLS
}

func (s *server) validateCreate(c *createReq) string {
	if c.Name != "" {
		name, msg := validDisplayName(c.Name)
		if msg != "" {
			return msg
		}
		c.Name = name
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
	if c.TTLS != nil && *c.TTLS < 0 {
		return "ttl_s must be >= 0"
	}
	if c.IdleStopS < -1 {
		return "idle_stop_s must be >= -1"
	}
	if c.Count < 0 || c.Count > maxBatch {
		return fmt.Sprintf("count must be 1-%d", maxBatch)
	}
	if c.Snapshot != "" && !reID.MatchString(c.Snapshot) {
		return "invalid snapshot id"
	}
	if msg := validLabels(c.Labels); msg != "" {
		return msg
	}
	for k := range c.Env {
		if !reEnvName.MatchString(k) {
			return "invalid env name " + k
		}
	}
	return ""
}

// undefaultSecret is the first of names that is not default-injected: a
// member may not pull such a secret into a VM it can exec in.
func (s *server) undefaultSecret(team string, names []string) string {
	st := s.store.Snapshot()
	for _, n := range names {
		if sec := st.Secrets[secretKey(team, n)]; n != defaultSelection && (sec == nil || !sec.Inject) {
			return n
		}
	}
	return ""
}

// checkSecretNames reports the first selected name the team does not have.
func (s *server) checkSecretNames(team string, names []string) string {
	st := s.store.Snapshot()
	for _, n := range names {
		if n != defaultSelection && st.Secrets[secretKey(team, n)] == nil {
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
	default:
		var found bool
		if env, found = resolveEnvRef(st, team, c.Environment); !found {
			return nil, unknownEnvPrefix + c.Environment // before anything is created
		}
		if env == nil {
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
	var (
		c             createReq
		env           *Environment
		foreign       bool
		only          []string
		vars          map[string]string
		runNames      []string // the team secrets the new VMs' runs are started with
		injectSelfAPI bool
		extraEnv      map[string]string // the per-sandbox env stored for the new VMs
	)
	t := newPhases()
	s.idempotentScoped(w, r, p, "v1:POST /v1/vms", func() bool {
		if !decode(w, r, &c) {
			return false
		}
		callerEnv := len(c.Env) > 0
		extraEnv = maps.Clone(c.Env) // the caller's own, before the environment's vars merge in
		explicit := c.Secrets
		var msg string
		// Authenticate a share and resolve its source policy before defaults or
		// explicit secrets are chosen. Importing its bytes must not change policy.
		sourceSnapshot := c.Snapshot
		if c.From != "" {
			if c.Snapshot != "" {
				writeErr(w, r, 400, "invalid_request", "from and snapshot conflict")
				return false
			}
			sh := s.shareByCode(c.From)
			if sh == nil {
				writeErr(w, r, 404, "not_found", "share not found or expired")
				return false
			}
			sourceSnapshot = sh.SnapshotID
		}
		// A VM created from a snapshot of a no_env VM is no_env too (sticky): it takes
		// no environment, not even the team's default one, and no secrets.
		if s.snapshotSourceNoEnv(sourceSnapshot) {
			if c.Environment != "" {
				writeErr(w, r, http.StatusBadRequest, errNoEnvSticky, "this snapshot is from a no_env sandbox: a sandbox created from it is no_env and takes no environment; omit environment")
				return false
			}
			if hasSecretNames(c.Secrets) {
				writeErr(w, r, http.StatusBadRequest, errNoEnvSticky, "this snapshot is from a no_env sandbox: a sandbox created from it is no_env and takes no secrets; omit secrets")
				return false
			}
			c.NoEnv = true
		}
		// no_env never holds team secrets: a create that is no_env and names some is
		// refused before anything is created.
		if c.NoEnv && hasSecretNames(c.Secrets) {
			writeErr(w, r, http.StatusBadRequest, errNoEnvSticky, msgNoEnvSecrets)
			return false
		}
		env, msg = s.resolveEnv(&c, p.Team.ID)
		if msg == "" {
			msg = s.validateCreate(&c)
		}
		if msg == "" && c.From != "" && c.Snapshot != "" {
			msg = "from and snapshot conflict"
		}
		if msg != "" {
			if strings.HasPrefix(msg, unknownEnvPrefix) {
				writeErr(w, r, http.StatusNotFound, errUnknownEnvironment, msg)
				return false
			}
			writeErr(w, r, 400, "invalid_request", msg)
			return false
		}
		if callerEnv && !p.has("exec") {
			writeErr(w, r, http.StatusForbidden, "forbidden", "env requires scope exec")
			return false
		}
		if c.SelfAPI != nil && *c.SelfAPI && (!p.has("exec") || !p.has("vms:read")) {
			writeErr(w, r, http.StatusForbidden, "forbidden", "self_api requires scopes vms:read and exec")
			return false
		}
		willBuild := env != nil && c.From == "" && c.Snapshot == "" && env.Build != nil && env.Build.SnapshotID != "" && env.Build.Version == env.Version
		if env != nil && !willBuild && strings.TrimSpace(env.SetupScript) != "" && !p.has("exec") {
			writeErr(w, r, http.StatusForbidden, "forbidden", "environment setup_script requires scope exec")
			return false
		}
		foreign = c.From != ""
		if c.Snapshot != "" {
			// The caller's own snapshot id, never the environment's build snapshot
			// (set below, on the environment path): a builder's disk and memory hold
			// the environment's secrets, which only an admin may put to use.
			if s.refuseCreateFromBuilder(w, r, p, c.Snapshot) {
				return false
			}
			var err error
			if foreign, err = s.snapshotForeign(r, p.Team.ID, c.Snapshot); err != nil {
				writeUpstream(w, r, err)
				return false
			}
		}
		if foreign {
			if c.NoEnv && sourceSnapshot != "" && !s.snapshotSourceProtected(sourceSnapshot) {
				writeErr(w, r, http.StatusConflict, "unsafe_source", "a protected clone of a foreign disk requires a proven protected source; legacy disk cleanup before boot cannot be certified")
				return false
			}
			if msg := foreignSecrets(&c, explicit, p); msg != "" {
				writeErr(w, r, 403, "forbidden", msg)
				return false
			}
		}
		only = []string{defaultSelection}
		if c.NoEnv && c.Secrets == nil {
			only = []string{} // no_env without secrets: no team secrets, not @default
		}
		if c.Secrets != nil {
			only = append([]string{}, *c.Secrets...)
			if msg := s.checkSecretNames(p.Team.ID, only); msg != "" {
				writeErr(w, r, 400, "invalid_request", msg)
				return false
			}
			if explicit != nil && !p.has("admin") {
				if n := s.undefaultSecret(p.Team.ID, *explicit); n != "" {
					writeErr(w, r, 403, "forbidden", "secret "+n+" is not default-injected: only an admin key may select it (or an admin's environment)")
					return false
				}
			}
		}
		var ok bool
		vars, ok = s.envOrFail(w, r, p, only)
		if !ok {
			return false
		}
		runNames = envNames(vars) // the team secrets alone: the caller's env is not one
		for k, v := range c.Env { // explicit env overrides team secrets
			vars[k] = v
		}
		if len(extraEnv) == 0 && c.Snapshot != "" && c.From == "" && !foreign && !c.NoEnv {
			// A clone of a sandbox's snapshot inherits that sandbox's env, unless
			// the request gives its own.
			extraEnv = s.snapshotSourceExtraEnv(c.Snapshot)
			for k, v := range extraEnv {
				vars[k] = v
			}
		}
		injectSelfAPI = !foreign && p.has("exec") && p.has("vms:read") && vars["BOAT_API_KEY"] == ""
		if c.SelfAPI != nil {
			injectSelfAPI = *c.SelfAPI
		}
		return true
	}, func(w http.ResponseWriter) {
		if c.From != "" {
			if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
				return
			}
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
			if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
				s.discardImportedSnapshot(str(snap, "id"))
				return
			}
			if s.shareByCode(c.From) == nil {
				s.discardImportedSnapshot(str(snap, "id"))
				writeErr(w, r, 404, "not_found", "share not found or expired")
				return
			}
			s.countShareUse(sh.ID)
			c.Snapshot = str(snap, "id")
			if s.snapshotSourceNoEnv(c.Snapshot) && !c.NoEnv {
				s.discardImportedSnapshot(c.Snapshot)
				writeErr(w, r, http.StatusConflict, "source_policy_changed", "the shared snapshot policy changed; retry the create")
				return
			}
		}
		// A current build means the setup is already on the snapshot.
		built := env != nil && c.Snapshot == "" && env.Build != nil && env.Build.SnapshotID != "" && env.Build.Version == env.Version
		if env != nil && !built && strings.TrimSpace(env.SetupScript) != "" && !p.has("exec") {
			if c.From != "" && c.Snapshot != "" {
				s.discardImportedSnapshot(c.Snapshot)
			}
			writeErr(w, r, http.StatusForbidden, "forbidden", "environment setup_script requires scope exec")
			return
		}
		if built {
			c.Snapshot = env.Build.SnapshotID
		}
		labels, labeled := withSelection(c.Labels, only)
		if foreign {
			labels[foreignLabel] = "1"
		}
		if !injectSelfAPI {
			labels[noSelfLabel] = "1"
		}
		if c.NoEnv {
			labels[secretsLabel], labels[noEnvLabel] = "", "1"
			if c.Snapshot == "" || s.snapshotSourceProtected(c.Snapshot) {
				labels[protectedLabel] = "1"
			}
		}
		// A memory snapshot resumes the run it was taken from, secrets in its
		// processes' environ included: a narrower selection cold-boots the disk only.
		freshBoot := s.snapshotNeedsFreshBoot(p.Team.ID, c.Snapshot, only) || (c.NoEnv && c.Snapshot != "" && !s.snapshotSourceProtected(c.Snapshot))
		if _, known := s.store.Snapshot().VMSecrets[snapSourceKey(c.Snapshot)]; foreign && c.Snapshot != "" && !known {
			freshBoot = true
		}
		if foreign || freshBoot || c.NoEnv {
			labels[restrictedLabel] = "1"
		}
		// The env is not sent at boot: boatd's boot-time set_env and the pushEnv
		// below (which must also carry the per-VM BOAT_ID/token extras that exist
		// only once the id is known) each cost a vsock round trip and the second
		// replaces the first. finishOne sends the full set exactly once, before
		// the SSH policy and the setup script.
		host := hostnameOf(c.Name) // boatd keeps the DNS slug; the display name is ours
		if c.Count > 1 {
			host = batchHostname(c.Name)
		}
		body := map[string]any{"name": host, "size": c.Size, "image": c.Image, "ttl_s": c.ttlOrDefault(),
			"idle_stop_s": c.IdleStopS, "snapshot": c.Snapshot, "team": p.Team.ID, "labels": labels}
		if c.FailFast {
			body["fail_fast"] = true
		}
		if freshBoot {
			body["fresh_boot"] = true
		}
		if c.Count > 1 {
			body["count"] = c.Count
		}
		if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
			if c.From != "" && c.Snapshot != "" {
				s.discardImportedSnapshot(c.Snapshot)
			}
			return
		}
		t.mark("prepare")
		// boatd announces the VM running before finishOne pushes its env.
		defer s.hooks.expectEnv(p.Team.ID)()
		doneLifecycle := s.vmCreates.begin(p.Team.ID)
		defer doneLifecycle()
		vms, err := s.bootVMs(r, body, c.Count > 1)
		if err != nil {
			writeUpstream(w, r, err)
			return
		}
		t.mark("boatd")
		ids := make([]string, len(vms))
		releasePush := make([]func(), len(vms))
		for i, vm := range vms {
			ids[i] = str(vm, "id")
			releasePush[i] = s.markEnvPush(ids[i]) // compat reads provisioning until the env landed
			defer releasePush[i]()
		}
		defer t.done("create", strings.Join(ids, ","))
		explicitNoSelf := c.SelfAPI != nil && !*c.SelfAPI
		names := createDisplayNames(c.Name, ids, c.Count > 1)
		extras, err := s.coalesceCreateStore(r, ids, names, p.Team.ID, only, runNames, foreign, labeled, vars, extraEnv, env, injectSelfAPI, c.NoEnv, r.Header.Get("Idempotency-Key") != "")
		if err != nil {
			// Fail loudly: without the record a later start would inject all secrets.
			// All or none, like boatd's batch.
			log.Printf("%s: record secret selection for %v: %v; deleting %v", info(r).id, ids, err, ids)
			cleanupFailed := false
			for _, x := range ids {
				if cleanup := s.discardCopyVM(x); cleanup != nil {
					log.Printf("%s: rollback VM %s: %v", info(r).id, x, cleanup)
					cleanupFailed = true
				}
			}
			if cleanupFailed {
				writeErr(w, r, 500, "cleanup_failed", "could not record policy or clean up the new VMs")
				return
			}
			writeErr(w, r, 500, "store_error", "could not record the secret selection")
			return
		}
		t.mark("record")
		outs := make([]VM, len(vms))
		closed := make([]closedCreate, len(vms)) // fresh-boot VMs whose env did not land
		batchSSH := len(vms) > 1 && s.sshPolicyConfigured()
		finishOne := func(i int, vm VM) {
			id := str(vm, "id")
			vmVars := make(map[string]string, len(vars))
			for k, v := range vars {
				vmVars[k] = v
			}
			ex := extras[id]
			if explicitNoSelf && len(ex) == 1 && ex["BOAT_ID"] == id {
				ex = nil
			}
			envFailed := false
			// A fresh boot always pushes (BOAT_ID at least): the push replaces the
			// snapshot's /etc/boat/env, which holds the source run's secrets.
			if len(ex) > 0 || len(vmVars) > 0 || freshBoot {
				var failVM VM // the fail-closed stop below: sandbox.error from this VM, before it
				if freshBoot {
					failVM = vm
				}
				envFailed = s.pushEnvVM(r, id, vmVars, ex, failVM) != nil
			}
			if freshBoot && envFailed {
				closed[i] = s.failClosedCreate(r, id, vm)
				outs[i] = closed[i].vm
				return
			}
			if !batchSSH {
				s.provisionSSHPolicyLoggedWithVM(r.Context(), id, p.Team.ID, vm)
			}
			out := s.publicVMRun(vm)
			if envFailed {
				// The env (secrets, BOAT_ID/token) never reached the guest: say so
				// instead of reporting a ready VM that silently lacks it.
				out["agent_ready"] = false
				out["env_error"] = envErrorMessage
			}
			if env != nil {
				out["environment"] = env.Name
				out["from_build"] = built
				if !built && strings.TrimSpace(env.SetupScript) != "" && envFailed {
					out["setup_error"] = "setup skipped: the VM environment could not be set"
				} else if !built && strings.TrimSpace(env.SetupScript) != "" {
					cid, _, serr := s.startDetached(r, p, id, "setup", []string{"bash", "-lc", "set -e\n" + env.SetupScript}, vmVars, "")
					if serr != nil {
						log.Printf("%s: setup on %s: %v", info(r).id, id, serr)
						out["setup_error"] = "could not start the setup script"
					} else {
						out["setup_cmd_id"] = cid
					}
				}
			}
			outs[i] = out
		}
		if len(vms) <= 1 {
			for i, vm := range vms {
				finishOne(i, vm)
			}
		} else {
			sem := make(chan struct{}, min(len(vms), 16))
			var wg sync.WaitGroup
			for i, vm := range vms {
				wg.Add(1)
				sem <- struct{}{}
				go func(i int, vm VM) {
					defer wg.Done()
					defer func() { <-sem }()
					finishOne(i, vm)
				}(i, vm)
			}
			wg.Wait()
			if batchSSH {
				live := make([]VM, 0, len(vms))
				for i, vm := range vms {
					if closed[i].kind == closedNone {
						live = append(live, vm)
					}
				}
				s.provisionSSHPolicyBatch(r.Context(), live, p.Team.ID)
			}
		}
		t.mark("env")
		info(r).idemVMs = slices.Clone(ids)
		for _, release := range releasePush {
			release() // the env pushes settled (the failed ones were stopped): env_pending=false below
		}
		doneLifecycle()
		if s.writeCreateFailClosed(w, r, closed, outs) {
			return
		}
		if c.Count > 1 {
			writeJSON(w, 201, map[string]any{"vms": outs})
			return
		}
		writeJSON(w, 201, outs[0])
	})
}

// bootVMs asks boatd for one VM, or for a batch ({"vms":[…]}).
func (s *server) bootVMs(r *http.Request, body map[string]any, batch bool) ([]VM, error) {
	if !batch {
		var vm VM
		err := s.boatd.call(r.Context(), "POST", "/v1/vms", body, &vm)
		return []VM{vm}, err
	}
	var out struct {
		VMs []VM `json:"vms"`
	}
	if err := s.boatd.call(r.Context(), "POST", "/v1/vms", body, &out); err != nil {
		return nil, err
	}
	if len(out.VMs) == 0 {
		return nil, errors.New("boatd answered a batch create without vms")
	}
	return out.VMs, nil
}

// coalesceCreateStore records secret selections, foreign markers, VMTokens,
// AIVMs, and VMEnvs across all newly created VMs in at most one Store write.
func (s *server) coalesceCreateStore(r *http.Request, ids []string, names map[string]string, team string, only, runNames []string, foreign, labeled bool, vars, extraEnv map[string]string, env *Environment, injectSelfAPI, noEnv, stageIdem bool) (map[string]map[string]string, error) {
	extras := make(map[string]map[string]string, len(ids))
	for _, id := range ids {
		extras[id] = map[string]string{"BOAT_ID": id}
	}
	cur := s.store.Snapshot()
	if cur.Teams[team] == nil {
		return nil, errors.New("VM team no longer exists")
	}
	for _, vmID := range ids {
		if tok := cur.VMTokens[vmID]; tok != nil && (tok.Retired || tok.TeamID != team) {
			return nil, errors.New("VM credentials are retired")
		}
		if ai := cur.AIVMs[vmID]; ai != nil && (ai.Retired || ai.TeamID != team) {
			return nil, errors.New("VM credentials are retired")
		}
	}
	base := s.vmAPIURL()
	needSelf := !(base == "" && !s.sshPolicyConfigured() && injectSelfAPI && !foreign)
	var creds []*AICredential
	for _, c := range cur.AICreds {
		if c.TeamID == team {
			creds = append(creds, c)
		}
	}
	rawAI := map[string]string{}
	needAI := false
	if len(creds) > 0 {
		if cur.AIProxyOff[team] {
			if !foreign && !noEnv { // a no_env VM holds no raw team key (it cannot be revoked per VM)
				ok := true
				for _, c := range creds {
					key, err := s.keys.Open(c.Sealed, "ai/"+aiCredKey(c.TeamID, c.Provider))
					if err != nil {
						log.Printf("%s: AI env decrypt %s: %v", info(r).id, c.Provider, err)
						ok = false
						break
					}
					rawAI[aiProviders[c.Provider].EnvKey] = string(key)
				}
				if !ok {
					rawAI = nil
				}
			}
		} else if s.cfg.AIProxyURL != "" {
			needAI = true
		} else {
			log.Printf("create: team %s has AI credentials but -ai-proxy-url is not set; none injected", team)
		}
	}
	selfGens := make(map[string]int, len(ids))
	noSelfMap := make(map[string]bool, len(ids))
	aiGens := make(map[string]int, len(ids))
	needRecord := only != nil || foreign || noEnv
	now := time.Now().UTC()
	if needRecord || needSelf || needAI || env != nil || len(extraEnv) > 0 || len(names) > 0 {
		mutate := func(st *State) (bool, error) {
			if st.Teams[team] == nil {
				return false, errors.New("VM team no longer exists")
			}
			for _, id := range ids {
				if tok := st.VMTokens[id]; tok != nil && (tok.Retired || tok.TeamID != team) {
					return false, errors.New("VM credentials are retired")
				}
				if ai := st.AIVMs[id]; ai != nil && (ai.Retired || ai.TeamID != team) {
					return false, errors.New("VM credentials are retired")
				}
			}
			syncWrite := foreign || (only != nil && !labeled)
			for _, id := range ids {
				if only != nil {
					st.VMSecrets[id] = append([]string{}, only...)
					setRunNames(st, id, runNames, false)
				}
				if foreign {
					st.ForeignVMs[id] = true
					if t := st.cowVMToken(id); t != nil {
						t.RestrictSelf = true
					}
					av := st.cowAIVM(id)
					if av == nil {
						av = &AIVM{TeamID: team}
						st.AIVMs[id] = av
					}
					av.Blocked = true
				}
				if needSelf {
					mintedBySelf := func(keyID string) bool { return strings.HasPrefix(keyID, "vm_"+id+"@") }
					t := st.cowVMToken(id)
					hasLinks := t != nil && !t.NoSelfAPI && hasLinksOf(st, mintedBySelf)
					if hasLinks {
						syncWrite = true
					}
					if t == nil {
						t = &VMToken{TeamID: team, NoSelfAPI: !injectSelfAPI}
						st.VMTokens[id] = t
					}
					if st.ForeignVMs[id] || foreign {
						t.RestrictSelf = true
					}
					t.bumpGeneration()
					selfGens[id] = t.Gen
					noSelfMap[id] = t.NoSelfAPI
					if hasLinks {
						revokeLinksOf(st, mintedBySelf)
					}
				}
				if needAI {
					av := st.cowAIVM(id)
					if av == nil {
						av = &AIVM{TeamID: team, Blocked: st.ForeignVMs[id] || foreign}
						st.AIVMs[id] = av
					}
					av.Gen++
					aiGens[id] = av.Gen
				}
				if env != nil {
					st.VMEnvs[id] = env.ID
					st.VMEnvPins[id] = pinOf(env)
				}
				if n := names[id]; n != "" {
					st.VMNames[id] = n
				}
				if len(extraEnv) > 0 && !noEnv {
					st.VMExtraEnv[id] = maps.Clone(extraEnv)
				}
				if noEnv {
					// no_env is sticky from the create on (bound to the token minted above).
					st.NoEnvRuns[id] = now
					markTokenNoEnv(st, id)
				}
			}
			return syncWrite, nil
		}
		var err error
		if stageIdem && !foreign && (only == nil || labeled) {
			err = s.store.Stage(func(st *State) error {
				syncWrite, merr := mutate(st)
				if merr != nil {
					return merr
				}
				if syncWrite {
					return s.store.persist(st)
				}
				return nil
			})
		} else {
			err = s.store.UpdateOrApply(mutate)
		}
		if err != nil {
			if needRecord {
				return nil, err
			}
			log.Printf("%s: create store update: %v", info(r).id, err)
			return extras, nil
		}
	}
	aiBase := strings.TrimRight(s.cfg.AIProxyURL, "/")
	for _, id := range ids {
		ex := extras[id]
		if gen := selfGens[id]; gen > 0 && base != "" && !noSelfMap[id] && vars["BOAT_API_KEY"] == "" {
			ex["BOAT_URL"], ex["BOAT_API_KEY"] = base, s.selfToken(id, gen)
		}
		if len(rawAI) > 0 {
			for k, v := range rawAI {
				ex[k] = v
			}
		} else if gen := aiGens[id]; gen > 0 {
			tok := s.aiToken(id, gen)
			for _, c := range creds {
				pr := aiProviders[c.Provider]
				ex[pr.EnvKey] = tok
				ex[pr.EnvBase] = aiBase + "/" + c.Provider + pr.BasePath
			}
		}
	}
	return extras, nil
}

// finishCreate gives a new VM what depends on its id (its own token, the AI
// proxy token) and its environment's setup.
func (s *server) finishCreate(r *http.Request, p *principal, vm VM, vars map[string]string, env *Environment, built, injectSelfAPI, foreign bool) VM {
	return s.finishCreateOpts(r, p, vm, vars, env, built, injectSelfAPI, foreign, true)
}

func (s *server) finishCreateOpts(r *http.Request, p *principal, vm VM, vars map[string]string, env *Environment, built, injectSelfAPI, foreign, provisionSSH bool) VM {
	id := str(vm, "id")
	extra := s.guestEnvWithPolicy(id, p.Team.ID, vars, injectSelfAPI, foreign)
	if ai, err := s.aiEnvNewVM(id, p.Team.ID); err != nil {
		log.Printf("%s: AI env for %s: %v", info(r).id, id, err)
	} else {
		for k, v := range ai {
			extra[k] = v
		}
	}
	s.pushEnv(r, id, vars, extra)
	if provisionSSH {
		s.provisionSSHPolicyLogged(r.Context(), id, p.Team.ID)
	}
	out := s.publicVMRun(vm)
	if env != nil {
		s.applyEnvToVM(r, p, id, env, built, vars, out)
	}
	return out
}

// validLabels checks a caller's labels: "" = fine.
func validLabels(labels map[string]string) string {
	if len(labels) > 32 {
		return "at most 32 labels"
	}
	for k, v := range labels {
		if !reLabel.MatchString(k) || len(v) > maxLabelValue {
			return "invalid label " + k
		}
		if strings.HasPrefix(k, reservedLabels) {
			return "labels starting with " + reservedLabels + " are reserved"
		}
	}
	return ""
}

// withSelection marks every new VM, plus its selection when it fits in a label.
func withSelection(labels map[string]string, only []string) (map[string]string, bool) {
	out := make(map[string]string, len(labels)+2)
	maps.Copy(out, labels)
	out[selectionMark] = "1"
	if only == nil {
		return out, false
	}
	v, ok := selectionLabel(only)
	if !ok {
		return out, false
	}
	out[secretsLabel] = v
	return out, true
}

// pushEnv rewrites the VM's /etc/boat/env. boatd's env op replaces the file.
// The error is logged here; callers that report VM readiness surface it.
func (s *server) pushEnv(r *http.Request, vmID string, base, extra map[string]string) error {
	return s.pushEnvVM(r, vmID, base, extra, nil)
}

// pushEnvVM is pushEnv for a caller that fails the VM closed when the push
// fails: failVM is the VM in hand (see recordEnvResultVM).
func (s *server) pushEnvVM(r *http.Request, vmID string, base, extra map[string]string, failVM VM) error {
	all := map[string]string{}
	for k, v := range base {
		all[k] = v
	}
	for k, v := range extra {
		all[k] = v
	}
	err := s.boatd.call(r.Context(), "POST", vmPath(vmID, "env"), map[string]any{"vars": all}, nil)
	if err != nil {
		log.Printf("%s: set env on %s: %v", info(r).id, vmID, err)
	}
	s.recordEnvResultVM(vmID, err, failVM)
	return err
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
	cid, _, err := s.startDetached(r, p, vmID, "setup", []string{"bash", "-lc", "set -e\n" + env.SetupScript}, vars, "")
	if err != nil {
		log.Printf("%s: setup on %s: %v", info(r).id, vmID, err)
		out["setup_error"] = "could not start the setup script"
		return
	}
	out["setup_cmd_id"] = cid
}

type ctxManagedRestart struct{}

// withManagedRestart marks r as the start half of a restart the server performs
// itself (recover, type change, restore after a failed one): startVM then keeps a
// still-marked no_env run no_env even if the caller's body forgot to say so.
func withManagedRestart(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxManagedRestart{}, true))
}

func isManagedRestart(r *http.Request) bool {
	v, _ := r.Context().Value(ctxManagedRestart{}).(bool)
	return v
}

// startLocks serializes the starts of one VM.
var startLocks keyedLocks

// Test hooks of startVM (nil in production): startPreGateHook runs under the
// start lock just before the not-ready gate is taken; startPostReadHook runs
// under the gate once the VM's state and the run's mark were read.
var (
	startPreGateHook  func(s *server, vmID string)
	startPostReadHook func(s *server, vmID string)
	// forkPreLockHook runs in a fork just before it takes its parent's start lock,
	// after the parent was read for authorization.
	forkPreLockHook func(s *server, parentID string)
	// forkPreparedHook runs after copy preparation releases the parent lock.
	forkPreparedHook func(s *server, parentID string)
	// snapshotPostPrepareHook runs in captureSnapshot under the VM's start lock,
	// after boatd prepared the snapshot and before its record is made.
	snapshotPostPrepareHook func(s *server, vmID string)
)

// keyedLocks is a set of mutexes by key, dropped when nobody holds or awaits one.
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.RWMutex
	refs int
}

// lock takes key's mutex exclusively.
func (k *keyedLocks) lock(key string) (unlock func()) {
	return k.take(key, false)
}

// rlock takes key's mutex shared: holders of it do not exclude each other, only
// lock.
func (k *keyedLocks) rlock(key string) (unlock func()) {
	return k.take(key, true)
}

func (k *keyedLocks) take(key string, shared bool) (unlock func()) {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyedLock{}
	}
	l := k.m[key]
	if l == nil {
		l = &keyedLock{}
		k.m[key] = l
	}
	l.refs++
	k.mu.Unlock()
	if shared {
		l.mu.RLock()
	} else {
		l.mu.Lock()
	}
	return func() {
		if shared {
			l.mu.RUnlock()
		} else {
			l.mu.Unlock()
		}
		k.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// pushRunningEnv replaces the env of a VM that is already running (a start with
// env or no_env, or a retry of a failed push). false: the response is written.
//
// boatd's set_env is atomic: a failed push leaves the guest its previous env
// file. So a failure on a healthy VM (no retry) is only reported (502): no
// env_error, no unbilled span, no gated commands, and the run's no_env mark is
// as it was. A VM already in env error stays there.
func (s *server) pushRunningEnv(w http.ResponseWriter, r *http.Request, id string, env map[string]string, noEnv, retry, hasBodyEnv bool, started, before VM) bool {
	prev := s.readRunMark(id)
	if noEnv {
		// Marked before the push (a prompt that races it must not see team secrets)
		// and put back below if the guest kept its old env.
		if err := s.markNoEnvRun(id, true); err != nil {
			s.storeErr(w, r, err)
			return false
		}
	}
	err := s.boatd.call(r.Context(), "POST", vmPath(id, "env"), map[string]any{"vars": env}, nil)
	if err == nil {
		s.recordEnvResult(id, nil)
		if retry {
			s.hooks.emitEnvRecovered(id)
		}
		return true
	}
	log.Printf("%s: set env on running %s: %v", info(r).id, id, err)
	if noEnv {
		s.restoreRunMark(id, prev)
	}
	if retry {
		s.recordEnvResult(id, err)
	}
	if hasBodyEnv || noEnv {
		writeErr(w, r, http.StatusBadGateway, "env_update_failed", "the VM is running but its environment could not be updated")
		return false
	}
	// A bare retry that failed again changes nothing: the VM stays in error.
	s.stops.supersede(id)
	writeJSON(w, 200, s.envFailedBody(started, before))
	return false
}

// errNoEnvSticky is the code of the 400 a no_env VM answers to a start or fork
// that names an environment or sends no_env:false.
const errNoEnvSticky = "no_env_sticky"

const msgNoEnvSticky = "this sandbox is no_env: it never gets team secrets or an environment again, so it cannot be started or forked with an environment or with no_env false; create a new sandbox to use an environment"

const msgNoEnvSecrets = "no_env sandboxes never get team secrets: a no_env create (or a create from a no_env snapshot) cannot name secrets; omit secrets or [], or drop no_env"

// hasSecretNames: whether a secrets selection names anything (absent and [] do not).
func hasSecretNames(secrets *[]string) bool {
	return secrets != nil && len(*secrets) > 0
}

// refuseStickyNoEnv answers 400 no_env_sticky and reports true when vmID is no_env
// (sticky) and the request names an environment or sends no_env:false: a no_env
// sandbox stays no_env, as boat.dev's. Nothing has been done when it answers.
func (s *server) refuseStickyNoEnv(w http.ResponseWriter, r *http.Request, vmID, environment string, noEnv *bool) bool {
	if environment == "" && (noEnv == nil || *noEnv) {
		return false
	}
	if !s.noEnvRun(vmID) {
		return false
	}
	writeErr(w, r, http.StatusBadRequest, errNoEnvSticky, msgNoEnvSticky)
	return true
}

func (s *server) startVM(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		FailFast bool              `json:"fail_fast"`
		Env      map[string]string `json:"env"` // this run's extra env, over team secrets
		TTLS     *int64            `json:"ttl_s"`
		NoEnvSet *bool             `json:"no_env"`
		NoEnv    bool              `json:"-"` // NoEnvSet is true
		// Environment applies that environment's vars and secret selection to this
		// run and the VM's later ones, as create does (boat.dev's resume --environment).
		// Refused (400 no_env_sticky) on a VM that is no_env.
		Environment string `json:"environment"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.NoEnv = body.NoEnvSet != nil && *body.NoEnvSet
	// A VM's own token never starts its VM: refused before anything below can
	// rotate a token generation.
	if p.VMID != "" {
		writeErr(w, r, http.StatusForbidden, "forbidden", "vm self-token cannot start its vm")
		return
	}
	if msg := validEnv(body.Env); msg != "" {
		writeErr(w, r, 400, "invalid_request", msg)
		return
	}
	if body.TTLS != nil && (*body.TTLS < 0 || *body.TTLS > maxTTL) {
		writeErr(w, r, 400, "invalid_request", "ttl_s must be 0 (no auto-stop) to 2592000")
		return
	}
	if len(body.Env) > 0 && !p.has("exec") {
		writeErr(w, r, http.StatusForbidden, "forbidden", "env requires scope exec")
		return
	}
	t := newPhases()
	vm, ok := s.ownVM(w, r, p) // refuses an environment builder for a non-admin key
	if !ok {
		return
	}
	id := r.PathValue("id")
	// no_env is sticky: a VM that is no_env takes no environment and no
	// no_env:false, whatever it names (before anything else is looked at).
	if s.refuseStickyNoEnv(w, r, id, body.Environment, body.NoEnvSet) {
		return
	}
	// An unknown environment (or one with no_env) refuses the start before the
	// VM is touched.
	startEnv, status, emsg := s.namedStartEnv(p.Team.ID, id, body.Environment, body.NoEnv)
	if emsg != "" {
		writeErr(w, r, status, envRefusalCode(status), emsg)
		return
	}
	// Starts of one VM are serialized here, and its state is read again under the
	// lock: whether this call cold-starts the VM (and so owns the run's no_env
	// mark) must not change under it.
	unlockStart := startLocks.lock(id)
	defer unlockStart()
	// Read again under the lock: a no_env start may have marked the VM meanwhile.
	if s.refuseStickyNoEnv(w, r, id, body.Environment, body.NoEnvSet) {
		return
	}
	// The not-ready gate is taken first, before this start reads the VM's state or
	// the run's no_env mark: while it is held noEnvRun never ends or drops the mark,
	// so a stop plus a read racing this start cannot lose it between the read and
	// the gate. It also covers a start of a VM read as running: boatd may serve it
	// after a stop took the VM lock first, restoring the VM. Released below once
	// the start settled (every return path).
	if startPreGateHook != nil {
		startPreGateHook(s, id)
	}
	releaseGate := s.markEnvPush(id)
	defer releaseGate()
	var fresh VM
	if err := s.boatd.call(r.Context(), "GET", vmPath(id), nil, &fresh); err != nil {
		writeUpstream(w, r, err)
		return
	}
	vm = fresh
	if str(vm, "id") != id || str(vm, "team") != p.Team.ID {
		writeErr(w, r, http.StatusNotFound, "not_found", "vm not found")
		return
	}
	if s.revalidatePrincipal(w, r, p, "vms:write") == nil || s.refuseBuilder(w, r, p, vm) {
		return
	}
	if err := s.adoptCopyPolicy(vm); err != nil {
		s.storeErr(w, r, err)
		return
	}
	startEnv, status, emsg = s.namedStartEnv(p.Team.ID, id, body.Environment, body.NoEnv)
	if emsg != "" {
		writeErr(w, r, status, envRefusalCode(status), emsg)
		return
	}
	alreadyRunning := str(vm, "state") == "running"
	pending := s.store.Snapshot().VMTransitions[id]
	if pending != nil {
		selectedID := ""
		if startEnv != nil && startEnv.env != nil {
			selectedID = startEnv.env.ID
		}
		if (startEnv != nil && (pending.NoEnv || selectedID != pending.EnvID)) || (body.NoEnv && !pending.NoEnv) ||
			(len(body.Env) > 0 && !maps.Equal(body.Env, pending.Extra)) {
			writeErr(w, r, http.StatusConflict, "credential_transition_pending", "retry the pending credential transition before choosing a different environment")
			return
		}
		if alreadyRunning {
			s.stopFailClosed(r, id, s.readRunMark(id))
			writeErr(w, r, http.StatusConflict, "stop_required", "a credential transition is pending; retry after the VM stops")
			return
		}
		// Retry exactly the pinned intent, including when the same environment
		// name now resolves to a newer version.
		startEnv = pending.named(p.Team.ID)
		body.Env = maps.Clone(pending.Extra)
		if pending.NoEnv {
			body.NoEnv = true
		}
	}
	// An empty start of a running VM whose environment never arrived is a retry of
	// that push, built like the original start.
	retryEnv := alreadyRunning && s.envErrorOf(vm) != ""
	// no_env is sticky: a VM that was ever started, created or forked with no_env
	// is started with no_env again by every later start (plain, type change,
	// recover, restore), whatever body the path built.
	marked := s.noEnvRun(id)
	prevMark := s.readRunMark(id)
	if startPostReadHook != nil {
		startPostReadHook(s, id)
	}
	if !alreadyRunning && marked {
		body.NoEnv = true
	}
	// A no_env VM stays no_env: a start of the running VM that carries env (or
	// retries) pushes only that env, never the team's secrets.
	keepMark := alreadyRunning && marked
	if alreadyRunning && startEnv != nil {
		// The run holds the secrets of the selection it started with (processes' environ,
		// the guest's env file): a different environment needs a new run.
		writeErr(w, r, http.StatusConflict, "stop_required", "the VM is running: stop it first to start it with an environment")
		return
	}
	if alreadyRunning && body.NoEnv && !keepMark {
		// The run already holds team secrets (its processes' environ, the guest's env
		// file): no_env is only a cold start's, never a conversion in place.
		writeErr(w, r, http.StatusConflict, "stop_required", "the VM is running with its environment: stop it first; a no_env run needs a fresh boot")
		return
	}
	noEnv := body.NoEnv || keepMark
	if noEnv && !marked && s.store.Snapshot().ForeignVMs[id] {
		writeErr(w, r, http.StatusConflict, "unsafe_source", "a foreign disk without proven protected provenance cannot be converted to no_env safely before boot")
		return
	}
	effectiveExtra := body.Env
	if len(effectiveExtra) == 0 {
		effectiveExtra = s.store.Snapshot().VMExtraEnv[id]
	}
	neverLanded := !alreadyRunning && s.envNeverLanded(id)
	heldNames, runKnown := runNamesOf(s.store.Snapshot(), id)
	freshEnv := !alreadyRunning && !noEnv && (!runKnown || (startEnv != nil && s.forkEnvNeedsFreshBoot(p.Team.ID, id, prevMark.had, startEnv, effectiveExtra)) || pending != nil)
	coldNoEnv := body.NoEnv && !alreadyRunning
	noEnvFresh := coldNoEnv && (!prevMark.had || !runKnown || len(heldNames) != 0 || pending != nil || neverLanded)
	protectedCold := freshEnv || noEnvFresh
	if protectedCold {
		sel := s.vmSecretsOf(id, vm)
		if noEnv {
			sel = []string{}
		} else if es, ok := startEnv.selection(); ok {
			sel = es
		}
		if err := s.store.Update(func(st *State) error {
			if _, err := requirePrincipalIn(st, p, "vms:write", time.Now()); err != nil {
				return err
			}
			intentEnv := startEnv
			if !noEnv && intentEnv == nil {
				// Plain cold retries retain the existing pin and selection too.
				intentEnv = &namedEnv{sel: sel, keepSel: st.ForeignVMs[id]}
				if eid := st.VMEnvs[id]; eid != "" {
					pin := s.parentPin(st, p.Team.ID, id)
					if pin != nil {
						intentEnv.env = &Environment{ID: eid, TeamID: p.Team.ID, Version: pin.Version, Env: maps.Clone(pin.Vars)}
					}
				}
			}
			intent := transitionFor(noEnv, intentEnv, sel)
			intent.Extra = maps.Clone(effectiveExtra)
			if noEnv {
				intent.Extra = maps.Clone(body.Env)
			}
			st.VMTransitions[id] = intent
			return nil
		}); err != nil {
			s.storeErr(w, r, err)
			return
		}
	}
	// A VM whose env never landed (it failed closed at create or fork) is started
	// like a no_env cold start in that respect: its env must land or it is stopped.
	if !alreadyRunning {
		// A VM that left running without an API call (TTL, crash) may still carry
		// an open env-error span: it ended when the VM did.
		s.closeEnvErrorSpan(id, vmStoppedAt(vm, time.Now()))
	}
	t.mark("get")
	defer t.done("start", id)
	env := map[string]string{}
	var runNames []string // the team secrets this start pushes
	if !noEnv {
		var ok bool
		sel := s.vmSecretsOf(id, vm)
		if es, ok := startEnv.selection(); ok {
			sel = es
		}
		env, ok = s.envOrFail(w, r, p, sel)
		if !ok {
			return
		}
		runNames = envNames(env)
		if startEnv != nil {
			for k, v := range startEnv.vars() {
				env[k] = v
			}
		} else {
			for k, v := range s.pinnedVars(s.store.Snapshot(), p.Team.ID, id) {
				env[k] = v
			}
		}
	}
	// This run's extra env: the one the request gives (it replaces the stored one),
	// else the one stored for the VM, over the environment's vars. A no_env run
	// takes only what the request gives: the stored env is never replayed to it.
	extraEnv := body.Env
	if len(extraEnv) == 0 && !noEnv {
		extraEnv = s.store.Snapshot().VMExtraEnv[id]
	}
	for k, v := range extraEnv {
		env[k] = v
	}
	// A new token generation per cold/resumed run: tokens copied from a past run are dead.
	// On an already-running VM boatd Start is a no-op, so preserve active tokens.
	ai, err := s.aiEnvRun(id, p.Team.ID, !alreadyRunning, noEnv)
	if err != nil {
		log.Printf("%s: AI env for %s: %v", info(r).id, id, err)
		writeErr(w, r, 500, "secrets_unavailable", "cannot prepare AI credentials")
		return
	}
	for k, v := range ai {
		env[k] = v
	}
	canMintSelf := p.has("exec") && p.has("vms:read")
	var guestVars map[string]string
	if alreadyRunning {
		guestVars = s.guestEnvExisting(id, p.Team.ID, env, canMintSelf)
	} else {
		guestVars = s.guestEnvForCaller(id, p.Team.ID, env, canMintSelf)
	}
	for k, v := range guestVars {
		env[k] = v
	}
	t.mark("env")
	if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
		return
	}
	in := map[string]any{"env": env}
	if body.FailFast {
		in["fail_fast"] = true
	}
	if body.TTLS != nil {
		in["ttl_s"] = *body.TTLS
	}
	// A cold start with no_env records the mark before the start (fail closed: a
	// prompt that races the start must not see team secrets); a normal cold start
	// forgets it afterwards. On a running VM the mark follows the env push below.
	// The not-ready gate (taken at the top) is held until the start has settled
	// (a start with no env still ends with the mark below): while it is set
	// /prompt and /prompts refuse, and noEnvRun keeps the run's mark.
	if !alreadyRunning && len(env) > 0 {
		// boatd announces the VM running before this call knows whether its env landed.
		defer s.hooks.expectEnv(p.Team.ID)()
	}
	if coldNoEnv {
		if noEnvFresh {
			// The previous run may have had team secrets: its /etc/boat/env stays on
			// the disk and a default stop's memory snapshot resumes its processes
			// with them in their environ. A no_env run boots from scratch instead.
			in["fresh_boot"] = true
		}
		if err := s.markNoEnvRun(id, true); err != nil {
			s.storeErr(w, r, err)
			return
		}
	}
	// A start that names an environment whose selection does not cover every secret
	// the previous run held cold-boots (the disk only): resuming that memory would
	// keep the secrets the narrower environment leaves out. The memory of a no_env
	// run holds none.
	if freshEnv {
		in["fresh_boot"] = true
	}
	if !body.NoEnv && !alreadyRunning {
		// A normal cold start forgets the previous run's no_env mark BEFORE boatd
		// starts the VM (inside the gate): if the call errors the VM may be running
		// with the team's secrets, and a surviving mark would call that run no_env.
		// Failing closed: a mark that cannot be dropped stops the start here.
		if err := s.markNoEnvRun(id, false); err != nil {
			log.Printf("%s: clear no_env run of %s: %v", info(r).id, id, err)
			s.storeErr(w, r, err)
			return
		}
	}
	tok := s.store.Snapshot().VMTokens[id]
	if !alreadyRunning && tok != nil && (freshEnv || (tok.NarrowGen != 0 && tok.NarrowGen == tok.Gen)) {
		if err := s.store.Update(func(st *State) error {
			if t := st.VMTokens[id]; t != nil && !t.Retired {
				t = st.cowVMToken(id)
				t.NarrowGen, t.NarrowAt = t.Gen, time.Now().UTC()
			}
			return nil // persist the restriction before publishing the token
		}); err != nil {
			s.storeErr(w, r, err)
			return
		}
	}
	if !noEnv && (!alreadyRunning || len(body.Env) > 0 || retryEnv) {
		if err := s.recordPossibleRunNames(p, "vms:write", id, runNames); err != nil {
			s.storeErr(w, r, err)
			return
		}
	}
	var started VM
	if err := s.boatd.call(r.Context(), "POST", vmPath(id, "start"), in, &started); err != nil {
		s.settleFailedStart(r, id, coldNoEnv || freshEnv || neverLanded, prevMark)
		writeUpstream(w, r, err)
		return
	}
	if started != nil && (!alreadyRunning || !s.sshPolicyCurrent(s.store.Snapshot(), started)) {
		s.provisionSSHPolicyLoggedWithVM(r.Context(), str(started, "id"), str(started, "team"), started)
	}
	if alreadyRunning && (len(body.Env) > 0 || body.NoEnv || retryEnv) {
		if !s.pushRunningEnv(w, r, id, env, body.NoEnv, retryEnv, len(body.Env) > 0, started, vm) {
			return
		}
	}
	if !alreadyRunning && !noEnv {
		// The run's team secrets, for a snapshot of it: a push that did not land
		// leaves the guest its old env, so that run's names stay in (and an unknown
		// old run stays unknown).
		ready, hasReady := started["agent_ready"].(bool)
		failed := started == nil || ((hasReady && !ready) || str(started, "error") != "") || (protectedCold && !ready)
		// A cold start resumes the stopped run's memory, whose processes still hold the
		// secrets that run had (an un-defaulted secret included): the record keeps them,
		// so a snapshot of this run is not mistaken for one that never held them. Only
		// the memory of a no_env run holds none, so only after one is the record replaced
		// (an unrecorded previous run has nothing to keep: the start records its own).
		_, prevKnown := runNamesOf(s.store.Snapshot(), id)
		s.recordRunNames(id, runNames, failed || (!freshEnv && !prevMark.had && prevKnown))
		if startEnv != nil && !failed && !protectedCold {
			// The environment's selection and vars now are the VM's: later prompts,
			// commands and starts use them. Not recorded when the push did not land.
			if err := s.recordStartEnv(p, id, startEnv); err != nil {
				s.stopFailClosed(r, id, prevMark)
				s.storeErr(w, r, err)
				return
			}
		}
	}
	if coldNoEnv && started != nil {
		// A no_env cold start boots without the previous run's memory (fresh_boot
		// after a run that held secrets), so the run holds no team secret: record
		// that, replacing the previous run's names, so a snapshot of it is @no_env.
		// A push that did not land fails closed below and keeps the old names.
		ready, hasReady := started["agent_ready"].(bool)
		if !(hasReady && !ready) && str(started, "error") == "" {
			s.recordRunNames(id, []string{}, false)
		}
	}
	if !alreadyRunning && (len(env) > 0 || coldNoEnv || freshEnv || neverLanded) {
		// boatd applied the env during the start; a failed set_env leaves the VM
		// running with agent_ready=false and an error rather than failing the start.
		ready, hasReady := started["agent_ready"].(bool)
		failed := started == nil || (hasReady && !ready) || str(started, "error") != "" || (protectedCold && !ready)
		if len(env) > 0 || failed {
			var failVM VM // the fail-closed stop below: sandbox.error from this VM, before it
			if failed && (coldNoEnv || freshEnv || neverLanded) {
				failVM = started
			}
			s.recordEnvResultVM(id, boolErr(failed), failVM)
		}
		if failed && (coldNoEnv || freshEnv || neverLanded) {
			// Fail closed: the guest may still hold the previous run's env (team
			// secrets) that native exec and SSH source, so the VM is not left running.
			log.Printf("%s: start of %s (no_env %v, env never landed %v) did not apply the env: agent_ready=%v error=%q; stopping it", info(r).id, id, coldNoEnv, neverLanded, ready, str(started, "error"))
			stopped := s.stopFailClosed(r, id, prevMark)
			releaseGate()
			if !stopped {
				writeErr(w, r, http.StatusBadGateway, "env_update_failed", "the VM's environment could not be set and the VM could not be stopped")
				return
			}
			writeEnvStartFailed(w, r, s.publicVMState(s.postStopVM(r, id, started)), "the VM's environment could not be set; the VM was stopped")
			return
		}
		if failed {
			log.Printf("%s: start of %s did not apply the env: agent_ready=%v error=%q", info(r).id, id, ready, str(started, "error"))
			s.stops.supersede(id) // the VM is running again, whatever its env
			releaseGate()
			// boatd did start the VM: answer with it, as create and fork do.
			writeJSON(w, 200, s.envFailedBody(started, vm))
			return
		}
		if neverLanded {
			s.clearEnvNeverLanded(id) // managed env delivery acknowledged; tenant disk cleanup is not certified
		}
	}
	if len(body.Env) > 0 && !noEnv && !protectedCold {
		// The env this run took replaces the stored one: later starts replay it. A
		// no_env run's is never stored.
		if err := s.recordExtraEnv(p, id, body.Env); err != nil {
			log.Printf("%s: record env of %s: %v", info(r).id, id, err)
		}
	}
	if protectedCold {
		if err := s.completeCredentialStart(p, id, startEnv, extraEnv, runNames, noEnv); err != nil {
			s.stopFailClosed(r, id, prevMark)
			s.storeErr(w, r, err)
			return
		}
	}
	s.stops.supersede(id)
	releaseGate() // the start settled: the answer reports env_pending=false
	if started == nil {
		writeJSON(w, 200, map[string]any{"ok": true})
	} else {
		writeJSON(w, 200, s.publicVMState(started))
	}
	t.mark("boatd")
}

// writeEnvStartFailed answers a no_env cold start that failed closed (the VM was
// stopped and its memory snapshot is gone): 502 env_start_failed, the standard
// error envelope plus "vm", the VM as it now is (state stopped).
func writeEnvStartFailed(w http.ResponseWriter, r *http.Request, vm map[string]any, msg string) {
	out := make(map[string]any, len(vm))
	for k, v := range vm {
		out[k] = v
	}
	stoppedVM(out)
	writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]string{
		"code": "env_start_failed", "message": msg, "request_id": info(r).id}, "vm": out})
}

// stoppedVM turns a VM body into the one of a VM that was just stopped: no
// expiry, no environment error, not ready.
func stoppedVM(vm VM) {
	vm["state"] = "stopped"
	delete(vm, "expires_at")
	delete(vm, "error")
	delete(vm, "env_error")
	vm["agent_ready"] = false
}

// postStopVM is the VM as boatd reports it after a fail-closed stop (no stale
// expiry or environment error); without that read, fallback made stopped.
func (s *server) postStopVM(r *http.Request, id string, fallback VM) VM {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), failClosedTimeout)
	defer cancel()
	var cur VM
	if err := s.boatd.call(ctx, "GET", vmPath(id), nil, &cur); err == nil && str(cur, "state") == "stopped" {
		return cur
	}
	out := make(VM, len(fallback)+1)
	for k, v := range fallback {
		out[k] = v
	}
	out["id"] = id
	stoppedVM(out)
	return out
}

// ---- "env never landed" ----
//
// A fresh-boot create or fork that fails closed leaves a stopped VM whose disk
// still holds /etc/boat/env of the snapshot or parent it came from (another run's
// secrets). Until one environment push lands on it, a start must not leave it
// running with that file: the mark makes such a start fail closed too (502
// env_start_failed, stopped). The mark lives in State.VMSecrets under
// envNeverPrefix+id (a map of name lists that nothing but a VM id, or a "snap:"
// record, is looked up by); forgetVM drops it with the VM.

const envNeverPrefix = "envnever:"

func envNeverKey(vmID string) string { return envNeverPrefix + vmID }

func (s *server) envNeverLanded(vmID string) bool {
	_, ok := s.store.Snapshot().VMSecrets[envNeverKey(vmID)]
	return ok
}

// markEnvNeverLanded persists the mark. If the durable write fails, it still
// applies the mark in memory (so this process never treats the VM as clean)
// and returns the error so callers fail closed instead of keeping an unmarked
// stopped VM.
func (s *server) markEnvNeverLanded(vmID string) error {
	if err := s.store.Update(func(st *State) error {
		st.VMSecrets[envNeverKey(vmID)] = []string{"env_never_landed"}
		return nil
	}); err != nil {
		log.Printf("mark %s as env never landed: %v", vmID, err)
		_ = s.store.Apply(func(st *State) error {
			st.VMSecrets[envNeverKey(vmID)] = []string{"env_never_landed"}
			return nil
		})
		return err
	}
	return nil
}

func (s *server) clearEnvNeverLanded(vmID string) {
	if !s.envNeverLanded(vmID) {
		return
	}
	if err := s.store.Update(func(st *State) error {
		delete(st.VMSecrets, envNeverKey(vmID))
		return nil
	}); err != nil {
		log.Printf("clear env never landed of %s: %v", vmID, err)
	}
}

// failClosedTimeout bounds the boatd calls that settle a failed start: they run
// even when the caller's request is already gone.
const failClosedTimeout = 30 * time.Second

// settleFailedStart runs when boatd's start call errored (the VM may be running
// anyway: the answer was lost, or boatd failed after booting). Under the gate it
// reads the VM again: a stopped VM did not run, so a cold no_env start gives its
// mark back; a VM that is not stopped is stopped (cold no_env start: its env may
// not have landed).
func (s *server) settleFailedStart(r *http.Request, id string, coldNoEnv bool, prev runMark) {
	if !coldNoEnv {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), failClosedTimeout)
	defer cancel()
	var vm VM
	if err := s.boatd.call(ctx, "GET", vmPath(id), nil, &vm); err == nil && str(vm, "state") == "stopped" {
		s.restoreRunMark(id, prev) // it never ran: the previous mark stands
		return
	}
	s.stopFailClosed(r, id, prev)
}

// stopFailClosed force-stops a VM whose no_env start may have left the previous
// run's environment in the guest. It is not a stop operation: nothing records it
// as ending a run. A stopped VM gets the previous run's mark back (the next no_env
// start boots fresh again); one that could not be stopped keeps the no_env mark.
func (s *server) stopFailClosed(r *http.Request, id string, prev runMark) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), failClosedTimeout)
	defer cancel()
	if err := s.boatd.call(ctx, "POST", vmPath(id, "stop"), map[string]any{"force": true}, nil); err != nil {
		log.Printf("%s: fail-closed stop of %s: %v", info(r).id, id, err)
		return false
	}
	s.closeEnvErrorSpan(id, time.Now())
	s.restoreRunMark(id, prev)
	return true
}

// envFailedBody is a start's answer when the VM runs but its environment never
// reached the guest: the VM (as create and fork report it) with agent_ready=false
// and env_error. started is boatd's answer, or the VM as it was before when boatd
// gave none.
func (s *server) envFailedBody(started, before VM) VM {
	vm := started
	if vm == nil {
		vm = before
	}
	out := s.publicVMRun(vm)
	out["agent_ready"] = false
	out["env_error"] = envErrorMessage
	return out
}

func (s *server) stopVM(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		// Force kills the VM without a snapshot: the next start is a cold boot.
		Force bool `json:"force"`
	}
	if !decode(w, r, &body) {
		return
	}
	var in any
	if body.Force {
		in = map[string]any{"force": true}
	}
	t := newPhases()
	if _, ok := s.ownVM(w, r, p); ok {
		if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
			return
		}
		t.mark("get")
		id := r.PathValue("id")
		if isManagedStop(r) {
			if s.vmAction(w, r, "stop", in) {
				s.closeEnvErrorSpan(id, time.Now())
			}
		} else {
			op := s.stops.begin(p.Team.ID, id)
			var vm VM
			if err := s.boatd.call(r.Context(), "POST", vmPath(id, "stop"), in, &vm); err != nil {
				s.stops.failAsync(id, op, err.Error())
				writeUpstream(w, r, err)
				return
			}
			s.stops.completeAsync(id, op)
			s.closeEnvErrorSpan(id, time.Now())
			if vm == nil {
				writeJSON(w, 200, map[string]any{"ok": true})
			} else {
				writeJSON(w, 200, s.withDisplay(vm))
			}
		}
		t.mark("boatd")
		t.done("stop", id)
	}
}

func (s *server) deleteVM(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); ok {
		if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
			return
		}
		id := r.PathValue("id")
		// The retirement is the one durable write: it must be on disk before the
		// delete is attempted (a persistence failure refuses the delete).
		if err := s.retireVM(id); err != nil {
			s.storeErr(w, r, err)
			return
		}
		if !s.vmAction(w, r, "delete", nil) {
			return
		}
		// forgetVM is one coalesced background write that also drops the VM from
		// SSH grants, so no host-wide syncJumpKeys scan or per-VM re-attestation
		// is needed here.
		s.forgetVM(id)
	}
}

func (s *server) forkVM(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Name     string            `json:"name"`
		FailFast bool              `json:"fail_fast"`
		Env      map[string]string `json:"env"` // the child's extra env, over team secrets
		// Labels are the child's own (absent: none: a fork never takes the parent's
		// user labels); the parent's reserved boat.* labels always carry over (except
		// boat.secrets when no_env).
		Labels map[string]string `json:"labels"`
		TTLS   *int64            `json:"ttl_s"`
		// NoEnv absent: the child follows the parent's run (a no_env run forks a
		// no_env child); an explicit false releases it with the team's secrets.
		NoEnv *bool `json:"no_env"`
		// Environment applies that environment's vars and secret selection to the
		// child as create does (r43: boat.dev's fork --environment).
		Environment string `json:"environment"`
	}
	t := newPhases()
	var forkEnv *namedEnv
	authorize := func() bool {
		if !decode(w, r, &body) {
			return false
		}
		if p.VMID != "" {
			writeErr(w, r, 403, "forbidden", "vm self-token cannot fork vms")
			return false
		}
		if body.Name != "" {
			name, msg := validDisplayName(body.Name)
			if msg != "" {
				writeErr(w, r, 400, "invalid_request", msg)
				return false
			}
			body.Name = name
		}
		if msg := validLabels(body.Labels); msg != "" {
			writeErr(w, r, 400, "invalid_request", msg)
			return false
		}
		if msg := validEnv(body.Env); msg != "" {
			writeErr(w, r, 400, "invalid_request", msg)
			return false
		}
		if body.TTLS != nil && (*body.TTLS < 0 || *body.TTLS > maxTTL) {
			writeErr(w, r, 400, "invalid_request", "ttl_s must be 0 (no auto-stop) to 2592000")
			return false
		}
		if len(body.Env) > 0 && !p.has("exec") {
			writeErr(w, r, http.StatusForbidden, "forbidden", "env requires scope exec")
			return false
		}
		var ok bool
		_, ok = s.ownVM(w, r, p)
		if !ok {
			return false
		}
		// A no_env parent forks no_env children only (a fork of it is always no_env).
		if s.refuseStickyNoEnv(w, r, r.PathValue("id"), body.Environment, body.NoEnv) {
			return false
		}
		// Before anything is copied: an unknown environment (or one that conflicts
		// with no_env) refuses the fork.
		var (
			status int
			msg    string
		)
		if forkEnv, status, msg = s.namedStartEnv(p.Team.ID, r.PathValue("id"), body.Environment, body.NoEnv != nil && *body.NoEnv); msg != "" {
			writeErr(w, r, status, envRefusalCode(status), msg)
			return false
		}
		return true
	}
	execute := func(w http.ResponseWriter) {
		if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
			return
		}
		t.mark("get")
		var child VM
		parent := r.PathValue("id")
		// The parent's run mark decides whether the child is released by a cold
		// boot, and the prepare copies the parent's memory: a start of the parent
		// (startLocks, exclusive) must not slip between the two. Forks of one parent
		// share the lock: they do not exclude each other.
		if forkPreLockHook != nil {
			forkPreLockHook(s, parent)
		}
		unlockParent := startLocks.rlock(parent)
		parentLocked := true
		releaseParent := func() {
			if parentLocked {
				parentLocked = false
				unlockParent()
			}
		}
		defer releaseParent()
		// The parent as it is now: the one read before the lock may predate a start
		// that held it (a no_env cold start turns a stopped secret run into a no_env
		// one), and the run mark, the labels and the selection below must all be the
		// post-start parent's, like startVM's re-read under its lock.
		var pvm VM
		if err := s.boatd.call(r.Context(), "GET", vmPath(parent), nil, &pvm); err != nil {
			writeUpstream(w, r, err)
			return
		}
		if str(pvm, "team") != p.Team.ID {
			writeErr(w, r, 404, "not_found", "vm not found")
			return
		}
		if s.revalidatePrincipal(w, r, p, "vms:write") == nil || s.refuseBuilder(w, r, p, pvm) {
			return
		}
		if err := s.adoptCopyPolicy(pvm); err != nil {
			s.storeErr(w, r, err)
			return
		}
		var status int
		var msg string
		forkEnv, status, msg = s.namedStartEnv(p.Team.ID, parent, body.Environment, body.NoEnv != nil && *body.NoEnv)
		if msg != "" {
			writeErr(w, r, status, envRefusalCode(status), msg)
			return
		}
		noEnv, parentNoEnv := s.forkNoEnv(body.NoEnv, parent, pvm)
		// Read again under the parent's start lock: it may have become no_env since
		// the request was authorized.
		if parentNoEnv && s.refuseStickyNoEnv(w, r, parent, body.Environment, body.NoEnv) {
			return
		}
		// A no_env child of a parent whose run is not no_env inherits the parent's
		// memory (its processes' environ hold team secrets) and its disk: it is
		// released by a cold boot (fresh_boot), never by resuming that memory.
		// A parent whose environment never landed (it failed closed at create or fork)
		// still holds another run's /etc/boat/env on its disk, which the child copies:
		// the child is released like a fresh boot that must fail closed, and inherits
		// the mark until one push lands on it.
		parentState := s.store.Snapshot() // immutable parent metadata, captured before copy under its lifecycle lock
		parentNever := s.envNeverLanded(parent)
		if parentState.VMTransitions[parent] != nil {
			writeErr(w, r, http.StatusConflict, "sandbox_not_ready", "the parent's credential transition is pending; resume it to retry")
			return
		}
		parentProtected := s.snapshotRunNoEnv(parent, pvm)
		if noEnv && !parentProtected && parentState.ForeignVMs[parent] {
			writeErr(w, r, http.StatusConflict, "unsafe_source", "a protected fork of a foreign disk requires a proven protected parent")
			return
		}
		effectiveExtra := body.Env
		if len(effectiveExtra) == 0 {
			effectiveExtra = parentState.VMExtraEnv[parent]
		}
		freshBoot := (noEnv && !parentProtected) || parentNever || s.forkEnvNeedsFreshBoot(p.Team.ID, parent, parentNoEnv, forkEnv, effectiveExtra)
		displayChild := body.Name // free text: boatd gets its DNS slug
		fin := map[string]any{"name": hostnameOf(body.Name), "team": p.Team.ID}
		if body.FailFast {
			fin["fail_fast"] = true
		}
		// A fork without a ttl gets boat.dev's default (1 hour), not its parent's
		// lifetime: a parent with auto-stop off does not make an immortal child. An
		// explicit 0 still means no auto-stop.
		fin["ttl_s"] = int64(defaultForkTTLS)
		if body.TTLS != nil {
			fin["ttl_s"] = *body.TTLS
		}
		if s.cfg.SSHPolicyHelperFile != "" {
			fin["reprovision_ssh"] = true
		}
		// The child never takes the parent's user labels (boat.dev): the request's
		// labels, else none, plus the parent's reserved boat.* ones. The map is
		// always sent, so boatd does not copy the parent's labels itself.
		labels := map[string]string{}
		pl, _ := pvm["labels"].(map[string]any)
		maps.Copy(labels, body.Labels)
		for k, v := range pl {
			if sv, ok := v.(string); ok && strings.HasPrefix(k, reservedLabels) {
				if _, envSel := forkEnv.selection(); (noEnv || envSel) && (k == secretsLabel || (noEnv && k == builderLabel)) {
					continue
				}
				labels[k] = sv
			}
		}
		if es, ok := forkEnv.selection(); ok {
			if v, ok := selectionLabel(es); ok {
				labels[secretsLabel] = v
			}
		}
		labels[selectionMark] = "1"
		if noEnv {
			labels[secretsLabel], labels[noEnvLabel] = "", "1"
			delete(labels, protectedLabel)
			if parentProtected {
				labels[protectedLabel] = "1"
			}
		}
		if parentState.ForeignVMs[parent] {
			labels[foreignLabel] = "1"
		}
		if tok := parentState.VMTokens[parent]; tok != nil {
			if tok.NoSelfAPI {
				labels[noSelfLabel] = "1"
			}
			if tok.RestrictSelf || tok.NarrowGen != 0 {
				labels[restrictedLabel] = "1"
			}
		}
		if freshBoot || noEnv || parentState.ForeignVMs[parent] {
			labels[restrictedLabel] = "1"
		}
		var sel []string
		if noEnv {
			sel = []string{}
		} else {
			sel = s.vmSecretsOf(parent, pvm)
			if es, ok := forkEnv.selection(); ok {
				sel = es
			}
			if sel != nil {
				sel = append([]string{}, sel...)
			}
		}
		if sel != nil {
			delete(labels, secretsLabel)
			if value, ok := selectionLabel(sel); ok {
				labels[secretsLabel] = value
			}
		}
		fin["labels"] = labels
		defer s.hooks.expectEnv(p.Team.ID)() // the child's running event waits for its env
		err := s.boatd.call(r.Context(), "POST", vmPath(parent, "fork", "prepare"), fin, &child)
		releaseParent()
		if forkPreparedHook != nil && err == nil {
			forkPreparedHook(s, parent)
		}
		if err != nil {
			writeUpstream(w, r, err)
			return
		}
		t.mark("boatd")
		childID := str(child, "id")
		defer t.done("fork", childID)
		releaseChild := s.markEnvPush(childID) // provisioning until the release settled its env
		defer releaseChild()
		// The child inherits the parent's disk, hence its secrets, so it keeps the
		// parent's selection for later starts too, and its foreign mark, unless no_env
		// clears secret injection for the child.
		_, labeled := labeledSelection(child)
		foreign := parentState.ForeignVMs[parent]
		vars := map[string]string{}
		var (
			envID         string
			childPin      *EnvPin // the environment version the child is pinned to
			prepErr       error
			childRunNames []string // the team secrets the child's run holds
			childRunKnown bool
		)
		if noEnv {
			childRunNames, childRunKnown = []string{}, true
		}
		if !noEnv {
			vars, prepErr = s.teamEnv(p.Team.ID, sel)
			if prepErr == nil {
				childRunNames, childRunKnown = envNames(vars), true
				if !freshBoot {
					// The child resumes the parent's memory: its run holds what the
					// parent's did too (unknown when the parent's is).
					pn, ok := runNamesOf(parentState, parent)
					childRunNames, childRunKnown = mergeNames(pn, childRunNames), ok
				}
				if forkEnv != nil {
					if forkEnv.env != nil {
						envID = forkEnv.env.ID
						childPin = pinOf(forkEnv.env)
					}
					for k, v := range forkEnv.vars() {
						vars[k] = v
					}
				} else if eid := parentState.VMEnvs[parent]; eid != "" {
					envID = eid
					// The child copies the parent's pin: the vars of the version the
					// parent runs on, not the environment's current ones.
					childPin = s.parentPin(parentState, p.Team.ID, parent)
					if childPin != nil {
						for k, v := range childPin.Vars {
							vars[k] = v
						}
					}
				}
			}
		}
		// The child's per-sandbox env: the request's (replacing), else the parent's.
		// A no_env child takes only the env the request gives: the parent's stored
		// one is not replayed to it, and the child's is not stored.
		childExtra := body.Env
		if len(childExtra) == 0 && !noEnv {
			childExtra = parentState.VMExtraEnv[parent]
		}
		for k, v := range childExtra {
			vars[k] = v
		}
		// Team credential configuration is current; inherited parent metadata
		// continues to use the immutable view from copy preparation.
		st := s.store.Snapshot()
		var creds []*AICredential
		for _, c := range st.AICreds {
			if c.TeamID == p.Team.ID {
				creds = append(creds, c)
			}
		}
		rawAI := map[string]string{}
		needChildAI := false
		if prepErr == nil && len(creds) > 0 {
			if st.AIProxyOff[p.Team.ID] {
				if !foreign && !noEnv { // a no_env child holds no raw team key
					for _, c := range creds {
						key, kerr := s.keys.Open(c.Sealed, "ai/"+aiCredKey(c.TeamID, c.Provider))
						if kerr != nil {
							prepErr = fmt.Errorf("decrypt %s credential: %w", c.Provider, kerr)
							break
						}
						rawAI[aiProviders[c.Provider].EnvKey] = string(key)
					}
				}
			} else if s.cfg.AIProxyURL != "" {
				needChildAI = true
			} else {
				log.Printf("vm %s: team %s has AI credentials but -ai-proxy-url is not set; none injected", childID, p.Team.ID)
			}
		}
		if prepErr != nil {
			if cleanup := s.discardCopyVM(childID); cleanup != nil {
				writeErr(w, r, 500, "cleanup_failed", "credential preparation failed; the pending VM "+childID+" could not be deleted")
				return
			}
			writeErr(w, r, 500, "store_error", "cannot prepare the child credentials")
			return
		}
		sshConfigured := s.cfg.SSHPolicyHelperFile != ""
		injectSelf := !foreign && (parentState.VMTokens[parent] == nil || !parentState.VMTokens[parent].NoSelfAPI) && p.has("exec") && p.has("vms:read")
		base := s.vmAPIURL()
		needChildSelf := !(base == "" && !s.sshPolicyConfigured() && injectSelf && !foreign)
		var (
			parentSelfGen   int
			parentRotatedAI bool
			childSelfGen    int
			childNoSelf     = !injectSelf
			childAIGen      int
			errStage        string
		)
		mintedByParent := func(keyID string) bool { return strings.HasPrefix(keyID, "vm_"+parent+"@") }
		mintedByChild := func(keyID string) bool { return strings.HasPrefix(keyID, "vm_"+childID+"@") }
		storeErr := s.store.UpdateOrApply(func(cur *State) (bool, error) {
			if _, err := requirePrincipalIn(cur, p, "vms:write", time.Now()); err != nil {
				return false, err
			}
			if cur.Teams[p.Team.ID] == nil {
				errStage = "record"
				return false, errors.New("VM team no longer exists")
			}
			if tok := cur.VMTokens[childID]; tok != nil && (tok.Retired || tok.TeamID != p.Team.ID) {
				errStage = "record"
				return false, errors.New("VM credentials are retired")
			}
			if ai := cur.AIVMs[childID]; ai != nil && (ai.Retired || ai.TeamID != p.Team.ID) {
				errStage = "record"
				return false, errors.New("VM credentials are retired")
			}
			// A no_env child overrides any selection
			// (recorded in the same write as its mark): its records need no fsync of their own.
			syncWrite := foreign || (sel != nil && !labeled)
			if sel != nil {
				cur.VMSecrets[childID] = append([]string{}, sel...)
			}
			if childRunKnown {
				setRunNames(cur, childID, childRunNames, false)
			}
			if foreign {
				cur.ForeignVMs[childID] = true
				if t := cur.cowVMToken(childID); t != nil {
					t.RestrictSelf = true
				}
				av := cur.cowAIVM(childID)
				if av == nil {
					av = &AIVM{TeamID: p.Team.ID}
					cur.AIVMs[childID] = av
				}
				av.Blocked = true
			}
			pt := cur.VMTokens[parent]
			if pt != nil && !pt.Retired && (!pt.NoSelfAPI || sshConfigured) {
				if cur.Teams[pt.TeamID] == nil || (sshConfigured && pt.TeamID != p.Team.ID) {
					errStage = "revoke"
					return false, fmt.Errorf("rotate VM %s token: VM team no longer exists", parent)
				}
				pt = cur.cowVMToken(parent)
				if cur.ForeignVMs[parent] {
					pt.RestrictSelf = true
				}
				pt.bumpGeneration()
				if !pt.NoSelfAPI {
					parentSelfGen = pt.Gen
				}
				revokeLinksOf(cur, mintedByParent)
				syncWrite = true
			} else if sshConfigured {
				if pt != nil && (pt.Retired || pt.TeamID != p.Team.ID) {
					errStage = "revoke"
					return false, fmt.Errorf("VM credentials are retired")
				}
				if pt == nil {
					pt = &VMToken{TeamID: p.Team.ID, Gen: 1, NoSelfAPI: cur.ForeignVMs[parent], RestrictSelf: cur.ForeignVMs[parent]}
					cur.VMTokens[parent] = pt
					syncWrite = true
				}
			}
			if pai := cur.AIVMs[parent]; pai != nil {
				if pai.Retired || pai.TeamID != p.Team.ID {
					errStage = "revoke"
					return false, fmt.Errorf("VM credentials are retired")
				}
				cur.cowAIVM(parent).Gen++
				parentRotatedAI = true
				syncWrite = true
			}
			if envID != "" {
				cur.VMEnvs[childID] = envID
				if childPin != nil {
					cur.VMEnvPins[childID] = &EnvPin{Version: childPin.Version, Vars: maps.Clone(childPin.Vars)}
				}
			}
			if displayChild != "" {
				cur.VMNames[childID] = displayChild
			}
			if len(childExtra) > 0 && !noEnv {
				cur.VMExtraEnv[childID] = maps.Clone(childExtra)
			}
			markChild := noEnv
			if needChildSelf {
				ct := cur.cowVMToken(childID)
				hasLinks := ct != nil && !ct.NoSelfAPI && hasLinksOf(cur, mintedByChild)
				if hasLinks {
					syncWrite = true
				}
				if ct == nil {
					ct = &VMToken{TeamID: p.Team.ID, NoSelfAPI: !injectSelf}
					cur.VMTokens[childID] = ct
				}
				if cur.ForeignVMs[childID] || foreign || labels[restrictedLabel] == "1" {
					ct.RestrictSelf = true
				}
				ct.bumpGeneration()
				childSelfGen = ct.Gen
				childNoSelf = ct.NoSelfAPI
				if hasLinks {
					revokeLinksOf(cur, mintedByChild)
				}
			}
			if needChildAI {
				cai := cur.cowAIVM(childID)
				if cai == nil {
					cai = &AIVM{TeamID: p.Team.ID, Blocked: cur.ForeignVMs[childID] || foreign}
					cur.AIVMs[childID] = cai
				}
				cai.Gen++
				childAIGen = cai.Gen
			}
			if markChild {
				// A no_env child is recorded with the rest of the
				// child's records (as durable as its secret selection: nothing here
				// waits for an fsync of its own).
				cur.NoEnvRuns[childID] = time.Now().UTC()
				markTokenNoEnv(cur, childID)
			}
			return syncWrite, nil
		})
		if storeErr != nil {
			if errStage == "revoke" {
				log.Printf("%s: revoke copied credentials: %v", info(r).id, storeErr)
				if cleanup := s.discardCopyVM(childID); cleanup != nil {
					writeErr(w, r, 500, "cleanup_failed", "credential revocation failed; the pending VM "+childID+" could not be deleted")
					return
				}
				writeErr(w, r, 500, "store_error", "could not persist copied credential revocation")
				return
			}
			log.Printf("%s: record secret selection for fork %s: %v; deleting it", info(r).id, childID, storeErr)
			if cleanup := s.discardCopyVM(childID); cleanup != nil {
				log.Printf("%s: delete unsafe fork %s: %v", info(r).id, childID, cleanup)
				writeErr(w, r, 500, "cleanup_failed", "could not record the secret selection or delete the new VM "+childID)
				return
			}
			writeErr(w, r, 500, "store_error", "could not record the secret selection")
			return
		}
		t.mark("record")
		// Refresh live parent before releasing child.
		var refreshErr error
		if sshConfigured {
			if parentRotatedAI || parentSelfGen > 0 {
				refreshErr = s.refreshCredentials(r.Context(), parent)
			} else {
				refreshErr = s.refreshSSHPolicyOnly(r.Context(), parent)
			}
		} else if parentRotatedAI {
			if rerr := s.refreshCredentials(r.Context(), parent); rerr != nil {
				log.Printf("vm %s: refresh rotated AI credentials: %v", parent, rerr)
			}
		} else if parentSelfGen > 0 {
			go s.pushSelf(s.baseCtx(), parent, parentSelfGen)
		}
		if refreshErr != nil {
			log.Printf("%s: revoke copied credentials: %v", info(r).id, refreshErr)
			if cleanup := s.discardCopyVM(childID); cleanup != nil {
				writeErr(w, r, 500, "cleanup_failed", "credential revocation failed; the pending VM "+childID+" could not be deleted")
				return
			}
			writeErr(w, r, 500, "store_error", "could not persist copied credential revocation")
			return
		}
		vars["BOAT_ID"] = childID
		if childSelfGen > 0 && base != "" && !childNoSelf && vars["BOAT_API_KEY"] == "" {
			vars["BOAT_URL"], vars["BOAT_API_KEY"] = base, s.selfToken(childID, childSelfGen)
		}
		if base != "" && vars["BOAT_API_KEY"] == "" && !childNoSelf {
			if cleanup := s.discardCopyVM(childID); cleanup != nil {
				writeErr(w, r, 500, "cleanup_failed", "credential preparation failed; the pending VM "+childID+" could not be deleted")
				return
			}
			writeErr(w, r, 500, "store_error", "cannot prepare the child credentials")
			return
		}
		if len(rawAI) > 0 {
			for k, v := range rawAI {
				vars[k] = v
			}
		} else if childAIGen > 0 {
			aiBase := strings.TrimRight(s.cfg.AIProxyURL, "/")
			tok := s.aiToken(childID, childAIGen)
			for _, c := range creds {
				pr := aiProviders[c.Provider]
				vars[pr.EnvKey] = tok
				vars[pr.EnvBase] = aiBase + "/" + c.Provider + pr.BasePath
			}
		}
		if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
			if cleanup := s.discardCopyVM(childID); cleanup != nil {
				log.Printf("%s: delete revoked-caller fork %s: %v", info(r).id, childID, cleanup)
			}
			return
		}
		// Sticky policy is already recorded, independently of RAM eligibility.
		childPrev := s.readRunMark(childID)
		if parentNever {
			if err := s.store.Update(func(st *State) error {
				st.VMSecrets[envNeverKey(childID)] = []string{"env_never_landed"}
				return nil
			}); err != nil {
				log.Printf("%s: mark %s as env never landed: %v; deleting it", info(r).id, childID, err)
				if cleanup := s.discardCopyVM(childID); cleanup != nil {
					writeErr(w, r, 500, "cleanup_failed", "could not record the inherited env mark or delete the new VM "+childID)
					return
				}
				writeErr(w, r, 500, "store_error", "could not record the inherited env mark")
				return
			}
		}
		// Serialize child release + SSH provisioning against SSE reconcileRunningVM.
		unlockChild := startLocks.lock(childID)
		defer unlockChild()
		release := map[string]any{"env": vars}
		if freshBoot {
			release["fresh_boot"] = true
		}
		// Decode into a fresh map: omitted omitempty fields must not survive prepare.
		var released VM
		if err := s.boatd.call(r.Context(), "POST", vmPath(childID, "release"), release, &released); err != nil {
			if freshBoot {
				s.failClosedFork(w, r, childID, childPrev, nil, fmt.Sprintf("the child could not be released: %v", err), releaseChild)
				return
			}
			if cleanup := s.discardCopyVM(childID); cleanup != nil {
				log.Printf("%s: delete unreleased fork %s after release failure (%v): %v", info(r).id, childID, err, cleanup)
			}
			writeUpstream(w, r, err)
			return
		}
		child = released
		// Like create: a release whose set_env failed leaves the child running with
		// agent_ready=false and an error; persist it so reads keep reporting it.
		ready, hasReady := child["agent_ready"].(bool)
		envFailed := (hasReady && !ready) || str(child, "error") != ""
		var failChild VM // the fail-closed stop below: sandbox.error from this VM, before it
		if envFailed && freshBoot {
			failChild = child
		}
		s.recordEnvResultVM(childID, boolErr(envFailed), failChild)
		if envFailed {
			log.Printf("%s: release of %s did not apply the env: agent_ready=%v error=%q", info(r).id, childID, ready, str(child, "error"))
			if freshBoot {
				s.failClosedFork(w, r, childID, childPrev, child, "the child's environment could not be set", releaseChild)
				return
			}
		}
		if parentNever && !envFailed {
			s.clearEnvNeverLanded(childID) // one push landed: the copied env file is gone
		}
		s.provisionSSHPolicyLoggedWithVM(r.Context(), childID, p.Team.ID, child)
		t.mark("env")
		s.copyConversations(parent, childID)
		t.mark("conversations")
		info(r).idemVMs = []string{childID}
		releaseChild() // settled: the answer reports env_pending=false
		writeJSON(w, 201, s.publicVMState(child))
	}
	if strings.HasSuffix(r.URL.Path, "/fork") {
		s.idempotentScoped(w, r, p, "v1:POST /v1/vms/"+r.PathValue("id")+"/fork", authorize, execute)
		return
	}
	if authorize() {
		execute(w)
	}
}

// forkNoEnv: whether a fork's child is released with no_env. An explicit no_env
// decides; absent, the child follows the parent: a no_env parent (sticky, running
// or not) forks a no_env child, so a fork never escapes it. (An explicit false or
// an environment on a no_env parent is refused before this, 400 no_env_sticky.)
//
// It also reports whether the parent is itself no_env: a no_env child of any other
// parent must not resume the parent's memory (fresh_boot).
func (s *server) forkNoEnv(explicit *bool, parent string, pvm VM) (noEnv, parentNoEnv bool) {
	parentNoEnv = protectedRunPolicy(s.store.Snapshot(), parent, pvm)
	if parentNoEnv {
		return true, true
	}
	if explicit != nil {
		return *explicit, false
	}
	return false, false
}

// failClosedFork settles a fresh-boot no_env child whose release errored or did
// not apply its env. The child is force-stopped (no memory snapshot) and the call
// answers 502 env_start_failed with the child (state stopped); the child is kept,
// stopped, for the caller to inspect or delete. A child that cannot be stopped is
// deleted instead; one that can be neither stopped nor deleted keeps its no_env
// mark and the answer is 502 cleanup_failed. A stopped child gives its mark back
// (none, as released), so a later no_env start of it boots fresh.
//
// releaseChild gives the child's env-push gate back once it settled and before the
// answer is written, so the answer reports env_pending=false.
func (s *server) failClosedFork(w http.ResponseWriter, r *http.Request, childID string, prev runMark, child VM, why string, releaseChild func()) {
	log.Printf("%s: fresh-boot release of %s failed: %s; stopping it", info(r).id, childID, why)
	markErr := s.markEnvNeverLanded(childID)
	if child == nil {
		// The release errored: it may have booted the child before the answer was lost.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), failClosedTimeout)
		var cur VM
		if err := s.boatd.call(ctx, "GET", vmPath(childID), nil, &cur); err == nil {
			child = cur
		}
		cancel()
	}
	if child == nil {
		child = VM{"id": childID}
	}
	stopped := s.stopFailClosed(r, childID, prev)
	if !stopped || markErr != nil {
		if cleanup := s.discardCopyVM(childID); cleanup != nil {
			writeErr(w, r, 500, "cleanup_failed", "the child could not be released, stopped or deleted: "+childID)
			return
		}
		writeErr(w, r, http.StatusBadGateway, "env_start_failed", "the child could not be started and was deleted")
		return
	}
	releaseChild()
	writeEnvStartFailed(w, r, s.publicVMState(s.postStopVM(r, childID, child)), "the child could not be started with a clean environment; it was stopped")
}

// closedKind says how a fresh-boot create settled a VM whose env did not land.
type closedKind int

const (
	closedNone    closedKind = iota // the env landed (or no fresh boot): nothing to settle
	closedStopped                   // force-stopped (no memory snapshot); kept
	closedDeleted                   // could not be stopped; deleted
	closedStuck                     // could be neither stopped nor deleted
)

type closedCreate struct {
	kind closedKind
	vm   VM // the VM as it is now (stopped), or as created when deleted or stuck
}

// failClosedCreate settles a VM created from a snapshot with a fresh boot whose
// environment push failed: its disk still holds the snapshot's /etc/boat/env
// (the source run's secrets), so it is not left running. It is force-stopped
// (kept, state stopped, for the caller to inspect), or deleted when it cannot be
// stopped. vm is the VM as boatd created it.
func (s *server) failClosedCreate(r *http.Request, id string, vm VM) closedCreate {
	log.Printf("%s: fresh-boot create of %s did not apply the env; stopping it", info(r).id, id)
	markErr := s.markEnvNeverLanded(id) // the disk still holds the snapshot's env: starts fail closed until one push lands
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), failClosedTimeout)
	err := s.boatd.call(ctx, "POST", vmPath(id, "stop"), map[string]any{"force": true}, nil)
	cancel()
	if err == nil {
		s.closeEnvErrorSpan(id, time.Now())
		if markErr == nil {
			return closedCreate{closedStopped, s.postStopVM(r, id, vm)}
		}
		log.Printf("%s: fail-closed mark of %s did not persist (%v); deleting it", info(r).id, id, markErr)
	} else {
		log.Printf("%s: fail-closed stop of %s: %v; deleting it", info(r).id, id, err)
	}
	if cleanup := s.discardCopyVM(id); cleanup != nil {
		log.Printf("%s: delete %s: %v", info(r).id, id, cleanup)
		return closedCreate{closedStuck, vm}
	}
	return closedCreate{closedDeleted, vm}
}

// writeCreateFailClosed answers a create in which some fresh-boot VM failed
// closed (reports whether it did): 502 env_start_failed with the VM (batch: all
// of them in "vms", the failed ones stopped); a VM that could be neither stopped
// nor deleted makes it 500 cleanup_failed.
func (s *server) writeCreateFailClosed(w http.ResponseWriter, r *http.Request, closed []closedCreate, outs []VM) bool {
	worst := closedNone
	for _, c := range closed {
		worst = max(worst, c.kind)
	}
	if worst == closedNone {
		return false
	}
	if worst == closedStuck {
		var ids []string
		for _, c := range closed {
			if c.kind == closedStuck {
				ids = append(ids, str(c.vm, "id"))
			}
		}
		writeErr(w, r, 500, "cleanup_failed", "the environment could not be set and the VM could not be stopped or deleted: "+strings.Join(ids, ","))
		return true
	}
	msg := "the VM's environment could not be set; the VM was stopped"
	if worst == closedDeleted {
		msg = "the VM's environment could not be set and the VM could not be stopped; it was deleted"
	}
	list := make([]VM, 0, len(outs))
	var first VM // the first VM that was stopped: "vm"
	for i, c := range closed {
		switch c.kind {
		case closedStopped:
			pub := s.publicVMState(c.vm)
			list = append(list, pub)
			if first == nil {
				first = pub
			}
		case closedNone:
			if outs[i] != nil {
				list = append(list, outs[i])
			}
		}
	}
	body := map[string]any{"error": map[string]string{"code": "env_start_failed", "message": msg, "request_id": info(r).id}}
	if len(outs) > 1 {
		body["vms"] = list
	}
	if first != nil {
		body["vm"] = first
	}
	writeJSON(w, http.StatusBadGateway, body)
	return true
}

// discardCopyVM cleans up a new copy even if its originating request was canceled.
func (s *server) discardCopyVM(id string) error {
	if err := s.retireVM(id); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.baseCtx(), 30*time.Second)
	defer cancel()
	if err := s.boatd.call(ctx, "POST", vmPath(id, "delete"), nil, nil); err != nil {
		var be *boatdError
		if !errors.As(err, &be) || be.Status != http.StatusNotFound {
			return err
		}
	}
	s.forgetVM(id)
	return nil
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

// forgetVM drops boat-api state tied to a deleted VM. It is cleanup, not an
// authorization barrier: the credentials were retired durably before the delete
// (retireVM, or reconcileCopyCredentials at startup), so the change is applied
// in memory at once and written by the background flusher (Store.Apply). A
// crash before that write leaves only dead rows; the vm.deleted webhook and
// startup reconcile cover them.
func (s *server) forgetVM(id string) {
	s.clearAttested(id)
	eventTexts.forget(s, id)
	err := s.store.Apply(func(st *State) error {
		retireCredentials(st, id)
		foldDeletedVMAIUsage(st, id) // keep the team's AI totals, drop the per-VM rows
		delete(st.VMSecrets, id)
		delete(st.VMSecrets, envNeverKey(id))
		delete(st.VMSecrets, runNamesKey(id))
		forgetSnapshotSourcesOf(st, id) // boatd deletes the VM's snapshots with it
		delete(st.Commands, id)
		delete(st.Prompts, id)
		delete(st.Conversations, id)
		delete(st.VMEnvs, id)
		delete(st.VMEnvPins, id)
		delete(st.VMTransitions, id)
		delete(st.VMNames, id)
		delete(st.VMExtraEnv, id)
		delete(st.VMEnvErrors, id)
		setEnvErrorSpan(st, id, false, time.Now()) // closed, not dropped: the VM's usage still excludes it
		delete(st.EnvErrorCutoffs, id)
		delete(st.NoEnvRuns, id)
		delete(st.ForeignVMs, id)
		delete(st.RouteGens, id)
		delete(st.PortMinters, id)
		delete(st.PortMinterGens, id)
		delete(st.DesktopVMs, id)
		forgetStops(st, id)
		forgetCompatID(st, id)
		delete(st.CommandSeq, id)
		delete(st.CompatSetups, id)
		for k, sh := range st.SnapShares {
			if sh.VMID == id {
				delete(st.SnapShares, k)
			}
		}
		forgetIdemByVM(st, id)
		dropVMFromGrants(st, id)
		return nil
	})
	if err != nil {
		log.Printf("forget vm %s: %v", id, err)
	}
	s.clearSSHDeferredVM(id)
	s.store.credentialPushes.Delete(id) // its token records are gone, so is the push lock
}

// vmAction proxies a lifecycle POST and reports whether it succeeded.
func (s *server) vmAction(w http.ResponseWriter, r *http.Request, action string, body any) bool {
	var vm VM
	if err := s.boatd.call(r.Context(), "POST", vmPath(r.PathValue("id"), action), body, &vm); err != nil {
		writeUpstream(w, r, err)
		return false
	}
	if action == "start" && vm != nil {
		s.provisionSSHPolicyLoggedWithVM(r.Context(), str(vm, "id"), str(vm, "team"), vm)
	}
	if vm == nil {
		writeJSON(w, 200, map[string]any{"ok": true})
		return true
	}
	writeJSON(w, 200, s.withDisplay(vm))
	return true
}

// ---- exec & prompt ----

type execReq struct {
	Cmd      []string          `json:"cmd"`
	Env      map[string]string `json:"env,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
	StdinB64 string            `json:"stdin_b64,omitempty"`
	TimeoutS int               `json:"timeout_s,omitempty"`
	TTY      bool              `json:"tty,omitempty"`
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
	if !validCwd(e.Cwd) {
		writeErr(w, r, 400, "invalid_request", "invalid cwd")
		return
	}
	for k := range e.Env {
		if !reEnvName.MatchString(k) {
			writeErr(w, r, 400, "invalid_request", "invalid env name "+k)
			return
		}
	}
	if _, ok := s.ownVM(w, r, p); ok {
		if s.store.Snapshot().VMTransitions[r.PathValue("id")] != nil {
			writeErr(w, r, 409, "sandbox_not_ready", "a credential transition is pending; resume the VM to retry")
			return
		}
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
	// The four below follow each CLI's --help; none has been run by CI. Their
	// output is passed through as is.
	"pi": func(prompt, model string) []string {
		return append(withModel([]string{"pi", "-p", "--mode", "json"}, "--model", model), "--", prompt)
	},
	"prime": func(prompt, model string) []string {
		return append(withModel([]string{"prime-agent", "-p", "--mode", "json"}, "--model", model), positional(prompt))
	},
	"kimi": func(prompt, model string) []string {
		return withModel([]string{"kimi", "-p", prompt, "--yolo", "--output-format", "stream-json"}, "-m", model)
	},
	"mistral": func(prompt, model string) []string {
		return withModel([]string{"vibe", "-p", prompt, "--auto-approve", "--output", "streaming"}, "--model", model)
	},
}

// agentAliases: other names callers use for an agent.
var agentAliases = map[string]string{"claude-code": "claude", "prime-agent": "prime", "kimi-code": "kimi"}

// agentNames is the list for error messages.
const agentNames = "claude, codex, opencode, pi, prime, kimi or mistral"

// canonicalAgent resolves an alias; "" stays "" (the default applies later).
func canonicalAgent(name string) string {
	if a, ok := agentAliases[name]; ok {
		return a
	}
	return name
}

func withModel(c []string, flag, model string) []string {
	if model != "" {
		return append(c, flag, model)
	}
	return c
}

// positional keeps a prompt that starts with "-" from being read as a flag by
// a CLI whose prompt is positional with no "--" separator documented.
func positional(prompt string) string {
	if strings.HasPrefix(prompt, "-") {
		return " " + prompt
	}
	return prompt
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
	body.Agent = canonicalAgent(body.Agent)
	if body.Agent == "" {
		body.Agent = "claude"
	}
	build, ok := agents[body.Agent]
	switch {
	case !ok:
		writeErr(w, r, 400, "invalid_request", "agent must be "+agentNames)
		return
	case strings.TrimSpace(body.Prompt) == "":
		writeErr(w, r, 400, "invalid_request", "prompt is required")
		return
	case body.Model != "" && !reModel.MatchString(body.Model):
		writeErr(w, r, 400, "invalid_request", "invalid model")
		return
	case !validCwd(body.Cwd):
		writeErr(w, r, 400, "invalid_request", "invalid cwd")
		return
	}
	vm, ok := s.ownVM(w, r, p)
	if !ok || s.refuseNotReady(w, r, vm) {
		return // like /prompts: no agent turn before the environment is applied
	}
	sel := s.runSecretsOf(r.PathValue("id"), vm)
	env, ok := s.envOrFail(w, r, p, sel)
	if !ok {
		return
	}
	ai, err := s.aiEnvRun(r.PathValue("id"), p.Team.ID, false, s.noEnvRun(r.PathValue("id")))
	if err != nil {
		log.Printf("%s: AI env: %v", info(r).id, err)
		writeErr(w, r, 500, "secrets_unavailable", "cannot prepare AI credentials")
		return
	}
	if s.secretsRaced(r.PathValue("id"), env) { // before the AI tokens join: they are not team secrets
		writeErr(w, r, http.StatusConflict, "sandbox_not_ready", msgNotReady)
		return
	}
	id := r.PathValue("id")
	unlock := startLocks.rlock(id)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(unlock) }
	defer release()
	if s.envPushPending(id) || s.store.Snapshot().VMTransitions[id] != nil || (len(env) > 0 && s.noEnvRun(id)) || !slices.Equal(sel, s.runSecretsOf(id, vm)) {
		writeErr(w, r, http.StatusConflict, "sandbox_not_ready", msgNotReady)
		return
	}
	if err := s.recordPossibleRunNames(p, "exec", id, envNames(env)); err != nil {
		s.storeErr(w, r, err)
		return
	}
	for k, v := range ai { // the proxy token replaces a raw key of the same name
		env[k] = v
	}
	s.streamExecDispatched(w, r, execReq{Cmd: build(body.Prompt, body.Model), Env: env, Cwd: body.Cwd, TimeoutS: body.TimeoutS}, release)
}

// streamExec relays boatd's JSON-lines exec stream, flushing per chunk.
func (s *server) streamExec(w http.ResponseWriter, r *http.Request, e execReq) {
	s.streamExecDispatched(w, r, e, func() {})
}

func (s *server) streamExecDispatched(w http.ResponseWriter, r *http.Request, e execReq, dispatched func()) {
	b, _ := json.Marshal(e)
	resp, err := s.boatd.do(r.Context(), "POST", vmPath(r.PathValue("id"), "exec"), bytes.NewReader(b), "application/json")
	dispatched()
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
	if s.revalidatePrincipal(w, r, p, "exec") == nil {
		return
	}
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
	rd, done := s.uploadBodyReader(w, r.Body)
	defer done()
	n, err := io.Copy(f, http.MaxBytesReader(w, rd, s.cfg.MaxFileBytes))
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		f.Close()
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			writeErr(w, r, http.StatusRequestEntityTooLarge, "too_large", "file exceeds limit")
		case isUploadTimeout(err):
			writeErr(w, r, http.StatusRequestTimeout, "request_timeout", "upload stalled or timed out")
		default:
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
	name := ""
	if r.Method == "POST" {
		var body struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &body) {
			return
		}
		name = body.Name
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	if r.Method != "GET" && s.revalidatePrincipal(w, r, p, "vms:write") == nil {
		return
	}
	var out any
	if r.Method == "POST" {
		snap, err := s.captureSnapshot(r.Context(), r.PathValue("id"), p.Team.ID, name)
		if err != nil {
			switch {
			case errors.Is(err, errCopyCleanup):
				writeErr(w, r, 500, "cleanup_failed", err.Error())
			case errors.Is(err, errCopyCredentials):
				writeErr(w, r, 500, "store_error", "could not persist copied credential revocation")
			default:
				writeUpstream(w, r, err)
			}
			return
		}
		writeJSON(w, 201, snap)
		return
	}
	if err := s.boatd.call(r.Context(), r.Method, path, in, &out); err != nil {
		writeUpstream(w, r, err)
		return
	}
	if r.Method == "DELETE" {
		s.forgetSnapshotSource(r.PathValue("sid"))
	}
	if out == nil {
		out = map[string]any{"ok": true}
	}
	writeJSON(w, 200, out)
}

var (
	errCopyCredentials = errors.New("copied credentials could not be revoked")
	errCopyCleanup     = errors.New("pending snapshot could not be deleted")
)

func (s *server) revokeCopiedCredentials(ctx context.Context, id, team string) error {
	sshConfigured := s.cfg.SSHPolicyHelperFile != ""
	cur := s.store.Snapshot()
	curSelf := cur.VMTokens[id]
	curAI := cur.AIVMs[id]
	needStore := (curSelf != nil && !curSelf.Retired && (!curSelf.NoSelfAPI || sshConfigured)) ||
		(sshConfigured && curSelf == nil) ||
		curAI != nil ||
		(sshConfigured && curSelf != nil && (curSelf.Retired || curSelf.TeamID != team))
	selfGen := 0
	rotatedAI := false
	if needStore {
		mintedBySelf := func(keyID string) bool { return strings.HasPrefix(keyID, "vm_"+id+"@") }
		err := s.store.Update(func(st *State) error {
			t := st.VMTokens[id]
			if t != nil && !t.Retired && (!t.NoSelfAPI || sshConfigured) {
				if st.Teams[t.TeamID] == nil || (sshConfigured && t.TeamID != team) {
					return fmt.Errorf("rotate VM %s token: VM team no longer exists", id)
				}
				t = st.cowVMToken(id)
				if st.ForeignVMs[id] {
					t.RestrictSelf = true
				}
				t.bumpGeneration()
				if !t.NoSelfAPI {
					selfGen = t.Gen
				}
				revokeLinksOf(st, mintedBySelf)
			} else if sshConfigured {
				if st.Teams[team] == nil {
					return fmt.Errorf("VM team no longer exists")
				}
				if t != nil && (t.Retired || t.TeamID != team) {
					return fmt.Errorf("VM credentials are retired")
				}
				if t == nil {
					t = &VMToken{TeamID: team, Gen: 1, NoSelfAPI: st.ForeignVMs[id], RestrictSelf: st.ForeignVMs[id]}
					st.VMTokens[id] = t
				}
			}
			if ai := st.AIVMs[id]; ai != nil {
				if st.Teams[team] == nil {
					return fmt.Errorf("VM team no longer exists")
				}
				if ai.Retired || ai.TeamID != team {
					return fmt.Errorf("VM credentials are retired")
				}
				st.cowAIVM(id).Gen++
				rotatedAI = true
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if sshConfigured {
		if rotatedAI || selfGen > 0 {
			return s.refreshCredentials(ctx, id)
		}
		return s.refreshSSHPolicyOnly(ctx, id)
	}
	if rotatedAI {
		if err := s.refreshCredentials(ctx, id); err != nil {
			log.Printf("vm %s: refresh rotated AI credentials: %v", id, err)
		}
		return nil
	}
	if selfGen > 0 {
		go s.pushSelf(s.baseCtx(), id, selfGen)
	}
	return nil
}

// captureSnapshot takes a snapshot of VM id and records the secret selection of
// the run it holds. The VM's start lock is held shared from the prepare to the
// record: a start of it (exclusive) cannot change the run, or its no_env mark,
// between the two. It is released before the credential revoke (a synchronous
// store write plus an SSH refresh). Sticky policy is persisted under the shared
// lock; ordinary source bookkeeping remains asynchronous. The revoke is also
// durable before the snapshot is released, as in fork.
// Snapshots of one VM do not exclude each other.
func (s *server) captureSnapshot(ctx context.Context, id, team, name string) (VM, error) {
	var snap VM
	path := vmPath(id, "snapshots")
	prep := map[string]any{"name": name}
	if s.cfg.SSHPolicyHelperFile != "" {
		prep["reprovision_ssh"] = true
	}
	sid, err := s.prepareAndRecordSnapshot(ctx, id, team, path, prep, &snap)
	if err != nil {
		return nil, err
	}
	if err := s.revokeCopiedCredentials(ctx, id, team); err != nil {
		cleanupCtx, cancel := context.WithTimeout(s.baseCtx(), 30*time.Second)
		defer cancel()
		if sid == "" {
			return nil, errCopyCleanup
		}
		if cleanup := s.boatd.call(cleanupCtx, "DELETE", path+"/"+url.PathEscape(sid), nil, nil); cleanup != nil {
			return nil, fmt.Errorf("%w: %s", errCopyCleanup, sid)
		}
		s.forgetSnapshotSource(sid)
		return nil, fmt.Errorf("%w: %v", errCopyCredentials, err)
	}
	// Release is a complete response, not a patch of the prepared snapshot.
	var released VM
	if err := s.boatd.call(ctx, "POST", path+"/"+url.PathEscape(sid)+"/release", nil, &released); err != nil {
		return nil, err
	}
	return released, nil
}

// prepareAndRecordSnapshot is captureSnapshot's part under the VM's shared start
// lock: boatd's prepare, then the record of the secrets the run holds (the
// snapshot holds this run's memory and /etc/boat/env: remember which secrets it
// was taken under, before anyone can create from it). It returns the snapshot's id.
func (s *server) prepareAndRecordSnapshot(ctx context.Context, id, team, path string, prep map[string]any, snap *VM) (string, error) {
	unlockStart := startLocks.rlock(id)
	defer unlockStart()
	if err := s.boatd.call(ctx, "POST", path+"/prepare", prep, snap); err != nil {
		return "", err
	}
	if snapshotPostPrepareHook != nil {
		snapshotPostPrepareHook(s, id)
	}
	sid := str(*snap, "id")
	if err := s.recordSnapshotSource(ctx, id, team, sid); err != nil {
		cleanupCtx, cancel := context.WithTimeout(s.baseCtx(), 30*time.Second)
		defer cancel()
		if sid == "" || s.boatd.call(cleanupCtx, "DELETE", path+"/"+url.PathEscape(sid), nil, nil) != nil {
			return "", errCopyCleanup
		}
		return "", fmt.Errorf("%w: snapshot policy: %v", errCopyCredentials, err)
	}
	return sid, nil
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
	if pub != "" && p.VMID != "" {
		st := s.store.Snapshot()
		tok := st.VMTokens[p.VMID]
		if !p.Team.SelfToken.SSH || st.ForeignVMs[p.VMID] || tok == nil || tok.Retired || tok.RestrictSelf {
			writeErr(w, r, 403, "forbidden", "a VM's own token cannot add ssh keys; use a team key, or a team admin can allow it (PUT /v1/teams/{id}/self-token)")
			return
		}
	}
	vm, ok := s.ownVM(w, r, p)
	if !ok {
		return
	}
	if pub != "" {
		if !s.sshServerConfigured() {
			writeErr(w, r, http.StatusNotImplemented, "not_configured", "ssh key authorization is not configured on this server")
			return
		}
		if err := s.grantJump(r, p, r.PathValue("id"), pub); err != nil {
			if errors.Is(err, errSSHAuthority) {
				writeErr(w, r, 403, "forbidden", "SSH authority is no longer valid")
				return
			}
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
	if err := s.boatd.call(r.Context(), "POST", vmPath(id, "desktop"), map[string]string{"action": "start", "token": SignGuestDesktop(PurposeKey(s.cfg.DesktopKey, purposeGuestDesktop), id)}, nil); err != nil {
		writeUpstream(w, r, err)
		return
	}
	// Like a private port link: revoked with its key or member (revokeLinksOf),
	// and never outliving the key.
	gen, exp, err := s.recordPortMinter(id, p, true, s.cfg.DesktopTTL)
	if err != nil {
		if errors.Is(err, errLinkUnauthorized) {
			writeErr(w, r, 403, "forbidden", err.Error())
		} else {
			s.storeErr(w, r, err)
		}
		return
	}
	sig := SignDesktop(PurposeKey(s.cfg.DesktopKey, purposeDesktopLink), id, gen, exp)
	u := fmt.Sprintf("%s/%s?exp=%d&sig=%s", strings.TrimRight(s.cfg.DesktopURL, "/"), url.PathEscape(id), exp, sig)
	writeJSON(w, 200, map[string]any{"url": u, "expires_at": time.Unix(exp, 0).UTC()})
}
