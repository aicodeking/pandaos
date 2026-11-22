package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
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
	path := vmPath(vmID, "snapshots", url.PathEscape(sid), "download")
	if s.snapshotSourceProtected(sid) {
		path += "?protected=1"
	}
	resp, err := s.boatd.do(r.Context(), "GET", path, nil, "")
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

func shareActiveIn(st *State, sh *SnapShare, now time.Time) bool {
	if sh == nil || (sh.Expires != nil && !now.Before(*sh.Expires)) {
		return false
	}
	if sh.KeyID != "" && !keyAliveGenIn(st, sh.KeyID, sh.MintedByKeyGen, sh.TeamID, "admin", now) {
		return false
	}
	return true
}

// listTeamSnapshots includes imported snapshots, which belong to no VM.
func (s *server) listTeamSnapshots(w http.ResponseWriter, r *http.Request, p *principal) {
	vis := r.URL.Query().Get("visibility")
	if vis != "" && vis != "shared" && vis != "all" {
		writeErr(w, r, 400, "invalid_query", "visibility must be shared or all")
		return
	}
	var out struct {
		Snapshots []map[string]any `json:"snapshots"`
	}
	if err := s.boatd.call(r.Context(), "GET", "/v1/snapshots?team="+url.QueryEscape(p.Team.ID), nil, &out); err != nil {
		writeUpstream(w, r, err)
		return
	}
	var shared map[string]bool
	if vis == "shared" {
		shared = map[string]bool{}
		st, now := s.store.Snapshot(), time.Now()
		for _, sh := range st.SnapShares {
			if sh.TeamID == p.Team.ID && shareActiveIn(st, sh, now) {
				shared[sh.SnapshotID] = true
			}
		}
	}
	list := []map[string]any{}
	for _, sn := range out.Snapshots {
		if str(sn, "team") == p.Team.ID && (vis != "shared" || shared[str(sn, "id")]) {
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
			if s.refuseBuilderSnapshot(w, r, p, sn) {
				return
			}
			if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
				return
			}
			if err := s.boatd.call(r.Context(), "DELETE", "/v1/snapshots/"+url.PathEscape(sid), nil, nil); err != nil {
				writeUpstream(w, r, err)
				return
			}
			s.forgetSnapshotSource(sid)
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	writeErr(w, r, 404, "not_found", "snapshot not found")
}

func isUploadTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (s *server) discardImportedSnapshot(sid string) {
	if sid == "" {
		return
	}
	ctx, cancel := context.WithTimeout(s.baseCtx(), 30*time.Second)
	defer cancel()
	if err := s.boatd.call(ctx, "DELETE", "/v1/snapshots/"+url.PathEscape(sid), nil, nil); err != nil {
		log.Printf("discard revoked snapshot import %s: %v", sid, err)
		return
	}
	s.forgetSnapshotSource(sid)
}

// importSnapshot is POST /v1/snapshots/import with the archive as the raw body.
func (s *server) importSnapshot(w http.ResponseWriter, r *http.Request, p *principal) {
	name := r.URL.Query().Get("name")
	if !snapName(name) {
		writeErr(w, r, 400, "invalid_request", "invalid name")
		return
	}
	if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
		return
	}
	rd, done := s.uploadBodyReader(w, r.Body)
	defer done()
	out, err := s.importInto(r, p.Team.ID, name, http.MaxBytesReader(w, rd, s.cfg.MaxImportBytes))
	if err != nil {
		if isUploadTimeout(err) {
			writeErr(w, r, http.StatusRequestTimeout, "request_timeout", "upload stalled or exceeded maximum duration")
			return
		}
		writeUpstream(w, r, err)
		return
	}
	sid := str(out, "id")
	if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
		s.discardImportedSnapshot(sid)
		return
	}
	protected := importedSnapshotProtected(out)
	noEnv := importedSnapshotNoEnv(out) || protected
	if noEnv && sid != "" {
		if err := s.store.Update(func(cur *State) error {
			cur.VMSecrets[snapPolicyKey(sid)] = []string{snapSourceVM}
			if protected {
				cur.VMSecrets[snapSourceKey(sid)] = []string{snapSourceVM, snapNoEnv}
				cur.VMSecrets[snapProtectedKey(sid)] = []string{snapSourceVM}
			}
			return nil
		}); err != nil {
			s.discardImportedSnapshot(sid)
			s.storeErr(w, r, err)
			return
		}
	}
	writeJSON(w, 201, out)
}

func importedSnapshotProtected(out map[string]any) bool {
	if out == nil {
		return false
	}
	b, _ := out["protected"].(bool)
	return b
}

func importedSnapshotNoEnv(out map[string]any) bool {
	if out == nil {
		return false
	}
	if b, _ := out["no_env"].(bool); b {
		return true
	}
	return labeledNoEnv(out)
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
	if body.TeamID != "" && (!reTeamID.MatchString(body.TeamID) || body.TeamID == p.Team.ID) {
		writeErr(w, r, 400, "invalid_request", "team_id must be another team's id")
		return
	}
	vmID := r.PathValue("id")
	shID := randHex(8)
	view := shareView{}
	var codeSalt, codeHash string
	if body.TeamID == "" {
		if ttl == 0 {
			ttl = defaultLinkTTL
		}
		secret := randHex(32)
		codeSalt = randHex(16)
		codeHash = s.keys.HashSecret(codeSalt, secret)
		view.Code = "bsh_" + shID + "_" + secret
		if s.cfg.PublicURL != "" {
			view.URL = strings.TrimRight(s.cfg.PublicURL, "/") + "/v1/snapshot-links/" + view.Code + "/download"
		}
	}
	errRefused := errors.New("snapshot share refused")
	status, code, message := 0, "", ""
	var sh *SnapShare
	selfGen := 0
	refreshAI := false
	err := s.store.Update(func(st *State) error {
		now := time.Now().UTC()
		current := currentKeyPrincipal(st, p, now)
		if current == nil {
			status, code, message = 401, "unauthorized", "missing or invalid API key"
			return errRefused
		}
		if (current.Role != "owner" && current.Role != "admin") || !current.has("admin") {
			status, code, message = 403, "forbidden", "key lacks scope admin"
			return errRefused
		}
		// Rotate VM self and AI tokens in the same transaction as the share so a
		// revoked key, retired credential, quota limit, or failed save leaves no
		// partial rotation or dangling share behind.
		if tok := st.VMTokens[vmID]; tok != nil {
			if tok.Retired || tok.TeamID != current.Team.ID {
				log.Printf("%s: rotate VM %s token: VM credentials are retired", info(r).id, vmID)
				status, code, message = 500, "store_error", "could not persist VM token rotation"
				return errRefused
			}
			tok = st.cowVMToken(vmID)
			tok.bumpGeneration()
			selfGen = tok.Gen
			revokeLinksOf(st, func(keyID string) bool { return strings.HasPrefix(keyID, "vm_"+vmID+"@") })
		} else if s.cfg.SSHPolicyHelperFile != "" {
			// Like the fork/export rotations: a VM with SSH policy but no token yet
			// gets one now, so its SSH state is rotated with the share too.
			if st.Teams[current.Team.ID] == nil {
				status, code, message = 500, "store_error", "could not persist VM token rotation"
				return errRefused
			}
			st.VMTokens[vmID] = &VMToken{TeamID: current.Team.ID, Gen: 1, NoSelfAPI: st.ForeignVMs[vmID], RestrictSelf: st.ForeignVMs[vmID]}
			selfGen = 1
		}
		if ai := st.AIVMs[vmID]; ai != nil {
			if ai.Retired || ai.TeamID != current.Team.ID {
				log.Printf("%s: rotate AI token of %s: VM credentials are retired", info(r).id, vmID)
				status, code, message = 500, "secrets_unavailable", "cannot rotate the VM's AI token"
				return errRefused
			}
			st.cowAIVM(vmID).Gen++
			refreshAI = true
		}
		n := 0
		for k, x := range st.SnapShares {
			switch {
			case x.TeamID != current.Team.ID:
			case !shareLive(x):
				delete(st.SnapShares, k)
			default:
				n++
			}
		}
		if n >= maxSharesPerTeam {
			return errLimit
		}
		created := &SnapShare{
			ID:             shID,
			TeamID:         current.Team.ID,
			VMID:           vmID,
			SnapshotID:     sid,
			Created:        now,
			KeyID:          durableKeyID(current), // sharing rotates a VM token: its share must outlive that
			MintedByKeyGen: current.KeyGen,
		}
		if body.TeamID != "" {
			// The answer is the same whether the team exists or not (no oracle);
			// the grant reaches it only once it allowlists this team (grantFor).
			created.Grantee = body.TeamID
			if ttl > 0 {
				until := now.Add(ttl)
				created.Expires = &until
			}
		} else {
			until := now.Add(ttl)
			created.Expires = &until
			created.CodeSalt = codeSalt
			created.CodeHash = codeHash
		}
		if current.Key != nil && current.Key.Expires != nil {
			if created.Expires == nil || current.Key.Expires.Before(*created.Expires) {
				exp := *current.Key.Expires
				created.Expires = &exp
			}
		}
		st.SnapShares[created.ID] = created
		sh = created
		return nil
	})
	if err != nil {
		if errors.Is(err, errRefused) {
			writeErr(w, r, status, code, message)
		} else {
			s.storeErr(w, r, err)
		}
		return
	}
	if selfGen > 0 || s.sshPolicyConfigured() {
		// Synchronous: the source VM must hold the rotated credentials (token,
		// AI proxy token when present, and SSH state) before this answers, never
		// a stale or partial set.
		var pushErr error
		if refreshAI || selfGen == 0 {
			pushErr = s.refreshCredentials(r.Context(), vmID)
		} else {
			pushErr = s.pushRotatedSelf(r.Context(), vmID, selfGen)
		}
		if pushErr != nil {
			log.Printf("%s: vm %s: push rotated credentials after share %s: %v", info(r).id, vmID, sh.ID, pushErr)
			shareID := sh.ID
			if uerr := s.store.Update(func(st *State) error { delete(st.SnapShares, shareID); return nil }); uerr != nil {
				log.Printf("%s: roll back share %s: %v", info(r).id, shareID, uerr)
			}
			writeErr(w, r, http.StatusBadGateway, "credential_push_failed", "the source VM could not take its rotated credentials; the share was not created, retry")
			return
		}
	} else if refreshAI {
		if err := s.refreshCredentials(r.Context(), vmID); err != nil {
			log.Printf("vm %s: refresh rotated AI credentials: %v", vmID, err)
		}
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
	if err := s.refreshCredentials(r.Context(), vmID); err != nil {
		log.Printf("vm %s: refresh rotated AI credentials: %v", vmID, err)
	}
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
	if err := s.store.Update(func(st *State) error {
		if _, err := requirePrincipalIn(st, p, "admin", time.Now().UTC()); err != nil {
			return err
		}
		delete(st.SnapShares, id)
		return nil
	}); err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func shareLive(sh *SnapShare) bool { return sh.Expires == nil || time.Now().Before(*sh.Expires) }

// grantFor: a live team grant to team that team has accepted (allowlisted the sender).
func grantFor(st *State, sh *SnapShare, team string) bool {
	t := st.Teams[team]
	now := time.Now()
	return sh != nil && sh.Grantee == team && shareLive(sh) && t != nil && slices.Contains(t.AcceptSharesFrom, sh.TeamID) &&
		sh.KeyID != "" && keyAliveGenIn(st, sh.KeyID, sh.MintedByKeyGen, sh.TeamID, "admin", now)
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
	st, now := s.store.Snapshot(), time.Now()
	sh := st.SnapShares[m[1]]
	if sh == nil || sh.CodeHash == "" || !shareLive(sh) ||
		!constEq(s.keys.HashSecret(sh.CodeSalt, m[2]), sh.CodeHash) ||
		(sh.KeyID != "" && !keyAliveGenIn(st, sh.KeyID, sh.MintedByKeyGen, sh.TeamID, "admin", now)) {
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
	path := vmPath(sh.VMID, "snapshots", url.PathEscape(sh.SnapshotID), "download")
	if s.snapshotSourceProtected(sh.SnapshotID) {
		path += "?protected=1"
	}
	resp, err := s.boatd.do(r.Context(), "GET", path, nil, "")
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
		if err = s.copyShareRecord(sh, team, str(out, "id")); err != nil {
			s.discardImportedSnapshot(str(out, "id"))
			return nil, err
		}
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
	if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
		return
	}
	resolve := func() *SnapShare {
		if body.Code != "" {
			return s.shareByCode(body.Code)
		}
		if st := s.store.Snapshot(); grantFor(st, st.SnapShares[body.ShareID], p.Team.ID) {
			return st.SnapShares[body.ShareID]
		}
		return nil
	}
	sh := resolve()
	if sh == nil {
		writeErr(w, r, 404, "not_found", "share not found or expired")
		return
	}
	out, err := s.copyShare(r, sh, p.Team.ID, body.Name)
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	if s.revalidatePrincipal(w, r, p, "vms:write") == nil {
		s.discardImportedSnapshot(str(out, "id"))
		return
	}
	if resolve() == nil {
		s.discardImportedSnapshot(str(out, "id"))
		writeErr(w, r, 404, "not_found", "share not found or expired")
		return
	}
	s.countShareUse(sh.ID)
	writeJSON(w, 201, out)
}

// linkDownload is the unauthenticated signed link: the code is the credential.
func (s *server) linkDownload(w http.ResponseWriter, r *http.Request) {
	info(r).audit = true
	ip := trustedClientIP(r)
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
	// Re-check the share (expiry, deletion, the minting key) while streaming.
	r, stop := s.watchAlive(r, func(st *State, now time.Time) bool {
		return shareActiveIn(st, st.SnapShares[sh.ID], now)
	})
	defer stop()
	s.relayDownload(w, r, sh.VMID, sh.SnapshotID)
}

// pushRotatedSelf delivers generation gen of vmID's credentials, unless a newer
// rotation already superseded it (that one pushes its own).
func (s *server) pushRotatedSelf(ctx context.Context, vmID string, gen int) error {
	if t := s.store.Snapshot().VMTokens[vmID]; t == nil || t.Retired || t.Gen != gen {
		return nil
	}
	return s.refreshCredentials(ctx, vmID)
}

// ---- the secret selection a snapshot was taken under ----
//
// A memory snapshot resumes the processes of the run it was taken from, whose
// environ holds that run's secrets; its disk holds /etc/boat/env. A VM created
// from it with a narrower selection must therefore cold-boot (fresh_boot: the
// disk only, never the memory). The selection of the source run is kept in
// State.VMSecrets (a map of name lists, nothing else reads it by anything but a
// VM id) under snapSourceKey(sid): the first element names the source VM, the
// rest is the selection: secret names, snapAll (every team secret), snapDefault
// (the team's inject defaults) or snapNoEnv (the run had no team secrets). No
// record (an import, a share copy, a snapshot of before this was kept, a lost
// write) is unknown.

const (
	snapSourcePrefix = "snap:"
	snapSourceVM     = "vm="
	snapAll          = "@all"
	snapNoEnv        = "@no_env"
)

func snapSourceKey(sid string) string { return snapSourcePrefix + sid }

// snapSourceRecord is what captureSnapshot stores for a snapshot of vmID taken
// while its run held the secrets names (concrete names, expanded when the record
// is made), or none of the team's (noEnv).
func snapSourceRecord(vmID string, names []string, noEnv bool) []string {
	rec := []string{snapSourceVM + vmID}
	if noEnv {
		return append(rec, snapNoEnv)
	}
	return append(rec, names...)
}

// snapshotRunNoEnv certifies only protected-from-creation lineage and a
// known-empty team run. It makes no claim about arbitrary files on tenant disks.
func (s *server) snapshotRunNoEnv(vmID string, vm VM) bool {
	st := s.store.Snapshot()
	names, known := runNamesOf(st, vmID)
	labels, _ := vm["labels"].(map[string]any)
	return protectedRunPolicy(st, vmID, vm) && (str(vm, "state") == "running" || str(vm, "state") == "stopped") && labels[protectedLabel] == "1" &&
		st.VMTransitions[vmID] == nil && !s.envNeverLanded(vmID) && known && len(names) == 0
}

func snapProtectedKey(sid string) string { return snapSourcePrefix + "protected:" + sid }
func snapPolicyKey(sid string) string    { return snapSourcePrefix + "policy:" + sid }

// Sticky no_env policy and permission to restore RAM are independent records.
func (s *server) snapshotSourceProtected(sid string) bool {
	return snapshotSourceProtectedIn(s.store.Snapshot(), sid)
}

func snapshotSourceProtectedIn(st *State, sid string) bool {
	rec, cert := st.VMSecrets[snapSourceKey(sid)], st.VMSecrets[snapProtectedKey(sid)]
	return sid != "" && len(rec) == 2 && strings.HasPrefix(rec[0], snapSourceVM) && rec[1] == snapNoEnv &&
		len(cert) == 1 && cert[0] == rec[0]
}

// snapshotSourceNoEnv preserves disk policy even when run inventory is unknown.
// The legacy strict source record remains a policy signal, never a certificate.
func (s *server) snapshotSourceNoEnv(sid string) bool {
	return snapshotSourceNoEnvIn(s.store.Snapshot(), sid)
}

func snapshotSourceNoEnvIn(st *State, sid string) bool {
	if sid == "" {
		return false
	}
	policy := st.VMSecrets[snapPolicyKey(sid)]
	rec := st.VMSecrets[snapSourceKey(sid)]
	return (len(policy) == 1 && strings.HasPrefix(policy[0], snapSourceVM)) ||
		(len(rec) == 2 && strings.HasPrefix(rec[0], snapSourceVM) && rec[1] == snapNoEnv)
}

// recordSnapshotSource runs under the source lifecycle lock. Sticky disk policy
// is durable before publication, including when inventory or delivery is unknown.
// Only positive, known-empty protected lineage can certify copied memory.
func (s *server) recordSnapshotSource(ctx context.Context, vmID, team, sid string) error {
	if sid == "" {
		return errors.New("snapshot prepare omitted id")
	}
	var vm VM
	if err := s.boatd.call(ctx, "GET", vmPath(vmID), nil, &vm); err != nil {
		log.Printf("record snapshot %s: read %s: %v; memory is uncertified", sid, vmID, err)
		vm = nil
	}
	if vm != nil && (str(vm, "id") != vmID || str(vm, "team") != team) {
		vm = nil
	}
	st := s.store.Snapshot()
	_, marked := st.NoEnvRuns[vmID]
	sticky := marked || labeledNoEnv(vm)
	names, known := runNamesOf(st, vmID)
	_, never := st.VMSecrets[envNeverKey(vmID)]
	var rec []string
	if known && !never && st.VMTransitions[vmID] == nil {
		noSecrets := sticky && len(names) == 0
		if !noSecrets && slices.Contains(s.vmSecretsOf(vmID, vm), defaultSelection) {
			names = append([]string{defaultSelection}, names...)
		}
		rec = snapSourceRecord(vmID, names, noSecrets)
	}
	certified := sticky && s.snapshotRunNoEnv(vmID, vm)
	return s.store.UpdateOrApply(func(cur *State) (bool, error) {
		delete(cur.VMSecrets, snapSourceKey(sid))
		delete(cur.VMSecrets, snapProtectedKey(sid))
		delete(cur.VMSecrets, snapPolicyKey(sid))
		if rec != nil {
			cur.VMSecrets[snapSourceKey(sid)] = rec
		}
		if sticky {
			cur.VMSecrets[snapPolicyKey(sid)] = []string{snapSourceVM + vmID}
		}
		if certified {
			cur.VMSecrets[snapProtectedKey(sid)] = []string{snapSourceVM + vmID}
		}
		return sticky, nil
	})
}

func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// copyShareRecord gives the snapshot a share import created the record of the
// shared snapshot, when the sender's record is there: what the imported memory
// holds is what the sender's run held. Only a same-team share keeps a list of
// names (they name the same team's secrets); across teams only "no team secrets"
// carries over, because the sender's names mean nothing in the receiver's team
// and the memory holds secrets the receiver's selection cannot cover. The copy
// belongs to no VM (its first element names none), so the sender's VM being
// deleted does not drop it; deleting the snapshot does.
func (s *server) copyShareRecord(sh *SnapShare, team, sid string) error {
	if sh == nil || sid == "" {
		return nil
	}
	st := s.store.Snapshot()
	src := st.VMSecrets[snapSourceKey(sh.SnapshotID)]
	sticky := snapshotSourceNoEnvIn(st, sh.SnapshotID)
	certified := snapshotSourceProtectedIn(st, sh.SnapshotID)
	var rec []string
	if len(src) > 0 && strings.HasPrefix(src[0], snapSourceVM) &&
		(sh.TeamID == team || (len(src) == 2 && src[1] == snapNoEnv)) &&
		!(slices.Contains(src[1:], snapNoEnv) && len(src) > 2) {
		rec = append([]string{snapSourceVM}, src[1:]...)
	}
	if rec == nil && !sticky {
		return nil
	}
	return s.store.Update(func(cur *State) error {
		if rec != nil {
			cur.VMSecrets[snapSourceKey(sid)] = rec
		}
		delete(cur.VMSecrets, snapProtectedKey(sid))
		if sticky {
			cur.VMSecrets[snapPolicyKey(sid)] = []string{snapSourceVM}
		}
		if certified {
			cur.VMSecrets[snapProtectedKey(sid)] = []string{snapSourceVM}
		}
		return nil
	})
}

// forgetSnapshotSource drops the record of a deleted snapshot.
func (s *server) forgetSnapshotSource(sid string) {
	st := s.store.Snapshot()
	if st.VMSecrets[snapSourceKey(sid)] == nil && st.VMSecrets[snapPolicyKey(sid)] == nil && st.VMSecrets[snapProtectedKey(sid)] == nil {
		return
	}
	if err := s.store.Apply(func(st *State) error {
		delete(st.VMSecrets, snapSourceKey(sid))
		delete(st.VMSecrets, snapProtectedKey(sid))
		delete(st.VMSecrets, snapPolicyKey(sid))
		return nil
	}); err != nil {
		log.Printf("forget the secret selection of snapshot %s: %v", sid, err)
	}
}

// forgetSnapshotSourcesOf drops the records of the snapshots of a deleted VM
// (boatd deletes them with it); called inside forgetVM's store write.
func forgetSnapshotSourcesOf(st *State, vmID string) {
	for k, v := range st.VMSecrets {
		if strings.HasPrefix(k, snapSourcePrefix) && len(v) > 0 && v[0] == snapSourceVM+vmID {
			delete(st.VMSecrets, k)
		}
	}
}

// selectionNames expands a selection to the team secret names it picks.
func selectionNames(st *State, team string, sel []string) map[string]bool {
	out := map[string]bool{}
	for _, sec := range st.Secrets {
		if sec.TeamID != team {
			continue
		}
		if sel == nil || slices.Contains(sel, snapAll) || slices.Contains(sel, sec.Name) ||
			(sec.Inject && slices.Contains(sel, defaultSelection)) {
			out[sec.Name] = true
		}
	}
	return out
}

// snapshotNeedsFreshBoot: whether a VM created from snapshot sid with the
// selection req must cold-boot. It must unless req covers everything the
// source run could have held; with an unknown source (an import, a snapshot of
// before records were kept, a lost write) that is every secret the team has.
func (s *server) snapshotNeedsFreshBoot(team, sid string, req []string) bool {
	if sid == "" {
		return false
	}
	st := s.store.Snapshot()
	rec, known := st.VMSecrets[snapSourceKey(sid)]
	want := map[string]bool{}
	if snapshotSourceNoEnvIn(st, sid) {
		return !snapshotSourceProtectedIn(st, sid)
	}
	if known && len(rec) > 0 {
		for _, t := range rec[1:] {
			switch t {
			case defaultSelection:
				if !slices.Contains(req, defaultSelection) {
					return true // the defaults at the time are only matched by their own marker
				}
			case snapAll:
				for n := range selectionNames(st, team, nil) {
					want[n] = true
				}
			default:
				want[t] = true
			}
		}
	} else {
		// Unknown: the run may have held any team secret.
		want = selectionNames(st, team, nil)
	}
	have := selectionNames(st, team, req)
	for n := range want {
		if !have[n] {
			return true
		}
	}
	return false
}
