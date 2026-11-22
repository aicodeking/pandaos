package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
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
		Sandbox   json.RawMessage `json:"sandbox"`
		Sandboxes json.RawMessage `json:"sandboxes"`
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
	// The sandbox a failed-closed create, fork or resume kept (stopped): its id.
	if len(native.Sandbox) > 0 && string(native.Sandbox) != "null" {
		out["sandbox"] = native.Sandbox
	}
	if len(native.Sandboxes) > 0 && string(native.Sandboxes) != "null" {
		out["sandboxes"] = native.Sandboxes
	}
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
	srv    *server // renders a failed answer's "vm" as a compat sandbox (relay); may be nil
	// cause is, for an answer that reports a start which failed after the previous
	// run was restored, the failed start's own message (the answer's message adds
	// that the run is running again).
	cause string
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
//
// A failed answer that carries the VM it concerns (env_start_failed: the VM was
// kept, stopped) hands it on as "sandbox", the compat sandbox object, so the
// client keeps the id of what was created or forked (compatWriter.finish).
func (c *nativeAnswer) relay(w http.ResponseWriter) {
	for k, v := range c.h {
		w.Header()[k] = v
	}
	body := c.body.Bytes()
	if c.srv != nil && c.status >= 400 {
		body = c.srv.withSandbox(body)
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(c.status)
	w.Write(body)
}

// withSandbox rewrites a native error body: its "vm" (and the VMs of a batch
// in "vms") become compat sandboxes under "sandbox" (and "sandboxes"). Bodies
// without a VM, and anything that is not a JSON object, pass through.
func (s *server) withSandbox(body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	changed := false
	if raw, ok := m["vm"]; ok {
		var vm VM
		if json.Unmarshal(raw, &vm) == nil && str(vm, "id") != "" {
			if b, err := json.Marshal(s.sandbox(vm)); err == nil {
				m["sandbox"] = b
				delete(m, "vm")
				changed = true
			}
		}
	}
	if raw, ok := m["vms"]; ok {
		var vms []VM
		if json.Unmarshal(raw, &vms) == nil && len(vms) > 0 {
			list := make([]map[string]any, 0, len(vms))
			for _, vm := range vms {
				if str(vm, "id") != "" {
					list = append(list, s.sandbox(vm))
				}
			}
			if b, err := json.Marshal(list); err == nil {
				m["sandboxes"] = b
				delete(m, "vms")
				changed = true
			}
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
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
	outer := info(r)
	if auditedReads[pattern] {
		outer.audit = true
	}
	ri := &reqInfo{id: outer.id, p: p, pattern: pattern}
	if outer.vm != nil && id != "" && str(outer.vm, "id") == id && str(outer.vm, "team") == p.Team.ID {
		ri.vm, ri.vmOK = outer.vm, true
	}
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
	c := &nativeAnswer{h: http.Header{}, srv: s}
	if msg := s.nativeRefusal(p, pattern); msg != "" {
		writeErr(c, r2, http.StatusForbidden, "forbidden", msg)
		return c
	}
	if p.VMID != "" && !selfAllowed(pattern, r2, p.VMID) {
		writeErr(c, r2, http.StatusNotFound, "not_found", "not found")
		return c
	}
	if s.refuseSelfNoEnv(c, r2, p, pattern) {
		return c
	}
	if s.onNative != nil {
		s.onNative(pattern, p)
	}
	h(c, r2, p)
	if method != http.MethodGet {
		outer.vm = nil
	} else if outer.vm == nil && ri.vm != nil && id != "" && str(ri.vm, "id") == id && str(ri.vm, "team") == p.Team.ID {
		outer.vm, outer.vmOK = ri.vm, true
	}
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

func (s *server) sandboxStage(vm VM) map[string]any {
	team, id := str(vm, "team"), str(vm, "id")
	return s.sandboxAs(vm, s.compatIDStage(team, id))
}

type ctxManagedStop struct{}

func withManagedStop(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxManagedStop{}, true))
}

func isManagedStop(r *http.Request) bool {
	v, _ := r.Context().Value(ctxManagedStop{}).(bool)
	return v
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
	cstate := compatState(state, s.promptBusy(id))
	pending := s.envPushPending(id)
	if pending {
		// A start or create is still pushing the env: not ready yet, whatever boatd
		// says (the pre-start window of a resume still reads "stopped") and whatever
		// an earlier push's remembered error says: the retry is in flight.
		cstate = "provisioning"
	}
	st := s.store.Snapshot()
	out := map[string]any{"id": bx, "name": displayName(st, vm),
		"state":            cstate,
		"desktopAvailable": s.cfg.DesktopURL != "" && state == "running", "snapshotAvailable": vm["has_snapshot"] == true,
		"memoryGB": int64(mem+512) / 1024, "ip": nullIfEmpty(str(vm, "ip")), "labels": labels,
		"createdAt": vm["created_at"], "updatedAt": vm["updated_at"], "ttlSeconds": nullIfZero(int64(ttl)),
		"archiveAfter": nil, "stop": s.stops.last(id), "error": nullIfEmpty(str(vm, "error")),
		"environment": nil, "environmentVersion": nil}
	// boatd does not retain the environment association. Use one local snapshot
	// for its current name and the version actually replayed by this sandbox.
	_, noEnv := st.NoEnvRuns[id]
	if pending := st.VMTransitions[id]; pending != nil && pending.NoEnv {
		noEnv = true
	}
	if e := st.Environments[st.VMEnvs[id]]; e != nil && e.TeamID == str(vm, "team") && !noEnv && !labeledNoEnv(vm) {
		out["environment"], out["environmentVersion"] = e.Name, e.Version
		if pin := st.VMEnvPins[id]; pin != nil {
			out["environmentVersion"] = pin.Version
		}
	}
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
		out["stoppedAt"] = nullIfEmpty(compatStoppedAt(vm))
	}
	if cs := s.store.Snapshot().CompatSetups[id]; cs != nil {
		out["setupStatus"], out["setupError"] = cs.Status, nullIfEmpty(cs.Error)
	}
	if msg := s.envErrorOf(vm); msg != "" {
		// The environment never reached the guest (persisted until a push succeeds).
		out["error"] = msg
		if !pending {
			out["state"] = "error"
		}
	}
	return out
}

// compatStoppedAt uses boatd's persisted stop transition, which survives later
// stopped-VM edits. Older records omit it, so only those fall back to updated_at.
func compatStoppedAt(vm VM) string {
	if stoppedAt, present := vm["stopped_at"]; present {
		value, _ := stoppedAt.(string)
		return value
	}
	return str(vm, "updated_at")
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
	Visibility  string            `json:"visibility"`
	// Accepted and ignored: always on here, or no meaning on one host.
	Snapshots   *bool           `json:"snapshots"`
	Monitoring  *bool           `json:"monitoring"`
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

// parseLifecycleTTL parses ttlSeconds on resume/fork: omitted -> (nil, ""),
// null or "infinite" -> (&0, ""), exact integer 1..2592000 -> (&n, ""), anything
// else (0, fractions, exponents, numeric strings, booleans) -> error.
func parseLifecycleTTL(raw json.RawMessage) (*int64, string) {
	const errMsg = "ttlSeconds must be 1-2592000, null or \"infinite\""
	t := strings.TrimSpace(string(raw))
	if t == "" {
		return nil, ""
	}
	if t == "null" || t == `"infinite"` {
		var zero int64
		return &zero, ""
	}
	if len(t) == 0 || t[0] < '1' || t[0] > '9' {
		return nil, errMsg
	}
	for i := 1; i < len(t); i++ {
		if t[i] < '0' || t[i] > '9' {
			return nil, errMsg
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n < 1 || n > compatMaxTTL {
		return nil, errMsg
	}
	return &n, ""
}

func compatVisibilityMsg(v string) string {
	switch v {
	case "", "shared":
		return ""
	case "private":
		return "visibility \"private\" is not supported on self-hosted boat: every sandbox is usable by its whole team"
	default:
		return "visibility is not supported unless \"shared\" or omitted: every sandbox on self-hosted boat is usable by its whole team"
	}
}

func (s *server) compatCreate(w http.ResponseWriter, r *http.Request, p *principal) {
	var c compatCreateReq
	var ttl int64
	var files []preparedFile
	s.idempotent(w, r, p, func() bool {
		if !decode(w, r, &c) {
			return false
		}
		if vmsg := compatVisibilityMsg(c.Visibility); vmsg != "" {
			writeErr(w, r, 400, "invalid_request", vmsg)
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
		case c.Count > 1 && c.From == "":
			msg = "count_requires_from: count above 1 clones a snapshot, so it needs from (a saved snapshot name or id, or a bsh_ share code)"
		case c.Count > 1 && c.FailFast:
			msg = "count_with_fail_fast: failFast cannot be combined with count above 1"
		case c.Count > maxBatch:
			msg = "count must be 1-16 on this server"
		default:
			msg = validEnv(c.Env)
		}
		if msg != "" {
			code := "invalid_request"
			if k, text, ok := strings.Cut(msg, ": "); ok && (strings.HasSuffix(k, "hosted_ports") || k == "invalid_files" || k == "invalid_setup_script" || k == "count_requires_from" || k == "count_with_fail_fast") {
				code, msg = k, text // boat.dev's codes for hostedPorts
			}
			writeErr(w, r, 400, code, msg)
			return false
		}
		if len(c.Env) > 0 && !p.has("exec") {
			writeErr(w, r, http.StatusForbidden, "forbidden", "env requires scope exec")
			return false
		}
		if (len(c.Files) > 0 || c.SetupScript != "") && !p.has("exec") {
			writeErr(w, r, http.StatusForbidden, "forbidden", "files and setupScript require scope exec")
			return false
		}
		if !c.NoEnv && !p.has("exec") {
			st := s.store.Snapshot()
			env, _ := resolveEnvRef(st, p.Team.ID, c.Environment) // an unknown name is refused by createVM
			built := env != nil && c.From == "" && env.Build != nil && env.Build.SnapshotID != "" && env.Build.Version == env.Version
			if env != nil && !built && strings.TrimSpace(env.SetupScript) != "" {
				writeErr(w, r, http.StatusForbidden, "forbidden", "environment setup_script requires scope exec")
				return false
			}
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
		if c.Environment != "" {
			body["environment"] = c.Environment // createVM's resolver: "base" is the team default
		}
		if c.NoEnv {
			// An explicit empty (non-nil) selection: createVM treats an absent one as
			// "inject @default", so noEnv must say "none" to inject no team secrets.
			body["secrets"] = []string{}
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
		envFailed := false
		hasIdem := r.Header.Get("Idempotency-Key") != ""
		if c.Count > 1 {
			json.Unmarshal(res.body.Bytes(), &many)
			ids := make([]string, len(many.VMs))
			for i, vm := range many.VMs {
				ids[i] = str(vm, "id")
			}
			var bx map[string]string
			if hasIdem {
				bx = s.compatIDsStage(p.Team.ID, ids)
			} else {
				bx = s.compatIDs(p.Team.ID, ids)
			}
			info(r).idemVMs = slices.Clone(ids)
			list := make([]any, len(many.VMs))
			sem := make(chan struct{}, min(len(many.VMs), 16))
			var wg sync.WaitGroup
			for i, vm := range many.VMs {
				wg.Add(1)
				sem <- struct{}{}
				go func(i int, vm VM) {
					defer wg.Done()
					defer func() { <-sem }()
					id := str(vm, "id")
					if str(vm, "env_error") == "" {
						s.compatSetupVM(r, id, files, c.SetupScript)
					}
					list[i] = withEnvError(s.sandboxAs(vm, bx[id]), vm)
				}(i, vm)
			}
			wg.Wait()
			for _, vm := range many.VMs {
				envFailed = envFailed || str(vm, "env_error") != ""
			}
			out["sandboxes"] = list
			if len(list) > 0 {
				out["sandbox"] = list[0]
			}
		} else {
			json.Unmarshal(res.body.Bytes(), &one)
			envFailed = str(one, "env_error") != ""
			if !envFailed {
				s.compatSetupVM(r, str(one, "id"), files, c.SetupScript)
			}
			if hasIdem {
				out["sandbox"] = withEnvError(s.sandboxStage(one), one)
			} else {
				out["sandbox"] = withEnvError(s.sandbox(one), one)
			}
			if id := str(one, "id"); id != "" {
				info(r).idemVMs = []string{id}
			}
			if len(c.HostedPorts) > 0 {
				out["hostedPorts"] = s.compatHostedPorts(r, p, str(one, "id"), c.HostedPorts)
			}
		}
		if envFailed {
			// boat.dev's create status is provisioning or ready; a sandbox whose
			// environment never reached the guest is not ready.
			out["status"] = "provisioning"
		}
		writeCompat(w, http.StatusAccepted, "sandbox.created", out)
	})
}

// withEnvError marks sb (a freshly built sandbox object) as failed when the
// native create reported that the VM environment could not be set.
func withEnvError(sb map[string]any, vm VM) map[string]any {
	if msg := str(vm, "env_error"); msg != "" {
		sb["state"] = "error"
		sb["error"] = msg
	}
	return sb
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
		if id := str(vm, "id"); str(vm, "state") == "running" && s.promptBusy(id) {
			s.refreshPrompts(r, id)
		}
		writeCompat(w, 200, "sandbox.info", map[string]any{"sandbox": s.sandbox(publicVM(vm))})
	}
}

// compatListParams are the list query parameters this server reads.
var compatListParams = map[string]bool{"limit": true, "cursor": true, "sort": true, "state": true, "name": true,
	"namePrefix": true, "label": true, "org": true, "include": true, "stoppedBefore": true, "dataExpired": true}

func (s *server) compatList(w http.ResponseWriter, r *http.Request, p *principal) {
	q := r.URL.Query()
	for k := range q {
		if !compatListParams[k] {
			writeErr(w, r, 400, "invalid_query", "unsupported query parameter "+k)
			return
		}
	}
	var stoppedBefore time.Time
	if values, present := q["stoppedBefore"]; present {
		var err error
		stoppedBefore, err = time.Parse(time.RFC3339Nano, values[0])
		if len(values) != 1 || err != nil {
			writeErr(w, r, 400, "invalid_query", "stoppedBefore must be an RFC 3339 date-time")
			return
		}
	}
	if values, present := q["dataExpired"]; present && (len(values) != 1 || (values[0] != "true" && values[0] != "false")) {
		writeErr(w, r, 400, "invalid_query", "dataExpired must be true or false")
		return
	}
	// No list-price billing meter or filtered deleted-sandbox ledger exists.
	// Every non-empty include must be refused rather than silently omitted.
	for _, include := range q["include"] {
		if include != "" {
			writeErr(w, r, 501, "not_supported", "sandbox list include="+include+" is not supported; calendar-month list-price costs are unavailable")
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
	var busy []string
	for _, vm := range list.VMs {
		if id := str(vm, "id"); str(vm, "team") == p.Team.ID && str(vm, "state") == "running" && s.promptBusy(id) {
			busy = append(busy, id)
		}
	}
	s.refreshPromptsForList(r, busy)
	for _, vm := range list.VMs {
		if str(vm, "team") != p.Team.ID {
			continue
		}
		// Local data is retained until explicit deletion, which removes the VM
		// entirely. Auto-stop deadlines (expires_at) never mean data expiry.
		if q.Get("dataExpired") == "true" {
			continue
		}
		if q.Has("stoppedBefore") {
			// boatd's stopped VM timestamp is also exposed as stoppedAt. Use
			// its state, not just API stop records, so auto-stops count too.
			stoppedAt, err := time.Parse(time.RFC3339Nano, compatStoppedAt(vm))
			if str(vm, "state") != "stopped" || err != nil || !stoppedAt.Before(stoppedBefore) {
				continue
			}
		}
		id := str(vm, "id")
		if sb := s.sandboxAs(publicVM(vm), bx[id]); compatMatch(sb, q) {
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

const (
	maxDeletionOpsPerTeam = 1024
	maxDeletionOps        = 32768
)

func opRequestedAt(op map[string]any) time.Time {
	switch t := op["requestedAt"].(type) {
	case time.Time:
		return t
	case string:
		parsed, _ := time.Parse(time.RFC3339Nano, t)
		return parsed
	}
	return time.Time{}
}

func oldestTeamOpID(ops map[string]map[string]any, team string) string {
	oldestID := ""
	var oldestAt time.Time
	for id, op := range ops {
		if (team != "" && op["_team"] != team) || isMarkRecord(op) {
			continue // a VM's no_env mark record (r37_seq.go) is not an operation to evict
		}
		at := opRequestedAt(op)
		if oldestID == "" || at.Before(oldestAt) || (at.Equal(oldestAt) && id < oldestID) {
			oldestID, oldestAt = id, at
		}
	}
	return oldestID
}

func (d *deletions) add(team string, op map[string]any) {
	cp := map[string]any{"_team": team}
	for k, v := range op {
		cp[k] = v
	}
	err := d.store.Apply(func(st *State) error {
		teamCount := 0
		for _, cur := range st.DeletionOps {
			if cur["_team"] == team {
				teamCount++
			}
		}
		for teamCount >= maxDeletionOpsPerTeam {
			evict := oldestTeamOpID(st.DeletionOps, team)
			if evict == "" {
				break
			}
			delete(st.DeletionOps, evict)
			teamCount--
		}
		for len(st.DeletionOps) >= maxDeletionOps {
			evict := oldestTeamOpID(st.DeletionOps, "")
			if evict == "" {
				break
			}
			delete(st.DeletionOps, evict)
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
	settled := false
	defer func() {
		if !settled {
			s.store.Kick()
		}
	}()
	res := s.callNative(withManagedStop(r), p, "POST /v1/vms/{id}/stop", s.stopVM, id, in)
	if !res.ok() {
		s.stops.failAsync(id, op, res.body.String())
		settled = true
		res.relay(w)
		return
	}
	op = s.stops.completeAsync(id, op)
	settled = true
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
		TTLSeconds  json.RawMessage   `json:"ttlSeconds"`
		Type        string            `json:"type"`
		Env         map[string]string `json:"env"`
		Environment string            `json:"environment"`
		NoEnv       bool              `json:"noEnv"`
	}
	if !decode(w, r, &body) {
		return
	}
	ttl, msg := parseLifecycleTTL(body.TTLSeconds)
	if msg == "" {
		msg = validEnv(body.Env)
	}
	switch {
	case msg != "":
		writeErr(w, r, 400, "invalid_request", msg)
		return
	case body.Type != "" && !reVMName.MatchString(body.Type):
		writeErr(w, r, 400, "invalid_request", "invalid type")
		return
	}
	if len(body.Env) > 0 && !p.has("exec") {
		writeErr(w, r, http.StatusForbidden, "forbidden", "env requires scope exec")
		return
	}
	// An unknown environment refuses the resume before a type change stops anything.
	if !s.compatNamedEnv(w, r, p, body.Environment, body.NoEnv) {
		return
	}
	// A change to the sandbox (an environment, noEnv, a type) is made while it is
	// stopped. A running one refuses it before anything is stopped, started or
	// resized: the caller stops it first. A no_env sandbox takes no environment at
	// all (400 no_env_sticky, whatever its state).
	if body.Environment != "" || body.NoEnv || body.Type != "" {
		vm, ok := s.ownVM(w, r, p)
		if !ok {
			return
		}
		if s.refuseStickyNoEnv(w, r, r.PathValue("id"), body.Environment, nil) {
			return
		}
		if str(vm, "state") == "running" {
			// Refusals of the caller come first: a VM's own token never starts its VM,
			// and a type change needs the resize scope.
			if msg := compatSelfStartRefusal(p); msg != "" {
				writeErr(w, r, http.StatusForbidden, "forbidden", msg)
				return
			}
			if body.Type != "" {
				if msg := s.nativeRefusal(p, "POST /v1/vms/{id}/resize"); msg != "" {
					writeErr(w, r, http.StatusForbidden, "forbidden", msg)
					return
				}
			}
			writeErr(w, r, http.StatusConflict, "sandbox_running_stop_first", "the sandbox is running: stop it first to resume it with an environment, noEnv or a type")
			return
		}
	}
	startBody := map[string]any{"fail_fast": body.FailFast, "env": body.Env}
	if body.Environment != "" {
		startBody["environment"] = body.Environment
	}
	if body.NoEnv {
		startBody["no_env"] = true
	}
	if ttl != nil {
		startBody["ttl_s"] = *ttl
	}
	restarted, prevSize, res := s.compatRetypeOpts(r, p, r.PathValue("id"), body.Type, startBody)
	if !restarted && res != nil {
		res.relay(w)
		return
	}
	// The stop operation is superseded only once a start succeeded (startVM does
	// it itself): a failed resume leaves the stop record as it was.
	if restarted {
		s.stops.supersede(r.PathValue("id"))
		var vm VM
		json.Unmarshal(res.body.Bytes(), &vm)
		sb := withEnvError(s.sandbox(vm), vm)
		writeCompat(w, http.StatusAccepted, "sandbox.resuming", map[string]any{"id": sb["id"], "status": sb["state"], "sandbox": sb,
			"forced": false, "operation": nil, "statusUrl": nil})
		return
	}
	startRes := s.callNative(r, p, "POST /v1/vms/{id}/start", s.startVM, r.PathValue("id"), startBody)
	if !startRes.ok() {
		if prevSize != "" {
			rbReq := r
			if rbReq.Context().Err() != nil {
				ctx, cancel := context.WithTimeout(s.baseCtx(), 10*time.Second)
				defer cancel()
				rbReq = r.WithContext(ctx)
			}
			if rb := s.callNative(rbReq, p, "POST /v1/vms/{id}/resize", s.resizeVM, r.PathValue("id"), map[string]any{"size": prevSize, "discard_memory": true}); !rb.ok() {
				log.Printf("%s: rollback resize %s to %s after failed start: %s", info(r).id, r.PathValue("id"), prevSize, rb.body.String())
			}
		}
		startRes.relay(w)
		return
	}
	s.stops.supersede(r.PathValue("id"))
	var vm VM
	json.Unmarshal(startRes.body.Bytes(), &vm)
	sb := withEnvError(s.sandbox(vm), vm)
	writeCompat(w, http.StatusAccepted, "sandbox.resuming", map[string]any{"id": sb["id"], "status": sb["state"], "sandbox": sb,
		"forced": false, "operation": nil, "statusUrl": nil})
}

func (s *server) compatFork(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Name        string            `json:"name"`
		FailFast    bool              `json:"failFast"`
		TTLSeconds  json.RawMessage   `json:"ttlSeconds"`
		Monitoring  *bool             `json:"monitoring"`
		Labels      map[string]string `json:"labels"`
		Env         map[string]string `json:"env"`
		Environment string            `json:"environment"`
		NoEnv       *bool             `json:"noEnv"`
		Type        string            `json:"type"`
	}
	var ttl *int64
	s.idempotent(w, r, p, func() bool {
		if !decode(w, r, &body) {
			return false
		}
		if p.VMID != "" {
			writeErr(w, r, http.StatusForbidden, "forbidden", "vm self-token cannot fork vms")
			return false
		}
		var msg string
		ttl, msg = parseLifecycleTTL(body.TTLSeconds)
		if msg == "" && body.Name != "" {
			body.Name, msg = validDisplayName(body.Name)
		}
		if msg == "" {
			msg = validLabels(body.Labels)
		}
		if msg == "" {
			msg = validEnv(body.Env)
		}
		if msg != "" {
			writeErr(w, r, 400, "invalid_request", msg)
			return false
		}
		if !s.compatNamedEnv(w, r, p, body.Environment, body.NoEnv != nil && *body.NoEnv) {
			return false
		}
		if body.Type != "" && !reVMName.MatchString(body.Type) {
			writeErr(w, r, 400, "invalid_request", "invalid type")
			return false
		}
		if len(body.Env) > 0 && !p.has("exec") {
			writeErr(w, r, http.StatusForbidden, "forbidden", "env requires scope exec")
			return false
		}
		_, ok := s.ownVM(w, r, p)
		return ok
	}, func(w http.ResponseWriter) {
		fin := map[string]any{"name": body.Name, "fail_fast": body.FailFast, "env": body.Env}
		if body.Environment != "" {
			fin["environment"] = body.Environment
		}
		if body.Labels != nil {
			fin["labels"] = body.Labels
		}
		if body.NoEnv != nil {
			fin["no_env"] = *body.NoEnv // absent: the child follows the parent's run
		}
		if ttl != nil {
			fin["ttl_s"] = *ttl
		}
		res := s.callNative(r, p, "POST /v1/vms/{id}/fork", s.forkVM, r.PathValue("id"), fin)
		if !res.ok() {
			res.relay(w)
			return
		}
		var vm VM
		json.Unmarshal(res.body.Bytes(), &vm)
		retypeStart := map[string]any{"env": body.Env, "fail_fast": body.FailFast} // as compat resume does
		if body.NoEnv != nil && *body.NoEnv {
			retypeStart["no_env"] = true
		}
		if ttl != nil {
			retypeStart["ttl_s"] = *ttl
		}
		restarted, _, rs := s.compatRetypeOpts(r, p, str(vm, "id"), body.Type, retypeStart)
		if !restarted && rs != nil {
			s.failForkRetype(w, r, str(vm, "id"), rs)
			return
		}
		if restarted {
			json.Unmarshal(rs.body.Bytes(), &vm)
		} else if body.Type != "" {
			s.boatd.call(r.Context(), "GET", vmPath(str(vm, "id")), nil, &vm)
		}
		var sb map[string]any
		if r.Header.Get("Idempotency-Key") != "" {
			sb = s.sandboxStage(vm)
		} else {
			sb = s.sandbox(vm)
		}
		if id := str(vm, "id"); id != "" {
			info(r).idemVMs = []string{id}
		}
		writeCompat(w, http.StatusAccepted, "sandbox.forking", map[string]any{"id": sb["id"], "status": sb["state"], "sandbox": sb,
			"forced": false, "operation": nil, "statusUrl": nil})
	})
}

// failForkRetype answers a fork whose type change failed. The child is deleted
// and the answer names no VM ("sandbox"/"vm" dropped: it no longer exists). A
// child that cannot be deleted is force-stopped (best effort, so it does not keep
// running with the parent's state) and kept; the answer names it.
func (s *server) failForkRetype(w http.ResponseWriter, r *http.Request, child string, rs *nativeAnswer) {
	cause := rs.cause
	if cause == "" {
		cause = retypeErrMessage(rs)
	}
	if err := s.discardCopyVM(child); err != nil {
		log.Printf("%s: rollback fork %s: %v", info(r).id, child, err)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), failClosedTimeout)
		stopErr := s.boatd.call(ctx, "POST", vmPath(child, "stop"), map[string]any{"force": true}, nil)
		cancel()
		kept := "it was left stopped"
		if stopErr != nil {
			kept = "it could not be stopped either"
		}
		writeErr(w, r, 500, "cleanup_failed", "the fork's type change failed ("+cause+") and the new VM "+child+" could not be deleted; "+kept)
		return
	}
	code, status := retypeErrCode(rs), rs.status
	if code == "" || status < 400 {
		code, status = "upstream_error", http.StatusBadGateway
	}
	writeErr(w, r, status, code, cause+"; the new VM "+child+" was deleted")
}

// compatSelfStartRefusal is startVM's refusal of a VM's own token ("" for any
// other principal): a compat call that stops a VM to start it again (type change,
// recover) checks it before the stop.
func compatSelfStartRefusal(p *principal) string {
	if p.VMID != "" {
		return "vm self-token cannot start its vm"
	}
	return ""
}

// compatRetype gives a VM boat.dev's type (a size here). boatd changes vCPU
// or RAM only on a stopped VM, with a cold boot next start, so a running VM is
// stopped, resized and started again. nil = done (or already that size, or
// "default" on a host without a size of that name).
func (s *server) compatRetype(r *http.Request, p *principal, id, typ string) *nativeAnswer {
	restarted, _, errAns := s.compatRetypeOpts(r, p, id, typ, nil)
	if restarted {
		return nil
	}
	return errAns
}

func (s *server) compatRetypeOpts(r *http.Request, p *principal, id, typ string, startBody map[string]any) (bool, string, *nativeAnswer) {
	if typ == "" {
		return false, "", nil
	}
	if !reVMName.MatchString(typ) {
		res := &nativeAnswer{h: http.Header{}}
		writeErr(res, r, 400, "invalid_request", "invalid type")
		return false, "", res
	}
	// Check resize permission before stopping: stop and resize have distinct scopes.
	if msg := s.nativeRefusal(p, "POST /v1/vms/{id}/resize"); msg != "" {
		res := &nativeAnswer{h: http.Header{}}
		writeErr(res, r, 403, "forbidden", msg)
		return false, "", res
	}
	var vm VM
	if outer := info(r); outer.vm != nil && str(outer.vm, "id") == id && str(outer.vm, "team") == p.Team.ID {
		vm = outer.vm
	} else if err := s.boatd.call(r.Context(), "GET", vmPath(id), nil, &vm); err != nil || str(vm, "team") != p.Team.ID {
		return false, "", nil // ownVM in the callers' native calls answers a missing or foreign VM
	}
	prevSize := str(vm, "size")
	if prevSize == typ {
		return false, "", nil
	}
	var imgs struct {
		Sizes map[string]any `json:"sizes"`
	}
	if err := s.boatd.call(r.Context(), "GET", "/v1/images", nil, &imgs); err == nil && len(imgs.Sizes) > 0 {
		if _, ok := imgs.Sizes[typ]; !ok {
			if typ == "default" {
				return false, "", nil
			}
			res := &nativeAnswer{h: http.Header{}}
			writeErr(res, r, 400, "invalid_request", fmt.Sprintf("unknown size %q", typ))
			return false, "", res
		}
	}
	running := str(vm, "state") == "running"
	if running {
		// A running VM is stopped, resized and started again; a VM's own token never
		// starts its VM (startVM refuses it). Refused here, before the stop, so the
		// call has no side effects.
		if msg := compatSelfStartRefusal(p); msg != "" {
			res := &nativeAnswer{h: http.Header{}}
			writeErr(res, r, 403, "forbidden", msg)
			return false, "", res
		}
	}
	// What a failed resize restores: the run's no_env and the VM's ttl. The
	// request's new env is never applied to the restored run.
	var restore map[string]any
	if running {
		restore = map[string]any{}
		if s.noEnvRun(id) {
			restore["no_env"] = true
		}
		// boatd omits ttl_s when it is 0 (never auto-stop): an absent field is 0, and
		// the restore says so explicitly so the retype's own ttl cannot stay.
		ttl, ok := vm["ttl_s"].(float64)
		if !ok {
			ttl = 0
		}
		if ttl >= 0 && ttl <= maxTTL {
			restore["ttl_s"] = int64(ttl)
		}
	}
	if running {
		if res := s.callNative(withManagedStop(r), p, "POST /v1/vms/{id}/stop", s.stopVM, id, nil); !res.ok() {
			return false, "", res
		}
	}
	rs := s.callNative(r, p, "POST /v1/vms/{id}/resize", s.resizeVM, id, map[string]any{"size": typ, "discard_memory": true})
	if !rs.ok() && !(typ == "default" && rs.status == 400 && strings.Contains(rs.body.String(), "unknown size")) {
		if running {
			// A rejected resize must not leave a previously running VM stopped.
			if back := s.callNative(withManagedRestart(r), p, "POST /v1/vms/{id}/start", s.startVM, id, restore); !back.ok() {
				res := &nativeAnswer{h: http.Header{}}
				writeErr(res, r, 500, "restore_failed", "resize failed and the previously running VM could not be restarted")
				return false, "", res
			}
		}
		return false, "", rs
	}
	if running {
		// A type change never ends a no_env run: the restart carries no_env whatever
		// the caller's body says (its env and ttl stay on top).
		res := s.callNative(withManagedRestart(r), p, "POST /v1/vms/{id}/start", s.startVM, id, retypeStartBody(startBody, restore))
		if !res.ok() {
			// The VM was running before: bring it back as it was (new size kept)
			// rather than leave it stopped, and answer with the start's error.
			if back := s.callNative(withManagedRestart(r), p, "POST /v1/vms/{id}/start", s.startVM, id, restore); !back.ok() {
				log.Printf("%s: restore %s after failed start following resize: %s", info(r).id, id, back.body.String())
				fail := &nativeAnswer{h: http.Header{}}
				writeErr(fail, r, 500, "restore_failed", "the start after the resize failed ("+retypeErrMessage(res)+") and the previously running VM could not be restarted")
				return false, "", fail
			}
			if retypeErrCode(res) == "env_start_failed" {
				return false, "", s.retypeRestoredAnswer(r, id, res, restore["no_env"] == true)
			}
			return false, "", res
		}
		return true, "", res
	}
	if !rs.ok() {
		return false, "", nil
	}
	return false, prevSize, nil
}

// retypeErrCode is the error code of a failed native answer ("" if none).
func retypeErrCode(a *nativeAnswer) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(a.body.Bytes(), &e) != nil {
		return ""
	}
	return e.Error.Code
}

// retypeRestoredAnswer is the 502 env_start_failed of a type change whose no_env
// start failed closed and whose previous run was then started again. The failed
// start's answer carries the VM as it was stopped; the VM runs again, so the
// answer carries it as it is now (read after the restore), and says so.
func (s *server) retypeRestoredAnswer(r *http.Request, id string, failed *nativeAnswer, wasNoEnv bool) *nativeAnswer {
	cause := retypeErrMessage(failed)
	again := "the previous run is running again with its previous secrets"
	if wasNoEnv {
		again = "the previous no_env run is running again, still without team secrets"
	}
	msg := "the no_env start after the resize failed (" + cause + "); " + again
	out := &nativeAnswer{h: http.Header{}, srv: s, cause: cause}
	body := map[string]any{"error": map[string]string{
		"code": "env_start_failed", "message": msg, "request_id": info(r).id}}
	var cur VM
	if err := s.boatd.call(r.Context(), "GET", vmPath(id), nil, &cur); err == nil {
		body["vm"] = s.publicVMState(cur)
	}
	writeJSON(out, http.StatusBadGateway, body)
	return out
}

// retypeErrMessage is the error text of a failed native answer (its body's
// "error" field, else the raw body, bounded).
func retypeErrMessage(a *nativeAnswer) string {
	var e struct {
		Error any `json:"error"`
	}
	msg := strings.TrimSpace(a.body.String())
	if json.Unmarshal(a.body.Bytes(), &e) == nil {
		switch v := e.Error.(type) {
		case string:
			msg = v
		case map[string]any:
			if m, ok := v["message"].(string); ok {
				msg = m
			}
		}
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}

// retypeStartBody is the start body after a resize: the caller's, with no_env
// forced on when the stopped VM is no_env (restore carries no_env). no_env is
// sticky: a type change never ends it, and a start that names an environment on
// a no_env VM was refused before the stop. A new map: neither input is changed.
func retypeStartBody(startBody, restore map[string]any) map[string]any {
	out := make(map[string]any, len(startBody)+1)
	for k, v := range startBody {
		out[k] = v
	}
	if restore["no_env"] == true {
		out["no_env"] = true
	}
	return out
}

// compatAction runs a native lifecycle call and answers boat.dev's
// SandboxActionResponse (202; this server is done when it answers).
func (s *server) compatAction(w http.ResponseWriter, r *http.Request, p *principal, pattern string, h handlerFunc, id string, body any, typ string) bool {
	res := s.callNative(r, p, pattern, h, id, body)
	if !res.ok() {
		res.relay(w)
		return false
	}
	var vm VM
	json.Unmarshal(res.body.Bytes(), &vm)
	sb := withEnvError(s.sandbox(vm), vm) // a start whose env never arrived answers state "error"
	writeCompat(w, http.StatusAccepted, typ, map[string]any{"id": sb["id"], "status": sb["state"], "sandbox": sb,
		"forced": false, "operation": nil, "statusUrl": nil})
	return true
}

// ---- limits ----

// compatActiveStates are the sandbox states that count as active in /limits.
var compatActiveStates = []string{"provisioning", "ready", "running", "archiving"}

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
		// Counted as boatd's admission counts: a running VM whose env never
		// arrived reads "error" but still holds its slot until it is stopped or
		// deleted, so it is active here too.
		if str(vm, "team") != p.Team.ID {
			continue
		}
		st := str(vm, "state")
		if slices.Contains(compatActiveStates, compatState(st, false)) || slices.Contains(compatActiveStates, st) {
			active++
		}
	}
	writeCompat(w, 200, "limits.info", map[string]any{"activeSandboxes": active, "maxActiveSandboxes": -1,
		"activeStates": compatActiveStates, "canStart": true, "billingStatus": "self_hosted"})
}

// listRefreshParallel bounds the guest status calls a sandbox list makes at once.
const listRefreshParallel = 8

// refreshPromptsForList refreshes the running turns of listed sandboxes. A list
// is a display read, so it reuses a refresh finished within promptRefreshTTL
// (a single sandbox GET keeps the join semantics) and runs a few at a time.
func (s *server) refreshPromptsForList(r *http.Request, ids []string) {
	sem := make(chan struct{}, listRefreshParallel)
	var wg sync.WaitGroup
	for _, id := range ids {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			s.refreshPromptsCached(r, id)
		}()
	}
	wg.Wait()
}

// compatNamedEnv checks the environment a resume or fork names (boat.dev's
// --environment), answering 404 unknown_environment or 400 and reporting false
// when it must not go on. The native start and fork apply it.
func (s *server) compatNamedEnv(w http.ResponseWriter, r *http.Request, p *principal, name string, noEnv bool) bool {
	_, status, msg := s.namedStartEnv(p.Team.ID, r.PathValue("id"), name, noEnv)
	if msg == "" {
		return true
	}
	writeErr(w, r, status, envRefusalCode(status), msg)
	return false
}
