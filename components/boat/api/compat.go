package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// boat.dev compatibility: /api/v1 mirrors the most used part of boat.dev's
// public API (docs.boat.dev/api/v1.md and its reference pages, read
// 2026-11-09) on top of the native /v1 handlers, so a boat.dev client pointed
// at https://<this host>/api/v1 can create, list, get, stop, resume, fork and
// delete sandboxes, run commands, read and write files and read /limits.
// Responses carry boat.dev's {ok, type, …} envelope and errors its
// {ok:false, type:"sandbox.error", code, message, error:{…}, requestId}.
// docs/boat.md lists what is mapped and what is not.

const compatPrefix = "/api/v1/"

func (s *server) compatRoutes() {
	s.route("POST /api/v1/sandboxes", "vms:write", s.compatCreate)
	s.route("GET /api/v1/sandboxes", "vms:read", s.compatList)
	s.route("GET /api/v1/sandboxes/{id}", "vms:read", s.compatGet)
	s.route("PATCH /api/v1/sandboxes/{id}", "vms:write", s.compatUpdate)
	s.route("DELETE /api/v1/sandboxes/{id}", "vms:write", s.compatDelete)
	s.route("GET /api/v1/deletion-operations/{opid}", "vms:read", s.compatDeletion)
	s.route("POST /api/v1/sandboxes/{id}/stop", "vms:write", s.compatStop)
	s.route("GET /api/v1/sandboxes/{id}/stops/{stopid}", "vms:read", s.compatGetStop)
	s.route("POST /api/v1/sandboxes/{id}/resume", "vms:write", s.compatResume)
	s.route("POST /api/v1/sandboxes/{id}/fork", "vms:write", s.compatFork)
	s.route("GET /api/v1/limits", "vms:read", s.compatLimits)
	s.route("GET /api/v1/me", "", s.compatMe)
	s.route("GET /api/v1/sandboxes/{id}/usage", "vms:read", s.compatUsage)
	// prompts (compat_prompts.go)
	s.route("POST /api/v1/sandboxes/{id}/prompt", "exec", s.compatPrompt)
	s.route("GET /api/v1/sandboxes/{id}/prompts/{pid}", "exec", s.compatPromptRun)
	s.route("GET /api/v1/sandboxes/{id}/events", "exec", s.compatEvents)
	s.route("GET /api/v1/sandboxes/{id}/conversations", "exec", s.compatConversations)
	s.route("POST /api/v1/sandboxes/{id}/steer", "exec", s.compatSteer)
	s.route("POST /api/v1/sandboxes/{id}/interrupt", "exec", s.compatInterrupt)
	// commands (compat_exec.go, compat_cmds.go)
	s.route("POST /api/v1/sandboxes/{id}/commands", "exec", s.compatExec)
	s.route("GET /api/v1/sandboxes/{id}/commands/{pid}", "exec", s.compatCommandStatus)
	s.route("DELETE /api/v1/sandboxes/{id}/commands/{pid}", "exec", s.compatStopCommand)
	s.route("GET /api/v1/sandboxes/{id}/command-history", "exec", s.compatCommandHistory)
	s.route("GET /api/v1/sandboxes/{id}/command-history/{cid}", "exec", s.compatCommandInfo)
	s.route("GET /api/v1/sandboxes/{id}/command-history/{cid}/logs", "exec", s.compatCommandLogs)
	s.route("GET /api/v1/sandboxes/{id}/processes", "exec", s.compatProcesses)
	// files (compat_exec.go, compat_files.go)
	s.route("GET /api/v1/sandboxes/{id}/files", "exec", s.compatReadFile)
	s.route("PUT /api/v1/sandboxes/{id}/files", "exec", s.compatWriteFile)
	s.route("DELETE /api/v1/sandboxes/{id}/files", "exec", s.compatDeleteFile)
	s.route("GET /api/v1/sandboxes/{id}/files/list", "exec", s.compatListFiles)
	s.route("GET /api/v1/sandboxes/{id}/files/info", "exec", s.compatFileInfo)
	s.route("GET /api/v1/sandboxes/{id}/files/search", "exec", s.compatSearchFiles)
	s.route("POST /api/v1/sandboxes/{id}/files/folder", "exec", s.compatMakeFolder)
	s.route("POST /api/v1/sandboxes/{id}/files/move", "exec", s.compatMoveFile)
	s.route("POST /api/v1/sandboxes/{id}/files/permissions", "exec", s.compatChmod)
	// saved snapshots and webhooks (compat_account.go)
	s.route("GET /api/v1/saved-snapshots", "vms:read", s.compatSavedList)
	s.route("POST /api/v1/saved-snapshots", "vms:write", s.compatSavedSave)
	s.route("GET /api/v1/saved-snapshots/{name}", "vms:read", s.compatSavedGet)
	s.route("DELETE /api/v1/saved-snapshots/{name}", "vms:write", s.compatSavedDelete)
	s.route("GET /api/v1/webhooks", "admin", s.compatHookList)
	s.route("POST /api/v1/webhooks", "admin", s.compatHookCreate)
	s.route("GET /api/v1/webhooks/{id}", "admin", s.compatHookGet)
	s.route("PATCH /api/v1/webhooks/{id}", "admin", s.compatHookUpdate)
	s.route("DELETE /api/v1/webhooks/{id}", "admin", s.compatHookDelete)
	s.route("POST /api/v1/webhooks/{id}/rotate", "admin", s.compatHookRotate)
}

// ---- envelope ----

func writeCompat(w http.ResponseWriter, status int, typ string, fields map[string]any) {
	out := map[string]any{"ok": true, "type": typ}
	for k, v := range fields {
		out[k] = v
	}
	writeJSON(w, status, out)
}

// compatCodes renames native error codes to boat.dev's where they differ.
var compatCodes = map[string]string{
	"quota_exceeded":       "limit_reached",
	"host_capacity":        "out_of_capacity",
	"too_many_in_flight":   "rate_limited",
	"upstream_error":       "http_500",
	"upstream_unavailable": "http_500",
	"store_error":          "http_500",
	"secrets_unavailable":  "http_500",
}

// compatWriter turns every native JSON error written under /api/v1 (auth,
// rate limits, validation, boatd) into boat.dev's error envelope; anything
// else passes through untouched, streams included.
type compatWriter struct {
	http.ResponseWriter
	rid    string
	status int // >= 400: buffering an error
	buf    bytes.Buffer
}

func (w *compatWriter) WriteHeader(code int) {
	if code >= 400 && strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		w.status = code
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *compatWriter) Write(b []byte) (int, error) {
	if w.status != 0 {
		return w.buf.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *compatWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *compatWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok && w.status == 0 {
		f.Flush()
	}
}

// finish writes the buffered error in boat.dev's shape.
func (w *compatWriter) finish() {
	if w.status == 0 {
		return
	}
	var native struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	json.Unmarshal(w.buf.Bytes(), &native)
	code := orDefault(compatCodes[native.Error.Code], native.Error.Code)
	code = orDefault(code, "http_"+strconv.Itoa(w.status))
	errObj := map[string]any{"code": code, "message": native.Error.Message, "status": w.status}
	if native.Error.Details != nil {
		errObj["details"] = native.Error.Details
	}
	out := map[string]any{"ok": false, "type": "sandbox.error", "status": w.status, "code": code,
		"message": native.Error.Message, "error": errObj, "requestId": w.rid}
	if ra, err := strconv.Atoi(w.Header().Get("Retry-After")); err == nil && w.status == http.StatusServiceUnavailable {
		out["retryAfterSeconds"] = ra
	}
	b, _ := json.Marshal(out)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)+1))
	w.ResponseWriter.WriteHeader(w.status)
	w.ResponseWriter.Write(append(b, '\n'))
}

// writeErrDetails is writeErr with boat.dev's error.details.
func writeErrDetails(w http.ResponseWriter, r *http.Request, status int, code, msg string, details map[string]any) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": code, "message": msg, "request_id": info(r).id, "details": details}})
}

// ---- calling the native handlers in-process ----

// nativeAnswer is an in-memory ResponseWriter for a native handler's answer.
type nativeAnswer struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (c *nativeAnswer) Header() http.Header { return c.h }
func (c *nativeAnswer) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}
func (c *nativeAnswer) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = 200
	}
	return c.body.Write(b)
}

// relay copies a failed native answer to w (compatWriter reshapes it).
func (c *nativeAnswer) relay(w http.ResponseWriter) {
	for k, v := range c.h {
		w.Header()[k] = v
	}
	w.WriteHeader(c.status)
	w.Write(c.body.Bytes())
}

func (c *nativeAnswer) ok() bool { return c.status/100 == 2 }

// callNative runs h as if pattern had been requested with body as its JSON,
// under the same principal and request id.
// kv are more path values: name, value, …; the name "?" sets the query string.
func (s *server) callNative(r *http.Request, p *principal, pattern string, h handlerFunc, id string, body any, kv ...string) *nativeAnswer {
	method, path, _ := strings.Cut(pattern, " ")
	query := ""
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] == "?" {
			query = kv[i+1]
			continue
		}
		path = strings.Replace(path, "{"+kv[i]+"}", url.PathEscape(kv[i+1]), 1)
	}
	var rd io.Reader = http.NoBody
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	ri := &reqInfo{id: info(r).id, p: p, pattern: pattern}
	r2, _ := http.NewRequestWithContext(context.WithValue(r.Context(), ctxKey{}, ri), method,
		strings.Replace(path, "{id}", url.PathEscape(id), 1), rd)
	if id != "" {
		r2.SetPathValue("id", id)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] != "?" {
			r2.SetPathValue(kv[i], kv[i+1])
		}
	}
	r2.URL.RawQuery = query
	c := &nativeAnswer{h: http.Header{}}
	h(c, r2, p)
	return c
}

// ---- sandboxes ----

// compatState maps boatd's states onto boat.dev's. A running VM is "ready",
// or "running" while one of its prompts is (boat.dev: an agent is working).
func compatState(st string, agentBusy bool) string {
	switch st {
	case "creating", "starting", "forking":
		return "provisioning"
	case "stopping":
		return "archiving"
	case "stopped":
		return "archived"
	case "running":
		if agentBusy {
			return "running"
		}
		return "ready"
	case "error":
		return st
	}
	return "error"
}

// compatTypes are boat.dev's machine types; another size reads as null.
var compatTypes = map[string]bool{"small": true, "default": true, "large": true, "xlarge": true}

// sandbox is a native VM as a boat.dev Sandbox object.
func (s *server) sandbox(vm VM) map[string]any {
	team, id := str(vm, "team"), str(vm, "id")
	return s.sandboxAs(vm, s.compatID(team, id))
}

// sandboxAs renders vm under the bx_ id already looked up for it.
func (s *server) sandboxAs(vm VM, bx string) map[string]any {
	labels := map[string]any{}
	if l, ok := vm["labels"].(map[string]any); ok {
		for k, v := range l {
			if !strings.HasPrefix(k, reservedLabels) {
				labels[k] = v
			}
		}
	}
	id := str(vm, "id")
	state := str(vm, "state")
	mem, _ := vm["mem_mib"].(float64)
	ttl, _ := vm["ttl_s"].(float64)
	out := map[string]any{"id": bx, "name": str(vm, "name"),
		"state":            compatState(state, s.promptBusy(id)),
		"desktopAvailable": s.cfg.DesktopURL != "" && state == "running", "snapshotAvailable": vm["has_snapshot"] == true,
		"memoryGB": int64(mem+512) / 1024, "ip": nullIfEmpty(str(vm, "ip")), "labels": labels,
		"createdAt": vm["created_at"], "updatedAt": vm["updated_at"], "ttlSeconds": nullIfZero(int64(ttl)),
		"archiveAfter": nil, "stop": s.stops.last(id), "error": nullIfEmpty(str(vm, "error")),
		"environment": nullIfEmpty(str(vm, "environment")), "access": "owner"}
	if compatTypes[str(vm, "size")] {
		out["type"] = str(vm, "size")
	}
	if v, ok := vm["vcpu"].(float64); ok {
		out["vcpu"] = int64(v)
	}
	if e, ok := vm["expires_at"].(string); ok && state == "running" {
		out["archiveAfter"] = e
	}
	if state == "stopped" {
		out["stoppedAt"] = vm["updated_at"]
	}
	return out
}

// promptBusy reports whether a prompt of the VM is running (boat-api's record).
func (s *server) promptBusy(vmID string) bool {
	for _, run := range s.store.Snapshot().Prompts[vmID] {
		if run.State == "running" {
			return true
		}
	}
	return false
}

type compatCreateReq struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	TTLSeconds  json.RawMessage   `json:"ttlSeconds"`
	Labels      map[string]string `json:"labels"`
	Env         map[string]string `json:"env"`
	Environment string            `json:"environment"`
	NoEnv       bool              `json:"noEnv"`
	FailFast    bool              `json:"failFast"`
	Count       int               `json:"count"`
	From        string            `json:"from"`
	// Accepted and ignored: always on here, or no meaning on one host.
	Snapshots  *bool  `json:"snapshots"`
	Monitoring *bool  `json:"monitoring"`
	Visibility string `json:"visibility"`
	Org        string `json:"org"`
	TeamID     string `json:"teamId"`
	// Refused with a clear message: not supported by this server.
	SetupScript string `json:"setupScript"`
	HostedPorts []any  `json:"hostedPorts"`
	Files       []any  `json:"files"`
}

const (
	compatDefaultTTL = 3600    // boat.dev's default ttlSeconds
	compatMaxTTL     = 2592000 // 30 days
)

// compatTTL reads ttlSeconds: absent = 3600, null or "infinite" = none.
func compatTTL(raw json.RawMessage) (int64, bool) {
	t := strings.TrimSpace(string(raw))
	switch t {
	case "":
		return compatDefaultTTL, true
	case "null", `"infinite"`:
		return 0, true
	}
	n, err := strconv.ParseInt(t, 10, 64)
	return n, err == nil && n >= 1 && n <= compatMaxTTL
}

func (s *server) compatCreate(w http.ResponseWriter, r *http.Request, p *principal) {
	s.idempotent(w, r, p, func(w http.ResponseWriter) {
		var c compatCreateReq
		if !decode(w, r, &c) {
			return
		}
		ttl, ok := compatTTL(c.TTLSeconds)
		msg := ""
		switch {
		case !ok:
			msg = "ttlSeconds must be 1-2592000, null or \"infinite\""
		case c.SetupScript != "" || len(c.HostedPorts) > 0 || len(c.Files) > 0:
			msg = "setupScript, hostedPorts and files are not supported by this server: use an environment, POST /api/v1/sandboxes/{id}/files, or the native /v1 API"
		case c.Count > maxBatch:
			msg = "count must be 1-16 on this server"
		}
		if msg != "" {
			writeErr(w, r, 400, "invalid_request", msg)
			return
		}
		body := map[string]any{"name": c.Name, "size": c.Type, "ttl_s": ttl, "labels": c.Labels, "env": c.Env,
			"no_env": c.NoEnv, "fail_fast": c.FailFast, "count": c.Count}
		if c.Environment != "" && c.Environment != "base" { // boat.dev's default name: the team default here
			body["environment"] = c.Environment
		}
		if !s.compatFrom(w, r, p, c.From, body) {
			return
		}
		res := s.callNative(r, p, "POST /v1/vms", s.createVM, "", body)
		if !res.ok() {
			res.relay(w)
			return
		}
		var one VM
		var many struct {
			VMs []VM `json:"vms"`
		}
		out := map[string]any{"status": "ready", "ttlSeconds": nullIfZero(ttl)}
		if c.Count > 1 {
			json.Unmarshal(res.body.Bytes(), &many)
			list := make([]any, len(many.VMs))
			for i, vm := range many.VMs {
				list[i] = s.sandbox(vm)
			}
			out["sandboxes"] = list
			if len(list) > 0 {
				out["sandbox"] = list[0]
			}
		} else {
			json.Unmarshal(res.body.Bytes(), &one)
			out["sandbox"] = s.sandbox(one)
		}
		writeCompat(w, http.StatusAccepted, "sandbox.created", out)
	})
}

func nullIfZero(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// compatFrom resolves boat.dev's from (a saved snapshot name or a share code)
// into the native create body; false means it already answered.
func (s *server) compatFrom(w http.ResponseWriter, r *http.Request, p *principal, from string, body map[string]any) bool {
	switch {
	case from == "":
		return true
	case strings.HasPrefix(from, "bsh_"):
		body["from"] = from
		return true
	}
	var list struct {
		Snapshots []map[string]any `json:"snapshots"`
	}
	if err := s.boatd.call(r.Context(), "GET", "/v1/snapshots?team="+url.QueryEscape(p.Team.ID), nil, &list); err != nil {
		writeUpstream(w, r, err)
		return false
	}
	for _, sn := range list.Snapshots {
		if str(sn, "team") == p.Team.ID && (str(sn, "name") == from || str(sn, "id") == from) {
			body["snapshot"] = str(sn, "id")
			return true
		}
	}
	writeErr(w, r, 404, "snapshot_not_found", "no saved snapshot "+from)
	return false
}

func (s *server) compatGet(w http.ResponseWriter, r *http.Request, p *principal) {
	if vm, ok := s.ownVM(w, r, p); ok {
		writeCompat(w, 200, "sandbox.info", map[string]any{"sandbox": s.sandbox(publicVM(vm))})
	}
}

// compatListParams are the list query parameters this server reads.
var compatListParams = map[string]bool{"limit": true, "cursor": true, "sort": true, "state": true, "name": true,
	"namePrefix": true, "label": true, "org": true, "include": true}

func (s *server) compatList(w http.ResponseWriter, r *http.Request, p *principal) {
	q := r.URL.Query()
	for k := range q {
		if !compatListParams[k] {
			writeErr(w, r, 400, "invalid_query", "unsupported query parameter "+k)
			return
		}
	}
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeErr(w, r, 400, "invalid_query", "limit must be 1-200")
			return
		}
		limit = n
	}
	desc := q.Get("sort") != "asc"
	if v := q.Get("sort"); v != "" && v != "asc" && v != "desc" {
		writeErr(w, r, 400, "invalid_query", "sort must be asc or desc")
		return
	}
	var list struct {
		VMs []VM `json:"vms"`
	}
	if err := s.boatd.call(r.Context(), "GET", "/v1/vms?team="+url.QueryEscape(p.Team.ID), nil, &list); err != nil {
		writeUpstream(w, r, err)
		return
	}
	var ids []string
	for _, vm := range list.VMs {
		if str(vm, "team") == p.Team.ID {
			ids = append(ids, str(vm, "id"))
		}
	}
	bx := s.compatIDs(p.Team.ID, ids)
	boxes := []map[string]any{}
	for _, vm := range list.VMs {
		if str(vm, "team") != p.Team.ID {
			continue
		}
		if sb := s.sandboxAs(publicVM(vm), bx[str(vm, "id")]); compatMatch(sb, q) {
			boxes = append(boxes, sb)
		}
	}
	page, next := compatPage(boxes, q.Get("cursor"), limit, desc)
	writeCompat(w, 200, "sandbox.list", map[string]any{"sandboxes": page,
		"pageInfo": map[string]any{"nextCursor": next, "hasMore": next != nil, "limit": limit}})
}

func compatMatch(sb map[string]any, q url.Values) bool {
	if v := q.Get("name"); v != "" && sb["name"] != v {
		return false
	}
	if v := q.Get("namePrefix"); v != "" && !strings.HasPrefix(sb["name"].(string), v) {
		return false
	}
	if v := q.Get("state"); v != "" && !containsStr(strings.Split(v, ","), sb["state"].(string)) {
		return false
	}
	labels, _ := sb["labels"].(map[string]any)
	for _, kv := range q["label"] {
		k, v, _ := strings.Cut(kv, "=")
		if labels[k] != v {
			return false
		}
	}
	return true
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if strings.TrimSpace(x) == s {
			return true
		}
	}
	return false
}

// compatPage orders by (createdAt, id) and returns the page after the cursor,
// an opaque base64 of the last item's key, and the next cursor (nil at the end).
func compatPage(boxes []map[string]any, cursor string, limit int, desc bool) ([]map[string]any, any) {
	key := func(b map[string]any) string {
		created, _ := b["createdAt"].(string)
		return created + "|" + b["id"].(string)
	}
	sort.Slice(boxes, func(i, j int) bool {
		if desc {
			return key(boxes[i]) > key(boxes[j])
		}
		return key(boxes[i]) < key(boxes[j])
	})
	start := 0
	if c, err := base64.RawURLEncoding.DecodeString(cursor); err == nil && cursor != "" {
		after := string(c)
		start = sort.Search(len(boxes), func(i int) bool {
			if desc {
				return key(boxes[i]) < after
			}
			return key(boxes[i]) > after
		})
	}
	end := min(start+limit, len(boxes))
	page := boxes[start:end]
	if end >= len(boxes) {
		return page, nil
	}
	return page, base64.RawURLEncoding.EncodeToString([]byte(key(boxes[end-1])))
}

// ---- delete, with boat.dev's confirmation header and deletion operations ----

// deletions remembers finished deletes for GET /deletion-operations (in memory).
type deletions struct {
	mu sync.Mutex
	m  map[string]map[string]any // op id -> operation (with its team under "_team")
}

const maxDeletionOps = 10000

func (d *deletions) add(team string, op map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.m == nil || len(d.m) >= maxDeletionOps {
		d.m = map[string]map[string]any{} // ponytail: forgets all past ops at the cap
	}
	cp := map[string]any{"_team": team}
	for k, v := range op {
		cp[k] = v
	}
	d.m[op["id"].(string)] = cp
}

func (d *deletions) get(team, id string) map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	op := d.m[id]
	if op == nil || op["_team"] != team {
		return nil
	}
	out := map[string]any{}
	for k, v := range op {
		if k != "_team" {
			out[k] = v
		}
	}
	return out
}

func (s *server) compatDelete(w http.ResponseWriter, r *http.Request, p *principal) {
	id := r.PathValue("id")
	asked := orDefault(info(r).compatID, id)
	if c := r.Header.Get("X-Ascii-Confirm-Delete"); c != asked && c != id {
		writeErr(w, r, 409, "confirmation_required", "set X-Ascii-Confirm-Delete to the sandbox id")
		return
	}
	res := s.callNative(r, p, "DELETE /v1/vms/{id}", s.deleteVM, id, nil)
	if !res.ok() {
		res.relay(w)
		return
	}
	now := time.Now().UTC()
	b := make([]byte, 16)
	rand.Read(b)
	op := map[string]any{"id": "bdop_" + hex.EncodeToString(b), "kind": "sandbox", "targetId": asked, "reason": "explicit",
		"status": "completed", "stage": "completed", "expectedBy": nil, "attemptCount": 1, "requestedAt": now, "completedAt": now}
	s.deletions.add(p.Team.ID, op)
	writeCompat(w, http.StatusAccepted, "sandbox.deleting", map[string]any{"operation": op})
}

func (s *server) compatDeletion(w http.ResponseWriter, r *http.Request, p *principal) {
	op := s.deletions.get(p.Team.ID, r.PathValue("opid"))
	if op == nil {
		writeErr(w, r, 404, "not_found", "deletion operation not found")
		return
	}
	writeCompat(w, 200, "deletion.operation", map[string]any{"operation": op})
}

// ---- stop, resume, fork ----

func (s *server) compatStop(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Force bool `json:"force"`
	}
	if !decode(w, r, &body) {
		return
	}
	var in any
	if body.Force {
		in = map[string]any{"force": true}
	}
	id := r.PathValue("id")
	op := s.stops.begin(p.Team.ID, id)
	res := s.callNative(r, p, "POST /v1/vms/{id}/stop", s.stopVM, id, in)
	if !res.ok() {
		s.stops.fail(id, op, res.body.String())
		res.relay(w)
		return
	}
	op = s.stops.complete(id, op)
	var vm VM
	json.Unmarshal(res.body.Bytes(), &vm)
	sb := s.sandbox(vm)
	writeCompat(w, http.StatusAccepted, "sandbox.stopping", map[string]any{"id": sb["id"], "status": sb["state"],
		"sandbox": sb, "forced": body.Force, "operation": op,
		"statusUrl": "/api/v1/sandboxes/" + sb["id"].(string) + "/stops/" + op["id"].(string)})
}

func (s *server) compatGetStop(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, ok := s.ownVM(w, r, p); !ok {
		return
	}
	op := s.stops.get(p.Team.ID, r.PathValue("id"), r.PathValue("stopid"))
	if op == nil {
		writeErr(w, r, 404, "not_found", "stop not found")
		return
	}
	writeCompat(w, 200, "stop.status", map[string]any{"id": op["id"], "stop": op})
}

func (s *server) compatResume(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		FailFast    bool              `json:"failFast"`
		TTLSeconds  json.RawMessage   `json:"ttlSeconds"` // accepted; the VM keeps its create-time TTL
		Type        string            `json:"type"`
		Env         map[string]string `json:"env"`
		Environment string            `json:"environment"`
		NoEnv       bool              `json:"noEnv"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Type != "" || body.Env != nil || body.Environment != "" || body.NoEnv {
		writeErr(w, r, 400, "invalid_request", "resume with type, env, environment or noEnv is not supported by this server; use POST /v1/vms/{id}/resize")
		return
	}
	s.stops.supersede(r.PathValue("id"))
	s.compatAction(w, r, p, "POST /v1/vms/{id}/start", s.startVM, r.PathValue("id"), map[string]any{"fail_fast": body.FailFast}, "sandbox.resuming")
}

func (s *server) compatFork(w http.ResponseWriter, r *http.Request, p *principal) {
	s.idempotent(w, r, p, func(w http.ResponseWriter) {
		var body struct {
			Name        string            `json:"name"`
			FailFast    bool              `json:"failFast"`
			TTLSeconds  json.RawMessage   `json:"ttlSeconds"` // accepted; a fork keeps the parent's TTL here
			Monitoring  *bool             `json:"monitoring"`
			Labels      map[string]string `json:"labels"`
			Env         map[string]string `json:"env"`
			Environment string            `json:"environment"`
			NoEnv       bool              `json:"noEnv"`
			Type        string            `json:"type"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Labels != nil || body.Env != nil || body.Environment != "" || body.NoEnv || body.Type != "" {
			writeErr(w, r, 400, "invalid_request", "fork with labels, env, environment, noEnv or type is not supported by this server")
			return
		}
		s.compatAction(w, r, p, "POST /v1/vms/{id}/fork", s.forkVM, r.PathValue("id"),
			map[string]any{"name": body.Name, "fail_fast": body.FailFast}, "sandbox.forking")
	})
}

// compatAction runs a native lifecycle call and answers boat.dev's
// SandboxActionResponse (202; this server is done when it answers).
func (s *server) compatAction(w http.ResponseWriter, r *http.Request, p *principal, pattern string, h handlerFunc, id string, body any, typ string) {
	res := s.callNative(r, p, pattern, h, id, body)
	if !res.ok() {
		res.relay(w)
		return
	}
	var vm VM
	json.Unmarshal(res.body.Bytes(), &vm)
	sb := s.sandbox(vm)
	writeCompat(w, http.StatusAccepted, typ, map[string]any{"id": sb["id"], "status": sb["state"], "sandbox": sb,
		"forced": false, "operation": nil, "statusUrl": nil})
}

// ---- limits ----

// compatLimits: what this server knows. Team quotas live in boatd's config,
// which boat-api cannot read, so maxActiveSandboxes is -1 (unknown) and a
// start that goes over one gets 429 limit_reached.
func (s *server) compatLimits(w http.ResponseWriter, r *http.Request, p *principal) {
	var list struct {
		VMs []VM `json:"vms"`
	}
	if err := s.boatd.call(r.Context(), "GET", "/v1/vms?team="+url.QueryEscape(p.Team.ID), nil, &list); err != nil {
		writeUpstream(w, r, err)
		return
	}
	active := 0
	for _, vm := range list.VMs {
		if st := str(vm, "state"); str(vm, "team") == p.Team.ID && st != "stopped" && st != "error" {
			active++
		}
	}
	writeCompat(w, 200, "limits.info", map[string]any{"activeSandboxes": active, "maxActiveSandboxes": -1,
		"activeStates": []string{"provisioning", "running", "archiving"}, "canStart": true, "billingStatus": "self_hosted"})
}
