package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
)

// The ssh jump host (user "jump" on the gateway) authorizes keys from
// cfg.JumpKeysFile. Each line only lets a key open port 22 on the VMs it
// was registered for:
//
//	restrict,port-forwarding,permitopen="10.200.x.y:22",... ssh-ed25519 AAAA...
//
// The file is rebuilt from SSHGrants plus boatd's live VM list, so deleted or
// re-assigned VMs (and their reused ips) drop out on the next sync.
//
// A grant belongs to the API key (and user) that minted it. When that key is
// deleted or expires, or its user leaves the team, the next sync drops the
// grant, rewrites the jump file and removes the key from the VM's
// authorized_keys (best effort). Grants from before this record (no KeyID)
// are "unowned": they keep working and team admins can revoke them.

var (
	errGrantDenied  = errors.New("ssh grant owned by another member")
	errGrantMissing = errors.New("no such ssh grant")
)

func grantKey(team, pub, keyID string) string {
	if keyID == "" {
		return team + "|" + pub // pre-ownership grants
	}
	return team + "|" + pub + "|" + keyID
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

// grantAlive: an owned grant stands while its minting key does (keyAliveIn).
func grantAlive(st *State, g *SSHGrant, now time.Time) bool {
	return g.KeyID == "" || keyAliveIn(st, g.KeyID, g.TeamID, "", now)
}

// grantJump records (team, key, vm, minting key) and rewrites the jump keys file.
func (s *server) grantJump(r *http.Request, p *principal, vmID, pub string) error {
	f := strings.Fields(pub)
	norm := f[0] + " " + f[1] // drop the comment; we control the whole line
	team := p.Team.ID
	err := s.store.Update(func(st *State) error {
		k := grantKey(team, norm, p.Key.ID)
		g := st.SSHGrants[k]
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
			g = &SSHGrant{TeamID: team, PublicKey: norm, KeyID: durableKeyID(p), UserID: p.User.ID, Created: time.Now().UTC()}
			st.SSHGrants[k] = g
		}
		if !slices.Contains(g.VMs, vmID) {
			g.VMs = append(slices.Clone(g.VMs), vmID)
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

// jumpExpiryLoop syncs when an owned grant's key has died without an API call
// (expiry): the sync drops it from the jump file and the VM.
func (s *server) jumpExpiryLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st, now := s.store.Snapshot(), time.Now()
			for _, g := range st.SSHGrants {
				if !grantAlive(st, g, now) {
					s.syncJumpKeys(ctx)
					break
				}
			}
		}
	}
}

func (s *server) syncJumpKeysErr(ctx context.Context) error {
	if s.cfg.JumpKeysFile == "" && len(s.store.Snapshot().SSHGrants) == 0 {
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
	now := time.Now()
	// drop dead-key grants and grants for VMs that no longer exist (or changed team)
	var revoked []*SSHGrant // dead-key grants, for the VMs' authorized_keys
	err := s.store.Update(func(st *State) error {
		for k, g := range st.SSHGrants {
			if !grantAlive(st, g, now) {
				revoked = append(revoked, g)
				delete(st.SSHGrants, k)
				continue
			}
			kept := slices.DeleteFunc(slices.Clone(g.VMs), func(id string) bool { vm := vms[id]; return vm == nil || str(vm, "team") != g.TeamID })
			if len(kept) == 0 {
				delete(st.SSHGrants, k)
			} else if len(kept) != len(g.VMs) {
				ng := *g
				ng.VMs = kept
				st.SSHGrants[k] = &ng
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	grants := s.store.Snapshot().SSHGrants
	for _, g := range revoked {
		for _, vm := range g.VMs {
			if v := vms[vm]; v != nil && str(v, "team") == g.TeamID && !keyGrantedOn(grants, g.TeamID, g.PublicKey, vm) {
				s.unauthorizeKeyLogged(ctx, vm, g.PublicKey)
			}
		}
	}
	if s.cfg.JumpKeysFile == "" {
		return nil
	}
	return writeAtomicMode(s.cfg.JumpKeysFile, renderJumpKeys(grants, vms), 0o640)
}

// keyGrantedOn: another live grant still lets pub reach vm.
func keyGrantedOn(grants map[string]*SSHGrant, team, pub, vm string) bool {
	for _, g := range grants {
		if g.TeamID == team && g.PublicKey == pub && slices.Contains(g.VMs, vm) {
			return true
		}
	}
	return false
}

// renderJumpKeys builds the authorized_keys file. sshd uses the first line
// that matches a key, so all grants of one key share one line.
func renderJumpKeys(grants map[string]*SSHGrant, vms map[string]VM) []byte {
	opens := map[string][]string{}
	for _, g := range grants {
		if !reSSHKey.MatchString(g.PublicKey) {
			continue
		}
		for _, id := range g.VMs {
			vm := vms[id]
			if vm == nil || str(vm, "team") != g.TeamID {
				continue
			}
			ip := net.ParseIP(str(vm, "ip"))
			if ip == nil || ip.To4() == nil {
				continue // not running / no address yet; kept for the next sync
			}
			opens[g.PublicKey] = append(opens[g.PublicKey], fmt.Sprintf(`permitopen="%s:22"`, ip.To4()))
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

func (s *server) unauthorizeKeyLogged(ctx context.Context, vmID, pub string) {
	if err := s.unauthorizeKey(ctx, vmID, pub); err != nil {
		log.Printf("ssh revoke: remove %s from vm %s authorized_keys: %v", sshFingerprint(pub), vmID, err)
	}
}

// unauthorizeKey removes pub's lines from the VM user's authorized_keys.
func (s *server) unauthorizeKey(ctx context.Context, vmID, pub string) error {
	path := s.authorizedKeysPath()
	resp, err := s.boatd.do(ctx, "GET", vmPath(vmID, "files")+"?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return err
	}
	cur, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil
	}
	if resp.StatusCode/100 != 2 {
		return &boatdError{Status: resp.StatusCode, Body: cur}
	}
	want := strings.Fields(pub)
	var kept []string
	for _, line := range strings.Split(string(cur), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == want[0] && f[1] == want[1] {
			continue
		}
		kept = append(kept, line)
	}
	next := strings.Join(kept, "\n")
	if next == string(cur) {
		return nil
	}
	resp, err = s.boatd.do(ctx, "PUT", vmPath(vmID, "files")+"?path="+url.QueryEscape(path), strings.NewReader(next), "application/octet-stream")
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
	for _, g := range s.store.Snapshot().SSHGrants {
		if g.TeamID != p.Team.ID || !slices.Contains(g.VMs, id) {
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
	admin := p.Role == "owner" || p.Role == "admin"
	var pub string
	err := s.store.Update(func(st *State) error {
		found := false
		for k, g := range st.SSHGrants {
			if g.TeamID != p.Team.ID || !slices.Contains(g.VMs, id) || sshFingerprint(g.PublicKey) != fp {
				continue
			}
			found, pub = true, g.PublicKey
			if !admin && (g.KeyID == "" || g.UserID != p.User.ID) {
				return errGrantDenied
			}
			ng := *g
			ng.VMs = slices.DeleteFunc(slices.Clone(g.VMs), func(v string) bool { return v == id })
			if len(ng.VMs) == 0 {
				delete(st.SSHGrants, k)
			} else {
				st.SSHGrants[k] = &ng
			}
		}
		if !found {
			return errGrantMissing
		}
		return nil
	})
	switch {
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
	s.unauthorizeKeyLogged(r.Context(), id, pub)
	if err := s.syncJumpKeysErr(r.Context()); err != nil {
		log.Printf("%s: jump keys: %v", info(r).id, err)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "fingerprint": fp})
}
