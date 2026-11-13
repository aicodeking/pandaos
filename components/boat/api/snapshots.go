package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Snapshot download, import and sharing. A share grants another team read
// access, or carries a secret code (a signed link) for anyone holding it.
// Taking a shared snapshot copies it into the taker's team through boatd's
// download + import, so tenant isolation in boatd stays as it is. A snapshot
// holds the VM's disk, so anything on it (secrets in /etc/boat/env included)
// goes with the share. Proxied AI keys do not, they were never on the disk, but
// the VM's proxy token is, so creating a share ends that token (rotateAIToken).

const (
	defaultLinkTTL = 7 * 24 * time.Hour
	maxLinkTTL     = 30 * 24 * time.Hour
)

var reShareCode = regexp.MustCompile(`^bsh_([0-9a-f]{16})_([0-9a-f]{64})$`)

// ownSnapshot checks the VM is the caller's and the snapshot is that VM's.
func (s *server) ownSnapshot(w http.ResponseWriter, r *http.Request, p *principal) (string, bool) {
	sid := r.PathValue("sid")
	if !reID.MatchString(sid) {
		writeErr(w, r, 404, "not_found", "snapshot not found")
		return "", false
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return "", false
	}
	var list struct {
		Snapshots []map[string]any `json:"snapshots"`
	}
	if err := s.boatd.call(r.Context(), "GET", vmPath(r.PathValue("id"), "snapshots"), nil, &list); err != nil {
		writeUpstream(w, r, err)
		return "", false
	}
	for _, sn := range list.Snapshots {
		if str(sn, "id") == sid {
			return sid, true
		}
	}
	writeErr(w, r, 404, "not_found", "snapshot not found")
	return "", false
}

func (s *server) relayDownload(w http.ResponseWriter, r *http.Request, vmID, sid string) {
	resp, err := s.boatd.do(r.Context(), "GET", vmPath(vmID, "snapshots", url.PathEscape(sid), "download"), nil, "")
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
	w.Header().Set("Content-Type", orDefault(resp.Header.Get("Content-Type"), "application/octet-stream"))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar.zst"`, sid))
	if n := resp.Header.Get("Content-Length"); n != "" {
		w.Header().Set("Content-Length", n)
	}
	w.WriteHeader(200)
	if _, err := io.Copy(w, resp.Body); err != nil && r.Context().Err() == nil {
		log.Printf("%s: snapshot download %s: %v", info(r).id, sid, err)
	}
}

func (s *server) downloadSnapshot(w http.ResponseWriter, r *http.Request, p *principal) {
	if sid, ok := s.ownSnapshot(w, r, p); ok {
		s.relayDownload(w, r, r.PathValue("id"), sid)
	}
}

func snapName(n string) bool { return n == "" || reVMName.MatchString(n) }

// importInto streams a snapshot archive to boatd under team.
func (s *server) importInto(r *http.Request, team, name string, body io.Reader) (map[string]any, error) {
	q := url.Values{"team": {team}} // boatd trusts this; it is always the caller's
	if name != "" {
		q.Set("name", name)
	}
	for _, k := range []string{"image", "size"} { // a disk-only import needs these
		if v := r.URL.Query().Get(k); reVMNameOrDotted.MatchString(v) {
			q.Set(k, v)
		}
	}
	resp, err := s.boatd.do(r.Context(), "POST", "/v1/snapshots/import?"+q.Encode(), body, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, &boatdError{Status: resp.StatusCode, Body: b}
	}
	out := map[string]any{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

var reVMNameOrDotted = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// listTeamSnapshots includes imported snapshots, which belong to no VM.
func (s *server) listTeamSnapshots(w http.ResponseWriter, r *http.Request, p *principal) {
	var out struct {
		Snapshots []map[string]any `json:"snapshots"`
	}
	if err := s.boatd.call(r.Context(), "GET", "/v1/snapshots?team="+url.QueryEscape(p.Team.ID), nil, &out); err != nil {
		writeUpstream(w, r, err)
		return
	}
	list := []map[string]any{}
	for _, sn := range out.Snapshots {
		if str(sn, "team") == p.Team.ID {
			list = append(list, sn)
		}
	}
	writeJSON(w, 200, map[string]any{"snapshots": list})
}

// deleteTeamSnapshot checks the team itself: boatd's route takes any id.
func (s *server) deleteTeamSnapshot(w http.ResponseWriter, r *http.Request, p *principal) {
	sid := r.PathValue("sid")
	var out struct {
		Snapshots []map[string]any `json:"snapshots"`
	}
	if !reID.MatchString(sid) {
		writeErr(w, r, 404, "not_found", "snapshot not found")
		return
	}
	if err := s.boatd.call(r.Context(), "GET", "/v1/snapshots?team="+url.QueryEscape(p.Team.ID), nil, &out); err != nil {
		writeUpstream(w, r, err)
		return
	}
	for _, sn := range out.Snapshots {
		if str(sn, "id") == sid && str(sn, "team") == p.Team.ID {
			if err := s.boatd.call(r.Context(), "DELETE", "/v1/snapshots/"+url.PathEscape(sid), nil, nil); err != nil {
				writeUpstream(w, r, err)
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	writeErr(w, r, 404, "not_found", "snapshot not found")
}

// importSnapshot is POST /v1/snapshots/import with the archive as the raw body.
func (s *server) importSnapshot(w http.ResponseWriter, r *http.Request, p *principal) {
	name := r.URL.Query().Get("name")
	if !snapName(name) {
		writeErr(w, r, 400, "invalid_request", "invalid name")
		return
	}
	out, err := s.importInto(r, p.Team.ID, name, http.MaxBytesReader(w, r.Body, s.cfg.MaxImportBytes))
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	writeJSON(w, 201, out)
}

type shareView struct {
	ID         string     `json:"id"`
	VMID       string     `json:"vm_id"`
	SnapshotID string     `json:"snapshot_id"`
	Grantee    string     `json:"grantee,omitempty"`
	Expires    *time.Time `json:"expires,omitempty"`
	Uses       int        `json:"uses"`
	Created    time.Time  `json:"created"`
	Code       string     `json:"code,omitempty"` // only in the create response
	URL        string     `json:"url,omitempty"`  // only in the create response
}

func viewShare(sh *SnapShare) shareView {
	return shareView{ID: sh.ID, VMID: sh.VMID, SnapshotID: sh.SnapshotID, Grantee: sh.Grantee, Expires: sh.Expires, Uses: sh.Uses, Created: sh.Created}
}

func (s *server) createShare(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		TeamID string `json:"team_id"`
		TTLS   int64  `json:"ttl_s"`
	}
	if !decode(w, r, &body) {
		return
	}
	ttl := time.Duration(body.TTLS) * time.Second
	if body.TTLS < 0 || ttl > maxLinkTTL {
		writeErr(w, r, 400, "invalid_request", "ttl_s must be 0..2592000")
		return
	}
	sid, ok := s.ownSnapshot(w, r, p)
	if !ok {
		return
	}
	now := time.Now().UTC()
	sh := &SnapShare{ID: randHex(8), TeamID: p.Team.ID, VMID: r.PathValue("id"), SnapshotID: sid, Created: now}
	view := shareView{}
	if body.TeamID != "" {
		// The answer is the same whether the team exists or not (no oracle);
		// the grant reaches it only once it allowlists this team (grantFor).
		if !reTeamID.MatchString(body.TeamID) || body.TeamID == p.Team.ID {
			writeErr(w, r, 400, "invalid_request", "team_id must be another team's id")
			return
		}
		sh.Grantee = body.TeamID
		if ttl > 0 {
			until := now.Add(ttl)
			sh.Expires = &until
		}
	} else {
		if ttl == 0 {
			ttl = defaultLinkTTL
		}
		until := now.Add(ttl)
		sh.Expires = &until
		secret := randHex(32)
		sh.CodeSalt = randHex(16)
		sh.CodeHash = s.keys.HashSecret(sh.CodeSalt, secret)
		sh.KeyID = durableKeyID(p) // sharing rotates a VM token: its link must outlive that
		view.Code = "bsh_" + sh.ID + "_" + secret
		if s.cfg.PublicURL != "" {
			view.URL = strings.TrimRight(s.cfg.PublicURL, "/") + "/v1/snapshot-links/" + view.Code + "/download"
		}
	}
	// Before the share exists: if this fails, no live token has left the team.
	if err := s.rotateSelf(sh.VMID); err != nil {
		log.Printf("%s: %v", info(r).id, err)
		writeErr(w, r, 500, "store_error", "could not persist VM token rotation")
		return
	}
	if err := s.rotateAIToken(r, sh.VMID, p.Team.ID); err != nil {
		log.Printf("%s: rotate AI token of %s: %v", info(r).id, sh.VMID, err)
		writeErr(w, r, 500, "secrets_unavailable", "cannot rotate the VM's AI token")
		return
	}
	err := s.store.Update(func(st *State) error {
		n := 0
		for k, x := range st.SnapShares {
			switch {
			case x.TeamID != p.Team.ID:
			case !shareLive(x):
				delete(st.SnapShares, k)
			default:
				n++
			}
		}
		if n >= maxSharesPerTeam {
			return errLimit
		}
		st.SnapShares[sh.ID] = sh
		return nil
	})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	v := viewShare(sh)
	v.Code, v.URL = view.Code, view.URL
	writeJSON(w, 201, v)
}

// rotateAIToken moves the VM to a new proxy token generation, so the token on
// any of its snapshots stops working, and gives a running VM the new one. Its
// running processes keep the old token until they restart. Raw keys (proxy off)
// cannot be taken back this way; docs/boat.md says so.
func (s *server) rotateAIToken(r *http.Request, vmID, team string) error {
	if s.store.Snapshot().AIVMs[vmID] == nil {
		return nil // never had a token
	}
	if _, err := s.aiBumpVM(vmID, team, true); err != nil {
		return err
	}
	ai, err := s.aiEnv(vmID, team, false)
	if err != nil {
		return err
	}
	var vm VM
	if err := s.boatd.call(r.Context(), "GET", vmPath(vmID), nil, &vm); err != nil {
		return err
	}
	if str(vm, "state") != "running" || len(ai) == 0 {
		return nil // the next start gets a fresh token
	}
	base := map[string]string{}
	cur, _ := vm["env"].(map[string]any)
	for k, v := range cur {
		if sv, ok := v.(string); ok {
			base[k] = sv
		}
	}
	s.pushEnv(r, vmID, base, ai)
	return nil
}

func (s *server) listShares(w http.ResponseWriter, r *http.Request, p *principal) {
	sid, ok := s.ownSnapshot(w, r, p)
	if !ok {
		return
	}
	out := []shareView{}
	for _, sh := range s.store.Snapshot().SnapShares {
		if sh.SnapshotID == sid && sh.TeamID == p.Team.ID {
			out = append(out, viewShare(sh))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	writeJSON(w, 200, map[string]any{"shares": out})
}

func (s *server) revokeShare(w http.ResponseWriter, r *http.Request, p *principal) {
	sid, ok := s.ownSnapshot(w, r, p)
	if !ok {
		return
	}
	id := r.PathValue("share")
	sh := s.store.Snapshot().SnapShares[id]
	if sh == nil || sh.SnapshotID != sid || sh.TeamID != p.Team.ID {
		writeErr(w, r, 404, "not_found", "share not found")
		return
	}
	if err := s.store.Update(func(st *State) error { delete(st.SnapShares, id); return nil }); err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func shareLive(sh *SnapShare) bool { return sh.Expires == nil || time.Now().Before(*sh.Expires) }

// grantFor: a live team grant to team that team has accepted (allowlisted the sender).
func grantFor(st *State, sh *SnapShare, team string) bool {
	t := st.Teams[team]
	return sh != nil && sh.Grantee == team && shareLive(sh) && t != nil && slices.Contains(t.AcceptSharesFrom, sh.TeamID)
}

// sharedWithMe lists grants to the caller's team.
func (s *server) sharedWithMe(w http.ResponseWriter, r *http.Request, p *principal) {
	out := []shareView{}
	st := s.store.Snapshot()
	for _, sh := range st.SnapShares {
		if grantFor(st, sh, p.Team.ID) {
			out = append(out, viewShare(sh))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	writeJSON(w, 200, map[string]any{"shares": out})
}

func (s *server) shareByCode(code string) *SnapShare {
	m := reShareCode.FindStringSubmatch(code)
	if m == nil {
		return nil
	}
	sh := s.store.Snapshot().SnapShares[m[1]]
	if sh == nil || sh.CodeHash == "" || !shareLive(sh) ||
		!constEq(s.keys.HashSecret(sh.CodeSalt, m[2]), sh.CodeHash) ||
		(sh.KeyID != "" && !s.keyAlive(sh.KeyID, sh.TeamID, "", time.Now())) {
		return nil
	}
	return sh
}

func (s *server) countShareUse(id string) {
	err := s.store.Update(func(st *State) error {
		if sh := st.SnapShares[id]; sh != nil {
			sh.Uses++
		}
		return nil
	})
	if err != nil {
		log.Printf("share %s: count use: %v", id, err)
	}
}

// copyShare streams the shared snapshot into team and returns the new snapshot.
func (s *server) copyShare(r *http.Request, sh *SnapShare, team, name string) (map[string]any, error) {
	resp, err := s.boatd.do(r.Context(), "GET", vmPath(sh.VMID, "snapshots", url.PathEscape(sh.SnapshotID), "download"), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return nil, &boatdError{Status: resp.StatusCode, Body: b}
	}
	out, err := s.importInto(r, team, name, resp.Body)
	if err == nil {
		s.countShareUse(sh.ID)
	}
	return out, err
}

// importShare takes a share: by id for a grant to this team, or by code.
func (s *server) importShare(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		ShareID string `json:"share_id"`
		Code    string `json:"code"`
		Name    string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !snapName(body.Name) || (body.ShareID == "") == (body.Code == "") {
		writeErr(w, r, 400, "invalid_request", "give exactly one of share_id or code, and a valid name")
		return
	}
	var sh *SnapShare
	if body.Code != "" {
		sh = s.shareByCode(body.Code)
	} else if st := s.store.Snapshot(); grantFor(st, st.SnapShares[body.ShareID], p.Team.ID) {
		sh = st.SnapShares[body.ShareID]
	}
	if sh == nil {
		writeErr(w, r, 404, "not_found", "share not found or expired")
		return
	}
	out, err := s.copyShare(r, sh, p.Team.ID, body.Name)
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	writeJSON(w, 201, out)
}

// linkDownload is the unauthenticated signed link: the code is the credential.
func (s *server) linkDownload(w http.ResponseWriter, r *http.Request) {
	info(r).audit = true
	ip := r.Header.Get("X-Forwarded-For") // Caddy sets it; the listener is loopback
	if ip == "" {
		ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	if ok, _ := s.limiter.allow("link:" + ip); !ok {
		writeErr(w, r, 429, "rate_limited", "rate limit exceeded")
		return
	}
	sh := s.shareByCode(r.PathValue("code"))
	if sh == nil {
		writeErr(w, r, 404, "not_found", "link not found or expired")
		return
	}
	// Its own bucket: link holders must not use up the sharing team's streams.
	release, ok := s.acquireOr429(w, r, s.streams, "link:"+sh.TeamID)
	if !ok {
		return
	}
	defer release()
	s.countShareUse(sh.ID)
	s.relayDownload(w, r, sh.VMID, sh.SnapshotID)
}
