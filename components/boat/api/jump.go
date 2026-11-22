package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

// Managed SSH authorization is served from committed grants at authentication
// time. Static guest and jump keys are never a publication mechanism.

var (
	errGrantDenied  = errors.New("ssh grant owned by another member")
	errGrantMissing = errors.New("no such ssh grant")
	errSSHAuthority = errors.New("SSH authority is no longer valid")
)

func grantKey(team, pub, keyID string) string {
	if keyID == "" {
		return team + "|" + pub // pre-ownership grants
	}
	return team + "|" + pub + "|" + keyID
}

func grantKeyGen(team, pub, keyID string, keyGen int64) string {
	if keyID == "" || keyGen == 0 || strings.HasPrefix(keyID, "vm_") {
		return grantKey(team, pub, keyID)
	}
	return fmt.Sprintf("%s|%s|%s@%d", team, pub, keyID, keyGen)
}

// sshFingerprint is OpenSSH's SHA256:<base64> fingerprint of "type base64".
func sshFingerprint(pub string) string {
	f := strings.Fields(pub)
	if len(f) < 2 {
		return ""
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// grantAlive: an owned grant stands while its minting key does (keyAliveGenIn).
func grantAlive(st *State, g *SSHGrant, now time.Time) bool {
	if k := st.Keys[g.KeyID]; k != nil && st.Users[k.UserID] == nil {
		return false
	}
	return len(g.VMs) > 0 && g.KeyID != "" && !strings.HasPrefix(g.KeyID, pendingSSHKeyPrefix) &&
		keyAliveGenIn(st, g.KeyID, g.CreatedByKeyGen, g.TeamID, "exec", now)
}

// Historical pending grants remain cleanup debt and never authorize. New
// revocations coalesce CleanupVMs on their original grants. API and VM key IDs
// cannot contain this prefix.
const pendingSSHKeyPrefix = "ssh-pending:"

func sshAuthority(st *State, p *principal, vmID string) bool {
	if p == nil || !p.has("exec") {
		return false
	}
	if p.VMID == "" {
		current := currentKeyPrincipal(st, p, time.Now())
		return current != nil && current.has("exec")
	}
	if st.ForeignVMs[p.VMID] {
		return false
	}
	token := st.VMTokens[p.VMID]
	team := st.Teams[p.Team.ID]
	return p.VMID == vmID && p.Team.SelfToken.SSH && team != nil && team.SelfToken.SSH &&
		token != nil && !token.Retired && !token.RestrictSelf && token.TeamID == team.ID &&
		(p.Key.ID == selfKeyID(p.VMID, token.Gen) && !st.CopyCredentialsV1 ||
			p.Key.ID == fmt.Sprintf("vm_%s@copy-v1.%d", p.VMID, token.Gen) && st.CopyCredentialsV1)
}

// Preparing guest policy cannot authorize a key: only the durable commit below
// publishes it. Final authority checks share Store.mu with every invalidation.
func (s *server) grantJump(r *http.Request, p *principal, vmID, pub string) error {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if !sshAuthority(s.store.Snapshot(), p, vmID) {
		return errSSHAuthority
	}
	f := strings.Fields(pub)
	if len(f) < 2 || !reSSHKey.MatchString(pub) {
		return errSSHAuthority
	}
	norm := f[0] + " " + f[1]
	if err := sshGrantCapacity(s.store.Snapshot(), p.Team.ID, grantKeyGen(p.Team.ID, norm, durableKeyID(p), p.KeyGen)); err != nil {
		return err
	}
	vm, err := s.prepareSSHPolicy(ctx, vmID, p.Team.ID)
	if err != nil {
		return err
	}
	fresh, err := s.getVMSSHView(ctx, vmID)
	if err != nil {
		return err
	}
	if str(fresh, "id") != vmID || str(fresh, "team") != p.Team.ID || str(fresh, "ssh_boot_epoch") != str(vm, "ssh_boot_epoch") || !s.sshPolicyCurrent(s.store.Snapshot(), fresh) {
		return errSSHAuthority
	}
	vm = fresh
	return s.store.Update(func(st *State) error {
		now := time.Now()
		if !sshAuthority(st, p, vmID) || !s.sshPolicyCurrent(st, vm) {
			return errSSHAuthority
		}
		keyGen := int64(0)
		if p.VMID == "" {
			cur := currentKeyPrincipal(st, p, now)
			if cur == nil || !cur.has("exec") {
				return errSSHAuthority
			}
			keyGen = cur.KeyGen
		}
		k := grantKeyGen(p.Team.ID, norm, durableKeyID(p), keyGen)
		if err := sshGrantCapacity(st, p.Team.ID, k); err != nil {
			return err
		}
		g := st.SSHGrants[k]
		if g == nil {
			g = &SSHGrant{TeamID: p.Team.ID, PublicKey: norm, KeyID: durableKeyID(p), CreatedByKeyGen: keyGen, UserID: p.User.ID, Created: now.UTC()}
			st.SSHGrants[k] = g
		} else if !grantAlive(st, g, now) {
			for _, oldVM := range g.VMs {
				if !slices.Contains(g.CleanupVMs, oldVM) {
					g.CleanupVMs = append(g.CleanupVMs, oldVM)
				}
			}
			g.VMs = nil
			g.CreatedByKeyGen = keyGen
		}
		if !slices.Contains(g.VMs, vmID) {
			g.VMs = append(slices.Clone(g.VMs), vmID)
		}
		return nil
	})
}

func sshGrantCapacity(st *State, team, k string) error {
	n := 0
	for _, g := range st.SSHGrants {
		if g.TeamID == team {
			n++
		}
	}
	if n > maxGrantsPerTeam || n == maxGrantsPerTeam && st.SSHGrants[k] == nil {
		return errLimit
	}
	return nil
}

func liveSSHGrants(st *State) map[string]*SSHGrant {
	out := map[string]*SSHGrant{}
	for k, g := range st.SSHGrants {
		if grantAlive(st, g, time.Now()) {
			out[k] = g
		}
	}
	return out
}

// dropVMFromGrants removes a deleted VM from every owned SSH grant (and its
// pending cleanup debt: the VM is gone, there is nothing left to clean), and
// deletes grants left with no VM. It is what syncJumpKeysOnce would conclude for
// a VM missing from boatd's list, without listing the host or re-attesting every
// other VM. Grants without a KeyID are unowned historical evidence and stay.
func dropVMFromGrants(st *State, id string) {
	for k, g := range st.SSHGrants {
		if g.KeyID == "" || (!slices.Contains(g.VMs, id) && !slices.Contains(g.CleanupVMs, id)) {
			continue
		}
		g.VMs = slices.DeleteFunc(slices.Clone(g.VMs), func(v string) bool { return v == id })
		g.CleanupVMs = slices.DeleteFunc(slices.Clone(g.CleanupVMs), func(v string) bool { return v == id })
		if len(g.VMs) == 0 && len(g.CleanupVMs) == 0 {
			delete(st.SSHGrants, k)
		}
	}
}

// jumpReattestScope bounds which running VMs syncJumpKeysOnce re-attests.
// A single-VM revoke re-attests only that VM; a team-wide revocation
// (removeMember, rotateKey, deleteKey) re-attests only that team's running VMs;
// startup/reconnect uses all=true; SSE-driven and expiry cleanup use empty scope.
type jumpReattestScope struct {
	all   bool
	teams map[string]bool
	vms   map[string]bool
}

func (sc jumpReattestScope) active() bool {
	return sc.all || len(sc.teams) > 0 || len(sc.vms) > 0
}

func (sc jumpReattestScope) matches(vmID, team string) bool {
	return sc.all || sc.vms[vmID] || sc.teams[team]
}

func (sc *jumpReattestScope) merge(other jumpReattestScope) {
	if other.all {
		sc.all = true
		sc.teams = nil
		sc.vms = nil
		return
	}
	if sc.all {
		return
	}
	for t := range other.teams {
		if sc.teams == nil {
			sc.teams = map[string]bool{}
		}
		sc.teams[t] = true
	}
	for id := range other.vms {
		if sc.vms == nil {
			sc.vms = map[string]bool{}
		}
		sc.vms[id] = true
	}
}

func (s *server) clearSSHDeferredVM(vmID string) {
	if vmID == "" {
		return
	}
	s.sshCleanupMu.Lock()
	delete(s.sshDeferredVMs, vmID)
	s.sshCleanupMu.Unlock()
}

func (s *server) hasActionableSSHGrantDebt(st *State, now time.Time) bool {
	s.sshCleanupMu.Lock()
	defer s.sshCleanupMu.Unlock()
	for _, g := range st.SSHGrants {
		for _, id := range g.CleanupVMs {
			if !s.sshDeferredVMs[id] {
				return true
			}
		}
		if !grantAlive(st, g, now) {
			if g.KeyID != "" && len(g.VMs) == 0 && len(g.CleanupVMs) == 0 {
				return true
			}
			for _, id := range g.VMs {
				if !s.sshDeferredVMs[id] {
					return true
				}
			}
		}
	}
	return false
}

func (s *server) syncJumpKeys(ctx context.Context) {
	if err := s.syncJumpKeysErr(ctx); err != nil {
		log.Printf("jump keys sync: %v", err)
	}
}

func (s *server) syncJumpKeysForTeam(ctx context.Context, teamID string) {
	scope := jumpReattestScope{}
	if teamID != "" {
		scope.teams = map[string]bool{teamID: true}
	}
	if err := s.syncJumpKeysScoped(ctx, scope); err != nil {
		log.Printf("jump keys sync (team %s): %v", teamID, err)
	}
}

func (s *server) syncJumpKeysForVM(ctx context.Context, vmID string) error {
	scope := jumpReattestScope{}
	if vmID != "" {
		scope.vms = map[string]bool{vmID: true}
	}
	return s.syncJumpKeysScoped(ctx, scope)
}

// jumpExpiryLoop retries bounded policy migration and cleanup after credential
// expiry. Authentication denial depends on committed state, never this retry.
// Stopped/non-running VMs already observed in cleanup debt are skipped until
// they start or are deleted, avoiding endless 60 s host-wide polling.
func (s *server) jumpExpiryLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st, now := s.store.Snapshot(), time.Now()
			if hasDeadPortMinters(st, now) {
				_ = s.store.Update(func(cur *State) error { revokeDeadPortMinters(cur, now); return nil })
			}
			if s.hasActionableSSHGrantDebt(st, now) {
				if err := s.syncJumpKeysScoped(ctx, jumpReattestScope{}); err != nil {
					log.Printf("jump keys sync: %v", err)
				}
			}
		}
	}
}

func (s *server) syncJumpKeysErr(ctx context.Context) error { return s.syncJumpKeysLocked(ctx) }

const defaultJumpSyncDebounce = 50 * time.Millisecond

func (s *server) jumpSyncDebounce() time.Duration {
	if s.sshCleanupDebounce > 0 {
		return s.sshCleanupDebounce
	}
	return defaultJumpSyncDebounce
}

func (s *server) jumpKeysFileNeedsClear() bool {
	if s.cfg.JumpKeysFile == "" {
		return false
	}
	fi, err := os.Lstat(s.cfg.JumpKeysFile)
	return err != nil || !fi.Mode().IsRegular() || fi.Size() != 0 || fi.Mode().Perm() != 0o640
}

func (s *server) clearJumpKeysFile() error {
	if !s.jumpKeysFileNeedsClear() {
		return nil
	}
	return writeAtomicMode(s.cfg.JumpKeysFile, nil, 0o640)
}

func vmEventAffectsJumpKeys(typ, state string) bool {
	switch typ {
	case "vm.created", "vm.deleted":
		return true
	case "vm.state":
		return state == "running" || state == "stopped" || state == "error"
	default:
		return false
	}
}

// needsJumpKeysSyncForEvent reports whether a boatd VM SSE event can require
// clearing the legacy jump file or cleaning/pruning SSH grants. Routine VM
// lifecycle transitions with no grant debt and an already-cleared (or unset)
// legacy file return false, avoiding host-wide scans and fsync storms.
func (s *server) needsJumpKeysSyncForEvent(typ, vmID, team, state string) bool {
	if !vmEventAffectsJumpKeys(typ, state) {
		return false
	}
	if vmID != "" && (typ == "vm.deleted" || state == "running") {
		s.clearSSHDeferredVM(vmID)
	}
	if s.jumpKeysFileNeedsClear() {
		return true
	}
	st := s.store.Snapshot()
	if len(st.SSHGrants) == 0 {
		return false
	}
	now := time.Now()
	for _, g := range st.SSHGrants {
		if g.KeyID != "" && len(g.VMs) == 0 && len(g.CleanupVMs) == 0 {
			return true
		}
		if vmID == "" {
			if g.KeyID != "" || len(g.CleanupVMs) > 0 || !grantAlive(st, g, now) {
				return true
			}
			continue
		}
		if len(g.CleanupVMs) > 0 && slices.Contains(g.CleanupVMs, vmID) && (typ == "vm.deleted" || state == "running") {
			return true
		}
		if !grantAlive(st, g, now) && slices.Contains(g.VMs, vmID) && (typ == "vm.deleted" || state == "running") {
			return true
		}
		if g.KeyID != "" && team != "" && g.TeamID != team && slices.Contains(g.VMs, vmID) {
			return true
		}
		if g.KeyID != "" && typ == "vm.deleted" && (slices.Contains(g.VMs, vmID) || slices.Contains(g.CleanupVMs, vmID)) {
			return true
		}
	}
	return false
}

// triggerJumpKeysSync coalesces and debounces asynchronous SSE-driven jump-key
// sync requests into a single-flight execution without host-wide re-attestation.
func (s *server) triggerJumpKeysSync(ctx context.Context) {
	if ctx.Err() != nil || s.store.closedFlag.Load() {
		return
	}
	s.sshCleanupMu.Lock()
	defer s.sshCleanupMu.Unlock()
	if s.sshCleanupRunning {
		s.sshCleanupDirty = true
		return
	}
	if s.sshCleanupTimer != nil {
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(s.jumpSyncDebounce(), func() {
		s.sshCleanupMu.Lock()
		if s.sshCleanupTimer != timer {
			s.sshCleanupMu.Unlock()
			return
		}
		s.sshCleanupTimer = nil
		s.sshCleanupMu.Unlock()
		if ctx.Err() != nil || s.store.closedFlag.Load() {
			return
		}
		if err := s.syncJumpKeysScoped(ctx, jumpReattestScope{}); err != nil {
			log.Printf("jump keys sync: %v", err)
		}
	})
	s.sshCleanupTimer = timer
}

// Historical static files are cleared, never populated. Guest installation
// archives legacy files and disables their use. Cleanup debt stays on its
// original grant, so repeated revokes cannot allocate unbounded records.
func (s *server) syncJumpKeysLocked(ctx context.Context) error {
	return s.syncJumpKeysScoped(ctx, jumpReattestScope{all: true})
}

func (s *server) syncJumpKeysScoped(ctx context.Context, scope jumpReattestScope) error {
	s.sshCleanupMu.Lock()
	if s.sshCleanupTimer != nil {
		s.sshCleanupTimer.Stop()
		s.sshCleanupTimer = nil
	}
	if s.sshCleanupRunning {
		s.sshCleanupDirty = true
		s.sshCleanupScope.merge(scope)
		s.sshCleanupMu.Unlock()
		return nil
	}
	s.sshCleanupRunning = true
	s.sshCleanupDirty = false
	s.sshCleanupScope = jumpReattestScope{}
	s.sshCleanupMu.Unlock()

	done := false
	defer func() {
		if !done {
			s.sshCleanupMu.Lock()
			s.sshCleanupRunning = false
			s.sshCleanupMu.Unlock()
		}
	}()

	var finalErr error
	runScope := scope
	for {
		finalErr = errors.Join(finalErr, s.syncJumpKeysOnce(ctx, runScope))
		s.sshCleanupMu.Lock()
		if !s.sshCleanupDirty || ctx.Err() != nil || s.store.closedFlag.Load() {
			s.sshCleanupDirty = false
			s.sshCleanupScope = jumpReattestScope{}
			s.sshCleanupRunning = false
			done = true
			s.sshCleanupMu.Unlock()
			return finalErr
		}
		runScope = s.sshCleanupScope
		s.sshCleanupDirty = false
		s.sshCleanupScope = jumpReattestScope{}
		s.sshCleanupMu.Unlock()
	}
}

func sshGrantsNeedPrune(st *State, vms map[string]VM, cleaned map[string]map[string]bool, now time.Time) bool {
	for k, g := range st.SSHGrants {
		if g.KeyID == "" {
			continue
		}
		if len(g.VMs) == 0 && len(g.CleanupVMs) == 0 {
			return true
		}
		for _, id := range g.VMs {
			vm := vms[id]
			if vm == nil || str(vm, "team") != g.TeamID {
				return true
			}
		}
		for _, id := range g.CleanupVMs {
			if cleaned[k][id] {
				return true
			}
		}
		if !grantAlive(st, g, now) {
			for _, id := range g.VMs {
				if cleaned[k][id] {
					return true
				}
			}
		}
	}
	return false
}

func (s *server) syncJumpKeysOnce(ctx context.Context, scope jumpReattestScope) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.clearJumpKeysFile(); err != nil {
		return err
	}
	st := s.store.Snapshot()
	if len(st.SSHGrants) == 0 && (!scope.active() || !s.sshPolicyConfigured()) {
		s.sshCleanupMu.Lock()
		s.sshDeferredVMs = nil
		s.sshCleanupMu.Unlock()
		return nil
	}
	var list struct {
		VMs []VM `json:"vms"`
	}
	if err := s.boatd.call(ctx, "GET", "/v1/vms", nil, &list); err != nil {
		return err
	}
	vms := map[string]VM{}
	for _, vm := range list.VMs {
		vms[str(vm, "id")] = vm
	}
	st = s.store.Snapshot()
	cleaned := map[string]map[string]bool{}
	var cleanupErr error
	prepared := map[string]bool{}
	attempted := map[string]bool{}
	deferred := map[string]bool{}
	now := time.Now()
	for k, g := range st.SSHGrants {
		ids := slices.Clone(g.CleanupVMs)
		if !grantAlive(st, g, now) {
			ids = append(ids, g.VMs...)
		}
		slices.Sort(ids)
		for _, id := range slices.Compact(ids) {
			vm := vms[id]
			ok := vm == nil || str(vm, "team") != g.TeamID
			if !ok {
				if str(vm, "state") != "running" {
					// A stopped/non-running VM gets its SSH policy applied on start;
					// skip prepareSSHPolicy and defer until it starts or is deleted.
					deferred[id] = true
				} else {
					identity := g.TeamID + "|" + id
					if !attempted[identity] {
						_, err := s.prepareSSHPolicyFromVM(ctx, id, g.TeamID, vm)
						attempted[identity] = true
						prepared[identity] = err == nil
						cleanupErr = errors.Join(cleanupErr, err)
					}
					ok = prepared[identity]
				}
			}
			if ok {
				if cleaned[k] == nil {
					cleaned[k] = map[string]bool{}
				}
				cleaned[k][id] = true
			}
		}
	}
	s.sshCleanupMu.Lock()
	if len(deferred) == 0 {
		s.sshDeferredVMs = nil
	} else {
		s.sshDeferredVMs = deferred
	}
	s.sshCleanupMu.Unlock()
	var err error
	if len(st.SSHGrants) > 0 && sshGrantsNeedPrune(st, vms, cleaned, time.Now()) {
		err = s.store.Update(func(cur *State) error {
			now := time.Now()
			changed := false
			for k, g := range cur.SSHGrants {
				// Preserve unowned historical evidence even after policy migration.
				if g.KeyID == "" {
					continue
				}
				original := st.SSHGrants[k]
				if original == nil {
					continue
				}
				newVMs := slices.DeleteFunc(slices.Clone(g.VMs), func(id string) bool {
					vm := vms[id]
					return slices.Contains(original.VMs, id) && (vm == nil || str(vm, "team") != g.TeamID)
				})
				newCleanup := slices.DeleteFunc(slices.Clone(g.CleanupVMs), func(id string) bool { return cleaned[k][id] })
				if !grantAlive(cur, g, now) {
					newVMs = slices.DeleteFunc(newVMs, func(id string) bool { return cleaned[k][id] })
				}
				if len(newVMs) == 0 && len(newCleanup) == 0 {
					delete(cur.SSHGrants, k)
					changed = true
					continue
				}
				if !slices.Equal(newVMs, g.VMs) || !slices.Equal(newCleanup, g.CleanupVMs) {
					g.VMs = newVMs
					g.CleanupVMs = newCleanup
					changed = true
				}
			}
			if !changed {
				return errNoChange
			}
			return nil
		})
		if errors.Is(err, errNoChange) {
			err = nil
		}
	}
	if scope.active() && s.sshPolicyConfigured() {
		for _, vm := range list.VMs {
			id, team := str(vm, "id"), str(vm, "team")
			if !scope.matches(id, team) {
				continue
			}
			if attempted[team+"|"+id] {
				continue
			}
			if rerr := s.ensureRunningVMSSHPolicy(ctx, vm); rerr != nil {
				log.Printf("vm %s: sync SSH policy re-attestation: %v", id, rerr)
			}
		}
	}
	return errors.Join(cleanupErr, err)
}

// keyGrantedOn: another live grant still lets pub reach vm.
func keyGrantedOn(grants map[string]*SSHGrant, team, pub, vm string) bool {
	for _, g := range grants {
		if g.TeamID == team && g.PublicKey == pub && (slices.Contains(g.VMs, vm) || slices.Contains(g.VMs, "*") || slices.Contains(g.VMs, "")) {
			return true
		}
	}
	return false
}

// renderJumpKeys builds the authorized_keys file. sshd uses the first line
// that matches a key, so all grants of one key share one line.
func renderJumpKeys(grants map[string]*SSHGrant, vms map[string]VM) []byte {
	opens := map[string][]string{}
	addOpen := func(pub string, vm VM, team string) {
		if vm == nil || str(vm, "team") != team {
			return
		}
		ip := net.ParseIP(str(vm, "ip"))
		if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.Equal(net.IPv4bcast) {
			return // not running / no address yet; kept for the next sync
		}
		opens[pub] = append(opens[pub], fmt.Sprintf(`permitopen="%s:22"`, ip.To4()))
	}
	for _, g := range grants {
		if !reSSHKey.MatchString(g.PublicKey) {
			continue
		}
		for _, id := range g.VMs {
			if id == "" || id == "*" {
				for _, vm := range vms {
					addOpen(g.PublicKey, vm, g.TeamID)
				}
				continue
			}
			addOpen(g.PublicKey, vms[id], g.TeamID)
		}
	}
	var lines []string
	for pub, o := range opens {
		sort.Strings(o)
		lines = append(lines, "restrict,port-forwarding,"+strings.Join(slices.Compact(o), ",")+" "+pub)
	}
	sort.Strings(lines)
	out := strings.Join(lines, "\n")
	if out != "" {
		out += "\n"
	}
	return []byte(out)
}

func (s *server) authorizedKeysPath() string {
	if s.cfg.SSHUser == "root" {
		return "/root/.ssh/authorized_keys"
	}
	return "/home/" + s.cfg.SSHUser + "/.ssh/authorized_keys"
}

type sshGrantView struct {
	Fingerprint string    `json:"fingerprint"`
	PublicKey   string    `json:"public_key"`
	KeyID       *string   `json:"key_id"`  // null: unowned (minted before grants recorded their key)
	UserID      *string   `json:"user_id"` // null: unowned
	Unowned     bool      `json:"unowned"`
	Created     time.Time `json:"created,omitzero"`
}

// listSSHGrants: GET /v1/vms/{id}/ssh, the keys granted on the VM.
func (s *server) listSSHGrants(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id := r.PathValue("id")
	out := []sshGrantView{}
	st, now := s.store.Snapshot(), time.Now()
	for _, g := range st.SSHGrants {
		if strings.HasPrefix(g.KeyID, pendingSSHKeyPrefix) || g.TeamID != p.Team.ID || !slices.Contains(g.VMs, id) {
			continue
		}
		if g.KeyID != "" && !grantAlive(st, g, now) {
			continue
		}
		v := sshGrantView{Fingerprint: sshFingerprint(g.PublicKey), PublicKey: g.PublicKey, Unowned: g.KeyID == "", Created: g.Created}
		if g.KeyID != "" {
			v.KeyID, v.UserID = &g.KeyID, &g.UserID
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fingerprint < out[j].Fingerprint })
	writeJSON(w, 200, map[string]any{"keys": out})
}

// normFingerprint accepts SHA256:<b64>, the bare <b64>, or its URL-safe form.
func normFingerprint(fp string) string {
	fp = strings.TrimPrefix(fp, "SHA256:")
	fp = strings.NewReplacer("-", "+", "_", "/").Replace(strings.TrimRight(fp, "="))
	return "SHA256:" + fp
}

// revokeSSHGrant: DELETE /v1/vms/{id}/ssh/{fingerprint}. A member revokes
// grants their own keys minted; team admins revoke any (unowned ones too).
func (s *server) revokeSSHGrant(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id, fp := r.PathValue("id"), normFingerprint(r.PathValue("fingerprint"))
	err := s.store.Update(func(st *State) error {
		// Revalidate inside the transaction (r27): a key revoked or demoted
		// since authentication cannot revoke grants. Revoking another member's
		// grant needs the admin scope as well as an owner/admin role.
		cur, aerr := requirePrincipalIn(st, p, "exec", time.Now())
		if aerr != nil {
			return aerr
		}
		admin := (cur.Role == "owner" || cur.Role == "admin") && cur.has("admin")
		found := false
		for k, g := range st.SSHGrants {
			if g.TeamID != p.Team.ID || !slices.Contains(g.VMs, id) || sshFingerprint(g.PublicKey) != fp {
				continue
			}
			found = true
			if !admin && (g.KeyID == "" || g.UserID != p.User.ID) {
				return errGrantDenied
			}
			// Move the VM to cleanup on the original grant. Its managed
			// authorization disappears at this durable transaction.
			ng := *g
			ng.VMs = slices.DeleteFunc(slices.Clone(g.VMs), func(v string) bool { return v == id })
			ng.CleanupVMs = slices.Clone(g.CleanupVMs)
			if !slices.Contains(ng.CleanupVMs, id) {
				ng.CleanupVMs = append(ng.CleanupVMs, id)
			}
			st.SSHGrants[k] = &ng
		}
		if !found {
			return errGrantMissing
		}

		return nil
	})
	var ae *authError
	switch {
	case errors.As(err, &ae):
		writeErr(w, r, ae.status, ae.code, ae.msg)
		return
	case errors.Is(err, errGrantDenied):
		writeErr(w, r, 403, "forbidden", "this key was granted by another member (or before grants recorded their owner); a team admin can revoke it")
		return
	case errors.Is(err, errGrantMissing):
		writeErr(w, r, 404, "not_found", "no ssh key with that fingerprint on this vm")
		return
	case err != nil:
		s.storeErr(w, r, err)
		return
	}
	if err := s.syncJumpKeysForVM(r.Context(), id); err != nil {
		log.Printf("%s: jump keys: %v", info(r).id, err)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "fingerprint": fp})
}
