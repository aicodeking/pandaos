package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
)

// The ssh jump host (user "jump" on the gateway) authorizes keys from
// cfg.JumpKeysFile. Each line only lets a key open port 22 on the VMs it
// was registered for:
//
//	restrict,port-forwarding,permitopen="10.200.x.y:22",... ssh-ed25519 AAAA...
//
// The file is rebuilt from SSHGrants plus boatd's live VM list, so deleted or
// re-assigned VMs (and their reused ips) drop out on the next sync.

func grantKey(team, pub string) string { return team + "|" + pub }

// grantJump records (team, key, vm) and rewrites the jump keys file.
func (s *server) grantJump(r *http.Request, team, vmID, pub string) error {
	if s.cfg.JumpKeysFile == "" {
		return nil
	}
	f := strings.Fields(pub)
	norm := f[0] + " " + f[1] // drop the comment; we control the whole line
	err := s.store.Update(func(st *State) error {
		g := st.SSHGrants[grantKey(team, norm)]
		if g == nil {
			n := 0
			for _, x := range st.SSHGrants {
				if x.TeamID == team {
					n++
				}
			}
			if n >= maxGrantsPerTeam {
				return errLimit
			}
			g = &SSHGrant{TeamID: team, PublicKey: norm}
			st.SSHGrants[grantKey(team, norm)] = g
		}
		if !slices.Contains(g.VMs, vmID) {
			g.VMs = append(g.VMs, vmID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.syncJumpKeysErr(r.Context())
}

func (s *server) syncJumpKeys(ctx context.Context) {
	if err := s.syncJumpKeysErr(ctx); err != nil {
		log.Printf("jump keys sync: %v", err)
	}
}

func (s *server) syncJumpKeysErr(ctx context.Context) error {
	if s.cfg.JumpKeysFile == "" {
		return nil
	}
	s.jumpMu.Lock()
	defer s.jumpMu.Unlock()
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
	data, live := renderJumpKeys(s.store.Snapshot().SSHGrants, vms)
	if err := writeAtomicMode(s.cfg.JumpKeysFile, data, 0o640); err != nil {
		return err
	}
	// prune grants for VMs that no longer exist (or changed team)
	return s.store.Update(func(st *State) error {
		for k, g := range st.SSHGrants {
			g.VMs = slices.DeleteFunc(g.VMs, func(id string) bool { return !live[id] })
			if len(g.VMs) == 0 {
				delete(st.SSHGrants, k)
			}
		}
		return nil
	})
}

// renderJumpKeys builds the authorized_keys file and the set of VMs still valid.
func renderJumpKeys(grants map[string]*SSHGrant, vms map[string]VM) ([]byte, map[string]bool) {
	live := map[string]bool{}
	var lines []string
	for _, g := range grants {
		var opens []string
		for _, id := range g.VMs {
			vm := vms[id]
			if vm == nil || str(vm, "team") != g.TeamID {
				continue
			}
			live[id] = true
			ip := net.ParseIP(str(vm, "ip"))
			if ip == nil || ip.To4() == nil {
				continue // not running / no address yet; kept for the next sync
			}
			opens = append(opens, fmt.Sprintf(`permitopen="%s:22"`, ip.To4()))
		}
		if len(opens) == 0 || !reSSHKey.MatchString(g.PublicKey) {
			continue
		}
		sort.Strings(opens)
		lines = append(lines, "restrict,port-forwarding,"+strings.Join(slices.Compact(opens), ",")+" "+g.PublicKey)
	}
	sort.Strings(lines)
	out := strings.Join(lines, "\n")
	if out != "" {
		out += "\n"
	}
	return []byte(out), live
}
