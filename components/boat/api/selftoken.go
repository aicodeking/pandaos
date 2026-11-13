package main

import (
	"context"
	"crypto/hmac"
	"fmt"
	"log"
	"net/http"
	"regexp"
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
	TeamID  string `json:"team_id"`
	Gen     int    `json:"gen"`
	Retired bool   `json:"retired,omitempty"`
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
	if t == nil || t.Retired || t.Gen != gen || !hmac.Equal([]byte(s.selfToken(vmID, gen)), []byte(tok)) {
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
	out := map[string]string{"BOAT_ID": vmID}
	base := s.vmAPIURL()
	if base == "" || vars["BOAT_API_KEY"] != "" {
		return out
	}
	gen, err := s.bumpSelf(vmID, team)
	if err != nil {
		log.Printf("vm %s: token generation: %v; no BOAT_API_KEY", vmID, err)
		return out
	}
	out["BOAT_URL"], out["BOAT_API_KEY"] = base, s.selfToken(vmID, gen)
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
	gen := 0
	mintedBySelf := func(keyID string) bool { return strings.HasPrefix(keyID, "vm_"+vmID+"@") }
	err := s.store.Update(func(st *State) error {
		if st.Teams[team] == nil {
			return fmt.Errorf("VM team no longer exists")
		}
		t := st.VMTokens[vmID]
		if t != nil && (t.Retired || t.TeamID != team) {
			return fmt.Errorf("VM credentials are retired")
		}
		if t == nil {
			t = &VMToken{TeamID: team}
			st.VMTokens[vmID] = t
		}
		t.Gen++
		gen = t.Gen
		revokeLinksOf(st, mintedBySelf) // the old token's port links
		return nil
	})
	return gen, err
}

// rotateSelf kills the token a copy of vmID's disk holds (see the top) and,
// in the background, gives the VM its new one if it is running.
func (s *server) rotateSelf(vmID string) error {
	t := s.store.Snapshot().VMTokens[vmID]
	if t == nil {
		return nil // never had one
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
	if self := st.VMTokens[vmID]; self != nil && !self.Retired && selfMatch != nil && selfMatch[3] == vmID {
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

// Retain tombstones: a request already past ownVM must not recreate generation 1.
// shortcut: tombstones persist forever; use non-repeating epochs if their size becomes costly.
func retireCredentials(st *State, id string) {
	self := st.VMTokens[id]
	if self == nil {
		self = &VMToken{}
		st.VMTokens[id] = self
	}
	self.Retired = true
	ai := st.AIVMs[id]
	if ai == nil {
		ai = &AIVM{}
		st.AIVMs[id] = ai
	}
	ai.Retired, ai.Blocked = true, true
	revokeLinksOf(st, func(key string) bool { return key == "vm_"+id || strings.HasPrefix(key, "vm_"+id+"@") })
}

func (s *server) retireVM(id string) error {
	return s.store.Update(func(st *State) error { retireCredentials(st, id); return nil })
}

// The first upgraded startup revokes tokens held by historical unsafe copies.
// Later startups also retire identities whose outside-API deletion was missed.
func (s *server) reconcileCopyCredentials(ctx context.Context) error {
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
	cur := s.store.Snapshot()
	missing := map[string]bool{}
	for id, tok := range cur.VMTokens {
		if !tok.Retired && (live[id] == nil || cur.Teams[tok.TeamID] == nil) {
			missing[id] = true
		}
	}
	for id, tok := range cur.AIVMs {
		if !tok.Retired && (live[id] == nil || cur.Teams[tok.TeamID] == nil) {
			missing[id] = true
		}
	}
	// Reconfirm even a previously visible rename before listeners can trust it.
	if err := s.store.Update(func(st *State) error {
		for id := range missing {
			retireCredentials(st, id)
		}
		if !st.CopyCredentialsV1 {
			for id, tok := range st.VMTokens {
				if !tok.Retired {
					tok.Gen++
					revokeLinksOf(st, func(key string) bool { return strings.HasPrefix(key, "vm_"+id+"@") })
				}
			}
			for _, tok := range st.AIVMs {
				if !tok.Retired {
					tok.Gen++
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
