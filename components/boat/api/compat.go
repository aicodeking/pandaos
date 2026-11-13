package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
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
	"quota_exceeded":     "limit_reached",
	"host_capacity":      "out_of_capacity",
	"too_many_in_flight": "rate_limited",
}

// compatHTTPCodes become boat.dev's generic http_<status>, so code and status agree.
var compatHTTPCodes = map[string]bool{"upstream_error": true, "upstream_unavailable": true,
	"store_error": true, "secrets_unavailable": true, "guest_error": true}

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
	if code == "" || compatHTTPCodes[code] {
		code = "http_" + strconv.Itoa(w.status)
	}
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
	if msg := s.nativeRefusal(p, pattern); msg != "" {
		writeErr(c, r2, http.StatusForbidden, "forbidden", msg)
		return c
	}
	if p.VMID != "" && !selfAllowed(pattern, r2, p.VMID) {
		writeErr(c, r2, http.StatusNotFound, "not_found", "not found")
		return c
	}
	if s.onNative != nil {
		s.onNative(pattern, p)
	}
	h(c, r2, p)
	return c
}

// nativeRefusal is why p may not run the native routes (callNative's scope
// check: a compat route never reaches past its caller's scopes), or "".
// An unregistered pattern is refused: the lookup fails closed.
func (s *server) nativeRefusal(p *principal, patterns ...string) string {
	for _, pattern := range patterns {
		need, ok := s.routeScopes[pattern]
		if !ok {
			return "no native route " + pattern
		}
		if need != "" && !p.has(need) {
			return "key lacks scope " + need
		}
	}
	return ""
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
	if cs := s.store.Snapshot().CompatSetups[id]; cs != nil {
		out["setupStatus"], out["setupError"] = cs.Status, nullIfEmpty(cs.Error)
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
	Snapshots   *bool           `json:"snapshots"`
	Monitoring  *bool           `json:"monitoring"`
	Visibility  string          `json:"visibility"`
	Org         string          `json:"org"`
	TeamID      string          `json:"teamId"`
	HostedPorts []compatHostReq `json:"hostedPorts"` // POST …/host for each, after the create
	// Written before create answers; the script then runs in the background (compat_setup.go).
	SetupScript string       `json:"setupScript"`
	Files       []compatFile `json:"files"`
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
	var c compatCreateReq
	var ttl int64
	var files []preparedFile
	s.idempotent(w, r, p, func() bool {
		if !decode(w, r, &c) {
			return false
		}
		var ok bool
		var fmsg string
		ttl, ok = compatTTL(c.TTLSeconds)
		files, fmsg = s.prepareCompatFiles(c.Files)
		msg := ""
		switch {
		case !ok:
			msg = "ttlSeconds must be 1-2592000, null or \"infinite\""
		case len(c.SetupScript) > maxCompatSetupScript || strings.ContainsRune(c.SetupScript, 0):
			msg = "invalid_setup_script: setupScript must be text of at most 64 KiB"
		case fmsg != "":
			msg = "invalid_files: " + fmsg
		case hostedPortsMsg(c.HostedPorts, c.Count, c.FailFast) != "":
			msg = hostedPortsMsg(c.HostedPorts, c.Count, c.FailFast)
		case c.Count > maxBatch:
			msg = "count must be 1-16 on this server"
		}
		if msg != "" {
			code := "invalid_request"
			if k, text, ok := strings.Cut(msg, ": "); ok && (strings.HasSuffix(k, "hosted_ports") || k == "invalid_files" || k == "invalid_setup_script") {
				code, msg = k, text // boat.dev's codes for hostedPorts
			}
			writeErr(w, r, 400, code, msg)
			return false
		}
		if (len(c.Files) > 0 || c.SetupScript != "") && !p.has("exec") {
			writeErr(w, r, http.StatusForbidden, "forbidden", "files and setupScript require scope exec")
			return false
		}
		if len(c.HostedPorts) > 0 {
			if msg := s.nativeRefusal(p, hostPortRoutes...); msg != "" {
				writeErr(w, r, http.StatusForbidden, "forbidden", "hostedPorts: "+msg)
				return false // before the VM exists, including a cached answer
			}
		}
		return true
	}, func(w http.ResponseWriter) {
		body := map[string]any{"name": c.Name, "size": c.Type, "ttl_s": ttl, "labels": c.Labels, "env": c.Env,
			"no_env": c.NoEnv, "fail_fast": c.FailFast, "count": c.Count}
		if c.Environment != "" && c.Environment != "base" { // boat.dev's default name: the team default here
			body["environment"] = c.Environment
		}
		if !s.compatFrom(w, r, p, c.From, body) {
			return
		}
		res := s.callNative(r, p, "POST /v1/vms", s.createVM, "", body)
		if c.Type == "default" && res.status == 400 && strings.Contains(res.body.String(), "unknown size") {
			// boat.dev's SDKs always send type "default": on a host with no size of that
			// name it means this host's default size.
			body["size"] = ""
			res = s.callNative(r, p, "POST /v1/vms", s.createVM, "", body)
		}
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
				s.compatSetupVM(r, str(vm, "id"), files, c.SetupScript)
				list[i] = s.sandbox(vm)
			}
			out["sandboxes"] = list
			if len(list) > 0 {
				out["sandbox"] = list[0]
			}
		} else {
			json.Unmarshal(res.body.Bytes(), &one)
			s.compatSetupVM(r, str(one, "id"), files, c.SetupScript)
			out["sandbox"] = s.sandbox(one)
			if len(c.HostedPorts) > 0 {
				out["hostedPorts"] = s.compatHostedPorts(r, p, str(one, "id"), c.HostedPorts)
			}
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
// deletions are boat.dev's deletion operations, kept in the store
// (State.DeletionOps) so a restart keeps them.
type deletions struct{ store *Store }

const maxDeletionOps = 10000

func (d *deletions) add(team string, op map[string]any) {
	cp := map[string]any{"_team": team}
	for k, v := range op {
		cp[k] = v
	}
	err := d.store.Update(func(st *State) error {
		if len(st.DeletionOps) >= maxDeletionOps {
			st.DeletionOps = map[string]map[string]any{} // ponytail: forgets all past ops at the cap
		}
		st.DeletionOps[op["id"].(string)] = cp
		return nil
	})
	if err != nil {
		log.Printf("deletion operations: %v", err) // the deletion happened; only its record is lost
	}
}

func (d *deletions) get(team, id string) map[string]any {
	op := d.store.Snapshot().DeletionOps[id]
	if op == nil || op["_team"] != team {
		return nil
	}
	return public(op)
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
	if body.Environment != "" || body.NoEnv {
		writeErr(w, r, 400, "invalid_request", "resume with environment or noEnv is not supported by this server: the VM keeps its create-time environment and secret selection")
		return
	}
	if res := s.compatRetype(r, p, r.PathValue("id"), body.Type); res != nil {
		res.relay(w)
		return
	}
	s.stops.supersede(r.PathValue("id"))
	s.compatAction(w, r, p, "POST /v1/vms/{id}/start", s.startVM, r.PathValue("id"), map[string]any{"fail_fast": body.FailFast, "env": body.Env}, "sandbox.resuming")
}

func (s *server) compatFork(w http.ResponseWriter, r *http.Request, p *principal) {
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
	s.idempotent(w, r, p, func() bool {
		if !decode(w, r, &body) {
			return false
		}
		if body.Environment != "" || body.NoEnv {
			writeErr(w, r, 400, "invalid_request", "fork with environment or noEnv is not supported by this server: a fork keeps the parent's environment and secret selection")
			return false
		}
		if body.Type != "" && !reVMName.MatchString(body.Type) {
			writeErr(w, r, 400, "invalid_request", "invalid type")
			return false
		}
		_, ok := s.ownVM(w, r, p)
		return ok
	}, func(w http.ResponseWriter) {
		fin := map[string]any{"name": body.Name, "fail_fast": body.FailFast, "env": body.Env}
		if body.Labels != nil {
			fin["labels"] = body.Labels
		}
		res := s.callNative(r, p, "POST /v1/vms/{id}/fork", s.forkVM, r.PathValue("id"), fin)
		if !res.ok() {
			res.relay(w)
			return
		}
		var vm VM
		json.Unmarshal(res.body.Bytes(), &vm)
		if rs := s.compatRetype(r, p, str(vm, "id"), body.Type); rs != nil {
			if err := s.discardCopyVM(str(vm, "id")); err != nil {
				log.Printf("%s: rollback fork %s: %v", info(r).id, str(vm, "id"), err)
				writeErr(w, r, 500, "cleanup_failed", "fork retype failed and the new VM "+str(vm, "id")+" could not be deleted")
				return
			}
			rs.relay(w)
			return
		}
		if body.Type != "" {
			s.boatd.call(r.Context(), "GET", vmPath(str(vm, "id")), nil, &vm)
		}
		sb := s.sandbox(vm)
		writeCompat(w, http.StatusAccepted, "sandbox.forking", map[string]any{"id": sb["id"], "status": sb["state"], "sandbox": sb,
			"forced": false, "operation": nil, "statusUrl": nil})
	})
}

// compatRetype gives a VM boat.dev's type (a size here). boatd changes vCPU
// or RAM only on a stopped VM, with a cold boot next start, so a running VM is
// stopped, resized and started again. nil = done (or already that size, or
// "default" on a host without a size of that name).
func (s *server) compatRetype(r *http.Request, p *principal, id, typ string) *nativeAnswer {
	if typ == "" {
		return nil
	}
	if !reVMName.MatchString(typ) {
		res := &nativeAnswer{h: http.Header{}}
		writeErr(res, r, 400, "invalid_request", "invalid type")
		return res
	}
	// Check resize permission before stopping: stop and resize have distinct scopes.
	if msg := s.nativeRefusal(p, "POST /v1/vms/{id}/resize"); msg != "" {
		res := &nativeAnswer{h: http.Header{}}
		writeErr(res, r, 403, "forbidden", msg)
		return res
	}
	var vm VM
	if err := s.boatd.call(r.Context(), "GET", vmPath(id), nil, &vm); err != nil || str(vm, "size") == typ {
		return nil // ownVM in the callers' native calls answers a missing VM
	}
	running := str(vm, "state") == "running"
	if running {
		if res := s.callNative(r, p, "POST /v1/vms/{id}/stop", s.stopVM, id, nil); !res.ok() {
			return res
		}
	}
	rs := s.callNative(r, p, "POST /v1/vms/{id}/resize", s.resizeVM, id, map[string]any{"size": typ, "discard_memory": true})
	if !rs.ok() && !(typ == "default" && rs.status == 400 && strings.Contains(rs.body.String(), "unknown size")) {
		if running {
			// A rejected resize must not leave a previously running VM stopped.
			if restore := s.callNative(r, p, "POST /v1/vms/{id}/start", s.startVM, id, nil); !restore.ok() {
				res := &nativeAnswer{h: http.Header{}}
				writeErr(res, r, 500, "restore_failed", "resize failed and the previously running VM could not be restarted")
				return res
			}
		}
		return rs
	}
	if running {
		if res := s.callNative(r, p, "POST /v1/vms/{id}/start", s.startVM, id, nil); !res.ok() {
			return res
		}
	}
	return nil
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
