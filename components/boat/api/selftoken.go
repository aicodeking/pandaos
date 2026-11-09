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
)

// A VM's own API token lets `boat self …` and `host …` work inside the guest
// without a team key. It is boatself_<gen>_<hex(HMAC(vmKey, "self|<vmid>|<gen>"))>_<vmid>,
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
// The generation is written with Store.Apply: a crash can lose a bump, which
// only makes an older token of the same VM valid again.

type VMToken struct {
	TeamID string `json:"team_id"`
	Gen    int    `json:"gen"`
}

const selfTokenPrefix = "boatself_"

var (
	reSelfToken = regexp.MustCompile(`^boatself_([0-9]{1,9})_([0-9a-f]{64})_([A-Za-z0-9_.-]{1,64})$`)
	selfScopes  = []string{"vms:read", "vms:write", "exec"}
)

func (s *server) selfToken(vmID string, gen int) string {
	return fmt.Sprintf("%s%d_%s_%s", selfTokenPrefix, gen, signHex(s.keys.vmKey, fmt.Sprintf("self|%s|%d", vmID, gen)), vmID)
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
	if t == nil || t.Gen != gen || !hmac.Equal([]byte(s.selfToken(vmID, gen)), []byte(tok)) {
		return nil
	}
	team := st.Teams[t.TeamID]
	if team == nil {
		return nil
	}
	id := "vm_" + vmID
	return &principal{Key: &APIKey{ID: id, UserID: id, TeamID: team.ID, Name: "vm " + vmID, Scopes: selfScopes},
		User: &User{ID: id, Name: "vm " + vmID}, Team: team, Role: "member", Scopes: selfScopes, VMID: vmID}
}

// selfAllowed: a VM token reaches only its own /v1/vms/{id} routes.
func selfAllowed(pattern string, r *http.Request, vmID string) bool {
	_, path, _ := strings.Cut(pattern, " ")
	return strings.HasPrefix(path, "/v1/vms/{id}") && r.PathValue("id") == vmID
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

func (s *server) bumpSelf(vmID, team string) (int, error) {
	gen := 0
	err := s.store.Apply(func(st *State) error {
		t := st.VMTokens[vmID]
		if t == nil {
			t = &VMToken{TeamID: team}
			st.VMTokens[vmID] = t
		}
		t.Gen++
		gen = t.Gen
		return nil
	})
	return gen, err
}

// rotateSelf kills the token a copy of vmID's disk holds (see the top) and,
// in the background, gives the VM its new one if it is running.
func (s *server) rotateSelf(vmID string) {
	t := s.store.Snapshot().VMTokens[vmID]
	if t == nil {
		return // never had one
	}
	gen, err := s.bumpSelf(vmID, t.TeamID)
	if err != nil {
		log.Printf("vm %s: rotate token: %v", vmID, err)
		return
	}
	go s.pushSelf(s.baseCtx(), vmID, gen)
}

// pushSelf rewrites the running VM's env with token generation gen.
func (s *server) pushSelf(ctx context.Context, vmID string, gen int) {
	var vm VM
	if err := s.boatd.call(ctx, "GET", vmPath(vmID), nil, &vm); err != nil || str(vm, "state") != "running" {
		return // the next start gets a fresh token
	}
	vars := map[string]string{}
	cur, _ := vm["env"].(map[string]any)
	for k, v := range cur {
		if sv, ok := v.(string); ok {
			vars[k] = sv
		}
	}
	if !strings.HasPrefix(vars["BOAT_API_KEY"], selfTokenPrefix) {
		return // the team's own key, or none
	}
	vars["BOAT_API_KEY"] = s.selfToken(vmID, gen)
	if err := s.boatd.call(ctx, "POST", vmPath(vmID, "env"), map[string]any{"vars": vars}, nil); err != nil {
		log.Printf("vm %s: push rotated token: %v", vmID, err)
	}
}
