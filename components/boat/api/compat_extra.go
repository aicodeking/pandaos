package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// boat.dev operations mapped onto native handlers in council round 3:
// telemetry, AI usage and access, environment vars, files replace and
// upload-url, artifacts, share and recover. docs/boat.md lists the rest.

// ---- telemetry (otel.go) ----

func compatTelemetry(res *nativeAnswer) any {
	var v struct {
		Configured  bool      `json:"configured"`
		Endpoint    string    `json:"endpoint"`
		HeaderNames []string  `json:"header_names"`
		Updated     time.Time `json:"updated"`
	}
	json.Unmarshal(res.body.Bytes(), &v)
	if !v.Configured {
		return nil
	}
	return map[string]any{"endpoint": v.Endpoint, "headers": v.HeaderNames, "lastExportAt": nil,
		"lastError": nil, "createdAt": v.Updated}
}

func (s *server) compatGetTelemetry(w http.ResponseWriter, r *http.Request, p *principal) {
	res := s.callNative(r, p, "GET /v1/telemetry", s.getTelemetry, "", nil)
	if !res.ok() {
		res.relay(w)
		return
	}
	writeCompat(w, 200, "telemetry", map[string]any{"telemetry": compatTelemetry(res)})
}

func (s *server) compatPutTelemetry(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Endpoint string            `json:"endpoint"`
		Headers  map[string]string `json:"headers"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Headers) > 20 {
		writeErr(w, r, 400, "invalid_request", "at most 20 headers")
		return
	}
	res := s.callNative(r, p, "PUT /v1/telemetry", s.putTelemetry, "", body)
	if !res.ok() {
		res.relay(w)
		return
	}
	// This server sends no test export; say so instead of claiming success.
	writeCompat(w, 200, "telemetry", map[string]any{"telemetry": compatTelemetry(res),
		"test": map[string]any{"ok": false, "status": nil, "message": "not tested: this server sends no test export"}})
}

func (s *server) compatDeleteTelemetry(w http.ResponseWriter, r *http.Request, p *principal) {
	res := s.callNative(r, p, "DELETE /v1/telemetry", s.deleteTelemetry, "", nil)
	if !res.ok() {
		res.relay(w)
		return
	}
	writeCompat(w, 200, "telemetry", map[string]any{"telemetry": nil})
}

// ---- AI usage and access (aiproxy.go) ----

// compatSince parses ?days (1-365, default 30) into the first day counted.
func compatSince(w http.ResponseWriter, r *http.Request) (time.Time, bool) {
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 365 {
			writeErr(w, r, 400, "invalid_query", "days must be 1-365")
			return time.Time{}, false
		}
		days = n
	}
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1-days), true
}

// compatAIRows renders native usage rows (one per VM, provider and model,
// dated by their last use) as boat.dev's per-day rows and totals.
func compatAIRows(rows []*AIUsageRow, since time.Time) ([]map[string]any, map[string]any) {
	out := []map[string]any{}
	var req, in, outTok int64
	for _, u := range rows {
		if u.Updated.Before(since) {
			continue
		}
		req, in, outTok = req+u.Requests, in+u.InputTokens, outTok+u.OutputTokens
		out = append(out, map[string]any{"day": u.Updated.UTC().Format(time.DateOnly), "credential": u.Provider,
			"model": u.Model, "requests": u.Requests, "errors": 0, "inputTokens": u.InputTokens,
			"outputTokens": u.OutputTokens, "cacheReadTokens": 0, "cacheWriteTokens": 0})
	}
	return out, map[string]any{"requests": req, "errors": 0, "inputTokens": in, "outputTokens": outTok,
		"cacheReadTokens": 0, "cacheWriteTokens": 0}
}

func (s *server) compatAIUsage(w http.ResponseWriter, r *http.Request, p *principal) {
	since, ok := compatSince(w, r)
	if !ok {
		return
	}
	res := s.callNative(r, p, "GET /v1/ai/usage", s.teamAIUsage, "", nil)
	if !res.ok() {
		res.relay(w)
		return
	}
	var all struct {
		Usage []*AIUsageRow `json:"usage"`
	}
	json.Unmarshal(res.body.Bytes(), &all)
	byVM := map[string][]*AIUsageRow{}
	var vms []string
	for _, u := range all.Usage {
		if byVM[u.VMID] == nil {
			vms = append(vms, u.VMID)
		}
		byVM[u.VMID] = append(byVM[u.VMID], u)
	}
	ids := s.compatIDs(p.Team.ID, vms)
	st := s.store.Snapshot()
	sandboxes := []map[string]any{}
	for _, vm := range vms {
		rows, totals := compatAIRows(byVM[vm], since)
		if len(rows) == 0 {
			continue
		}
		sandboxes = append(sandboxes, map[string]any{"sandboxId": nullIfEmpty(ids[vm]), "name": nil,
			"blocked": st.AIVMs[vm] != nil && st.AIVMs[vm].Blocked, "usage": rows, "totals": totals, "dollars": 0})
	}
	rows, totals := compatAIRows(all.Usage, since)
	writeCompat(w, 200, "ai.usage", map[string]any{"accountId": p.Team.ID, "profileId": nil,
		"since": since.Format(time.DateOnly), "sandboxes": sandboxes, "usage": rows, "totals": totals, "dollars": 0})
}

func (s *server) compatSandboxAIUsage(w http.ResponseWriter, r *http.Request, p *principal) {
	since, ok := compatSince(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	res := s.callNative(r, p, "GET /v1/vms/{id}/ai-usage", s.vmAIUsage, id, nil)
	if !res.ok() {
		res.relay(w)
		return
	}
	var v struct {
		Usage []*AIUsageRow `json:"usage"`
	}
	json.Unmarshal(res.body.Bytes(), &v)
	rows, totals := compatAIRows(v.Usage, since)
	ai := s.store.Snapshot().AIVMs[id]
	writeCompat(w, 200, "sandbox.ai_usage", map[string]any{"sandboxId": s.compatID(p.Team.ID, id),
		"blocked": ai != nil && ai.Blocked, "proxied": s.cfg.AIProxyURL != "" && !s.store.Snapshot().AIProxyOff[p.Team.ID],
		"credentialProfileId": nil, "since": since.Format(time.DateOnly), "usage": rows, "totals": totals, "dollars": 0})
}

func (s *server) compatSetAIAccess(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Blocked *bool `json:"blocked"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Blocked == nil {
		writeErr(w, r, 400, "invalid_request", "blocked is required")
		return
	}
	id := r.PathValue("id")
	res := s.callNative(r, p, "PUT /v1/vms/{id}/ai-access", s.setAIAccess, id, map[string]any{"enabled": !*body.Blocked})
	if !res.ok() {
		res.relay(w)
		return
	}
	writeCompat(w, 200, "sandbox.ai_access", map[string]any{"success": true, "sandboxId": s.compatID(p.Team.ID, id), "blocked": *body.Blocked})
}

// ---- environment vars (environments.go) ----

// compatEnvVar sets (value != nil) or removes one var through the native PUT,
// which validates names and sizes.
// ponytail: read-modify-write of the whole map; two concurrent var writes to
// one environment can lose one. Add a per-var native route if that matters.
func (s *server) compatEnvVar(w http.ResponseWriter, r *http.Request, p *principal, value *string) {
	e, ok := s.pathEnv(w, r, p)
	if !ok {
		return
	}
	key := r.PathValue("key")
	vars := maps.Clone(e.Env)
	if vars == nil {
		vars = map[string]string{}
	}
	if value != nil {
		vars[key] = *value
	} else if _, had := vars[key]; !had {
		writeErr(w, r, 404, "not_found", "variable not found")
		return
	} else {
		delete(vars, key)
	}
	res := s.callNative(r, p, "PUT /v1/environments/{id}", s.updateEnv, e.ID, map[string]any{"env": vars})
	if !res.ok() {
		res.relay(w)
		return
	}
	var cur struct {
		Version int `json:"version"`
	}
	json.Unmarshal(res.body.Bytes(), &cur)
	writeJSON(w, 200, map[string]any{"success": true, "versionId": envUUID(e.ID, cur.Version), "versionNumber": cur.Version})
}

func (s *server) compatSetEnvVar(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Value *string `json:"value"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Value == nil {
		writeErr(w, r, 400, "invalid_request", "value is required")
		return
	}
	s.compatEnvVar(w, r, p, body.Value)
}

func (s *server) compatDeleteEnvVar(w http.ResponseWriter, r *http.Request, p *principal) {
	s.compatEnvVar(w, r, p, nil)
}

// ---- files: replace, upload-url, artifacts ----

var errFileTooBig = errors.New("file is too large")

// readVMFile reads up to max bytes of a guest file (errFileTooBig past that).
func (s *server) readVMFile(r *http.Request, id, path string, max int64) ([]byte, error) {
	resp, err := s.boatd.do(r.Context(), "GET", vmPath(id, "files")+"?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return nil, &boatdError{Status: resp.StatusCode, Body: b}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err == nil && int64(len(data)) > max {
		return nil, errFileTooBig
	}
	return data, err
}

func (s *server) compatReplace(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Paths    []string `json:"paths"`
		Pattern  string   `json:"pattern"`
		NewValue *string  `json:"newValue"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Paths) < 1 || len(body.Paths) > 100 || body.Pattern == "" || body.NewValue == nil {
		writeErr(w, r, 400, "invalid_request", "paths (1-100), pattern and newValue are required")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	id, pat, nv := r.PathValue("id"), []byte(body.Pattern), []byte(*body.NewValue)
	results := []map[string]any{}
	all := true
	for _, f := range body.Paths {
		path := s.compatPath(f)
		res := map[string]any{"file": path, "success": false}
		data, err := s.readVMFile(r, id, path, compatMaxFile)
		n := bytes.Count(data, pat)
		if err == nil && n > 0 {
			err = s.putVMFile(r, id, path, bytes.NewReader(bytes.ReplaceAll(data, pat, nv)))
		}
		if err != nil {
			res["error"], all = replaceErr(err), false
		} else {
			res["success"], res["replacements"] = true, n
		}
		results = append(results, res)
	}
	writeCompat(w, 200, "file.replaced", map[string]any{"success": all, "results": results})
}

func replaceErr(err error) string {
	var be *boatdError
	if errors.As(err, &be) {
		if be.Status == 404 {
			return "file not found"
		}
		return be.message()
	}
	return err.Error()
}

// fileUploadPath serves upload URLs: a fileGrant with Op "put" (download URLs
// have none, so neither token works for the other).
const fileUploadPath = "/api/v1/file-uploads/"

func (s *server) compatUploadURL(w http.ResponseWriter, r *http.Request, p *principal) {
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
	file := s.compatPath(body.Path)
	if strings.TrimSpace(body.Path) == "" || ttl < 1 || ttl > fileURLMaxTTL || !validFilePath(file) {
		writeErr(w, r, 400, "invalid_request", "path is required; ttlSeconds 1-3600")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	exp := time.Now().Add(time.Duration(ttl) * time.Second).Truncate(time.Second)
	tok := s.signFileGrant(fileGrant{Team: p.Team.ID, VM: r.PathValue("id"), Path: file, Exp: exp.Unix(), Op: "put"})
	writeCompat(w, 200, "file.upload_url", map[string]any{"url": s.publicBase(r) + fileUploadPath + tok,
		"method": "PUT", "path": file, "expiresAt": exp.UTC()})
}

// compatFileUpload takes an upload URL's PUT: no API key, the token is the grant.
func (s *server) compatFileUpload(w http.ResponseWriter, r *http.Request) {
	info(r).audit = true
	ip := r.Header.Get("X-Forwarded-For") // Caddy sets it; the listener is loopback
	if ip == "" {
		ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	if ok, _ := s.limiter.allow("fileul:" + ip); !ok {
		writeErr(w, r, 429, "rate_limited", "rate limit exceeded")
		return
	}
	g, ok := s.openFileGrant(r.PathValue("token"), time.Now())
	if !ok || g.Op != "put" {
		writeErr(w, r, 404, "not_found", "upload URL not found or expired")
		return
	}
	vm, err := s.vmOf(r, g.VM)
	if err != nil || str(vm, "team") != g.Team {
		writeErr(w, r, 404, "not_found", "upload URL not found or expired")
		return
	}
	release, ok := s.acquireOr429(w, r, s.uploads, "fileul:"+g.Team)
	if !ok {
		return
	}
	defer release()
	spool, n, ok := s.spoolUpload(w, r)
	if !ok {
		return
	}
	defer spool.Close()
	if err := s.putVMFile(r, g.VM, g.Path, spool); err != nil {
		writeUpstream(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": g.Path, "bytes": n})
}

const compatMaxArtifact = 50 << 20

// compatArtifact: GET a file (up to 50 MiB) as raw bytes.
func (s *server) compatArtifact(w http.ResponseWriter, r *http.Request, p *principal) {
	q := r.URL.Query().Get("path")
	if q == "" {
		writeErr(w, r, 400, "invalid_query", "path is required")
		return
	}
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	data, err := s.readVMFile(r, r.PathValue("id"), s.compatPath(q), compatMaxArtifact)
	var be *boatdError
	switch {
	case err == nil:
	case errors.As(err, &be) && be.Status == 404:
		writeErr(w, r, 404, "not_found", "file not found")
		return
	case errors.Is(err, errFileTooBig):
		writeErr(w, r, 413, "file_too_large", "artifact is over 50 MiB; use files/download-url")
		return
	default:
		writeUpstream(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(200)
	w.Write(data)
}

// ---- share and recover ----

// compatShare: every sandbox here is already usable by all of its team, so
// sharing with the organization is a no-op that answers the sandbox. Making
// one private (unshare) has no equivalent and is not mapped.
func (s *server) compatShare(w http.ResponseWriter, r *http.Request, p *principal) {
	vm, ok := s.ownVM(w, r, p)
	if !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"sandbox": s.sandbox(vm)})
}

// compatRecover power-cycles a running VM: a forced stop (no snapshot; the
// disk is kept) then a start.
func (s *server) compatRecover(w http.ResponseWriter, r *http.Request, p *principal) {
	vm, ok := s.ownVM(w, r, p)
	if !ok {
		return
	}
	if str(vm, "state") != "running" {
		writeErr(w, r, 409, "no_machine", "the sandbox is not running; resume it instead")
		return
	}
	id := r.PathValue("id")
	if res := s.callNative(r, p, "POST /v1/vms/{id}/stop", s.stopVM, id, map[string]any{"force": true}); !res.ok() {
		res.relay(w)
		return
	}
	if res := s.callNative(r, p, "POST /v1/vms/{id}/start", s.startVM, id, nil); !res.ok() {
		res.relay(w)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "type": "sandbox.recover", "status": "recovering",
		"message": "the machine was power-cycled; the disk was kept"})
}
