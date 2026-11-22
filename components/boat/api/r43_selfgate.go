package main

import (
	"net/http"
	"strconv"
	"strings"
)

// A no_env sandbox holds no team secrets from the moment it is marked (r46:
// sticky, boat.dev's), but its earlier snapshots (disk and memory) and its command
// and prompt history may: they come from runs before the mark. The sandbox's own
// BOAT_API_KEY must not become a way to read them, so while the VM is no_env its
// self-token is limited to an ALLOWLIST (selfNoEnvAllowed): GET of its own VM,
// exec, the commands it started since the mark (start, status, stream, logs,
// signal, stop), the files of its own VM, its AI usage and stopping itself. Every
// other route answers 403.
//
// The restriction also follows the token generation (VMToken.NoEnvGen): a token
// minted for a no_env run stays restricted. Narrowed resumes carry NarrowGen
// and NarrowAt instead, and use the same allowlist and command run-start check.
//
// A command addressed by id is a record of the VM's per-VM history, and its
// processId is a counter from 1 that anyone can guess. So a record is readable
// by the token only if it started at or after the VM's no_env mark
// (State.NoEnvRuns, re-written by each cold start). Records of earlier runs and
// unknown ids are 403.
//
// The gate runs in route() and again in callNative, so a compat handler cannot
// reach a native route the caller itself may not.
var selfNoEnvAllowed = map[string]string{
	"GET /v1/vms/{id}":                                 "",
	"POST /v1/vms/{id}/exec":                           "",
	"POST /v1/vms/{id}/stop":                           "",
	"GET /v1/vms/{id}/ai-usage":                        "",
	"POST /v1/vms/{id}/commands":                       "",
	"GET /v1/vms/{id}/commands/{cid}":                  "cid",
	"GET /v1/vms/{id}/commands/{cid}/logs":             "cid",
	"POST /v1/vms/{id}/commands/{cid}/signal":          "cid",
	"DELETE /v1/vms/{id}/commands/{cid}":               "cid",
	"GET /v1/vms/{id}/files":                           "",
	"PUT /v1/vms/{id}/files":                           "",
	"DELETE /v1/vms/{id}/files":                        "",
	"GET /v1/vms/{id}/files/list":                      "",
	"POST /v1/vms/{id}/files/move":                     "",
	"GET /v1/vms/{id}/files/search":                    "",
	"GET /v1/vms/{id}/files/info":                      "",
	"POST /v1/vms/{id}/files/folder":                   "",
	"POST /v1/vms/{id}/files/permissions":              "",
	"GET /api/v1/sandboxes/{id}":                       "",
	"POST /api/v1/sandboxes/{id}/stop":                 "",
	"GET /api/v1/sandboxes/{id}/ai-usage":              "",
	"POST /api/v1/sandboxes/{id}/commands":             "",
	"GET /api/v1/sandboxes/{id}/commands/{pid}":        "pid",
	"DELETE /api/v1/sandboxes/{id}/commands/{pid}":     "pid",
	"GET /api/v1/sandboxes/{id}/commands/{pid}/stream": "pid",
	"GET /api/v1/sandboxes/{id}/files":                 "",
	"PUT /api/v1/sandboxes/{id}/files":                 "",
	"DELETE /api/v1/sandboxes/{id}/files":              "",
	"GET /api/v1/sandboxes/{id}/files/list":            "",
	"GET /api/v1/sandboxes/{id}/files/info":            "",
	"GET /api/v1/sandboxes/{id}/files/search":          "",
	"GET /api/v1/sandboxes/{id}/files/find":            "",
	"POST /api/v1/sandboxes/{id}/files/folder":         "",
	"POST /api/v1/sandboxes/{id}/files/move":           "",
	"POST /api/v1/sandboxes/{id}/files/permissions":    "",
	"POST /api/v1/sandboxes/{id}/files/replace":        "",
	"POST /api/v1/sandboxes/{id}/files/download-url":   "",
	"POST /api/v1/sandboxes/{id}/files/upload-url":     "",
}

const (
	selfNoEnvMsg = "this VM is no_env: its own token may only run commands, use files, read its own VM and stop it; snapshots, history, logs of earlier runs and everything else may hold team secrets from earlier runs"
	selfNoEnvRun = "this VM is no_env: its own token may only reach commands that started since it became no_env (its current run)"
	selfLifeMsg  = "a restricted or no_env self-token may not change its VM's auto-stop (ttl_s, extend_s, idle_stop_s), resize it or delete it"
)

// selfNoEnvActive: the VM is no_env (sticky), or its token generation was minted
// for a no_env or narrowed run.
func (s *server) selfNoEnvActive(vmID string) bool {
	if s.store.Snapshot().VMTransitions[vmID] != nil {
		return true
	}
	if t := s.store.Snapshot().VMTokens[vmID]; t != nil && t.NarrowGen != 0 && t.NarrowGen == t.Gen {
		return true
	}
	return s.noEnvRun(vmID) || s.tokenNoEnvGen(vmID)
}

// selfRestricted: a self-token that may not reshape its VM's lifetime: a
// no_env run's, or one minted with RestrictSelf.
func (s *server) selfRestricted(vmID string) bool {
	if t := s.store.Snapshot().VMTokens[vmID]; t != nil && t.RestrictSelf {
		return true
	}
	return s.selfNoEnvActive(vmID)
}

// selfLifecyclePatterns are the routes a restricted self-token may not call at
// all (PATCH is judged by its body, see refuseSelfLifetime).
var selfLifecyclePatterns = map[string]bool{
	"DELETE /v1/vms/{id}":                true,
	"POST /v1/vms/{id}/resize":           true,
	"DELETE /api/v1/sandboxes/{id}":      true,
	"POST /api/v1/sandboxes/{id}/resize": true,
}

// refuseSelfLifetime answers 403 and reports true when p is a restricted
// self-token (see selfRestricted) and the request would change its VM's auto-stop.
func (s *server) refuseSelfLifetime(w http.ResponseWriter, r *http.Request, p *principal) bool {
	if p == nil || p.VMID == "" || !s.selfRestricted(p.VMID) {
		return false
	}
	writeErr(w, r, http.StatusForbidden, "forbidden", selfLifeMsg)
	return true
}

// refuseSelfNoEnv answers 403 and reports true for a VM-token principal on a
// route its token may not use: the lifecycle routes of a restricted token, and
// every route off the allowlist while the VM's run is no_env.
func (s *server) refuseSelfNoEnv(w http.ResponseWriter, r *http.Request, p *principal, pattern string) bool {
	if p == nil || p.VMID == "" {
		return false
	}
	if selfLifecyclePatterns[pattern] && s.selfRestricted(p.VMID) {
		writeErr(w, r, http.StatusForbidden, "forbidden", selfLifeMsg)
		return true
	}
	if !s.selfNoEnvActive(p.VMID) {
		return false
	}
	param, ok := selfNoEnvAllowed[pattern]
	if !ok {
		writeErr(w, r, http.StatusForbidden, "forbidden", selfNoEnvMsg)
		return true
	}
	if param != "" && !s.selfRunCommand(p.VMID, r.PathValue(param), strings.Contains(pattern, "/api/v1/")) {
		writeErr(w, r, http.StatusForbidden, "forbidden", selfNoEnvRun)
		return true
	}
	return false
}

// selfRunCommand: whether the command a path names (a commandId, or for compat a
// processId) may be reached by the no_env token: it is a record that started at
// or after the current run's mark. An unknown id fails closed too, so the answer
// does not tell a guessed id of an earlier run from one that never existed.
func (s *server) selfRunCommand(vmID, name string, compat bool) bool {
	st := s.store.Snapshot()
	n := 0
	if compat {
		n, _ = strconv.Atoi(name)
	}
	at, marked := st.NoEnvRuns[vmID]
	if t := st.VMTokens[vmID]; t != nil && t.NarrowGen != 0 && t.NarrowGen == t.Gen && !t.NarrowAt.IsZero() {
		if !marked || t.NarrowAt.After(at) {
			at = t.NarrowAt
		}
		marked = true
	}
	if !marked {
		return false
	}
	for _, c := range st.Commands[vmID] {
		if c.ID == name || (n > 0 && c.ProcessID == n) {
			return !c.StartedAt.Before(at)
		}
	}
	return false
}
