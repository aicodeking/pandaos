package main

import (
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
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
// the VM and the path, and expires after ttlSeconds (default 900, at most
// 3600). Served at /api/v1/file-downloads/<token>, which Caddy routes with
// the rest of /api/v1; the audit log never shows the token.

const (
	fileURLDefaultTTL = 900
	fileURLMaxTTL     = 3600
	fileDownloadPath  = "/api/v1/file-downloads/"
)

type fileGrant struct {
	Team string `json:"t"`
	VM   string `json:"v"`
	Path string `json:"p"`
	Exp  int64  `json:"e"`
	Op   string `json:"o,omitempty"` // "put": an upload URL (compat_extra.go)
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
	return g, true
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
	tok := s.signFileGrant(fileGrant{Team: p.Team.ID, VM: r.PathValue("id"), Path: file, Exp: exp.Unix()})
	writeCompat(w, 200, "file.download_url", map[string]any{"url": s.publicBase(r) + fileDownloadPath + tok,
		"method": "GET", "path": file, "expiresAt": exp.UTC()})
}

// compatFileDownload serves a download URL: no API key, the token is the grant.
func (s *server) compatFileDownload(w http.ResponseWriter, r *http.Request) {
	info(r).audit = true
	ip := r.Header.Get("X-Forwarded-For") // Caddy sets it; the listener is loopback
	if ip == "" {
		ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	if ok, _ := s.limiter.allow("filedl:" + ip); !ok {
		writeErr(w, r, 429, "rate_limited", "rate limit exceeded")
		return
	}
	g, ok := s.openFileGrant(r.PathValue("token"), time.Now())
	if !ok || g.Op != "" {
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
