package main

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A VM's own API token lets `boat self …` and `host …` work inside the guest
// without a team key. It is boatself_<gen>_<hex(HMAC(vmKey, "copy-v1|self|<vmid>|<gen>"))>_<vmid>,
// valid while the VM's generation is <gen>, and it reaches only routes under
// /v1/vms/<that id>/ (vms:read, vms:write, exec; never admin). Everything else
// answers 404, as for another team's VM.
//
// The generation moves on at create, fork and start, so a token copied from an
// earlier run is dead, and on the source VM whenever its disk is copied (fork,
// snapshot, share): a child or a snapshot holds the parent's token, and that
// token must not reach the parent. The running source VM gets its new token
// pushed to /etc/boat/env; processes that read the old one keep a dead token.
//
// Generations are persisted before publishing them: a disk copy must never
// hold a token that becomes valid again after an API crash.

type VMToken struct {
	TeamID       string `json:"team_id"`
	Gen          int    `json:"gen"`
	Retired      bool   `json:"retired,omitempty"`
	NoSelfAPI    bool   `json:"no_self_api,omitempty"`
	RestrictSelf bool   `json:"restrict_self,omitempty"`
	// NoEnvGen is the generation minted for a no_env run (0: none). The token of
	// that generation stays refused on the snapshot and history routes
	// (r43_selfgate.go) after the run is stopped, until a new generation exists.
	NoEnvGen int `json:"no_env_gen,omitempty"`
	// Narrowed runs use the same history gate without becoming sticky no_env.
	NarrowGen int       `json:"narrow_gen,omitempty"`
	NarrowAt  time.Time `json:"narrow_at,omitempty"`
}

func (t *VMToken) bumpGeneration() {
	narrow := t.NarrowGen != 0 && t.NarrowGen == t.Gen
	t.Gen++
	if narrow {
		t.NarrowGen = t.Gen // rotating copied credentials must not release the gate
	}
}

const selfTokenPrefix = "boatself_"

var (
	reSelfToken = regexp.MustCompile(`^boatself_([0-9]{1,9})_([0-9a-f]{64})_([A-Za-z0-9_.-]{1,64})$`)
	selfScopes  = []string{"vms:read", "vms:write", "exec"}
)

func (s *server) selfToken(vmID string, gen int) string {
	message := fmt.Sprintf("self|%s|%d", vmID, gen)
	if s.store.Snapshot().CopyCredentialsV1 {
		message = "copy-v1|" + message
	}
	return fmt.Sprintf("%s%d_%s_%s", selfTokenPrefix, gen, signHex(s.keys.vmKey, message), vmID)
}

// authenticateSelf resolves a VM token to a principal limited to that VM.
func (s *server) authenticateSelf(tok string) *principal {
	m := reSelfToken.FindStringSubmatch(tok)
	if m == nil {
		return nil
	}
	gen, err := strconv.Atoi(m[1])
	if err != nil {
		return nil
	}
	vmID := m[3]
	st := s.store.Snapshot()
	t := st.VMTokens[vmID]
	if t == nil || t.Retired || t.NoSelfAPI || t.Gen != gen || !hmac.Equal([]byte(s.selfToken(vmID, gen)), []byte(tok)) {
		return nil
	}
	team := st.Teams[t.TeamID]
	if team == nil {
		return nil
	}
	id := "vm_" + vmID
	keyID := selfKeyID(vmID, gen)
	if st.CopyCredentialsV1 {
		keyID = fmt.Sprintf("vm_%s@copy-v1.%d", vmID, gen)
	}
	return &principal{Key: &APIKey{ID: keyID, UserID: id, TeamID: team.ID, Name: "vm " + vmID, Scopes: selfScopes},
		User: &User{ID: id, Name: "vm " + vmID}, Team: team, Role: "member", Scopes: selfScopes, VMID: vmID}
}

// selfAllowed: a VM token reaches only its own /v1/vms/{id} routes.
func selfAllowed(pattern string, r *http.Request, vmID string) bool {
	_, path, _ := strings.Cut(pattern, " ")
	own := strings.HasPrefix(path, "/v1/vms/{id}") || strings.HasPrefix(path, "/api/v1/sandboxes/{id}")
	return own && r.PathValue("id") == vmID
}

func (s *server) vmAPIURL() string {
	return strings.TrimRight(orDefault(s.cfg.VMAPIURL, s.cfg.PublicURL), "/")
}

// guestEnv starts a new token generation for vmID and returns BOAT_ID, and
// BOAT_URL + BOAT_API_KEY when VMs know where this API is. vars wins: a team
// that stores its own BOAT_API_KEY as a secret keeps it.
func (s *server) guestEnv(vmID, team string, vars map[string]string) map[string]string {
	return s.guestEnvForCaller(vmID, team, vars, true)
}

func (s *server) guestEnvForCaller(vmID, team string, vars map[string]string, canMint bool) map[string]string {
	inject := true
	st := s.store.Snapshot()
	if tok := st.VMTokens[vmID]; tok != nil {
		if tok.NoSelfAPI {
			inject = false
		}
	} else if st.ForeignVMs[vmID] || !canMint {
		inject = false
	}
	return s.guestEnvWithPolicy(vmID, team, vars, inject, st.ForeignVMs[vmID])
}

// guestEnvExisting returns BOAT_ID (and active BOAT_URL + BOAT_API_KEY) for an
// already-running VM without rotating its active token generation.
func (s *server) guestEnvExisting(vmID, team string, vars map[string]string, canMint bool) map[string]string {
	out := map[string]string{"BOAT_ID": vmID}
	base := s.vmAPIURL()
	st := s.store.Snapshot()
	if t := st.VMTokens[vmID]; t != nil {
		if !t.Retired && t.TeamID == team && !t.NoSelfAPI && base != "" && vars["BOAT_API_KEY"] == "" {
			out["BOAT_URL"], out["BOAT_API_KEY"] = base, s.selfToken(vmID, t.Gen)
		}
		return out
	}
	if (base != "" && !st.ForeignVMs[vmID] && canMint && vars["BOAT_API_KEY"] == "") || s.sshPolicyConfigured() {
		return s.guestEnvWithPolicy(vmID, team, vars, !st.ForeignVMs[vmID] && canMint, st.ForeignVMs[vmID])
	}
	return out
}

func (s *server) guestEnvWithPolicy(vmID, team string, vars map[string]string, injectSelfAPI, restrictSelf bool) map[string]string {
	out := map[string]string{"BOAT_ID": vmID}
	base := s.vmAPIURL()
	if base == "" && !s.sshPolicyConfigured() && injectSelfAPI && !restrictSelf {
		return out
	}
	gen, noSelfAPI, err := s.bumpSelfOpts(vmID, team, !injectSelfAPI, restrictSelf, false)
	if err != nil {
		log.Printf("vm %s: token generation: %v; no BOAT_API_KEY", vmID, err)
		return out
	}
	if base != "" && !noSelfAPI && vars["BOAT_API_KEY"] == "" {
		out["BOAT_URL"], out["BOAT_API_KEY"] = base, s.selfToken(vmID, gen)
	}
	return out
}

// selfKeyID names generation gen of vmID's token as a link minter: links it
// mints die with that generation (keyAliveIn, bumpSelf).
func selfKeyID(vmID string, gen int) string { return fmt.Sprintf("vm_%s@%d", vmID, gen) }

// durableKeyID is p's key id for grants that should outlive a VM token's
// rotation (share codes, ssh grants): vm_<id>, alive while the VM's token is.
func durableKeyID(p *principal) string {
	if p.VMID != "" {
		return p.User.ID
	}
	return p.Key.ID
}

func (s *server) bumpSelf(vmID, team string) (int, error) {
	st := s.store.Snapshot()
	noSelf := false
	if tok := st.VMTokens[vmID]; tok != nil {
		noSelf = tok.NoSelfAPI
	} else if st.ForeignVMs[vmID] {
		noSelf = true
	}
	gen, _, err := s.bumpSelfOpts(vmID, team, noSelf, st.ForeignVMs[vmID], true)
	return gen, err
}

func (s *server) bumpSelfOpts(vmID, team string, noSelfAPI, restrictSelf, copyRevoke bool) (int, bool, error) {
	gen := 0
	finalNoSelf := false
	mintedBySelf := func(keyID string) bool { return strings.HasPrefix(keyID, "vm_"+vmID+"@") }
	err := s.store.UpdateOrApply(func(st *State) (bool, error) {
		if st.Teams[team] == nil {
			return false, fmt.Errorf("VM team no longer exists")
		}
		t := st.VMTokens[vmID]
		if t != nil && (t.Retired || t.TeamID != team) {
			return false, fmt.Errorf("VM credentials are retired")
		}
		hasLinks := t != nil && !t.NoSelfAPI && hasLinksOf(st, mintedBySelf)
		syncWrite := (copyRevoke && t != nil && !t.NoSelfAPI) || hasLinks
		if t == nil {
			t = &VMToken{TeamID: team, NoSelfAPI: noSelfAPI}
			st.VMTokens[vmID] = t
		} else {
			t = st.cowVMToken(vmID)
		}
		if st.ForeignVMs[vmID] || restrictSelf {
			t.RestrictSelf = true
		}
		t.bumpGeneration()
		gen = t.Gen
		finalNoSelf = t.NoSelfAPI
		if hasLinks || (copyRevoke && !t.NoSelfAPI) {
			revokeLinksOf(st, mintedBySelf) // the old token's port links
		}
		return syncWrite, nil
	})
	return gen, finalNoSelf, err
}

func (s *server) initBuilderVMToken(vmID, team string) error {
	_, _, err := s.bumpSelfOpts(vmID, team, true, true, false)
	return err
}

func (s *server) ensureAdoptedVMToken(vmID, team string) (bool, error) {
	if vmID == "" || team == "" {
		return false, nil
	}
	if cur := s.store.Snapshot(); cur.VMTokens[vmID] != nil {
		tok := cur.VMTokens[vmID]
		return !tok.Retired && tok.TeamID == team && cur.Teams[team] != nil, nil
	}
	ok := false
	err := s.store.Update(func(st *State) error {
		if st.Teams[team] == nil {
			return nil
		}
		tok := st.VMTokens[vmID]
		if tok == nil {
			tok = &VMToken{TeamID: team, Gen: 1, NoSelfAPI: st.ForeignVMs[vmID], RestrictSelf: st.ForeignVMs[vmID]}
			st.VMTokens[vmID] = tok
			ok = true
			return nil
		}
		if st.ForeignVMs[vmID] {
			tok = st.cowVMToken(vmID)
			tok.RestrictSelf = true
		}
		ok = !tok.Retired && tok.TeamID == team
		return nil
	})
	return ok, err
}

// rotateSelf kills the token a copy of vmID's disk holds (see the top) and,
// in the background, gives the VM its new one if it is running.
func (s *server) rotateSelf(vmID string) error {
	t := s.store.Snapshot().VMTokens[vmID]
	if t == nil || t.Retired || t.NoSelfAPI {
		return nil // never had an active self token
	}
	gen, err := s.bumpSelf(vmID, t.TeamID)
	if err != nil {
		return fmt.Errorf("rotate VM %s token: %w", vmID, err)
	}
	go s.pushSelf(s.baseCtx(), vmID, gen)
	return nil
}

// pushSelf rewrites the running VM's env with token generation gen.
func (s *server) pushSelf(ctx context.Context, vmID string, gen int) {
	if t := s.store.Snapshot().VMTokens[vmID]; t == nil || t.Retired || t.Gen != gen {
		return
	}
	if err := s.refreshCredentials(ctx, vmID); err != nil {
		log.Printf("vm %s: push rotated token: %v", vmID, err)
	}
}

// Refresh reads current generations; delayed pushes never deliberately publish an old generation.
func (s *server) refreshCredentials(ctx context.Context, vmID string) error {
	// Callers release their lifecycle lock before refreshing: this full-map
	// read/write must serialize with starts and upgrades without locking twice.
	unlock := startLocks.lock(vmID)
	defer unlock()
	if s.store.Snapshot().VMTransitions[vmID] != nil {
		return errors.New("credential transition pending")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lock, _ := s.store.credentialPushes.LoadOrStore(vmID, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	var vm VM
	if err := s.boatd.call(ctx, "GET", vmPath(vmID), nil, &vm); err != nil {
		return err
	}
	if str(vm, "state") != "running" {
		return nil
	}
	if s.cfg.SSHPolicyHelperFile != "" {
		if _, err := s.prepareSSHPolicyFromVM(ctx, vmID, str(vm, "team"), vm); err != nil {
			return err
		}
	}
	var guest struct {
		Vars map[string]string `json:"vars"`
	}
	if err := s.boatd.call(ctx, "GET", vmPath(vmID, "env"), nil, &guest); err != nil {
		return err
	}
	if guest.Vars == nil {
		return fmt.Errorf("guest env response omitted vars")
	}
	vars := guest.Vars
	st := s.store.Snapshot()
	selfMatch := reSelfToken.FindStringSubmatch(vars["BOAT_API_KEY"])
	if self := st.VMTokens[vmID]; self != nil && !self.Retired && !self.NoSelfAPI && selfMatch != nil && selfMatch[3] == vmID {
		vars["BOAT_API_KEY"] = s.selfToken(vmID, self.Gen)
	}
	if identity := st.AIVMs[vmID]; identity != nil && !identity.Retired {
		for _, provider := range aiProviders {
			match := reAIToken.FindStringSubmatch(vars[provider.EnvKey])
			if match != nil && match[1] == vmID {
				vars[provider.EnvKey] = s.aiToken(vmID, identity.Gen)
			}
		}
	}
	return s.boatd.call(ctx, "POST", vmPath(vmID, "env"), map[string]any{"vars": vars}, nil)
}

func (s *server) refreshSSHPolicyOnly(ctx context.Context, vmID string) error {
	if s.cfg.SSHPolicyHelperFile == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lock, _ := s.store.credentialPushes.LoadOrStore(vmID, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	vm, err := s.getVMSSHView(ctx, vmID)
	if err != nil {
		return err
	}
	if str(vm, "state") != "running" {
		return nil
	}
	_, err = s.prepareSSHPolicyFromVM(ctx, vmID, str(vm, "team"), vm)
	return err
}

const maxRetiredVMTombstones = 8192

// Retain tombstones: a request already past ownVM must not recreate generation 1.
func retireCredentials(st *State, id string) {
	self := st.cowVMToken(id)
	if self == nil {
		self = &VMToken{}
		st.VMTokens[id] = self
	}
	self.Retired = true
	ai := st.cowAIVM(id)
	if ai == nil {
		ai = &AIVM{}
		st.AIVMs[id] = ai
	}
	ai.Retired, ai.Blocked = true, true
	revokeLinksOf(st, func(key string) bool { return key == "vm_"+id || strings.HasPrefix(key, "vm_"+id+"@") })
	pruneRetiredTombstones(st, id)
}

func pruneRetiredTombstones(st *State, keepID string) {
	retired := 0
	for _, tok := range st.VMTokens {
		if tok != nil && tok.Retired {
			retired++
		}
	}
	if retired <= maxRetiredVMTombstones {
		return
	}
	var candidates []string
	for vmID, tok := range st.VMTokens {
		if vmID != keepID && tok != nil && tok.Retired && !st.ForeignVMs[vmID] {
			candidates = append(candidates, vmID)
		}
	}
	slices.Sort(candidates)
	need := retired - maxRetiredVMTombstones
	for i := 0; i < need && i < len(candidates); i++ {
		victim := candidates[i]
		delete(st.VMTokens, victim)
		if ai := st.AIVMs[victim]; ai != nil && ai.Retired {
			delete(st.AIVMs, victim)
		}
	}
}

func (s *server) retireVM(id string) error {
	return s.store.Update(func(st *State) error { retireCredentials(st, id); return nil })
}

// The first upgraded startup revokes tokens held by historical unsafe copies.
// Later startups also retire identities whose outside-API deletion was missed.
func (s *server) reconcileCopyCredentials(ctx context.Context) error {
	before := s.store.Snapshot()
	var list struct {
		VMs []VM `json:"vms"`
	}
	if err := s.boatd.call(ctx, "GET", "/v1/vms?include_pending=true", nil, &list); err != nil {
		return err
	}
	live := map[string]VM{}
	for _, vm := range list.VMs {
		live[str(vm, "id")] = vm
	}
	// Creation labels survive an API crash. Restore policy before considering
	// any guest-held token for adoption, and fence incomplete cold transitions.
	needRehydrate := false
	for _, vm := range live {
		if needsCopyPolicyRehydrate(before, vm) {
			needRehydrate = true
			break
		}
	}
	if needRehydrate {
		if err := s.store.Update(func(st *State) error {
			for _, vm := range live {
				rehydrateCopyPolicy(st, vm)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	before = s.store.Snapshot()
	for id := range before.VMTransitions {
		if err := func() error {
			unlock := startLocks.lock(id)
			defer unlock()
			if s.store.Snapshot().VMTransitions[id] == nil {
				return nil
			}
			var vm VM
			if err := s.boatd.call(ctx, "GET", vmPath(id), nil, &vm); err != nil {
				var be *boatdError
				if errors.As(err, &be) && be.Status == http.StatusNotFound {
					delete(live, id)
					return nil
				}
				return err
			}
			live[id] = vm
			if str(vm, "state") == "running" {
				if err := s.boatd.call(ctx, "POST", vmPath(id, "stop"), map[string]any{"force": true}, nil); err != nil {
					return err
				}
				vm["state"] = "stopped"
			}
			return nil
		}(); err != nil {
			return err
		}
	}
	before = s.store.Snapshot()
	missing := map[string]bool{}
	for id, tok := range before.VMTokens {
		if !tok.Retired && (live[id] == nil || before.Teams[tok.TeamID] == nil) {
			missing[id] = true
		}
	}
	for id, tok := range before.AIVMs {
		if !tok.Retired && (live[id] == nil || before.Teams[tok.TeamID] == nil) {
			missing[id] = true
		}
	}
	for id := range before.VMEnvs {
		if live[id] == nil {
			missing[id] = true
		}
	}
	for id := range before.VMEnvPins {
		if live[id] == nil {
			missing[id] = true
		}
	}
	// Inventory may predate a create or a completed transition. Confirm each
	// candidate under its lifecycle lock and retire in that same critical section.
	for id := range missing {
		if err := func() error {
			unlock := startLocks.lock(id)
			defer unlock()
			var vm VM
			err := s.boatd.call(ctx, "GET", vmPath(id), nil, &vm)
			if err != nil {
				var be *boatdError
				if !errors.As(err, &be) || be.Status != http.StatusNotFound {
					return err
				}
				vm = nil
			}
			if vm != nil {
				live[id] = vm
			}
			return s.store.Update(func(st *State) error {
				if vm == nil {
					delete(st.VMEnvs, id)
					delete(st.VMEnvPins, id)
					retireCredentials(st, id)
					return nil
				}
				v, a := st.VMTokens[id], st.AIVMs[id]
				if v != nil && !v.Retired && (str(vm, "team") != v.TeamID || st.Teams[v.TeamID] == nil) {
					retireCredentials(st, id)
					return nil
				}
				if a != nil && !a.Retired && (str(vm, "team") != a.TeamID || st.Teams[a.TeamID] == nil) {
					retireCredentials(st, id)
					return nil
				}
				return nil
			})
		}(); err != nil {
			return err
		}
	}
	// Recover AI or self tokens issued via Store.Apply (create/start) if an
	// abrupt process crash preceded the background flush.
	recoveredAI := map[string]int{}
	recoveredSelf := map[string]int{}
	if before.CopyCredentialsV1 {
		for id, vm := range live {
			if vm["pending_copy"] == true || str(vm, "state") != "running" {
				continue
			}
			var guest struct {
				Vars map[string]string `json:"vars"`
			}
			if err := s.boatd.call(ctx, "GET", vmPath(id, "env"), nil, &guest); err != nil || guest.Vars == nil {
				continue
			}
			if s.cfg.AIProxyURL != "" {
				curGen := 0
				if row := before.AIVMs[id]; row != nil {
					if row.Retired {
						curGen = -1
					} else {
						curGen = row.Gen
					}
				}
				if curGen >= 0 {
					for _, pr := range aiProviders {
						val := guest.Vars[pr.EnvKey]
						if val == "" {
							continue
						}
						for cand := curGen + 1; cand <= curGen+16; cand++ {
							if hmac.Equal([]byte(s.aiToken(id, cand)), []byte(val)) {
								recoveredAI[id] = cand
								break
							}
						}
						if recoveredAI[id] > 0 {
							break
						}
					}
				}
			}
			if rawSelf := guest.Vars["BOAT_API_KEY"]; rawSelf != "" {
				curGen := 0
				if row := before.VMTokens[id]; row != nil {
					if row.Retired {
						curGen = -1
					} else {
						curGen = row.Gen
					}
				}
				if m := reSelfToken.FindStringSubmatch(rawSelf); curGen >= 0 && m != nil && m[3] == id {
					if cand, err := strconv.Atoi(m[1]); err == nil && cand > curGen && cand <= curGen+16 && hmac.Equal([]byte(s.selfToken(id, cand)), []byte(rawSelf)) {
						recoveredSelf[id] = cand
					}
				}
			}
		}
	}
	// Reconfirm even a previously visible rename before listeners can trust it.
	if err := s.store.Update(func(st *State) error {
		for id, gen := range recoveredAI {
			team := str(live[id], "team")
			if st.Teams[team] == nil {
				continue
			}
			if row := st.AIVMs[id]; row == nil {
				st.AIVMs[id] = &AIVM{TeamID: team, Gen: gen, Blocked: st.ForeignVMs[id]}
			} else if !row.Retired && row.TeamID == team && gen > row.Gen {
				st.cowAIVM(id).Gen = gen
			}
		}
		for id, gen := range recoveredSelf {
			team := str(live[id], "team")
			if st.Teams[team] == nil {
				continue
			}
			if row := st.VMTokens[id]; row == nil {
				st.VMTokens[id] = &VMToken{TeamID: team, Gen: gen, RestrictSelf: st.ForeignVMs[id]}
			} else if !row.Retired && row.TeamID == team && gen > row.Gen {
				st.cowVMToken(id).Gen = gen
			}
		}
		// Adopted live VMs also need lookup identities even when tenant env
		// intentionally overrides the self API token.
		if s.sshPolicyConfigured() || s.vmAPIURL() != "" {
			for id, vm := range live {
				if vm["pending_copy"] == true {
					continue
				}
				team := str(vm, "team")
				if st.VMTokens[id] == nil && st.Teams[team] != nil {
					st.VMTokens[id] = &VMToken{TeamID: team, Gen: 1, NoSelfAPI: st.ForeignVMs[id], RestrictSelf: st.ForeignVMs[id]}
				} else if tok := st.VMTokens[id]; tok != nil && st.ForeignVMs[id] {
					st.cowVMToken(id).RestrictSelf = true
				}
			}
		}
		for _, vm := range live {
			rehydrateCopyPolicy(st, vm)
		}
		if !st.CopyCredentialsV1 {
			for id, tok := range st.VMTokens {
				if !tok.Retired {
					st.cowVMToken(id).bumpGeneration()
					revokeLinksOf(st, func(key string) bool { return strings.HasPrefix(key, "vm_"+id+"@") })
				}
			}
			for id, tok := range st.AIVMs {
				if !tok.Retired {
					st.cowAIVM(id).Gen++
				}
			}
			st.CopyCredentialsV1 = true
		}
		return nil
	}); err != nil {
		return err
	}
	for id, vm := range live {
		if str(vm, "state") == "running" && vm["pending_copy"] != true {
			if s.store.Snapshot().VMTokens[id] == nil && s.store.Snapshot().AIVMs[id] == nil {
				continue
			}
			go func() {
				pushCtx, cancel := context.WithTimeout(s.baseCtx(), 30*time.Second)
				defer cancel()
				if err := s.refreshCredentials(pushCtx, id); err != nil {
					log.Printf("vm %s: startup credential refresh: %v", id, err)
				}
			}()
		}
	}
	return nil
}

// reconcileSingleRunningVMCredentials reconciles copy policy, crash-unflushed
// tokens, and adopted VM identity for one running VM without scanning the host
// or touching any other VM. Caller holds startLocks.lock(vmID) and has verified
// vm is running and not pending_copy.
func (s *server) reconcileSingleRunningVMCredentials(ctx context.Context, vmID string, vm VM) error {
	team := str(vm, "team")
	st := s.store.Snapshot()
	// Fail closed if the VM moved teams or its team no longer exists.
	if v := st.VMTokens[vmID]; v != nil && !v.Retired && (v.TeamID != team || st.Teams[v.TeamID] == nil) {
		return s.retireVM(vmID)
	}
	if a := st.AIVMs[vmID]; a != nil && !a.Retired && (a.TeamID != team || st.Teams[a.TeamID] == nil) {
		return s.retireVM(vmID)
	}
	if st.Teams[team] == nil {
		return nil
	}
	if tok := st.VMTokens[vmID]; tok != nil {
		// VM already has a recorded identity (active or retired); only ensure
		// creation labels are reflected (no-op with 0 writes when already synced).
		return s.adoptCopyPolicy(vm)
	}
	needsPolicy := needsCopyPolicyRehydrate(st, vm)
	needSelfAdopt := s.sshPolicyConfigured() || s.vmAPIURL() != ""
	var recoveredAI, recoveredSelf int
	if st.CopyCredentialsV1 && (needSelfAdopt || s.cfg.AIProxyURL != "") {
		var guest struct {
			Vars map[string]string `json:"vars"`
		}
		if err := s.boatd.call(ctx, "GET", vmPath(vmID, "env"), nil, &guest); err == nil && guest.Vars != nil {
			if s.cfg.AIProxyURL != "" {
				curGen := 0
				if row := st.AIVMs[vmID]; row != nil {
					if row.Retired {
						curGen = -1
					} else {
						curGen = row.Gen
					}
				}
				if curGen >= 0 {
					for _, pr := range aiProviders {
						val := guest.Vars[pr.EnvKey]
						if val == "" {
							continue
						}
						for cand := curGen + 1; cand <= curGen+16; cand++ {
							if hmac.Equal([]byte(s.aiToken(vmID, cand)), []byte(val)) {
								recoveredAI = cand
								break
							}
						}
						if recoveredAI > 0 {
							break
						}
					}
				}
			}
			if rawSelf := guest.Vars["BOAT_API_KEY"]; rawSelf != "" {
				if m := reSelfToken.FindStringSubmatch(rawSelf); m != nil && m[3] == vmID {
					if cand, err := strconv.Atoi(m[1]); err == nil && cand > 0 && cand <= 16 && hmac.Equal([]byte(s.selfToken(vmID, cand)), []byte(rawSelf)) {
						recoveredSelf = cand
					}
				}
			}
		}
	}
	if !needsPolicy && !needSelfAdopt && recoveredAI == 0 && recoveredSelf == 0 {
		return nil
	}
	err := s.store.Update(func(cur *State) error {
		if cur.Teams[team] == nil {
			return errNoChange
		}
		changed := needsCopyPolicyRehydrate(cur, vm)
		rehydrateCopyPolicy(cur, vm)
		if recoveredAI > 0 {
			if row := cur.AIVMs[vmID]; row == nil {
				cur.AIVMs[vmID] = &AIVM{TeamID: team, Gen: recoveredAI, Blocked: cur.ForeignVMs[vmID]}
				changed = true
			} else if !row.Retired && row.TeamID == team && recoveredAI > row.Gen {
				cur.cowAIVM(vmID).Gen = recoveredAI
				changed = true
			}
		}
		if recoveredSelf > 0 {
			if row := cur.VMTokens[vmID]; row == nil {
				cur.VMTokens[vmID] = &VMToken{TeamID: team, Gen: recoveredSelf, RestrictSelf: cur.ForeignVMs[vmID]}
				changed = true
			} else if !row.Retired && row.TeamID == team && recoveredSelf > row.Gen {
				cur.cowVMToken(vmID).Gen = recoveredSelf
				changed = true
			}
		}
		if needSelfAdopt && cur.VMTokens[vmID] == nil {
			cur.VMTokens[vmID] = &VMToken{TeamID: team, Gen: 1, NoSelfAPI: cur.ForeignVMs[vmID], RestrictSelf: cur.ForeignVMs[vmID]}
			changed = true
		}
		if needsCopyPolicyRehydrate(cur, vm) {
			changed = true
		}
		rehydrateCopyPolicy(cur, vm)
		if !changed {
			return errNoChange
		}
		return nil
	})
	if errors.Is(err, errNoChange) {
		return nil
	}
	return err
}
