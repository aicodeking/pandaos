package main

import (
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// boat.dev's files/find and files/download-url.
//
// find: the native content search (GET /v1/vms/{id}/files/search?content=1,
// ripgrep in the guest), as {file, line, content} matches.
//
// download-url: a short-lived signed URL, GET with no Authorization, that
// streams the file as raw bytes with no size limit. The token is
// base64url({t,v,p,e}) "." hex(HMAC-SHA256(fileKey, that)), fileKey derived
// from the API's master key for this purpose only. It is bound to the team,
// the VM, the path and the minting key (dead with that key or its member),
// and expires after ttlSeconds (default 900, at most
// 3600). Served at /api/v1/file-downloads/<token>, which Caddy routes with
// the rest of /api/v1; the audit log never shows the token.

const (
	fileURLDefaultTTL = 900
	fileURLMaxTTL     = 3600
	fileDownloadPath  = "/api/v1/file-downloads/"
)

type fileGrant struct {
	Team   string `json:"t"`
	VM     string `json:"v"`
	Path   string `json:"p"`
	Exp    int64  `json:"e"`
	Op     string `json:"o,omitempty"` // "put": an upload URL (compat_extra.go); "snap": a snapshot chunk
	Key    string `json:"k"`           // the minting API key: the URL dies with it (keyAlive)
	KeyGen int64  `json:"g,omitempty"` // secret generation of Key when minted
	// Scope the minting route required; the key must still hold it at use
	// ("" on older URLs: exec, what every minting route requires).
	Scope string `json:"s,omitempty"`
}

// mintGrant signs g for p as minted by r's route (its key and scope).
func (s *server) mintGrant(r *http.Request, p *principal, g fileGrant) string {
	g.Team, g.Key, g.KeyGen, g.Scope = p.Team.ID, p.Key.ID, p.KeyGen, s.routeScopes[info(r).pattern]
	return s.signFileGrant(g)
}

// mintFileURL rechecks authority after body reads and upstream work against the
// current snapshot; no persistent state is needed for a file grant.
// Keep mintGrant's snapshot-chunk contract separate from these file routes.
func (s *server) mintFileURL(w http.ResponseWriter, r *http.Request, p *principal, g *fileGrant) string {
	vm, err := s.vmOf(r, g.VM)
	if err != nil || p == nil || p.Team == nil || str(vm, "team") != p.Team.ID {
		writeErr(w, r, 404, "not_found", "vm not found")
		return ""
	}
	// The recheck reads one immutable snapshot and takes no store lock: the writer
	// mutex is held across Update's fsync, and a read-only check must not queue
	// behind it. A later revocation is enforced when the link is used
	// (openFileGrant) and by watchAlive on open transfers.
	st, now := s.store.Snapshot(), time.Now()
	scope := orDefault(s.routeScopes[info(r).pattern], "exec")
	current := currentKeyPrincipal(st, p, now)
	if p.VMID != "" && p.VMID == g.VM && p.Key != nil && p.has(scope) && keyAliveIn(st, p.Key.ID, p.Team.ID, scope, now) {
		current = p
	}
	if current == nil || !current.has(scope) || !p.has(scope) {
		writeErr(w, r, 403, "forbidden", "credential no longer permits file URL issuance")
		return ""
	}
	for _, key := range []*APIKey{p.Key, current.Key} {
		if key.Expires != nil && key.Expires.Unix() < g.Exp {
			g.Exp = key.Expires.Unix()
		}
	}
	if !now.Before(time.Unix(g.Exp, 0)) {
		writeErr(w, r, 403, "forbidden", "credential no longer permits file URL issuance")
		return ""
	}
	return s.mintGrant(r, current, *g)
}

func (s *server) signFileGrant(g fileGrant) string {
	b, _ := json.Marshal(g)
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + signHex(s.keys.fileKey, payload)
}

func (s *server) openFileGrant(token string, now time.Time) (fileGrant, bool) {
	var g fileGrant
	payload, sig, ok := strings.Cut(token, ".")
	if !ok || !hmac.Equal([]byte(signHex(s.keys.fileKey, payload)), []byte(sig)) {
		return g, false
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || json.Unmarshal(b, &g) != nil || !now.Before(time.Unix(g.Exp, 0)) {
		return g, false
	}
	return g, keyAliveGenIn(s.store.Snapshot(), g.Key, g.KeyGen, g.Team, orDefault(g.Scope, "exec"), now)
}

// publicBase is this API's public base URL: -public-url, else what the request
// came in on (Caddy sets X-Forwarded-Proto).
func (s *server) publicBase(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	scheme := "http"
	if r.Header.Get("X-Forwarded-Proto") == "https" || r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *server) compatDownloadURL(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Path       string `json:"path"`
		TTLSeconds *int   `json:"ttlSeconds"`
	}
	if !decode(w, r, &body) {
		return
	}
	ttl := fileURLDefaultTTL
	if body.TTLSeconds != nil {
		ttl = *body.TTLSeconds
	}
	if strings.TrimSpace(body.Path) == "" || ttl < 1 || ttl > fileURLMaxTTL {
		writeErr(w, r, 400, "invalid_request", "path is required; ttlSeconds 1-3600")
		return
	}
	file := s.compatPath(body.Path)
	// The file must exist now; the URL then serves it as it is when fetched.
	res := s.callNative(r, p, "GET /v1/vms/{id}/files/info", s.fileInfo, r.PathValue("id"), nil, "?", url.Values{"path": {file}}.Encode())
	if !res.ok() {
		res.relay(w)
		return
	}
	exp := time.Now().Add(time.Duration(ttl) * time.Second).Truncate(time.Second)
	grant := fileGrant{VM: r.PathValue("id"), Path: file, Exp: exp.Unix()}
	tok := s.mintFileURL(w, r, p, &grant)
	if tok == "" {
		return
	}
	exp = time.Unix(grant.Exp, 0)
	writeCompat(w, 200, "file.download_url", map[string]any{"url": s.publicBase(r) + fileDownloadPath + tok,
		"method": "GET", "path": file, "expiresAt": exp.UTC()})
}

// compatFileDownload serves a download URL: no API key, the token is the grant.
func (s *server) compatFileDownload(w http.ResponseWriter, r *http.Request) {
	info(r).audit = true
	ip := trustedClientIP(r)
	if ok, _ := s.limiter.allow("filedl:" + ip); !ok {
		writeErr(w, r, 429, "rate_limited", "rate limit exceeded")
		return
	}
	g, ok := s.openFileGrant(r.PathValue("token"), time.Now())
	if !ok || (g.Op != "" && g.Op != "snap") {
		writeErr(w, r, 404, "not_found", "download URL not found or expired")
		return
	}
	vm, err := s.vmOf(r, g.VM)
	if err != nil || str(vm, "team") != g.Team {
		writeErr(w, r, 404, "not_found", "download URL not found or expired")
		return
	}
	release, ok := s.acquireOr429(w, r, s.streams, "filedl:"+g.Team)
	if !ok {
		return
	}
	defer release()
	// The link's minting key must stay alive for the whole transfer.
	r, stop := s.watchAlive(r, func(st *State, now time.Time) bool {
		return keyAliveGenIn(st, g.Key, g.KeyGen, g.Team, orDefault(g.Scope, "exec"), now)
	})
	defer stop()
	if g.Op == "snap" { // a snapshot chunk URL (compatSnapshotDownload); Path is the snapshot id
		if !s.grantMaySnapshot(w, r, g) {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		s.relayDownload(w, r, g.VM, g.Path)
		return
	}
	resp, err := s.boatd.do(r.Context(), "GET", vmPath(g.VM, "files")+"?path="+url.QueryEscape(g.Path), nil, "")
	if err != nil {
		writeUpstream(w, r, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		writeErr(w, r, 404, "not_found", "file not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(path.Base(g.Path), `"`, "")+`"`)
	w.Header().Set("Cache-Control", "no-store")
	if n := resp.Header.Get("Content-Length"); n != "" {
		if _, err := strconv.ParseInt(n, 10, 64); err == nil {
			w.Header().Set("Content-Length", n)
		}
	}
	w.WriteHeader(200)
	io.Copy(w, resp.Body)
}

func (s *server) compatFindInFiles(w http.ResponseWriter, r *http.Request, p *principal) {
	q := r.URL.Query()
	pattern := q.Get("pattern")
	if pattern == "" {
		writeErr(w, r, 400, "invalid_query", "pattern is required")
		return
	}
	nq := url.Values{"path": {s.compatPath(orDefault(q.Get("path"), "/home/user"))}, "q": {pattern}, "content": {"1"}, "limit": {"1000"}}
	if q.Get("regex") == "true" || q.Get("regex") == "1" {
		nq.Set("regex", "1")
	}
	res := s.fileQuery(r, p, "GET /v1/vms/{id}/files/search", s.searchFiles, nq)
	if !res.ok() {
		res.relay(w)
		return
	}
	var out struct {
		Matches []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Text string `json:"text"`
		} `json:"matches"`
		Truncated bool `json:"truncated"`
	}
	json.Unmarshal(res.body.Bytes(), &out)
	matches := make([]map[string]any, 0, len(out.Matches))
	for _, m := range out.Matches {
		matches = append(matches, map[string]any{"file": m.Path, "line": m.Line, "content": m.Text})
	}
	writeCompat(w, 200, "file.find", map[string]any{"success": true, "matches": matches, "truncated": out.Truncated})
}

// grantMaySnapshot re-checks the builder gate when a snapshot chunk URL is used:
// the snapshot may have become an environment's build snapshot, or its VM a
// builder, since the URL was minted. It answers (and reports false) when not.
func (s *server) grantMaySnapshot(w http.ResponseWriter, r *http.Request, g fileGrant) bool {
	st := s.store.Snapshot()
	k := st.Keys[g.Key]
	team := st.Teams[g.Team]
	if k == nil || team == nil || team.role(k.UserID) == "" {
		writeErr(w, r, 404, "not_found", "download URL not found or expired")
		return false
	}
	role := team.role(k.UserID)
	p := &principal{Key: k, User: st.Users[k.UserID], Team: team, Role: role, Scopes: effectiveScopes(k.Scopes, role)}
	var list struct {
		Snapshots []map[string]any `json:"snapshots"`
	}
	if err := s.boatd.call(r.Context(), "GET", "/v1/snapshots?team="+url.QueryEscape(g.Team), nil, &list); err != nil {
		writeUpstream(w, r, err)
		return false
	}
	for _, sn := range list.Snapshots {
		if str(sn, "id") == g.Path && str(sn, "team") == g.Team {
			return !s.refuseBuilderSnapshot(w, r, p, sn)
		}
	}
	writeErr(w, r, 404, "not_found", "download URL not found or expired")
	return false
}
